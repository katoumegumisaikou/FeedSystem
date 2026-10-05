package feed

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"feed-system/internal/model/video"
	"feed-system/internal/pkg/errs"
	"feed-system/internal/pkg/sfcache"
)

// FeedService Feed 流业务层。
type FeedService struct {
	repo      FeedRepository
	rdb       *redis.Client
	likes     LikeStatusProvider
	followees FolloweeLister
	bigvs     BigVChecker
}

// LikeStatusProvider 提供用户对一批视频的点赞状态。
type LikeStatusProvider interface {
	GetUserLikeStatuses(ctx context.Context, userID int64, videoIDs []int64) (map[int64]bool, error)
}

// FolloweeLister 提供「我关注了谁」。由 follow 包实现,main.go 注入 ——
// feed 不直接依赖 follow 包。
type FolloweeLister interface {
	ListFolloweeIDs(ctx context.Context, followerID int64) ([]int64, error)
}

// BigVChecker 判断给定作者里哪些是大V(走拉模式而非推模式)。
// *follow.BigVSet 天然满足这个形状。
type BigVChecker interface {
	Contains(ctx context.Context, authorIDs []int64) (map[int64]bool, error)
}

func NewFeedService(repo FeedRepository, rdb *redis.Client, likes LikeStatusProvider, followees FolloweeLister, bigvs BigVChecker) *FeedService {
	return &FeedService{repo: repo, rdb: rdb, likes: likes, followees: followees, bigvs: bigvs}
}

const (
	latestVideosKey    = "feed:video:latest"
	latestVideosTTL    = time.Hour
	latestBackfillSize = 500
	defaultLatestLimit = 100
)

// 关注流的 key 与淘汰参数。
//
// 前缀 feed:bigv: 同时被每个作者的 ZSET(feed:bigv:<author_id>)和
// follow 包的名单 SET(feed:bigv:set)/哨兵(feed:bigv:ready)使用。
// 之所以不撞,是因为后两个后缀不是纯数字,而 author_id 一定是。
// 以后往这个前缀下加 key 必须保持这个前提。
const (
	inboxKeyPrefix = "feed:inbox:"
	bigVKeyPrefix  = "feed:bigv:"

	inboxCap   = 50                  // 收件箱条数硬顶
	inboxEntry = 7 * 24 * time.Hour  // 条目时间窗口,超期条目滚出
	inboxTTL   = 14 * 24 * time.Hour // key TTL,写入与读取后都重置

	bigVEntry = 7 * 24 * time.Hour  // 大V 缓存的条目窗口
	bigVTTL   = 30 * 24 * time.Hour // 大V 缓存 key TTL,发布时重置
)

// followingCandidateCap 关注流单页回表查询的候选条目上限。
//
// 合并后的候选 = 收件箱(≤50)+ 每个大V缓存(各≤50),关注 200 个大V 就是上万个条目;
// 若不设限,每个翻页请求都会拿上万个 ID 打一次库。一页最多消费 limit(≤100)条,
// 这里按 score 降序只保留最前面这么多条 —— 足以填满一页并留出余量给被过滤掉的条目
// (视频已删/已下架/作者已取关),又远小于最坏情况。
//
// 为什么不改成「少读几个大V缓存」:缓存之间没有可靠顺序,少读任何一个都可能漏掉
// 本该排在本页的条目,游标会越过它、永久漏条。只截断候选条目则安全 —— 见 ListFollowing
// 里对游标的处理。
const followingCandidateCap = 500

// estimateBigVMax 预分配 entries 时,最多按几个大V 的缓存估容量(见 ListFollowing)。
// 取 8:足够覆盖多数用户,又不会让「关注 200 个大V 但缓存多为空」的请求白占约 160KB。
const estimateBigVMax = 8

// estimateBigVCount 取预分配用的大V 个数,封顶 estimateBigVMax。
func estimateBigVCount(n int) int {
	if n > estimateBigVMax {
		return estimateBigVMax
	}
	return n
}

// inboxKey 某个用户的收件箱 key。
func inboxKey(userID int64) string {
	return inboxKeyPrefix + strconv.FormatInt(userID, 10)
}

// bigVKey 某个大V 的全局缓存 key。
func bigVKey(authorID int64) string {
	return bigVKeyPrefix + strconv.FormatInt(authorID, 10)
}

// inboxPushScript 往收件箱/大V 缓存推一条视频并顺手淘汰。
//
// 必须打包成一个脚本:分开做的话,进程崩在 ZADD 与 trim 之间会留下膨胀的 ZSET。
// 按 score 删的是过期条目,按 rank 删的是条数硬顶,两者不能互相替代 ——
// 只有前者会让长期无人关注的收件箱无限长,只有后者会让陈旧条目一直占位。
//
// KEYS[1] 收件箱  ARGV: 1=score(微秒) 2=video_id 3=窗口下界(微秒) 4=条数上限 5=TTL 秒
var inboxPushScript = redis.NewScript(`
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', '(' .. ARGV[3])
redis.call('ZREMRANGEBYRANK', KEYS[1], 0, -(tonumber(ARGV[4]) + 1))
redis.call('EXPIRE', KEYS[1], ARGV[5])
return 1
`)

func toFeedItem(v *video.Video) *FeedItem {
	if v == nil {
		return nil
	}
	return &FeedItem{
		ID:           v.ID,
		AuthorID:     v.AuthorID,
		Username:     v.Username,
		AvatarURL:    v.AvatarURL,
		Title:        v.Title,
		Description:  v.Description,
		PlayURL:      v.PlayURL,
		CoverURL:     v.CoverURL,
		CreatedAt:    v.CreatedAt,
		PublishedAt:  v.PublishedAt,
		Status:       v.Status,
		PlayCount:    v.PlayCount,
		LikesCount:   v.LikesCount,
		CommentCount: v.CommentCount,
	}
}

// setLikeStatuses 把当前用户的点赞态填进 items(原地改)。
//
// 刻意不返回 error:点赞态只是卡片上的一个装饰字段,缺失时 IsLike 保持 nil ——
// 这是 DTO 明确允许的状态(见 FeedItem.IsLike 注释「未计算」)。若为它让整个视频流
// 返回 500,就是一个装饰字段拖垮了主功能,代价与收益完全不成比例。所以两种失败
// 都只记日志、然后跳过填充,让视频流照常返回。
//
//   - 提供方未配置:属接线遗漏,记 Error 级便于发现;
//   - 查询失败:提供方已记录底层错误(见 video.GetUserLikeStatuses),这里只补一条
//     说明「本次降级」的日志,点明响应仍会正常返回、只是没有点赞态。
func (s *FeedService) setLikeStatuses(ctx context.Context, userID int64, items []FeedItem) {
	if userID <= 0 || len(items) == 0 {
		return
	}
	if s.likes == nil {
		slog.ErrorContext(ctx, "视频流点赞状态查询器未配置,本页不填点赞态", "user_id", userID)
		return
	}

	videoIDs := make([]int64, 0, len(items))
	for _, item := range items {
		videoIDs = append(videoIDs, item.ID)
	}
	statuses, err := s.likes.GetUserLikeStatuses(ctx, userID, videoIDs)
	if err != nil {
		// 底层错误已由提供方记录;这里补记降级影响,并继续返回列表
		slog.WarnContext(ctx, "点赞状态查询失败,本页不填点赞态(列表照常返回)",
			"user_id", userID, "video_count", len(videoIDs))
		return
	}
	for i := range items {
		liked := statuses[items[i].ID]
		items[i].IsLike = &liked
	}
}

// parseFeedCursor 解析复合游标 (created_at, id)。两个字段要么都给要么都不给 ——
// 只给一个的话边界不完整,不如当首页处理。首页返回 hasCursor=false。
//
// at 以 time.Time 返回;ListFollowing 的排序键是 ZSET score(int64 微秒),
// 需自行取 at.UnixMicro() 对齐 —— 与 ListLatest 直接比时间同源,往返无损。
func parseFeedCursor(createdAt, videoID *int64) (hasCursor bool, at time.Time, id int64, err error) {
	switch {
	case createdAt == nil && videoID == nil:
		return false, time.Time{}, 0, nil
	case createdAt == nil || videoID == nil:
		return false, time.Time{}, 0, errs.ErrInvalidParam.WithMsg("分页游标无效")
	case *videoID <= 0:
		return false, time.Time{}, 0, errs.ErrInvalidParam.WithMsg("分页游标无效")
	default:
		return true, time.UnixMicro(*createdAt), *videoID, nil
	}
}

func (s *FeedService) ListLatest(ctx context.Context, req ListLatestReq, userID int64) (*ListLatestResp, error) {
	if s.rdb == nil {
		return nil, errs.ErrInternalCache
	}

	limit := req.Limit
	if limit == 0 {
		limit = defaultLatestLimit
	}
	// 多要一条:能取到就说明后面还有,这时才给 cursor。
	// 和 ListLike 同一套判据 —— next_cursor 的「有没有」立刻是准的,
	// 前端不用为了确认到底而多请求一次空页
	want := limit + 1

	// 取缓存里最旧的一条,只为判断 ZSET 空不空
	oldest, err := s.rdb.ZRangeWithScores(ctx, latestVideosKey, 0, 0).Result()
	if err != nil {
		return nil, errs.ErrInternal
	}

	var result []*FeedItem
	if len(oldest) == 0 {
		// sfcache 这里只用于并发去重,缓存本身由下面的 ZSET 保存。
		videos, err := sfcache.Load(ctx, nil, latestVideosKey, time.Minute,
			func(loadCtx context.Context) ([]*video.Video, error) {
				// beforeID 传 0:回填要的是「最新一批」,不设上界
				videos, err := s.repo.ListLatestVideos(loadCtx, time.Now(), 0, latestBackfillSize)
				if err != nil {
					return nil, errs.ErrInternal
				}
				return videos, nil
			})
		if err != nil {
			return nil, errs.ErrInternal
		}

		members := make([]redis.Z, 0, len(videos))
		for _, item := range videos {
			card := toFeedItem(item)
			result = append(result, card)

			member, err := json.Marshal(card)
			if err != nil {
				slog.WarnContext(ctx, "最新流成员序列化失败,跳过", "video_id", item.ID, "err", err)
				continue
			}
			// score 只用于排序;分页游标取 member 中的 created_at。
			members = append(members, redis.Z{
				Score:  float64(item.CreatedAt.Unix()),
				Member: member,
			})
		}

		// 空 ZSET 是合法状态,没有已发布视频时不调用 ZAdd。
		if len(members) > 0 {
			// 一起设置 TTL,避免 ZADD 成功后进程退出留下永久缓存。
			pipe := s.rdb.TxPipeline()
			pipe.ZAdd(ctx, latestVideosKey, members...)
			pipe.Expire(ctx, latestVideosKey, latestVideosTTL)
			if _, err := pipe.Exec(ctx); err != nil {
				return nil, errs.ErrInternal
			}
		}
	} else {
		zSlice, err := s.rdb.ZRevRangeWithScores(ctx, latestVideosKey, 0, int64(latestBackfillSize)-1).Result()
		if err != nil {
			return nil, errs.ErrInternal
		}
		for _, z := range zSlice {
			var item FeedItem
			if err := json.Unmarshal([]byte(z.Member.(string)), &item); err != nil {
				slog.WarnContext(ctx, "最新流缓存里有解不开的成员", "err", err)
				continue
			}
			result = append(result, &item)
		}
	}

	// ZSET 的 score 只到秒,同一秒的成员 Redis 按 member 字典序排 —— 那个顺序和
	// (created_at, id) 无关。缓存里成员不多(≤ latestBackfillSize),统一再排一次,
	// 让内存里的顺序和 SQL 的 ORDER BY created_at DESC, id DESC 完全一致;
	// 否则同一时刻的几条会在翻页时重复出现
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		if am, bm := a.CreatedAt.UnixMicro(), b.CreatedAt.UnixMicro(); am != bm {
			return am > bm
		}
		return a.ID > b.ID
	})

	// 游标是 (created_at, id) 复合的,校验与 ListFollowing 共用 parseFeedCursor
	hasCursor, cursorAt, cursorID, err := parseFeedCursor(req.CursorCreatedAt, req.CursorVideoID)
	if err != nil {
		return nil, err
	}

	resp := ListLatestResp{Items: []FeedItem{}}

	// 缓存里最旧的那条(按 created_at DESC, id DESC 排最后),用来判断缓存覆盖到哪
	var (
		oldestAt time.Time
		oldestID int64
	)
	for _, item := range result {
		if item == nil {
			continue
		}
		if oldestID == 0 || older(item.CreatedAt, item.ID, oldestAt, oldestID) {
			oldestAt, oldestID = item.CreatedAt, item.ID
		}
	}

	// 游标落在缓存覆盖范围之外(比缓存最旧的还旧)时,直接查数据库
	if len(result) == 0 || (hasCursor && older(cursorAt, cursorID, oldestAt, oldestID)) {
		videos, err := s.repo.ListLatestVideos(ctx, cursorAt, cursorID, want)
		if err != nil {
			slog.ErrorContext(ctx, "查询最新流失败", "cursor_at", cursorAt, "cursor_id", cursorID, "err", err)
			return nil, errs.ErrInternal.WithMsg("查询最新流失败")
		}
		for _, item := range videos {
			resp.Items = append(resp.Items, *toFeedItem(item))
		}
	} else {
		for _, item := range result {
			if len(resp.Items) >= want {
				break
			}
			if !hasCursor || older(item.CreatedAt, item.ID, cursorAt, cursorID) {
				resp.Items = append(resp.Items, *item)
			}
		}

		// 热数据不足一页时,从缓存边界之前查冷数据补足
		if len(resp.Items) < want {
			remaining := want - len(resp.Items)
			videos, err := s.repo.ListLatestVideos(ctx, oldestAt, oldestID, remaining)
			if err != nil {
				return nil, errs.ErrInternal.WithMsg("查询最新流失败")
			}
			for _, item := range videos {
				resp.Items = append(resp.Items, *toFeedItem(item))
			}
		}
	}

	// 多要的那条只用来判「还有没有更多」,不进响应
	hasMore := len(resp.Items) > limit
	if hasMore {
		resp.Items = resp.Items[:limit]
	}

	// 点赞状态只加在响应副本上,不写入共享 Redis 缓存。
	// 失败只在 setLikeStatuses 内部记日志并跳过填充,不影响本页返回。
	s.setLikeStatuses(ctx, userID, resp.Items)

	// 只在确实还有下一页时给游标;到底了就是 nil(见 ListLatestResp 注释)
	if hasMore && len(resp.Items) > 0 {
		last := resp.Items[len(resp.Items)-1]
		resp.NextCursor = &FeedCursor{CreatedAt: last.CreatedAt.UnixMicro(), VideoID: last.ID}
	}
	return &resp, nil
}

// older 判断 (at, id) 是否严格排在 (thanAt, thanID) 之后,也就是更旧的那一侧。
// 与 SQL 的 (created_at, id) < (?, ?) 同一语义:先比时间,时间相同再比 id。
//
// 时间只比到微秒 —— created_at 列是 TIMESTAMP(微秒),游标也按微秒传。
// 直接比 time.Time 的话,值里残留的纳秒会让本该相等的一对判成不等,
// 那些视频会被当成「不是更旧」而静默跳过,翻页整批漏掉
func older(at time.Time, id int64, thanAt time.Time, thanID int64) bool {
	am, bm := at.UnixMicro(), thanAt.UnixMicro()
	if am != bm {
		return am < bm
	}
	return id < thanID
}

func (s *FeedService) ListLike(ctx context.Context, req ListLikeReq, userID int64) (*ListLikeResp, error) {
	limit := req.Limit
	if limit == 0 {
		limit = defaultLatestLimit
	}
	if limit < 1 || limit > defaultLatestLimit {
		return nil, errs.ErrInvalidParam.WithMsg("每页数量无效")
	}

	var cursorLikesCount, cursorVideoID int64
	switch {
	case req.CursorLikesCount == nil && req.CursorVideoID == nil:
		// 首页不带游标。
	case req.CursorLikesCount == nil || req.CursorVideoID == nil:
		return nil, errs.ErrInvalidParam.WithMsg("分页游标无效")
	default:
		cursorLikesCount = *req.CursorLikesCount
		cursorVideoID = *req.CursorVideoID
		if cursorLikesCount < 0 || cursorVideoID <= 0 {
			return nil, errs.ErrInvalidParam.WithMsg("分页游标无效")
		}
	}

	videos, err := s.repo.ListVideosByLikes(ctx, cursorLikesCount, cursorVideoID, limit+1)
	if err != nil {
		slog.ErrorContext(ctx, "按点赞数查询视频流失败", "cursor_likes_count", cursorLikesCount, "cursor_video_id", cursorVideoID, "err", err)
		return nil, errs.ErrInternal
	}

	hasMore := len(videos) > limit
	if hasMore {
		videos = videos[:limit]
	}

	resp := ListLikeResp{Items: make([]FeedItem, 0, len(videos))}
	for _, item := range videos {
		if card := toFeedItem(item); card != nil {
			resp.Items = append(resp.Items, *card)
		}
	}
	if hasMore && len(resp.Items) > 0 {
		last := resp.Items[len(resp.Items)-1]
		resp.NextCursor = &LikeCursor{LikesCount: last.LikesCount, VideoID: last.ID}
	}
	s.setLikeStatuses(ctx, userID, resp.Items)
	return &resp, nil
}

// followingEntry 关注流合并排序用的中间条目:视频 ID 与排序分数(Unix 微秒)。
type followingEntry struct {
	videoID int64
	score   int64
}

// ListFollowing 关注流:合并「收件箱(推模式)」与「大V 缓存(拉模式)」两个来源。
func (s *FeedService) ListFollowing(ctx context.Context, req ListFollowingReq, userID int64) (*ListFollowingResp, error) {
	if userID <= 0 {
		return nil, errs.ErrUnauthorized
	}
	if s.rdb == nil {
		return nil, errs.ErrInternalCache
	}
	// 依赖没接上不能 500 掉整个端点,按空关注列表处理
	if s.followees == nil {
		slog.ErrorContext(ctx, "关注流的关注列表查询器未配置", "user_id", userID)
		return &ListFollowingResp{Items: []FeedItem{}}, nil
	}

	limit := req.Limit
	if limit == 0 {
		limit = defaultLatestLimit
	}
	// 与 ListLike 同一套校验:HTTP 绑定虽然也拦,但直连 service 的调用(测试、内部)
	// 传负数或超大值会让「取 limit 条」失去意义、还把 hasMore 判成假。服务层自己兜住。
	if limit < 1 || limit > defaultLatestLimit {
		return nil, errs.ErrInvalidParam.WithMsg("每页数量无效")
	}

	// 游标校验与 ListLatest 共用 parseFeedCursor;排序键统一成微秒整数,
	// 与 ZSET score(int64 微秒)直接比较,避免 time.Time 残留
	hasCursor, cursorAtTime, cursorID, err := parseFeedCursor(req.CursorCreatedAt, req.CursorVideoID)
	if err != nil {
		return nil, err
	}
	cursorAt := cursorAtTime.UnixMicro()

	// 收件箱:推模式写入,成员是 video_id 字符串,score 是发布时刻(微秒)
	inbox, err := s.rdb.ZRevRangeWithScores(ctx, inboxKey(userID), 0, -1).Result()
	if err != nil {
		slog.ErrorContext(ctx, "读取关注流收件箱失败", "user_id", userID, "err", err)
		return nil, errs.ErrInternal
	}

	followees, err := s.followees.ListFolloweeIDs(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "查询关注列表失败", "user_id", userID, "err", err)
		return nil, errs.ErrInternal
	}

	// 大V 判定失败只降级成「无大V」:它是加速层,不是真相来源,不能挡读路径。
	// 没有关注的人时连 Contains 都不用调。
	var bigvMap map[int64]bool
	if s.bigvs == nil {
		slog.WarnContext(ctx, "关注流的大V 判定器未配置,按无大V处理", "user_id", userID)
	} else if len(followees) > 0 {
		m, err := s.bigvs.Contains(ctx, followees)
		if err != nil {
			slog.ErrorContext(ctx, "查询大V名单失败,本次按无大V降级", "user_id", userID, "err", err)
		} else {
			bigvMap = m
		}
	}

	bigVIDs := make([]int64, 0, len(followees))
	for _, id := range followees {
		if bigvMap[id] {
			bigVIDs = append(bigVIDs, id)
		}
	}

	// 预分配容量:收件箱的条数已知,大V 缓存各最多 bigVInboxCap 条。
	//
	// 但只按前 estimateBigVMax 个大V 估,不按全部 —— 每个大V 最多 50 条 × 16 字节
	// (两个 int64)≈ 800B,关注 200 个大V 时按全量估会预占约 160KB;而缓存可能是空的
	// (大V 久未发新视频),那 160KB 就白占了。只估前几个:多数用户的关注里大V 不多,
	// 完全命中不触发扩容;超出预估的部分退化成 append 自带增长,不会白占内存。
	entries := make([]followingEntry, 0, len(inbox)+estimateBigVCount(len(bigVIDs))*bigVInboxCap)
	for _, z := range inbox {
		if e, ok := parseFollowingEntry(z); ok {
			entries = append(entries, e)
		}
	}

	// 大V 走拉模式:一次性拿到所有大V 的缓存,放在同一个 pipeline 里,避免 N 次往返
	if len(bigVIDs) > 0 {
		pipe := s.rdb.Pipeline()
		cmds := make([]*redis.ZSliceCmd, len(bigVIDs))
		for i, aid := range bigVIDs {
			cmds[i] = pipe.ZRevRangeWithScores(ctx, bigVKey(aid), 0, -1)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			slog.ErrorContext(ctx, "读取大V 缓存失败", "user_id", userID, "err", err)
			return nil, errs.ErrInternal
		}
		for _, cmd := range cmds {
			for _, z := range cmd.Val() {
				if e, ok := parseFollowingEntry(z); ok {
					entries = append(entries, e)
				}
			}
		}
	}

	// 两个来源必须在游标过滤和截断之前先合并:否则先截断会把另一个来源里本该
	// 排在本页的条目切掉,本页变短、游标还可能越过没看过的条目。

	// Redis ZSET 对相同 score 的成员按 member 字节序排,不是数值序(会得到
	// [10 100 101 11 9] 这种)。score 是微秒时间戳,同微秒虽罕见但确实可能,
	// 那时字节序的兜底是错的。转成 int64(微秒 ≈1.79e15 < 2^53,精确)后
	// 按 (score, video_id) 降序重排,和最新流的 SQL ORDER BY 口径对齐。
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		return entries[i].videoID > entries[j].videoID
	})

	// 推模式与拉模式对同一作者互斥,正常不会重;但关注关系从普通作者变成大V 时,
	// 收件箱里可能残留旧条目,和大V 缓存重复。这里用 seen 集合按 videoID 去重,而不是
	// 只比相邻一条:相邻去重依赖「相同 videoID 必然相同 score」这个隐含前提,一旦日后
	// 有路径给同一视频写了不同 score,相同 id 会被别的条目隔开而不相邻,只比前一条就
	// 漏过去,读者会看到重复卡片。seen 集合 O(n) 且与顺序无关,保留首次出现(score 最高)
	// 的那条,输出顺序不变。
	if len(entries) > 0 {
		seen := make(map[int64]struct{}, len(entries))
		deduped := entries[:0]
		for _, e := range entries {
			if _, dup := seen[e.videoID]; dup {
				continue
			}
			seen[e.videoID] = struct{}{}
			deduped = append(deduped, e)
		}
		entries = deduped
	}

	if hasCursor {
		filtered := entries[:0]
		for _, e := range entries {
			// 严格小于游标:比 (cursorAt, cursorID) 更旧的那侧
			if e.score < cursorAt || (e.score == cursorAt && e.videoID < cursorID) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	// 候选集封顶:见 followingCandidateCap。截断发生在游标过滤之后,所以每一页都是
	// 「从本页游标往下最多再看 cap 条」。被切掉的只是「本页根本消费不到、且更旧」的
	// 尾巴 —— 游标仍取实际扫描到的位置,下一页用 (score,id) 严格小于游标继续,所以
	// 尾巴下一轮还能被扫到,不会被跳过。真正的风险在「本页没填满」那条路径:若因
	// 没填满就判成到底、不给游标,被切掉的尾巴就再也无人回访。truncated 标记正是
	// 为堵这条路径 —— 被截断过就一定要给游标(见下方 if truncated)。
	truncated := false
	if len(entries) > followingCandidateCap {
		entries = entries[:followingCandidateCap]
		truncated = true
	}

	// 候选集(cursor 过滤、并按 followingCandidateCap 封顶后的条目)已经全在内存里,
	// 一次性把这批卡片查出来,再在内存里按顺序过滤 + 截断,避免逐条回表造成 O(n) 次
	// DB 往返。ID 数量已被 followingCandidateCap 限住,单次 IN 查询不会无限膨胀。
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.videoID)
	}
	videos, err := s.repo.ListVideosByIDs(ctx, ids)
	if err != nil {
		slog.ErrorContext(ctx, "查询关注流视频失败", "user_id", userID, "err", err)
		return nil, errs.ErrInternal.WithMsg("查询关注流失败")
	}
	byID := make(map[int64]*video.Video, len(videos))
	for _, v := range videos {
		byID[v.ID] = v
	}

	// 按「当前」关注集合过滤,而不是条目被推进收件箱时的关注关系:收件箱条目是
	// 关注期内推的,取关后最多 7 天才滚出,不按当前关注集合过滤的话,已取关作者的
	// 视频会继续露出。大V 走拉模式,取关即不再读其缓存 —— 两条路径口径必须一致。
	// 作者要等拿到卡片才知道,所以过滤只能放在这一步。
	followeeSet := make(map[int64]bool, len(followees))
	for _, id := range followees {
		followeeSet[id] = true
	}

	// 边扫边过滤,直到攒够一页或候选耗尽。被过滤掉的条目(视频已删/下架、作者已取关)
	// 不能让分页悄悄判成「到底了」:只要后面还有候选条目,就必须给出游标,
	// 否则这些旧条目会被永久跳过。
	resp := ListFollowingResp{Items: make([]FeedItem, 0, limit)}
	var (
		lastExamined followingEntry
		hasMore      bool
	)
	for i, e := range entries {
		lastExamined = e
		v := byID[e.videoID]
		if v == nil || !followeeSet[v.AuthorID] {
			continue
		}
		resp.Items = append(resp.Items, *toFeedItem(v))
		if len(resp.Items) == limit {
			// 本页已满,立刻跳出。注意此刻 lastExamined 就是刚 append 的那张卡 ——
			// 两个值本就相同,不存在「游标越过没返回的条目」这回事。
			//
			// 这里用 lastExamined(而不是从 resp.Items 取最后一张)是为了让两条退出
			// 路径共用一个变量:循环自然跑完(没填满)时,lastExamined 停在窗口实际
			// 扫描到的位置,游标也照样正确。
			//
			// 是否给游标看后面还有没有候选:truncated 表示被 cap 砍过(后面必有),
			// 否则看本次扫描是否已到 entries 末尾。
			hasMore = truncated || i < len(entries)-1
			break
		}
	}

	// 候选被 followingCandidateCap 截断时,被切掉的尾巴一定还有条目:即使本页因
	// 过滤而没填满,也必须给游标(取实际扫描到的最后一条),否则那些更旧的条目
	// 会被永久判成「到底了」而丢掉。下一页面用 (score,id) 严格小于游标继续。
	if truncated {
		hasMore = true
	}

	// 游标必须取 ZSET 的 score(发布时刻)本身,而不是回表拿到的 created_at:
	// 过滤时比的就是 score,两边同源才不会漏页或重复。若用 created_at 生成游标、
	// 却拿 score 比较,长期草稿的 created_at 会明显早于 score,边界当场错位
	if hasMore {
		resp.NextCursor = &FeedCursor{CreatedAt: lastExamined.score, VideoID: lastExamined.videoID}
	}

	// 点赞态填充失败只降级(不填 IsLike),在 setLikeStatuses 内部处理,不阻断返回。
	// 放在续 TTL 之前也无所谓失败顺序 —— 它已不再返回错误,不会让下面的 Expire 被跳过。
	s.setLikeStatuses(ctx, userID, resp.Items)

	// 读的时候续 TTL,让活跃用户关注流不过期。key 不存在时 EXPIRE 返回 false,
	// 那是正常情况,不当失败;真出错也只记日志,绝不因此让请求失败。
	if err := s.rdb.Expire(ctx, inboxKey(userID), inboxTTL).Err(); err != nil {
		slog.WarnContext(ctx, "刷新收件箱 TTL 失败", "user_id", userID, "err", err)
	}

	return &resp, nil
}

// parseFollowingEntry 把 ZSET 成员解析成条目。成员是 video_id 字符串;
// 解不出来(脏数据)就丢弃,不让一条坏数据打挂整页。
func parseFollowingEntry(z redis.Z) (followingEntry, bool) {
	member, ok := z.Member.(string)
	if !ok {
		return followingEntry{}, false
	}
	id, err := strconv.ParseInt(member, 10, 64)
	if err != nil {
		return followingEntry{}, false
	}
	return followingEntry{videoID: id, score: int64(z.Score)}, true
}
