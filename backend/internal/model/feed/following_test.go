package feed

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/model/video"
	"feed-system/internal/pkg/errs"
)

// ---------- 测试替身 ----------

// fakeFollowees 「我关注了谁」的内存实现
type fakeFollowees struct {
	ids []int64
	err error
}

func (f fakeFollowees) ListFolloweeIDs(context.Context, int64) ([]int64, error) {
	return f.ids, f.err
}

// fakeBigV 大V 名单的内存实现。big 里没有的 ID 一律返回 false
type fakeBigV struct {
	big map[int64]bool
	err error
}

func (f fakeBigV) Contains(_ context.Context, authorIDs []int64) (map[int64]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	m := make(map[int64]bool, len(authorIDs))
	for _, id := range authorIDs {
		m[id] = f.big[id]
	}
	return m, nil
}

// fakeLikes 点赞状态查询的内存实现。关注流要求 userID > 0,setLikeStatuses 会真的
// 被调用,这里传一个可用的实现覆盖「点赞态被正常填上」的路径。
// (它不返回错误:setLikeStatuses 已改成失败只记日志、不阻断列表,所以这里是否失败
// 都不会影响接口可用性,无需再为 nil 设防。)
type fakeLikes struct{}

func (fakeLikes) GetUserLikeStatuses(context.Context, int64, []int64) (map[int64]bool, error) {
	return map[int64]bool{}, nil
}

// ---------- 辅助 ----------

// testMicroBase 测试时间基准。
//
// 这些测试用 1_000_000 / 5_000_000 这种小数字表达**相对次序**(数字越大越新),
// 但按绝对时间算它们是 1970 年 —— 读侧加上 followingReadWindow 之后会全被判过期,
// 一条都返回不了。所以把基准挪到「现在之前一点」,再把测试给的值加上去:
// 相对次序完全不变,而绝对值落进窗口内。
//
// 取 now-2000s(约 33 分钟前)而不是 now:加上最大字面量(5s)之后仍是过去时刻,
// 不会造出「发布时间在未来」的数据;同时给窗口测试留出用负值表达「更早」的空间
// (见 TestListFollowing_时间窗口)。
var testMicroBase = time.Now().UnixMicro() - 2_000_000_000

// testMicro 把测试里的小数字映射成落在读侧窗口内的绝对微秒。
func testMicro(v int64) int64 { return testMicroBase + v }

// feedVideoAtMicro 造一条已发布视频。关注流不按视频的 created_at 排序(它按 ZSET
// score = published_at),这里的 created_at 只是给卡片填个值,分页顺序由 seedZSet 控制。
// authorID 参与「按当前关注集合过滤」:作者不在当前关注列表里的条目会被丢掉
func feedVideoAtMicro(id, authorID, micros int64) *video.Video {
	return &video.Video{ID: id, AuthorID: authorID, CreatedAt: time.UnixMicro(testMicro(micros)), Status: video.StatusPublished}
}

// newFollowingService 起一个带 miniredis 的关注流服务。
//
// repo 同时兼作清理任务队列的内存替身(见 cleanup_test.go),所以这里只需传一个对象。
func newFollowingService(t *testing.T, videos []*video.Video, followees FolloweeLister, bigvs BigVChecker) (*FeedService, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := &pagedFeedRepo{videos: videos}
	svc := NewFeedService(repo, rdb, fakeLikes{}, followees, bigvs, repo)
	return svc, mr, rdb
}

// seedZSet 直接往一个 ZSET 里塞 member=video_id 字符串、score=微秒 的条目。
// 绕过 push 脚本,才能把同一 score 的成员按我们想要的顺序摆进去
func seedZSet(t *testing.T, rdb *redis.Client, key string, idToScore map[int64]int64) {
	t.Helper()
	ctx := context.Background()
	for id, score := range idToScore {
		// score 过 testMicro:让这些「表达相对次序的小数字」落在读侧时间窗口内。
		// 窗口相关的测试用负值表达「更早」(见 TestListFollowing_时间窗口)
		require.NoError(t, rdb.ZAdd(ctx, key, redis.Z{
			Score:  float64(testMicro(score)),
			Member: strconv.FormatInt(id, 10),
		}).Err())
	}
}

func itemIDs(items []FeedItem) []int64 {
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

// zsetIDs 读出一个 ZSET 当前的成员(升序),用于断言「Redis 里真的删了」。
// 断言必须落在 Redis 上,不能只看响应 —— 响应里的缺失也可能只是被内存过滤掉了
func zsetIDs(t *testing.T, rdb *redis.Client, key string) []int64 {
	t.Helper()
	members, err := rdb.ZRange(context.Background(), key, 0, -1).Result()
	require.NoError(t, err)
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		id, err := strconv.ParseInt(m, 10, 64)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	return ids
}

// runCleanup 把任务表里积压的清理任务全部执行掉,模拟 worker 消费循环的若干轮。
//
// 生产路径由 FanoutWorker.Run 驱动;测试不能依赖它(后台调度不可控),所以直接走
// 同一个「领取 + trimStaleKeys」—— 覆盖的是真正执行清理的那段代码,只是驱动方式不同。
func runCleanup(t *testing.T, repo *pagedFeedRepo, rdb *redis.Client) {
	t.Helper()
	ctx := context.Background()
	for {
		tasks, err := repo.ClaimCleanups(ctx, cleanupClaimBatch)
		require.NoError(t, err)
		if len(tasks) == 0 {
			return
		}
		require.NoError(t, trimStaleKeys(ctx, rdb, tasks))
	}
}

// ---------- 测试 ----------

// TestListFollowing_合并两个来源 inbox(推模式)+ 大V 缓存(拉模式)合并后按 (created_at, id) 降序
func TestListFollowing_合并两个来源(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const bigAid int64 = 7
	const normalAid int64 = 3

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(42, bigAid, 4_000_000),
			feedVideoAtMicro(32, bigAid, 3_000_000),
			feedVideoAtMicro(21, normalAid, 2_000_000),
			feedVideoAtMicro(11, normalAid, 1_000_000),
		},
		fakeFollowees{ids: []int64{bigAid, normalAid}},
		fakeBigV{big: map[int64]bool{bigAid: true}},
	)

	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{21: 2_000_000, 11: 1_000_000})
	seedZSet(t, rdb, bigVKey(bigAid), map[int64]int64{42: 4_000_000, 32: 3_000_000})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)
	require.NotNil(t, resp.Items)
	assert.Equal(t, []int64{42, 32, 21, 11}, itemIDs(resp.Items), "两个来源合并后按 score 降序")
	assert.Nil(t, resp.NextCursor)
}

// TestListFollowing_同分按ID降序 是这个方法最关键的测试。
//
// Redis 对相同 score 的成员按 member 字节序排,不是数值序。实测:
//
//	ZRange 升序 = [100 11 9](字节序)
//	ZRevRange   = [9 11 100](读路径实际拿到的)
//
// 若不重排,结果会是 [9 11 100];正确结果必须按 id 数值降序 [100 11 9]。
func TestListFollowing_同分按ID降序(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const same int64 = 12_345
	const author int64 = 5

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(9, author, same),
			feedVideoAtMicro(100, author, same),
			feedVideoAtMicro(11, author, same),
		},
		// 关注列表非 nil 但只有一个作者:只走 inbox 一条来源
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{9: same, 100: same, 11: same})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)
	require.Len(t, resp.Items, 3)

	got := itemIDs(resp.Items)
	assert.Equal(t, []int64{100, 11, 9}, got, "同一 score 必须按 id 数值降序,不能吃 Redis 字节序")
	// 显式点出错误的字节序结果,免得日后有人把断言改成 [9 11 100] 还以为对
	assert.NotEqual(t, []int64{9, 11, 100}, got, "[9 11 100] 是 Redis 字节序,不是我们要的顺序")
}

// TestListFollowing_游标翻页 5 条、limit=2 连翻三页,合起来不重不漏
func TestListFollowing_游标翻页(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 5

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(50, author, 5_000_000),
			feedVideoAtMicro(40, author, 4_000_000),
			feedVideoAtMicro(30, author, 3_000_000),
			feedVideoAtMicro(20, author, 2_000_000),
			feedVideoAtMicro(10, author, 1_000_000),
		},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{
		50: 5_000_000, 40: 4_000_000, 30: 3_000_000, 20: 2_000_000, 10: 1_000_000,
	})

	var all []int64
	req := ListFollowingReq{Limit: 2}
	for page := 1; page <= 3; page++ {
		resp, err := svc.ListFollowing(ctx, req, uid)
		require.NoError(t, err)
		all = append(all, itemIDs(resp.Items)...)
		if page < 3 {
			require.NotNil(t, resp.NextCursor, "第 %d 页后面还有,该给游标", page)
			req.CursorCreatedAt = &resp.NextCursor.CreatedAt
			req.CursorVideoID = &resp.NextCursor.VideoID
		} else {
			assert.Nil(t, resp.NextCursor, "第三页取完了,游标该是 nil")
		}
	}

	assert.Equal(t, []int64{50, 40, 30, 20, 10}, all, "三页合起来恰好是全部 5 条,无重复、顺序正确")
}

// TestListFollowing_收件箱为空 空页也返回 [],不是 null
func TestListFollowing_收件箱为空(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFollowingService(t, nil, fakeFollowees{ids: []int64{}}, nil)

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, 1)
	require.NoError(t, err)
	require.NotNil(t, resp.Items, "Items 必须是非 nil 的 []")
	assert.Empty(t, resp.Items)
	assert.Nil(t, resp.NextCursor)
}

// TestListFollowing_非大V不读大V缓存 判定为 false 时,feed:bigv:<aid> 的内容不能出现在结果里
func TestListFollowing_非大V不读大V缓存(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const aid int64 = 7

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{feedVideoAtMicro(42, aid, 4_000_000)},
		fakeFollowees{ids: []int64{aid}},
		fakeBigV{big: map[int64]bool{aid: false}},
	)
	// 大V 缓存里确实有内容,但判定说他不是大V,就不该被读
	seedZSet(t, rdb, bigVKey(aid), map[int64]int64{42: 4_000_000})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)
	assert.Empty(t, resp.Items, "非大V 时不能读 feed:bigv:<aid>,缓存内容不该出现")
}

// TestListFollowing_关注列表未配置 依赖没接上时降级成空关注列表,不 500
func TestListFollowing_关注列表未配置(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := NewFeedService(&pagedFeedRepo{}, rdb, fakeLikes{}, nil, nil, nil)

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, 1)
	require.NoError(t, err, "关注列表查询器缺失不该报错")
	require.NotNil(t, resp.Items)
	assert.Empty(t, resp.Items)
	assert.Nil(t, resp.NextCursor)
}

// TestListFollowing_参数与依赖校验 rdb 为 nil / 未登录 / 游标只给一半
func TestListFollowing_参数与依赖校验(t *testing.T) {
	ctx := context.Background()

	t.Run("rdb 为 nil 返回缓存错误", func(t *testing.T) {
		svc := NewFeedService(&pagedFeedRepo{}, nil, fakeLikes{}, fakeFollowees{}, nil, nil)
		_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, 1)
		assert.ErrorIs(t, err, errs.ErrInternalCache)
	})

	t.Run("未登录返回未授权", func(t *testing.T) {
		svc := NewFeedService(&pagedFeedRepo{}, nil, fakeLikes{}, fakeFollowees{}, nil, nil)
		_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, 0)
		assert.ErrorIs(t, err, errs.ErrUnauthorized)
	})

	t.Run("游标只给 created_at", func(t *testing.T) {
		at := int64(1)
		svc, _, _ := newFollowingService(t, nil, fakeFollowees{}, nil)
		_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10, CursorCreatedAt: &at}, 1)
		assert.ErrorIs(t, err, errs.ErrInvalidParam)
	})

	t.Run("游标只给 video_id", func(t *testing.T) {
		id := int64(7)
		svc, _, _ := newFollowingService(t, nil, fakeFollowees{}, nil)
		_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10, CursorVideoID: &id}, 1)
		assert.ErrorIs(t, err, errs.ErrInvalidParam)
	})
}

// TestListFollowing_limit校验 服务层必须自己校验 limit,不能只依赖 HTTP 绑定:
// 直连 service 的调用传负数或超大值,若不拦,负 limit 会让取条数失去意义、hasMore 判成假。
func TestListFollowing_limit校验(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFollowingService(t, nil, fakeFollowees{}, nil)

	_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: -1}, 1)
	assert.ErrorIs(t, err, errs.ErrInvalidParam, "负 limit 必须报参数错误")

	_, err = svc.ListFollowing(ctx, ListFollowingReq{Limit: 101}, 1)
	assert.ErrorIs(t, err, errs.ErrInvalidParam, "超过上限的 limit 必须报参数错误")
}

// TestListFollowing_读后刷新TTL 读取后收件箱 key 的 TTL 要被续上
func TestListFollowing_读后刷新TTL(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 5

	svc, mr, rdb := newFollowingService(t,
		[]*video.Video{feedVideoAtMicro(10, author, 1_000_000)},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	// 只写成员,不设 TTL,模拟已经快过期的收件箱
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{10: 1_000_000})
	require.Zero(t, mr.TTL(inboxKey(uid)), "前置条件:调用前该 key 没有 TTL")

	_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)

	ttl := mr.TTL(inboxKey(uid))
	assert.Greater(t, ttl, time.Duration(0), "读取后应续上 TTL")
	assert.LessOrEqual(t, ttl, inboxTTL, "续的 TTL 不该超过配置值")
}

// ---------- 缺陷修复回归 ----------

// TestListFollowing_按当前关注集合过滤 已取关作者的视频不能再出现。
//
// 收件箱条目是「关注期内」推入的,取关后最多 7 天才滚出。若不按当前关注集合过滤,
// 已取关作者的视频会在关注流里残留最多 7 天。此处 reader 当前只关注作者 1,
// 收件箱里却还有作者 2 的视频 —— 必须被过滤掉;作者 1 的视频照常返回。
func TestListFollowing_按当前关注集合过滤(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const followed int64 = 1
	const unfollowed int64 = 2

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(100, followed, 2_000_000),
			feedVideoAtMicro(200, unfollowed, 1_000_000),
		},
		// 当前只关注作者 1:FolloweeLister 的返回值才是真相,收件箱里的残留不算
		fakeFollowees{ids: []int64{followed}},
		nil,
	)
	// 收件箱里两条都在(200 是取关作者 2 之前推入的残留)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{100: 2_000_000, 200: 1_000_000})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{100}, itemIDs(resp.Items),
		"已取关作者 2 的视频必须被过滤,只留关注中的作者 1")
}

// TestListFollowing_整页被过滤不终止分页 最新若干条都取不到卡片(已删/下架)时,
// 分页不能提前结束。
//
// 选择的语义:继续向后扫描候选,把能取到的旧条目填进本页(而不是返回空页 + 游标),
// 所以第一页就能看到那条更旧的存活视频;本页填满且后面还有候选时才给游标。
func TestListFollowing_整页被过滤不终止分页(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 5

	// 50、40 是「幽灵」:收件箱里有,但仓储取不到卡片(模拟已删/下架);
	// 30、20、10 存活。limit=2,最新 2 条恰好全被过滤。
	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(30, author, 3_000_000),
			feedVideoAtMicro(20, author, 2_000_000),
			feedVideoAtMicro(10, author, 1_000_000),
		},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{
		50: 5_000_000, 40: 4_000_000, 30: 3_000_000, 20: 2_000_000, 10: 1_000_000,
	})

	req := ListFollowingReq{Limit: 2}
	page1, err := svc.ListFollowing(ctx, req, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{30, 20}, itemIDs(page1.Items),
		"最新两条取不到卡片时,必须继续向后扫描填满本页")
	require.NotNil(t, page1.NextCursor, "后面还有条目 10,游标必须给出")

	req.CursorCreatedAt = &page1.NextCursor.CreatedAt
	req.CursorVideoID = &page1.NextCursor.VideoID
	page2, err := svc.ListFollowing(ctx, req, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{10}, itemIDs(page2.Items),
		"第二页应能取到最后一条,不被幽灵条目挡住")
	assert.Nil(t, page2.NextCursor)
}

// TestListFollowing_游标取最后消费的边界 本页由某条填满,其后只剩取不到卡片的条目时,
// 游标仍指向该条(窗口实际到达的边界),第二页据此过滤掉它、返回空页且不再给游标 ——
// 游标不能越过没返回的条目,也不能让客户端无限翻页。
func TestListFollowing_游标取最后消费的边界(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 5

	// limit=1:30 存活,20、10 取不到卡片。
	svc, _, rdb := newFollowingService(t,
		[]*video.Video{feedVideoAtMicro(30, author, 3_000_000)},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{
		30: 3_000_000, 20: 2_000_000, 10: 1_000_000,
	})

	page1, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 1}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{30}, itemIDs(page1.Items))
	require.NotNil(t, page1.NextCursor)
	assert.Equal(t, testMicro(3_000_000), page1.NextCursor.CreatedAt, "游标取最后消费条目的 score")
	assert.Equal(t, int64(30), page1.NextCursor.VideoID)

	page2, err := svc.ListFollowing(ctx, ListFollowingReq{
		Limit:           1,
		CursorCreatedAt: &page1.NextCursor.CreatedAt,
		CursorVideoID:   &page1.NextCursor.VideoID,
	}, uid)
	require.NoError(t, err)
	assert.Empty(t, page2.Items, "边界之后只剩取不到卡片的条目")
	assert.Nil(t, page2.NextCursor, "没有更多数据时游标该是 nil,避免无限翻页")
}

// TestListFollowing_正常整页不受影响 全量有效数据时,长度/顺序/游标与修复前一致。
func TestListFollowing_正常整页不受影响(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 5

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(50, author, 5_000_000),
			feedVideoAtMicro(40, author, 4_000_000),
			feedVideoAtMicro(30, author, 3_000_000),
			feedVideoAtMicro(20, author, 2_000_000),
			feedVideoAtMicro(10, author, 1_000_000),
		},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{
		50: 5_000_000, 40: 4_000_000, 30: 3_000_000, 20: 2_000_000, 10: 1_000_000,
	})

	page1, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 2}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{50, 40}, itemIDs(page1.Items), "正常页长度与顺序不变")
	require.NotNil(t, page1.NextCursor, "还有下一页,游标照给")
	assert.Equal(t, testMicro(4_000_000), page1.NextCursor.CreatedAt)
	assert.Equal(t, int64(40), page1.NextCursor.VideoID)
}

// TestListFollowing_整页取满且无更多时不给游标 边界回归:存活条目数恰好等于 limit、
// 取满一页且其后没有任何候选时,NextCursor 必须是 nil。
//
// 这是 hasMore 判据最容易写错的边界 —— 只看「本页是否满」就给游标,或把判据写成恒真,
// 都会在这里多给一个假游标,让前端永远多翻一次空页。
func TestListFollowing_整页取满且无更多时不给游标(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 5

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(20, author, 2_000_000),
			feedVideoAtMicro(10, author, 1_000_000),
		},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	// 恰好 limit=2 条存活,取满一页后一艘无余
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{20: 2_000_000, 10: 1_000_000})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 2}, uid)
	require.NoError(t, err)
	require.Len(t, resp.Items, 2, "恰好一页,应取满")
	assert.Equal(t, []int64{20, 10}, itemIDs(resp.Items))
	assert.Nil(t, resp.NextCursor, "整页取满且后面没有候选时,不该给游标")
}

// TestListFollowing_候选封顶不丢条目 关注大量大V 时合并后的候选会被 followingCandidateCap
// 截断。这条构造「窗口内最前面的 followingCandidateCap 条全是取不到卡片的幽灵、存活条目
// 排在其后」的极端情形,验证截断后仍给出游标,下一页能顺着游标取回存活条目 ——
// 游标没有越过任何未消费的条目,也没有提前判「到底了」把存活内容永久丢掉。
func TestListFollowing_候选封顶不丢条目(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 5

	// 塞满 followingCandidateCap 条幽灵条目(收件箱里有、仓储取不到卡片),score 都高于存活条目;
	// 存活两条 score 最低,排在幽灵之后。
	seed := map[int64]int64{10: 1_000, 11: 2_000}
	for i := 0; i < followingCandidateCap; i++ {
		seed[int64(1_000_000+i)] = int64(3_000_000 + i)
	}

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(10, author, 1_000),
			feedVideoAtMicro(11, author, 2_000),
		},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), seed)

	req := ListFollowingReq{Limit: 2}
	page1, err := svc.ListFollowing(ctx, req, uid)
	require.NoError(t, err)
	assert.Empty(t, page1.Items, "前 cap 条全是幽灵,本页填不出存活条目")
	require.NotNil(t, page1.NextCursor,
		"候选被截断且尚未取到存活条目 —— 必须给游标,否则存活条目永久丢失")

	req.CursorCreatedAt = &page1.NextCursor.CreatedAt
	req.CursorVideoID = &page1.NextCursor.VideoID
	page2, err := svc.ListFollowing(ctx, req, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{11, 10}, itemIDs(page2.Items), "翻页应取回被截断挡在后面的存活条目")
	assert.Nil(t, page2.NextCursor)
}

// TestListFollowing_跨来源同ID去重 同一视频同时出现在收件箱(推模式残留)与大V缓存(拉模式)、
// 且两处 score 不同时,合并排序后会被别的条目隔开而不相邻 —— 只比相邻一条的去重会漏过去,
// 读者会看到重复卡片。这里用 seen 集合按 videoID 去重,与顺序无关。
func TestListFollowing_跨来源同ID去重(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 7

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(100, author, 5_000_000),
			feedVideoAtMicro(200, author, 3_000_000),
		},
		fakeFollowees{ids: []int64{author}},
		fakeBigV{big: map[int64]bool{author: true}},
	)
	// 收件箱:100(旧 score 残留)+ 200;大V 缓存:100(新 score)。
	// 合并排序后为 [100@5M, 200@3M, 100@1M] —— 同 id 的两次出现不相邻。
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{100: 1_000_000, 200: 3_000_000})
	seedZSet(t, rdb, bigVKey(author), map[int64]int64{100: 5_000_000})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{100, 200}, itemIDs(resp.Items),
		"同一视频的两个来源必须去重成一个,且保留 score 最高的那次")
}

// errLikes 永远报错的点赞查询器,用来验证「点赞态只是装饰字段,失败不该拖垮列表」。
type errLikes struct{}

func (errLikes) GetUserLikeStatuses(context.Context, int64, []int64) (map[int64]bool, error) {
	return nil, errs.ErrInternal
}

// TestListFollowing_点赞失败不阻断列表 回归:点赞态填充失败只该降级(IsLike 保持 nil),
// 不该让整个关注流 500。修复前 setLikeStatuses 返回 error、调用方直接 return nil, err,
// 一个只影响「红心显不显示」的查询会把整个列表拖垮。
func TestListFollowing_点赞失败不阻断列表(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 7

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := NewFeedService(
		&pagedFeedRepo{videos: []*video.Video{
			feedVideoAtMicro(100, author, 2_000_000),
			feedVideoAtMicro(200, author, 1_000_000),
		}},
		rdb,
		errLikes{}, // ← 点赞查询必失败
		fakeFollowees{ids: []int64{author}},
		nil,
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{100: 2_000_000, 200: 1_000_000})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err, "点赞查询失败不该让关注流报错")
	assert.Equal(t, []int64{100, 200}, itemIDs(resp.Items), "列表内容应照常返回")
	for i, it := range resp.Items {
		assert.Nil(t, it.IsLike, "第 %d 条未算出点赞态,IsLike 应为 nil(DTO 允许「未计算」)", i)
	}
}

// TestListFollowing_时间窗口 读侧必须丢掉超过 followingReadWindow 的条目。
//
// 为什么读侧要自己判一次:写入侧的 7 天裁剪(见 inboxPushScript 的 ZREMRANGEBYSCORE)
// 是「写一次扫一次」。停止更新的作者,其缓存自最后一次投递后再没人扫过,过期条目会
// 一直留到 key TTL;而读是 ZRevRange(0, -1)、不带 score 过滤 —— 不补这一刀就会
// 把十天前的视频照常返回。真库上就是这么暴露的。
//
// 用负值表达「更早」:seedZSet 会把 score 过 testMicro(加上基准),所以 -5 天 就是
// 「基准往前 5 天」,配合基准本身(now-2000s)落在 5 天前;而 -10 天 落在 10 天前。
// 两个值离 7 天边界都有足够余量,不依赖时钟精度。
func TestListFollowing_时间窗口(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const bigAid int64 = 7
	const normalAid int64 = 3
	const dayMicros = int64(24 * time.Hour / time.Microsecond)

	valid := -5 * dayMicros    // ≈5 天前:窗口内,应返回
	expired := -10 * dayMicros // ≈10 天前:窗口外,应丢弃

	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(11, normalAid, valid),
			feedVideoAtMicro(22, bigAid, expired),
		},
		fakeFollowees{ids: []int64{bigAid, normalAid}},
		fakeBigV{big: map[int64]bool{bigAid: true}},
	)
	// 过期的放【大V 缓存】、有效的放【收件箱】—— 这一刀是对**合并后的序列**做的,
	// 所以两个来源一起被覆盖。分开摆是为了防止「只在某一个来源上过滤」这种半成品
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{11: valid})
	seedZSet(t, rdb, bigVKey(bigAid), map[int64]int64{22: expired})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{11}, itemIDs(resp.Items), "10 天前的那条必须被丢弃")
	assert.Nil(t, resp.NextCursor,
		"剩下的都在窗口外,不该给出一个指向过期位置的游标 —— 否则客户端白翻一页空页")
}

// TestListFollowing_时间窗口不影响分页 窗口内的条目照常翻页。
//
// 上一条验的是「过期的不出现」,这条验的是「没把窗口内的误杀」——
// 两者缺一不可:只验前者的话,一个「把 entries 清空」的实现也能通过。
func TestListFollowing_时间窗口不影响分页(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 3
	const dayMicros = int64(24 * time.Hour / time.Microsecond)

	// 三条都在窗口内(1/2/3 天前),外加一条 9 天前的干扰项
	svc, _, rdb := newFollowingService(t,
		[]*video.Video{
			feedVideoAtMicro(30, author, -1*dayMicros),
			feedVideoAtMicro(20, author, -2*dayMicros),
			feedVideoAtMicro(10, author, -3*dayMicros),
			feedVideoAtMicro(99, author, -9*dayMicros),
		},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{
		30: -1 * dayMicros, 20: -2 * dayMicros, 10: -3 * dayMicros, 99: -9 * dayMicros,
	})

	// limit=2:第一页应拿到最新的两条,游标指向 20
	page1, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 2}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{30, 20}, itemIDs(page1.Items), "窗口内的条目照常返回")
	require.NotNil(t, page1.NextCursor, "窗口内还有第三条,该给游标")

	page2, err := svc.ListFollowing(ctx, ListFollowingReq{
		Limit:           2,
		CursorCreatedAt: &page1.NextCursor.CreatedAt,
		CursorVideoID:   &page1.NextCursor.VideoID,
	}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{10}, itemIDs(page2.Items), "第三条仍是窗口内的,应能翻到")
	assert.Nil(t, page2.NextCursor, "剩下的只有过期项,到底了")
}

// TestListFollowing_读侧惰性清理 过期成员要被**真的从 Redis 删掉**,而不只是不返回。
//
// 为什么要删:写入侧的裁剪是「写一次扫一次」,停止更新的作者再没有事件去扫它,
// 过期条目会一直留到 key TTL,而每次读都要把它们取回来再在内存里过滤掉 —— 纯浪费
// 且持续到 TTL。读的时候顺手删掉,后续请求读到的就是干净的集合。
//
// 断言必须落在 Redis 上(zcard / zrange),只看响应是不够的:上一条测试已经覆盖了
// 「过期的不返回」,这一条要验的是它**不见了**。
func TestListFollowing_读侧惰性清理(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const bigAid int64 = 7
	const normalAid int64 = 3
	const dayMicros = int64(24 * time.Hour / time.Microsecond)

	valid := -2 * dayMicros     // 2 天前:窗口内
	edge := -5 * dayMicros      // 5 天前:**关键档**,见下方注释
	stale := -10 * dayMicros    // 10 天前:存储窗口外,应被删
	staleToo := -20 * dayMicros // 20 天前:同样该被删

	// edge 那一档不能省。它落在「7 天窗口内、但超出任何比 7 天窄的窗口」这个区间 ——
	// 没有它的话,「把删除窗口改成 followingReadWindow/2(3.5 天)」这种改错也能通过:
	// 只用 valid(2 天)+ stale(10 天)时,valid 比 3.5 天还新、stale 本来就该删,
	// 那个变异**实测存活着**。加上 edge 之后,正确的 7 天窗口会保留它、3.5 天会误删它,
	// 两者才区分得开。
	svc, repo, rdb := cleanupFixture(t,
		[]*video.Video{
			feedVideoAtMicro(11, normalAid, valid),
			feedVideoAtMicro(55, normalAid, edge),
			feedVideoAtMicro(22, bigAid, stale),
			feedVideoAtMicro(44, bigAid, edge),
		},
		fakeFollowees{ids: []int64{bigAid, normalAid}},
		fakeBigV{big: map[int64]bool{bigAid: true}},
	)
	// 两个来源各自都摆上 edge 与 stale:只摆一个来源的话,另一侧的删除窗口
	// 改错了也看不出来(收件箱与大V 缓存是分开传 window 的)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{11: valid, 55: edge, 33: staleToo})
	seedZSet(t, rdb, bigVKey(bigAid), map[int64]int64{44: edge, 22: stale})

	require.EqualValues(t, 3, rdb.ZCard(ctx, inboxKey(uid)).Val(), "前置条件:收件箱 3 条")
	require.EqualValues(t, 2, rdb.ZCard(ctx, bigVKey(bigAid)).Val(), "前置条件:大V 缓存 2 条")

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{11, 55, 44}, itemIDs(resp.Items),
		"窗口内的都该返回:收件箱 2 条 + 大V 缓存 1 条,按发布时间降序")

	// 清理已经不在请求路径上了:请求返回时缓存还没动过,过期成员仍在里面。
	// 一个「在请求里顺手删」的实现会在这里当场红 —— 这正是上面那句「顺手删」改成
	// 入库之后必须重新钉住的东西
	assert.EqualValues(t, 3, rdb.ZCard(ctx, inboxKey(uid)).Val(),
		"请求返回时不该已经删过:清理只入库,等 worker 消费")
	assert.EqualValues(t, 2, rdb.ZCard(ctx, bigVKey(bigAid)).Val())
	assert.ElementsMatch(t, []string{inboxKey(uid), bigVKey(bigAid)}, repo.enqueuedKeys(),
		"两个来源各自的 key 都该入库(各自带自己的窗口)")

	runCleanup(t, repo, rdb)

	// ★ 关键断言:过期成员在 Redis 里真的没了,而窗口内的(尤其 edge)必须留着
	// 注意 zsetIDs 是**按分数升序**读的,所以更早的 edge(55) 排在 valid(11) 前面
	assert.Equal(t, []int64{55, 11}, zsetIDs(t, rdb, inboxKey(uid)),
		"收件箱应只剩 edge 与 valid,过期那条被删;edge 被误删说明删除窗口比存储窗口窄了")
	assert.Equal(t, []int64{44}, zsetIDs(t, rdb, bigVKey(bigAid)),
		"大V 缓存应只剩 edge,过期那条被删")
}

// TestListFollowing_清理不误删窗口内成员 是上一条的对照组。
//
// 只验「过期被删」的话,一个「把整个 key 删掉」的实现也能通过 —— 必须同时验
// 「窗口内的仍留着」,才能排除过度清理。
func TestListFollowing_清理不误删窗口内成员(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 3
	const dayMicros = int64(24 * time.Hour / time.Microsecond)

	svc, repo, rdb := cleanupFixture(t,
		[]*video.Video{
			feedVideoAtMicro(30, author, -1*dayMicros),
			feedVideoAtMicro(20, author, -6*dayMicros), // 6 天前,仍在 7 天窗口内
		},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	// 同时放一条 10 天前的。清理必须**真的跑起来**才有意义 —— 全新鲜的话一轮清理
	// 一条都不会删,这条测试就退化成「什么都没做也绿」。而它只该删掉过期那一条
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{
		30: -1 * dayMicros, 20: -6 * dayMicros, 99: -10 * dayMicros,
	})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)
	assert.Equal(t, []int64{30, 20}, itemIDs(resp.Items))

	runCleanup(t, repo, rdb)

	// 按分数升序:6 天前的 20 最旧排最前,然后是 1 天前的 30;99 已过期,不该在
	assert.Equal(t, []int64{20, 30}, zsetIDs(t, rdb, inboxKey(uid)),
		"只该删掉过期那条:窗口内的两条(含贴边界的 6 天前)必须留着")
}

// TestListFollowing_新鲜缓存不发清理命令 钉住「只在有超期成员时才写」。
//
// 热路径上每个请求都要跑这段;若无条件发 ZREMRANGEBYSCORE,关注 200 个大V 的请求
// 每次会多出 201 条命令。用 go-redis 的 Hook 记录命令名来验:读一次新鲜缓存时,
// 清理类命令应为 0 条。
//
// 不用 miniredis 的「命令日志」—— 它没有这个功能。Hook 是 go-redis 的标准扩展点,
// 也顺带覆盖了 pipeline 里的命令(清理走 pipeline,不走 ProcessHook 就走 pipeline 那个)
func TestListFollowing_新鲜缓存不发清理命令(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 3

	mr := miniredis.RunT(t)
	var (
		mu   sync.Mutex
		seen []string
	)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdb.AddHook(&cmdRecorder{mu: &mu, seen: &seen})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := &pagedFeedRepo{videos: []*video.Video{feedVideoAtMicro(30, author, 0)}}
	svc := NewFeedService(repo, rdb, fakeLikes{}, fakeFollowees{ids: []int64{author}}, nil, repo)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{30: 0})

	// 放在播种之后清空,只统计这次请求发出的命令
	mu.Lock()
	seen = seen[:0]
	mu.Unlock()

	_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	var cleanup []string
	for _, name := range seen {
		// 注意大小写:ToUpper 之后必须与**大写**字面量比。
		// 这里第一版写的是小写字面量,switch 永远不匹配,清理命令一条都识别不出来 ——
		// 于是这条测试在「无条件清理」的变异下依然通过(变异存活暴露了它)。
		switch strings.ToUpper(name) {
		case "ZREMRANGEBYSCORE", "ZREMRANGEBYRANK", "DEL":
			cleanup = append(cleanup, name)
		}
	}
	assert.Empty(t, cleanup, "缓存新鲜时不该发清理命令;本次全部命令: %v", seen)

	// 更强的判据:没发命令可能是「没入库」、也可能是「入了库但 worker 还没跑」。
	// 这里直接钉住前者 —— 新鲜缓存一个 key 都不该进任务表
	assert.Zero(t, repo.pendingCleanups(), "缓存新鲜时不该入队任何待清理 key")
}

// TestListFollowing_清理离开请求路径 钉住:清理不在请求延迟里,但任务确实落了库。
//
// 判据是「请求返回时缓存还没被删、而任务表里已经有这一行」—— 光是「最后缓存干净了」
// 不够,因为把清理挪回请求路径也能达到那个结果,区别只在什么时候删。挪回去,前半段
// 断言立刻红。
func TestListFollowing_清理离开请求路径(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 3
	const dayMicros = int64(24 * time.Hour / time.Microsecond)

	svc, repo, rdb := cleanupFixture(t,
		[]*video.Video{feedVideoAtMicro(11, author, -1*dayMicros)},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{
		11: -1 * dayMicros, 99: -10 * dayMicros,
	})

	_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err)

	// 请求已经返回,缓存原封不动 —— 清理只在任务表里等着
	assert.EqualValues(t, 2, rdb.ZCard(ctx, inboxKey(uid)).Val(),
		"请求路径上不该发清理命令")
	assert.Equal(t, []string{inboxKey(uid)}, repo.enqueuedKeys(),
		"过期的 key 应该已经写进任务表")

	// worker 那一轮跑完之后才真的干净
	runCleanup(t, repo, rdb)
	assert.Equal(t, []int64{11}, zsetIDs(t, rdb, inboxKey(uid)))
	assert.Zero(t, repo.pendingCleanups(), "领走的行不该还留在表里")
}

// TestListFollowing_入队失败不阻断返回 钉住 enqueueStaleTrims 唯一的失败分支。
//
// 清理是**优化**,不是正确性的一环:正确性由读侧的展示窗口截断独立保证(见
// ListFollowing 里的 cutoff 截断)。所以写任务表失败时,列表必须照常返回 ——
// 为一次优化写库失败而让整个关注流 500,是「装饰字段拖垮主功能」的同一类错误。
func TestListFollowing_入队失败不阻断返回(t *testing.T) {
	ctx := context.Background()
	const uid int64 = 1
	const author int64 = 3
	const dayMicros = int64(24 * time.Hour / time.Microsecond)

	svc, repo, rdb := cleanupFixture(t,
		[]*video.Video{
			feedVideoAtMicro(11, author, -1*dayMicros),
			feedVideoAtMicro(22, author, -2*dayMicros),
		},
		fakeFollowees{ids: []int64{author}},
		nil,
	)
	// 窗口内两条 + 一条过期的:有过期的才会走 enqueueStaleTrims
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{
		11: -1 * dayMicros, 22: -2 * dayMicros, 99: -10 * dayMicros,
	})
	repo.cleanupError = errors.New("任务表写失败")

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err, "入队失败不该让关注流报错")
	assert.Equal(t, []int64{11, 22}, itemIDs(resp.Items), "列表内容应照常返回")
	assert.Zero(t, repo.pendingCleanups(), "前置条件:任务确实没写进去")
}

// panickingPipelineHook 让任何 pipeline 命令直接 panic,模拟清理执行过程中崩掉。
// 只拦 pipeline:ZAdd(播种)走单命令路径,照常通过。
type panickingPipelineHook struct{}

func (panickingPipelineHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (panickingPipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (panickingPipelineHook) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error {
		panic("pipeline 故意 panic")
	}
}

// cmdRecorder 记录经过客户端的命令名,供上面的测试断言。
// go-redis 的 Hook 接口有三个方法,只有后两个与「发了什么命令」有关;
// DialHook 原样透传(它管的是连接建立,与命令无关)
type cmdRecorder struct {
	mu   *sync.Mutex
	seen *[]string
}

func (h *cmdRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *cmdRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		*h.seen = append(*h.seen, cmd.Name())
		h.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (h *cmdRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.mu.Lock()
		for _, c := range cmds {
			*h.seen = append(*h.seen, c.Name())
		}
		h.mu.Unlock()
		return next(ctx, cmds)
	}
}
