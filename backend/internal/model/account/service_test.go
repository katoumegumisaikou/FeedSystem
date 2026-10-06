package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/pkg/errs"
	"feed-system/internal/pkg/token"
	"feed-system/internal/util/filetype"
	"feed-system/internal/util/password"
)

// 测试替身

// fakeUserRepo 内存版 UserRepository。不用 mock 框架:service 依赖「读→改→写」
// 的状态流转,有状态的内存实现比逐次 stub 更贴近真实仓储
type fakeUserRepo struct {
	mu     sync.Mutex
	users  map[int64]*User
	nextID int64
	errs   map[string]error // 方法名 → 强制返回的错误(模拟 DB 故障)
	calls  map[string]int   // 方法名 → 调用次数(断言副作用)
}

var _ UserRepository = (*fakeUserRepo)(nil)

func newFakeRepo() *fakeUserRepo {
	return &fakeUserRepo{
		users:  make(map[int64]*User),
		nextID: 1,
		errs:   make(map[string]error),
		calls:  make(map[string]int),
	}
}

// 让指定方法强制返回 err
func (r *fakeUserRepo) failWith(method string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs[method] = err
}

func (r *fakeUserRepo) callCount(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[method]
}

// 直接写入并返回落库副本,不计入调用统计,避免污染副作用断言
func (r *fakeUserRepo) seed(u *User) *User {
	r.mu.Lock()
	defer r.mu.Unlock()
	u.ID = r.nextID
	r.nextID++
	// 模拟 PostgreSQL 的 DEFAULT 1:GORM 会把 DB 生成的默认值回填进结构体
	if u.Version == 0 {
		u.Version = 1
	}
	cp := *u
	r.users[u.ID] = &cp
	return &cp
}

// 读取当前落库快照
func (r *fakeUserRepo) get(id int64) *User {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return nil
	}
	cp := *u
	return &cp
}

func (r *fakeUserRepo) Create(_ context.Context, user *User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls["Create"]++
	if err := r.errs["Create"]; err != nil {
		return err
	}
	user.ID = r.nextID
	r.nextID++
	if user.Version == 0 {
		user.Version = 1
	}
	cp := *user
	r.users[user.ID] = &cp
	return nil
}

func (r *fakeUserRepo) findBy(method string, match func(*User) bool) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[method]++
	if err := r.errs[method]; err != nil {
		return nil, err
	}
	for _, u := range r.users {
		if match(u) {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (r *fakeUserRepo) FindByID(_ context.Context, id int64) (*User, error) {
	return r.findBy("FindByID", func(u *User) bool { return u.ID == id })
}

func (r *fakeUserRepo) FindByPhone(_ context.Context, phone string) (*User, error) {
	return r.findBy("FindByPhone", func(u *User) bool { return u.Phone == phone })
}

func (r *fakeUserRepo) FindByUserName(_ context.Context, name string) (*User, error) {
	return r.findBy("FindByUserName", func(u *User) bool { return u.UserName == name })
}

func (r *fakeUserRepo) FindByEmail(_ context.Context, email string) (*User, error) {
	return r.findBy("FindByEmail", func(u *User) bool {
		return u.Email != nil && *u.Email == email
	})
}

func (r *fakeUserRepo) Update(_ context.Context, user *User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls["Update"]++
	if err := r.errs["Update"]; err != nil {
		return err
	}
	if _, ok := r.users[user.ID]; !ok {
		return ErrNotFound
	}
	cp := *user
	r.users[user.ID] = &cp
	return nil
}

func (r *fakeUserRepo) UpdateLastLoginAt(_ context.Context, id int64, t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls["UpdateLastLoginAt"]++
	if err := r.errs["UpdateLastLoginAt"]; err != nil {
		return err
	}
	u, ok := r.users[id]
	if !ok {
		return ErrNotFound
	}
	u.LastLoginAt = &t
	return nil
}

func (r *fakeUserRepo) UpdateAvatarURL(_ context.Context, id int64, url string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls["UpdateAvatarURL"]++
	if err := r.errs["UpdateAvatarURL"]; err != nil {
		return err
	}
	u, ok := r.users[id]
	if !ok {
		return ErrNotFound
	}
	u.AvatarURL = url
	return nil
}

func (r *fakeUserRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls["Delete"]++
	if err := r.errs["Delete"]; err != nil {
		return err
	}
	delete(r.users, id)
	return nil
}

// 测试脚手架

const (
	testPassword = "Passw0rd!x" // 字母 + 数字 + 符号,满足 password.Strong
	testPhone    = "13800001111"
	testPhone2   = "13800002222"
	testUserName = "alice"
)

var (
	hashOnce sync.Once
	hashVal  string
)

// 全进程只算一次 bcrypt;cost=12 约 250ms,逐条现算会拖慢整个套件
func testHash(t *testing.T) string {
	t.Helper()
	hashOnce.Do(func() {
		h, err := password.Hash(testPassword)
		require.NoError(t, err)
		hashVal = h
	})
	return hashVal
}

// 返回 devMode=true 的 service + Redis 替身
func newSvc(t *testing.T) (*AccountService, *fakeUserRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := newFakeRepo()
	return NewAccountService(repo, rdb, true), repo, mr
}

// 落一个可登录的用户
func seedUser(t *testing.T, repo *fakeUserRepo) *User {
	t.Helper()
	return repo.seed(&User{
		UserName: testUserName,
		Password: testHash(t),
		Phone:    testPhone,
	})
}

// 断言 err 是 business ServiceErr 且 Code 匹配
func assertCode(t *testing.T, err error, want errs.ServiceErr) {
	t.Helper()
	require.Error(t, err)
	got, ok := errs.As(err)
	require.True(t, ok, "错误应是 errs.ServiceErr(前端才能拿到业务码),实际是裸 error: %v", err)
	assert.Equal(t, want.Code, got.Code, "错误码不匹配,msg=%q", got.Msg)
}

// 解析并校验 token,失败直接终止
func claimsOf(t *testing.T, tok string) *token.Claims {
	t.Helper()
	require.NotEmpty(t, tok, "token 不应为空")
	c, err := token.Parse(tok)
	require.NoError(t, err, "token 应可解析")
	return c
}

// Register

func TestRegister(t *testing.T) {
	newReq := func() RegisterReq {
		return RegisterReq{
			UserName: testUserName,
			Password: testPassword,
			Phone:    testPhone,
		}
	}

	t.Run("注册成功_密码以哈希落库且_token_携带版本号", func(t *testing.T) {
		svc, repo, _ := newSvc(t)

		resp, err := svc.Register(context.Background(), newReq())
		require.NoError(t, err)
		require.NotNil(t, resp)

		claims := claimsOf(t, resp.AccessToken)
		claimsOf(t, resp.RefreshToken)

		// 密码必须哈希落库,原文不能出现在库里
		stored := repo.get(claims.UserID)
		require.NotNil(t, stored)
		assert.NotEqual(t, testPassword, stored.Password, "密码不能明文落库")
		assert.True(t, password.Verify(stored.Password, testPassword), "哈希应能校验通过")

		// token 的 version 必须等于 DB 版本,否则鉴权中间件比对会 401
		assert.Equal(t, stored.Version, claims.Version,
			"签发 token 用的 version 必须与落库版本一致")
		assert.Equal(t, int64(1), claims.Version, "GORM default:1 回填后版本应为 1")
	})

	t.Run("用户名已占用_返回冲突", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)

		_, err := svc.Register(context.Background(), newReq())
		assertCode(t, err, errs.ErrConflict)
	})

	t.Run("手机号已注册_返回冲突", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)

		req := newReq()
		req.UserName = "bob"
		_, err := svc.Register(context.Background(), req)
		assertCode(t, err, errs.ErrConflict)
	})

	t.Run("邮箱已注册_返回冲突", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		email := "taken@example.com"
		repo.seed(&User{UserName: "bob", Password: testHash(t), Phone: testPhone2, Email: &email})

		req := newReq()
		newEmail := "taken@example.com"
		req.Email = &newEmail
		_, err := svc.Register(context.Background(), req)
		assertCode(t, err, errs.ErrConflict)
	})

	t.Run("用户名不合法_返回参数错误", func(t *testing.T) {
		svc, _, _ := newSvc(t)

		req := newReq()
		req.UserName = "1alice" // 数字开头,username.Validate 应拒绝
		_, err := svc.Register(context.Background(), req)
		assertCode(t, err, errs.ErrInvalidParam)
	})

	t.Run("密码强度不足_返回参数错误", func(t *testing.T) {
		svc, _, _ := newSvc(t)

		req := newReq()
		req.Password = "aaaaaaaa" // 只有字母,不足两类
		_, err := svc.Register(context.Background(), req)
		assertCode(t, err, errs.ErrInvalidParam)
	})

	t.Run("仓储查询出错_应返回业务错误而非裸_error", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		repo.failWith("FindByUserName", errors.New("connection refused"))

		_, err := svc.Register(context.Background(), newReq())

		// 裸 error 到 response 层会退化成 code=1「操作失败」,丢掉业务码
		assertCode(t, err, errs.ErrInternal)
	})
}

// Login

func TestLogin(t *testing.T) {
	newReq := func() LoginReq {
		return LoginReq{Phone: testPhone, Password: testPassword}
	}

	t.Run("登录成功_签发_token_并记录登录时间", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		u := seedUser(t, repo)

		resp, err := svc.Login(context.Background(), newReq())
		require.NoError(t, err)
		require.NotNil(t, resp)

		claims := claimsOf(t, resp.AccessToken)
		assert.Equal(t, u.ID, claims.UserID)
		assert.Equal(t, u.Version, claims.Version)
		assert.Equal(t, 1, repo.callCount("UpdateLastLoginAt"), "登录成功应写一次 last_login_at")
		assert.NotNil(t, repo.get(u.ID).LastLoginAt)
	})

	t.Run("密码错误_返回未授权", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)

		req := newReq()
		req.Password = "WrongPwd1!"
		_, err := svc.Login(context.Background(), req)
		assertCode(t, err, errs.ErrUnauthorized)
	})

	t.Run("用户不存在_也必须返回未授权而不是资源不存在", func(t *testing.T) {
		svc, _, _ := newSvc(t)

		req := newReq()
		req.Phone = "13900009999"
		_, err := svc.Login(context.Background(), req)

		// 若返回 ErrNotFound,攻击者可据此枚举出哪些手机号已注册
		assertCode(t, err, errs.ErrUnauthorized)
	})

	t.Run("锁定阈值为5次_前5次都应是密码错误", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)
		ctx := context.Background()

		bad := newReq()
		bad.Password = "WrongPwd1!"
		for i := 1; i <= 5; i++ {
			_, err := svc.Login(ctx, bad)
			got, ok := errs.As(err)
			require.True(t, ok, "第 %d 次失败应返回业务错误,实际: %v", i, err)
			// 计数 key 带上 TTL 后,loginLockCheck 会把它当成「已锁定」
			require.Equal(t, errs.ErrUnauthorized.Code, got.Code,
				"才错到第 %d 次就报「%s」:锁定阈值退化成了 1 次", i, got.Msg)
		}

		// 第 6 次(即使密码正确)才应被挡
		_, err := svc.Login(ctx, newReq())
		assertCode(t, err, errs.ErrTooFrequent)
	})

	t.Run("锁定到期后可以重新登录", func(t *testing.T) {
		svc, repo, mr := newSvc(t)
		seedUser(t, repo)

		bad := newReq()
		bad.Password = "WrongPwd1!"
		for i := 0; i < 5; i++ {
			_, _ = svc.Login(context.Background(), bad)
		}

		mr.FastForward(2 * time.Minute)

		_, err := svc.Login(context.Background(), newReq())
		assert.NoError(t, err, "锁定期满后应可正常登录")
	})

	t.Run("登录成功清除失败计数", func(t *testing.T) {
		svc, repo, mr := newSvc(t)
		seedUser(t, repo)
		ctx := context.Background()

		bad := newReq()
		bad.Password = "WrongPwd1!"
		_, _ = svc.Login(ctx, bad)
		require.True(t, mr.Exists("feed:fail:"+testPhone), "失败后应留下计数 key")

		_, err := svc.Login(ctx, newReq())
		// 报「失败次数过多」就是同一个阈值 bug:一次失败就锁住用户
		require.NoError(t, err, "密码正确应能登录")

		assert.False(t, mr.Exists("feed:fail:"+testPhone), "成功登录应清零失败计数")
	})
}

// Logout

func TestLogout(t *testing.T) {
	t.Run("双_token_均进入黑名单", func(t *testing.T) {
		svc, repo, mr := newSvc(t)
		seedUser(t, repo)

		resp, err := svc.Login(context.Background(), LoginReq{Phone: testPhone, Password: testPassword})
		require.NoError(t, err)

		err = svc.Logout(context.Background(), resp.AccessToken, resp.RefreshToken)
		require.NoError(t, err)

		assert.True(t, mr.Exists("feed:revoked:refresh:"+resp.RefreshToken), "refresh_token 应被拉黑")
		assert.True(t, mr.Exists("feed:revoked:access:"+resp.AccessToken), "access_token 应被拉黑")
	})

	t.Run("refresh_token_缺失_返回未授权", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		err := svc.Logout(context.Background(), "whatever", "")
		assertCode(t, err, errs.ErrUnauthorized)
	})
}

// GetProfile / UpdateProfile

func TestGetProfile(t *testing.T) {
	t.Run("查他人资料只返回公开字段", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		email := "alice@example.com"
		lastLogin := time.Now()
		u := repo.seed(&User{
			UserName: testUserName, Password: testHash(t), Phone: testPhone,
			Email: &email, LastLoginAt: &lastLogin,
		})

		resp, err := svc.GetProfile(context.Background(), u.ID)
		require.NoError(t, err)
		assert.Equal(t, u.ID, resp.ID)
		assert.Equal(t, testUserName, resp.UserName)

		// PublicUserResp 无 Phone/Email/LastLoginAt 字段,再序列化确认没别的路径漏出去
		raw, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), testPhone, "查他人资料不该带手机号")
		assert.NotContains(t, string(raw), "alice@example.com", "查他人资料不该带邮箱")
		assert.NotContains(t, string(raw), "last_login_at", "最近登录时间会暴露作息,也不该带")
	})

	t.Run("查自己返回完整资料", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		u := seedUser(t, repo)

		resp, err := svc.GetMyProfile(context.Background(), u.ID)
		require.NoError(t, err)
		assert.Equal(t, testPhone, resp.Phone, "自己的手机号要能看到")
	})

	t.Run("用户不存在_返回资源不存在", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		_, err := svc.GetProfile(context.Background(), 999)
		assertCode(t, err, errs.ErrNotFound)
	})
}

func TestUpdateProfile(t *testing.T) {
	t.Run("修改邮箱成功", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		u := seedUser(t, repo)

		email := "new@example.com"
		resp, err := svc.UpdateProfile(context.Background(), u.ID, UpdateProfileReq{Email: &email})
		require.NoError(t, err)
		require.NotNil(t, resp.Email)
		assert.Equal(t, "new@example.com", *repo.get(u.ID).Email)
	})

	t.Run("邮箱与他人冲突_返回冲突", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		taken := "taken@example.com"
		repo.seed(&User{UserName: "bob", Password: testHash(t), Phone: testPhone2, Email: &taken})
		u := seedUser(t, repo)

		email := "taken@example.com"
		_, err := svc.UpdateProfile(context.Background(), u.ID, UpdateProfileReq{Email: &email})
		assertCode(t, err, errs.ErrConflict)
	})

	t.Run("提交自己当前的邮箱_不应误判冲突", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		mine := "mine@example.com"
		u := repo.seed(&User{
			UserName: testUserName, Password: testHash(t), Phone: testPhone, Email: &mine,
		})

		same := "mine@example.com"
		_, err := svc.UpdateProfile(context.Background(), u.ID, UpdateProfileReq{Email: &same})
		assert.NoError(t, err)
	})

	t.Run("只改头像时不动邮箱", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		email := "keep@example.com"
		u := repo.seed(&User{
			UserName: testUserName, Password: testHash(t), Phone: testPhone, Email: &email,
		})

		avatar := "/avatars/1/x.png"
		_, err := svc.UpdateProfile(context.Background(), u.ID, UpdateProfileReq{AvatarURL: &avatar})
		require.NoError(t, err)

		stored := repo.get(u.ID)
		assert.Equal(t, avatar, stored.AvatarURL)
		require.NotNil(t, stored.Email)
		assert.Equal(t, "keep@example.com", *stored.Email, "未提交的字段不应被清空")
	})
}

// SMS 验证码 / 改密

func TestSendSmsCode(t *testing.T) {
	t.Run("dev_模式返回6位数字码并写入_Redis", func(t *testing.T) {
		svc, _, mr := newSvc(t)

		code, err := svc.SendSmsCode(context.Background(), SendSmsCodeReq{Phone: testPhone})
		require.NoError(t, err)
		assert.Len(t, code, 6)

		saved, err := mr.Get("feed:sms:" + testPhone)
		require.NoError(t, err)
		assert.Equal(t, code, saved)
		assert.Equal(t, 5*time.Minute, mr.TTL("feed:sms:"+testPhone))
	})

	t.Run("生产模式不返回验证码", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })

		svc := NewAccountService(newFakeRepo(), rdb, false)
		code, err := svc.SendSmsCode(context.Background(), SendSmsCodeReq{Phone: testPhone})
		require.NoError(t, err)
		assert.Empty(t, code, "生产模式绝不能把验证码回给客户端")
	})

	t.Run("未配置_Redis_返回内部错误", func(t *testing.T) {
		svc := NewAccountService(newFakeRepo(), nil, true)
		_, err := svc.SendSmsCode(context.Background(), SendSmsCodeReq{Phone: testPhone})
		assertCode(t, err, errs.ErrInternal)
	})
}

func TestChangePassword(t *testing.T) {
	// 发一个验证码并返回明文(devMode=true 直接拿到)
	issue := func(t *testing.T, svc *AccountService) string {
		t.Helper()
		code, err := svc.SendSmsCode(context.Background(), SendSmsCodeReq{Phone: testPhone})
		require.NoError(t, err)
		return code
	}

	t.Run("改密成功_验证码被消费且版本号递增", func(t *testing.T) {
		svc, repo, mr := newSvc(t)
		u := seedUser(t, repo)
		phone := testPhone
		code := issue(t, svc)

		// 先造出「version 已进缓存」这个现场 —— 真实线上必然如此:
		// 用户改密前一定用过 App,任意一次鉴权请求都会把这个 key 填上
		require.NoError(t, mr.Set("feed:user:version:"+strconv.FormatInt(u.ID, 10), strconv.FormatInt(u.Version, 10)))

		resp, err := svc.ChangePassword(context.Background(), ChangePasswordReq{
			Phone: phone, Password: "NewPassw0rd!", SmsCode: code,
		})
		require.NoError(t, err)

		// 旧 token 必须失效 → version 递增,新 token 用新版本签发
		stored := repo.get(u.ID)
		assert.Equal(t, u.Version+1, stored.Version)
		assert.Equal(t, stored.Version, claimsOf(t, resp.AccessToken).Version)

		assert.True(t, password.Verify(stored.Password, "NewPassw0rd!"))
		assert.False(t, password.Verify(stored.Password, testPassword))

		assert.False(t, mr.Exists("feed:sms:"+phone), "验证码校验通过后必须删除")

		// ★ version 缓存必须被删。不删的话 middleware.checkUserVersion 会继续读
		// 缓存里的旧值,「旧 token 立刻失效」实际变成「最长 versionCacheTTL 后失效」
		assert.False(t, mr.Exists("feed:user:version:"+strconv.FormatInt(u.ID, 10)),
			"改密后必须清掉 version 缓存,否则旧 access_token 还能用 5 分钟")
	})

	t.Run("更新用户失败_版本号与缓存都不动", func(t *testing.T) {
		// 对照组:失效必须排在 Update 成功之后。若写库失败却清了缓存,
		// 缓存虽然被清但 version 没变 —— 下次读回原值,行为上无害,
		// 但「清缓存」与「version 已递增」被解耦了,以后改动容易踩空
		svc, repo, mr := newSvc(t)
		u := seedUser(t, repo)
		code := issue(t, svc)
		repo.failWith("Update", errors.New("db down"))

		key := "feed:user:version:" + strconv.FormatInt(u.ID, 10)
		require.NoError(t, mr.Set(key, strconv.FormatInt(u.Version, 10)))

		_, err := svc.ChangePassword(context.Background(), ChangePasswordReq{
			Phone: testPhone, Password: "NewPassw0rd!", SmsCode: code,
		})
		assertCode(t, err, errs.ErrInternal)

		assert.Equal(t, u.Version, repo.get(u.ID).Version, "失败时不该递增")
		assert.True(t, mr.Exists(key), "写库失败时不该动缓存")
	})

	t.Run("验证码错误_密码不变", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		u := seedUser(t, repo)
		issue(t, svc)

		_, err := svc.ChangePassword(context.Background(), ChangePasswordReq{
			Phone: testPhone, Password: "NewPassw0rd!", SmsCode: "000000",
		})
		assertCode(t, err, errs.ErrInvalidParam)
		assert.Equal(t, u.Version, repo.get(u.ID).Version, "失败时不应改动用户")
	})

	t.Run("验证码重复使用_第二次被拒", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)
		code := issue(t, svc)
		ctx := context.Background()

		_, err := svc.ChangePassword(ctx, ChangePasswordReq{
			Phone: testPhone, Password: "NewPassw0rd!", SmsCode: code,
		})
		require.NoError(t, err)

		_, err = svc.ChangePassword(ctx, ChangePasswordReq{
			Phone: testPhone, Password: "Another0ne!", SmsCode: code,
		})
		assertCode(t, err, errs.ErrInvalidParam)
	})

	t.Run("新密码与旧密码相同_返回参数错误", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)
		code := issue(t, svc)

		_, err := svc.ChangePassword(context.Background(), ChangePasswordReq{
			Phone: testPhone, Password: testPassword, SmsCode: code,
		})
		assertCode(t, err, errs.ErrInvalidParam)
	})

	t.Run("用户不存在_返回资源不存在", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		code := issue(t, svc)

		_, err := svc.ChangePassword(context.Background(), ChangePasswordReq{
			Phone: testPhone, Password: "NewPassw0rd!", SmsCode: code,
		})
		assertCode(t, err, errs.ErrNotFound)
	})

	t.Run("Redis_不可用时不应伪装成验证码错误", func(t *testing.T) {
		svc, repo, mr := newSvc(t)
		seedUser(t, repo)
		code := issue(t, svc)
		mr.Close() // 模拟 Redis 宕机

		_, err := svc.ChangePassword(context.Background(), ChangePasswordReq{
			Phone: testPhone, Password: "NewPassw0rd!", SmsCode: code,
		})

		// readSmsCode 把 redis.Nil 与其他 err 分开:连接失败必须报内部错误,
		// 不能因为 saved 是空串就退化成「验证码错误」
		assertCode(t, err, errs.ErrInternal)
	})

	t.Run("业务校验失败不消耗验证码_同一个码可重试", func(t *testing.T) {
		// 这两种失败都不该让用户白等一条短信
		cases := []struct {
			name     string
			password string
		}{
			{"密码强度不足", "abcdefgh"},
			{"新密码与旧密码相同", testPassword},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				svc, repo, mr := newSvc(t)
				seedUser(t, repo)
				code := issue(t, svc)
				ctx := context.Background()

				_, err := svc.ChangePassword(ctx, ChangePasswordReq{
					Phone: testPhone, Password: tc.password, SmsCode: code,
				})
				require.Error(t, err)
				assert.True(t, mr.Exists("feed:sms:"+testPhone),
					"校验失败不应消耗验证码,否则用户要重新发短信")

				_, err = svc.ChangePassword(ctx, ChangePasswordReq{
					Phone: testPhone, Password: "NewPassw0rd!", SmsCode: code,
				})
				assert.NoError(t, err, "同一个验证码应还能用")
			})
		}
	})

	t.Run("用户不存在时也不消耗验证码", func(t *testing.T) {
		svc, _, mr := newSvc(t) // 故意不落用户
		code := issue(t, svc)

		_, err := svc.ChangePassword(context.Background(), ChangePasswordReq{
			Phone: testPhone, Password: "NewPassw0rd!", SmsCode: code,
		})
		assertCode(t, err, errs.ErrNotFound)
		assert.True(t, mr.Exists("feed:sms:"+testPhone), "查无此人时也不该烧掉验证码")
	})

	t.Run("验证码错误优先于业务校验_不泄露码是否正确", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)
		code := issue(t, svc)

		// 避开 1/10^6 的随机撞车
		wrong := "000000"
		if wrong == code {
			wrong = "111111"
		}

		// 同时给出「验证码错误」和「密码强度不足」时必须报前者,
		// 否则攻击者拿弱密码试码,收到密码相关报错就知道自己猜中了
		_, err := svc.ChangePassword(context.Background(), ChangePasswordReq{
			Phone: testPhone, Password: "abcdefgh", SmsCode: wrong,
		})
		got, ok := errs.As(err)
		require.True(t, ok)
		assert.Contains(t, got.Msg, "验证码", "错误优先级泄漏了验证码是否正确")
	})
}

// RefreshToken

func TestRefreshToken(t *testing.T) {
	login := func(t *testing.T, svc *AccountService) *TokenResp {
		t.Helper()
		resp, err := svc.Login(context.Background(), LoginReq{Phone: testPhone, Password: testPassword})
		require.NoError(t, err)
		return resp
	}

	t.Run("正常刷新_旧_refresh_token_进入黑名单", func(t *testing.T) {
		svc, repo, mr := newSvc(t)
		seedUser(t, repo)
		ctx := context.Background()

		old := login(t, svc)
		next, err := svc.RefreshToken(ctx, old.RefreshToken)
		require.NoError(t, err)
		require.NotNil(t, next)

		assert.True(t, mr.Exists("feed:revoked:refresh:"+old.RefreshToken),
			"用过的 refresh_token 必须拉黑,防重放")
	})

	t.Run("已用过的_refresh_token_再次使用被拒", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)
		ctx := context.Background()

		old := login(t, svc)
		_, err := svc.RefreshToken(ctx, old.RefreshToken)
		require.NoError(t, err)

		_, err = svc.RefreshToken(ctx, old.RefreshToken)
		assertCode(t, err, errs.ErrUnauthorized)
	})

	t.Run("版本不匹配_返回未授权", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		u := seedUser(t, repo)
		ctx := context.Background()

		old := login(t, svc)

		// 模拟别处改了密码:DB 版本 +1,手里的 refresh_token 版本落后
		stale := repo.get(u.ID)
		stale.Version++
		require.NoError(t, repo.Update(ctx, stale))

		_, err := svc.RefreshToken(ctx, old.RefreshToken)
		assertCode(t, err, errs.ErrUnauthorized)
	})

	t.Run("失效的_refresh_token_返回未授权", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		_, err := svc.RefreshToken(context.Background(), "not-a-jwt")
		assertCode(t, err, errs.ErrUnauthorized)
	})

	t.Run("刷新返回的_refresh_token_必须能继续使用", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		seedUser(t, repo)
		ctx := context.Background()

		old := login(t, svc)
		first, err := svc.RefreshToken(ctx, old.RefreshToken)
		require.NoError(t, err)

		// access_token 到期就刷一次,是客户端最普通的续期行为
		_, err = svc.RefreshToken(ctx, first.RefreshToken)
		assert.NoError(t, err, "刷新签发的新 refresh_token 必须可用,否则用户会被强制登出")

		// 根因:JWT 的 iat/exp 只有秒级精度,同一秒内同参数签发的 token 字节完全相同,
		// 于是「签发新 token」实际是「把刚拉黑的那个又发了一遍」
		assert.NotEqual(t, old.RefreshToken, first.RefreshToken,
			"新 refresh_token 与旧值相同 → 已被拉黑 → 下一次刷新必然 401")
	})
}

// toUserResp

func TestToUserResp(t *testing.T) {
	t.Run("nil_安全返回_nil", func(t *testing.T) {
		assert.Nil(t, toUserResp(nil))
	})

	t.Run("字段映射完整且不含敏感字段", func(t *testing.T) {
		now := time.Now()
		u := &User{
			ID: 7, UserName: "alice", Phone: testPhone,
			Password: "must-not-leak", Version: 3,
			AvatarURL: "/avatars/7/a.png", CreatedAt: now, LastLoginAt: &now,
		}

		resp := toUserResp(u)
		require.NotNil(t, resp)
		assert.Equal(t, int64(7), resp.ID)
		assert.Equal(t, "alice", resp.UserName)
		assert.Equal(t, "/avatars/7/a.png", resp.AvatarURL)
		assert.Equal(t, now, resp.CreatedAt)

		// UserResp 无 Password/Version/DeletedAt,序列化再确认一次,防止以后往 DTO 加字段
		raw, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "must-not-leak")
		assert.NotContains(t, string(raw), "password")
	})
}

// CheckUserVersion(中间件每次鉴权都会调,是 token 失效机制的执行者)

func TestCheckUserVersion(t *testing.T) {
	ctx := context.Background()

	t.Run("返回库里的当前版本号", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		u := seedUser(t, repo)

		// 模拟用户改过密码:DB 版本已经涨到 7
		bumped := repo.get(u.ID)
		bumped.Version = 7
		require.NoError(t, repo.Update(ctx, bumped))

		v, err := svc.CheckUserVersion(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(7), v, "必须回源 DB 拿最新版本,否则旧 token 判不出来")
	})

	t.Run("用户不存在返回资源不存在", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		_, err := svc.CheckUserVersion(ctx, 999)
		assertCode(t, err, errs.ErrNotFound)
	})

	t.Run("仓储出错返回内部错误", func(t *testing.T) {
		svc, repo, _ := newSvc(t)
		repo.failWith("FindByID", errors.New("db down"))

		_, err := svc.CheckUserVersion(ctx, 1)
		// 查不到版本时若返回 nil 错误,中间件会当成「校验通过」
		assertCode(t, err, errs.ErrInternal)
	})
}

// UploadAvatar
//
// 只覆盖「校验失败」路径 —— 都在 os.MkdirAll 之前 return,不写磁盘。
// 成功路径(Seek 复位、孤儿清理、扩展名取自魔数)要把 AvatarStorageDir 换成
// 临时目录,它是硬编码 const,本轮不改生产代码,故覆盖不到

// 造一个带内容的 *multipart.FileHeader。content 是未导出字段,只能靠 multipart 往返生成
func avatarHeaderWith(t *testing.T, filename string, content []byte) *multipart.FileHeader {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("avatar", filename)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	form, err := multipart.NewReader(&buf, w.Boundary()).ReadForm(32 << 20)
	require.NoError(t, err)
	t.Cleanup(func() { _ = form.RemoveAll() })

	fhs := form.File["avatar"]
	require.Len(t, fhs, 1)
	return fhs[0]
}

func TestUploadAvatar(t *testing.T) {
	// 用一个不可能存在的 ID,好断言「被拒时没在磁盘上留下目录」
	const userID = 999999001
	ctx := context.Background()

	// 造「魔数合法 + 填充」的样本,长度达到 HeaderSize
	imageSample := func(magic []byte) []byte {
		b := make([]byte, filetype.HeaderSize+8)
		copy(b, magic)
		return b
	}

	rejectedWithoutDisk := func(t *testing.T, err error) {
		t.Helper()
		assertCode(t, err, errs.ErrInvalidParam)
		_, statErr := os.Stat(filepath.Join(AvatarStorageDir, fmt.Sprintf("%d", userID)))
		assert.True(t, os.IsNotExist(statErr),
			"被拒时不该创建任何目录 —— 说明它确实在 os.MkdirAll 之前就返回了")
	}

	t.Run("空文件被拒", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		// Size 是导出字段可直接构造;大小校验排在 Open 之前,无需 content
		fh := &multipart.FileHeader{Filename: "a.png", Size: 0}
		rejectedWithoutDisk(t, svc.UploadAvatar(ctx, fh, userID))
	})

	t.Run("超过10MiB被拒", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		fh := &multipart.FileHeader{Filename: "a.png", Size: maxAvatarSize + 1}
		rejectedWithoutDisk(t, svc.UploadAvatar(ctx, fh, userID))
	})

	t.Run("刚好10MiB不被大小校验拒绝", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		// 边界:判定是 > 而非 >=,等于上限应放行;这里不给 content,
		// 会在下一步 Open 失败 —— 恰好证明它过了大小校验
		fh := &multipart.FileHeader{Filename: "a.png", Size: maxAvatarSize}
		assertCode(t, svc.UploadAvatar(ctx, fh, userID), errs.ErrInternal)
	})

	t.Run("伪装成图片的HTML被拒", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		// 文件名带 .html,但类型由魔数判定,内容不是图片 → 拒
		fh := avatarHeaderWith(t, "evil.html", []byte("<!DOCTYPE html><html><script>alert(1)</script>"))
		rejectedWithoutDisk(t, svc.UploadAvatar(ctx, fh, userID))
	})

	t.Run("BMP能被识别但不在白名单_被拒", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		// DetectImage 认 "BM",但白名单没有 BMP:白名单把「能识别」收窄为「愿接受」
		fh := avatarHeaderWith(t, "a.bmp", imageSample([]byte("BM")))
		rejectedWithoutDisk(t, svc.UploadAvatar(ctx, fh, userID))
	})

	t.Run("内容不足12字节且无魔数_被拒", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		fh := avatarHeaderWith(t, "a.png", []byte{0x01, 0x02, 0x03})
		rejectedWithoutDisk(t, svc.UploadAvatar(ctx, fh, userID))
	})

	t.Run("Open_失败返回内部错误", func(t *testing.T) {
		svc, _, _ := newSvc(t)
		// 零值 FileHeader 无内存 content 也无临时文件,Open() 会 os.Open("") 失败 ——
		// 覆盖「读不出文件」这条分支
		fh := &multipart.FileHeader{Filename: "a.png", Size: 1024}
		assertCode(t, svc.UploadAvatar(ctx, fh, userID), errs.ErrInternal)
	})
}
