package feed

import (
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"feed-system/internal/middleware"
)

var latestFeedIPLimit = redis_rate.PerMinute(120)

// RegisterRouter 注册公开的视频流路由。
// 为兼容现有客户端,最新视频仍使用 GET /videos/latest。
func RegisterRouter(rg *gin.RouterGroup, h *FeedHandler, db *gorm.DB, rdb *redis.Client) {
	// 公开组:最新流游客可看,挂 Auth 但不挂 SetSensitive —— 软鉴权,
	// 带 token 注入 userID(用于点赞状态),不带也放行
	pub := rg.Group("/videos",
		middleware.IPRateLimiter(rdb, latestFeedIPLimit),
		middleware.Auth(db, rdb),
	)
	pub.GET("/latest", h.ListLatest)
	// 点赞流:游客也能看,带 token 时额外带上 per-user 的点赞态(与最新流同口径)
	pub.GET("/like", h.ListLike)

	// 关注流强制登录:SetSensitive 让无 token 的直接 401,是纵深防御,
	// 不单纯依赖 ListFollowing 内部 userID <= 0 的兜底。
	// 单独建组而不是给上面那组加 SetSensitive:/videos/latest 必须保持公开
	priv := rg.Group("/videos",
		middleware.SetSensitive(),
		middleware.IPRateLimiter(rdb, latestFeedIPLimit),
		middleware.Auth(db, rdb),
	)
	priv.GET("/following", h.ListFollowing)
}
