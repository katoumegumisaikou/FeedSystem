package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/pkg/token"
)

// authTestToken 签一个能通过 Parse 的 token
func authTestToken(t *testing.T, userID, version int64) string {
	t.Helper()
	s, err := token.SignTokenWithVersion(userID, version, time.Hour)
	require.NoError(t, err)
	return s
}

func authTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

// authTestEngine 挂上待测的 Auth,后面接一个返回 200 的 handler。
// 用 SetSensitive 让它走「必须带 token」那条分支。
func authTestEngine(mws ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handlers := append([]gin.HandlerFunc{}, mws...)
	handlers = append(handlers, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"code": 0}) })
	r.GET("/t", handlers...)
	return r
}

func authCall(r *gin.Engine, tokenStr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	if tokenStr != "" {
		req.Header.Set("Authorization", "Bearer "+tokenStr)
	}
	req.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestTokenRevoked 单个 token 的撤销标记查询。
//
// 这里 4 条覆盖的是「读侧」的全部行为;写侧(account.Service.Logout)与它共用
// token.RevokedAccessKey,所以两侧不会漂移 —— 这一点由 TestRevokedKey_读写同源 钉住
func TestTokenRevoked(t *testing.T) {
	ctx := t.Context()

	t.Run("已拉黑返回true", func(t *testing.T) {
		rdb, _ := authTestRedis(t)
		require.NoError(t, rdb.Set(ctx, token.RevokedAccessKey("tok-a"), 7, time.Hour).Err())
		assert.True(t, tokenRevoked(ctx, rdb, "tok-a"))
	})

	t.Run("未拉黑返回false", func(t *testing.T) {
		rdb, _ := authTestRedis(t)
		assert.False(t, tokenRevoked(ctx, rdb, "tok-b"))
	})

	t.Run("rdb为nil放行", func(t *testing.T) {
		// main.go 的 mustRedisClient 连不上就返回 nil,设计上是「鉴权仍走 DB」。
		// 若这里 fail-closed,没有 Redis 就等于所有人无法登录
		assert.False(t, tokenRevoked(ctx, nil, "tok-c"))
	})

	t.Run("Redis报错放行", func(t *testing.T) {
		// 与限流器同源的 fail-open:代价是「已登出的 token 最长还能用到它自己的 exp」,
		// 而不是全站鉴权挂掉
		rdb, mr := authTestRedis(t)
		mr.SetError("boom")
		assert.False(t, tokenRevoked(ctx, rdb, "tok-d"))
	})
}

// TestRevokedKey_格式 钉住撤销 key 的字面量形式。
//
// 第一版这条测试写的是「写侧用 token.RevokedAccessKey 写入、读侧用同一个函数读出」,
// 并命名为「读写同源」—— **它测不出任何东西**:两侧调同一个函数,函数体怎么改都同步
// 漂移,行为等价。变异验证(在读侧函数体末尾加一个字符)把它证实为存活变异体。
//
// 能真正锁住的只有字面量本身,所以这里直接断言完整字符串。理由:
//   - 前缀是**已落到 Redis 里的**约定,改了就和线上的撤销标记不再匹配
//     (旧标记读不到 = 登出静默失效,且要等 TTL 才自愈)
//   - 跨侧漂移(structure 上)已由「只有一处定义」排除;这条负责的是
//     「有人手改了那唯一的定义」
func TestRevokedKey_格式(t *testing.T) {
	assert.Equal(t, "feed:revoked:access:abc", token.RevokedAccessKey("abc"))
	assert.Equal(t, "feed:revoked:refresh:xyz", token.RevokedRefreshKey("xyz"))

	// refresh 与 access 必须是不同的命名空间:否则「刷新时拉黑旧 refresh」
	// 会顺带把同名 access 也拉黑(或反之),而两者本就该独立
	assert.NotEqual(t, token.RevokedAccessKey("same"), token.RevokedRefreshKey("same"))
}

// TestTokenRevoked_与写入侧闭环 写侧按字面量写、读侧必须命中。
//
// 与上一条配合:一条锁格式,一条锁「按那个格式写进去能被读出来」。
// 两条都不能省 —— 只锁格式的话,读侧若改成自己拼一个别的 key 仍会漏
func TestTokenRevoked_与写入侧闭环(t *testing.T) {
	ctx := t.Context()
	rdb, _ := authTestRedis(t)
	tok := authTestToken(t, 7, 1)

	// 故意用字面量而不是 token.RevokedAccessKey —— 这才是「漂移」的定义:
	// 写侧按线上既有格式写入,读侧若换了算法就必须被这条抓住
	require.NoError(t, rdb.Set(ctx, "feed:revoked:access:"+tok, int64(7), time.Hour).Err())

	assert.True(t, tokenRevoked(ctx, rdb, tok), "读侧的 key 算法与线上的写入格式不一致")
}

// TestAuth_登出黑名单 走完整的 Auth 中间件。
//
// 「已拉黑」那条特意传 db = nil:tokenRevoked 排在 checkUserVersion 之前,
// 所以它能在触碰 DB 之前就返回 401。若哪天有人把顺序调换,这条会因为 db 为 nil
// 而 panic —— 顺序这个约束因此也被钉住了
func TestAuth_登出黑名单(t *testing.T) {
	t.Run("已拉黑的token被拒", func(t *testing.T) {
		rdb, _ := authTestRedis(t)
		tok := authTestToken(t, 7, 1)
		require.NoError(t, rdb.Set(t.Context(), token.RevokedAccessKey(tok), int64(7), time.Hour).Err())

		r := authTestEngine(SetSensitive(), Auth(nil, rdb))
		w := authCall(r, tok)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "token 已失效")
	})

	t.Run("无效token被拒", func(t *testing.T) {
		rdb, _ := authTestRedis(t)
		r := authTestEngine(SetSensitive(), Auth(nil, rdb))
		assert.Equal(t, http.StatusUnauthorized, authCall(r, "not-a-jwt").Code)
	})

	t.Run("敏感路由无token被拒", func(t *testing.T) {
		rdb, _ := authTestRedis(t)
		r := authTestEngine(SetSensitive(), Auth(nil, rdb))
		w := authCall(r, "")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "未登录")
	})

	t.Run("非敏感路由无token放行(软鉴权)", func(t *testing.T) {
		// 这条不需要 DB —— 它在 token 为空时就 c.Next() 了,
		// 所以能佐证「公告类路由挂了 Auth 也不会强制登录」
		rdb, _ := authTestRedis(t)
		r := authTestEngine(Auth(nil, rdb))
		assert.Equal(t, http.StatusOK, authCall(r, "").Code)
	})
}

// TestInvalidateUserVersion 递增 users.version 后必须能清掉它的缓存。
//
// 这是 B21 的回归测试:ChangePassword 递增 version 却没人清缓存,
// 于是 checkUserVersion 继续读旧值,「旧 token 立刻失效」变成「最长 5 分钟后失效」。
// 实测当时旧 access_token 仍可用,而同刻旧 refresh_token 已失效
// (Refresh 直查 DB、不经缓存)—— 同一个 version 值给出两种判定。
func TestInvalidateUserVersion(t *testing.T) {
	ctx := t.Context()

	t.Run("删掉已存在的缓存", func(t *testing.T) {
		rdb, mr := authTestRedis(t)
		require.NoError(t, rdb.Set(ctx, userVersionKey(7), int64(3), time.Minute).Err())
		require.True(t, mr.Exists(userVersionKey(7)), "前置条件:缓存应已写入")

		InvalidateUserVersion(ctx, rdb, 7)

		assert.False(t, mr.Exists(userVersionKey(7)), "缓存必须被删掉")
	})

	t.Run("只删目标用户", func(t *testing.T) {
		// key 里必须带 userID,否则会顺手清掉别人的 —— 而别人的 version 没变,
		// 清掉只是白回源一次 DB,不致命,但这种「误伤」掩盖了真正的拼 key 错误
		rdb, mr := authTestRedis(t)
		require.NoError(t, rdb.Set(ctx, userVersionKey(7), int64(3), time.Minute).Err())
		require.NoError(t, rdb.Set(ctx, userVersionKey(8), int64(5), time.Minute).Err())

		InvalidateUserVersion(ctx, rdb, 7)

		assert.False(t, mr.Exists(userVersionKey(7)))
		assert.True(t, mr.Exists(userVersionKey(8)), "不该碰别的用户")
	})

	t.Run("key不存在时不报错", func(t *testing.T) {
		// Redis 对不存在的 key 执行 DEL 返回 0、不报错 —— 这正是「拼错 key」
		// 会静默的原因,所以幂等性得单独确认
		rdb, _ := authTestRedis(t)
		assert.NotPanics(t, func() { InvalidateUserVersion(ctx, rdb, 999) })
	})

	t.Run("rdb为nil不panic", func(t *testing.T) {
		// main.go 的既有路径:Redis 连不上时 rdb 就是 nil
		assert.NotPanics(t, func() { InvalidateUserVersion(ctx, nil, 7) })
	})
}
