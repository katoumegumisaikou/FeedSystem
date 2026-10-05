package feed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/model/video"
	"feed-system/internal/pkg/errs"
)

type latestFeedRepo struct {
	items []*video.Video
}

func (r latestFeedRepo) ListLatestVideos(context.Context, time.Time, int64, int) ([]*video.Video, error) {
	return r.items, nil
}

func (r latestFeedRepo) ListVideosByLikes(context.Context, int64, int64, int) ([]*video.Video, error) {
	return r.items, nil
}

// ListVideosByIDs 按 ID 过滤。真实实现只返回已发布的,这里的数据本来就都是已发布的,
// 不再重复判状态。顺序不保证 —— 和真实实现一致,调用方要自己按原顺序拼
func (r latestFeedRepo) ListVideosByIDs(_ context.Context, ids []int64) ([]*video.Video, error) {
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []*video.Video
	for _, v := range r.items {
		if want[v.ID] {
			out = append(out, v)
		}
	}
	return out, nil
}

// ListRecentPublishedVideoIDs 补拉接口的内存实现:按作者过滤、按 published_at 倒序取前 n 条,
// 与真实实现语义一致(测试不覆盖补拉,但接口必须完整实现)
func (r latestFeedRepo) ListRecentPublishedVideoIDs(_ context.Context, authorID int64, n int) ([]VideoRef, error) {
	var out []VideoRef
	for _, v := range r.items {
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

func TestRegisterRouter_ListLatest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	miniRedis := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: miniRedis.Addr()})
	t.Cleanup(func() {
		if err := rdb.Close(); err != nil {
			t.Errorf("关闭测试 Redis 客户端失败: %v", err)
		}
	})

	repo := latestFeedRepo{items: []*video.Video{{
		ID:        1,
		CreatedAt: time.Now().Add(-time.Minute),
		Status:    video.StatusPublished,
	}}}
	svc := NewFeedService(repo, rdb, nil, nil, nil)
	router := gin.New()
	v1 := router.Group("/api/v1")
	RegisterRouter(v1, NewFeedHandler(svc), nil, rdb)
	// 同时注册 video 路由,确认原有 /videos/latest 不会被 /videos/:id 抢走。
	video.RegisterRouter(v1, video.NewVideoHandler(video.NewVideoService(nil, nil, nil)), nil, rdb)

	tests := []struct {
		name     string
		query    string
		wantCode int
	}{
		{name: "公开路由返回最新视频", query: "?limit=1", wantCode: 0},
		{name: "limit 超出范围返回参数错误", query: "?limit=101", wantCode: errs.ErrInvalidParam.Code},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/videos/latest"+tt.query, nil)
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			assert.Equal(t, http.StatusOK, resp.Code)
			var body struct {
				Code int             `json:"code"`
				Data *ListLatestResp `json:"data"`
			}
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
			assert.Equal(t, tt.wantCode, body.Code)
			if tt.wantCode == 0 {
				require.NotNil(t, body.Data)
				require.Len(t, body.Data.Items, 1)
				assert.Equal(t, int64(1), body.Data.Items[0].ID)
			}
		})
	}
}
