package video

import (
	"context"
	"errors"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/pkg/errs"
)

func TestValidCoverURL(t *testing.T) {
	allow := []string{
		"/covers/abc.png",
		"/covers/2026/09/x.jpg",
	}
	// 这几种一旦放进去,前端拿它当 img src 就会出事
	reject := []string{
		"",                          // 空串由调用方跳过,不归这个函数管
		"covers/abc.png",            // 缺前导 /
		"/covers",                   // 只有前缀,没有具体文件
		"/coversabc/x.png",          // 前缀不完整
		"/covers/../../etc/passwd",  // 目录穿越
		"http://evil.com/x.png",     // 外站
		"//evil.com/x.png",          // 协议相对,浏览器会当外站
		"javascript:alert(1)",       // 直接就是脚本
		"data:image/svg+xml,<svg/>", // data URL
	}

	for _, u := range allow {
		assert.True(t, validCoverURL(u), "应放行: %s", u)
	}
	for _, u := range reject {
		assert.False(t, validCoverURL(u), "应拦下: %s", u)
	}
}

func TestVideoIDParam(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		param   string
		wantID  int64
		wantErr bool
	}{
		{"7", 7, false},
		{"0", 0, true},                    // 0 不是合法主键
		{"-1", 0, true},                   // 负数
		{"abc", 0, true},                  // 非数字
		{"", 0, true},                     // 参数缺失
		{"99999999999999999999", 0, true}, // 超出 int64
	}

	for _, tc := range cases {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Params = gin.Params{{Key: "id", Value: tc.param}}

		id, err := videoIDParam(c)
		if tc.wantErr {
			assert.Error(t, err, "应报错: %q", tc.param)
			continue
		}
		assert.NoError(t, err, "不该报错: %q", tc.param)
		assert.Equal(t, tc.wantID, id)
	}
}

// fakeVideoRepo 内存版 VideoRepository。
// 仓储提成接口后,归属与状态这类业务规则不用连 PG 就能测
type fakeVideoRepo struct {
	videos     map[int64]*Video
	lastFields map[string]any // 记下最后一次写了哪些列
	updateErr  error

	records []*PlayRecord // 记下写进来的播放流水,同时当观看历史的查询源
	saveErr error

	findByIDCalls  int               // FindVideoByID 被调了几次,用于断言详情缓存有没有挡住回源
	likedByUser    map[int64][]int64 // 各用户点赞过的视频,用于测点赞状态没被缓存串号
	listLikedCalls int

	playURLCalls   int // FindVideoByPlayURL 被调了几次,用于断言缓存有没有挡住回源
	listHistoryErr error
}

func (f *fakeVideoRepo) CreateVideo(ctx context.Context, v *Video) error { return nil }

func (f *fakeVideoRepo) FindVideoByID(ctx context.Context, id int64) (*Video, error) {
	f.findByIDCalls++
	if v, ok := f.videos[id]; ok {
		return v, nil
	}
	return nil, ErrNotFound
}

// FindVideoByPlayURL 全表遍历 —— 真实实现走 idx_videos_play_url 索引,这里数据量是个位数
func (f *fakeVideoRepo) FindVideoByPlayURL(ctx context.Context, playURL string) (*Video, error) {
	f.playURLCalls++
	for _, v := range f.videos {
		if v.PlayURL == playURL {
			return v, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeVideoRepo) ListLikedVideoIDs(ctx context.Context, userID int64, videoIDs []int64) ([]int64, error) {
	f.listLikedCalls++
	out := []int64{}
	for _, liked := range f.likedByUser[userID] {
		for _, want := range videoIDs {
			if liked == want {
				out = append(out, liked)
				break
			}
		}
	}
	return out, nil
}

// FindVideosByIDs 顺序不保证,和真实实现一致 —— 调用方必须自己按原顺序拼
func (f *fakeVideoRepo) FindVideosByIDs(ctx context.Context, ids []int64) ([]*Video, error) {
	var out []*Video
	for _, id := range ids {
		if v, ok := f.videos[id]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// olderPlay 与 SQL 的 (created_at, video_id) < (?, ?) 同一语义:先比时间,
// 时间相同再比 id。时间比到微秒 —— 和列精度对齐
func olderPlay(at time.Time, videoID int64, thanAt time.Time, thanVideoID int64) bool {
	am, bm := at.UnixMicro(), thanAt.UnixMicro()
	if am != bm {
		return am < bm
	}
	return videoID < thanVideoID
}

// ListPlayHistory 内存实现,语义与 SQL 版对齐:先按 video_id 收敛成最近一条,
// 再按游标过滤、排序、截断。只返回已发布的视频(真实实现是在 JOIN 时过滤的)
func (f *fakeVideoRepo) ListPlayHistory(_ context.Context, userID int64, before time.Time, beforeVideoID int64, limit int) ([]*PlayHistoryEntry, error) {
	if f.listHistoryErr != nil {
		return nil, f.listHistoryErr
	}

	latest := map[int64]*PlayRecord{}
	for _, r := range f.records {
		if r.UserID != userID {
			continue
		}
		if cur, ok := latest[r.VideoID]; !ok || r.CreatedAt.After(cur.CreatedAt) {
			latest[r.VideoID] = r
		}
	}

	var out []*PlayHistoryEntry
	for _, r := range latest {
		// 收敛之后才按游标过滤 —— 顺序反了会把同一视频的旧记录留到下一页
		if beforeVideoID > 0 && !olderPlay(r.CreatedAt, r.VideoID, before, beforeVideoID) {
			continue
		}
		if v, ok := f.videos[r.VideoID]; !ok || v.Status != StatusPublished {
			continue
		}
		out = append(out, &PlayHistoryEntry{
			VideoID:   r.VideoID,
			Watched:   r.Watched,
			Duration:  r.Duration,
			WatchedAt: r.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		am, bm := out[i].WatchedAt.UnixMicro(), out[j].WatchedAt.UnixMicro()
		if am != bm {
			return am > bm
		}
		return out[i].VideoID > out[j].VideoID
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SavePlayReport 真实实现里插流水和 play_count 自增在同一个事务,这里也一起做
func (f *fakeVideoRepo) SavePlayReport(ctx context.Context, r *PlayRecord) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.records = append(f.records, r)
	if v, ok := f.videos[r.VideoID]; ok {
		v.PlayCount++
	}
	return nil
}

// UpdateVideoFields 真实实现按 map 逐列写库。这里也把值落到内存实体上 ——
// 不落的话「写完再读」的用例(缓存失效就是靠它验的)会读到旧值,测不出真问题
func (f *fakeVideoRepo) UpdateVideoFields(ctx context.Context, id int64, fields map[string]any) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.lastFields = fields

	v, ok := f.videos[id]
	if !ok {
		return nil
	}
	for col, val := range fields {
		switch col {
		case "title":
			v.Title, _ = val.(string)
		case "description":
			v.Description, _ = val.(string)
		case "cover_url":
			v.CoverURL, _ = val.(string)
		case "status":
			v.Status, _ = val.(int8)
		}
	}
	return nil
}

const testUserID int64 = 7

func draftVideo() *Video {
	return &Video{
		ID:       1,
		AuthorID: testUserID,
		Title:    "原始文件名.mp4",
		PlayURL:  "/videos/7/a.mp4",
		Status:   StatusDraft,
	}
}

// newTestService 只测编辑与发布这两条路径 —— 它们不碰 rdb / users,所以传 nil
func newTestService(v *Video) (*VideoService, *fakeVideoRepo) {
	repo := &fakeVideoRepo{videos: map[int64]*Video{}}
	if v != nil {
		repo.videos[v.ID] = v
	}
	return NewVideoService(repo, nil, nil), repo
}

// newCachedTestService 带 Redis 的服务实例。详情缓存要真的走一遍 GET/SET ——
// 客户端传 nil 只能测降级,测不出「缓存到底挡没挡住回源」
func newCachedTestService(t *testing.T, v *Video) (*VideoService, *fakeVideoRepo, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := &fakeVideoRepo{videos: map[int64]*Video{}}
	if v != nil {
		repo.videos[v.ID] = v
	}
	return NewVideoService(repo, rdb, nil), repo, mr
}

func assertCode(t *testing.T, err error, want errs.ServiceErr) {
	t.Helper()
	require.Error(t, err)
	got, ok := errs.As(err)
	require.True(t, ok, "应该返回 ServiceErr,实际 %T: %v", err, err)
	assert.Equal(t, want.Code, got.Code, "错误码不符: %v", err)
}

func TestUpdateVideo(t *testing.T) {
	ctx := context.Background()

	t.Run("更新标题与简介", func(t *testing.T) {
		svc, repo := newTestService(draftVideo())

		resp, err := svc.UpdateVideo(ctx, 1, testUserID, UpdateVideoReq{Title: "新标题", Description: "简介"})
		require.NoError(t, err)
		assert.Equal(t, "新标题", resp.Title)
		assert.Equal(t, "简介", resp.Description)
		assert.Equal(t, "新标题", repo.lastFields["title"])
	})

	t.Run("cover_url 留空时不动原封面", func(t *testing.T) {
		v := draftVideo()
		v.CoverURL = "/covers/old.png"
		svc, repo := newTestService(v)

		resp, err := svc.UpdateVideo(ctx, 1, testUserID, UpdateVideoReq{Title: "t"})
		require.NoError(t, err)
		assert.Equal(t, "/covers/old.png", resp.CoverURL)
		_, wrote := repo.lastFields["cover_url"]
		assert.False(t, wrote, "留空就不该把 cover_url 放进更新列")
	})

	t.Run("封面不是站内路径被拒", func(t *testing.T) {
		svc, _ := newTestService(draftVideo())
		for _, bad := range []string{"http://evil.com/x.png", "javascript:alert(1)", "/covers/../secret"} {
			_, err := svc.UpdateVideo(ctx, 1, testUserID, UpdateVideoReq{Title: "t", CoverURL: bad})
			assertCode(t, err, errs.ErrInvalidParam)
		}
	})

	t.Run("非作者 403", func(t *testing.T) {
		svc, _ := newTestService(draftVideo())
		_, err := svc.UpdateVideo(ctx, 1, 999, UpdateVideoReq{Title: "t"})
		assertCode(t, err, errs.ErrForbidden)
	})

	t.Run("视频不存在 404", func(t *testing.T) {
		svc, _ := newTestService(nil)
		_, err := svc.UpdateVideo(ctx, 42, testUserID, UpdateVideoReq{Title: "t"})
		assertCode(t, err, errs.ErrNotFound)
	})

	t.Run("已下架不能编辑", func(t *testing.T) {
		v := draftVideo()
		v.Status = StatusRemoved
		svc, _ := newTestService(v)
		_, err := svc.UpdateVideo(ctx, 1, testUserID, UpdateVideoReq{Title: "t"})
		assertCode(t, err, errs.ErrConflict)
	})

	t.Run("仓储出错", func(t *testing.T) {
		svc, repo := newTestService(draftVideo())
		repo.updateErr = errors.New("boom")
		_, err := svc.UpdateVideo(ctx, 1, testUserID, UpdateVideoReq{Title: "t"})
		assertCode(t, err, errs.ErrInternal)
	})
}

func TestPublishVideo(t *testing.T) {
	ctx := context.Background()

	t.Run("草稿可以发布", func(t *testing.T) {
		svc, repo := newTestService(draftVideo())

		resp, err := svc.PublishVideo(ctx, 1, testUserID)
		require.NoError(t, err)
		assert.Equal(t, StatusPublished, resp.Status)
		assert.Equal(t, StatusPublished, repo.lastFields["status"])
	})

	t.Run("重复发布幂等且不再写库", func(t *testing.T) {
		v := draftVideo()
		v.Status = StatusPublished
		svc, repo := newTestService(v)

		resp, err := svc.PublishVideo(ctx, 1, testUserID)
		require.NoError(t, err)
		assert.Equal(t, StatusPublished, resp.Status)
		assert.Nil(t, repo.lastFields, "已发布的再调一次不该产生写操作")
	})

	t.Run("已下架不能发布", func(t *testing.T) {
		v := draftVideo()
		v.Status = StatusRemoved
		svc, _ := newTestService(v)
		_, err := svc.PublishVideo(ctx, 1, testUserID)
		assertCode(t, err, errs.ErrConflict)
	})

	t.Run("非作者 403", func(t *testing.T) {
		svc, _ := newTestService(draftVideo())
		_, err := svc.PublishVideo(ctx, 1, 999)
		assertCode(t, err, errs.ErrForbidden)
	})
}

// TestGetVideoDetail 锁住可见性规则:未发布的一律 404,作者本人除外。
// 这里是安全相关的 —— 放开了就等于草稿能被遍历 ID 探到
func TestGetVideoDetail(t *testing.T) {
	ctx := context.Background()

	t.Run("已发布对匿名可见", func(t *testing.T) {
		v := draftVideo()
		v.Status = StatusPublished
		svc, _ := newTestService(v)

		resp, err := svc.GetVideoDetail(ctx, 1, 0) // requesterID 0 = 匿名
		require.NoError(t, err)
		assert.Equal(t, int64(1), resp.ID)
	})

	t.Run("草稿匿名看不到", func(t *testing.T) {
		svc, _ := newTestService(draftVideo())
		_, err := svc.GetVideoDetail(ctx, 1, 0)
		assertCode(t, err, errs.ErrNotFound)
	})

	t.Run("草稿对非作者看不到", func(t *testing.T) {
		svc, _ := newTestService(draftVideo())
		_, err := svc.GetVideoDetail(ctx, 1, 999)
		assertCode(t, err, errs.ErrNotFound)
	})

	t.Run("草稿对作者可见", func(t *testing.T) {
		svc, _ := newTestService(draftVideo())

		resp, err := svc.GetVideoDetail(ctx, 1, testUserID)
		require.NoError(t, err)
		assert.Equal(t, StatusDraft, resp.Status)
	})

	t.Run("已下架匿名看不到但作者可见", func(t *testing.T) {
		v := draftVideo()
		v.Status = StatusRemoved
		svc, _ := newTestService(v)

		_, err := svc.GetVideoDetail(ctx, 1, 0)
		assertCode(t, err, errs.ErrNotFound)

		_, err = svc.GetVideoDetail(ctx, 1, testUserID)
		assert.NoError(t, err)
	})

	t.Run("不存在 404", func(t *testing.T) {
		svc, _ := newTestService(nil)
		_, err := svc.GetVideoDetail(ctx, 42, 0)
		assertCode(t, err, errs.ErrNotFound)
	})

	t.Run("ID 非法 400", func(t *testing.T) {
		svc, _ := newTestService(draftVideo())
		_, err := svc.GetVideoDetail(ctx, 0, 0)
		assertCode(t, err, errs.ErrInvalidParam)
	})
}

// TestGetVideoDetailCache 覆盖详情缓存的几条边界:命中不回源、点赞状态不串号、
// 写操作让缓存失效、Redis 不可用时降级直查库
func TestGetVideoDetailCache(t *testing.T) {
	ctx := context.Background()

	t.Run("第二次请求命中缓存不再回源", func(t *testing.T) {
		svc, repo, _ := newCachedTestService(t, publishedVideo())

		_, err := svc.GetVideoDetail(ctx, 1, 0)
		require.NoError(t, err)
		_, err = svc.GetVideoDetail(ctx, 1, 0)
		require.NoError(t, err)

		assert.Equal(t, 1, repo.findByIDCalls, "第二次应命中缓存")
	})

	// 这是整个改动最容易写错的地方:IsLike 是 per-user 的。
	// 缓存实体没问题,把响应一起缓存就会把上一个用户的点赞状态发给下一个
	t.Run("点赞状态不随实体一起缓存", func(t *testing.T) {
		svc, repo, _ := newCachedTestService(t, publishedVideo())
		repo.likedByUser = map[int64][]int64{42: {1}}

		// 没点赞的用户先把实体灌进缓存
		other, err := svc.GetVideoDetail(ctx, 1, 99)
		require.NoError(t, err)
		require.NotNil(t, other.IsLike)
		assert.False(t, *other.IsLike, "用户 99 没点过赞")

		// 点过赞的用户再请求:实体来自缓存,但点赞状态必须现算
		liked, err := svc.GetVideoDetail(ctx, 1, 42)
		require.NoError(t, err)
		require.NotNil(t, liked.IsLike)
		assert.True(t, *liked.IsLike, "点赞状态被缓存串号了")
		assert.Equal(t, 2, repo.listLikedCalls, "点赞状态应每次都查,不能跟着实体进缓存")
	})

	t.Run("编辑后缓存失效", func(t *testing.T) {
		svc, _, _ := newCachedTestService(t, publishedVideo())

		_, err := svc.GetVideoDetail(ctx, 1, 0) // 灌缓存
		require.NoError(t, err)

		_, err = svc.UpdateVideo(ctx, 1, testUserID, UpdateVideoReq{Title: "新标题"})
		require.NoError(t, err)

		resp, err := svc.GetVideoDetail(ctx, 1, 0)
		require.NoError(t, err)
		assert.Equal(t, "新标题", resp.Title, "编辑后还在读旧缓存")
	})

	t.Run("发布后缓存失效", func(t *testing.T) {
		svc, _, _ := newCachedTestService(t, draftVideo())

		_, err := svc.GetVideoDetail(ctx, 1, testUserID) // 作者看草稿,把 Draft 灌进缓存
		require.NoError(t, err)

		_, err = svc.PublishVideo(ctx, 1, testUserID)
		require.NoError(t, err)

		// 匿名能看到 —— 缓存里还留着 Draft 的话这里会 404
		resp, err := svc.GetVideoDetail(ctx, 1, 0)
		require.NoError(t, err)
		assert.Equal(t, StatusPublished, resp.Status)
	})

	// rdb 传 nil 正是 main.go 里 Redis 连不上时的形态,mustRedisClient 返回 nil
	t.Run("Redis 不可用时降级直查库", func(t *testing.T) {
		svc, _ := newTestService(publishedVideo())

		resp, err := svc.GetVideoDetail(ctx, 1, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(1), resp.ID)
	})

	t.Run("Redis 报错时降级直查库", func(t *testing.T) {
		svc, _, mr := newCachedTestService(t, publishedVideo())
		mr.SetError("boom") // 之后所有命令都返回这个错误

		resp, err := svc.GetVideoDetail(ctx, 1, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(1), resp.ID)
	})
}

// publishedVideo 已发布的视频,用于播放上报
func publishedVideo() *Video {
	v := draftVideo()
	v.Status = StatusPublished
	return v
}

// TestVisibleTo 覆盖可见性判定本身 —— 它是文件路由与上报接口共用的那一处口径,
// 两条路径的测试都建立在它正确的前提上
func TestVisibleTo(t *testing.T) {
	const other int64 = 999
	cases := []struct {
		name          string
		status        int8
		wantGuest     bool
		wantAuthor    bool
		wantOtherUser bool
	}{
		{"已发布对所有人可见", StatusPublished, true, true, true},
		{"已下架对所有人不可见(含作者)", StatusRemoved, false, false, false},
		{"草稿只对作者可见", StatusDraft, false, true, false},
		{"转码中只对作者可见", StatusTranscoding, false, true, false},
		{"转码失败只对作者可见", StatusTranscodeFailed, false, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantGuest, visibleTo(tc.status, testUserID, 0), "游客")
			assert.Equal(t, tc.wantAuthor, visibleTo(tc.status, testUserID, testUserID), "作者")
			assert.Equal(t, tc.wantOtherUser, visibleTo(tc.status, testUserID, other), "他人")
		})
	}
}

func TestReportPlay(t *testing.T) {
	ctx := context.Background()
	const ip = "203.0.113.9"

	t.Run("已发布视频:落一条完整流水", func(t *testing.T) {
		svc, repo := newTestService(publishedVideo())

		err := svc.ReportPlay(ctx, 1, testUserID, PlayReportReq{Watched: 30, Duration: 120}, ip)
		require.NoError(t, err)
		require.Len(t, repo.records, 1)

		rec := repo.records[0]
		assert.Equal(t, testUserID, rec.UserID)
		assert.Equal(t, int64(1), rec.VideoID)
		assert.Equal(t, testUserID, rec.AuthorID, "author_id 是冗余存的,应等于视频作者")
		assert.Equal(t, 30, rec.Watched)
		assert.Equal(t, 120, rec.Duration)
		assert.Equal(t, ip, rec.IP)
	})

	t.Run("游客 user_id 落 0 也记", func(t *testing.T) {
		svc, repo := newTestService(publishedVideo())

		err := svc.ReportPlay(ctx, 1, 0, PlayReportReq{Watched: 5, Duration: 120}, ip)
		require.NoError(t, err)
		require.Len(t, repo.records, 1)
		assert.Equal(t, int64(0), repo.records[0].UserID)
	})

	t.Run("上报后 play_count 自增", func(t *testing.T) {
		v := publishedVideo()
		svc, repo := newTestService(v)
		require.Equal(t, int64(0), v.PlayCount)

		require.NoError(t, svc.ReportPlay(ctx, 1, 0, PlayReportReq{Watched: 1, Duration: 2}, ip))
		assert.Equal(t, int64(1), repo.videos[1].PlayCount)
	})

	t.Run("草稿:作者可上报", func(t *testing.T) {
		svc, repo := newTestService(draftVideo())
		require.NoError(t, svc.ReportPlay(ctx, 1, testUserID, PlayReportReq{Watched: 1, Duration: 2}, ip))
		assert.Len(t, repo.records, 1)
	})

	t.Run("草稿:游客与他人不可上报", func(t *testing.T) {
		for _, requester := range []int64{0, 999} {
			svc, repo := newTestService(draftVideo())
			err := svc.ReportPlay(ctx, 1, requester, PlayReportReq{Watched: 1, Duration: 2}, ip)
			assertCode(t, err, errs.ErrNotFound)
			assert.Empty(t, repo.records, "看不见的视频不该留下流水")
		}
	})

	t.Run("已下架:连作者也不能上报", func(t *testing.T) {
		v := draftVideo()
		v.Status = StatusRemoved
		svc, repo := newTestService(v)

		err := svc.ReportPlay(ctx, 1, testUserID, PlayReportReq{Watched: 1, Duration: 2}, ip)
		assertCode(t, err, errs.ErrNotFound)
		assert.Empty(t, repo.records)
	})

	t.Run("watched 超过 duration 被拒", func(t *testing.T) {
		svc, repo := newTestService(publishedVideo())

		err := svc.ReportPlay(ctx, 1, 0, PlayReportReq{Watched: 121, Duration: 120}, ip)
		assertCode(t, err, errs.ErrInvalidParam)
		assert.Empty(t, repo.records, "被拒的上报不该留下流水")
	})

	t.Run("watched 等于 duration 是允许的(正好看完)", func(t *testing.T) {
		svc, _ := newTestService(publishedVideo())
		require.NoError(t, svc.ReportPlay(ctx, 1, 0, PlayReportReq{Watched: 120, Duration: 120}, ip))
	})

	t.Run("视频不存在 404", func(t *testing.T) {
		svc, _ := newTestService(nil)
		err := svc.ReportPlay(ctx, 42, 0, PlayReportReq{Watched: 1, Duration: 2}, ip)
		assertCode(t, err, errs.ErrNotFound)
	})

	t.Run("仓储出错 500", func(t *testing.T) {
		svc, repo := newTestService(publishedVideo())
		repo.saveErr = errors.New("boom")

		err := svc.ReportPlay(ctx, 1, 0, PlayReportReq{Watched: 1, Duration: 2}, ip)
		assertCode(t, err, errs.ErrInternal)
	})
}

// TestListHistory 观看历史。重点在两处:
//   - 同一个视频看多次只出一条(play_records 是流水表,直接吐会重复)
//   - 只拿得到自己的记录(user_id 是过滤条件,不是装饰)
func TestListHistory(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	pub := func(id int64, at time.Time) *Video {
		v := draftVideo()
		v.ID = id
		v.Status = StatusPublished
		v.CreatedAt = at
		return v
	}
	rec := func(videoID, watched int, at time.Time) *PlayRecord {
		return &PlayRecord{
			UserID: testUserID, VideoID: int64(videoID),
			Watched: watched, Duration: 120, CreatedAt: at,
		}
	}
	// historyRepo 直接塞 records,不走上报接口 —— 这样才能精确控制 created_at
	historyRepo := func(videos []*Video, records []*PlayRecord) *fakeVideoRepo {
		repo := &fakeVideoRepo{videos: map[int64]*Video{}}
		for _, v := range videos {
			repo.videos[v.ID] = v
		}
		repo.records = records
		return repo
	}
	svcOf := func(repo *fakeVideoRepo) *VideoService { return NewVideoService(repo, nil, nil) }
	// historyPage 把上一页返回的游标塞回请求,模拟前端翻页
	historyPage := func(req ListHistoryReq, c *HistoryCursor) ListHistoryReq {
		req.CursorWatchedAt = &c.WatchedAt
		req.CursorVideoID = &c.VideoID
		return req
	}

	t.Run("游客 401", func(t *testing.T) {
		_, err := svcOf(historyRepo(nil, nil)).ListHistory(ctx, 0, ListHistoryReq{})
		assertCode(t, err, errs.ErrUnauthorized)
	})

	t.Run("没有记录时返回空列表而不是 nil", func(t *testing.T) {
		resp, err := svcOf(historyRepo(nil, nil)).ListHistory(ctx, testUserID, ListHistoryReq{})
		require.NoError(t, err)
		assert.NotNil(t, resp.Items, "空页也要是 [],前端才不用判空")
		assert.Empty(t, resp.Items)
		assert.Nil(t, resp.NextCursor)
	})

	t.Run("按最近观看时间倒序", func(t *testing.T) {
		repo := historyRepo(
			[]*Video{pub(1, base), pub(2, base)},
			[]*PlayRecord{
				rec(1, 30, base.Add(-2*time.Hour)),
				rec(2, 90, base.Add(-1*time.Hour)),
			},
		)

		resp, err := svcOf(repo).ListHistory(ctx, testUserID, ListHistoryReq{})
		require.NoError(t, err)
		require.Len(t, resp.Items, 2)
		assert.Equal(t, int64(2), resp.Items[0].ID, "最近看的排前面")
		assert.Equal(t, 90, resp.Items[0].Watched)
		assert.Equal(t, base.Add(-1*time.Hour), resp.Items[0].WatchedAt)
		assert.Equal(t, int64(1), resp.Items[1].ID)
	})

	t.Run("同一个视频看多次只出一条,取最近那次", func(t *testing.T) {
		repo := historyRepo(
			[]*Video{pub(1, base)},
			[]*PlayRecord{
				rec(1, 10, base.Add(-3*time.Hour)),
				rec(1, 80, base.Add(-1*time.Hour)), // 最近
				rec(1, 50, base.Add(-2*time.Hour)),
			},
		)

		resp, err := svcOf(repo).ListHistory(ctx, testUserID, ListHistoryReq{})
		require.NoError(t, err)
		require.Len(t, resp.Items, 1, "同一个视频只该出现一条")
		assert.Equal(t, 80, resp.Items[0].Watched, "进度取最近那次")
		assert.Equal(t, base.Add(-1*time.Hour), resp.Items[0].WatchedAt)
	})

	t.Run("只看得到自己的记录", func(t *testing.T) {
		mine := rec(1, 60, base.Add(-2*time.Hour))
		others := rec(1, 30, base.Add(-time.Hour)) // 更新,但属于别人
		others.UserID = 999

		repo := historyRepo([]*Video{pub(1, base)}, []*PlayRecord{others, mine})

		resp, err := svcOf(repo).ListHistory(ctx, testUserID, ListHistoryReq{})
		require.NoError(t, err)
		require.Len(t, resp.Items, 1)
		assert.Equal(t, 60, resp.Items[0].Watched, "不该被别人的记录顶掉")
	})

	t.Run("未发布的视频不出现", func(t *testing.T) {
		draft := pub(2, base)
		draft.Status = StatusDraft

		repo := historyRepo(
			[]*Video{pub(1, base), draft},
			[]*PlayRecord{
				rec(1, 30, base.Add(-time.Hour)),
				rec(2, 30, base.Add(-time.Hour)),
			},
		)

		resp, err := svcOf(repo).ListHistory(ctx, testUserID, ListHistoryReq{})
		require.NoError(t, err)
		require.Len(t, resp.Items, 1)
		assert.Equal(t, int64(1), resp.Items[0].ID)
	})

	t.Run("分页:cursor 接着上一页", func(t *testing.T) {
		repo := historyRepo(
			[]*Video{pub(1, base), pub(2, base), pub(3, base)},
			[]*PlayRecord{
				rec(1, 30, base.Add(-3*time.Hour)),
				rec(2, 30, base.Add(-2*time.Hour)),
				rec(3, 30, base.Add(-1*time.Hour)),
			},
		)
		svc := svcOf(repo)

		page1, err := svc.ListHistory(ctx, testUserID, ListHistoryReq{Limit: 2})
		require.NoError(t, err)
		require.Len(t, page1.Items, 2)
		assert.Equal(t, int64(3), page1.Items[0].ID)
		assert.Equal(t, int64(2), page1.Items[1].ID)
		require.NotNil(t, page1.NextCursor)

		page2, err := svc.ListHistory(ctx, testUserID, historyPage(ListHistoryReq{Limit: 2}, page1.NextCursor))
		require.NoError(t, err)
		require.Len(t, page2.Items, 1)
		assert.Equal(t, int64(1), page2.Items[0].ID)
		assert.Nil(t, page2.NextCursor, "取完了,游标该是 nil")
	})

	// 同一时刻的两条靠 video_id 兜底。只带时间戳的旧游标在第二页会问
	// 「watched_at < 那个时刻」→ 一条都不满足 → 另一条被整个丢掉
	t.Run("同一时刻观看的两条不会跨页漏掉", func(t *testing.T) {
		repo := historyRepo(
			[]*Video{pub(1, base), pub(2, base)},
			[]*PlayRecord{
				rec(1, 30, base.Add(-time.Hour)),
				rec(2, 30, base.Add(-time.Hour)),
			},
		)
		svc := svcOf(repo)

		page1, err := svc.ListHistory(ctx, testUserID, ListHistoryReq{Limit: 1})
		require.NoError(t, err)
		require.Len(t, page1.Items, 1)
		assert.Equal(t, int64(2), page1.Items[0].ID, "同一时刻按 video_id 倒序")
		require.NotNil(t, page1.NextCursor)

		page2, err := svc.ListHistory(ctx, testUserID, historyPage(ListHistoryReq{Limit: 1}, page1.NextCursor))
		require.NoError(t, err)
		require.Len(t, page2.Items, 1, "另一条不能因为和第一条同一时刻就被丢掉")
		assert.Equal(t, int64(1), page2.Items[0].ID)
	})

	// 这条锁的是「先去重、再按 cursor 过滤」这个顺序。
	// 反过来的话,视频 1 那条更旧的记录(-2.5h)会通过第二页的过滤条件,
	// 于是视频 1 在翻页后又冒出来一次 —— 而它早在第一页出现过了
	t.Run("分页不重复:同一视频不会跨页再出现", func(t *testing.T) {
		repo := historyRepo(
			[]*Video{pub(1, base), pub(2, base)},
			[]*PlayRecord{
				rec(1, 10, base.Add(-150*time.Minute)), // -2.5h,比视频2更新
				rec(1, 80, base.Add(-time.Hour)),       // 视频1的最近一次
				rec(2, 30, base.Add(-3*time.Hour)),
			},
		)
		svc := svcOf(repo)

		page1, err := svc.ListHistory(ctx, testUserID, ListHistoryReq{Limit: 1})
		require.NoError(t, err)
		require.Len(t, page1.Items, 1)
		assert.Equal(t, int64(1), page1.Items[0].ID)
		require.NotNil(t, page1.NextCursor)

		page2, err := svc.ListHistory(ctx, testUserID, historyPage(ListHistoryReq{Limit: 1}, page1.NextCursor))
		require.NoError(t, err)
		require.Len(t, page2.Items, 1)
		assert.Equal(t, int64(2), page2.Items[0].ID,
			"第二页该是视频2 —— 视频1已经出现过了,它的旧记录不能再冒出来")
		assert.Nil(t, page2.NextCursor)
	})

	t.Run("仓储出错返回 500", func(t *testing.T) {
		repo := historyRepo(nil, nil)
		repo.listHistoryErr = errors.New("boom")

		_, err := svcOf(repo).ListHistory(ctx, testUserID, ListHistoryReq{})
		assertCode(t, err, errs.ErrInternal)
	})
}
