package feed

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"feed-system/internal/model/video"
)

// ---------- 测试替身 ----------

// fakeFanoutTask 内存 outbox 的一行,字段对应 fanout_tasks 表。
type fakeFanoutTask struct {
	id            int64
	videoID       int64
	authorID      int64
	publishedAt   time.Time
	taskType      int
	attempts      int
	status        int
	nextAttemptAt time.Time
	lastError     string
	updatedAt     time.Time
}

// fakeFanoutRepo FanoutRepository 的内存实现,按 fanout.go 的状态机运作:
// 领取把行推进 status = 3(处理中)、失败写退避、达上限转死信。
//
// 诚实说明:状态机的「判定」已抽成纯函数 fanoutNextState / fanoutBackoff,由真实仓储
// 和本 fake 共用 —— 所以改动判定(例如死信判定)会被下面的状态机测试抓到,这是伪造
// 不出来的。
//
// 但它仍未覆盖真实实现里的 SQL 管道:领取的 WHERE 条件、FOR UPDATE SKIP LOCKED、
// 租约回收、MarkDone/MarkFailed 的 status 守卫、入队的 ON CONFLICT —— 这些都要靠
// 真 PostgreSQL 集成测试,内存桩给不出并发与 SQL 语义。不要以为这套测试证明了那些。
//
// clock 可注入,方便在测试里把时间推过退避窗口。
type fakeFanoutRepo struct {
	tasks []*fakeFanoutTask
	clock func() time.Time
}

func newFakeFanoutRepo(tasks ...*fakeFanoutTask) *fakeFanoutRepo {
	return &fakeFanoutRepo{tasks: tasks, clock: time.Now}
}

func (r *fakeFanoutRepo) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func (r *fakeFanoutRepo) find(id int64) *fakeFanoutTask {
	for _, t := range r.tasks {
		if t.id == id {
			return t
		}
	}
	return nil
}

func (r *fakeFanoutRepo) EnqueueReplay(_ context.Context, videoID, authorID int64, publishedAt time.Time) error {
	return r.enqueueReplay(videoID, authorID, publishedAt)
}

// enqueueReplay 复刻真实仓储 EnqueueReplay 的有条件 upsert 语义:
//   - 行不存在             → 插入
//   - status 待处理 / 处理中 → 原样不动(在途,重复入队只会白做功)
//   - status 已完结 / 死信   → 重置回待处理、attempts 归零
//
// 注意这与发布投递的 DO NOTHING 不是一回事:发布投递由 video 包的发布事务直接写,
// 不在本 fake 职责内。两者共用 (video_id, task_type) 唯一键,但幂等规则不同。
func (r *fakeFanoutRepo) enqueueReplay(videoID, authorID int64, publishedAt time.Time) error {
	for _, t := range r.tasks {
		if t.videoID != videoID || t.taskType != fanoutTaskTypeReplay {
			continue
		}
		switch t.status {
		case fanoutStatusDone, fanoutStatusDead:
			now := r.now()
			t.status = fanoutStatusPending
			t.attempts = 0
			t.lastError = ""
			t.nextAttemptAt = now
			t.updatedAt = now
		}
		return nil
	}

	now := r.now()
	r.tasks = append(r.tasks, &fakeFanoutTask{
		id: videoID, videoID: videoID, authorID: authorID, publishedAt: publishedAt,
		taskType: fanoutTaskTypeReplay, status: fanoutStatusPending, nextAttemptAt: now, updatedAt: now,
	})
	return nil
}

// ClaimPending 复刻真实实现的领取语义:按 (next_attempt_at, id) 排序,
// 只取可领的行(待处理且到点,或处理中且租约已过期),并把它们改成处理中。
func (r *fakeFanoutRepo) ClaimPending(_ context.Context, limit int) ([]*FanoutTask, error) {
	now := r.now()
	leaseCutoff := now.Add(-fanoutLease)

	var claimable []*fakeFanoutTask
	for _, t := range r.tasks {
		pendingReady := t.status == fanoutStatusPending && !t.nextAttemptAt.After(now)
		leaseExpired := t.status == fanoutStatusProcessing && t.updatedAt.Before(leaseCutoff)
		if pendingReady || leaseExpired {
			claimable = append(claimable, t)
		}
	}
	sort.Slice(claimable, func(i, j int) bool {
		if !claimable[i].nextAttemptAt.Equal(claimable[j].nextAttemptAt) {
			return claimable[i].nextAttemptAt.Before(claimable[j].nextAttemptAt)
		}
		return claimable[i].id < claimable[j].id
	})
	if limit >= 0 && len(claimable) > limit {
		claimable = claimable[:limit]
	}

	out := make([]*FanoutTask, 0, len(claimable))
	for _, t := range claimable {
		// 领取即租约:移出待处理,后到的同刻领取就拿不到它
		t.status = fanoutStatusProcessing
		t.updatedAt = now
		out = append(out, &FanoutTask{
			ID: t.id, VideoID: t.videoID, AuthorID: t.authorID,
			PublishedAt: t.publishedAt, Attempts: t.attempts,
		})
	}
	return out, nil
}

// MarkDone 复刻真实实现的 status 守卫:只允许从 processing 转 done,
// 任务已不归本 worker 持有时无操作。
func (r *fakeFanoutRepo) MarkDone(_ context.Context, taskID int64) error {
	if t := r.find(taskID); t != nil && t.status == fanoutStatusProcessing {
		t.status = fanoutStatusDone
		t.updatedAt = r.now()
	}
	return nil
}

// MarkFailed 复刻真实实现:先做 status 守卫(只处理 processing),再调用共用的
// fanoutNextState 得到新状态与退避 —— 判定与真实仓储同源,改真实实现就会改到这里。
func (r *fakeFanoutRepo) MarkFailed(_ context.Context, taskID int64, errMsg string) error {
	t := r.find(taskID)
	if t == nil {
		return nil
	}
	// 不持有该任务(已被回收/已完成/已死信):无操作,绝不覆盖别人的状态
	if t.status != fanoutStatusProcessing {
		return nil
	}
	now := r.now()
	t.attempts++
	status, backoff := fanoutNextState(t.attempts)
	t.status = int(status)
	t.lastError = fanoutTruncateErr(errMsg)
	t.updatedAt = now
	t.nextAttemptAt = now.Add(backoff)
	return nil
}

// claimedIDs 取一批任务里的 ID,断言用。
func claimedIDs(tasks []*FanoutTask) []int64 {
	ids := make([]int64, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return ids
}

// fakeFollowers 粉丝列表的内存实现。页面由 fn 按 afterID 决定
type fakeFollowers struct {
	fn    func(followeeID, afterID int64) ([]int64, error)
	calls []int64
}

func (f *fakeFollowers) ListFollowerIDs(_ context.Context, followeeID, afterID int64, _ int) ([]int64, error) {
	f.calls = append(f.calls, afterID)
	return f.fn(followeeID, afterID)
}

// constantFollowers 永远返回同一页粉丝
func constantFollowers(ids ...int64) *fakeFollowers {
	return &fakeFollowers{fn: func(int64, int64) ([]int64, error) { return ids, nil }}
}

// failingFollowers 被调用即报错,用来证明某条路径没有去翻粉丝
func failingFollowers() *fakeFollowers {
	return &fakeFollowers{fn: func(int64, int64) ([]int64, error) {
		return nil, errors.New("不该翻粉丝")
	}}
}

// panicFollowers 一被调用就 panic,用来验证 worker 的 recover 保护:
// 投递路径里的意外 panic 不能把整个进程带走。
type panicFollowers struct{}

func (panicFollowers) ListFollowerIDs(context.Context, int64, int64, int) ([]int64, error) {
	panic("投递时意外 panic")
}

// panicClaimRepo 领取任务时 panic,用来验证 Run 兜住「消费循环自身」的 panic。
// 与 panicFollowers 分工不同:那个的 panic 发生在 DeliverOne 内部(由 deliverSafely 兜),
// 这个发生在循环体里(没有被那层覆盖)。
type panicClaimRepo struct{ *fakeFanoutRepo }

func (panicClaimRepo) ClaimPending(context.Context, int) ([]*FanoutTask, error) {
	panic("领取任务时意外 panic")
}

// signalingRepo 在 MarkFailed 被调用时往带缓冲 channel 发一次任务 ID,
// 让测试能在不读 fake 内部状态(避免数据竞争)的前提下,观察到「panic 的任务
// 确实被走失败路径处理了」。
type signalingRepo struct {
	*fakeFanoutRepo
	failed chan int64
}

func (r *signalingRepo) MarkFailed(ctx context.Context, taskID int64, errMsg string) error {
	err := r.fakeFanoutRepo.MarkFailed(ctx, taskID, errMsg)
	select {
	case r.failed <- taskID:
	default:
	}
	return err
}

// ---------- 辅助 ----------

// newFanoutWorker 起一个带 miniredis 的 worker
func newFanoutWorker(t *testing.T, feeds FeedRepository, followers FollowerLister, bigvs BigVChecker) (*FanoutWorker, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewFanoutWorker(newFakeFanoutRepo(), feeds, followers, bigvs, rdb), mr, rdb
}

// zsetEntries 取一个 ZSET 的全部 (member, score)
func zsetEntries(t *testing.T, rdb *redis.Client, key string) []redis.Z {
	t.Helper()
	zs, err := rdb.ZRangeWithScores(context.Background(), key, 0, -1).Result()
	require.NoError(t, err)
	return zs
}

// keysWithPrefix 列出 miniredis 里以某前缀开头的 key
func keysWithPrefix(mr *miniredis.Miniredis, prefix string) []string {
	var out []string
	for _, k := range mr.Keys() {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func memberString(videoID int64) string { return strconv.FormatInt(videoID, 10) }

// ---------- 测试 ----------

// TestDeliverOne_小V走粉丝收件箱 普通作者逐个粉丝推,不写大V 缓存
func TestDeliverOne_小V走粉丝收件箱(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7
	task := &FanoutTask{ID: 1, VideoID: 100, AuthorID: author, PublishedAt: time.Now().Add(-time.Minute)}

	w, mr, rdb := newFanoutWorker(t, nil, constantFollowers(11, 12, 13), fakeBigV{big: map[int64]bool{author: false}})

	require.NoError(t, w.DeliverOne(ctx, task))

	for _, fid := range []int64{11, 12, 13} {
		entries := zsetEntries(t, rdb, inboxKey(fid))
		require.Len(t, entries, 1, "粉丝 %d 的收件箱应有一条", fid)
		assert.Equal(t, memberString(100), entries[0].Member)
	}
	assert.Empty(t, keysWithPrefix(mr, bigVKeyPrefix), "小V 不该写大V 缓存")
}

// TestDeliverOne_大V走全局缓存 大V 只写自己的缓存,一个粉丝收件箱都不碰
func TestDeliverOne_大V走全局缓存(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7
	task := &FanoutTask{ID: 1, VideoID: 100, AuthorID: author, PublishedAt: time.Now().Add(-time.Minute)}

	// followers 一律报错:大V 路径根本不该去翻粉丝
	w, mr, rdb := newFanoutWorker(t, nil, failingFollowers(), fakeBigV{big: map[int64]bool{author: true}})

	require.NoError(t, w.DeliverOne(ctx, task))

	entries := zsetEntries(t, rdb, bigVKey(author))
	require.Len(t, entries, 1, "大V 缓存应有一条")
	assert.Equal(t, memberString(100), entries[0].Member)
	assert.Empty(t, keysWithPrefix(mr, inboxKeyPrefix), "大V 不该写任何粉丝收件箱")
}

// TestDeliverOne_score用发布微秒 锁住 score == published_at 的微秒值。
//
// 用带微秒、非整秒的时间戳:截断到秒或毫秒的实现都会差出余数而失败。
// 注意不能拿 1700000000 那种远古时间 —— 收件箱有 7 天窗口,
// 越界条目会被脚本当场裁掉,断言会因「条目不存在」而非「score 不对」失败
func TestDeliverOne_score用发布微秒(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7
	const fid int64 = 11

	base := time.Now().Add(-time.Minute).Truncate(time.Second)
	publishedAt := time.Unix(base.Unix(), 123456000) // 带 123456 微秒
	task := &FanoutTask{ID: 1, VideoID: 100, AuthorID: author, PublishedAt: publishedAt}

	w, _, rdb := newFanoutWorker(t, nil, constantFollowers(fid), fakeBigV{big: map[int64]bool{author: false}})

	require.NoError(t, w.DeliverOne(ctx, task))

	score, err := rdb.ZScore(ctx, inboxKey(fid), memberString(100)).Result()
	require.NoError(t, err)
	assert.Equal(t, publishedAt.UnixMicro(), int64(score), "score 必须精确到 published_at 的微秒")
}

// TestDeliverOne_rdb为nil返回错误 静默成功会让 outbox 标完成、fan-out 永久丢失
func TestDeliverOne_rdb为nil返回错误(t *testing.T) {
	ctx := context.Background()
	task := &FanoutTask{ID: 1, VideoID: 100, AuthorID: 7, PublishedAt: time.Now()}

	w := NewFanoutWorker(newFakeFanoutRepo(), nil, constantFollowers(11), fakeBigV{}, nil)

	err := w.DeliverOne(ctx, task)
	require.Error(t, err, "缺 Redis 必须报错,不能静默成功")
}

// TestDeliverOne_单个粉丝失败不中断 第二页报错时,第一页的粉丝仍已收到推送
func TestDeliverOne_单个粉丝失败不中断(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7

	// 第一页必须正好满页(fanoutFollowerPageSize),否则不会翻第二页
	page1 := make([]int64, fanoutFollowerPageSize)
	for i := range page1 {
		page1[i] = int64(i + 1)
	}
	followers := &fakeFollowers{fn: func(_, afterID int64) ([]int64, error) {
		if afterID == 0 {
			return page1, nil
		}
		return nil, errors.New("第二页失败")
	}}

	task := &FanoutTask{ID: 1, VideoID: 100, AuthorID: author, PublishedAt: time.Now().Add(-time.Minute)}
	w, _, rdb := newFanoutWorker(t, nil, followers, fakeBigV{big: map[int64]bool{author: false}})

	err := w.DeliverOne(ctx, task)
	require.Error(t, err, "第二页失败应把错误返回给调用方")

	for _, fid := range page1 {
		entries := zsetEntries(t, rdb, inboxKey(fid))
		require.Len(t, entries, 1, "第一页的粉丝 %d 仍应收到推送", fid)
	}
}

// TestDeliverOne_跨页粉丝全部投达 粉丝数超过一页时,分页游标必须正确前移,每一页都投到。
// 收件箱投递已由「逐条 push」改成「每页一个 pipeline」,游标推进方式也跟着变,这条钉住它。
func TestDeliverOne_跨页粉丝全部投达(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7
	const total = fanoutFollowerPageSize + 3 // 一满页 + 一页尾巴(尾巴不满,循环该停)

	all := make([]int64, total)
	for i := range all {
		all[i] = int64(i + 1)
	}
	// 复刻游标分页:返回 afterID 之后的第一页,最多 fanoutFollowerPageSize 条
	followers := &fakeFollowers{fn: func(_, afterID int64) ([]int64, error) {
		var out []int64
		for _, id := range all {
			if id > afterID {
				out = append(out, id)
				if len(out) == fanoutFollowerPageSize {
					break
				}
			}
		}
		return out, nil
	}}

	task := &FanoutTask{ID: 1, VideoID: 100, AuthorID: author, PublishedAt: time.Now().Add(-time.Minute)}
	w, _, rdb := newFanoutWorker(t, nil, followers, fakeBigV{big: map[int64]bool{author: false}})

	require.NoError(t, w.DeliverOne(ctx, task))

	for _, fid := range all {
		entries := zsetEntries(t, rdb, inboxKey(fid))
		require.Len(t, entries, 1, "跨页投递后粉丝 %d 应恰好收到一条", fid)
	}
}

// TestDeliverOne_大V判定失败降级 判定器报错时按普通作者推,不阻断投递
func TestDeliverOne_大V判定失败降级(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7
	task := &FanoutTask{ID: 1, VideoID: 100, AuthorID: author, PublishedAt: time.Now().Add(-time.Minute)}

	w, mr, rdb := newFanoutWorker(t, nil, constantFollowers(11), fakeBigV{err: errors.New("名单查询失败")})

	require.NoError(t, w.DeliverOne(ctx, task), "判定失败不该阻断投递")

	assert.Len(t, zsetEntries(t, rdb, inboxKey(11)), 1, "降级后应写粉丝收件箱")
	assert.Empty(t, keysWithPrefix(mr, bigVKeyPrefix), "判定失败不能当大V")
}

// TestBackfillOnFollow 新关注补拉:大V 跳过,非大V 按视频自身 published_at 落位
func TestBackfillOnFollow(t *testing.T) {
	ctx := context.Background()
	const followeeID int64 = 7
	const followerID int64 = 5

	base := time.Now().Add(-time.Hour).Truncate(time.Second).Unix()
	// published_at 与 created_at 故意取不同值:必须落 published_at,用 created_at 会失败
	pub1 := time.Unix(base, 222333000)
	pub2 := time.Unix(base, 111222000)
	videos := []*video.Video{
		{ID: 101, AuthorID: followeeID, CreatedAt: time.Unix(base, 999999000), PublishedAt: &pub1, Status: video.StatusPublished},
		{ID: 102, AuthorID: followeeID, CreatedAt: time.Unix(base, 888888000), PublishedAt: &pub2, Status: video.StatusPublished},
	}

	t.Run("大V 被跳过", func(t *testing.T) {
		// feeds 传 nil:大V 分支根本不该查库
		w, mr, _ := newFanoutWorker(t, nil, nil, fakeBigV{big: map[int64]bool{followeeID: true}})
		require.NoError(t, w.BackfillOnFollow(ctx, followerID, followeeID))
		assert.Empty(t, keysWithPrefix(mr, inboxKeyPrefix), "大V 走拉模式,不该往收件箱补")
	})

	t.Run("非大V 按视频 published_at 落位", func(t *testing.T) {
		w, _, rdb := newFanoutWorker(t, &pagedFeedRepo{videos: videos}, nil, fakeBigV{big: map[int64]bool{followeeID: false}})

		require.NoError(t, w.BackfillOnFollow(ctx, followerID, followeeID))

		entries := zsetEntries(t, rdb, inboxKey(followerID))
		require.Len(t, entries, 2, "两条历史都应补进收件箱")

		got := make(map[string]float64, len(entries))
		for _, e := range entries {
			got[e.Member.(string)] = e.Score
		}
		assert.Equal(t, float64(pub1.UnixMicro()), got["101"])
		assert.Equal(t, float64(pub2.UnixMicro()), got["102"])
		assert.NotEqual(t, float64(time.Now().UnixMicro()), got["101"], "score 用的是视频自己的 published_at,不是 now")
	})
}

// TestDeliverOne_长期草稿发布后仍能进收件箱 回归测试:created_at 是「上传」时刻,
// 一个传完搁置超过 7 天才发布的草稿,若拿 created_at 当 score,会在被投递的同一轮
// 脚本里因越出 7 天窗口而当场裁掉,视频永远进不了任何粉丝的关注流。
// 修复后 score 取 outbox 的 published_at(发布时刻),必定落在窗口内。
func TestDeliverOne_长期草稿发布后仍能进收件箱(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7
	const fid int64 = 11

	publishAt := time.Now().Add(-time.Minute)            // 发布时刻:当下
	oldCreatedAt := time.Now().Add(-10 * 24 * time.Hour) // 上传时刻:10 天前

	// 正确路径:outbox 带的是发布时刻
	task := &FanoutTask{ID: 1, VideoID: 100, AuthorID: author, PublishedAt: publishAt}
	w, _, rdb := newFanoutWorker(t, nil, constantFollowers(fid), fakeBigV{big: map[int64]bool{author: false}})

	require.NoError(t, w.DeliverOne(ctx, task))

	score, err := rdb.ZScore(ctx, inboxKey(fid), memberString(100)).Result()
	require.NoError(t, err, "视频必须出现在粉丝收件箱里,不能被 7 天窗口裁掉")
	assert.Equal(t, publishAt.UnixMicro(), int64(score), "score 必须是发布时刻,而不是旧的 created_at")

	// 对照:若错误地拿 created_at(上传时刻)当 score,越出 7 天窗口的条目会被当场裁掉
	badTask := &FanoutTask{ID: 2, VideoID: 200, AuthorID: author, PublishedAt: oldCreatedAt}
	require.NoError(t, w.DeliverOne(ctx, badTask))
	_, err = rdb.ZScore(ctx, inboxKey(fid), memberString(200)).Result()
	assert.ErrorIs(t, err, redis.Nil, "越出窗口的旧时刻会被脚本裁掉 —— 这正是原 bug")
}

// TestReplayBigVCache_按缓存score入队重放任务 大V降级重放:每条缓存视频入队一条
// task_type=1 的重放任务,published_at 必须取缓存里已有的 score,而不是 now()。
func TestReplayBigVCache_按缓存score入队重放任务(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	base := time.Now().Add(-time.Hour).Truncate(time.Second).Unix()
	// score 带微秒且两条不同:用 now 或截断到秒/毫秒的实现都会差出余数而失败
	s1 := time.Unix(base, 111222000)
	s2 := time.Unix(base, 222333000)
	require.NoError(t, rdb.ZAdd(ctx, bigVKey(author),
		redis.Z{Score: float64(s1.UnixMicro()), Member: "101"},
		redis.Z{Score: float64(s2.UnixMicro()), Member: "102"},
	).Err())

	repo := newFakeFanoutRepo()
	// 重放不查库、不翻粉丝、不判大V,feeds/followers/bigvs 传空即可
	w := NewFanoutWorker(repo, nil, nil, fakeBigV{}, rdb)

	require.NoError(t, w.ReplayBigVCache(ctx, author))

	require.Len(t, repo.tasks, 2, "每条缓存视频应入队一条重放任务")
	got := map[int64]*fakeFanoutTask{}
	for _, task := range repo.tasks {
		got[task.videoID] = task
	}
	require.Contains(t, got, int64(101))
	require.Contains(t, got, int64(102))
	assert.Equal(t, fanoutTaskTypeReplay, got[101].taskType, "重放任务必须是 task_type=1")
	assert.Equal(t, fanoutTaskTypeReplay, got[102].taskType)
	assert.Equal(t, author, got[101].authorID)
	assert.True(t, got[101].publishedAt.Equal(s1), "published_at 必须等于缓存里的 score")
	assert.True(t, got[102].publishedAt.Equal(s2), "published_at 必须等于缓存里的 score")
	assert.NotEqual(t, time.Now().UnixMicro(), got[101].publishedAt.UnixMicro(),
		"绝不能用 now 当 published_at:否则旧内容会跳到粉丝关注流最前面")
}

// TestReplayBigVCache_空缓存不报错 没有存量的作者降级时不该出错,只是没任务可入队。
func TestReplayBigVCache_空缓存不报错(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := newFakeFanoutRepo()
	w := NewFanoutWorker(repo, nil, nil, fakeBigV{}, rdb)

	require.NoError(t, w.ReplayBigVCache(ctx, 7))
	assert.Empty(t, repo.tasks, "空缓存不该产生任何任务")
}

// TestReplayBigVCache_rdb为nil返回错误 没有 Redis 拿不到缓存,必须报错而非假装成功。
func TestReplayBigVCache_rdb为nil返回错误(t *testing.T) {
	w := NewFanoutWorker(newFakeFanoutRepo(), nil, nil, fakeBigV{}, nil)
	require.Error(t, w.ReplayBigVCache(context.Background(), 7))
}

// TestReplayBigVCache_重放任务走普通推送路径 证明 DeliverOne 不需要为 task_type=1
// 加分支:投递重放任务时作者已不再是大大V,Contains 返回 false,自然落到「逐个粉丝推」
// 的普通路径。这里就用一个「非大V」的判定器投一条重放任务并断言进了粉丝收件箱。
func TestReplayBigVCache_重放任务走普通推送路径(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// 缓存里有一条视频(它在作者还是大V时发布)
	score := time.Now().Add(-time.Minute).UnixMicro()
	require.NoError(t, rdb.ZAdd(ctx, bigVKey(author), redis.Z{Score: float64(score), Member: "100"}).Err())

	repo := newFakeFanoutRepo()
	// 此刻作者已被判为非大V —— 与降级后的现实一致
	w := NewFanoutWorker(repo, nil, constantFollowers(11, 12), fakeBigV{big: map[int64]bool{author: false}}, rdb)

	require.NoError(t, w.ReplayBigVCache(ctx, author))
	require.Len(t, repo.tasks, 1)

	task := &FanoutTask{ID: repo.tasks[0].id, VideoID: 100, AuthorID: author, PublishedAt: repo.tasks[0].publishedAt}
	require.NoError(t, w.DeliverOne(ctx, task))

	assert.Len(t, zsetEntries(t, rdb, inboxKey(11)), 1, "重放视频应进粉丝收件箱")
	assert.Len(t, zsetEntries(t, rdb, inboxKey(12)), 1)
}

// TestEnqueueReplay_按已有任务状态决定是否重来 锁住重放的幂等规则:
// 在途的(待处理 / 处理中)原样不动,已完结 / 死信的重新排队。
//
// 背景:若第二次降级被第一次留下的行永久挡掉,期间新关注的粉丝就永远拿不到那批视频
// —— 而他们关注时补拉对大V 是直接跳过的(见 BackfillOnFollow)。
//
// 诚实说明:这里测的是内存替身复刻的规则。真实实现是 PG 的
// `ON CONFLICT ... DO UPDATE ... WHERE`,其「DO UPDATE 的条件不成立则不改该行」
// 语义内存桩给不出,必须有真 PostgreSQL 的集成测试。别把这条当成了真库证据。
func TestEnqueueReplay_按已有任务状态决定是否重来(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7
	const video int64 = 100

	published := time.Unix(1700000000, 0)

	cases := []struct {
		name         string
		status       int
		attempts     int
		wantStatus   int
		wantAttempts int
		// wantReset 区分「重新排队了」与「本来就在待处理、原样没动」——
		// 两者的结果状态都是 pending,只有前者该清掉上一轮的错误
		wantReset bool
	}{
		{"待处理:不动,避免重复投递", fanoutStatusPending, 0, fanoutStatusPending, 0, false},
		{"处理中:不动,worker 正拿它投递", fanoutStatusProcessing, 0, fanoutStatusProcessing, 0, false},
		{"已完结:重新排队", fanoutStatusDone, 0, fanoutStatusPending, 0, true},
		{"死信:重新排队且 attempts 归零", fanoutStatusDead, fanoutMaxAttempts, fanoutStatusPending, 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := newFakeFanoutRepo(&fakeFanoutTask{
				id: 1, videoID: video, authorID: author, publishedAt: published,
				taskType: fanoutTaskTypeReplay, status: c.status, attempts: c.attempts,
				lastError: "上一轮残留", nextAttemptAt: time.Now(), updatedAt: time.Now(),
			})

			require.NoError(t, repo.EnqueueReplay(ctx, video, author, published))

			require.Len(t, repo.tasks, 1, "同 (video_id, task_type) 不该产生第二行")
			assert.Equal(t, c.wantStatus, repo.tasks[0].status)
			assert.Equal(t, c.wantAttempts, repo.tasks[0].attempts)
			if c.wantReset {
				assert.Empty(t, repo.tasks[0].lastError, "重新排队应清掉上一轮的错误")
			} else {
				assert.Equal(t, "上一轮残留", repo.tasks[0].lastError, "在途的任务不该被改动")
			}
		})
	}

	t.Run("没有已有行时插入", func(t *testing.T) {
		repo := newFakeFanoutRepo()
		require.NoError(t, repo.EnqueueReplay(ctx, video, author, published))
		require.Len(t, repo.tasks, 1)
		assert.Equal(t, fanoutStatusPending, repo.tasks[0].status)
		assert.Equal(t, fanoutTaskTypeReplay, repo.tasks[0].taskType)
	})

	t.Run("已有的发布投递行不阻挡重放", func(t *testing.T) {
		repo := newFakeFanoutRepo(&fakeFanoutTask{
			id: 1, videoID: video, authorID: author, publishedAt: published,
			taskType: fanoutTaskTypePublish, status: fanoutStatusDone,
		})
		require.NoError(t, repo.EnqueueReplay(ctx, video, author, published))
		assert.Len(t, repo.tasks, 2, "两种 task_type 共享 video_id,但必须能并存")
	})
}

// TestFanoutTruncateErr_截断落在UTF8字符边界 500 字节上限若切在中文(3 字节/字)中间,
// 会产出非法 UTF-8;PostgreSQL 在 UTF-8 编码下拒收这种字节序列,MarkFailed 的 UPDATE
// 会整条失败 —— attempts 涨不上去、任务永远到不了死信,只能被反复租约回收重投。
func TestFanoutTruncateErr_截断落在UTF8字符边界(t *testing.T) {
	// 600 字节中文,500 字节处必然切在字符中间(500 = 3*166 + 2)
	got := fanoutTruncateErr(strings.Repeat("错", 200))
	assert.True(t, utf8.ValidString(got), "截断结果必须是合法 UTF-8")
	assert.Equal(t, 498, len(got), "应回退到 166 个完整字符(3 字节/字)")

	// 纯 ASCII 不受影响,恰好截到上限
	assert.Equal(t, fanoutMaxLastErrorLen, len(fanoutTruncateErr(strings.Repeat("a", 600))))
	// 未超上限的原样返回
	assert.Equal(t, "boom", fanoutTruncateErr("boom"))
}

// ---------- outbox 状态机 ----------
//
// 下面几条用 fakeFanoutRepo 钉住领取/失败的「状态机契约」。
// 它们能证明:退避写没写、死信会不会被再领、领取后行有没有移出 pending —— 三个 bug 的所在。
// 它们不能证明:真实 PostgreSQL 上并发 FOR UPDATE SKIP LOCKED 的互斥。
// 那条要靠真库集成测试,内存桩(fake)给不出并发语义。

// TestClaimPending_死信不再被领取 已达 MaxAttempts 的任务(死信)永远不该回到队列。
// 修复前 MarkFailed 只自增 attempts、不判上限,这类任务会每 200ms 被再领一次、无限重试。
func TestClaimPending_死信不再被领取(t *testing.T) {
	now := time.Now()
	dead := &fakeFanoutTask{
		id: 1, videoID: 100, authorID: 7, publishedAt: now,
		attempts: fanoutMaxAttempts, status: fanoutStatusDead,
		nextAttemptAt: now, updatedAt: now,
	}
	repo := newFakeFanoutRepo(dead)

	tasks, err := repo.ClaimPending(context.Background(), 100)
	require.NoError(t, err)
	assert.Empty(t, claimedIDs(tasks), "死信任务不该被领取")

	// 再确认一次状态机入口:差一次就到上限(attempts = Max-1)时,一次失败必须转死信
	almost := &fakeFanoutTask{
		id: 2, videoID: 200, authorID: 7, publishedAt: now,
		attempts: fanoutMaxAttempts - 1, status: fanoutStatusProcessing,
		nextAttemptAt: now, updatedAt: now,
	}
	repo.tasks = append(repo.tasks, almost)
	require.NoError(t, repo.MarkFailed(context.Background(), almost.id, "still failing"))
	require.Equal(t, fanoutStatusDead, almost.status, "第 MaxAttempts 次失败应转死信")

	// 死信之后无论过多久都不该被领走
	repo.clock = func() time.Time { return now.Add(24 * time.Hour) }
	tasks, err = repo.ClaimPending(context.Background(), 100)
	require.NoError(t, err)
	assert.Empty(t, claimedIDs(tasks), "转死信后即便过了很久也不该被领取")
}

// TestMarkFailed_退避期内不再被领取 MarkFailed 必须把 next_attempt_at 推到未来,
// 紧跟着的领取拿不到它;等越过退避窗口后才重新可领。修复前完全不写退避,会被立即重领。
func TestMarkFailed_退避期内不再被领取(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := base
	repo := newFakeFanoutRepo(&fakeFanoutTask{
		id: 1, videoID: 100, authorID: 7, publishedAt: base,
		status: fanoutStatusProcessing, nextAttemptAt: base, updatedAt: base,
	})
	repo.clock = func() time.Time { return cur }

	require.NoError(t, repo.MarkFailed(context.Background(), 1, "boom"))
	require.True(t, repo.find(1).nextAttemptAt.After(base), "退避时刻必须落到未来")

	tasks, err := repo.ClaimPending(context.Background(), 100)
	require.NoError(t, err)
	assert.Empty(t, claimedIDs(tasks), "退避期内不该被领取")

	// 把时钟推过退避窗口,任务应重新可领
	cur = base.Add(fanoutBackoff(1) + time.Second)
	tasks, err = repo.ClaimPending(context.Background(), 100)
	require.NoError(t, err)
	assert.Equal(t, []int64{1}, claimedIDs(tasks), "越过退避窗口后应重新可领")
}

// TestClaimPending_领取即移出待处理 领取会把行推进 status = 3,同一刻的第二次领取
// 不能再拿到它。修复前的「仅 SELECT 不改状态」正是重复投递的根因 —— 事务一提交,
// 锁就释放,行还停在 pending,第二个实例(或本实例下一轮)会再领一遍。
func TestClaimPending_领取即移出待处理(t *testing.T) {
	now := time.Now()
	repo := newFakeFanoutRepo(&fakeFanoutTask{
		id: 1, videoID: 100, authorID: 7, publishedAt: now,
		status: fanoutStatusPending, nextAttemptAt: now, updatedAt: now,
	})
	ctx := context.Background()

	first, err := repo.ClaimPending(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, []int64{1}, claimedIDs(first), "首次领取应拿到该任务")

	second, err := repo.ClaimPending(ctx, 100)
	require.NoError(t, err)
	assert.Empty(t, claimedIDs(second), "行已移出待处理,同刻再领不该拿到")
}

// TestClaimPending_按可领时刻排序避免队头饥饿 修复前按 id 升序取批,
// 最老的失败任务若一直留在队头会把后面所有任务堵死。改成按 next_attempt_at 排序后,
// 一个退避到未来的老任务应让位于一个此刻就可领的较新任务。
func TestClaimPending_按可领时刻排序避免队头饥饿(t *testing.T) {
	now := time.Now()
	// id 更小(更老)但退避到远期 —— 不该因此挡住 id 更大的可领任务
	backedOff := &fakeFanoutTask{
		id: 1, videoID: 100, authorID: 7, publishedAt: now,
		status: fanoutStatusPending, nextAttemptAt: now.Add(time.Hour), updatedAt: now,
	}
	ready := &fakeFanoutTask{
		id: 2, videoID: 200, authorID: 7, publishedAt: now,
		status: fanoutStatusPending, nextAttemptAt: now, updatedAt: now,
	}
	repo := newFakeFanoutRepo(backedOff, ready)

	tasks, err := repo.ClaimPending(context.Background(), 100)
	require.NoError(t, err)
	assert.Equal(t, []int64{2}, claimedIDs(tasks), "应领到此刻可领的 id=2,而不是被远期老任务阻塞")
}

// TestMarkFailed_不覆盖已完结任务 租约过期后才醒来的 worker,对其任务调 MarkFailed 或
// MarkDone 时,任务可能已被别人回收并标成完成/死信 —— 这类调用必须无操作,不能把它
// 翻回 pending。
//
// 注意:真实实现的守卫在 SQL 的 WHERE status = processing 里,内存 fake 只能镜像这套
// 语义、锁住契约;SQL 守卫本身要真 PostgreSQL 才能验。
func TestMarkFailed_不覆盖已完结任务(t *testing.T) {
	now := time.Now()
	done := &fakeFanoutTask{
		id: 1, videoID: 100, authorID: 7, publishedAt: now,
		attempts: 3, status: fanoutStatusDone, nextAttemptAt: now, updatedAt: now,
	}
	repo := newFakeFanoutRepo(done)

	require.NoError(t, repo.MarkFailed(context.Background(), 1, "迟到的失败"))
	assert.Equal(t, fanoutStatusDone, done.status, "已完成的任务不能被 MarkFailed 翻回 pending")
	assert.Equal(t, 3, done.attempts, "不持有的任务,attempts 也不该被改动")

	// MarkDone 只对 processing 生效:对未持有的 pending 任务调它是无操作
	pending := &fakeFanoutTask{
		id: 2, videoID: 200, authorID: 7, publishedAt: now,
		status: fanoutStatusPending, nextAttemptAt: now, updatedAt: now,
	}
	repo.tasks = append(repo.tasks, pending)
	require.NoError(t, repo.MarkDone(context.Background(), 2))
	assert.Equal(t, fanoutStatusPending, pending.status, "不持有的任务不能被标完成")
}

// TestRun_单任务panic不拖垮worker 投递中 panic 必须被 recover 兜住、按失败处理
// (MarkFailed),而不是让未捕获的 panic 打崩整个进程 —— 一个坏任务只降级不致命。
func TestRun_单任务panic不拖垮worker(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	now := time.Now()
	repo := &signalingRepo{
		fakeFanoutRepo: newFakeFanoutRepo(&fakeFanoutTask{
			id: 1, videoID: 100, authorID: 7, publishedAt: now,
			status: fanoutStatusPending, nextAttemptAt: now, updatedAt: now,
		}),
		failed: make(chan int64, 1),
	}
	// 粉丝查询故意 panic,模拟投递路径里的意外崩溃
	w := NewFanoutWorker(repo, nil, panicFollowers{}, fakeBigV{big: map[int64]bool{7: false}}, rdb)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	select {
	case id := <-repo.failed:
		assert.Equal(t, int64(1), id, "panic 的任务应被 MarkFailed 走失败路径,而不是崩掉进程")
	case <-time.After(2 * time.Second):
		t.Fatal("超时:panic 未被 recover,或未按失败处理")
	}

	cancel()
	<-done // 等 Run 退出,避免 cleanup 关 rdb 时它还在用
}

// TestRun_循环panic不拖垮进程 消费循环自身的 panic(非 DeliverOne 内部)必须被兜住。
// 与上一条的分工:deliverSafely 只保护单条任务,ClaimPending / MarkDone / MarkFailed
// 这些循环体代码没有保护;而 Run 跑在 main 起的裸 goroutine 里,没有 HTTP 中间件兜 panic,
// 漏出去会直接带走整个进程。
func TestRun_循环panic不拖垮进程(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	w := NewFanoutWorker(panicClaimRepo{newFakeFanoutRepo()}, nil, nil, nil, rdb)

	require.NotPanics(t, func() { w.Run(context.Background()) },
		"消费循环自身的 panic 必须被兜住,不能冒到 goroutine 顶端打崩进程")
}

// ---------- 升入大V:缓存回填 ----------

// recentVideoFeedRepo 只实现 PromoteBigVCache 用到的 ListRecentPublishedVideoIDs,
// 其余方法返回零值。用来直接喂入构造好的 VideoRef(含 published_at 为 NULL 的行)——
// pagedFeedRepo 会把 NULL 过滤掉,测不到「跳过 NULL」这条路径。
type recentVideoFeedRepo struct {
	refs []VideoRef
	err  error
}

func (r recentVideoFeedRepo) ListRecentPublishedVideoIDs(context.Context, int64, int) ([]VideoRef, error) {
	return r.refs, r.err
}

func (recentVideoFeedRepo) ListLatestVideos(context.Context, time.Time, int64, int) ([]*video.Video, error) {
	return nil, nil
}

func (recentVideoFeedRepo) ListVideosByLikes(context.Context, int64, int64, int) ([]*video.Video, error) {
	return nil, nil
}

func (recentVideoFeedRepo) ListVideosByIDs(context.Context, []int64) ([]*video.Video, error) {
	return nil, nil
}

// TestPromoteBigVCache_回填到大V缓存 升入大V 时把近期视频写进 feed:bigv:<id>,
// score 取视频自己的 published_at 微秒,且不碰任何收件箱。
func TestPromoteBigVCache_回填到大V缓存(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7

	base := time.Now().Add(-time.Hour).Truncate(time.Second).Unix()
	pub1 := time.Unix(base, 222333000)
	pub2 := time.Unix(base, 111222000)
	feeds := recentVideoFeedRepo{refs: []VideoRef{
		{ID: 101, PublishedAt: &pub1},
		{ID: 102, PublishedAt: &pub2},
	}}

	w, mr, rdb := newFanoutWorker(t, feeds, nil, fakeBigV{})

	require.NoError(t, w.PromoteBigVCache(ctx, author))

	entries := zsetEntries(t, rdb, bigVKey(author))
	require.Len(t, entries, 2, "两条近期视频都应回填进大V缓存")
	got := make(map[string]float64, len(entries))
	for _, e := range entries {
		got[e.Member.(string)] = e.Score
	}
	assert.Equal(t, float64(pub1.UnixMicro()), got["101"], "score 必须是 published_at 的微秒")
	assert.Equal(t, float64(pub2.UnixMicro()), got["102"])
	assert.NotEqual(t, float64(time.Now().UnixMicro()), got["101"], "绝不能用 now() 当 score")

	assert.Empty(t, keysWithPrefix(mr, inboxKeyPrefix), "回填只写作者自己的缓存,不碰任何收件箱")
}

// TestPromoteBigVCache_推送参数与DeliverOne对齐 钉住回填用的条目窗口与 key TTL:
// 二者必须与 DeliverOne 的大V 分支逐一对齐(bigVEntry / bigVTTL),传错会静默改变
// 缓存的保留期或可见范围。两个可观察的后果各钉一条:
//   - key TTL 必须是 bigVTTL(30d),传成 bigVEntry(7d)会被这条抓住;
//   - 越出 bigVEntry(7d)窗口但仍在 bigVTTL(30d)内的旧视频必须被脚本裁掉,
//     若误把窗口传成 bigVTTL,它就会留在缓存里而被这条抓住。
func TestPromoteBigVCache_推送参数与DeliverOne对齐(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7

	inside := time.Now().Add(-time.Hour)            // 7 天窗口内
	outside := time.Now().Add(-10 * 24 * time.Hour) // 越出 7 天,但仍在 30 天内
	feeds := recentVideoFeedRepo{refs: []VideoRef{
		{ID: 101, PublishedAt: &inside},
		{ID: 102, PublishedAt: &outside},
	}}

	w, mr, rdb := newFanoutWorker(t, feeds, nil, fakeBigV{})
	require.NoError(t, w.PromoteBigVCache(ctx, author))

	assert.Equal(t, bigVTTL, mr.TTL(bigVKey(author)), "缓存 TTL 必须用 bigVTTL,不能用 bigVEntry")

	entries := zsetEntries(t, rdb, bigVKey(author))
	members := make(map[string]bool, len(entries))
	for _, e := range entries {
		members[e.Member.(string)] = true
	}
	assert.True(t, members[memberString(101)], "窗口内的视频应进缓存")
	assert.False(t, members[memberString(102)],
		"越出 bigVEntry 窗口的旧视频应被裁掉(窗口误用 bigVTTL 就会留下它)")
}

// TestPromoteBigVCache_跳过NULL的published_at published_at 列可空,
// 一条 NULL 不该打挂整次回填。
func TestPromoteBigVCache_跳过NULL的published_at(t *testing.T) {
	ctx := context.Background()
	const author int64 = 7

	pub := time.Now().Add(-time.Hour)
	feeds := recentVideoFeedRepo{refs: []VideoRef{
		{ID: 101, PublishedAt: &pub},
		{ID: 102, PublishedAt: nil}, // 列可空:历史遗留行
	}}

	w, _, rdb := newFanoutWorker(t, feeds, nil, fakeBigV{})
	require.NoError(t, w.PromoteBigVCache(ctx, author), "NULL 行应被跳过,不能让整次回填失败")

	entries := zsetEntries(t, rdb, bigVKey(author))
	require.Len(t, entries, 1, "只有非 NULL 的那条该进缓存")
	assert.Equal(t, memberString(101), entries[0].Member)
}

// TestPromoteBigVCache_空结果不报错 没有近期视频的作者升入大V 不该出错,也不该建 key。
func TestPromoteBigVCache_空结果不报错(t *testing.T) {
	w, mr, _ := newFanoutWorker(t, recentVideoFeedRepo{}, nil, fakeBigV{})

	require.NoError(t, w.PromoteBigVCache(context.Background(), 7))
	assert.Empty(t, keysWithPrefix(mr, bigVKeyPrefix), "无视频就不该建缓存 key")
}

// TestPromoteBigVCache_rdb为nil返回错误 没有 Redis 无从回填,必须报错而非假装成功。
func TestPromoteBigVCache_rdb为nil返回错误(t *testing.T) {
	w := NewFanoutWorker(newFakeFanoutRepo(), recentVideoFeedRepo{}, nil, fakeBigV{}, nil)
	require.Error(t, w.PromoteBigVCache(context.Background(), 7))
}
