// Package token 提供 JWT 签发与解析,不依赖任何业务包。
package token

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TODO: 从配置(env / config.yaml)读取
var jwtSecret = []byte("katoumegumi")

const (
	DefaultAccessTTL  = 2 * time.Hour
	DefaultRefreshTTL = 30 * 24 * time.Hour
)

// Claims 自定义 JWT 载荷。
//
// Version 是**每用户**的失效开关,只在改密码时递增(见 account.ChangePassword)。
// 鉴权时由 middleware.checkUserVersion 与 DB 当前值比对,不一致就 401 —— 所以它一次
// 会让该用户的**所有**设备下线,适合「怀疑账号被盗」那种场景。
//
// 登出**不用** version:那是每台设备各自的事,走的是 per-token 黑名单
// (见 RevokedAccessKey) —— 否则在家退出会把手机也一起踢了。
//
// 这两套机制曾经在注释里被混为一谈,写成「version 在改密码 / 注销时递增」,
// 而实际上登出从未碰过它;照那份注释去读代码会得出错误结论,所以这里写明分工。
type Claims struct {
	UserID  int64 `json:"user_id"`
	Version int64 `json:"version"`
	jwt.RegisteredClaims
}

// SignToken 用 HMAC-SHA256 签发 JWT,ttl 决定有效期(access 2h / refresh 30d)。
//
// version 恒为 0,所以只能用于**不需要鉴权**的场景 —— 带上它去访问挂 Auth 的路由,
// 会被 checkUserVersion 因为没有 matching version 而判 401。
// 需要鉴权的签发一律走 SignTokenWithVersion
func SignToken(userID int64, ttl time.Duration) (string, error) {
	return signToken(userID, 0, ttl)
}

// SignTokenWithVersion 同 SignToken,但带版本号。
//
// 目前唯一的调用方是 account:签发时把 users.version 写进 token,使 middleware 能在
// 改密码后认出旧 token。version 为 0 时等价于 SignToken —— 但那条路不可用于
// 需要鉴权的场景:users.version 的 GORM 默认值是 1(非 0),
// 用 0 签发会被 checkUserVersion 判成不一致而 401
func SignTokenWithVersion(userID int64, version int64, ttl time.Duration) (string, error) {
	return signToken(userID, version, ttl)
}

func signToken(userID int64, version int64, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:  userID,
		Version: version,
		RegisteredClaims: jwt.RegisteredClaims{
			// jti:每次签发唯一。iat / exp 只有秒级精度,同一秒内用相同参数签发
			// 会得到完全相同的 token,而刷新流程是「先拉黑旧的、再签新的」——
			// 没有 jti 就会把刚拉黑的那个原样再发一遍,用户下次刷新必然被判重放。
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			NotBefore: jwt.NewNumericDate(now),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(jwtSecret)
}

// Parse 解析并校验 token
func Parse(tokenStr string) (*Claims, error) {
	t, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("非预期的签名算法")
		}
		return jwtSecret, nil
	})
	if err != nil {
		return nil, err
	}
	if claims, ok := t.Claims.(*Claims); ok && t.Valid {
		return claims, nil
	}
	return nil, errors.New("token 无效")
}

// 撤销黑名单的 Redis key 前缀。放这里而不是分别写在读写两侧,是因为两边**必须**
// 用同一个算法:写侧(account.Service.Logout)与读侧(middleware.Auth)各写一份字面量
// 迟早会漂移,而 Redis 对不存在的 key 返回「不存在」而非报错 —— 漂移是静默的,
// 表现为「登出后 token 依然有效」,现场没有任何报错可查。
//
// 前缀沿用项目既有的 feed:revoked:*,不要改 —— 改了就与已在 Redis 里的标记不再匹配。
const (
	revokedAccessPrefix  = "feed:revoked:access:"
	revokedRefreshPrefix = "feed:revoked:refresh:"
)

// RevokedAccessKey 某个 access_token 的撤销标记 key。
// 粒度是单个 token(不是每用户),这样登出只结束当前这台设备的会话,
// 不会把用户的其他设备一起踢下线 —— 这也正是它与递增 users.version 的区别。
func RevokedAccessKey(accessToken string) string {
	return revokedAccessPrefix + accessToken
}

// RevokedRefreshKey 某个 refresh_token 的撤销标记 key
func RevokedRefreshKey(refreshToken string) string {
	return revokedRefreshPrefix + refreshToken
}
