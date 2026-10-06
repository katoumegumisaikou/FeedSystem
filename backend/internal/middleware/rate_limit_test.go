package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

// newTestRedis 用 miniredis 起一个内存 Redis 并返回客户端。
// 限流脚本用的是 Lua(GCRA),miniredis 能跑
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// newTestEngine 把待测中间件挂到 /t,后面接一个固定返回 200 的 handler
func newTestEngine(mws ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handlers := make([]gin.HandlerFunc, 0, len(mws)+1)
	handlers = append(handlers, mws...)
	handlers = append(handlers, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"code": 0}) })
	r.GET("/t", handlers...)
	return r
}

// call 发一次请求。remoteAddr 是 TCP 来源地址,forwardedFor 是客户端可伪造的头
func call(r *gin.Engine, remoteAddr, forwardedFor string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestIPRateLimiter(t *testing.T) {
	t.Run("超过上限返回429", func(t *testing.T) {
		// burst = 2:前两次放行,第三次开始拦
		r := newTestEngine(IPRateLimiter(newTestRedis(t), "test", redis_rate.PerSecond(2)))
		const addr = "10.0.0.1:1234"

		assert.Equal(t, http.StatusOK, call(r, addr, "").Code)
		assert.Equal(t, http.StatusOK, call(r, addr, "").Code)

		w := call(r, addr, "")
		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.JSONEq(t, `{"code":10006,"msg":"请求过于频繁,请稍后再试"}`, w.Body.String())
		// 必须是实数秒:库用 -1 表示「不用等」,不判正负会写出 Retry-After: 0
		assert.Equal(t, "1", w.Header().Get("Retry-After"))
	})

	t.Run("Redis不可用时放行且不 panic", func(t *testing.T) {
		// main.go 里 Redis 连不上会把 rdb 降级成 nil。若照常调 NewLimiter(nil),
		// 第一次 Allow 就是 nil 解引用 panic,所以这条必须显式短路
		r := newTestEngine(IPRateLimiter(nil, "test", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(r, "10.0.0.1:1234", "").Code)
	})

	t.Run("不同IP是独立配额", func(t *testing.T) {
		r := newTestEngine(IPRateLimiter(newTestRedis(t), "test", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(r, "10.0.0.1:1111", "").Code)
		assert.Equal(t, http.StatusOK, call(r, "10.0.0.2:2222", "").Code)
	})

	t.Run("伪造XForwardedFor不能绕过", func(t *testing.T) {
		// 中间件读 RemoteIP 而不是 ClientIP,所以换 XFF 头不该换来新配额。
		// 用 ClientIP 的话这三行会全是 200,限流形同虚设
		r := newTestEngine(IPRateLimiter(newTestRedis(t), "test", redis_rate.PerSecond(1)))

		assert.Equal(t, http.StatusOK, call(r, "10.0.0.1:1234", "1.2.3.1").Code)
		assert.Equal(t, http.StatusTooManyRequests, call(r, "10.0.0.1:1234", "1.2.3.2").Code)
		assert.Equal(t, http.StatusTooManyRequests, call(r, "10.0.0.1:1234", "1.2.3.3").Code)
	})
}

// TestRateLimiter_scope隔离 锁住「不同限流策略不共用 GCRA 状态」这条不变式。
//
// 这是真库上踩过的坑:当时 key 只有 "ip:<addr>",而 limit 是各路由传进来的,
// 于是 PerMinute(30)(每格 2s、缓冲 60s)与 PerSecond(30)(每格 1/30s、缓冲 1s)
// 共用同一个 tat。一次 login 把 tat 推到 now+2s,后者用 1 秒缓冲去解释,算出
// 「超速 31 个额度」→ 登录后立刻调 /users/me 就是 429。
//
// 旧测试抓不到:newTestEngine 每次只挂一个 limiter,永远不会有两个档位先后
// 打到同一个 IP 上。所以这两条必须用**同一个 Redis + 同一个 IP** 交叉验证。
func TestRateLimiter_scope隔离(t *testing.T) {
	const addr = "10.0.0.1:1234"

	t.Run("不同scope之间互不污染", func(t *testing.T) {
		rdb := newTestRedis(t)

		// "auth" 档:每格 2 秒。跑 4 次,把 tat 推到远远超过 1 秒后
		slow := newTestEngine(IPRateLimiter(rdb, "auth", redis_rate.PerMinute(30)))
		for i := 0; i < 4; i++ {
			assert.Equal(t, http.StatusOK, call(slow, addr, "").Code)
		}

		// "priv" 档:每格 1/30 秒、缓冲只有 1 秒。同一个 Redis、同一个 IP。
		// 修之前这里会 429 —— tat 被上一个 scope 推走了
		fast := newTestEngine(IPRateLimiter(rdb, "priv", redis_rate.PerSecond(30)))
		for i := 0; i < 5; i++ {
			assert.Equalf(t, http.StatusOK, call(fast, addr, "").Code,
				"第 %d 个请求被拒 —— 上一个 scope 的 tat 污染了这个 scope", i+1)
		}
	})

	t.Run("只有scope相同且参数相同才共桶", func(t *testing.T) {
		rdb := newTestRedis(t)

		// 同 scope + 同参数 → 同一 key,应该共用一个额度
		r1 := newTestEngine(IPRateLimiter(rdb, "video:upload", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(r1, addr, "").Code)

		r2 := newTestEngine(IPRateLimiter(rdb, "video:upload", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusTooManyRequests, call(r2, addr, "").Code,
			"同 scope 同参数应该是同一个桶")

		// 同 scope 但参数不同 → 不同 key。这是给「以后有人复制粘贴出重名 scope」兜底的:
		// 只靠 scope 命名的话这里会被上一个桶挡住,加了参数后缀就再也不会
		r3 := newTestEngine(IPRateLimiter(rdb, "video:upload", redis_rate.PerSecond(5)))
		assert.Equal(t, http.StatusOK, call(r3, addr, "").Code,
			"参数不同就该是另一个桶,否则重名 scope 会静默复现污染")
	})

	t.Run("IP与用户两个维度不互相影响", func(t *testing.T) {
		rdb := newTestRedis(t)

		rIP := newTestEngine(IPRateLimiter(rdb, "video:like", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(rIP, addr, "").Code)
		assert.Equal(t, http.StatusTooManyRequests, call(rIP, addr, "").Code)

		// 同一个 scope、同一个 body,但换到 user 维度 —— 是另一批 key
		asUser := func(c *gin.Context) { c.Set(userIDKey, int64(7)) }
		rUser := newTestEngine(asUser, UserRateLimiter(rdb, "video:like", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(rUser, addr, "").Code)
	})
}

func TestUserRateLimiter(t *testing.T) {
	// asUser 顶替 Auth:真实实现也是把 userID 塞进 gin.Context
	asUser := func(id int64) gin.HandlerFunc {
		return func(c *gin.Context) { c.Set(userIDKey, id) }
	}

	t.Run("按用户限流且各用户配额独立", func(t *testing.T) {
		rdb := newTestRedis(t)

		r7 := newTestEngine(asUser(7), UserRateLimiter(rdb, "test", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(r7, "10.0.0.1:1234", "").Code)
		assert.Equal(t, http.StatusTooManyRequests, call(r7, "10.0.0.1:1234", "").Code)

		// 同一个 Redis,换用户应该重新拿满配额 —— key 里必须带 userID
		r8 := newTestEngine(asUser(8), UserRateLimiter(rdb, "test", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(r8, "10.0.0.1:1234", "").Code)
	})

	t.Run("取不到userID时放行而不是拦截", func(t *testing.T) {
		// 挂在 Auth 之前会走到这里。选择放行 + 记日志:路由配错不该变成全站 429
		r := newTestEngine(UserRateLimiter(newTestRedis(t), "test", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(r, "10.0.0.1:1234", "").Code)
	})

	t.Run("Redis不可用时放行且不 panic", func(t *testing.T) {
		r := newTestEngine(asUser(7), UserRateLimiter(nil, "test", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(r, "10.0.0.1:1234", "").Code)
	})
}

// TestUserRateLimiter_顺序依赖 锁住「必须排在 Auth 之后」:排错了不报错不 panic,
// 只会静默一条都不限,所以得有测试看着
func TestUserRateLimiter_顺序依赖(t *testing.T) {
	// asAuth 顶替 Auth:真实实现的行为也就是「写 userID + c.Next()」
	asAuth := func(c *gin.Context) { c.Set(userIDKey, int64(7)); c.Next() }

	t.Run("排在Auth之后正常限流", func(t *testing.T) {
		r := newTestEngine(asAuth, UserRateLimiter(newTestRedis(t), "test", redis_rate.PerSecond(1)))
		assert.Equal(t, http.StatusOK, call(r, "10.0.0.1:1234", "").Code)
		assert.Equal(t, http.StatusTooManyRequests, call(r, "10.0.0.1:1234", "").Code)
	})

	t.Run("排在Auth之前等于没挂", func(t *testing.T) {
		r := newTestEngine(UserRateLimiter(newTestRedis(t), "test", redis_rate.PerSecond(1)), asAuth)
		// 两个请求全是 200 —— 拿不到 userID 就走了放行分支
		assert.Equal(t, http.StatusOK, call(r, "10.0.0.1:1234", "").Code)
		assert.Equal(t, http.StatusOK, call(r, "10.0.0.1:1234", "").Code)
	})
}
