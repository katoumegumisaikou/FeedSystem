package follow

import (
	"context"
	"errors"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MaxFollowees 单个用户的关注数上限。
// 只在上层服务(关注前校验)生效,查询本身不封顶:封顶是业务规则,
// 放在仓储层会污染「查所有关注」这个纯读语义
const MaxFollowees = 200

// FollowRepository 关注关系仓储接口(对外暴露的唯一契约)。
// 声明成接口而不是结构体:service 依赖抽象,测试里才能注入内存实现
type FollowRepository interface {
	// Follow 建立关注关系。重复关注不报错(幂等)。
	//
	// limit 是当前用户的关注数上限。计数与插入在同一个事务里、并用
	// pg_advisory_xact_lock 按 follower_id 串行化 —— 分成两条语句的话,
	// 并发请求会同时读到「还没满」然后一起插入,上限形同虚设。
	// 超限返回 ErrFolloweeLimit;被关注者不存在(users 外键失败)返回 ErrFolloweeNotFound。
	Follow(ctx context.Context, followerID, followeeID int64, limit int64) error

	// Unfollow 取关,物理删除。删不存在的行不算错(幂等),重复取关无害
	Unfollow(ctx context.Context, followerID, followeeID int64) error

	// IsFollowing 判断是否已关注。没关注返回 (false, nil) 而不是 ErrNotFound ——
	// 「没关注」是正常业务答案,不是异常,调用方不必做错误分支
	IsFollowing(ctx context.Context, followerID, followeeID int64) (bool, error)

	// CountFollowees 某用户关注了多少人(走 idx_follows_follower)
	CountFollowees(ctx context.Context, followerID int64) (int64, error)

	// CountFollowers 某人有多少粉丝(走 idx_follows_followee)
	CountFollowers(ctx context.Context, followeeID int64) (int64, error)

	// ListFolloweeIDs 取某用户关注的全部人 ID,按 followee_id 升序保证确定性。
	// 上限由服务层按 MaxFollowees 把关,查询不截断
	ListFolloweeIDs(ctx context.Context, followerID int64) ([]int64, error)

	// ListFollowerIDs 取某人的粉丝 ID 一页,游标分页 by follower_id。
	// afterID <= 0 表示首页。用游标而非 OFFSET:大 V 可能有上万粉丝,
	// fan-out 要一页页翻完,OFFSET 越翻越慢(每页都从头数)
	ListFollowerIDs(ctx context.Context, followeeID, afterID int64, limit int) ([]int64, error)

	// ListBigVAuthorIDs 取粉丝数达到 threshold 的作者 ID(大 V)。
	// 扫描 idx_follows_followee 做分组聚合,只在冷启动 / 周期性重算时跑,
	// 不在请求路径上调用
	ListBigVAuthorIDs(ctx context.Context, threshold int64) ([]int64, error)
}

// ErrNotFound 仓储层「没查到」的哨兵错误。
// 不把 gorm.ErrRecordNotFound 透出去:它会一路冒到 response.Error,
// 而那里的类型断言认不出它不是 ServiceErr,最后只会给前端「未知错误」
var ErrNotFound = errors.New("follow: not found")

// ErrFolloweeLimit 关注数超上限的哨兵错误。
// 和 ErrNotFound 分开成一个独立哨兵:调用方要把它映射成业务错误(409),
// 不能像其它仓储错误那样统一吞成 500
var ErrFolloweeLimit = errors.New("follow: followee limit exceeded")

// ErrFolloweeNotFound 被关注者不存在(外键约束失败)。
// 单独一个哨兵:服务层要把它映射成 404,不能像其它仓储错误那样吞成 500。
// 触发条件是 follows.followee_id → users.id 的外键失败(SQLSTATE 23503)
var ErrFolloweeNotFound = errors.New("follow: followee not found")

// isForeignKeyViolation 判断错误是否为 PostgreSQL 外键约束失败(SQLSTATE 23503)。
//
// 用结构化的 SQLState() 而非匹配错误串:错误文案会随 PG 版本 / 语言变,靠字符串很脆。
// 同时兼容今后若在 gorm.Config 打开 TranslateError 后返回的 gorm.ErrForeignKeyViolated。
func isForeignKeyViolation(err error) bool {
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		return true
	}
	var st interface{ SQLState() string }
	return errors.As(err, &st) && st.SQLState() == "23503"
}

// followRepository 基于 GORM 的实现(包内私有,外部只能通过接口访问)
type followRepository struct {
	db *gorm.DB
}

// 编译期断言:确保 followRepository 实现 FollowRepository 接口
var _ FollowRepository = (*followRepository)(nil)

// NewFollowRepository 构造 FollowRepository;返回接口类型以隐藏 GORM 实现细节
func NewFollowRepository(db *gorm.DB) FollowRepository {
	return &followRepository{db: db}
}

// Follow 在事务里按 follower_id 取咨询锁,把「计数」与「插入」串成原子操作。
//
// follows 没有 per-follower 的行可锁(关系本身就是行),所以用 pg_advisory_xact_lock
// 按 follower_id 串行化同一关注者的并发请求:没有锁的话,两个请求会同时读到
// 「还没满」然后各自插入,上限形同虚设。xact 版随事务结束自动释放,无需显式解锁。
//
// 锁键用两参数形式 pg_advisory_xact_lock(int, int),第一个参数是命名空间。单 bigint
// 形式(hashtext 之前)把裸 follower_id 直接丢进全局锁空间,日后任何也用单 bigint
// 键的功能都可能与它撞锁、互相串行。命名空间把 follow 的锁圈进自己的领地。
//
// 为什么第二个参数也走 hashtext 而不是直传 follower_id:重载只有 (bigint) 与 (int,int)
// 两个,follower_id 是 int64,GORM/pgx 会按 int8 发送,PG 找不到 (int,bigint) 重载会
// 直接报错;hashtext(text) 返回 int4,正好落进 (int,int)。不同 follower 若哈希撞车,
// 只是两者偶尔多串行一次(安全),不影响上限的正确性。
//
// 计数 >= limit 时返回 ErrFolloweeLimit(事务回滚,不落库);重复关注靠
// ON CONFLICT DO NOTHING 幂等。CreatedAt 由 GORM 按约定名自动填充,不用手动赋值。
// 外键失败(被关注者不存在)→ ErrFolloweeNotFound,由服务层映射成 404
func (r *followRepository) Follow(ctx context.Context, followerID, followeeID int64, limit int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 事务级咨询锁:同一 follower_id 的并发关注在这里排队,锁在 COMMIT/ROLLBACK 时释放。
		// 第一个参数是命名空间,见函数头注释
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('follows'), hashtext(?))",
			strconv.FormatInt(followerID, 10)).Error; err != nil {
			return err
		}

		var count int64
		if err := tx.Model(&Follow{}).
			Where("follower_id = ?", followerID).
			Count(&count).Error; err != nil {
			return err
		}
		if count >= limit {
			return ErrFolloweeLimit
		}

		err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&Follow{FollowerID: followerID, FolloweeID: followeeID}).Error
		if err != nil && isForeignKeyViolation(err) {
			// 外键失败只可能来自 followee_id:followee_id 指向一个不存在的用户。
			// (follower_id 是已鉴权的当前用户,常规下必然存在。)转成业务哨兵,
			// 由服务层映射成 404 —— 不能让用户瞎填的 id 冒成 500
			return ErrFolloweeNotFound
		}
		return err
	})
}

// Unfollow 按两列删除。删不到行时 Delete 的 RowsAffected 为 0,但不是错误,
// 所以直接返回 .Error 即可(本表无软删除,是真 DELETE)
func (r *followRepository) Unfollow(ctx context.Context, followerID, followeeID int64) error {
	return r.db.WithContext(ctx).
		Where("follower_id = ? AND followee_id = ?", followerID, followeeID).
		Delete(&Follow{}).Error
}

func (r *followRepository) IsFollowing(ctx context.Context, followerID, followeeID int64) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Follow{}).
		Where("follower_id = ? AND followee_id = ?", followerID, followeeID).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *followRepository) CountFollowees(ctx context.Context, followerID int64) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Follow{}).
		Where("follower_id = ?", followerID).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *followRepository) CountFollowers(ctx context.Context, followeeID int64) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Follow{}).
		Where("followee_id = ?", followeeID).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *followRepository) ListFolloweeIDs(ctx context.Context, followerID int64) ([]int64, error) {
	var ids []int64
	err := r.db.WithContext(ctx).Model(&Follow{}).
		Where("follower_id = ?", followerID).
		Order("followee_id ASC").
		Pluck("followee_id", &ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *followRepository) ListFollowerIDs(ctx context.Context, followeeID, afterID int64, limit int) ([]int64, error) {
	q := r.db.WithContext(ctx).Model(&Follow{}).Where("followee_id = ?", followeeID)
	// afterID <= 0 是首页,不加游标边界,直接取最前面 limit 条
	if afterID > 0 {
		q = q.Where("follower_id > ?", afterID)
	}

	var ids []int64
	err := q.Order("follower_id ASC").Limit(limit).Pluck("follower_id", &ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *followRepository) ListBigVAuthorIDs(ctx context.Context, threshold int64) ([]int64, error) {
	var ids []int64
	err := r.db.WithContext(ctx).Model(&Follow{}).
		Group("followee_id").
		Having("count(*) >= ?", threshold).
		Pluck("followee_id", &ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}
