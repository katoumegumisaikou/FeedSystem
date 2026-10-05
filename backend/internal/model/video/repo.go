package video

import (
	"context"
	"errors"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// VideoRepository 视频仓储接口(对外暴露的唯一契约)。
// 声明成接口而不是结构体:service 依赖抽象,测试里才能注入内存实现
type VideoRepository interface {
	// CreateVideo 插入一条记录,GORM 会把自增主键回填进 v.ID
	CreateVideo(ctx context.Context, v *Video) error

	// FindVideoByID 按主键查,查不到返回 ErrNotFound
	FindVideoByID(ctx context.Context, id int64) (*Video, error)

	// FindVideoByPlayURL 按播放地址反查,查不到返回 ErrNotFound。
	// 公开文件路由只有 URL 没有 ID,靠它拿到视频状态才能判可见性(走 006 迁移的
	// idx_videos_play_url,是索引命中不是全表扫)
	FindVideoByPlayURL(ctx context.Context, playURL string) (*Video, error)

	// ListLikedVideoIDs 在给定视频 ID 中查询某用户点过赞的视频 ID,一次批量查询避免 N+1。
	ListLikedVideoIDs(ctx context.Context, userID int64, videoIDs []int64) ([]int64, error)

	// ListPlayHistory 取某用户「每个视频一条」的观看记录,按 (最近观看时间, video_id) 倒序。
	//
	// 一条对应一个视频,不是一次播放:play_records 是流水表(上报一次插一行),
	// 原样吐出来同一个视频会重复出现。所以先把该用户的记录按 video_id 收敛成
	// 「最近的那一行」,再按游标过滤和分页 —— 顺序不能反,先按游标过滤会把
	// 同一视频的另一条记录留到下一页,那个视频就会跨页重复出现。
	//
	// beforeVideoID 为 0 表示首页,不设边界;否则只取严格排在 (before, beforeVideoID)
	// 之后的那批。带上 video_id 是因为 watched_at 会重复:只比时间的话,
	// 同一微秒的两条会漏掉或重复,边界必须细化到「某条记录」。
	//
	// 只返回已发布且未软删的视频:下架的内容不该再从历史里冒出来。
	// 代价是 DISTINCT ON 用不上 idx_play_user_time 的顺序,要排一次序;
	// 单用户的历史规模有限,可接受
	ListPlayHistory(ctx context.Context, userID int64, before time.Time, beforeVideoID int64, limit int) ([]*PlayHistoryEntry, error)

	// FindVideosByIDs 按主键批量查,顺序不保证。用来把历史记录拼成视频卡片,避免 N+1
	FindVideosByIDs(ctx context.Context, ids []int64) ([]*Video, error)

	// SavePlayReport 在一个事务里追加一条播放流水,并把 videos.play_count 加一。
	// 两件事必须同生共死:流水是明细、play_count 是读列表直接用的聚合,
	// 只成一件就会出现「有明细没计数」这种对不上的漂移
	SavePlayReport(ctx context.Context, r *PlayRecord) error

	// MarkPublished 在一个事务里把「草稿」置为已发布,并写入一条 fan-out 任务。
	//
	// 两件事必须同生共死:分开写会在中间失败时留下「已发布但没 fan-out」的视频,
	// 而那种视频不会出现在任何粉丝的关注流里,且没有任何补偿路径。
	//
	// 状态更新带 WHERE status = 草稿 条件,只有真正完成这次迁移的事务才写列并插 outbox:
	// 并发重复发布时其它事务影响 0 行,直接跳过入队(实现内见细节)。
	//
	// 发布时刻由本方法内部取 time.Now(),不从外部传入:关注流按 published_at 排序,
	// 它必须是「当下」的时刻才能保证落在 7 天投递窗口内。若拿 video.CreatedAt
	// (上传时刻)当它,长期草稿会带着旧时间戳入队,投递时被窗口当场裁掉。
	//
	// 任务靠 UNIQUE (video_id, task_type) 幂等 —— 重复发布不会产生第二条同类型任务。
	MarkPublished(ctx context.Context, videoID, authorID int64) error

	// UpdateVideoFields 只更新传入的列,updated_at 由 GORM 自动填。
	// 用 map 而不是 struct:struct 更新会跳过零值,把 Description 清空成 "" 就写不进库
	UpdateVideoFields(ctx context.Context, id int64, fields map[string]any) error
}

// PlayHistoryEntry 观看历史的一条:视频 ID + 最近一次观看的进度。
//
// 只带 ID,视频本身由上层按 ID 批量取 —— 让这个查询只管播放记录,
// 不必把 videos 的十几列都摊进来
type PlayHistoryEntry struct {
	VideoID   int64
	Watched   int
	Duration  int
	WatchedAt time.Time
}

// listPlayHistorySQL 观看历史的分页查询。
//
// 内层 DISTINCT ON (video_id) 按 video_id 分组取 created_at 最大的那一行 ——
// 也就是「这个视频最近一次观看」。外层才按时间过滤、排序、截断。
// 内外顺序反了的话,同一个视频的旧记录会在下一页再冒出来一次。
//
// 手写 SQL 而不是 GORM 链:软删除条件平时由 GORM 自动补,但这里 JOIN 的是自己写的
// 子查询,GORM 认不出外层在查 videos,得把这条件显式写出来
const listPlayHistorySQL = `
SELECT h.video_id, h.watched, h.duration, h.created_at AS watched_at
FROM (
    SELECT DISTINCT ON (video_id) video_id, watched, duration, created_at
    FROM play_records
    WHERE user_id = ?
    ORDER BY video_id, created_at DESC
) h
JOIN videos v ON v.id = h.video_id
WHERE (h.created_at, h.video_id) < (?, ?) AND v.status = ? AND v.deleted_at IS NULL
ORDER BY h.created_at DESC, h.video_id DESC
LIMIT ?`

// ErrNotFound 仓储层「没查到」的哨兵错误。
// 不把 gorm.ErrRecordNotFound 透出去:它会一路冒到 response.Error,
// 而那里的类型断言认不出它不是 ServiceErr,最后只会给前端「未知错误」
var ErrNotFound = errors.New("video: not found")

// videoRepository 基于 GORM 的实现(包内私有,外部只能通过接口访问)
type videoRepository struct {
	db *gorm.DB
}

// 编译期断言:确保 videoRepository 实现 VideoRepository 接口
var _ VideoRepository = (*videoRepository)(nil)

// NewVideoRepository 构造 VideoRepository;返回接口类型以隐藏 GORM 实现细节
func NewVideoRepository(db *gorm.DB) VideoRepository {
	return &videoRepository{db: db}
}

func (r *videoRepository) CreateVideo(ctx context.Context, v *Video) error {
	return r.db.WithContext(ctx).Create(v).Error
}

func (r *videoRepository) FindVideoByID(ctx context.Context, id int64) (*Video, error) {
	var v Video
	err := r.db.WithContext(ctx).First(&v, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *videoRepository) FindVideoByPlayURL(ctx context.Context, playURL string) (*Video, error) {
	var v Video
	err := r.db.WithContext(ctx).Where("play_url = ?", playURL).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *videoRepository) ListLikedVideoIDs(ctx context.Context, userID int64, videoIDs []int64) ([]int64, error) {
	if userID <= 0 || len(videoIDs) == 0 {
		return []int64{}, nil
	}

	var likedVideoIDs []int64
	err := r.db.WithContext(ctx).
		Model(&VideoLike{}).
		Where("user_id = ? AND video_id IN ?", userID, videoIDs).
		Pluck("video_id", &likedVideoIDs).Error
	if err != nil {
		return nil, err
	}
	return likedVideoIDs, nil
}

// historyFirstPageAt 首页没有游标时的上界。取一个远到任何真实数据都排在它之前的时刻,
// 这样 SQL 只有一条路径,不用为「首页」再写一份查询(视频不可能是 9999 年发的)
var historyFirstPageAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

func (r *videoRepository) ListPlayHistory(ctx context.Context, userID int64, before time.Time, beforeVideoID int64, limit int) ([]*PlayHistoryEntry, error) {
	// 首页把边界推到「无穷远」,让 WHERE 里那个复合比较恒成立
	if beforeVideoID <= 0 {
		before = historyFirstPageAt
		beforeVideoID = math.MaxInt64
	}

	var entries []*PlayHistoryEntry
	err := r.db.WithContext(ctx).
		Raw(listPlayHistorySQL, userID, before, beforeVideoID, StatusPublished, limit).
		Scan(&entries).Error
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (r *videoRepository) FindVideosByIDs(ctx context.Context, ids []int64) ([]*Video, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var videos []*Video
	err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&videos).Error
	if err != nil {
		return nil, err
	}
	return videos, nil
}

// SavePlayReport 流水与计数必须一起成功,所以包一个事务:
// 分开写会在中间失败时留下「有明细没计数」的漂移,而 play_count 没有自愈途径
func (r *videoRepository) SavePlayReport(ctx context.Context, rec *PlayRecord) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(rec).Error; err != nil {
			return err
		}
		return tx.Model(&Video{}).
			Where("id = ?", rec.VideoID).
			// UpdateColumn 而非 Update:播放数涨了不等于内容被改动,不该顺带顶掉 updated_at
			UpdateColumn("play_count", gorm.Expr("play_count + 1")).Error
	})
}

// MarkPublished 状态与 fan-out 任务必须一起落库,所以包一个事务:
// 分开写会在中间失败时留下「已发布却没进粉丝关注流」的视频,而它没有自愈途径。
// 任务表靠 UNIQUE (video_id, task_type),重复发布插入冲突时直接跳过,天然幂等。
//
// published_at 取当下的 time.Now(),而不是复用 videos.created_at:后者是上传时刻,
// 长期草稿会在发布时带着一个很旧的时刻进入 7 天投递窗口,被 fan-out 脚本当场裁掉。
func (r *videoRepository) MarkPublished(ctx context.Context, videoID, authorID int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		publishedAt := time.Now()
		// 条件更新:只允许「草稿 → 已发布」这一次真实迁移。两个并发的 POST /publish
		// 都会在服务层看到 StatusDraft,无条件写的话后到者会把 published_at 覆盖成更晚的
		// 时刻,而它的 outbox 行会被 ON CONFLICT DO NOTHING 跳过(先到者的任务已占住
		// UNIQUE (video_id, task_type))—— 于是 videos.published_at(展示与列过滤用)与
		// outbox.published_at(ZSET score、游标用)各说各话,关注流排序与翻页就会漏条/重复。
		// 加 WHERE status = 草稿 后只有真正完成迁移的事务写列。
		res := tx.Model(&Video{}).
			Where("id = ? AND status = ?", videoID, StatusDraft).
			Updates(map[string]any{
				"status":       StatusPublished,
				"published_at": publishedAt,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 0 行 = 别人已抢先发布(或状态已被改走)。此时既不覆写列也不入队:
			// 只有「真正迁移了」的那次才允许写 outbox,列与行才永远同源。
			// 任务表本就靠 UNIQUE 去重,这里跳过不影响幂等,服务层按成功返回。
			return nil
		}
		// 只列写入用得到的三列,status / attempts 等交给库默认值 —— 列全抄一遍就多一份会漂移的 schema
		return tx.Table("fanout_tasks").
			Clauses(clause.OnConflict{DoNothing: true}).
			Create(map[string]any{
				"video_id":     videoID,
				"author_id":    authorID,
				"published_at": publishedAt,
			}).Error
	})
}

func (r *videoRepository) UpdateVideoFields(ctx context.Context, id int64, fields map[string]any) error {
	return r.db.WithContext(ctx).Model(&Video{}).Where("id = ?", id).Updates(fields).Error
}
