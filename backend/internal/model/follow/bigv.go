package follow

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// BigVThreshold 粉丝数达到此值即按「大V」处理,走拉模式而非推模式。
// 这是「入列」的上界;降级的下界是 BigVThreshold - BigVHysteresis,见 BigVHysteresis。
const BigVThreshold int64 = 50000

// BigVHysteresis 大V 判定的滞回带宽:升级用 BigVThreshold,降级用
// BigVThreshold - BigVHysteresis,两条线之间「维持现状」。
//
// 为什么需要滞回:升降同用一个阈值时,粉丝数在 50000 上下抖 ±1 就会让名单反复增删。
// 而每次「真实降级」都会同步触发一次大V缓存重放(ZREVRANGE + 最多 50 次入队),
// 于是少量账号就能在临界值来回关注/取关,反复让服务做无用功。滞回把这条带拉宽 5000:
// 跨越需要粉丝数真的移动这么多,1~2 个账号的横跳就失效了。
const BigVHysteresis int64 = 5000

const (
	bigVSetKey       = "feed:bigv:set"
	bigVReadyKey     = "feed:bigv:ready"
	bigVLockKey      = "feed:bigv:lock"
	bigVLockTTL      = 30 * time.Second
	bigVBuildWait    = 50 * time.Millisecond
	bigVBuildRetries = 20
)

// 注意:上面三个 key,连同 service.go 里降级重放防抖用的 `feed:bigv:replay:<author_id>`,
// 都与别处的「每作者 ZSET」key `feed:bigv:<author_id>` 共用 `feed:bigv:` 前缀。
// 不冲突的前提是:该前缀下除 author_id 外的第一段永远非数字(replay 也不以数字开头),
// 而 author_id 永远是纯数字 —— 两边的 key 空间因此天然不相交。
// 今后在这个前缀下新增 key,必须保持这一性质。

// BigVSet 维护「全平台哪些作者是大V」这份名单。
//
// 名单本体是一个 Redis SET,配一个独立的哨兵 key 标记「已构建过」。哨兵不可省:
// 空 SET 的 key 根本不存在(删掉最后一个成员后 EXISTS 就是 0),所以只靠 SET
// 无法区分「从没建过」和「建过但没有大V」—— 后者会导致平台没有大V时每次读都触发全量重算。
type BigVSet struct {
	repo FollowRepository
	rdb  *redis.Client
}

// NewBigVSet 构造大V名单;rdb 传 nil 表示无缓存(与项目其它地方一致),
// 此时所有读都降级成「没有大V」,不报错。
func NewBigVSet(repo FollowRepository, rdb *redis.Client) *BigVSet {
	return &BigVSet{repo: repo, rdb: rdb}
}

// Contains 返回 authorIDs 中哪些是大V。内部先 EnsureBuilt。
// 返回的 map 只含值为 true 的项,调用方按「此人是不是大V」读即可。
// rdb == nil 或 Redis 读失败时降级:返回空 map(等同于「没有大V」),不报错。
func (s *BigVSet) Contains(ctx context.Context, authorIDs []int64) (map[int64]bool, error) {
	if s.rdb == nil {
		return map[int64]bool{}, nil
	}
	if err := s.EnsureBuilt(ctx); err != nil {
		// 纯防御:EnsureBuilt 目前所有分支都只降级、恒返回 nil,这里实际触发不了。
		// 保留是为了它将来的实现若真开始返回错误,读路径也不会被挡。
		slog.ErrorContext(ctx, "大V名单懒构建失败,本次按「无大V」降级", "err", err)
		return map[int64]bool{}, nil
	}
	if len(authorIDs) == 0 {
		return map[int64]bool{}, nil
	}

	members := make([]any, len(authorIDs))
	for i, id := range authorIDs {
		members[i] = id
	}
	flags, err := s.rdb.SMIsMember(ctx, bigVSetKey, members...).Result()
	if err != nil {
		slog.ErrorContext(ctx, "查询大V名单失败,本次按「无大V」降级", "err", err)
		return map[int64]bool{}, nil
	}

	out := make(map[int64]bool, len(authorIDs))
	// 防御:理论上 flags 恒与入参等长,截到较短一侧即可,越界只可能来自协议异常,
	// 不该因此 panic 掉整条读路径
	n := len(authorIDs)
	if len(flags) < n {
		n = len(flags)
	}
	for i := 0; i < n; i++ {
		if flags[i] {
			out[authorIDs[i]] = true
		}
	}
	return out, nil
}

// EnsureBuilt 冷启动懒构建。哨兵存在则直接返回。
// 构建失败一律降级(不报错),见各分支注释。
func (s *BigVSet) EnsureBuilt(ctx context.Context) error {
	if s.rdb == nil {
		return nil
	}

	ready, err := s.isReady(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "大V名单哨兵查询失败,本次按「无大V」降级", "err", err)
		return nil
	}
	if ready {
		return nil
	}

	ok, err := s.rdb.SetNX(ctx, bigVLockKey, 1, bigVLockTTL).Result()
	if err != nil {
		slog.ErrorContext(ctx, "获取大V构建锁失败,本次按「无大V」降级", "err", err)
		return nil
	}
	if ok {
		// 抢到锁:本 goroutine 负责构建,完事无论成败都释放锁(锁本身也有 TTL 兜底)
		defer s.rdb.Del(ctx, bigVLockKey)
		if err := s.Rebuild(ctx); err != nil {
			slog.ErrorContext(ctx, "大V名单构建失败", "err", err)
		}
		return nil
	}

	// 没抢到锁 = 别的 goroutine 正在构建,轮询等它写完哨兵。
	// 等超时仍无哨兵就放弃并降级(当作没有大V),绝不返回 error:
	// 卡死的构建者不能把整条读路径一起拖垮 —— 大V名单是加速层,不是真相来源。
	for i := 0; i < bigVBuildRetries; i++ {
		time.Sleep(bigVBuildWait)
		// 必须每次重查:抢到锁的那一方可能刚好在这轮之前写完哨兵
		ready, err := s.isReady(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "大V名单哨兵查询失败,本次按「无大V」降级", "err", err)
			return nil
		}
		if ready {
			return nil
		}
	}
	slog.ErrorContext(ctx, "等待大V名单构建超时,本次按「无大V」降级",
		"waited_ms", bigVBuildWait.Milliseconds()*int64(bigVBuildRetries))
	return nil
}

// isReady 查哨兵。哨兵存在才算「构建过」—— 空 SET 的 key 不存在,不能拿它判断。
func (s *BigVSet) isReady(ctx context.Context) (bool, error) {
	n, err := s.rdb.Exists(ctx, bigVReadyKey).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// OnFollowerCountChanged 关注/取关后调用,按当前粉丝数决定增删名单。幂等。
// 在写路径上,出错会返回给调用方(同时记日志),由调用方决定是否重试/忽略。
//
// 判定带滞回(见 BigVHysteresis),三段式:
//   - count >= BigVThreshold                → 入列(SAdd)
//   - count <  BigVThreshold-BigVHysteresis → 出列(SRem),这才是一次真实降级
//   - 落在带内 [下界, 上界)                  → 什么都不做,维持原状态
//
// 带内「不做」而非「按当前值重设」,让成员状态自动保留:带里本是大V 的继续是大V,
// 本是小的继续是小的 —— 不产生任何抖动,这正是挡住临界值横跳的关键。
//
// 两个返回值回答的都是「本次是否真的发生了一次状态迁移」,而不是「当前是什么状态」:
//   - promoted:本次 SADD 真的把作者加进了名单(返回 1)。原本就是成员时返回 false,
//     让上层据此保证「升入大V 时的缓存回填」至多跑一次,不随每次关注重来。
//   - downgraded:本次 SREM 真的把作者从名单里删掉了(返回 1),也就是原本是成员、
//     此刻跌破下界。作者本来就已在阈值下时返回 false,避免上层把普通的小V取关
//     误当成降级事件而重复触发重放。
//
// 两者都靠 Redis 集合操作的返回值判定,而 SADD 与 SREM 的 count 区间不相交
// (入列 >= 上界、出列 < 下界,带内两者都不走),所以一次调用最多只会让其中一个为 true。
func (s *BigVSet) OnFollowerCountChanged(ctx context.Context, authorID, count int64) (promoted bool, downgraded bool, err error) {
	if s.rdb == nil {
		return false, false, nil
	}

	switch {
	case count >= BigVThreshold:
		added, err := s.rdb.SAdd(ctx, bigVSetKey, authorID).Result()
		if err != nil {
			slog.ErrorContext(ctx, "更新大V名单失败", "author_id", authorID, "count", count, "err", err)
			return false, false, err
		}
		return added > 0, false, nil

	case count < BigVThreshold-BigVHysteresis:
		removed, err := s.rdb.SRem(ctx, bigVSetKey, authorID).Result()
		if err != nil {
			slog.ErrorContext(ctx, "更新大V名单失败", "author_id", authorID, "count", count, "err", err)
			return false, false, err
		}
		return false, removed > 0, nil

	default:
		// 滞回带内:维持现状。不读写 Redis —— 抖动在这里被吸收掉
		return false, false, nil
	}
}

// Rebuild 全量重算:覆盖式重建 SET + 写哨兵。
func (s *BigVSet) Rebuild(ctx context.Context) error {
	if s.rdb == nil {
		return nil
	}

	// 用降级下界(而非 BigVThreshold)重算:滞回带 [下界, 上界) 里的作者若曾在名单中,
	// 事件驱动的增删会把他们保留为成员;重算没有这份记忆,只能二选一。取下界=宁可多纳入,
	// 与带内「维持现状」的语义一致。反过来用上界会把这批人踢出名单,而 Rebuild 不触发重放 ——
	// 他们的缓存条目会当场不可见,正好复现重放机制要修的丢数据。多纳入的代价只是这些作者
	// 多走拉模式(写更少),安全。
	ids, err := s.repo.ListBigVAuthorIDs(ctx, BigVThreshold-BigVHysteresis)
	if err != nil {
		return err
	}

	pipe := s.rdb.Pipeline()
	// DEL 不能省:只做 SADD 的「增量重建」永远删不掉已经掉出阈值的账号 ——
	// 比如大V掉粉后,旧成员会一直留在 SET 里,判定从此失真。先清空再灌才是真相。
	pipe.Del(ctx, bigVSetKey)
	if len(ids) > 0 {
		members := make([]any, len(ids))
		for i, id := range ids {
			members[i] = id
		}
		pipe.SAdd(ctx, bigVSetKey, members...)
	}
	// 哨兵最后写:先建好名单再置 ready,读者看到哨兵时名单一定已就位
	pipe.Set(ctx, bigVReadyKey, 1, 0)

	_, err = pipe.Exec(ctx)
	return err
}
