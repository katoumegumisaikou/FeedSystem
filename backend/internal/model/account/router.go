package account

import (
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"feed-system/internal/middleware"
)

// 限流阈值。放模块里而不是 middleware 包:阈值是业务策略,各模块可以不同。
//
// 每个限流点还有一个 scope 名(见下面各组),它决定 Redis key。原则是
// **该不该共用预算**,而不是路径像不像 —— 所以同一模块内按「成本 + 使用频率」分档。
var (
	// sms-code 单独一档,且比原来(与其他认证接口共 30/min)更紧。
	// 理由:它真发短信,有真金白银的成本,是刷接口的首要目标;而 login / refresh
	// 是用户正常使用也要调的。混在一个桶里,"换台设备多登几次"会把短信额度吃掉,
	// 反过来刷短信也会把登录打成 429
	smsIPLimit = redis_rate.PerMinute(5)

	// 登录 / 注册 / 登出 / 刷新 / 改密。保持原来的 30/min。
	// 防爆破的主力是「单账号锁定」(见 account service),这里的 IP 桶只防洪水;
	// 阈值不能太紧:NAT 出口(wifi / 校园网 / 运营商 CGNAT)后是一群人共用,
	// 早上集中登录时 30/min 已经不算宽裕
	authIPLimit = redis_rate.PerMinute(30)

	// 公开读(查他人资料)。防按 ID 遍历拖库 —— 60/min 让遍历慢到没有收益
	publicIPLimit = redis_rate.PerMinute(60)

	privUserLimit = redis_rate.PerSecond(10) // 自己的资料,按用户:精确到人,可以紧
	privIPLimit   = redis_rate.PerSecond(30) // 按 IP 兜底:要比用户那份宽,NAT 后是好多人
)

// RegisterRouter 把 account 模块的所有路由挂到指定的路由组
// 用法:account.RegisterRouter(r.Group("/api/v1"), h, db, rdb)
//
// Auth 中间件在 internal/middleware 包,只依赖 gorm + redis,不依赖任何业务包,
// 因此 account → middleware 是单向依赖,不会有循环引用
func RegisterRouter(rg *gin.RouterGroup, h *AccountHandler, db *gorm.DB, rdb *redis.Client) {
	// ========== 公开路由(无需登录) ==========
	// 匿名可调,是撞库 / 刷短信的主要入口:只能靠 IP 限流(拿不到 userID)
	auth := rg.Group("/auth", middleware.IPRateLimiter(rdb, "account:auth", authIPLimit))
	{
		auth.POST("/register", h.Register)
		auth.POST("/login", h.Login)
		auth.POST("/logout", h.Logout)
		auth.POST("/refresh", h.Refresh)
		// sms-code 额外再挂一层更紧的桶。两层是**独立**的限额,不是替换:
		// 组级那份防匿名洪水,这层防烧短信费。任一层超限都会 429
		auth.POST("/sms-code", middleware.IPRateLimiter(rdb, "account:sms", smsIPLimit), h.SendSmsCode)
		auth.PUT("/password", h.ChangePassword) // 走 SMS 验证码流程,无需 JWT
	}

	// ========== 公开路由(查他人资料,无需登录) ==========
	// 游客点开作者主页不该被要求登录,安全性由 DTO 兜底:GetProfile 只返回
	// PublicUserResp,不含手机号 / 邮箱 / 最近登录时间。
	// 限流是防爬 —— 按 ID 遍历就能拖库
	users := rg.Group("/users", middleware.IPRateLimiter(rdb, "account:profile-public", publicIPLimit))
	{
		users.GET("/:id", h.GetProfile)
	}

	// ========== 私有路由(需要登录,Auth 中间件校验 token + version) ==========
	//
	// 顺序不能乱:IP 在最前(洪水在 JWT 解析前就 429),UserRateLimiter 依赖 Auth 注入的 userID。
	// 两层都挂:用户限流精确到人,IP 兜住「同一账号被多机同时刷」
	priv := rg.Group("/users",
		middleware.SetSensitive(),
		middleware.IPRateLimiter(rdb, "account:profile-me", privIPLimit),
		middleware.Auth(db, rdb),
		middleware.UserRateLimiter(rdb, "account:profile-me", privUserLimit),
	)
	{
		priv.GET("/me", h.GetMyProfile)
		priv.PUT("/me", h.UpdateProfile)
		priv.POST("/me/avatar", h.UploadAvatar)
	}
}
