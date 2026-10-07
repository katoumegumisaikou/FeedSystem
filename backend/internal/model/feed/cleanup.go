package feed

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CleanupTask 一条已从 feed_cleanup_tasks 领取出来的清理任务。
//
// 同时带着 Window:窗口跟着 key 一起走,消费端就不必按 key 前缀去推断它是什么、
// 该用哪个窗口 —— 否则每加一类 key 都要回来改一次判断,漏改的后果是静默用错窗口。
type CleanupTask struct {
	ID       int64
	CacheKey string
	Window   time.Duration
}

// CleanupTaskRepository 关注流缓存清理任务的持久化队列。
//
// 只有「入队」与「领取」两个动作,没有 MarkDone / MarkFailed —— 领取即删除,
// 所以没有需要标记的终态。理由见迁移 013 的注释:清理是单步且幂等的,重复执行永远
// 安全,失败只需等下一次读重新入队,不需要重试计数与租约。
type CleanupTaskRepository interface {
	// EnqueueCleanup 把一批待清理的 key 入队。同一个 key 重复入队是幂等的:
	// 已经在队列里就不再插入。入队方是读路径,每个被判定为过期的 key 都会来一次。
	EnqueueCleanup(ctx context.Context, trims []staleTrim) error
	// ClaimCleanups 领一批任务,并**同时把它们从表中删除**(领取即消费)。
	//
	// 没有租约也没有「处理中」状态:领取方崩在清理中途时,这一轮就是没清成 ——
	// 而行已经没了,所以不会有卡住的行需要回收。代价是这一轮不重试,由下一次读
	// 重新入队补上(见迁移 013)。
	ClaimCleanups(ctx context.Context, limit int) ([]*CleanupTask, error)
}

// cleanupRepository 基于 GORM 的实现(包内私有,外部只经接口访问)。
type cleanupRepository struct {
	db *gorm.DB
}

// 编译期断言:确保 cleanupRepository 实现 CleanupTaskRepository 接口
var _ CleanupTaskRepository = (*cleanupRepository)(nil)

func NewCleanupRepository(db *gorm.DB) CleanupTaskRepository {
	return &cleanupRepository{db: db}
}

// EnqueueCleanup 批量入队,一批只发一条 INSERT。
//
// 用一条带多个 VALUES 的语句而不是循环单插:一次读请求里过期的 key 通常有好几个
// (一个收件箱 + 若干大V 缓存),循环的话就是好几次往返,而这几次往返是**在请求路径上**的。
//
// ON CONFLICT DO NOTHING 而不是「先查后插」:并发的两个读者会同时判定同一个 key 过期,
// 先查后插之间存在 TOCTOU,两边都查到「不存在」然后都去插,其中一方报唯一键冲突而整个
// 请求失败。交给数据库裁决,冲突直接跳过,不报错。
//
// 用 DO NOTHING(而不是 fanout.EnqueueReplay 那种「已完成/死信则重置」)是因为这里
// 没有终态行:领取即删除,所以「清理过一次之后又能再次入队」是天然成立的 ——
// 行已不在,新插就是新任务。这也正是「同一个 key 需要被反复清理」不会冲突的原因。
func (r *cleanupRepository) EnqueueCleanup(ctx context.Context, trims []staleTrim) error {
	if len(trims) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(trims))
	for _, t := range trims {
		rows = append(rows, map[string]any{
			"cache_key":      t.key,
			"window_seconds": int(t.window.Seconds()),
		})
	}
	return r.db.WithContext(ctx).
		Table("feed_cleanup_tasks").
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "cache_key"}},
			DoNothing: true,
		}).
		Create(&rows).Error
}

// cleanupRow 是领取时用来接住一次 DELETE ... RETURNING 的中间结构。
//
// 不能直接 Scan 进 CleanupTask:窗口在库里是 INT 秒(见迁移 013),而它是对外的
// time.Duration(纳秒),类型对不上。分开一层显式转换,比给 CleanupTask 挂 gorm 标签
// 再让 Duration 承担两种单位要清楚。
type cleanupRow struct {
	ID            int64  `gorm:"column:id"`
	CacheKey      string `gorm:"column:cache_key"`
	WindowSeconds int    `gorm:"column:window_seconds"`
}

// ClaimCleanups 领一批任务并删除它们。
//
// DELETE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED) RETURNING 是**单条语句**,
// 领取与删除天然原子 —— 不需要 fanout.ClaimPending 那样的显式事务把「查」与「改状态」
// 包起来,因为这里没有中间状态可停在上面。两个 worker 并发时,SKIP LOCKED 让后到的
// 直接跳过别人正在删的行,各领各的,不互相阻塞、也不会领到同一条。
//
// 按 id 升序领取(先进先出),失败的行不会留在表里反复占位,所以不存在
// ClaimPending 注释里那种「队头饥饿」—— 那边需要按 next_attempt_at 排序正是因为
// 退避的行会停在表里。
func (r *cleanupRepository) ClaimCleanups(ctx context.Context, limit int) ([]*CleanupTask, error) {
	var rows []cleanupRow
	err := r.db.WithContext(ctx).Raw(`
		DELETE FROM feed_cleanup_tasks
		WHERE id IN (
			SELECT id FROM feed_cleanup_tasks
			ORDER BY id
			LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, cache_key, window_seconds
	`, limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	tasks := make([]*CleanupTask, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, &CleanupTask{
			ID:       row.ID,
			CacheKey: row.CacheKey,
			Window:   time.Duration(row.WindowSeconds) * time.Second,
		})
	}
	return tasks, nil
}

// trimStaleKeys 把各 key 里超出各自存储窗口的成员删掉(一批合并成一条 pipeline)。
//
// 为什么要清理:写入侧的裁剪(见 inboxPushScript 的 ZREMRANGEBYSCORE)是
// 「写一次扫一次」—— 每投递一条视频顺手扫一次那个 key。**停止更新的作者是个例外**:
// 最后一次投递之后没有任何事件再去扫它,过期条目就一直留到 key TTL 到期。而读路径
// 每次都把它们取回来、再在内存里过滤掉,纯属白做功且会一直持续到 TTL。
//
// 用**存储窗口**(inboxEntry / bigVEntry)而不是读侧展示窗口(followingReadWindow):
// 存储窗口才是「这条数据该不该留在缓存里」的权威判据。两者目前同值,但如果哪天展示
// 窗口调窄(比如 3 天),用展示窗口删就会删掉写侧认为有效(3~7 天)的条目 —— 那是真
// 丢数据且不可恢复。只删存储窗口外的,读侧窗口怎么调都安全。
//
// 用 storage window 也正是要把窗口**存进任务行**的原因:选谁做判据是入队那一刻的决定,
// 不该由消费端凭 key 前缀重新猜一遍。
//
// 一批合成一条 pipeline:一次 ZRemRangeByScore 就是一次 Redis 往返,合并之后清 N 个
// key 的代价≈清 1 个。这也是走任务表的意义之一 —— 攒在表里的任务由 worker 一次性取走。
//
// 失败只返回错误让调用方记日志:清理是优化,不是正确性的一环 —— 正确性由读侧的展示
// 窗口过滤(见 ListFollowing 里的 cutoff 截断)保证。删不掉最多是下次请求再过滤一遍。
func trimStaleKeys(ctx context.Context, rdb *redis.Client, tasks []*CleanupTask) error {
	if len(tasks) == 0 || rdb == nil {
		return nil
	}
	// 同一时刻取一次 now:各 key 的窗口不同,但基准必须一致,否则同一条 pipeline 里
	// 两个 key 的边界会差几微秒 —— 无害但会让「为什么是两个不同的 cutoff」变得难解释
	now := time.Now()
	pipe := rdb.Pipeline()
	for _, t := range tasks {
		// 左开区间 "(":等于 cutoff 的成员保留,与写入侧脚本的 '(' .. ARGV[3] 一致
		pipe.ZRemRangeByScore(ctx, t.CacheKey, "-inf",
			"("+strconv.FormatInt(now.Add(-t.Window).UnixMicro(), 10))
	}
	_, err := pipe.Exec(ctx)
	return err
}

// runCleanupBatch 领一批清理任务并执行,供 worker 的消费循环调用。
//
// 只记日志、不向上返回错误:清理失败不该影响投递,也不该让循环退出。领取时行已经被
// 删掉了,所以这一轮失败不会重试 —— 补上它的是下一次读(会重新判定过期并重新入队)。
func (w *FanoutWorker) runCleanupBatch(ctx context.Context) {
	tasks, err := w.cleanups.ClaimCleanups(ctx, cleanupClaimBatch)
	if err != nil {
		slog.ErrorContext(ctx, "领取缓存清理任务失败", "err", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	if err := trimStaleKeys(ctx, w.rdb, tasks); err != nil {
		slog.ErrorContext(ctx, "清理过期的关注流缓存失败(不影响投递)",
			"keys", len(tasks), "err", err)
	}
}
