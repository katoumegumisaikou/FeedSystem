// Package middleware 存放 gin 中间件。
// 只依赖 gorm / redis / pkg 层工具,不依赖业务包,避免与 model 层循环引用。
package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"feed-system/internal/pkg/errs"
	"feed-system/internal/pkg/sfcache"
	"feed-system/internal/pkg/token"
)

const (
	userIDKey       = "user_id"
	usersTable      = "users"
	versionCol      = "version"
	versionCacheTTL = 5 * time.Minute
)

// SetSensitive 将当前路由标记为强制登录，需与 Auth 一起使用。
func SetSensitive() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("sensitive", true)
	}
}

// Auth 用户鉴权中间件,从 cookie 或 Authorization 头取 token,校验签名后注入 userID,
// 再比对 users.version(见 checkUserVersion);sensitive 路由无 token 直接 401。
// 用法:router.GET("/api/v1/users/me", middleware.Auth(db, rdb), handler.GetMyProfile)
func Auth(db *gorm.DB, rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr := readToken(c)
		sensitive := c.GetBool("sensitive")
		if sensitive && tokenStr == "" {
			abortUnauthorized(c, "未登录")
			return
		} else if !sensitive && tokenStr == "" {
			c.Next()
			return
		}
		claims, err := token.Parse(tokenStr)
		if err != nil {
			abortUnauthorized(c, "token 无效或已过期")
			return
		}

		// 登出黑名单。必须排在 version 校验之前:
		// 两者都是 O(1),但 version 走 sfcache,缓存未命中时会回源 DB;
		// 而黑名单只是一次 Redis EXISTS。被撤销的 token 不必再付那次 DB 开销
		if tokenRevoked(c.Request.Context(), rdb, tokenStr) {
			abortUnauthorized(c, "token 已失效,请重新登录")
			return
		}

		if !checkUserVersion(c, db, rdb, claims.UserID, claims.Version) {
			return
		}
		c.Set(userIDKey, claims.UserID)
		c.Next()
	}
}

// tokenRevoked 查 access_token 是否已被登出拉黑。
//
// 与 users.version 的分工:version 是**每用户**一个,改密码时递增,会把该用户所有
// 设备一起下线;黑名单是**每个 token** 一个,登出只结束当前会话。两个都留着 ——
// 「怀疑账号被盗,全下线」和「我只是想退出这台设备」是两件不同的事。
//
// 查询失败一律放行(fail-open),理由与限流器的 fail-open 同源,但这里的取舍值得写明:
//   - rdb == nil 是 main.go 的既定路径(mustRedisClient 连不上就返回 nil,
//     设计上「鉴权仍走 DB」)。若在这里 fail-closed,没有 Redis 就等于所有人无法登录,
//     把可选依赖变成硬依赖
//   - Redis 临时故障时,代价是「已登出的 token 最长还能用到它自己的 exp(≤2h)」,
//     而不是全站鉴权挂掉。两害相权取轻
//
// 已知残留:写侧(Logout)若因 Redis 故障没写成功,登出会静默不生效,两侧都没有补偿。
// 要彻底解决得让撤销状态脱离外部存储(例如短 TTL + 轮换签名密钥),不在本改动范围内。
func tokenRevoked(ctx context.Context, rdb *redis.Client, accessToken string) bool {
	if rdb == nil {
		return false
	}
	n, err := rdb.Exists(ctx, token.RevokedAccessKey(accessToken)).Result()
	if err != nil {
		slog.ErrorContext(ctx, "查询 token 黑名单失败,本次放行", "err", err)
		return false
	}
	return n > 0
}

// checkUserVersion 比对 token 中的 version 与 DB 当前 version,不一致则 401 ——
// 改密 / 注销会改 version,使旧 token 失效。
// 缓存优先,miss 时回源 DB 并回写;同一用户的并发回源会被合并成一次。
// 返回 false 表示已 abort,调用方直接 return。
//
// 走包级 sfcache.Load:它用的是进程级唯一去重域,所以并发去重不依赖调用方
// 维护什么生命周期。key 自带 feed:user:version: 前缀,不会和别的业务撞。
// Redis 不可用时 sfcache 会降级回源 DB,鉴权不会跟着挂
func checkUserVersion(c *gin.Context, db *gorm.DB, rdb *redis.Client, userID, tokenVersion int64) bool {
	ctx := c.Request.Context()

	dbVersion, err := sfcache.Load(ctx, rdb, userVersionKey(userID), versionCacheTTL,
		func(loadCtx context.Context) (int64, error) {
			var v int64
			if err := db.WithContext(loadCtx).
				Table(usersTable).
				Select(versionCol).
				Where("id = ?", userID).
				Take(&v).Error; err != nil {
				return 0, err
			}
			return v, nil
		})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			abortUnauthorized(c, "用户不存在")
			return false
		}
		abortUnauthorized(c, "token 校验失败")
		return false
	}

	if tokenVersion != dbVersion {
		abortUnauthorized(c, "token 已失效,请重新登录")
		return false
	}
	return true
}

// userVersionKey 生成用户 version 缓存的 Redis key
func userVersionKey(userID int64) string {
	return fmt.Sprintf("feed:user:version:%d", userID)
}

// InvalidateUserVersion 清掉某用户 version 的缓存。
//
// **任何递增 users.version 的地方都必须调用它**,否则 checkUserVersion 会继续用缓存里
// 的旧值,「旧 token 立刻失效」实际变成「最长 versionCacheTTL 后失效」。
// 这个坑已经踩过一次:ChangePassword 递增了 version 却没人清缓存,实测改密后旧
// access_token 仍能用,而同一时刻旧 refresh_token 已经失效(Refresh 直查 DB、不经缓存)
// —— 同一个 version 值给出两种判定,矛盾本身就是漏了失效的证据。
//
// 为什么放在 middleware 而不是让调用方自己拼 key:key 的算法是这份缓存的实现细节,
// 读写必须共用同一份定义。两侧各写一份字面量迟早漂移,而 **Redis 删不存在的 key
// 不报错** —— 漂移是静默的,表现为「改了 version 但 token 没失效」,
// 现场没有任何报错可查。这与 token.RevokedAccessKey 是同一条理由。
//
// 删失败只记日志,不返回 error:调用方那边数据库已经提交,为一次缓存清理反过来报错
// 会让用户以为改密失败。删不掉最多让旧 token 多活一个 TTL,下次读会自愈
//
// 已知残留(与各处的缓存失效同源,是有意接受):并发的读若恰好在「DB 提交之后、
// 这次 Del 之前」完成回源,会把旧值重新写进缓存,于是新值被挡住一个 TTL。
// 排序(写库后再删)已把窗口压到最窄,彻底消除需要版本号/CAS 式回填,不在本次范围
func InvalidateUserVersion(ctx context.Context, rdb *redis.Client, userID int64) {
	if rdb == nil {
		return
	}
	if err := rdb.Del(ctx, userVersionKey(userID)).Err(); err != nil {
		slog.ErrorContext(ctx, "删除用户 version 缓存失败", "user_id", userID, "err", err)
	}
}

// UserID 从 gin.Context 取当前登录用户 ID
// 必须在 Auth 中间件之后调用
func UserID(c *gin.Context) int64 {
	v, _ := c.Get(userIDKey)
	id, _ := v.(int64)
	return id
}

// readToken 优先读 cookie,再读 Authorization header
func readToken(c *gin.Context) string {
	if v, err := c.Cookie("access_token"); err == nil && v != "" {
		return v
	}
	if auth := c.GetHeader("Authorization"); hasBearerPrefix(auth) {
		return auth[7:] // len("Bearer ") = 7
	}
	return ""
}

func hasBearerPrefix(s string) bool {
	return len(s) >= 7 && s[:7] == "Bearer "
}

// abortUnauthorized 401 终止请求
func abortUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(401, gin.H{
		"code": errs.ErrUnauthorized.Code,
		"msg":  msg,
	})
}
