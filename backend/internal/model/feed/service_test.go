package feed

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/model/video"
)

// pagedFeedRepo 遵守 (before, beforeID) 和 limit 的内存仓储。
//
// handler_test 里那个 latestFeedRepo 把这几个参数全忽略了,所以只能验「返回了什么」,
// 验不了分页 —— hasMore 和复合游标都必须让仓储真的按参数筛选/截断才测得出来
type pagedFeedRepo struct {
	videos []*video.Video
}

func (r *pagedFeedRepo) ListLatestVideos(_ context.Context, before time.Time, beforeID int64, limit int) ([]*video.Video, error) {
	var out []*video.Video
	for _, v := range r.videos {
		// beforeID 为 0 表示首页,不设边界 —— 和 SQL 里那条 if 对应
		if beforeID > 0 && !older(v.CreatedAt, v.ID, before, beforeID) {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *pagedFeedRepo) ListVideosByLikes(context.Context, int64, int64, int) ([]*video.Video, error) {
	return nil, nil
}

// ListVideosByIDs 按 ID 过滤,顺序不保证 —— 和真实实现一致,调用方自己按原顺序拼
func (r *pagedFeedRepo) ListVideosByIDs(_ context.Context, ids []int64) ([]*video.Video, error) {
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []*video.Video
	for _, v := range r.videos {
		if want[v.ID] {
			out = append(out, v)
		}
	}
	return out, nil
}

// ListRecentPublishedVideoIDs 补拉接口的内存实现:按作者过滤、按 published_at 倒序取前 n 条,
// 与真实实现语义一致(测试不覆盖补拉,但接口必须完整实现)
func (r *pagedFeedRepo) ListRecentPublishedVideoIDs(_ context.Context, authorID int64, n int) ([]VideoRef, error) {
	var out []VideoRef
	for _, v := range r.videos {
		// published_at 列可空,真实实现按指针扫描并靠调用方跳过 NULL;这里同样只收非 NULL
		if v.AuthorID != authorID || v.Status != video.StatusPublished || v.PublishedAt == nil {
			continue
		}
		out = append(out, VideoRef{ID: v.ID, PublishedAt: v.PublishedAt})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].PublishedAt.Equal(*out[j].PublishedAt) {
			return out[i].PublishedAt.After(*out[j].PublishedAt)
		}
		return out[i].ID > out[j].ID
	})
	if n >= 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// newLatestService userID 传 0 走匿名分支,setLikeStatuses 会直接返回,
// 所以第三个参数(likes 提供方)可以传 nil
func newLatestService(t *testing.T, videos []*video.Video) *FeedService {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewFeedService(&pagedFeedRepo{videos: videos}, rdb, nil, nil, nil)
}

func publishedAt(id int64, at time.Time) *video.Video {
	return &video.Video{ID: id, Title: "v", CreatedAt: at, Status: video.StatusPublished}
}

// pageWith 把上一页返回的游标塞回请求,模拟前端翻页
func pageWith(req ListLatestReq, c *FeedCursor) ListLatestReq {
	req.CursorCreatedAt = &c.CreatedAt
	req.CursorVideoID = &c.VideoID
	return req
}

// TestListLatest_NextCursor 锁住「next_cursor 只在确实还有下一页时才给」。
//
// 改之前是「这页只要有数据就给」,于是最后一页也会带一个游标,
// 前端得再请求一次空页才知道到底了
func TestListLatest_NextCursor(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("还有更多时给游标", func(t *testing.T) {
		svc := newLatestService(t, []*video.Video{
			publishedAt(3, now.Add(-time.Minute)),
			publishedAt(2, now.Add(-2*time.Minute)),
			publishedAt(1, now.Add(-3*time.Minute)),
		})

		page1, err := svc.ListLatest(ctx, ListLatestReq{Limit: 2}, 0)
		require.NoError(t, err)
		require.Len(t, page1.Items, 2, "limit=2 就只给 2 条,多要的那条不进响应")
		assert.Equal(t, int64(3), page1.Items[0].ID)
		assert.Equal(t, int64(2), page1.Items[1].ID)
		require.NotNil(t, page1.NextCursor, "后面还剩一条,该给游标")

		page2, err := svc.ListLatest(ctx, pageWith(ListLatestReq{Limit: 2}, page1.NextCursor), 0)
		require.NoError(t, err)
		require.Len(t, page2.Items, 1)
		assert.Equal(t, int64(1), page2.Items[0].ID)
		assert.Nil(t, page2.NextCursor, "取完了,游标该是 nil")
	})

	t.Run("正好取满时不留游标", func(t *testing.T) {
		svc := newLatestService(t, []*video.Video{
			publishedAt(2, now.Add(-time.Minute)),
			publishedAt(1, now.Add(-2*time.Minute)),
		})

		resp, err := svc.ListLatest(ctx, ListLatestReq{Limit: 2}, 0)
		require.NoError(t, err)
		require.Len(t, resp.Items, 2)
		assert.Nil(t, resp.NextCursor,
			"正好取满且后面没有了 —— 不该给个假游标让前端白跑一趟")
	})

	t.Run("没有视频时没有游标", func(t *testing.T) {
		resp, err := newLatestService(t, nil).ListLatest(ctx, ListLatestReq{Limit: 2}, 0)
		require.NoError(t, err)
		assert.NotNil(t, resp.Items)
		assert.Empty(t, resp.Items)
		assert.Nil(t, resp.NextCursor)
	})

	// 这条是复合游标存在的理由。三条视频同一时刻发布,只带时间戳的旧游标
	// 在第二页会问「created_at < 那个时刻」→ 一条都不满足 → 第三条被整个丢掉
	t.Run("同一时刻发布的多条不会跨页漏掉", func(t *testing.T) {
		at := now.Add(-time.Minute)
		svc := newLatestService(t, []*video.Video{
			publishedAt(3, at),
			publishedAt(2, at),
			publishedAt(1, at),
		})

		page1, err := svc.ListLatest(ctx, ListLatestReq{Limit: 2}, 0)
		require.NoError(t, err)
		require.Len(t, page1.Items, 2)
		assert.Equal(t, int64(3), page1.Items[0].ID, "同一时刻按 id 倒序")
		assert.Equal(t, int64(2), page1.Items[1].ID)
		require.NotNil(t, page1.NextCursor)

		page2, err := svc.ListLatest(ctx, pageWith(ListLatestReq{Limit: 2}, page1.NextCursor), 0)
		require.NoError(t, err)
		require.Len(t, page2.Items, 1, "第三条不能因为和前两条同一时刻就被丢掉")
		assert.Equal(t, int64(1), page2.Items[0].ID)
		assert.Nil(t, page2.NextCursor)
	})

	t.Run("只传一半的游标被判为参数无效", func(t *testing.T) {
		at := now.Add(-time.Minute).UnixMicro()
		svc := newLatestService(t, nil)

		_, err := svc.ListLatest(ctx, ListLatestReq{Limit: 2, CursorCreatedAt: &at}, 0)
		assert.Error(t, err, "只给 created_at 不给 id 的话边界不完整")

		id := int64(7)
		_, err = svc.ListLatest(ctx, ListLatestReq{Limit: 2, CursorVideoID: &id}, 0)
		assert.Error(t, err)
	})
}
