package feed

import (
	"context"
	"strconv"
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

// feedVideoAtMicro 造一条已发布视频。关注流不按视频的 created_at 排序(它按 ZSET
// score = published_at),这里的 created_at 只是给卡片填个值,分页顺序由 seedZSet 控制。
// authorID 参与「按当前关注集合过滤」:作者不在当前关注列表里的条目会被丢掉
func feedVideoAtMicro(id, authorID, micros int64) *video.Video {
	return &video.Video{ID: id, AuthorID: authorID, CreatedAt: time.UnixMicro(micros), Status: video.StatusPublished}
}

// newFollowingService 起一个带 miniredis 的关注流服务
func newFollowingService(t *testing.T, videos []*video.Video, followees FolloweeLister, bigvs BigVChecker) (*FeedService, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := NewFeedService(&pagedFeedRepo{videos: videos}, rdb, fakeLikes{}, followees, bigvs)
	return svc, mr, rdb
}

// seedZSet 直接往一个 ZSET 里塞 member=video_id 字符串、score=微秒 的条目。
// 绕过 push 脚本,才能把同一 score 的成员按我们想要的顺序摆进去
func seedZSet(t *testing.T, rdb *redis.Client, key string, idToScore map[int64]int64) {
	t.Helper()
	ctx := context.Background()
	for id, score := range idToScore {
		require.NoError(t, rdb.ZAdd(ctx, key, redis.Z{
			Score:  float64(score),
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

	svc := NewFeedService(&pagedFeedRepo{}, rdb, fakeLikes{}, nil, nil)

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
		svc := NewFeedService(&pagedFeedRepo{}, nil, fakeLikes{}, fakeFollowees{}, nil)
		_, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, 1)
		assert.ErrorIs(t, err, errs.ErrInternalCache)
	})

	t.Run("未登录返回未授权", func(t *testing.T) {
		svc := NewFeedService(&pagedFeedRepo{}, nil, fakeLikes{}, fakeFollowees{}, nil)
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
	assert.Equal(t, int64(3_000_000), page1.NextCursor.CreatedAt, "游标取最后消费条目的 score")
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
	assert.Equal(t, int64(4_000_000), page1.NextCursor.CreatedAt)
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
	)
	seedZSet(t, rdb, inboxKey(uid), map[int64]int64{100: 2_000_000, 200: 1_000_000})

	resp, err := svc.ListFollowing(ctx, ListFollowingReq{Limit: 10}, uid)
	require.NoError(t, err, "点赞查询失败不该让关注流报错")
	assert.Equal(t, []int64{100, 200}, itemIDs(resp.Items), "列表内容应照常返回")
	for i, it := range resp.Items {
		assert.Nil(t, it.IsLike, "第 %d 条未算出点赞态,IsLike 应为 nil(DTO 允许「未计算」)", i)
	}
}
