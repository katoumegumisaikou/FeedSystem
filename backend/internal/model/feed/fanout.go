package feed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FanoutTask 一条待投递的 fan-out 任务。
type FanoutTask struct {
	ID          int64
	VideoID     int64
	AuthorID    int64
	PublishedAt time.Time
	// Attempts 已失败次数(本次领取前的值)。worker 在投递失败(含 panic)重试时
	// 把它打进日志,便于观察某条任务反复失败。退避与死信的判定由仓储写库时完成
	// (见 fanoutNextState)。
	Attempts int
}

// outbox 任务的状态机。数值与 010/012 迁移一致,改动必须两边同步。
const (
	fanoutStatusPending    = 0 // 待处理,可被领取
	fanoutStatusDone       = 1 // 已完成
	fanoutStatusDead       = 2 // 死信,不再被领取
	fanoutStatusProcessing = 3 // 处理中,已被某个 worker 租用
)

const (
	// fanoutMaxAttempts 累计失败达到此值即转为死信,不再重试。
	fanoutMaxAttempts = 10
	// fanoutBackoffCap 指数退避的上限。
	fanoutBackoffCap = 10 * time.Minute
	// fanoutLease 处理中状态的租约时长。worker 崩在半路会留下 status = 3 的行,
	// 超过租约仍未续期就被判定为「原 worker 已死」,重新纳入领取。
	fanoutLease = 5 * time.Minute
	// fanoutMaxLastErrorLen last_error 入库前的截断长度,避免超长错误串把行撑大。
	fanoutMaxLastErrorLen = 500
)

// fanoutBackoff 第 attempts 次失败后的退避时长:2^attempts 秒,封顶 fanoutBackoffCap。
// 在 Go 里算而不用 SQL,是为了让退避策略一眼可读、可单独测。
func fanoutBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// 2^attempts 秒;先挡住大 attempts 的移位溢出(1<<attempts 到 20 已经远超上限)
	if attempts > 20 {
		return fanoutBackoffCap
	}
	if d := time.Second << attempts; d < fanoutBackoffCap {
		return d
	}
	return fanoutBackoffCap
}

// fanoutNextState 是 outbox 状态机的纯决策:给定「本次失败后的累计失败次数」(已自增),
// 返回该任务的新状态与本次退避时长。
//   - 达到 fanoutMaxAttempts → 死信(fanoutStatusDead),不再被领取;
//   - 否则退回待处理(fanoutStatusPending),按指数退避延后重试。
//
// 抽成纯函数是为了让「真实仓储」与「测试替身」共用同一份判定:只要 fake 调它,
// 改动判定逻辑就会同时被状态机测试覆盖到 —— 否则测试只验替身、验不到真实实现。
func fanoutNextState(attempts int) (status int8, backoff time.Duration) {
	if attempts >= fanoutMaxAttempts {
		return fanoutStatusDead, fanoutBackoff(attempts)
	}
	return fanoutStatusPending, fanoutBackoff(attempts)
}

// fanoutTruncateErr 把错误串截到上限,防止一条巨长的错误把整行撑爆。
//
// 截断必须落在 UTF-8 字符边界上。按字节硬切可能切断一个多字节字符(中文 3 字节/字),
// 产出非法字节序列 —— PostgreSQL 在 UTF-8 编码下会拒收它,MarkFailed 的整条 UPDATE
// 连带失败:attempts 涨不上去、状态停在 processing,任务只能被反复租约回收重投,永远
// 到不了死信。错误串恰恰常以中文开头(如投递失败的 fmt.Errorf),边界落在字符中间是
// 现实组合而非理论。ToValidUTF8 把尾部残缺的半个字符替换为空,保留前面完整内容。
func fanoutTruncateErr(s string) string {
	if len(s) <= fanoutMaxLastErrorLen {
		return s
	}
	return strings.ToValidUTF8(s[:fanoutMaxLastErrorLen], "")
}

// outbox 任务类型。数值与 010 迁移一致,改动必须两边同步。
// 发布投递与降级重放可以同时存在(UNIQUE 是 (video_id, task_type),两者不互相压制)。
const (
	fanoutTaskTypePublish = 0 // 发布投递
	fanoutTaskTypeReplay  = 1 // 降级重放:大V 掉出阈值后把缓存里的存量视频补推给粉丝
)

// FanoutRepository outbox 的读写。
type FanoutRepository interface {
	// EnqueueReplay 入队一条降级重放任务(task_type = 1)。
	// 发布投递(task_type = 0)的 outbox 行由 video 包的发布事务直接写,
	// 不经本接口 —— 这里只剩重放这一条入队路径。两种 task_type 靠
	// UNIQUE(video_id, task_type) 在库层面并存,互不压制。
	//
	// 与发布投递的幂等语义不同:同一个视频的重放**允许重来**。已有行处于
	// 待处理 / 处理中时空操作,处于已完结 / 死信时重置回待处理。理由见实现处的注释。
	EnqueueReplay(ctx context.Context, videoID, authorID int64, publishedAt time.Time) error
	ClaimPending(ctx context.Context, limit int) ([]*FanoutTask, error)
	MarkDone(ctx context.Context, taskID int64) error
	MarkFailed(ctx context.Context, taskID int64, errMsg string) error
}

// FollowerLister 取某个作者的粉丝,游标分页。
// 由 follow 包实现、main.go 注入 —— feed 不直接依赖 follow 包。
type FollowerLister interface {
	ListFollowerIDs(ctx context.Context, followeeID, afterID int64, limit int) ([]int64, error)
}

// fanoutRepository 基于 GORM 的实现(包内私有,外部只经接口访问)。
type fanoutRepository struct {
	db *gorm.DB
}

// 编译期断言:确保 fanoutRepository 实现 FanoutRepository 接口
var _ FanoutRepository = (*fanoutRepository)(nil)

func NewFanoutRepository(db *gorm.DB) FanoutRepository {
	return &fanoutRepository{db: db}
}

// EnqueueReplay 入队一条降级重放任务(task_type = 1)。
//
// 不同于发布投递,重放允许「重来」:大V 掉出阈值后又涨回、再掉出时,期间新关注的
// 粉丝仍然需要这批视频 —— 他们关注时补拉对大V 是直接跳过的(见 BackfillOnFollow)。
// 若还是 DO NOTHING,第二次降级会被第一次留下的行永久挡掉,那批粉丝再也拿不到。
//
// 所以这里是有条件的 upsert,按已有行的 status 分三种处理:
//   - 行不存在         → 插入
//   - status 为 0 或 3 → 原样不动(已在队列里 / 正被某个 worker 投递,重复入队只会白做功)
//   - status 为 1 或 2 → 重置回待处理(上一轮跑完了 / 已进死信,都该再跑一次)
//
// 用 ON CONFLICT 而不是「先查后写」:两条独立语句之间有 TOCTOU,并发的两次降级
// 可能都读到「已完成」然后都去重置,同一视频被重复投递。
func (r *fanoutRepository) EnqueueReplay(ctx context.Context, videoID, authorID int64, publishedAt time.Time) error {
	return r.db.WithContext(ctx).
		Table("fanout_tasks").
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "video_id"}, {Name: "task_type"}},
			DoUpdates: clause.Assignments(map[string]any{
				"status":          fanoutStatusPending,
				"attempts":        0,
				"last_error":      nil,
				"next_attempt_at": gorm.Expr("now()"),
				"updated_at":      gorm.Expr("now()"),
			}),
			// DO UPDATE 的 WHERE:条件不成立时该行不改,等价于跳过。
			// 只重置已完结 / 死信,待处理与处理中原样保留。
			Where: clause.Where{Exprs: []clause.Expression{
				clause.Expr{
					SQL:  "fanout_tasks.status IN (?, ?)",
					Vars: []any{fanoutStatusDone, fanoutStatusDead},
				},
			}},
		}).
		Create(map[string]any{
			"video_id":     videoID,
			"author_id":    authorID,
			"published_at": publishedAt,
			"task_type":    fanoutTaskTypeReplay,
		}).Error
}

// ClaimPending 领一批待处理任务,并把它们推进到「处理中」。
//
// 领取与状态变更必须在同一个事务里完成 —— 这是「领取即租约」的关键:
// FOR UPDATE SKIP LOCKED 的锁会随事务提交而释放,若提交时行还停在 status = 0,
// 别的 worker 实例会立刻把它再领一遍,造成重复投递、attempts 重复自增。
// 提交前把它改成 status = 3,锁和状态一起生效,后到的实例 SKIP LOCKED 直接跳过。
//
// 排序按 next_attempt_at(而非 id):最早可领的先出队。长期失败的行退避后会排到队尾,
// 不会像「按 id 升序」那样把后面所有任务堵死(队头饥饿)。
//
// 同时回收卡住的行:worker 崩在投递中途会留下 status = 3 且 updated_at 不再更新的行,
// 超过 fanoutLease 仍未续期即视为原 worker 已死,重新纳入领取,否则它会永远卡住。
func (r *fanoutRepository) ClaimPending(ctx context.Context, limit int) ([]*FanoutTask, error) {
	var tasks []*FanoutTask
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Table("fanout_tasks").
			Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where(
				"(status = ? AND next_attempt_at <= now()) OR (status = ? AND updated_at < now() - (? * interval '1 second'))",
				fanoutStatusPending, fanoutStatusProcessing, int(fanoutLease.Seconds()),
			).
			Order("next_attempt_at ASC").
			Order("id ASC").
			Limit(limit).
			Find(&tasks).Error
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			return nil
		}
		// 锁住的行改成处理中,updated_at 续上租约起点,提交后别人就领不走了
		ids := make([]int64, len(tasks))
		for i, t := range tasks {
			ids[i] = t.ID
		}
		return tx.Table("fanout_tasks").
			Where("id IN ?", ids).
			Updates(map[string]any{
				"status":     fanoutStatusProcessing,
				"updated_at": gorm.Expr("now()"),
			}).Error
	})
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// MarkDone 标记任务完成,终态,不再被领取。
//
// 只允许从 processing 转 done —— worker 必须「持有」任务才能标记完成。租约过期被
// 别的实例回收、并已由它标完成的任务,这里匹配不到行,更新 0 行即无操作(benign),
// 绝不会把别人的状态覆盖掉。
func (r *fanoutRepository) MarkDone(ctx context.Context, taskID int64) error {
	return r.db.WithContext(ctx).
		Table("fanout_tasks").
		Where("id = ? AND status = ?", taskID, fanoutStatusProcessing).
		Updates(map[string]any{
			"status":     fanoutStatusDone,
			"updated_at": gorm.Expr("now()"),
		}).Error
}

// MarkFailed 记录一次失败并推进状态机:
//   - attempts 自增;
//   - next_attempt_at 推到 now() + 指数退避,退避期内不会被再领走;
//   - 未达 fanoutMaxAttempts 退回待处理重试,达到则转死信(不再领取),
//     否则永久失败的任务会每 200ms 被领一次,空转打库、刷屏日志;
//   - last_error 截断入库,防止超长错误撑大行。
//
// 退避与状态判定在 Go 里算(见 fanoutNextState),SQL 只负责加时间。
//
// 只处理「本 worker 持有」的任务:状态必须是 processing。一个租约过期后才醒来的
// worker,其任务可能已被别人回收并标成 done/dead —— 这里查不到行(或更新 0 行),
// 直接当无操作,绝不把已完结的任务翻回 pending。查行时加行锁,避免与并发的
// MarkFailed 互相盖写。
func (r *fanoutRepository) MarkFailed(ctx context.Context, taskID int64, errMsg string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 用切片 Find 而不是单行 Scan:靠长度判断「有没有查到持在手里的行」,
		// 不依赖 RowsAffected 的填充语义。加 FOR UPDATE 锁住该行,避免并发盖写。
		var rows []struct{ Attempts int }
		if err := tx.Table("fanout_tasks").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("attempts").
			Where("id = ? AND status = ?", taskID, fanoutStatusProcessing).
			Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			// 任务已不归本 worker 持有(被回收后已完成/已死信),无操作
			return nil
		}

		attempts := rows[0].Attempts + 1
		status, backoff := fanoutNextState(attempts)

		return tx.Table("fanout_tasks").
			Where("id = ? AND status = ?", taskID, fanoutStatusProcessing).
			Updates(map[string]any{
				"attempts": attempts,
				"status":   status,
				// ? * interval '1 second':退避秒数由 Go 算好传参,SQL 只做时间加法
				"next_attempt_at": gorm.Expr("now() + (? * interval '1 second')", int(backoff.Seconds())),
				"last_error":      fanoutTruncateErr(errMsg),
				"updated_at":      gorm.Expr("now()"),
			}).Error
	})
}

const (
	fanoutBatchSize        = 100                    // 每轮领取的任务数
	fanoutFollowerPageSize = 500                    // 粉丝游标分页每页大小
	fanoutBackfillLimit    = 10                     // 新关注时补拉的历史视频条数
	fanoutIdleDelay        = 200 * time.Millisecond // 队列空时的轮询间隔

	// bigVInboxCap 大V 缓存的条数硬顶。数值与收件箱同为 50,但语义独立,
	// 单独声明,避免日后调整其中一条路径时误连带改到另一条。
	bigVInboxCap = 50
)

// FanoutWorker 消费 outbox、把已发布视频投进粉丝收件箱(或大V 缓存)。
type FanoutWorker struct {
	repo      FanoutRepository
	feeds     FeedRepository
	followers FollowerLister
	bigvs     BigVChecker
	rdb       *redis.Client
}

// NewFanoutWorker 构造 worker。
//
// feeds(FeedRepository)单独注入而不并进 FanoutRepository:补拉要的是「查某作者最近视频」,
// 这是 feed 的读契约、不是 outbox 的职责。从构造器注入,接线点明确,也免得 worker
// 反向依赖别的仓储。
func NewFanoutWorker(repo FanoutRepository, feeds FeedRepository, followers FollowerLister, bigvs BigVChecker, rdb *redis.Client) *FanoutWorker {
	return &FanoutWorker{repo: repo, feeds: feeds, followers: followers, bigvs: bigvs, rdb: rdb}
}

// Run 持续消费 outbox,直到 ctx 取消。
func (w *FanoutWorker) Run(ctx context.Context) {
	// 兜住整轮消费循环的 panic。deliverSafely 只管单条任务(DeliverOne),
	// 而循环体自身(ClaimPending / MarkDone / MarkFailed)没有保护 —— 这层一旦 panic,
	// 会顺着 main 起的裸 goroutine 冒到顶端,把整个进程带走(Run 不在 HTTP 请求里,
	// 没有 gin.Recovery 兜着)。
	//
	// 兜住后只记日志并退出循环:进程保住,代价是 fan-out 停摆,需运维从日志发现后重启。
	// 这里刻意不自动重启:引发 panic 的条件若仍在,立即重启会变成崩溃循环,
	// 反而把问题从「一次可见的停摆」变成「持续刷屏 + 反复无效重启」。
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "fan-out worker 消费循环 panic,已停止消费", "panic", r)
		}
	}()

	// 没有 Redis 一条都投不出去:claim 出来也只能失败重试。直接退出,
	// 不要空转到 DB 上反复领取 —— 那只会刷屏日志、空耗连接。
	if w.rdb == nil {
		slog.ErrorContext(ctx, "fan-out worker 缺少 Redis 连接,不启动消费循环")
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tasks, err := w.repo.ClaimPending(ctx, fanoutBatchSize)
		if err != nil {
			slog.ErrorContext(ctx, "领取 fan-out 任务失败", "err", err)
			// 领取失败也退避一下,避免库故障时忙等打爆数据库
			if !sleepOrDone(ctx, fanoutIdleDelay) {
				return
			}
			continue
		}
		if len(tasks) == 0 {
			if !sleepOrDone(ctx, fanoutIdleDelay) {
				return
			}
			continue
		}

		for _, t := range tasks {
			if err := w.deliverSafely(ctx, t); err != nil {
				// 单个任务失败(含 panic)不中断整批:其余任务照投。失败的那条由 MarkFailed
				// 退回待处理并进入退避(或达上限转死信),下一轮按 next_attempt_at 再领
				slog.ErrorContext(ctx, "投递 fan-out 失败", "task_id", t.ID, "video_id", t.VideoID, "attempts", t.Attempts, "err", err)
				if mErr := w.repo.MarkFailed(ctx, t.ID, err.Error()); mErr != nil {
					slog.ErrorContext(ctx, "标记 fan-out 失败状态出错", "task_id", t.ID, "err", mErr)
				}
				continue
			}
			if err := w.repo.MarkDone(ctx, t.ID); err != nil {
				slog.ErrorContext(ctx, "标记 fan-out 完成失败", "task_id", t.ID, "err", err)
			}
		}
	}
}

// deliverSafely 在 recover 保护下投递单条任务。DeliverOne 里的任何 panic(脏缓存成员、
// 第三方库的意外 panic)都不该把整个进程带走:兜住、记日志、当作「本次投递失败」返回,
// 由调用方走 MarkFailed 退避重试,一个坏任务只降级不致命。绝不静默吞掉 —— panic 值进日志。
func (w *FanoutWorker) deliverSafely(ctx context.Context, t *FanoutTask) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "投递 fan-out 任务 panic,按失败处理",
				"task_id", t.ID, "video_id", t.VideoID, "attempts", t.Attempts, "panic", r)
			err = fmt.Errorf("投递任务 %d panic: %v", t.ID, r)
		}
	}()
	return w.DeliverOne(ctx, t)
}

// sleepOrDone 睡 d 或 ctx 取消,返回 false 表示该退出。
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// DeliverOne 投递一条任务:大V 只写其全局缓存(读时拉),普通作者逐个粉丝推。
func (w *FanoutWorker) DeliverOne(ctx context.Context, t *FanoutTask) error {
	if w.rdb == nil {
		// 没有 Redis 就一条都投不出去。必须返回错误让任务重试 ——
		// 若默默标成完成,这批 fan-out 就永久丢了
		return errors.New("fan-out worker 缺少 Redis 连接")
	}

	// 大V 判定失败只降级成「按普通作者推」:判定器是加速层不是真相来源,不能挡投递
	isBigV := false
	if w.bigvs != nil {
		m, err := w.bigvs.Contains(ctx, []int64{t.AuthorID})
		if err != nil {
			slog.ErrorContext(ctx, "fan-out 判定大V失败,本次按普通作者投递", "author_id", t.AuthorID, "err", err)
		} else {
			isBigV = m[t.AuthorID]
		}
	}

	// score 取 outbox 的 published_at,但两种 task_type 的来源不同,不能一概而论:
	//   - 发布投递(task_type=0):published_at 由发布事务写成「发布时刻」≈ 当下,
	//     必定落在 7 天窗口内;
	//   - 降级重放(task_type=1):published_at 取自大V缓存里原视频的历史 score,
	//     可以早于窗口 —— 这类条目会在 push 的脚本里按 7 天窗口当场裁掉。
	//
	// 后果必须讲明:窗口外的重放条目会被静默裁掉,因此「大V 降级重放」只能找回
	// 仍在 7 天窗口内的存量视频,更早的历史内容无法通过重放补回。
	//
	// 绝不能用 videos.created_at(上传时刻)当 score:长期草稿的 created_at 早已越界,
	// 会在同一轮里被当场裁掉。读路径的游标也直接取自这个 score,两边同源,翻页才不漏不重。
	score := t.PublishedAt.UnixMicro()

	if isBigV {
		// 大V 缓存:条数上限同为 50,但 TTL 更长(30d),别把收件箱的 TTL 传进来
		return w.push(ctx, bigVKey(t.AuthorID), score, t.VideoID, bigVEntry, bigVInboxCap, bigVTTL)
	}

	// 普通作者:按游标一页页翻完粉丝,逐个投进收件箱。
	//
	// 先把脚本 Load 一次:下面每页用 pipeline 发 EVALSHA,而 pipeline 内不做 NOSCRIPT
	// 回退(见 pushBatch),不预加载就会整批 NOSCRIPT。一次加载覆盖整条遍历。
	if err := inboxPushScript.Load(ctx, w.rdb).Err(); err != nil {
		return err
	}

	afterID := int64(0)
	for {
		followers, err := w.followers.ListFollowerIDs(ctx, t.AuthorID, afterID, fanoutFollowerPageSize)
		if err != nil {
			return err
		}
		if len(followers) == 0 {
			return nil
		}
		// 一页粉丝合并成一个 pipeline:推模式上限 5 万粉,逐条是数万次串行往返,
		// 才是真正的瓶颈;合并后往返次数量级下降(见 pushBatch)
		keys := make([]string, len(followers))
		for i, fid := range followers {
			keys[i] = inboxKey(fid)
		}
		if err := w.pushBatch(ctx, keys, score, t.VideoID, inboxEntry, inboxCap, inboxTTL); err != nil {
			return err
		}
		// ListFollowerIDs 按 follower_id 升序返回,取最后一个当下一页游标
		afterID = followers[len(followers)-1]
		// 不足一页说明翻完了;满页则继续下一游标
		if len(followers) < fanoutFollowerPageSize {
			return nil
		}
	}
}

// push 走 inboxPushScript 往一个 ZSET 推一条并淘汰。
// 收件箱与大V 缓存的「条目窗口/条数上限/TTL」各不相同,由调用方按路径传入。
func (w *FanoutWorker) push(ctx context.Context, key string, scoreMicros, videoID int64, window time.Duration, maxEntries int, ttl time.Duration) error {
	cutoff := time.Now().Add(-window).UnixMicro()
	// 脚本 ARGV 顺序:1=score 2=video_id 3=窗口下界 4=条数上限 5=TTL 秒(见 service.go)
	return inboxPushScript.Run(ctx, w.rdb, []string{key},
		scoreMicros, videoID, cutoff, maxEntries, int64(ttl.Seconds())).Err()
}

// pushBatch 用一个 pipeline 把同一条视频推进一批收件箱,把 N 次网络往返压成 1 次。
//
// 逐条 push 才是粉丝投递真正的瓶颈:推模式的上限是 5 万粉(BigVThreshold,再多就走
// 拉模式),逐条推就是数万次串行往返;论 Redis 吞吐它毫无压力,贵的是「每条一个 RTT」。
// 一页(500)合并成一次 pipeline 后,往返次数按页数下降。
//
// 调用前必须已 Load 过脚本(见 DeliverOne):Script.Run 的 NOSCRIPT 回退依据是「执行后
// 立即可读的 Err()」,而 pipeline 里的命令要到 Exec 才发送,排队时 Err() 恒为 nil,于是
// 只会排队 EVALSHA;服务端若没缓存该脚本,整批在 Exec 时直接 NOSCRIPT。预加载一次即可
// 覆盖整批。
//
// Exec 会执行全部命令并返回首个命令错误:部分成功也无需回滚 —— ZADD 幂等,任务重投
// 只是把同一批再推一遍,结果不变。所以这里直接把错误交给调用方走失败重试路径。
func (w *FanoutWorker) pushBatch(ctx context.Context, keys []string, scoreMicros, videoID int64, window time.Duration, maxEntries int, ttl time.Duration) error {
	if len(keys) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-window).UnixMicro()
	// 脚本 ARGV 顺序:1=score 2=video_id 3=窗口下界 4=条数上限 5=TTL 秒(见 service.go)
	pipe := w.rdb.Pipeline()
	for _, key := range keys {
		inboxPushScript.Run(ctx, pipe, []string{key},
			scoreMicros, videoID, cutoff, maxEntries, int64(ttl.Seconds()))
	}
	_, err := pipe.Exec(ctx)
	return err
}

// BackfillOnFollow 新关注时把对方最近若干条已发布视频补进关注者收件箱。
// 结构化满足 follow.FollowBackfiller(不在本包声明该接口,以免 feed 反向依赖 follow)。
func (w *FanoutWorker) BackfillOnFollow(ctx context.Context, followerID, followeeID int64) error {
	if w.rdb == nil {
		return errors.New("补拉历史视频需要 Redis 连接")
	}

	// 大V 走拉模式,读关注流时会现拿其缓存,补拉是多余的;
	// 而且大V 粉丝海量,补拉只会把全量缓存往每个新粉丝收件箱里塞一遍
	if w.bigvs != nil {
		m, err := w.bigvs.Contains(ctx, []int64{followeeID})
		if err != nil {
			slog.ErrorContext(ctx, "补拉时判定大V失败", "followee_id", followeeID, "err", err)
		} else if m[followeeID] {
			return nil
		}
	}

	refs, err := w.feeds.ListRecentPublishedVideoIDs(ctx, followeeID, fanoutBackfillLimit)
	if err != nil {
		return err
	}

	var firstErr error
	for _, ref := range refs {
		// published_at 在原理上可能为 NULL:虽然查询带了 status = 1(本应排除草稿),
		// 但列本身可空,历史遗留行或绕过 MarkPublished 的写入都可能留下 NULL。
		// 扫描成指针并在这里跳过,免得一条 NULL 让整次补拉失败。
		if ref.PublishedAt == nil {
			slog.WarnContext(ctx, "补拉视频缺少 published_at,跳过", "follower_id", followerID, "video_id", ref.ID)
			continue
		}
		// 用视频自己的 published_at 当 score,而不是 now():补拉的历史要落回它真实的发布位置,
		// 否则会被当成「刚发布」挤到关注流最前,还可能越过用户正在使用的分页游标。
		// 也不能用 created_at —— 那是上传时刻,长期草稿会带着旧时刻补进来被 7 天窗口裁掉
		score := ref.PublishedAt.UnixMicro()
		if err := w.push(ctx, inboxKey(followerID), score, ref.ID, inboxEntry, inboxCap, inboxTTL); err != nil {
			// 单条失败不中断其余,记下来继续;最后把首个错误返回,让调用方感知是部分失败
			slog.ErrorContext(ctx, "补拉单条视频失败", "follower_id", followerID, "video_id", ref.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}
	return firstErr
}

// PromoteBigVCache 作者刚升入大V 时,把它最近的已发布视频回填进它自己的全局缓存
// feed:bigv:<authorID>。
//
// 结构化满足 follow.BigVPromoter(不在本包声明该接口,以免 feed 反向依赖 follow)。
//
// 缓存此前只在作者「发布」时经 DeliverOne 写入,升入大V 时它是空的;而新粉丝关注大V
// 走拉模式、补拉被跳过(见 BackfillOnFollow),他们在作者发下一条视频之前一条都看不到。
// 这里补上这段空窗,与降级方向的 ReplayBigVCache 对称:一个在升入时填缓存,
// 一个在跌出时把缓存里的存量推给粉丝。
//
// 代价可控(读一个作者的近期视频、写一个 key,至多 bigVInboxCap 条),故由调用方同步执行,不走 outbox。
func (w *FanoutWorker) PromoteBigVCache(ctx context.Context, authorID int64) error {
	if w.rdb == nil {
		return errors.New("回填大V缓存需要 Redis 连接")
	}

	// 填到缓存自己的上限(bigVInboxCap),不是收件箱补拉的 fanoutBackfillLimit —— 那是另一个旋钮
	refs, err := w.feeds.ListRecentPublishedVideoIDs(ctx, authorID, bigVInboxCap)
	if err != nil {
		return err
	}

	// 全部写同一个 key,是 pushBatch(一条视频写多个收件箱)的转置 —— 不能照搬它,
	// 这里改成「多条视频对一个 key」:仍合成一个 pipeline,把 N 次往返压成 1 次。
	pipe := w.rdb.Pipeline()
	n := 0
	for _, ref := range refs {
		// published_at 列可空:跳过 NULL,免得一条坏行打挂整次回填(与 BackfillOnFollow 同口径)
		if ref.PublishedAt == nil {
			slog.WarnContext(ctx, "回填大V缓存时视频缺少 published_at,跳过", "author_id", authorID, "video_id", ref.ID)
			continue
		}
		// score 取视频自己的 published_at 微秒,绝不能用 now():读路径的游标与缓存内排序
		// 都依赖它,re-stamp 成「现在」会让历史视频挤到最前、还会越过用户正在用的分页游标。
		// 窗口/上限/TTL 与 DeliverOne 的大V 分支逐一对齐:bigVEntry / bigVInboxCap / bigVTTL,
		// 传错会静默改变缓存的保留期或可见范围。
		cutoff := time.Now().Add(-bigVEntry).UnixMicro()
		inboxPushScript.Run(ctx, pipe, []string{bigVKey(authorID)},
			ref.PublishedAt.UnixMicro(), ref.ID, cutoff, bigVInboxCap, int64(bigVTTL.Seconds()))
		n++
	}
	if n == 0 {
		// 无视频(或全是 NULL)不是错误,也没有可执行的命令
		return nil
	}

	// 预加载脚本:下面用 pipeline 发 EVALSHA,而 pipeline 内不做 NOSCRIPT 回退(见 pushBatch),
	// 不预加载整批都会 NOSCRIPT
	if err := inboxPushScript.Load(ctx, w.rdb).Err(); err != nil {
		return err
	}

	// Exec 会执行全部命令并返回首个命令错误:部分成功无需回滚(ZADD 幂等,重投结果不变),
	// 部分失败也把首个错误返回给调用方,与 BackfillOnFollow 的错误风格一致
	_, err = pipe.Exec(ctx)
	return err
}

// ReplayBigVCache 大V 降级时的重放:读出 feed:bigv:<authorID> 缓存里的存量视频,
// 逐个入队成降级重放任务(task_type = 1),由 worker 按普通作者推给每个粉丝。
//
// 结构化满足 follow.BigVDowngradeReplayer(不在本包声明该接口,以免 feed 反向依赖 follow)。
//
// 缓存里的视频发布时只进了大V缓存、没进过任何收件箱;作者一旦不再是大大V,
// 读路径就不再读那个缓存,不重放它们就永久不可见。
func (w *FanoutWorker) ReplayBigVCache(ctx context.Context, authorID int64) error {
	if w.rdb == nil {
		// 没有 Redis 拿不到缓存,也就无从重放。返回错误让调用方感知,别假装成功
		return errors.New("重放大V缓存需要 Redis 连接")
	}

	// 带 score 读出:score 就是原发布时刻,重放时要原样保留
	entries, err := w.rdb.ZRevRangeWithScores(ctx, bigVKey(authorID), 0, -1).Result()
	if err != nil {
		return err
	}
	for _, e := range entries {
		member, ok := e.Member.(string)
		if !ok {
			slog.ErrorContext(ctx, "大V缓存成员类型异常,跳过", "author_id", authorID, "member", e.Member)
			continue
		}
		videoID, err := strconv.ParseInt(member, 10, 64)
		if err != nil {
			// 缓存里理论上只有视频 ID 字符串;出现异常成员只跳过这一条,不因它中断整次重放
			slog.ErrorContext(ctx, "大V缓存成员不是合法视频ID,跳过", "author_id", authorID, "member", member, "err", err)
			continue
		}
		// 用缓存里已有的 score 当 published_at,绝不能用 time.Now():
		// 重放的视频必须保持它原来的发布位置,re-stamp 成「现在」会让旧内容
		// 跳到每个粉丝关注流的最前面,也会越过用户正在使用的分页游标。
		publishedAt := time.UnixMicro(int64(e.Score))
		if err := w.repo.EnqueueReplay(ctx, videoID, authorID, publishedAt); err != nil {
			return err
		}
	}
	return nil
}
