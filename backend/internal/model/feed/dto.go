package feed

import (
	"time"
)

// ListLatestReq 最新视频流查询参数(GET /videos/latest)。
//
// 游标是复合的 (created_at, id):只带时间戳的话,同一时刻发布的视频没有确定顺序,
// 翻页会漏条或重复。带上 id 兜底,边界就从「某个时刻」变成「某条记录之前」。
//
// 时间戳用微秒,和 videos.created_at(TIMESTAMP,微秒精度)齐平 —— 必须能精确还原
// 库里的值,否则 id 那个兜底条件的等号永远不成立,等于没加。
// 微秒时间戳现在约 1.79e15,离 JS 的 MAX_SAFE_INTEGER(9.007e15)还有 5 倍余量
type ListLatestReq struct {
	// Limit 每页条数。0 由 service 补默认值。
	Limit int `form:"limit" binding:"omitempty,min=1,max=100"`
	// CursorCreatedAt 上一页最后一条的 created_at(Unix 微秒)。首页不传
	CursorCreatedAt *int64 `form:"cursor_created_at" binding:"omitempty,min=0"`
	// CursorVideoID 上一页最后一条的视频 ID。首页不传;必须和 CursorCreatedAt 同时给
	CursorVideoID *int64 `form:"cursor_video_id" binding:"omitempty,min=1"`
}

// FeedItem 最新视频流中的视频卡片。
type FeedItem struct {
	ID          int64     `json:"id"`
	AuthorID    int64     `json:"author_id"`
	Username    string    `json:"username"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	PlayURL     string    `json:"play_url"`
	CoverURL    string    `json:"cover_url"`
	CreatedAt   time.Time `json:"created_at"`
	// PublishedAt 首次发布的时刻。关注流按它排序,所以只有关注流把它当排序依据;
	// 最新流/点赞流仍按 created_at 排,不用它。created_at 是上传时刻,长期草稿会
	// 明显早于 published_at,两者不可混用
	PublishedAt  *time.Time `json:"published_at,omitempty"`
	Status       int8       `json:"status"`
	PlayCount    int64      `json:"play_count"`
	LikesCount   int64      `json:"likes_count"`
	CommentCount int64      `json:"comment_count"`
	IsLike       *bool      `json:"is_like,omitempty"` // nil 表示未登录或未计算;非 nil 时表示当前用户是否点赞
}

// FeedCursor 流的下一页游标(JSON 的 created_at 字段承载排序键的 Unix 微秒)。
//
// 两个字段必须一起带回来:只带排序键的话,同值的视频会漏或重复。
// 关注流里 created_at 字段承载的其实是 published_at(ZSET score),字段名沿用
// 是为了保持接口形状与最新流一致
type FeedCursor struct {
	CreatedAt int64 `json:"created_at"` // Unix 微秒;关注流里承载 published_at
	VideoID   int64 `json:"video_id"`
}

// ListLatestResp 最新视频流响应。
type ListLatestResp struct {
	// Items service 保证非 nil,空页也返回 [] 而不是 null。
	Items []FeedItem `json:"items"`
	// NextCursor 下一页游标。只在确实还有下一页时才给,到底了是 null ——
	// 前端可以直接拿它决定「加载更多」显不显示,不用再多请求一次空页来确认
	NextCursor *FeedCursor `json:"next_cursor,omitempty"`
}

// ListLikeReq 按点赞数排序的视频流请求。
// 首页不传游标;翻页时两个游标字段需同时传。
type ListLikeReq struct {
	Limit            int    `form:"limit" binding:"omitempty,min=1,max=100"`
	CursorLikesCount *int64 `form:"cursor_likes_count" binding:"omitempty,min=0"`
	CursorVideoID    *int64 `form:"cursor_video_id" binding:"omitempty,min=1"`
}

// LikeCursor 是视频流的下一页游标。
type LikeCursor struct {
	LikesCount int64 `json:"likes_count"`
	VideoID    int64 `json:"video_id"`
}

// ListLikeResp 按点赞数排序的视频流响应。
type ListLikeResp struct {
	Items []FeedItem `json:"items"`
	// nil 表示没有更多数据。
	NextCursor *LikeCursor `json:"next_cursor,omitempty"`
}

// ListFollowingReq 关注流查询参数(GET /videos/following)。
//
// 游标与最新流同构:复合 (score, id)。但这里的 score 是发布时刻 published_at
// (收件箱 ZSET 的 score),不是 created_at;字段名沿用 cursor_created_at 只为保持
// 接口形状,其值实际承载的是 ZSET score。不用 last_login_at 当游标 ——
// 它是用户维度的时间戳,不在结果集里,而且每次登录都会被改写,
// 翻页途中换设备登录就会把边界推到未来,已翻/未翻的页全部错位
type ListFollowingReq struct {
	// Limit 每页条数,最大 100。收件箱只保留 inboxCap=50 条,所以只关注非大V
	// 作者的读者最多只能拿到 50 条 —— 这是有意的保留,不是上限设错;上限留宽
	// 是因为叠加多个大V 的拉模式缓存后一页可以更长。
	Limit int `form:"limit" binding:"omitempty,min=1,max=100"`
	// CursorCreatedAt 上一页最后一条的排序键(发布时刻,ZSET score 的 Unix 微秒)。首页不传
	CursorCreatedAt *int64 `form:"cursor_created_at" binding:"omitempty,min=0"`
	// CursorVideoID 上一页最后一条的视频 ID。首页不传;必须和 CursorCreatedAt 同时给
	CursorVideoID *int64 `form:"cursor_video_id" binding:"omitempty,min=1"`
}

// ListFollowingResp 关注流响应。游标复用 FeedCursor —— 形状与最新流完全相同,
// 不另起一个类型。注意其 CreatedAt 字段承载的是发布时刻(ZSET score),见 FeedCursor
//
// ⚠️ 接口契约:判断「还有没有更多」只能看 NextCursor 是否为 null,绝不能看
// len(Items) 是否等于 limit。Items 少于 limit 是合法情况,出现在本页候选被
// followingCandidateCap 截断、且窗口内多数条目已失效(视频下架/作者取关)时 ——
// 此时游标之后确实还有内容,必须继续翻页;只看长度就会提前停止,把后面的内容漏掉。
// 反过来 Items 为空也不是结束信号:它同样可能伴随非 null 游标(整窗口都是失效条目)。
// 换句话说,Items 的长度与「是否到底」无关,NextCursor 才是唯一判据。
type ListFollowingResp struct {
	// Items 本页视频卡片。长度可能小于请求的 limit(甚至是空数组),这不代表到底 ——
	// 只表示本页窗口内可用条目较少。是否结束请看 NextCursor。
	Items []FeedItem `json:"items"`
	// NextCursor 下一页游标。null 表示确实到底;非 null 时即使 Items 很短甚至为空,
	// 也必须带着它继续请求下一页。
	NextCursor *FeedCursor `json:"next_cursor,omitempty"`
}
