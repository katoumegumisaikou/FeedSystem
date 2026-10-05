package video

import (
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"feed-system/internal/middleware"
)

// 限流阈值。分片上传 1 GiB = 205 个请求,阈值必须让一次正常上传跑得完
var (
	uploadUserLimit  = redis_rate.PerSecond(10)
	uploadIPLimit    = redis_rate.PerSecond(30)
	detailIPLimit    = redis_rate.PerMinute(120) // 详情是公开读接口,防按 ID 遍历拖库
	historyIPLimit   = redis_rate.PerMinute(120) // 观看历史:同样是读接口,同样防遍历
	historyUserLimit = redis_rate.PerSecond(5)   // 历史是自己的数据,按用户比按 IP 更准
	likeIPLimit      = redis_rate.PerSecond(30)  // 点赞是高频小写,按 IP 兜底要放宽(NAT 后是好多人)
	likeUserLimit    = redis_rate.PerSecond(10)  // 按用户精确到人,可以紧一点
)

// RegisterRouter 把 video 模块的所有路由挂到指定的路由组
// 用法:video.RegisterRouter(r.Group("/api/v1"), h, db, rdb)
//
// Auth 中间件在 internal/middleware 包,只依赖 gorm + redis,不依赖任何业务包,
// 因此 video → middleware 是单向依赖,不会有循环引用
func RegisterRouter(rg *gin.RouterGroup, h *VideoHandler, db *gorm.DB, rdb *redis.Client) {
	// ========== 公开路由(无需登录) ==========
	// 游客看视频详情不该被要求登录。这里挂 Auth 但**不挂 SetSensitive** = 软鉴权:
	// 带了合法 token 就注入 userID,没带直接放行。这样作者能用同一个接口看到
	// 自己的草稿,游客只能看到已发布的 —— 可见范围由 service 判定
	pub := rg.Group("/videos",
		middleware.IPRateLimiter(rdb, detailIPLimit),
		middleware.Auth(db, rdb),
	)
	{
		pub.GET("/:id", h.GetVideoDetail)   // 视频详情
		pub.POST("/:id/play", h.ReportPlay) // 播放上报:游客也记(user_id=0),所以放公开组
	}

	// ========== 某人的视频列表(软鉴权) ==========
	// 路径挂在 /users 下(语义是「某人的」),但内容归视频模块管,所以在这里注册而不是
	// account 那边。软鉴权:游客只看得到已发布,作者本人额外能看到草稿
	//
	// 与 account 的 GET /users/:id 不冲突:通配段名都是 :id,静态子段 videos 独立
	userVideos := rg.Group("/users",
		middleware.IPRateLimiter(rdb, detailIPLimit),
		middleware.Auth(db, rdb),
	)
	userVideos.GET("/:id/videos", h.ListUserVideos)

	// ========== 观看历史(强制登录) ==========
	// 不能借用上面的公开组:那组是软鉴权,游客放行后 userID = 0,而 user_id = 0 是
	// 全体未登录访客共用的一个桶 —— 查出来是所有人的记录混在一起,不是「我的历史」
	//
	// 也不挂在 /videos/*filepath 那个静态文件组上:gin 不允许 catch-all 与静态段共存,
	// 注册时直接 panic,而且那组在根路径、不在 /api/v1 下
	hist := rg.Group("/videos",
		middleware.SetSensitive(),
		middleware.IPRateLimiter(rdb, historyIPLimit),
		middleware.Auth(db, rdb),
		middleware.UserRateLimiter(rdb, historyUserLimit),
	)
	{
		hist.GET("/history", h.ListHistory)
	}

	// ========== 私有路由(全部强制登录) ==========
	// SetSensitive 保证没 token 直接 401;UploadChunk / Complete 要拿 userID 校验会话归属
	//
	// 用 /videos/chunk 前缀而不是散在 /videos 下:gin 允许静态段与通配段共存,
	// 所以上面那个 GET /videos/:id 与这里不冲突
	//
	// 限流顺序不能乱:IP 在最前,UserRateLimiter 依赖 Auth 注入的 userID
	priv := rg.Group("/videos",
		middleware.SetSensitive(),
		middleware.IPRateLimiter(rdb, uploadIPLimit),
		middleware.Auth(db, rdb),
		middleware.UserRateLimiter(rdb, uploadUserLimit),
	)
	{
		priv.POST("/chunk/init", h.InitChunkUpload)         // 初始化上传,拿 upload_id
		priv.POST("/chunk", h.UploadChunk)                  // 上传一个分片
		priv.POST("/chunk/complete", h.CompleteChunkUpload) // 合并分片,生成草稿视频
		priv.PUT("/:id", h.UpdateVideo)                     // 编辑标题 / 简介 / 封面
		priv.POST("/:id/publish", h.PublishVideo)           // 草稿 → 已发布
		priv.DELETE("/:id", h.DeleteVideo)                  // 软删除(幂等)
	}

	// ========== 互动(点赞,强制登录) ==========
	// 单独一组而不并进上面的上传组:上传的阈值是按 1GiB 分片场景定的(10/s),
	// 点赞是高频小请求,共用一个桶会让上传把点赞的额度吃光(反之亦然)。
	//
	// 路径 /videos/:id/like 与公开组的 /videos/:id 靠多一段静态段区分:gin 的
	// 通配段名必须一致(都用 :id),静态子段 like/play 可以并存,不冲突
	like := rg.Group("/videos",
		middleware.SetSensitive(),
		middleware.IPRateLimiter(rdb, likeIPLimit),
		middleware.Auth(db, rdb),
		middleware.UserRateLimiter(rdb, likeUserLimit),
	)
	{
		like.POST("/:id/like", h.Like)     // 点赞(幂等)
		like.DELETE("/:id/like", h.Unlike) // 取消点赞(幂等)
	}
}
