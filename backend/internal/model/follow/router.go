package follow

import (
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"feed-system/internal/middleware"
)

// 限流阈值。放模块里而不是 middleware 包:阈值是业务策略,各模块可以不同
var (
	followUserLimit = redis_rate.PerSecond(10) // 已登录接口,按用户:精确到人,可以紧
	followIPLimit   = redis_rate.PerSecond(30) // 已登录接口,按 IP 兜底:要比用户那份宽,NAT 出口后是好多人
)

// RegisterRouter 把 follow 模块的所有路由挂到指定的路由组
// 用法:follow.RegisterRouter(r.Group("/api/v1"), h, db, rdb)
//
// 三条路由都是「我的关注关系」的读写,没有游客可用的公开接口,所以只建私有组。
//
// Auth 中间件在 internal/middleware 包,只依赖 gorm + redis,不依赖任何业务包,
// 因此 follow → middleware 是单向依赖,不会有循环引用
func RegisterRouter(rg *gin.RouterGroup, h *FollowHandler, db *gorm.DB, rdb *redis.Client) {
	// 顺序不能乱:IP 在最前(洪水在 JWT 解析前就 429),UserRateLimiter 依赖 Auth 注入的 userID。
	// 两层都挂:用户限流精确到人,IP 兜住「同一账号被多机同时刷」
	//
	// 路径用 /users 而不是另起前缀,是为了复用 account 已在同一组注册的 /users/:id、
	// /users/me。gin 的路由树按 HTTP 方法分开构建,静态段(me)优先级高于参数段(:id),
	// 且两者可以共存,所以 /users/me/followees 与 /users/:id 不冲突
	priv := rg.Group("/users",
		middleware.SetSensitive(),
		middleware.IPRateLimiter(rdb, followIPLimit),
		middleware.Auth(db, rdb),
		middleware.UserRateLimiter(rdb, followUserLimit),
	)
	{
		priv.POST("/:id/follow", h.Follow)     // 关注
		priv.DELETE("/:id/follow", h.Unfollow) // 取关
		priv.GET("/me/followees", h.ListFollowees)
		priv.GET("/me/followers", h.ListFollowers) // 谁关注了我(分页)
	}
}
