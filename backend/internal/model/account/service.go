package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"mime/multipart"
	"os"
	"path/filepath"
	"time"

	"feed-system/internal/pkg/errs"
	"feed-system/internal/pkg/token"
	"feed-system/internal/util/filetype"
	"feed-system/internal/util/password"
	"feed-system/internal/util/username"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// AccountService 用户账号业务层
type AccountService struct {
	userrepo UserRepository
	rdb      *redis.Client // nil 则跳过登录失败锁定
	devMode  bool          // dev 模式:验证码直接返回给客户端
}

// NewAccountService 构造 AccountService;rdb 为 nil 表示不启用登录失败锁定
func NewAccountService(userrepo UserRepository, rdb *redis.Client, devMode bool) *AccountService {
	return &AccountService{userrepo: userrepo, rdb: rdb, devMode: devMode}
}

// 登录失败锁定(Redis 版)。计数与锁定是两个独立的 key:
// 计数 key 的 TTL 只是「计数窗口」,当成「已锁定」会让第一次失败就被锁

const (
	loginLockMaxAttempts = 5
	loginLockCountWindow = 15 * time.Minute // 计数窗口,到期归零
	loginLockBaseTTL     = 60 * time.Second
	loginLockMaxTTL      = 24 * time.Hour
)

// 失败计数器 key(带计数窗口 TTL)
func (s *AccountService) loginLockCountKey(phone string) string {
	return "feed:fail:" + phone
}

// 锁定标记 key,存在才算「已锁定」
func (s *AccountService) loginLockFlagKey(phone string) string {
	return "feed:fail:lock:" + phone
}

// 检查是否锁定,返回剩余时长(0 未锁)
func (s *AccountService) loginLockCheck(ctx context.Context, phone string) (time.Duration, error) {
	if s.rdb == nil {
		return 0, nil
	}
	ttl, err := s.rdb.TTL(ctx, s.loginLockFlagKey(phone)).Result()
	if err != nil {
		return 0, err
	}
	if ttl < 0 {
		return 0, nil
	}
	return ttl, nil
}

// loginLockFail 记录一次失败。第 5、10、15… 次失败时写锁标记,
// TTL = BaseTTL × 2^((N/5)-1),封顶 MaxTTL 后不再延长
func (s *AccountService) loginLockFail(ctx context.Context, phone string) (time.Duration, error) {
	if s.rdb == nil {
		return 0, nil
	}
	countKey := s.loginLockCountKey(phone)
	count, err := s.rdb.Incr(ctx, countKey).Result()
	if err != nil {
		return 0, err
	}

	// 首次失败开计数窗口,避免陈年失败被一直累加
	if count == 1 {
		s.rdb.Expire(ctx, countKey, loginLockCountWindow)
		return 0, nil
	}

	if count%int64(loginLockMaxAttempts) != 0 {
		return 0, nil
	}

	tier := count/int64(loginLockMaxAttempts) - 1
	if tier > 30 { // 防溢出
		tier = 30
	}
	ttl := loginLockBaseTTL * time.Duration(1<<uint(tier))
	if ttl > loginLockMaxTTL {
		ttl = loginLockMaxTTL
	}
	if err := s.rdb.Set(ctx, s.loginLockFlagKey(phone), 1, ttl).Err(); err != nil {
		return 0, err
	}
	return ttl, nil
}

// 清除失败计数与锁定标记
func (s *AccountService) loginLockReset(ctx context.Context, phone string) error {
	if s.rdb == nil {
		return nil
	}
	return s.rdb.Del(ctx, s.loginLockCountKey(phone), s.loginLockFlagKey(phone)).Err()
}

func (s *AccountService) Register(ctx context.Context, req RegisterReq) (*TokenResp, error) {
	// binding 只校验长度,字符组成靠业务校验
	if ok, reason := username.Validate(req.UserName); !ok {
		return nil, errs.ErrInvalidParam.WithMsg(reason)
	}

	// 密码强度同理,binding 只校验长度
	if ok, reason := password.Strong(req.Password); !ok {
		return nil, errs.ErrInvalidParam.WithMsg(reason)
	}

	u, err := s.userrepo.FindByUserName(ctx, req.UserName)
	if err != nil && !errors.Is(err, ErrNotFound) {
		slog.ErrorContext(ctx, "注册失败:查询用户名出错", "err", err)
		return nil, errs.ErrInternal.WithMsg("注册失败")
	} else if u != nil {
		return nil, errs.ErrConflict.WithMsg("用户名已被占用")
	}

	u, err = s.userrepo.FindByPhone(ctx, req.Phone)
	if err != nil && !errors.Is(err, ErrNotFound) {
		slog.ErrorContext(ctx, "注册失败:查询手机号出错", "err", err)
		return nil, errs.ErrInternal.WithMsg("注册失败")
	} else if u != nil {
		return nil, errs.ErrConflict.WithMsg("手机号已被注册")
	}

	if req.Email != nil {
		u, err = s.userrepo.FindByEmail(ctx, *req.Email)
		if err != nil && !errors.Is(err, ErrNotFound) {
			slog.ErrorContext(ctx, "注册失败:查询邮箱出错", "err", err)
			return nil, errs.ErrInternal.WithMsg("注册失败")
		} else if u != nil {
			return nil, errs.ErrConflict.WithMsg("邮箱已被注册")
		}
	}

	hashed, err := password.Hash(req.Password)
	if err != nil {
		slog.ErrorContext(ctx, "注册失败:密码哈希出错", "err", err)
		return nil, errs.ErrInternal.WithMsg("密码哈希失败")
	}

	user := &User{
		UserName: req.UserName,
		Password: hashed,
		Phone:    req.Phone,
		Email:    req.Email,
	}
	if err := s.userrepo.Create(ctx, user); err != nil {
		slog.ErrorContext(ctx, "注册失败:创建用户出错", "err", err)
		return nil, errs.ErrInternal.WithMsg("创建用户失败")
	}

	// 必须带 user.Version:DB 里 version 默认是 1(非 0),
	// 用 0 签发会被鉴权中间件的 claims.Version != dbVersion 判成 401
	return &TokenResp{
		AccessToken:  mustSignTokenWithVersion(user.ID, user.Version, token.DefaultAccessTTL),
		RefreshToken: mustSignTokenWithVersion(user.ID, user.Version, token.DefaultRefreshTTL),
	}, nil
}

func (s *AccountService) Login(ctx context.Context, req LoginReq) (*TokenResp, error) {
	if ttl, err := s.loginLockCheck(ctx, req.Phone); err != nil {
		return nil, errs.ErrInternal.WithMsg("登录失败")
	} else if ttl > 0 {
		return nil, errs.ErrTooFrequent.WithMsg(fmt.Sprintf("登录失败次数过多,请 %d 秒后再试", int(ttl.Seconds())))
	}

	user, err := s.userrepo.FindByPhone(ctx, req.Phone)
	if errors.Is(err, ErrNotFound) {
		// 用户不存在也算失败,触发锁定(防枚举)
		_, _ = s.loginLockFail(ctx, req.Phone)
		return nil, errs.ErrUnauthorized.WithMsg("手机号或密码错误")
	}
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("登录失败")
	}

	// password.Verify 的入参顺序是 hash 在前、明文在后
	if !password.Verify(user.Password, req.Password) {
		_, _ = s.loginLockFail(ctx, req.Phone)
		return nil, errs.ErrUnauthorized.WithMsg("手机号或密码错误")
	}

	_ = s.loginLockReset(ctx, req.Phone)

	_ = s.userrepo.UpdateLastLoginAt(ctx, user.ID, time.Now())

	// 带 DB 里的 version,否则中间件校验会 401
	return &TokenResp{
		AccessToken:  mustSignTokenWithVersion(user.ID, user.Version, token.DefaultAccessTTL),
		RefreshToken: mustSignTokenWithVersion(user.ID, user.Version, token.DefaultRefreshTTL),
	}, nil
}

func (s *AccountService) Logout(ctx context.Context, accessToken, refreshToken string) error {
	if refreshToken == "" {
		return errs.ErrUnauthorized.WithMsg("refresh_token 缺失")
	}

	// refresh_token 入黑名单(TTL 到其过期为止,约 30 天)。
	// key 走 token.RevokedRefreshKey —— 读侧(middleware.Auth / Refresh)用同一个函数,
	// 两边不许各写一份字面量:Redis 查不存在的 key 不报错,漂移会静默表现为「登出无效」
	if claims, err := token.Parse(refreshToken); err == nil && claims.ExpiresAt != nil {
		ttl := time.Until(claims.ExpiresAt.Time)
		if ttl > 0 && s.rdb != nil {
			s.rdb.Set(ctx, token.RevokedRefreshKey(refreshToken), claims.UserID, ttl)
		}
	}

	// access_token 入黑名单(约 2 小时)。
	// 粒度是单个 token 而非每用户:登出只结束当前这台设备,不踢用户的其他设备
	if accessToken != "" {
		if claims, err := token.Parse(accessToken); err == nil && claims.ExpiresAt != nil {
			ttl := time.Until(claims.ExpiresAt.Time)
			if ttl > 0 && s.rdb != nil {
				s.rdb.Set(ctx, token.RevokedAccessKey(accessToken), claims.UserID, ttl)
			}
		}
	}

	return nil
}

// 按主键查用户,并把仓储错误翻译成业务错误
func (s *AccountService) findUserByID(ctx context.Context, userID int64) (*User, error) {
	user, err := s.userrepo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, errs.ErrNotFound.WithMsg("用户不存在")
		}
		return nil, errs.ErrInternal.WithMsg("查询用户失败")
	}
	return user, nil
}

// GetProfile 查询他人公开资料 (GET /users/:id)。
// 只返回 PublicUserResp:不该顺手把对方的手机号、邮箱、最近登录时间也交出去
func (s *AccountService) GetProfile(ctx context.Context, userID int64) (*PublicUserResp, error) {
	user, err := s.findUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return toPublicUserResp(user), nil
}

// GetMyProfile 查询自己的完整资料 (GET /users/me)
func (s *AccountService) GetMyProfile(ctx context.Context, userID int64) (*UserResp, error) {
	user, err := s.findUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return toUserResp(user), nil
}

// UpdateProfile 更新用户资料,只允许改 AvatarURL / Email
func (s *AccountService) UpdateProfile(ctx context.Context, userID int64, req UpdateProfileReq) (*UserResp, error) {
	user, err := s.findUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	// 只在提交了新邮箱且与当前不同时校验
	if req.Email != nil && (user.Email == nil || *req.Email != *user.Email) {
		existing, err := s.userrepo.FindByEmail(ctx, *req.Email)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, errs.ErrInternal.WithMsg("查询邮箱失败")
		}
		if existing != nil {
			return nil, errs.ErrConflict.WithMsg("邮箱已被注册")
		}
		user.Email = req.Email
	}

	if req.AvatarURL != nil {
		user.AvatarURL = *req.AvatarURL
	}

	if err := s.userrepo.Update(ctx, user); err != nil {
		return nil, errs.ErrInternal.WithMsg("更新用户失败")
	}
	return toUserResp(user), nil
}

// SendSmsCode 生成 6 位验证码存 Redis(5 分钟);dev 模式返回明文,生产返回空串
func (s *AccountService) SendSmsCode(ctx context.Context, req SendSmsCodeReq) (string, error) {
	if s.rdb == nil {
		return "", errs.ErrInternal.WithMsg("短信服务未启用")
	}
	code := fmt.Sprintf("%06d", rand.Intn(1000000))
	if err := s.rdb.Set(ctx, smsCodeKey(req.Phone), code, 5*time.Minute).Err(); err != nil {
		return "", errs.ErrInternal.WithMsg("验证码存储失败")
	}
	fmt.Printf("[SMS-DEV] phone=%s code=%s\n", req.Phone, code)
	if s.devMode {
		return code, nil
	}
	return "", nil
}

// 验证码的 Redis key;写入与校验必须共用,拼错会导致校验恒失败
func smsCodeKey(phone string) string {
	return "feed:sms:" + phone
}

// 验证码失败的统一文案
const errMsgSmsCode = "验证码错误或已过期"

// readSmsCode 读验证码;consume=true 时用 GETDEL 原子读删
func (s *AccountService) readSmsCode(ctx context.Context, phone string, consume bool) (string, error) {
	if s.rdb == nil {
		return "", errs.ErrInternal.WithMsg("短信服务未启用")
	}
	var (
		saved string
		err   error
	)
	if consume {
		saved, err = s.rdb.GetDel(ctx, smsCodeKey(phone)).Result()
	} else {
		saved, err = s.rdb.Get(ctx, smsCodeKey(phone)).Result()
	}
	if errors.Is(err, redis.Nil) {
		return "", errs.ErrInvalidParam.WithMsg(errMsgSmsCode)
	}
	// Redis 故障不能伪装成「验证码错误」:既掩盖真实故障,又误导用户重发短信
	if err != nil {
		return "", errs.ErrInternal.WithMsg("校验验证码失败")
	}
	return saved, nil
}

// checkSmsCode 校验但不消费验证码,用于改密这类「验证码之后还有业务校验」
// 的流程:后续校验失败时用户能拿同一个码重试,不必重等短信
func (s *AccountService) checkSmsCode(ctx context.Context, phone, code string) error {
	saved, err := s.readSmsCode(ctx, phone, false)
	if err != nil {
		return err
	}
	if saved != code {
		return errs.ErrInvalidParam.WithMsg(errMsgSmsCode)
	}
	return nil
}

// consumeSmsCode 校验并一次性消费验证码。用 GETDEL 而非 Get+Del:
// 后者两步间有窗口,并发请求会读到同一个码并都通过校验
func (s *AccountService) consumeSmsCode(ctx context.Context, phone, code string) error {
	saved, err := s.readSmsCode(ctx, phone, true)
	if err != nil {
		return err
	}
	if saved != code {
		return errs.ErrInvalidParam.WithMsg(errMsgSmsCode)
	}
	return nil
}

// RefreshToken 用 refresh_token 换新 token
func (s *AccountService) RefreshToken(ctx context.Context, refreshToken string) (*TokenResp, error) {
	if refreshToken == "" {
		return nil, errs.ErrUnauthorized.WithMsg("refresh_token 缺失")
	}

	claims, err := token.Parse(refreshToken)
	if err != nil {
		return nil, errs.ErrUnauthorized.WithMsg("refresh_token 无效或已过期")
	}

	if s.rdb != nil {
		blacklisted, err := s.rdb.Exists(ctx, token.RevokedRefreshKey(refreshToken)).Result()
		if err != nil {
			return nil, errs.ErrInternal.WithMsg("校验 refresh_token 失败")
		}
		if blacklisted > 0 {
			return nil, errs.ErrUnauthorized.WithMsg("refresh_token 已使用过,请重新登录")
		}
	}

	user, err := s.userrepo.FindByID(ctx, claims.UserID)
	if errors.Is(err, ErrNotFound) {
		return nil, errs.ErrNotFound.WithMsg("用户不存在")
	}
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("查询用户失败")
	}

	// version 不匹配 → 改密后旧 refresh_token 已失效
	if claims.Version != user.Version {
		return nil, errs.ErrUnauthorized.WithMsg("token 已失效,请重新登录")
	}

	// 旧 refresh_token 拉黑,防重放
	if s.rdb != nil && claims.ExpiresAt != nil {
		ttl := time.Until(claims.ExpiresAt.Time)
		if ttl > 0 {
			s.rdb.Set(ctx, token.RevokedRefreshKey(refreshToken), claims.UserID, ttl)
		}
	}

	// 6. 用当前 Version 签发新 token
	return &TokenResp{
		AccessToken:  mustSignTokenWithVersion(user.ID, user.Version, token.DefaultAccessTTL),
		RefreshToken: mustSignTokenWithVersion(user.ID, user.Version, token.DefaultRefreshTTL),
	}, nil
}

func (s *AccountService) ChangePassword(ctx context.Context, req ChangePasswordReq) (*TokenResp, error) {
	// 0. 验证码既放最前又不消费。放最前:若排在业务校验之后,攻击者拿弱密码
	//    试码收到「密码强度不足」而非「验证码错误」,就等于确认码猜中了;
	//    不消费:后续业务校验失败时还能用同一个码重试,不必重发短信
	if err := s.checkSmsCode(ctx, req.Phone, req.SmsCode); err != nil {
		return nil, err
	}

	if ok, reason := password.Strong(req.Password); !ok {
		return nil, errs.ErrInvalidParam.WithMsg(reason)
	}

	user, err := s.userrepo.FindByPhone(ctx, req.Phone)
	if errors.Is(err, ErrNotFound) {
		return nil, errs.ErrNotFound.WithMsg("用户不存在")
	}
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("查询用户失败")
	}

	if password.Verify(user.Password, req.Password) {
		return nil, errs.ErrInvalidParam.WithMsg("新密码不能与旧密码相同")
	}

	// 先算哈希(bcrypt 是这里唯一还可能失败的步骤),再消费验证码,
	// 免得哈希出错把用户的验证码白烧掉
	hashed, err := password.Hash(req.Password)
	if err != nil {
		return nil, errs.ErrInternal.WithMsg("密码哈希失败")
	}

	// 业务校验全过,到这里才真正消费验证码
	if err := s.consumeSmsCode(ctx, req.Phone, req.SmsCode); err != nil {
		return nil, err
	}

	user.Password = hashed
	user.Version++ // 让该用户所有旧 token 立刻失效
	if err := s.userrepo.Update(ctx, user); err != nil {
		return nil, errs.ErrInternal.WithMsg("更新用户失败")
	}

	return &TokenResp{
		AccessToken:  mustSignTokenWithVersion(user.ID, user.Version, token.DefaultAccessTTL),
		RefreshToken: mustSignTokenWithVersion(user.ID, user.Version, token.DefaultRefreshTTL),
	}, nil
}

// toUserResp User → UserResp。显式列出字段,Password / DeletedAt 永不暴露
func toUserResp(u *User) *UserResp {
	if u == nil {
		return nil
	}
	return &UserResp{
		ID:          u.ID,
		UserName:    u.UserName,
		Phone:       u.Phone,
		Email:       u.Email,
		AvatarURL:   u.AvatarURL,
		CreatedAt:   u.CreatedAt,
		LastLoginAt: u.LastLoginAt,
	}
}

// toPublicUserResp User → PublicUserResp。显式列字段:给 User 加新字段时不会自动泄漏到公开接口
func toPublicUserResp(u *User) *PublicUserResp {
	if u == nil {
		return nil
	}
	return &PublicUserResp{
		ID:        u.ID,
		UserName:  u.UserName,
		AvatarURL: u.AvatarURL,
		CreatedAt: u.CreatedAt,
	}
}

// 签发带版本号的 token
func mustSignTokenWithVersion(userID int64, version int64, ttl time.Duration) string {
	t, err := token.SignTokenWithVersion(userID, version, ttl)
	if err != nil {
		return ""
	}
	return t
}

// CheckUserVersion 实现 token.VersionChecker;中间件每次鉴权都用它比对 token 与 DB 的 version
func (s *AccountService) CheckUserVersion(ctx context.Context, userID int64) (int64, error) {
	user, err := s.findUserByID(ctx, userID)
	if err != nil {
		return 0, err
	}
	return user.Version, nil
}

// newFileName 生成随机文件名(不含扩展名),调用方自行拼后缀。
// 不用 fileheader.Filename:客户端可控,可含 "../" 路径穿越,也可带
// .php/.html 后缀被静态伺服造成 XSS。
// 用 UUID v4(crypto/rand)而非 math/rand:后者输出可反推,攻击者能预测出
// 他人文件名并遍历下载
func newFileName() string {
	return uuid.NewString()
}

// AvatarURLPrefix 头像对外 URL 前缀,由 main.go 静态路由挂载;
// 必须与 AvatarStorageDir 对应,否则写进库的 URL 会 404
const AvatarURLPrefix = "/avatars"

// AvatarStorageDir 头像存储根目录。相对路径,基准是进程工作目录
// (Makefile 的后端 target 都是 cd backend 后启动,所以落在 backend/storage/avatars)
const AvatarStorageDir = "storage/avatars"

// 头像大小上限 10 MiB
const maxAvatarSize = 10 << 20

// 头像格式白名单(在魔数判定之后过滤)
var allowedAvatarMIME = map[string]bool{
	filetype.MIMEJPEG: true,
	filetype.MIMEPNG:  true,
	filetype.MIMEGIF:  true,
	filetype.MIMEWebP: true,
}

func (s *AccountService) UploadAvatar(ctx context.Context, fileheader *multipart.FileHeader, userID int64) error {
	if fileheader.Size <= 0 || fileheader.Size > maxAvatarSize {
		return errs.ErrInvalidParam.WithMsg("头像文件不能为空,且大小不能超过 10MB")
	}

	multiFile, err := fileheader.Open()
	if err != nil {
		slog.ErrorContext(ctx, "打开上传文件失败", "user_id", userID, "err", err)
		return errs.ErrInternal.WithMsg("读取上传文件失败")
	}
	defer multiFile.Close()

	// 后缀和 Content-Type 都由客户端提供,只能用文件头魔数判真实类型
	header := make([]byte, filetype.HeaderSize)
	n, readErr := io.ReadFull(multiFile, header)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		slog.ErrorContext(ctx, "读取文件头失败", "user_id", userID, "err", readErr)
		return errs.ErrInternal.WithMsg("读取上传文件失败")
	}
	mime, ok := filetype.DetectImage(header[:n])
	if !ok || !allowedAvatarMIME[mime] {
		return errs.ErrInvalidParam.WithMsg("头像只支持 JPG / PNG / GIF / WebP 格式")
	}
	ext, ok := filetype.ExtForMIME(mime)
	if !ok {
		return errs.ErrInvalidParam.WithMsg("头像只支持 JPG / PNG / GIF / WebP 格式")
	}
	// 读魔数移动了文件偏移,保存前必须 Seek 回开头,否则丢失前 12 字节
	if _, err := multiFile.Seek(0, io.SeekStart); err != nil {
		slog.ErrorContext(ctx, "重置文件偏移失败", "user_id", userID, "err", err)
		return errs.ErrInternal.WithMsg("读取上传文件失败")
	}

	// 目录必须 0o755 带 x 位才能进入,不能用 0o644
	dir := filepath.Join(AvatarStorageDir, fmt.Sprintf("%d", userID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.ErrorContext(ctx, "创建头像目录失败", "user_id", userID, "dir", dir, "err", err)
		return err
	}

	fileName := newFileName() + ext
	filePath := filepath.Join(dir, fileName)

	osFile, err := os.Create(filePath)
	if err != nil {
		slog.ErrorContext(ctx, "创建头像文件失败", "user_id", userID, "path", filePath, "err", err)
		return err
	}
	// Close 的错误只能记日志:defer 到函数返回才执行,那时写库早已完成,挡不住
	// 「库里指向一个没落稳的文件」;但 close 可能带延迟写错误(NFS/ext4 的 ENOSPC),别让它无声无息
	defer func() {
		if cerr := osFile.Close(); cerr != nil {
			slog.ErrorContext(ctx, "关闭头像文件失败", "user_id", userID, "path", filePath, "err", cerr)
		}
	}()

	if _, err := io.Copy(osFile, multiFile); err != nil {
		_ = os.Remove(filePath) // 写盘失败,清掉半个文件
		slog.ErrorContext(ctx, "写入头像文件失败", "user_id", userID, "path", filePath, "err", err)
		return err
	}

	// 存对外 URL(相对路径,前端同源;生产由 Nginx 反代到存储目录)
	avatarURL := fmt.Sprintf("%s/%d/%s", AvatarURLPrefix, userID, fileName)
	if err := s.userrepo.UpdateAvatarURL(ctx, userID, avatarURL); err != nil {
		_ = os.Remove(filePath) // 写库失败,删掉刚落的文件,避免孤儿
		slog.ErrorContext(ctx, "更新头像 URL 失败", "user_id", userID, "path", filePath, "err", err)
		return err
	}

	return nil
}
