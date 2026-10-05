package follow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/pkg/errs"
)

// fakeBackfiller 记录补拉次数;err 非 nil 时模拟补拉失败
type fakeBackfiller struct {
	calls int
	err   error
}

func (f *fakeBackfiller) BackfillOnFollow(_ context.Context, _, _ int64) error {
	f.calls++
	return f.err
}

// fakeReplayer 记录降级重放调用;err 非 nil 时模拟重放失败。
//
// 诚实说明:这个桩只能证明服务层在「检测到降级」时确实调了一次重放、
// 且重放失败不回滚取关。它证明不了真实 Feeds 缓存里的视频真的被搬进粉丝收件箱 ——
// 那是 feed 包 ReplayBigVCache 的职责,在自己的包内用 miniredis 验证。
type fakeReplayer struct {
	calls []int64
	err   error
}

func (f *fakeReplayer) ReplayBigVCache(_ context.Context, authorID int64) error {
	f.calls = append(f.calls, authorID)
	return f.err
}

// fakePromoter 记录升入大V 时的缓存回填调用;err 非 nil 时模拟回填失败。
//
// 诚实说明:这个桩只能证明服务层在「本次真的入列」时确实调了一次回填、
// 且回填失败不回滚关注。它证明不了视频真的被写进 feed:bigv:<id> ——
// 那是 feed 包 PromoteBigVCache 的职责,在自己的包内用 miniredis 验证。
type fakePromoter struct {
	calls []int64
	err   error
}

func (f *fakePromoter) PromoteBigVCache(_ context.Context, authorID int64) error {
	f.calls = append(f.calls, authorID)
	return f.err
}

// newTestFollowService 造一个只测业务规则的服务:bigv 用 rdb==nil 的实现(所有读写降级),
// 免去为关注/取关用例引入 Redis;补拉与重放默认不注入
func newTestFollowService(repo *fakeFollowRepo) *FollowService {
	return NewFollowService(repo, NewBigVSet(repo, nil), nil, nil, nil)
}

func assertCode(t *testing.T, err error, want errs.ServiceErr) {
	t.Helper()
	require.Error(t, err)
	got, ok := errs.As(err)
	require.True(t, ok, "应该返回 ServiceErr,实际 %T: %v", err, err)
	assert.Equal(t, want.Code, got.Code, "错误码不符: %v", err)
}

func TestFollowService_Follow(t *testing.T) {
	ctx := context.Background()

	t.Run("关注自己 400", func(t *testing.T) {
		svc := newTestFollowService(newFakeFollowRepo())
		_, err := svc.Follow(ctx, 7, 7)
		assertCode(t, err, errs.ErrInvalidParam)
	})

	t.Run("未登录 401", func(t *testing.T) {
		svc := newTestFollowService(newFakeFollowRepo())
		_, err := svc.Follow(ctx, 0, 5)
		assertCode(t, err, errs.ErrUnauthorized)
	})

	t.Run("正常关注:响应为已关注且关系真的落库", func(t *testing.T) {
		repo := newFakeFollowRepo()
		svc := newTestFollowService(repo)

		resp, err := svc.Follow(ctx, 1, 2)
		require.NoError(t, err)
		assert.True(t, resp.Following)
		assert.True(t, repo.follows[1][2], "关注关系必须真的写进仓储")
	})

	// 这条是幂等分支最重要的性质:重放既不能再写库,也不能重复投递补拉
	t.Run("重复关注幂等:Follow 与补拉都只发生一次", func(t *testing.T) {
		repo := newFakeFollowRepo()
		backfill := &fakeBackfiller{}
		svc := NewFollowService(repo, NewBigVSet(repo, nil), backfill, nil, nil)

		resp, err := svc.Follow(ctx, 1, 2)
		require.NoError(t, err)
		assert.True(t, resp.Following)

		resp, err = svc.Follow(ctx, 1, 2)
		require.NoError(t, err)
		assert.True(t, resp.Following)

		assert.Equal(t, 1, repo.followCalls, "第二次应被「已关注」短路,不再写库")
		assert.Equal(t, 1, backfill.calls, "重放不该重复投递补拉")
	})

	t.Run("关注数达上限被拒且不写入", func(t *testing.T) {
		repo := newFakeFollowRepo()
		repo.follows[1] = map[int64]bool{}
		for i := int64(1); i <= MaxFollowees; i++ {
			repo.follows[1][i] = true
		}
		svc := newTestFollowService(repo)

		_, err := svc.Follow(ctx, 1, 9999)
		assertCode(t, err, errs.ErrConflict)
		// 上限判定已挪进 repo.Follow(带 limit 的事务),服务层不再单独预检,
		// 所以这里会调用一次 Follow 但被它原子拒绝 —— 关键是没落库
		assert.Equal(t, 1, repo.followCalls, "服务层应把上限判定交给 repo.Follow")
		assert.False(t, repo.follows[1][9999], "被拒的关注不该落库")
	})

	// 被关注者不存在由 users 外键在入库时报错,仓储转成 ErrFolloweeNotFound,
	// 服务层必须映射成 404 —— 不能让客户端瞎填的 id 冒成 500。
	//
	// 诚实边界:内存 fake 造不出真正的外键冲突,这里只验证服务层的错误映射;
	// 外键是否真的挡住脏 id,取决于 009 迁移的 FOREIGN KEY 定义,需真实 PG 才能验证。
	t.Run("被关注者不存在:FK 失败映射成 404 而非 500", func(t *testing.T) {
		repo := newFakeFollowRepo()
		repo.followErr = ErrFolloweeNotFound
		svc := newTestFollowService(repo)

		_, err := svc.Follow(ctx, 1, 999999999)
		assertCode(t, err, errs.ErrNotFound)
	})

	t.Run("关注成功后回填对方粉丝数", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Follow(ctx, 10, 2, MaxFollowees)) // 2 号已有两个粉丝
		require.NoError(t, repo.Follow(ctx, 11, 2, MaxFollowees))
		svc := newTestFollowService(repo)

		resp, err := svc.Follow(ctx, 1, 2)
		require.NoError(t, err)
		assert.Equal(t, int64(3), resp.FollowerCount, "应含刚关注上的这条")
	})

	t.Run("补拉失败不影响关注本身", func(t *testing.T) {
		repo := newFakeFollowRepo()
		backfill := &fakeBackfiller{err: errors.New("补拉炸了")}
		svc := NewFollowService(repo, NewBigVSet(repo, nil), backfill, nil, nil)

		resp, err := svc.Follow(ctx, 1, 2)
		require.NoError(t, err, "补拉是善意优化,失败不该让关注失败")
		assert.True(t, resp.Following)
		assert.Equal(t, 1, backfill.calls)
		assert.True(t, repo.follows[1][2], "关注关系仍应落库")
	})

	// 升入大V 的缓存回填:promoted 信号来自 SADD 的返回值,内存桩给不出这个语义,
	// 所以这几个用例用接 miniredis 的真实 BigVSet 跑一遍。
	newSvcWithPromoter := func(t *testing.T, repo *fakeFollowRepo, promoter BigVPromoter) (*FollowService, *BigVSet) {
		t.Helper()
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		bigv := NewBigVSet(repo, rdb)
		return NewFollowService(repo, bigv, nil, nil, promoter), bigv
	}

	t.Run("新升入大V:回填恰好一次", func(t *testing.T) {
		repo := newFakeFollowRepo()
		// 让 2 号在本次关注前差一个粉丝到阈值:关注后正好升入大V
		repo.setBigVFollowerCount(BigVThreshold-1, 2)
		promoter := &fakePromoter{}
		svc, _ := newSvcWithPromoter(t, repo, promoter)

		resp, err := svc.Follow(ctx, 999999, 2)
		require.NoError(t, err)
		assert.True(t, resp.Following)
		assert.Equal(t, []int64{2}, promoter.calls, "刚升入大V 应回填一次作者缓存")
	})

	t.Run("已是大V:关注不再回填", func(t *testing.T) {
		repo := newFakeFollowRepo()
		repo.setBigVFollowerCount(BigVThreshold-1, 2)
		promoter := &fakePromoter{}
		svc, bigv := newSvcWithPromoter(t, repo, promoter)

		// 先把 2 号放进名单,模拟它此前已升入
		_, _, err := bigv.OnFollowerCountChanged(ctx, 2, BigVThreshold)
		require.NoError(t, err)

		// 关注后粉丝数仍 >= 阈值(SADD 分支),但已是成员 → promoted=false → 不该回填
		resp, err := svc.Follow(ctx, 999999, 2)
		require.NoError(t, err)
		assert.True(t, resp.Following)
		assert.Empty(t, promoter.calls, "已是大V,关注不该再触发回填")
	})

	t.Run("回填失败不影响关注本身", func(t *testing.T) {
		repo := newFakeFollowRepo()
		repo.setBigVFollowerCount(BigVThreshold-1, 2)
		promoter := &fakePromoter{err: errors.New("回填炸了")}
		svc, _ := newSvcWithPromoter(t, repo, promoter)

		resp, err := svc.Follow(ctx, 999999, 2)
		require.NoError(t, err, "回填是善意优化,失败不该让关注失败")
		assert.True(t, resp.Following)
		assert.Equal(t, []int64{2}, promoter.calls)
		assert.True(t, repo.follows[999999][2], "关注关系仍应落库")
	})
}

func TestFollowService_Unfollow(t *testing.T) {
	ctx := context.Background()

	t.Run("取关成功且状态变为未关注", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Follow(ctx, 1, 2, MaxFollowees))
		svc := newTestFollowService(repo)

		resp, err := svc.Unfollow(ctx, 1, 2)
		require.NoError(t, err)
		assert.False(t, resp.Following)
		assert.False(t, repo.follows[1][2], "关系应被物理删除")
	})

	t.Run("取关没关注过的人也算成功", func(t *testing.T) {
		svc := newTestFollowService(newFakeFollowRepo())
		resp, err := svc.Unfollow(ctx, 1, 2)
		require.NoError(t, err)
		assert.False(t, resp.Following)
	})

	t.Run("未登录 401", func(t *testing.T) {
		svc := newTestFollowService(newFakeFollowRepo())
		_, err := svc.Unfollow(ctx, 0, 2)
		assertCode(t, err, errs.ErrUnauthorized)
	})

	// 大V降级重放:降级判定依赖 SREM 的返回值,内存桩给不出这个语义,
	// 所以这几个用例用接 miniredis 的真实 BigVSet 跑一遍。
	newSvc := func(t *testing.T, repo *fakeFollowRepo, replayer BigVDowngradeReplayer) (*FollowService, *BigVSet, *miniredis.Miniredis) {
		t.Helper()
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		bigv := NewBigVSet(repo, rdb)
		return NewFollowService(repo, bigv, nil, replayer, nil), bigv, mr
	}

	t.Run("跌破阈值:真降级时重放恰好一次", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Follow(ctx, 1, 2, MaxFollowees)) // 2 号有 1 个粉丝
		replayer := &fakeReplayer{}
		svc, bigv, _ := newSvc(t, repo, replayer)

		// 先把它放进名单,模拟它曾是大V(count >= 阈值)
		_, downgraded, err := bigv.OnFollowerCountChanged(ctx, 2, BigVThreshold)
		require.NoError(t, err)
		require.False(t, downgraded)

		// 取关后粉丝数掉到 0,跌破阈值 → 应触发一次重放,且只针对作者 2
		resp, err := svc.Unfollow(ctx, 1, 2)
		require.NoError(t, err)
		assert.False(t, resp.Following)
		assert.Equal(t, []int64{2}, replayer.calls, "真降级应恰好触发一次重放")
	})

	t.Run("本就不是大V:不触发重放", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Follow(ctx, 1, 2, MaxFollowees))
		replayer := &fakeReplayer{}
		svc, _, _ := newSvc(t, repo, replayer) // 名单里没有 2 号

		resp, err := svc.Unfollow(ctx, 1, 2)
		require.NoError(t, err)
		assert.False(t, resp.Following)
		assert.Empty(t, replayer.calls, "从未进过名单的小V取关不该误报降级、不该重放")
	})

	t.Run("重放失败不影响取关本身", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Follow(ctx, 1, 2, MaxFollowees))
		replayer := &fakeReplayer{err: errors.New("重放炸了")}
		svc, bigv, _ := newSvc(t, repo, replayer)

		_, _, err := bigv.OnFollowerCountChanged(ctx, 2, BigVThreshold)
		require.NoError(t, err)

		resp, err := svc.Unfollow(ctx, 1, 2)
		require.NoError(t, err, "重放是补救,失败不该让取关失败")
		assert.False(t, resp.Following)
		assert.Equal(t, []int64{2}, replayer.calls)
		assert.False(t, repo.follows[1][2], "取关关系仍应物理删除")
	})

	// 防抖:同一个作者在窗口内反复「降级」时,同步重放只跑第一次。
	// 复刻「账号把作者吊在阈值附近反复 follow/unfollow」的打法 —— 每次都会
	// 真的发生一次 SREM 降级,若没有防抖,每次都同步重放一遍。
	//
	// 诚实边界:这里验的是服务层用 SETNX 把重复重放挡在门外;真正的任务去重
	// 仍靠 fanout_tasks 的 UNIQUE (video_id, task_type)(在 feed 包验)。
	t.Run("防抖:窗口内二次降级不再重复同步重放,过期后恢复", func(t *testing.T) {
		repo := newFakeFollowRepo()
		for _, fid := range []int64{1, 3, 4} {
			require.NoError(t, repo.Follow(ctx, fid, 2, MaxFollowees))
		}
		replayer := &fakeReplayer{}
		svc, bigv, mr := newSvc(t, repo, replayer)

		// 把作者 2 放进大V名单(模拟它曾是大V)
		_, downgraded, err := bigv.OnFollowerCountChanged(ctx, 2, BigVThreshold)
		require.NoError(t, err)
		require.False(t, downgraded)

		// 第一次取关:粉丝数跌破阈值 → 真降级 → 重放一次(set 防抖 key)
		_, err = svc.Unfollow(ctx, 1, 2)
		require.NoError(t, err)
		require.Equal(t, []int64{2}, replayer.calls, "首次降级应重放")

		// 再次把它放回名单,再取关一次:又是真降级,但防抖窗口内 → 不再重放
		_, _, err = bigv.OnFollowerCountChanged(ctx, 2, BigVThreshold)
		require.NoError(t, err)
		_, err = svc.Unfollow(ctx, 3, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{2}, replayer.calls, "防抖窗口内的二次降级不该重放")

		// 时间越过 TTL:防抖 key 过期,第三次降级应恢复重放
		mr.FastForward(2 * time.Minute)
		_, _, err = bigv.OnFollowerCountChanged(ctx, 2, BigVThreshold)
		require.NoError(t, err)
		_, err = svc.Unfollow(ctx, 4, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{2, 2}, replayer.calls, "TTL 过后应恢复重放")
	})
}

func TestFollowService_ListFollowees(t *testing.T) {
	ctx := context.Background()

	t.Run("空列表返回空切片而不是 nil", func(t *testing.T) {
		svc := newTestFollowService(newFakeFollowRepo())
		resp, err := svc.ListFollowees(ctx, 1)
		require.NoError(t, err)
		assert.NotNil(t, resp.FolloweeIDs, "前端按数组用,不能给 null")
		assert.Empty(t, resp.FolloweeIDs)
		assert.Equal(t, MaxFollowees, resp.Limit)
	})

	t.Run("未登录 401", func(t *testing.T) {
		svc := newTestFollowService(newFakeFollowRepo())
		_, err := svc.ListFollowees(ctx, 0)
		assertCode(t, err, errs.ErrUnauthorized)
	})

	t.Run("升序返回关注的人", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Follow(ctx, 1, 9, MaxFollowees))
		require.NoError(t, repo.Follow(ctx, 1, 3, MaxFollowees))
		svc := newTestFollowService(repo)

		resp, err := svc.ListFollowees(ctx, 1)
		require.NoError(t, err)
		assert.Equal(t, []int64{3, 9}, resp.FolloweeIDs)
	})
}
