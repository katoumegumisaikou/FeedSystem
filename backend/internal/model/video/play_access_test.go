package video

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/middleware"
)

const playTestBody = "0123456789"

// playTestFiles 测试用到的文件,全部真实落盘。
//
// 必须落盘:文件不存在的话 http.FileServer 自己就会 404,那些「应该被中间件挡下」
// 的用例会因为错误的原因通过。ghost.mp4 也在这里 —— 它要验的是「文件在盘上、
// 但库里没有这个 play_url」,文件缺失就测不出来
var playTestFiles = []string{
	"pub.mp4", "draft.mp4", "removed.mp4", "ghost.mp4", "a.mp4",
	"cached.mp4", "cached-ghost.mp4", "dirty.mp4", "roundtrip.mp4",
	"publish-live.mp4", "delete-live.mp4",
}

// newPlayRouter 复刻 main.go 里 /videos 的挂载:软鉴权 → 可见性判定 → 静态文件服务。
//
// 刻意用真实的 http.FileServer 而不是打桩 —— 要锁住的正是「挂上中间件之后
// Range / HEAD / 路径穿越防护有没有被破坏」,打桩就测不到这些了
func newPlayRouter(t *testing.T, repo VideoRepository, rdb *redis.Client, requesterID int64) *gin.Engine {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "7"), 0o755))
	for _, name := range playTestFiles {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "7", name), []byte(playTestBody), 0o644))
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Group(VideoURLPrefix, fakeAuth(requesterID), PlayAccess(repo, rdb)).Static("", dir)
	return r
}

// fakeAuth 冒充 middleware.Auth:真 Auth 要 gorm + redis。这里只需要它注入 userID 的效果。
//
// "user_id" 必须与 middleware.userIDKey 同值 —— 那个常量没导出。对不上时
// middleware.UserID 恒返回 0,「作者能看草稿」这类断言会失败,不会静默放过
func fakeAuth(requesterID int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("user_id", requesterID)
		c.Next()
	}
}

// newPlayRepo 造一个仓储,按 playURL 挂上不同状态的视频。
// 每个用例的 playURL 必须互不相同 —— sfcache 的去重域是进程级的,key 撞了
// 会把别的用例的结果发过来
func newPlayRepo(videos ...*Video) *fakeVideoRepo {
	repo := &fakeVideoRepo{videos: map[int64]*Video{}}
	for i, v := range videos {
		v.ID = int64(i + 1)
		repo.videos[v.ID] = v
	}
	return repo
}

func videoAt(playURL string, status int8) *Video {
	return &Video{AuthorID: testUserID, PlayURL: playURL, Status: status}
}

func doGet(t *testing.T, r *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPlayAccess_可见性(t *testing.T) {
	const (
		pubURL     = "/videos/7/pub.mp4"
		draftURL   = "/videos/7/draft.mp4"
		removedURL = "/videos/7/removed.mp4"
		ghostURL   = "/videos/7/ghost.mp4" // 文件在盘上,但库里没有这个 play_url
	)
	const other int64 = 999

	newRepo := func() *fakeVideoRepo {
		return newPlayRepo(
			videoAt(pubURL, StatusPublished),
			videoAt(draftURL, StatusDraft),
			videoAt(removedURL, StatusRemoved),
		)
	}
	get := func(t *testing.T, url string, requester int64) *httptest.ResponseRecorder {
		return doGet(t, newPlayRouter(t, newRepo(), nil, requester), url, nil)
	}

	t.Run("已发布:游客放行并真的拿到文件", func(t *testing.T) {
		w := get(t, pubURL, 0)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, playTestBody, w.Body.String())
		assert.Equal(t, "video/mp4", w.Header().Get("Content-Type"))
	})

	t.Run("已发布:作者也放行", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, get(t, pubURL, testUserID).Code)
	})

	t.Run("已发布:他人放行", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, get(t, pubURL, other).Code)
	})

	t.Run("草稿:作者放行", func(t *testing.T) {
		w := get(t, draftURL, testUserID)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, playTestBody, w.Body.String())
	})

	t.Run("草稿:游客 404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get(t, draftURL, 0).Code)
	})

	t.Run("草稿:他人 404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get(t, draftURL, other).Code)
	})

	t.Run("已下架:连作者也 404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get(t, removedURL, testUserID).Code)
		assert.Equal(t, http.StatusNotFound, get(t, removedURL, 0).Code)
	})

	t.Run("文件在盘上但库里没有这个 play_url:404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get(t, ghostURL, 0).Code)
	})
}

// TestPlayAccess_保留静态文件服务的行为 挂中间件最容易顺手弄坏这几件事,
// 单独锁一遍:Range 断了视频就没法拖进度条,穿越防护破了就是任意文件读取
func TestPlayAccess_保留静态文件服务的行为(t *testing.T) {
	const rangeURL = "/videos/7/a.mp4"

	repo := newPlayRepo(videoAt(rangeURL, StatusPublished))
	r := newPlayRouter(t, repo, nil, 0)

	t.Run("Range 请求仍是 206 且只回请求的那一段", func(t *testing.T) {
		w := doGet(t, r, rangeURL, map[string]string{"Range": "bytes=2-5"})
		require.Equal(t, http.StatusPartialContent, w.Code)
		assert.Equal(t, "bytes 2-5/10", w.Header().Get("Content-Range"))
		assert.Equal(t, "2345", w.Body.String())
	})

	t.Run("HEAD 返回 200 且没有 body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, rangeURL, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		assert.Zero(t, w.Body.Len())
	})

	t.Run("路径穿越仍被挡下", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, doGet(t, r, "/videos/7/../../etc/passwd", nil).Code)
	})

	t.Run("目录列表仍关闭", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, doGet(t, r, "/videos/7/", nil).Code)
	})
}

// TestPlayAccess_缓存 用 miniredis 起真缓存,验的是「回源被挡住了」而不是「判定对不对」
func TestPlayAccess_缓存(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	t.Run("第二次请求走缓存不回源", func(t *testing.T) {
		const url = "/videos/7/cached.mp4"
		repo := newPlayRepo(videoAt(url, StatusPublished))
		r := newPlayRouter(t, repo, rdb, 0)

		require.Equal(t, http.StatusOK, doGet(t, r, url, nil).Code)
		require.Equal(t, 1, repo.playURLCalls)

		require.Equal(t, http.StatusOK, doGet(t, r, url, nil).Code)
		assert.Equal(t, 1, repo.playURLCalls, "第二次该命中缓存,不该再查库")
	})

	t.Run("不存在的路径也会被缓存,避免旧链接反复打库", func(t *testing.T) {
		const url = "/videos/7/cached-ghost.mp4"
		repo := newPlayRepo(videoAt("/videos/7/other.mp4", StatusPublished))
		r := newPlayRouter(t, repo, rdb, 0)

		require.Equal(t, http.StatusNotFound, doGet(t, r, url, nil).Code)
		require.Equal(t, http.StatusNotFound, doGet(t, r, url, nil).Code)
		assert.Equal(t, 1, repo.playURLCalls)
	})

	t.Run("缓存里是脏值:当未命中回源自愈,不把视频挡在门外", func(t *testing.T) {
		const url = "/videos/7/dirty.mp4"
		repo := newPlayRepo(videoAt(url, StatusPublished))
		r := newPlayRouter(t, repo, rdb, 0)

		require.NoError(t, rdb.Set(context.Background(), playURLKey(url), "垃圾数据", time.Minute).Err())

		w := doGet(t, r, url, nil)
		assert.Equal(t, http.StatusOK, w.Code, "解不开的缓存值该被当成未命中")
		assert.Equal(t, 1, repo.playURLCalls)
	})
}

// TestPlayAccess_与真Auth串联 上面都用 fakeAuth 注入 userID,这一条用真的
// middleware.Auth 走一遍匿名路径 —— Auth 软鉴权在后,没 token 时应当直接放行且不碰 DB
func TestPlayAccess_与真Auth串联(t *testing.T) {
	const url = "/videos/7/real-auth.mp4"

	repo := newPlayRepo(videoAt(url, StatusPublished))

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "7"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "7", "real-auth.mp4"), []byte(playTestBody), 0o644))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// db / rdb 传 nil:匿名请求走不到 checkUserVersion,不会解引用
	r.Group(VideoURLPrefix, middleware.Auth(nil, nil), PlayAccess(repo, nil)).Static("", dir)

	w := doGet(t, r, url, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, playTestBody, w.Body.String())
}

// TestPlayAccess_发布与删除后立即生效 判定缓存必须跟着 status 一起失效。
//
// 这两条是原始症状的端到端复现,不是「断言 key 被删了」:走的是同一套
// 软鉴权 → PlayAccess → http.FileServer,而且文件真在盘上、正文在校验。
//
// 顺带钉住一个跨边界的等价关系:PublishVideo / DeleteVideo 用的是库里存的
// video.PlayURL,而 PlayAccess 用的是 VideoURLPrefix + 路径通配段。两者必须
// 拼出同一个字符串,否则 Del 删的是一个不存在的 key —— Redis 删不存在的 key
// 返回成功、不报错,于是缓存永远清不掉,现场也没有任何报错可查。
// 这条测试是那个等价关系的唯一防线
func TestPlayAccess_发布与删除后立即生效(t *testing.T) {
	const (
		publishURL = "/videos/7/publish-live.mp4"
		deleteURL  = "/videos/7/delete-live.mp4"
	)

	// newEnv 让 HTTP 层与 service 层共用同一个 repo 和 rdb —— 这正是生产里的形态:
	// PlayAccess 拿 repo 反查,service 改库后去删同一个 rdb 里的 key
	newEnv := func(t *testing.T, playURL string, status int8) (*gin.Engine, *VideoService, *fakeVideoRepo) {
		t.Helper()

		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })

		v := videoAt(playURL, status)
		v.ID = 1
		repo := &fakeVideoRepo{videos: map[int64]*Video{1: v}}
		return newPlayRouter(t, repo, rdb, 0), NewVideoService(repo, rdb, nil), repo
	}

	t.Run("草稿发布后,游客立刻能播放", func(t *testing.T) {
		ctx := context.Background()
		r, svc, repo := newEnv(t, publishURL, StatusDraft)

		// 1. 游客先请求一次:404,并把 draft 这判定灌进缓存
		require.Equal(t, http.StatusNotFound, doGet(t, r, publishURL, nil).Code)
		require.Equal(t, 1, repo.playURLCalls, "前置条件:这一请求应当回源")

		// 2. 作者发布
		_, err := svc.PublishVideo(ctx, 1, testUserID)
		require.NoError(t, err)
		require.Equal(t, StatusPublished, repo.videos[1].Status, "前置条件:状态真的翻过来了")

		// 3. 同一个 URL 再请求:必须立刻放行,而不是等 5 分钟 TTL 过去
		w := doGet(t, r, publishURL, nil)
		require.Equal(t, http.StatusOK, w.Code,
			"判定缓存没失效的话这里还是 404 —— 详情页已显示已发布,文件却下不到")
		assert.Equal(t, playTestBody, w.Body.String())
	})

	t.Run("已发布视频删除后,立刻不可下载", func(t *testing.T) {
		ctx := context.Background()
		r, svc, repo := newEnv(t, deleteURL, StatusPublished)

		// 1. 先能下,并把 published 判定灌进缓存
		require.Equal(t, http.StatusOK, doGet(t, r, deleteURL, nil).Code)
		require.Equal(t, 1, repo.playURLCalls)

		// 2. 作者删除
		require.NoError(t, svc.DeleteVideo(ctx, 1, testUserID))

		// 3. 已经删掉的文件必须当场不可下载
		assert.Equal(t, http.StatusNotFound, doGet(t, r, deleteURL, nil).Code,
			"判定缓存里还写着 Found=true 的话,删了的文件最长 5 分钟内还能下")
	})

	t.Run("重复发布也清一次,治好上一次崩在两步之间的残留", func(t *testing.T) {
		ctx := context.Background()
		r, svc, _ := newEnv(t, publishURL, StatusDraft)

		// 模拟「第一次发布写库成功、但删缓存那一步没执行」:状态已是已发布,
		// 而缓存里还是那份 draft
		require.Equal(t, http.StatusNotFound, doGet(t, r, publishURL, nil).Code)
		svc.videorepo.(*fakeVideoRepo).videos[1].Status = StatusPublished

		// 客户端没收到响应而重试:拿到成功,并且这一次必须把残留清掉
		_, err := svc.PublishVideo(ctx, 1, testUserID)
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, doGet(t, r, publishURL, nil).Code)
	})
}

// TestPlayTarget_经缓存往返 锁住 playTarget 能原样过一趟 Redis。
//
// 它的字段必须导出 —— 否则 sfcache 的 JSON 会静默编成 {}、命中缓存时解出全零值,
// 判定全错却不报任何错。这条测试是那个约束的唯一防线
func TestPlayTarget_经缓存往返(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	const url = "/videos/7/roundtrip.mp4"
	repo := newPlayRepo(videoAt(url, StatusPublished))
	r := newPlayRouter(t, repo, rdb, 0)

	// 第一次:回源,写进缓存
	require.Equal(t, http.StatusOK, doGet(t, r, url, nil).Code)
	require.Equal(t, 1, repo.playURLCalls)

	// 缓存里应该是完整的 JSON —— 是 {} 就说明字段没导出
	raw, err := rdb.Get(context.Background(), playURLKey(url)).Result()
	require.NoError(t, err)
	assert.JSONEq(t, `{"author_id":7,"status":1,"found":true}`, raw)

	// 第二次:纯走缓存,判定仍正确(说明 author_id / status / found 都没被解成零值)
	require.Equal(t, http.StatusOK, doGet(t, r, url, nil).Code)
	assert.Equal(t, 1, repo.playURLCalls)
}
