package video

import (
	"time"

	"gorm.io/gorm"
)

// 视频状态。数值不能重排:DB 列是 SMALLINT,已有数据按当前数值解释,
// 新增状态一律往后追加,不要插中间
//
// 只有「已发布」与「草稿」两个状态有写入路径:前者由 MarkPublished 写,后者由
// CompleteChunkUpload 建行时写。其余三个都是**预留但入口未实现** —— 读代码时
// 不要因为见到守卫就以为功能已存在。真库实测里 status 只会出现 1 与 4 两种值。
const (
	StatusTranscoding     int8 = iota // 0 转码中(尚未接入转码,当前没有代码写入这个值)
	StatusPublished                   // 1 已发布
	StatusTranscodeFailed             // 2 转码失败(同上,预留)

	// 3 已下架。**没有任何接口或仓储方法能设置它** —— 运营下架功能未实现。
	//
	// 但下游已有 4 处守卫在检查它,所以读代码时会以为这个状态是活的:
	//   - UpdateVideo   : 已下架不能编辑
	//   - UploadCover   : 已下架不能换封面
	//   - visibleTo     : 已下架对所有人不可见(含作者),文件路由与播放上报共用
	//   - repo 的列表查询: query.Where("status <> ?", StatusRemoved)
	//
	// 守卫本身是对的 —— 与其等运营接口做出来再补,不如先把不可变的规则写进读路径。
	// 但要清楚:**这些分支现在永远不会命中**,它们的测试也都是「用 fake 手工造出
	// StatusRemoved 才走得通」,不代表真实流量里出现过。
	//
	// 做运营接口时唯一要注意:必须与上面四处口径一致,否则会出现「下架了但还能编辑」
	// 或「下架了文件仍可下载」。
	StatusRemoved

	StatusDraft // 4 草稿:分片上传完成,但还没编辑标题/封面
)

// Video 视频。
//
// 索引不在 gorm tag 里声明 —— 本项目建表全走 goose 迁移、不用 AutoMigrate,
// tag 里的索引不生效,写了就是第二份会漂移的真相。真实定义见
// migrations/003_create_videos.sql:
// idx_videos_author_id · idx_videos_create_time · idx_videos_popularity_time_id
// idx_videos_likes_count_id · idx_videos_status
type Video struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"   json:"id"`
	AuthorID    int64  `gorm:"not null"                   json:"author_id"`
	Username    string `gorm:"type:varchar(255);not null" json:"username"`             // 冗余,列表免 JOIN
	AvatarURL   string `gorm:"type:varchar(512)"          json:"avatar_url,omitempty"` // 冗余,可空
	Title       string `gorm:"type:varchar(255);not null" json:"title"`
	Description string `gorm:"type:varchar(1000)"         json:"description,omitempty"`
	PlayURL     string `gorm:"type:varchar(255);not null" json:"play_url"`
	CoverURL    string `gorm:"type:varchar(255);not null" json:"cover_url"`

	// CreatedAt / UpdatedAt 是 GORM 约定名,按名字自动识别填充,不需要 autoCreateTime tag。
	// type 必须显式写:GORM 默认把 time.Time 映射成 timestamptz,而迁移里建的是 TIMESTAMP
	CreatedAt time.Time      `gorm:"type:timestamp;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"type:timestamp;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"type:timestamp"          json:"-"` // 软删除,不暴露前端

	// PublishedAt 首次发布的时刻。草稿期为 NULL,发布时写入。
	// 关注流按它排序 —— created_at 是「上传」时刻,长期草稿会在发布时带着一个
	// 很旧的 created_at 进入 7 天投递窗口,被 fan-out 脚本当场裁掉
	PublishedAt *time.Time `gorm:"type:timestamp" json:"published_at,omitempty"`

	Status     int8  `gorm:"not null;default:4" json:"status"` // 状态(0 转码中 / 1 已发布 / 2 转码失败 / 3 已下架 / 4 草稿)
	PlayCount  int64 `gorm:"not null;default:0" json:"play_count"`
	LikesCount int64 `gorm:"not null;default:0" json:"likes_count"`

	// CommentCount 评论数。**评论模块尚未实现,这个值没有任何写入路径,恒为 0**。
	//
	// 列、DTO、以及三处映射都已就位(本结构体、video.VideoResp、feed.FeedItem),
	// 所以 API 响应里会带一个恒为 0 的 comment_count —— 前端拿它显示「0 条评论」
	// 与真实的「还没有评论功能」是两回事,接前端时注意别把它当成已实现的功能。
	//
	// 与 LikesCount / PlayCount 的区别就在这里:后两者都有真实写入路径
	// (LikeVideo / UnlikeVideo 在事务里自增,ReportPlay 累加),只有它是空壳。
	CommentCount int64 `gorm:"not null;default:0" json:"comment_count"`

	Popularity int64 `gorm:"not null;default:0" json:"popularity"` // 热度
}

// PlayRecord 播放流水:每次播放插一条,只增不改。
// 与 videos.play_count 分工:后者是聚合总数,读列表直接用;本表是原始明细,
// 用于完播率分析、防刷风控、后续推荐的行为数据。纯追加表,无 UpdatedAt,不软删除
//
// 索引同样不在 tag 里声明,见 migrations/004_create_play_records.sql:idx_play_user_time
type PlayRecord struct {
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID    int64     `gorm:"not null"                 json:"user_id"` // 0 表示未登录游客
	VideoID   int64     `gorm:"not null"                 json:"video_id"`
	AuthorID  int64     `gorm:"not null"                 json:"author_id"` // 冗余,便于按作者聚合免 JOIN
	Watched   int       `gorm:"not null;default:0"       json:"watched"`   // 实际观看秒数
	Duration  int       `gorm:"not null;default:0"       json:"duration"`  // 冗余,用于算完播率
	IP        string    `gorm:"type:varchar(45)"         json:"ip"`        // IPv6 最长 45 字符
	CreatedAt time.Time `gorm:"type:timestamp;not null" json:"created_at"`
}

// VideoLike 表示用户当前对视频的一次点赞。
// 取消点赞时删除记录;同一用户对同一视频只能有一条记录,由数据库唯一约束保证。
// 表结构与索引定义见 migrations/007_create_video_likes.sql。
type VideoLike struct {
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID    int64     `gorm:"not null"                 json:"user_id"`
	VideoID   int64     `gorm:"not null"                 json:"video_id"`
	CreatedAt time.Time `gorm:"type:timestamp;not null" json:"created_at"`
}
