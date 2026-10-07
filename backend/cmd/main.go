// feed_system 后端入口
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"feed-system/internal/database"
	"feed-system/internal/middleware"
	"feed-system/internal/model/account"
	"feed-system/internal/model/feed"
	"feed-system/internal/model/follow"
	"feed-system/internal/model/video"
	"feed-system/internal/pkg/logger"
)

// userInfoAdapter 把 account 的用户仓储适配成 video.UserInfoProvider,
// 让 video 不必 import account —— 否则两模块双向耦合,account 再引 video 就成环。
type userInfoAdapter struct {
	repo account.UserRepository
}

func (a userInfoAdapter) GetAuthorInfo(ctx context.Context, userID int64) (string, string, error) {
	u, err := a.repo.FindByID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	return u.UserName, u.AvatarURL, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvBool 解析 bool 环境变量(1/true/yes/on,大小写均可);其他值走 fallback
func getEnvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "False", "no", "NO", "off", "OFF":
		return false
	}
	return fallback
}

func main() {
	// 初始化日志:之后所有输出(含标准库 log)都写入文件
	closeLog, err := logger.Init(getEnv("LOG_FILE", "logs/app.log"))
	if err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	defer closeLog()

	cfg := database.LoadConfigFromEnv()

	db, err := database.Open(cfg)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}
	log.Printf("✓ 已连接到 PostgreSQL: %s:%s/%s", cfg.Host, cfg.Port, cfg.DBName)

	migrator, err := database.NewMigrator(db)
	if err != nil {
		log.Fatalf("创建迁移器失败: %v", err)
	}
	if err := migrator.Up(); err != nil {
		log.Fatalf("执行迁移失败: %v", err)
	}
	log.Println("✓ 数据库迁移完成")

	// 连接 Redis:失败不致命,降级为 nil → SMS / 登录锁定不可用,但鉴权仍走 DB
	rdb := mustRedisClient()

	userRepo := account.NewUserRepository(db)
	devMode := getEnvBool("SMS_DEV_MODE", true) // 默认 dev 模式:短信验证码会回显给前端
	accountSvc := account.NewAccountService(userRepo, rdb, devMode)
	accountHandler := account.NewAccountHandler(accountSvc)

	videoRepo := video.NewVideoRepository(db)
	videoSvc := video.NewVideoService(videoRepo, rdb, userInfoAdapter{repo: userRepo})
	videoHandler := video.NewVideoHandler(videoSvc)

	// 关注流接线。顺序不能随意换:
	// feedSvc 要先建出来 —— 它需要清理任务仓储;fanoutWorker 也要它,而 fanoutWorker
	// 得先存在才能喂给 NewFollowService。
	followRepo := follow.NewFollowRepository(db)
	bigvSet := follow.NewBigVSet(followRepo, rdb)

	feedRepo := feed.NewFeedRepository(db)
	// 读侧惰性清理的任务队列:读路径写、worker 消费。两边用同一个仓储实例。
	cleanupRepo := feed.NewCleanupRepository(db)
	// feed 服务:likes 用 videoSvc,关注列表用 followRepo,大V 判定用 bigvSet,
	// 清理任务队列用 cleanupRepo
	feedSvc := feed.NewFeedService(feedRepo, rdb, videoSvc, followRepo, bigvSet, cleanupRepo)
	feedHandler := feed.NewFeedHandler(feedSvc)

	fanoutRepo := feed.NewFanoutRepository(db)
	// followers 传 followRepo(满足 feed.FollowerLister),bigvs 传 bigvSet(满足
	// feed.BigVChecker)。feed 不 import follow,靠接口解耦,避免循环依赖。
	// 最后一个参数是读侧清理任务的消费端:关注流缓存的过期清理由这个 worker 的循环
	// 顺带驱动,不为它单独起协程(理由见 feed 包的 runCleanupBatch)
	fanoutWorker := feed.NewFanoutWorker(fanoutRepo, feedRepo, followRepo, bigvSet, rdb, cleanupRepo)
	// fanoutWorker 同时满足 follow 包的三个接缝 —— FollowBackfiller(关注后补拉历史)、
	// BigVDowngradeReplayer(大V 降级后重放缓存)与 BigVPromoter(大V 升入后回填缓存)
	followSvc := follow.NewFollowService(followRepo, bigvSet, fanoutWorker, fanoutWorker, fanoutWorker)
	followHandler := follow.NewFollowHandler(followSvc)

	// 后台常驻协程:轮询 fanout_tasks 把新发布视频投进粉丝收件箱,并顺带清理关注流
	// 缓存里的过期成员。用独立的可取消 ctx,退出时先停它再关连接,免得它拿着已关闭的
	// DB/Redis 继续跑。
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go fanoutWorker.Run(workerCtx)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	// 头像公开访问 /avatars/{userID}/{fileName}:Static 只暴露头像目录,不含项目其他文件
	r.Static(account.AvatarURLPrefix, account.AvatarStorageDir)
	// 视频公开访问 /videos/{authorID}/{fileName}。
	//
	// 不能直接 r.Static:那样请求不经过任何中间件,而静态文件服务不认识「视频状态」,
	// 草稿和已下架的视频文件谁知道路径谁就能下。挂到组上之后仍然是同一套 http.FileServer
	// (Range / HEAD / 路径穿越防护都保留),只是多了两道判定:
	// Auth 软鉴权注入 userID(游客为 0),PlayAccess 按 play_url 反查状态决定放不放行
	r.Group(video.VideoURLPrefix,
		middleware.Auth(db, rdb),
		video.PlayAccess(videoRepo, rdb),
	).Static("", video.VideoStorageDir)
	// 封面公开访问 /covers/{videoID}/{fileName}。
	//
	// 与视频不同,封面不设门禁:它出现在列表页首屏,游客也要能直接加载,没有
	// Auth 软鉴权与状态判定的必要。上传入口在 POST /videos/:id/cover(仅作者)
	r.Static(video.CoverURLPrefix, video.CoverStorageDir)

	v1 := r.Group("/api/v1")
	account.RegisterRouter(v1, accountHandler, db, rdb)
	// feed 注册 /videos/latest 与 /videos/following,follow 注册 /users/:id/follow 等。
	// 它们与 video/account 共用 /videos、/users 前缀:gin 按 HTTP 方法 + 路径段建树,
	// 同一层的静态段(latest/following/me)恒优先命中参数段(:id),两者可共存 ——
	// 与注册先后无关,顺序在这里不影响匹配结果。
	feed.RegisterRouter(v1, feedHandler, db, rdb)
	follow.RegisterRouter(v1, followHandler, db, rdb)
	video.RegisterRouter(v1, videoHandler, db, rdb)

	addr := getEnv("HTTP_ADDR", ":8080")
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}
	go func() {
		log.Printf("✓ HTTP server 监听 %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server 启动失败: %v", err)
		}
	}()

	// 优雅退出:收到信号后给 5 秒做收尾
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("收到信号 %s,准备退出...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP server 关闭失败: %v", err)
	}
	// 先停后台协程(fan-out worker 可能正卡在投递或清理里),再关连接,
	// 避免它拿着已关闭的资源继续跑
	stopWorker()
	if rdb != nil {
		_ = rdb.Close()
	}
	sqlDB, err := db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
	log.Println("bye")
}

// mustRedisClient 创建 Redis 客户端,连接失败只 warn 不 fatal(rdb 返回 nil)
// SMS / 登录锁定等依赖 Redis 的功能在 rdb=nil 时会跳过,鉴权中间件也会回源 DB
func mustRedisClient() *redis.Client {
	addr := getEnv("REDIS_HOST", "localhost") + ":" + getEnv("REDIS_PORT", "6379")
	password := getEnv("REDIS_PASSWORD", "")

	rdb := redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    password,
		DB:          0,
		DialTimeout: 3 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("⚠ Redis 连接失败(%s),降级为 nil:SMS / 登录锁定不可用,鉴权走 DB。错误:%v", addr, err)
		_ = rdb.Close()
		return nil
	}
	log.Printf("✓ 已连接到 Redis: %s", addr)
	return rdb
}
