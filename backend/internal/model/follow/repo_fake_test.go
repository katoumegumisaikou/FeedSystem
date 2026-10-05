package follow

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFollowRepo 内存版 FollowRepository,仓储提成接口后不连 PG 就能测业务规则。
//
// follows 是唯一真相:关注者 -> 关注集合。order 只记首次建立关系的先后,
// 用来验证幂等重放不会真的落第二条
type fakeFollowRepo struct {
	follows map[int64]map[int64]bool // followerID -> {followeeID: true}
	order   []int64                  // 关系首次建立的先后顺序

	// 调用计数:断言「已关注」短路没再写库、大V名单没被反复重算
	followCalls       int
	unfollowCalls     int
	listFolloweeCalls int
	bigVCalls         int

	// 错误注入:各方法可单独打开,验证上层对仓储失败的降级/报错
	followErr         error
	unfollowErr       error
	isFollowErr       error
	countFolloweesErr error
	countFollowersErr error
	listFolloweeErr   error
	listFollowerErr   error
	bigVErr           error
}

// 编译期断言:换接口签名时这里会先编译不过,而不是等运行时踩空
var _ FollowRepository = (*fakeFollowRepo)(nil)

func newFakeFollowRepo() *fakeFollowRepo {
	return &fakeFollowRepo{follows: map[int64]map[int64]bool{}}
}

// Follow 幂等:已存在的关系不重写也不进 order —— 与 ON CONFLICT DO NOTHING 同义。
// 计数 >= limit 时返回 ErrFolloweeLimit,复刻仓储在事务里的原子判定
func (f *fakeFollowRepo) Follow(_ context.Context, followerID, followeeID int64, limit int64) error {
	f.followCalls++
	if f.followErr != nil {
		return f.followErr
	}
	if int64(len(f.follows[followerID])) >= limit {
		return ErrFolloweeLimit
	}
	set := f.follows[followerID]
	if set == nil {
		set = map[int64]bool{}
		f.follows[followerID] = set
	}
	if !set[followeeID] {
		set[followeeID] = true
		f.order = append(f.order, followeeID)
	}
	return nil
}

// Unfollow 物理删除。删不存在的键不是错误(delete 对 nil map 也安全)
func (f *fakeFollowRepo) Unfollow(_ context.Context, followerID, followeeID int64) error {
	f.unfollowCalls++
	if f.unfollowErr != nil {
		return f.unfollowErr
	}
	delete(f.follows[followerID], followeeID)
	return nil
}

func (f *fakeFollowRepo) IsFollowing(_ context.Context, followerID, followeeID int64) (bool, error) {
	if f.isFollowErr != nil {
		return false, f.isFollowErr
	}
	return f.follows[followerID][followeeID], nil // 读 nil map 恒 false,无需判空
}

func (f *fakeFollowRepo) CountFollowees(_ context.Context, followerID int64) (int64, error) {
	if f.countFolloweesErr != nil {
		return 0, f.countFolloweesErr
	}
	return int64(len(f.follows[followerID])), nil
}

// CountFollowers 遍历所有关注者数谁关注了 followeeID —— 真实实现走 idx_follows_followee
func (f *fakeFollowRepo) CountFollowers(_ context.Context, followeeID int64) (int64, error) {
	if f.countFollowersErr != nil {
		return 0, f.countFollowersErr
	}
	var n int64
	for _, set := range f.follows {
		if set[followeeID] {
			n++
		}
	}
	return n, nil
}

// ListFolloweeIDs 升序返回 —— 与 SQL 的 ORDER BY followee_id ASC 对齐
func (f *fakeFollowRepo) ListFolloweeIDs(_ context.Context, followerID int64) ([]int64, error) {
	f.listFolloweeCalls++
	if f.listFolloweeErr != nil {
		return nil, f.listFolloweeErr
	}
	ids := make([]int64, 0, len(f.follows[followerID]))
	for id := range f.follows[followerID] {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// ListFollowerIDs 游标分页:afterID<=0 为首页,按 follower_id 升序取前 limit 条。
// limit 为负按「不限」处理,与 SQL LIMIT 语义一致
func (f *fakeFollowRepo) ListFollowerIDs(_ context.Context, followeeID, afterID int64, limit int) ([]int64, error) {
	if f.listFollowerErr != nil {
		return nil, f.listFollowerErr
	}
	ids := []int64{}
	for followerID, set := range f.follows {
		if !set[followeeID] {
			continue
		}
		if afterID > 0 && followerID <= afterID {
			continue
		}
		ids = append(ids, followerID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if limit >= 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// ListBigVAuthorIDs 按粉丝数聚合、升序返回达到阈值的作者,供 BigVSet 冷启动重算
func (f *fakeFollowRepo) ListBigVAuthorIDs(_ context.Context, threshold int64) ([]int64, error) {
	f.bigVCalls++
	if f.bigVErr != nil {
		return nil, f.bigVErr
	}
	counts := map[int64]int64{}
	for _, set := range f.follows {
		for followeeID := range set {
			counts[followeeID]++
		}
	}
	var ids []int64
	for id, n := range counts {
		if n >= threshold {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// setBigV 把关注关系重置成「名单里的作者都是大V」:每个作者各配 BigVThreshold 个粉丝。
// 用来精确控制 ListBigVAuthorIDs 的结果,又不必为用例堆业务数据
func (f *fakeFollowRepo) setBigV(authors ...int64) {
	f.setBigVFollowerCount(BigVThreshold, authors...)
}

// setBigVFollowerCount 与 setBigV 同,但粉丝数可指定 —— 用来造出落在滞回带
// [BigVThreshold-BigVHysteresis, BigVThreshold) 里的作者
func (f *fakeFollowRepo) setBigVFollowerCount(n int64, authors ...int64) {
	f.follows = map[int64]map[int64]bool{}
	for _, author := range authors {
		for i := int64(1); i <= n; i++ {
			set := f.follows[i]
			if set == nil {
				set = map[int64]bool{}
				f.follows[i] = set
			}
			set[author] = true
		}
	}
}

// TestFakeFollowRepo 锁住 fake 本身的契约 —— service/bigv 的用例都建立在这些语义之上
func TestFakeFollowRepo(t *testing.T) {
	ctx := context.Background()

	t.Run("Follow 幂等:重复关注不产生第二条记录", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Follow(ctx, 1, 2, MaxFollowees))
		require.NoError(t, repo.Follow(ctx, 1, 2, MaxFollowees))
		assert.Len(t, repo.order, 1, "幂等重放不该再落一条")
		ids, err := repo.ListFolloweeIDs(ctx, 1)
		require.NoError(t, err)
		assert.Equal(t, []int64{2}, ids)
	})

	// 边界用例:关注数到达 limit 时 Follow 必须在写库前拒绝,返回 ErrFolloweeLimit。
	// 旧的「先 Count 再插入」两条语句实现在这里会把第 limit+1 条插进去并返回 nil,
	// 所以这条用例对旧实现是失败的 —— 它钉的是「计数与插入之间不能有可乘之机」这个契约。
	//
	// 诚实声明:内存 fake 单线程执行,复现不了真实并发竞态。别把这条用例当作
	// 「竞态已消除」的证明。真正的并发安全来自 SQL 侧按 follower_id 取的
	// pg_advisory_xact_lock(见 repo.go 的 Follow),它需要真实 PostgreSQL 才能验证,
	// 不在这个测试的覆盖范围里。
	t.Run("Follow 在 limit 边界拒绝并返回 ErrFolloweeLimit", func(t *testing.T) {
		repo := newFakeFollowRepo()
		// 先填到恰好 MaxFollowees-1 条(1..MaxFollowees-1)
		for i := int64(1); i < MaxFollowees; i++ {
			require.NoError(t, repo.Follow(ctx, 1, i, MaxFollowees))
		}
		// 还能再加一个,正好补满到 MaxFollowees
		require.NoError(t, repo.Follow(ctx, 1, MaxFollowees, MaxFollowees))

		// 已满,再加任何人必须被拒,且不落库
		err := repo.Follow(ctx, 1, MaxFollowees+1, MaxFollowees)
		assert.ErrorIs(t, err, ErrFolloweeLimit)
		assert.False(t, repo.follows[1][MaxFollowees+1], "超限的关注不该落库")
		assert.Len(t, repo.follows[1], MaxFollowees, "关注数应停在 limit,不多一条")
	})

	t.Run("Unfollow 不存在的关系不算错", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Unfollow(ctx, 1, 999))
	})

	t.Run("ListFolloweeIDs 升序", func(t *testing.T) {
		repo := newFakeFollowRepo()
		require.NoError(t, repo.Follow(ctx, 1, 30, MaxFollowees))
		require.NoError(t, repo.Follow(ctx, 1, 10, MaxFollowees))
		require.NoError(t, repo.Follow(ctx, 1, 20, MaxFollowees))

		ids, err := repo.ListFolloweeIDs(ctx, 1)
		require.NoError(t, err)
		assert.Equal(t, []int64{10, 20, 30}, ids)
	})

	t.Run("ListFollowerIDs 游标与 limit", func(t *testing.T) {
		repo := newFakeFollowRepo()
		for _, id := range []int64{5, 3, 9, 7} {
			require.NoError(t, repo.Follow(ctx, id, 100, MaxFollowees))
		}

		page1, err := repo.ListFollowerIDs(ctx, 100, 0, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{3, 5}, page1)

		page2, err := repo.ListFollowerIDs(ctx, 100, 5, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{7, 9}, page2)
	})

	t.Run("ListBigVAuthorIDs 按粉丝数过滤且升序", func(t *testing.T) {
		repo := newFakeFollowRepo()
		for _, f := range []int64{10, 11, 12} {
			require.NoError(t, repo.Follow(ctx, f, 2, MaxFollowees)) // 作者 2 有三个粉丝
		}
		require.NoError(t, repo.Follow(ctx, 10, 1, MaxFollowees)) // 作者 1 只有一个

		ids, err := repo.ListBigVAuthorIDs(ctx, 3)
		require.NoError(t, err)
		assert.Equal(t, []int64{2}, ids)
	})
}
