package follow

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"feed-system/internal/pkg/errs"
)

// FollowBackfiller 新关注时把对方最近的视频补进关注者的收件箱。
// 由 feed 包实现、main.go 注入 —— follow 不直接依赖 feed,避免循环依赖。
// 允许为 nil:补拉失败不该让「关注」这个动作失败。
type FollowBackfiller interface {
	BackfillOnFollow(ctx context.Context, followerID, followeeID int64) error
}

// BigVDowngradeReplayer 大V 掉到阈值以下时,把它 feed:bigv:<id> 缓存里的存量视频
// 重新投递进粉丝收件箱。
//
// 不补这一次的话,那些视频会永久消失:它们发布时只进了大V缓存(没进任何收件箱),
// 而作者一旦不是大V,读路径就不再读那个缓存了。
// 由 feed 包实现、main.go 注入 —— follow 不直接依赖 feed。
// 允许为 nil。
type BigVDowngradeReplayer interface {
	ReplayBigVCache(ctx context.Context, authorID int64) error
}

// BigVPromoter 账号刚升入大V 时,把它的历史视频回填进它自己的全局缓存 feed:bigv:<id>。
//
// 不回填的话,它在大V 缓存里是空的 —— 而新粉丝关注它时补拉是直接跳过的
// (BackfillOnFollow 对大V 提前返回),于是新粉丝在它发下一条视频之前一条都看不到。
// 由 feed 包实现、main.go 注入 —— follow 不直接依赖 feed。允许为 nil。
type BigVPromoter interface {
	PromoteBigVCache(ctx context.Context, authorID int64) error
}

// FollowService 关注业务层
type FollowService struct {
	repo     FollowRepository
	bigv     *BigVSet
	backfill FollowBackfiller
	replayer BigVDowngradeReplayer
	promoter BigVPromoter

	// rdb 仅供「大V降级重放」的防抖使用,与 bigv 共用同一个客户端。
	// 不额外开一个构造参数:那会牵动 main.go 的接线,而 bigv 本来就持有同一个 redis.Client。
	// rdb == nil(无缓存部署)时防抖降级为「不防抖、直接重放」
	rdb *redis.Client
}

// NewFollowService 构造 FollowService。backfill / replayer / promoter 可为 nil。
func NewFollowService(repo FollowRepository, bigv *BigVSet, backfill FollowBackfiller, replayer BigVDowngradeReplayer, promoter BigVPromoter) *FollowService {
	s := &FollowService{repo: repo, bigv: bigv, backfill: backfill, replayer: replayer, promoter: promoter}
	if bigv != nil {
		s.rdb = bigv.rdb
	}
	return s
}

// validateFollowArgs 关注双方的公共校验(登录态 / ID 合法 / 不能关注自己)。
// Follow 与 Unfollow 共用一份,避免两条路径的校验口径日后漂移
func validateFollowArgs(followerID, followeeID int64) error {
	if followerID <= 0 {
		return errs.ErrUnauthorized.WithMsg("用户未登录")
	}
	if followeeID <= 0 {
		return errs.ErrInvalidParam.WithMsg("用户 ID 无效")
	}
	if followerID == followeeID {
		return errs.ErrInvalidParam.WithMsg("不能关注自己")
	}
	return nil
}

// followerCount 读被关注者当前粉丝数,失败按内部错误处理。
func (s *FollowService) followerCount(ctx context.Context, followeeID int64) (int64, error) {
	count, err := s.repo.CountFollowers(ctx, followeeID)
	if err != nil {
		slog.ErrorContext(ctx, "查询粉丝数失败", "followee_id", followeeID, "err", err)
		return 0, errs.ErrInternal.WithMsg("查询粉丝数失败")
	}
	return count, nil
}

// Follow 关注某人(POST /users/:id/follow)。
//
// 步骤的先后顺序都是有意的,不能调换:
//  1. 先做纯参数校验,不碰库。
//  2. 「已关注」短路必须排在关注数上限校验之前 —— 关注数已满的人重新点一个已关注的人,
//     是幂等重放,不该被上限挡住报错;短路还要跳过大V名单与补拉,否则重试会重复投递。
//  3. 关注数上限不再由服务层单独预检,而是交给 repo.Follow 在事务里原子判定并插入。
//     分两条语句(先 Count 再 Follow)会有 TOCTOU:并发请求同时读到「没满」再各自
//     插入,上限形同虚设。仓储把超限表达成 ErrFolloweeLimit,这里映射成 409。
//
// 写库之后的两个副作用(更新大V名单、补拉历史)都是尽力而为的优化,失败只记日志、
// 不回滚也不报错:关注这个动作本身已经成功,为一次善意的优化回滚已成功的主操作更糟。
func (s *FollowService) Follow(ctx context.Context, followerID, followeeID int64) (*FollowResp, error) {
	if err := validateFollowArgs(followerID, followeeID); err != nil {
		return nil, err
	}

	following, err := s.repo.IsFollowing(ctx, followerID, followeeID)
	if err != nil {
		slog.ErrorContext(ctx, "查询关注状态失败", "follower_id", followerID, "followee_id", followeeID, "err", err)
		return nil, errs.ErrInternal.WithMsg("查询关注状态失败")
	}
	if following {
		// 已关注:幂等返回成功。跳过大V名单(粉丝数没变)与补拉(重试不该重复投递),
		// 但仍读一次粉丝数把响应字段填准确 —— 只读,无副作用
		count, err := s.followerCount(ctx, followeeID)
		if err != nil {
			return nil, err
		}
		return &FollowResp{Following: true, FollowerCount: count}, nil
	}

	if err := s.repo.Follow(ctx, followerID, followeeID, MaxFollowees); err != nil {
		// 超限是业务错误(409),不能和其它仓储错误一样吞成 500
		if errors.Is(err, ErrFolloweeLimit) {
			return nil, errs.ErrConflict.WithMsg("关注数已达上限")
		}
		// 被关注者不存在(外键失败)是客户端传了脏 id,映射成 404,也不能冒成 500
		if errors.Is(err, ErrFolloweeNotFound) {
			return nil, errs.ErrNotFound.WithMsg("被关注者不存在")
		}
		slog.ErrorContext(ctx, "关注失败", "follower_id", followerID, "followee_id", followeeID, "err", err)
		return nil, errs.ErrInternal.WithMsg("关注失败")
	}

	count, err := s.followerCount(ctx, followeeID)
	if err != nil {
		return nil, err
	}

	// 大V名单是最终一致的加速层:漏更新只误判一个账号的 fan-out 走向,且名单有全量重建路径,
	// 所以失败不让关注本身失败。关注只会让粉丝数上升,不会触发降级重放,忽略 downgraded。
	promoted, _, err := s.bigv.OnFollowerCountChanged(ctx, followeeID, count)
	if err != nil {
		slog.ErrorContext(ctx, "关注后更新大V名单失败", "followee_id", followeeID, "count", count, "err", err)
	}

	// 作者恰在这次关注升入大V:它的缓存 feed:bigv:<id> 此前从未被填过(缓存只在它发布时写),
	// 而新粉丝关注大V 走拉模式、补拉被跳过(BackfillOnFollow 对大V 提前返回),于是这些新粉丝
	// 在它发下一条视频之前一条都看不到。升入时把它的历史视频回填进缓存,补上这段空窗。
	//
	// 与降级重放对称:回填只读一个作者的近期视频、写一个 Redis key(至多 50 条),代价远小于
	// 重放(最多 5 万收件箱),所以同步执行、不加防抖。promoted 只在本次真的入列时为 true,
	// 已是大V 的重复关注不会重复回填。
	//
	// 失败只记日志:与其它副作用一致,不让一次已成功的关注为善意优化回滚
	if promoted && s.promoter != nil {
		if err := s.promoter.PromoteBigVCache(ctx, followeeID); err != nil {
			slog.ErrorContext(ctx, "升入大V后回填缓存失败", "followee_id", followeeID, "err", err)
		}
	}

	// 补拉对两种账号行为不同:
	//   - 小V:把对方最近 10 条已发布视频推进「你」的收件箱(推模式,内容按人分发)
	//   - 大V:直接跳过 —— 他走拉模式,读关注流时现拿他的全局缓存,补拉是多余的;
	//     而且大V 粉丝海量,补一次等于把整份缓存往每个新粉丝的收件箱里塞一遍
	//
	// 失败只记日志:让一次已经成功的关注,为一次善意优化而回滚更糟
	if s.backfill != nil {
		if err := s.backfill.BackfillOnFollow(ctx, followerID, followeeID); err != nil {
			slog.ErrorContext(ctx, "关注后补拉历史视频失败",
				"follower_id", followerID, "followee_id", followeeID, "err", err)
		}
	}

	return &FollowResp{Following: true, FollowerCount: count}, nil
}

// Unfollow 取关某人(DELETE /users/:id/follow)。
//
// 只做参数校验 → 删除 → 更新大V名单。不做上限校验(取关只会让关注数变少),
// 不做补拉(取关还可能要把补进去的视频撤掉,那是另一个方向的事)。
// 取关一个从没关注过的人算成功:仓储是物理 DELETE,删不到行也不算错
func (s *FollowService) Unfollow(ctx context.Context, followerID, followeeID int64) (*FollowResp, error) {
	if err := validateFollowArgs(followerID, followeeID); err != nil {
		return nil, err
	}

	if err := s.repo.Unfollow(ctx, followerID, followeeID); err != nil {
		slog.ErrorContext(ctx, "取关失败", "follower_id", followerID, "followee_id", followeeID, "err", err)
		return nil, errs.ErrInternal.WithMsg("取关失败")
	}

	count, err := s.followerCount(ctx, followeeID)
	if err != nil {
		return nil, err
	}

	_, downgraded, err := s.bigv.OnFollowerCountChanged(ctx, followeeID, count)
	if err != nil {
		// 同 Follow:大V名单更新失败只记日志,不回滚已成功的取关
		slog.ErrorContext(ctx, "取关后更新大V名单失败", "followee_id", followeeID, "count", count, "err", err)
	}

	// 作者恰在这次取关跌破大V阈值:缓存里只进过 feed:bigv:、没进过任何收件箱的存量视频
	// 会因读路径不再读那个缓存而消失,必须回放进粉丝收件箱。
	//
	// 走防抖入口:重放是「ZREVRANGE + 最多 50 次入队」的同步耗时操作,
	// 单个账号可以把作者吊在阈值上反复关注/取关来反复触发(见 replayBigVCacheDebounced)。
	if downgraded && s.replayer != nil {
		s.replayBigVCacheDebounced(ctx, followeeID)
	}

	return &FollowResp{Following: false, FollowerCount: count}, nil
}

// bigVReplayDebounceTTL 降级重放的防抖窗口。
// 取 1 分钟:足以挡住「把作者吊在阈值上反复关注/取关」的手动抖动,
// 又不至于把一次真实的二次降级挡太久(重放本身是可重试的补救)
const bigVReplayDebounceTTL = time.Minute

// bigVReplayDebounceKey 降级重放的防抖 key,按作者区分
func bigVReplayDebounceKey(authorID int64) string {
	return "feed:bigv:replay:" + strconv.FormatInt(authorID, 10)
}

// replayBigVCacheDebounced 在重放前加一层廉价防抖。
//
// 真正的去重是 DB 层的 UNIQUE (video_id, task_type):重复重放不会产生重复任务。
// 这个防抖只解决「每个请求同步做一遍 ZREVRANGE + 最多 50 次 EnqueueReplay」的
// 单价问题 —— 否则单个账号能在阈值附近反复 follow/unfollow,每次都把整段同步工作
// 重做一遍;唯一约束挡住了重复任务,却挡不住重复的劳动。TTL 内只放第一次过去。
//
// rdb == nil 时无从防抖,直接重放(与「无 Redis 时整体降级」的口径一致);
// SETNX 出错时也按未防抖处理:宁可能多跑一次,也不让补救被丢掉。
func (s *FollowService) replayBigVCacheDebounced(ctx context.Context, authorID int64) {
	if s.rdb == nil {
		s.replayBigVCache(ctx, authorID)
		return
	}
	ok, err := s.rdb.SetNX(ctx, bigVReplayDebounceKey(authorID), 1, bigVReplayDebounceTTL).Result()
	if err != nil {
		slog.WarnContext(ctx, "降级重放防抖查询失败,本次仍执行重放", "followee_id", authorID, "err", err)
		s.replayBigVCache(ctx, authorID)
		return
	}
	if !ok {
		slog.InfoContext(ctx, "降级重放命中防抖窗口,跳过本次同步重放", "followee_id", authorID)
		return
	}
	s.replayBigVCache(ctx, authorID)
}

// replayBigVCache 执行一次重放。失败只记日志,绝不回滚已成功的取关
func (s *FollowService) replayBigVCache(ctx context.Context, authorID int64) {
	if err := s.replayer.ReplayBigVCache(ctx, authorID); err != nil {
		slog.ErrorContext(ctx, "大V降级后重放缓存视频失败", "followee_id", authorID, "err", err)
	}
}

// followerListDefaultLimit / followerListMaxLimit 粉丝列表的分页参数。
// 关注列表能一次返回是因为它被 MaxFollowees 封顶;粉丝数没有上界,必须分页
const (
	followerListDefaultLimit = 50
	followerListMaxLimit     = 100
)

// ListFollowers 谁关注了我(GET /users/me/followers)。
//
// 只认当前登录用户,不接受「查某个别人的粉丝」—— 路由挂了 SetSensitive,
// 没 token 在中间件就被拦成 401,这里的 userID 兜底是纵深防御。
//
// 分页用 follower_id 游标而不是 OFFSET:大V 有上万粉丝,OFFSET 每页都从头数一遍,
// 越翻越慢(与 fan-out 遍历粉丝时用的是同一条查询、同一套理由)。
func (s *FollowService) ListFollowers(ctx context.Context, userID int64, req ListFollowersReq) (*ListFollowersResp, error) {
	if userID <= 0 {
		return nil, errs.ErrUnauthorized.WithMsg("用户未登录")
	}

	limit := req.Limit
	if limit == 0 {
		limit = followerListDefaultLimit
	}
	// HTTP 绑定虽然也拦,但直连 service 的调用(测试、内部)传负数或超大值会让
	// 「取 limit 条」失去意义,服务层自己兜住 —— 与视频流同一套口径
	if limit < 1 || limit > followerListMaxLimit {
		return nil, errs.ErrInvalidParam.WithMsg("每页数量无效")
	}

	var afterID int64
	if req.Cursor != nil {
		afterID = *req.Cursor
	}

	// 多取一条:能取到就说明后面还有,这时才给游标(与视频流同一套判据)
	ids, err := s.repo.ListFollowerIDs(ctx, userID, afterID, limit+1)
	if err != nil {
		slog.ErrorContext(ctx, "查询粉丝列表失败", "user_id", userID, "err", err)
		return nil, errs.ErrInternal.WithMsg("查询粉丝列表失败")
	}

	resp := ListFollowersResp{FollowerIDs: []int64{}}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	resp.FollowerIDs = append(resp.FollowerIDs, ids...)
	// 只在确实还有下一页时给游标;游标取本页最后一条的 follower_id,
	// 下一页用 follower_id > 游标 继续,严格前进不会重复
	if hasMore && len(ids) > 0 {
		last := ids[len(ids)-1]
		resp.NextCursor = &last
	}
	return &resp, nil
}

// ListFollowees 取我关注的所有人(GET /users/me/followees)。
func (s *FollowService) ListFollowees(ctx context.Context, followerID int64) (*ListFolloweesResp, error) {
	if followerID <= 0 {
		return nil, errs.ErrUnauthorized.WithMsg("用户未登录")
	}

	ids, err := s.repo.ListFolloweeIDs(ctx, followerID)
	if err != nil {
		slog.ErrorContext(ctx, "查询关注列表失败", "follower_id", followerID, "err", err)
		return nil, errs.ErrInternal.WithMsg("查询关注列表失败")
	}
	// 空列表也要给 [],不能是 null —— 前端按数组用,不用判空
	if ids == nil {
		ids = []int64{}
	}

	return &ListFolloweesResp{FolloweeIDs: ids, Limit: MaxFollowees}, nil
}
