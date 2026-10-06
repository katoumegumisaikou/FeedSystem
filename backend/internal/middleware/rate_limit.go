package middleware

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"feed-system/internal/pkg/errs"
)

// 限流 key 前缀。redis_rate 会在最前面再补 "rate:",最终形如
// rate:feed:rate_limit:ip:video:upload:r10-p1-b10:1.2.3.4
const rateLimitPrefix = "feed:rate_limit:"

// scopeKey 把「业务域 + 档位参数」拼成一个稳定的 bucket 标识。
//
// 为什么 key 里必须同时带这两样 —— 这是一个已经踩过的坑:
//
// redis_rate 用 GCRA,它把状态(tat,理论到达时刻)存在 key 的 value 里,而那是一个
// 裸浮点数,不带任何「按哪套参数写进去的」标记。每次调用都用**当次传入**的
// Burst/Rate/Period 去解释同一个数字,而不同组合解释出的结论可以完全相反:
//
//	PerMinute(30) → emission_interval = 2s,   burst_offset = 60s
//	PerSecond(30) → emission_interval = 1/30s, burst_offset = 1s
//
// 一次 login 把 tat 推到 now+2s。同一个 now+2s,在两套解释下是:
//
//	60 秒缓冲下: (now+2)-60 = now-58  → 比允许时刻晚 58s → 「还剩 29 个额度」
//	1  秒缓冲下: (now+2)-1  = now+1   → 比允许时刻早 1s  → 「超速 31 个额度」
//
// 于是共用 key 的后果是:一次 login 之后 /users/me 立刻 429(真库实测)。
// 这跟"共用同一个 Redis"无关 —— 前缀区分就是为此存在的 —— 问题只在**前缀分得不够细**:
// 只分到了「哪个主体」(ip / user),没分到「哪套参数」,而参数是状态含义的一部分。
//
// scope 让 key 可读(一眼看出是谁的桶);参数后缀则把「不同参数不共桶」这条不变式
// 从「靠命名纪律」变成「结构上不可能违反」。只写 scope 也够,但那样以后有人复制粘贴
// 出一个重名的 scope 就会静默复现同一个 bug —— 加了参数就再也复现不了。
func scopeKey(scope string, limit redis_rate.Limit) string {
	return fmt.Sprintf("%s:r%d-p%gs-b%d", scope, limit.Rate, limit.Period.Seconds(), limit.Burst)
}

// IPRateLimiter 按客户端来源 IP 限流。
//
// scope 是这笔配额的业务标识(如 "video:upload"),必须**每套独立的限流策略一个值**:
// 同一个 scope + 同一个 IP 共用一个 GCRA 状态槽,不同策略共用就会互相污染(见 scopeKey)。
// 两个路由组该不该共用配额,取决于它们该不该共用预算 —— 而不是取决于路径前缀像不像。
//
// 用 RemoteIP 而不是 ClientIP:后者读 X-Forwarded-For,gin 默认信任所有来源,
// 客户端改这个头就是一个全新配额桶,限流会被完全绕过。
// 代价:挂反向代理后所有人共用一个桶,那时要改成「配可信网段 + ClientIP」
func IPRateLimiter(rdb *redis.Client, scope string, limit redis_rate.Limit) gin.HandlerFunc {
	if rdb == nil {
		return passThrough
	}
	limiter := redis_rate.NewLimiter(rdb)
	// 前缀先拼好:scope 与档位都是启动时确定的,只有 IP 是每请求变的
	keyPrefix := rateLimitPrefix + "ip:" + scopeKey(scope, limit) + ":"
	return func(c *gin.Context) {
		if allowOrAbort(c, limiter, keyPrefix+c.RemoteIP(), limit) {
			c.Next()
		}
	}
}

// UserRateLimiter 按登录用户限流。必须挂在 Auth 之后 —— userID 由 Auth 注入,
// 挂错了不报错也不 panic,只等于没限流。
// 与 IPRateLimiter 互补:这个精确到人、不受 NAT 连累,但盖不住匿名接口。
//
// scope 的约定与 IPRateLimiter 相同;两层各用自己的 key(维度前缀 ip: / user:),
// 所以同一个 scope 的 IP 配额与用户配额天然是两个桶,不会互相影响
func UserRateLimiter(rdb *redis.Client, scope string, limit redis_rate.Limit) gin.HandlerFunc {
	if rdb == nil {
		return passThrough
	}
	limiter := redis_rate.NewLimiter(rdb)
	keyPrefix := rateLimitPrefix + "user:" + scopeKey(scope, limit) + ":"
	return func(c *gin.Context) {
		userID := UserID(c)
		if userID <= 0 {
			// 挂对位置就不会走到这;走到了基本是路由漏挂 Auth,记一条别静默放过
			slog.ErrorContext(c.Request.Context(), "用户限流取不到 userID,本次跳过(检查路由是否漏挂 Auth)")
			c.Next()
			return
		}
		if allowOrAbort(c, limiter, keyPrefix+strconv.FormatInt(userID, 10), limit) {
			c.Next()
		}
	}
}

// passThrough 原样放行。Redis 不可用时用它让限流整体失效
func passThrough(c *gin.Context) {
	c.Next()
}

// allowOrAbort 判断是否放行;超限时写好响应并返回 false。
//
// 读 Allowed 而不是 Remaining:burst 里把桶用光的那一个请求 Remaining 恰好是 0,
// 但它其实是放行的 —— 用 Remaining<=0 会让 PerSecond(1) 变成「全拒」(实测)
func allowOrAbort(c *gin.Context, limiter *redis_rate.Limiter, key string, limit redis_rate.Limit) bool {
	ctx := c.Request.Context()
	res, err := limiter.Allow(ctx, key, limit)
	if err != nil {
		// Redis 故障就放行(fail-open):限流组件不该把整站打死
		slog.ErrorContext(ctx, "限流检查失败,本次放行", "key", key, "err", err)
		return true
	}
	if res.Allowed > 0 {
		return true
	}

	// 库用 -1 表示「不用等」,那种情况不写这个头
	if res.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(int(math.Ceil(res.RetryAfter.Seconds()))))
	}
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"code": errs.ErrTooFrequent.Code,
		"msg":  "请求过于频繁,请稍后再试",
	})
	return false
}
