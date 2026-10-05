package follow

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cmdRecorder 记下真正发给 Redis 的命令名,用来断言某些路径没发无效命令
// (比如空入参时不该发不带成员的 SMISMEMBER)
type cmdRecorder struct {
	mu   sync.Mutex
	cmds []string
}

func (r *cmdRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *cmdRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.mu.Lock()
		r.cmds = append(r.cmds, cmd.Name())
		r.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (r *cmdRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (r *cmdRecorder) calls(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.cmds {
		if c == name {
			n++
		}
	}
	return n
}

func (r *cmdRecorder) reset() {
	r.mu.Lock()
	r.cmds = nil
	r.mu.Unlock()
}

// newBigVRedis 起一个接 miniredis 的 go-redis 客户端,并挂上命令记录器
func newBigVRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client, *cmdRecorder) {
	t.Helper()
	mr := miniredis.RunT(t)
	rec := &cmdRecorder{}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdb.AddHook(rec)
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb, rec
}

func TestBigVSet(t *testing.T) {
	ctx := context.Background()

	// 平台上一个过阈值的作者都没有 —— 空 SET 的 key 在 Redis 里根本不存在,
	// 这正是哨兵存在的理由。没有哨兵,每次读都会被当成「没建过」而全量重算
	t.Run("空名单只构建一次", func(t *testing.T) {
		repo := newFakeFollowRepo()
		_, rdb, _ := newBigVRedis(t)
		svc := NewBigVSet(repo, rdb)

		for i := 0; i < 3; i++ {
			got, err := svc.Contains(ctx, []int64{1, 2})
			require.NoError(t, err)
			assert.Empty(t, got)
		}
		assert.Equal(t, 1, repo.bigVCalls, "哨兵在,后续读不该再次触发全量重算")
	})

	t.Run("哨兵存在时 Rebuild 之后不再重算", func(t *testing.T) {
		repo := newFakeFollowRepo()
		repo.setBigV(1)
		_, rdb, _ := newBigVRedis(t)
		svc := NewBigVSet(repo, rdb)

		require.NoError(t, svc.Rebuild(ctx))
		require.Equal(t, 1, repo.bigVCalls)

		for i := 0; i < 3; i++ {
			got, err := svc.Contains(ctx, []int64{1, 2})
			require.NoError(t, err)
			assert.True(t, got[1])
			assert.False(t, got[2])
		}
		assert.Equal(t, 1, repo.bigVCalls, "已 Rebuild 过,读路径不该再算")
	})

	// 只做 SADD 的增量重建删不掉掉出阈值的账号,这条就是为它写的
	t.Run("Rebuild 移除跌破阈值的账号", func(t *testing.T) {
		repo := newFakeFollowRepo()
		_, rdb, _ := newBigVRedis(t)
		svc := NewBigVSet(repo, rdb)

		repo.setBigV(1, 2)
		require.NoError(t, svc.Rebuild(ctx))
		got, err := svc.Contains(ctx, []int64{1, 2})
		require.NoError(t, err)
		require.True(t, got[1])
		require.True(t, got[2])

		repo.setBigV(1) // 2 号掉粉跌破阈值
		require.NoError(t, svc.Rebuild(ctx))

		got, err = svc.Contains(ctx, []int64{1, 2})
		require.NoError(t, err)
		assert.True(t, got[1])
		assert.False(t, got[2], "跌破阈值的账号必须被 DEL+SADD 的重建清掉")
	})

	t.Run("OnFollowerCountChanged 按粉丝数增删名单", func(t *testing.T) {
		repo := newFakeFollowRepo()
		_, rdb, _ := newBigVRedis(t)
		svc := NewBigVSet(repo, rdb)

		require.NoError(t, svc.Rebuild(ctx)) // 先立哨兵,免得 Contains 触发重算覆盖掉手工改动

		// 加入名单:粉丝数达到阈值,不是降级,返回值应为 false
		_, downgraded, err := svc.OnFollowerCountChanged(ctx, 7, BigVThreshold)
		require.NoError(t, err)
		assert.False(t, downgraded, "加入名单不是降级")
		got, err := svc.Contains(ctx, []int64{7})
		require.NoError(t, err)
		assert.True(t, got[7], "粉丝数达到阈值应加入名单")

		// 跌破降级下界:原本是成员,应报告一次真实降级
		// (注意不是 BigVThreshold-1 —— 那条落在滞回带内,不该被移除)
		_, downgraded, err = svc.OnFollowerCountChanged(ctx, 7, BigVThreshold-BigVHysteresis-1)
		require.NoError(t, err)
		assert.True(t, downgraded, "从名单里被移除才算降级")
		got, err = svc.Contains(ctx, []int64{7})
		require.NoError(t, err)
		assert.False(t, got[7], "跌破降级下界应 SREM 出名单")

		// 再报一次同样的低粉丝数:已不在名单里,不该再算作降级(false transition)
		_, downgraded, err = svc.OnFollowerCountChanged(ctx, 7, BigVThreshold-BigVHysteresis-1)
		require.NoError(t, err)
		assert.False(t, downgraded, "本就不在名单里,不该报告降级")
	})

	// 升入大V 的信号来自 SADD 的返回值(1 = 本次真的入列),内存桩给不出这个语义,
	// 所以用接 miniredis 的真实 BigVSet 跑一遍。
	t.Run("OnFollowerCountChanged 首次入列报 promoted,重复调用为 false", func(t *testing.T) {
		repo := newFakeFollowRepo()
		_, rdb, _ := newBigVRedis(t)
		svc := NewBigVSet(repo, rdb)

		require.NoError(t, svc.Rebuild(ctx)) // 先立哨兵,免得 Contains 触发重算覆盖手工改动

		// 首次达到阈值:SADD 真的加了成员,promoted 应为 true
		promoted, downgraded, err := svc.OnFollowerCountChanged(ctx, 7, BigVThreshold)
		require.NoError(t, err)
		assert.True(t, promoted, "首次 SADD 入列应报 promoted")
		assert.False(t, downgraded, "入列不是降级")

		// 已是大V,再次上报同一粉丝数:SADD 返回 0,不该重复报 promoted
		// (上层据此保证「升入时的缓存回填」至多跑一次)
		promoted, downgraded, err = svc.OnFollowerCountChanged(ctx, 7, BigVThreshold+1)
		require.NoError(t, err)
		assert.False(t, promoted, "已是大V 的重复调用不该再报 promoted")
		assert.False(t, downgraded)
	})

	t.Run("Contains 空入参不发 SMISMEMBER", func(t *testing.T) {
		repo := newFakeFollowRepo()
		_, rdb, rec := newBigVRedis(t)
		svc := NewBigVSet(repo, rdb)
		require.NoError(t, svc.Rebuild(ctx)) // 立哨兵,让 EnsureBuilt 不再发构建命令

		rec.reset()
		got, err := svc.Contains(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, got)
		// 空入参若不短路,会发出不带成员的 SMISMEMBER —— 真实 Redis 直接报参数错误
		assert.Zero(t, rec.calls("smismember"), "空入参不该查名单")
	})
}

// TestBigVSet_滞回带 带内维持现状,只有跨过降级下界才算真降级。
//
// 这是防「粉丝数在临界值附近来回横跳、反复触发降级重放」的核心性质:每次真实降级
// 都会同步做一次 ZREVRANGE + 最多 50 次入队,没有滞回时少量账号在 50000 上下 ±1
// 抖动就能让服务反复做无用功。
func TestBigVSet_滞回带(t *testing.T) {
	ctx := context.Background()
	upper := BigVThreshold                  // 入列线
	lower := BigVThreshold - BigVHysteresis // 下界本身仍算带内
	band := BigVThreshold - 1               // 带内(紧贴上界之下)
	under := lower - 1                      // 跌破下界

	newSvc := func(t *testing.T) *BigVSet {
		t.Helper()
		repo := newFakeFollowRepo()
		_, rdb, _ := newBigVRedis(t)
		svc := NewBigVSet(repo, rdb)
		require.NoError(t, svc.Rebuild(ctx)) // 立哨兵,免得 Contains 触发重算覆盖手工改动
		return svc
	}

	t.Run("带内不删:原是大V,掉进带里仍保留且不报降级", func(t *testing.T) {
		svc := newSvc(t)
		_, _, err := svc.OnFollowerCountChanged(ctx, 7, upper) // 先入列
		require.NoError(t, err)

		for _, c := range []int64{band, lower} { // 下界本身仍在带内
			_, downgraded, err := svc.OnFollowerCountChanged(ctx, 7, c)
			require.NoError(t, err)
			assert.False(t, downgraded, "粉丝数 %d 在带内,不该报降级", c)
			got, err := svc.Contains(ctx, []int64{7})
			require.NoError(t, err)
			assert.True(t, got[7], "粉丝数 %d 在带内,应保留成员身份", c)
		}
	})

	t.Run("带内不增:原是小V,涨进带里仍不算大V", func(t *testing.T) {
		svc := newSvc(t)
		_, downgraded, err := svc.OnFollowerCountChanged(ctx, 8, band)
		require.NoError(t, err)
		assert.False(t, downgraded)
		got, err := svc.Contains(ctx, []int64{8})
		require.NoError(t, err)
		assert.False(t, got[8], "带内不该被加进名单")
	})

	t.Run("跌破下界才算真降级", func(t *testing.T) {
		svc := newSvc(t)
		_, _, err := svc.OnFollowerCountChanged(ctx, 9, upper)
		require.NoError(t, err)

		_, downgraded, err := svc.OnFollowerCountChanged(ctx, 9, under)
		require.NoError(t, err)
		assert.True(t, downgraded, "跌破下界应报一次真实降级")
		got, err := svc.Contains(ctx, []int64{9})
		require.NoError(t, err)
		assert.False(t, got[9], "跌破下界应出列")
	})

	t.Run("横跳不再反复触发降级", func(t *testing.T) {
		svc := newSvc(t)
		_, _, err := svc.OnFollowerCountChanged(ctx, 10, upper)
		require.NoError(t, err)

		// 在临界值附近上下抖动多次:一次都不该报降级
		for _, c := range []int64{band, upper, band, upper - 2, lower, upper - 1} {
			_, downgraded, err := svc.OnFollowerCountChanged(ctx, 10, c)
			require.NoError(t, err)
			assert.False(t, downgraded, "粉丝数 %d 在带内,不该报降级", c)
		}
		got, err := svc.Contains(ctx, []int64{10})
		require.NoError(t, err)
		assert.True(t, got[10], "抖动之后仍应是大V")

		// 真的掉下去才报一次
		_, downgraded, err := svc.OnFollowerCountChanged(ctx, 10, under)
		require.NoError(t, err)
		assert.True(t, downgraded, "真跌破下界才报降级")
	})
}

// TestBigVSet_Rebuild用下界重建 重算必须按滞回下界收录。若按上界,带内的作者会被
// 踢出名单,而 Rebuild 不触发重放 —— 他们的缓存条目当场不可见,正是重放机制要修的丢数据。
func TestBigVSet_Rebuild用下界重建(t *testing.T) {
	ctx := context.Background()
	repo := newFakeFollowRepo()
	_, rdb, _ := newBigVRedis(t)
	svc := NewBigVSet(repo, rdb)

	// 作者粉丝数恰好落在滞回带下界上(带内):按上界重算会漏掉他
	repo.setBigVFollowerCount(BigVThreshold-BigVHysteresis, 10)
	require.NoError(t, svc.Rebuild(ctx))
	got, err := svc.Contains(ctx, []int64{10})
	require.NoError(t, err)
	assert.True(t, got[10], "滞回带内的作者应被重建收录(按上界会漏)")

	// 跌破下界则应被清掉
	repo.setBigVFollowerCount(BigVThreshold-BigVHysteresis-1, 10)
	require.NoError(t, svc.Rebuild(ctx))
	got, err = svc.Contains(ctx, []int64{10})
	require.NoError(t, err)
	assert.False(t, got[10], "跌破下界应被重建清掉")
}

func TestBigVSet_降级(t *testing.T) {
	ctx := context.Background()

	t.Run("rdb 为 nil 时 Contains 返回空且不报错", func(t *testing.T) {
		svc := NewBigVSet(newFakeFollowRepo(), nil)
		got, err := svc.Contains(ctx, []int64{1, 2})
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("rdb 为 nil 时 EnsureBuilt 返回 nil", func(t *testing.T) {
		svc := NewBigVSet(newFakeFollowRepo(), nil)
		assert.NoError(t, svc.EnsureBuilt(ctx))
	})

	t.Run("rdb 为 nil 时 OnFollowerCountChanged 返回 nil", func(t *testing.T) {
		svc := NewBigVSet(newFakeFollowRepo(), nil)
		_, downgraded, err := svc.OnFollowerCountChanged(ctx, 1, BigVThreshold*2)
		assert.NoError(t, err)
		assert.False(t, downgraded, "无 Redis 时无从判断降级,一律 false")
	})
}
