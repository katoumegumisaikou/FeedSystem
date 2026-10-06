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

// Claims 自定义 JWT 载荷。Version 在改密码 / 注销时递增,校验时与 DB 中的值比对
type Claims struct {
	UserID  int64 `json:"user_id"`
	Version int64 `json:"version"`
	jwt.RegisteredClaims
}

// SignToken 用 HMAC-SHA256 签发 JWT,ttl 决定有效期(access 2h / refresh 30d)
func SignToken(userID int64, ttl time.Duration) (string, error) {
	return signToken(userID, 0, ttl)
}

// SignTokenWithVersion 同 SignToken,但带版本号:改密码 / 注销时递增,旧 token 校验失败
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
