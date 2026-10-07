package feed

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/model/video"
)

// ---------- 内存替身 ----------
//
// 放在这里而不是 service_test.go:pagedFeedRepo 兼任了这个替身,但清理队列的语义
// (key 幂等、领取即删除)是独立的一块,集中在一处才看得清。

// EnqueueCleanup 复刻真实实现的幂等语义:同一个 cache_key 只留一行。
//
// 注意它**没有**「已完成/死信要重置」那套(fanout.EnqueueReplay 有)——
// 因为领取即删除,表里没有终态行,「清理过之后又变脏」天然是插一行新任务。
func (r *pagedFeedRepo) EnqueueCleanup(_ context.Context, trims []staleTrim) error {
	if r.cleanupError != nil {
		return r.cleanupError
	}
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	for _, t := range trims {
		dup := false
		for _, row := range r.cleanupRows {
			if row.CacheKey == t.key {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		r.cleanupSeq++
		r.cleanupRows = append(r.cleanupRows, CleanupTask{
			ID:       r.cleanupSeq,
			CacheKey: t.key,
			Window:   t.window,
		})
	}
	return nil
}

// ClaimCleanups 复刻「领取即删除」:按 id 升序取前 limit 条并从表里拿掉。
func (r *pagedFeedRepo) ClaimCleanups(_ context.Context, limit int) ([]*CleanupTask, error) {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	if len(r.cleanupRows) == 0 {
		return nil, nil
	}
	n := limit
	if n > len(r.cleanupRows) {
		n = len(r.cleanupRows)
	}
	out := make([]*CleanupTask, 0, n)
	for _, row := range r.cleanupRows[:n] {
		t := row
		out = append(out, &t)
	}
	r.cleanupRows = r.cleanupRows[n:]
	return out, nil
}

// enqueuedKeys 当前还留在队里的 key(升序无关,按入队顺序)。
func (r *pagedFeedRepo) enqueuedKeys() []string {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	keys := make([]string, 0, len(r.cleanupRows))
	for _, row := range r.cleanupRows {
		keys = append(keys, row.CacheKey)
	}
	return keys
}

// pendingCleanups 还留在队里的任务条数。
func (r *pagedFeedRepo) pendingCleanups() int {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	return len(r.cleanupRows)
}

// ---------- 测试 ----------

// cleanupFixture 造一个「服务 + 任务表替身 + miniredis」的组合。
//
// 需要同时观察 Redis 与任务表的测试用它:newFollowingService 只返回服务与 Redis,
// 拿不到替身,而清理相关的断言两边都要看 —— 只看 Redis 会漏掉「任务没入库」,
// 只看任务表会漏掉「入了库但没人执行」。
func cleanupFixture(t *testing.T, videos []*video.Video, followees FolloweeLister, bigvs BigVChecker) (*FeedService, *pagedFeedRepo, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := &pagedFeedRepo{videos: videos}
	return NewFeedService(repo, rdb, fakeLikes{}, followees, bigvs, repo), repo, rdb
}

// TestEnqueueCleanup_同一key幂等 钉住「同一个 key 只留一行」。
//
// 这是清理队列最重要的一条性质:一个热门大V 的缓存会被成百上千个读者在同一瞬间
// 判定为过期。没有幂等,表里就会堆积成千上万条指向同一个 key 的重复任务,
// 而它们要做的事完全一样。
func TestEnqueueCleanup_同一key幂等(t *testing.T) {
	ctx := context.Background()
	repo := &pagedFeedRepo{}
	trim := []staleTrim{{key: bigVKey(7), window: bigVEntry}}

	for i := 0; i < 100; i++ {
		require.NoError(t, repo.EnqueueCleanup(ctx, trim))
	}

	assert.Equal(t, 1, repo.pendingCleanups(), "同一个 key 反复入队只该留一行")
	assert.Equal(t, []string{bigVKey(7)}, repo.enqueuedKeys())
}

// TestEnqueueCleanup_不同key各自成行 上一条的对照组:幂等不能变成「只留第一行」。
func TestEnqueueCleanup_不同key各自成行(t *testing.T) {
	ctx := context.Background()
	repo := &pagedFeedRepo{}

	require.NoError(t, repo.EnqueueCleanup(ctx, []staleTrim{
		{key: inboxKey(1), window: inboxEntry},
		{key: bigVKey(7), window: bigVEntry},
		{key: bigVKey(9), window: bigVEntry},
	}))

	assert.Equal(t, 3, repo.pendingCleanups())
	assert.ElementsMatch(t, []string{inboxKey(1), bigVKey(7), bigVKey(9)}, repo.enqueuedKeys())
}

// TestClaimCleanups_领取即删除 钉住「领走就是消费掉」。
//
// 这是这张表不需要 status / attempts / 租约的原因(见迁移 013):领取即删除,
// 所以没有卡在「处理中」的行需要回收。反过来说,如果实现变成「标记而非删除」,
// 表里就会留下永远无人回收的行 —— 这条测试拦住的就是那个改动。
func TestClaimCleanups_领取即删除(t *testing.T) {
	ctx := context.Background()
	repo := &pagedFeedRepo{}
	require.NoError(t, repo.EnqueueCleanup(ctx, []staleTrim{
		{key: inboxKey(1), window: inboxEntry},
		{key: bigVKey(7), window: bigVEntry},
	}))

	claimed, err := repo.ClaimCleanups(ctx, cleanupClaimBatch)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.Zero(t, repo.pendingCleanups(), "领走的行不该还留在表里")

	// 窗口跟着 key 一起出来 —— 消费端不必按 key 前缀去猜该用哪个窗口
	assert.Equal(t, inboxEntry, claimed[0].Window, "窗口必须随行返回")

	again, err := repo.ClaimCleanups(ctx, cleanupClaimBatch)
	require.NoError(t, err)
	assert.Empty(t, again, "同一条任务不该被领第二次")
}

// TestClaimCleanups_批量上限 钉住 cleanupClaimBatch 的截断:一轮只领这么多,
// 剩下的留给下一轮(排队靠后的 key 不该被某一轮独占着干等)。
func TestClaimCleanups_批量上限(t *testing.T) {
	ctx := context.Background()
	repo := &pagedFeedRepo{}
	for i := 1; i <= cleanupClaimBatch+3; i++ {
		require.NoError(t, repo.EnqueueCleanup(ctx, []staleTrim{
			{key: inboxKey(int64(i)), window: inboxEntry},
		}))
	}

	first, err := repo.ClaimCleanups(ctx, cleanupClaimBatch)
	require.NoError(t, err)
	assert.Len(t, first, cleanupClaimBatch)
	assert.Equal(t, 3, repo.pendingCleanups(), "超出上限的部分该留在队里")

	second, err := repo.ClaimCleanups(ctx, cleanupClaimBatch)
	require.NoError(t, err)
	assert.Len(t, second, 3)
	assert.Zero(t, repo.pendingCleanups())
}

// TestTrimStaleKeys_窗口随行 验实际执行那一步按「行里带的窗口」删,而不是某个全局窗口。
//
// 用两个不同窗口的 key 一次清:若实现只听其中一个(或用一个写死的窗口),
// 另一条的时间边界就会错 —— 这正是不把窗口存进行里的代价。
func TestTrimStaleKeys_窗口随行(t *testing.T) {
	ctx := context.Background()
	_, _, rdb := newFollowingService(t, nil, fakeFollowees{}, nil)
	const dayMicros = int64(24 * time.Hour / time.Microsecond)

	// 两条都在 5 天前:对 7 天窗口是「保留」,对 3 天窗口是「过期」
	both := -5 * dayMicros
	seedZSet(t, rdb, "k:wide", map[int64]int64{11: both})
	seedZSet(t, rdb, "k:narrow", map[int64]int64{22: both})

	require.NoError(t, trimStaleKeys(ctx, rdb, []*CleanupTask{
		{ID: 1, CacheKey: "k:wide", Window: 7 * 24 * time.Hour},
		{ID: 2, CacheKey: "k:narrow", Window: 3 * 24 * time.Hour},
	}))

	assert.Equal(t, []int64{11}, zsetIDs(t, rdb, "k:wide"), "7 天窗口内,不该删")
	assert.Empty(t, zsetIDs(t, rdb, "k:narrow"), "超出 3 天窗口,该删")
}
