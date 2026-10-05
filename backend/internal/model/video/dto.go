package video

import "time"

// UpdateVideoReq 编辑视频元数据(PUT /videos/:id)。
//
// 不含 PlayURL:播放地址由服务端在合并分片时推导,让客户端传就等于允许它
// 指向别人的文件或外站资源。
//
// CoverURL 不加 binding:"url" —— 它存的是站内相对路径(/covers/xx.png),
// url 规则会直接判非法;格式改由 service 层校验前缀
type UpdateVideoReq struct {
	Title       string `json:"title"       binding:"required,max=255"`
	Description string `json:"description" binding:"max=1000"`
	CoverURL    string `json:"cover_url"   binding:"omitempty,max=255"` // 留空表示不改动现有封面
}

// PlayReportReq 播放上报请求(POST /videos/:id/play)。
//
// 不含 video_id:ID 走路径参数。路径和 body 都能带的话,两者不一致时以谁为准
// 就成了 bug 温床 —— 详情 / 编辑 / 发布也都是从路径取 ID。
//
// 两个字段都要 min=0:不卡的话前端传负数能过,完播率会算出负值
type PlayReportReq struct {
	Watched  int `json:"watched"  binding:"min=0"` // 实际观看秒数
	Duration int `json:"duration" binding:"min=0"` // 总时长(秒),用于算完播率
}

// VideoResp 视频视图
// 显式列出字段:entity 里的 DeletedAt 与 Popularity(内部排序分)不对外暴露
type VideoResp struct {
	ID           int64     `json:"id"`
	AuthorID     int64     `json:"author_id"`
	Username     string    `json:"username"`
	AvatarURL    string    `json:"avatar_url,omitempty"`
	Title        string    `json:"title"`
	Description  string    `json:"description,omitempty"`
	PlayURL      string    `json:"play_url"`
	CoverURL     string    `json:"cover_url"`
	CreatedAt    time.Time `json:"created_at"`
	Status       int8      `json:"status"` // 状态(0 转码中 / 1 已发布 / 2 转码失败 / 3 已下架 / 4 草稿)
	PlayCount    int64     `json:"play_count"`
	LikesCount   int64     `json:"likes_count"`
	CommentCount int64     `json:"comment_count"`
	IsLike       *bool     `json:"is_like,omitempty"` // nil 表示匿名或未计算
}

// LikeResp 点赞 / 取消点赞的结果。
//
// 两个字段都返回是因为点赞是个「开关」动作:前端做乐观更新时会先翻转图标,
// 拿到响应后要用服务端的真值纠正 —— 只回 bool 的话,并发点赞导致的计数变化就反映不出来。
// LikesCount 是事务内读到的最新值,而不是「旧值 ± 1」的估算
type LikeResp struct {
	IsLike     bool  `json:"is_like"`     // 操作后是否处于已点赞状态
	LikesCount int64 `json:"likes_count"` // 操作后的点赞总数
}

// ListUserVideosReq 某人的视频列表查询参数(GET /users/:id/videos)。
//
// 游标是复合的 (created_at, video_id)。只带时间戳的话,同一微秒上传的两条没有
// 确定顺序,翻页会漏条或重复;带上 video_id 兜底,边界就从「某个时刻」变成
// 「某条记录之前」。时间用微秒,与 videos.created_at(TIMESTAMP 微秒)齐平 ——
// 必须能精确还原库里的值,否则 video_id 那个兜底条件的等号永远不成立,等于没加
type ListUserVideosReq struct {
	// Limit 每页条数。0 由 service 补默认值;max=100 卡住「一次拉全表」
	Limit int `form:"limit" binding:"omitempty,min=1,max=100"`
	// CursorCreatedAt 上一页最后一条的 created_at(Unix 微秒)。首页不传
	CursorCreatedAt *int64 `form:"cursor_created_at" binding:"omitempty,min=0"`
	// CursorVideoID 上一页最后一条的视频 ID。首页不传;必须和 CursorCreatedAt 同时给
	CursorVideoID *int64 `form:"cursor_video_id" binding:"omitempty,min=1"`
}

// UserVideoCursor 某人视频列表的下一页游标。
//
// 两个字段必须一起带回来:只带 created_at 的话,同一微秒上传的两条会漏或重复
type UserVideoCursor struct {
	CreatedAt int64 `json:"created_at"` // Unix 微秒
	VideoID   int64 `json:"video_id"`
}

// ListUserVideosResp 某人一页视频。和最新流一样不带 has_more:next_cursor 为 null 即到底
type ListUserVideosResp struct {
	// Items 视频卡片,按上传时间倒序。service 保证非 nil(空页返回 [] 而不是 null)
	Items []VideoResp `json:"items"`
	// NextCursor 只在确实还有下一页时才给;到底了是 null
	NextCursor *UserVideoCursor `json:"next_cursor,omitempty"`
}

// ListHistoryReq 观看历史查询参数(GET /videos/history)。
//
// 游标是复合的 (watched_at, video_id):只带时间戳的话,同一时刻的两条没有确定顺序,
// 翻页会漏条或重复。带上 video_id 兜底,边界就从「某个时刻」变成「某条记录之前」。
//
// 时间用微秒,和 play_records.created_at(TIMESTAMP,微秒精度)齐平 —— 必须能精确
// 还原库里的值,否则 video_id 那个兜底条件的等号永远不成立,等于没加
type ListHistoryReq struct {
	// Limit 每页条数。0 由 service 补默认值;max=100 卡住「一次拉全表」
	Limit int `form:"limit" binding:"omitempty,min=1,max=100"`
	// CursorWatchedAt 上一页最后一条的 watched_at(Unix 微秒)。首页不传
	CursorWatchedAt *int64 `form:"cursor_watched_at" binding:"omitempty,min=0"`
	// CursorVideoID 上一页最后一条的视频 ID。首页不传;必须和 CursorWatchedAt 同时给
	CursorVideoID *int64 `form:"cursor_video_id" binding:"omitempty,min=1"`
}

// HistoryItem 观看历史里的一条:视频卡片 + 最近一次观看的进度。
//
// 内嵌 VideoResp 而不是再抄一遍字段 —— 前端拿到的就是它已经熟悉的卡片结构,
// 进度字段平铺在同一层,拼进度条不用往嵌套里翻
type HistoryItem struct {
	VideoResp
	Watched   int       `json:"watched"`    // 看到第几秒
	Duration  int       `json:"duration"`   // 视频总长,和 watched 一起算进度
	WatchedAt time.Time `json:"watched_at"` // 最近一次观看时间
}

// HistoryCursor 观看历史的下一页游标。
//
// 两个字段必须一起带回来:只带 watched_at 的话,同一微秒的两条会漏或重复
type HistoryCursor struct {
	WatchedAt int64 `json:"watched_at"` // Unix 微秒
	VideoID   int64 `json:"video_id"`
}

// ListHistoryResp 观看历史响应。和最新流一样不带 has_more:next_cursor 为 null 即到底
type ListHistoryResp struct {
	// Items 视频卡片,按最近观看时间倒序。service 保证非 nil(空页返回 [] 而不是 null)
	Items []HistoryItem `json:"items"`
	// NextCursor 下一页游标。只在确实还有下一页时才给,到底了是 null ——
	// 前端可以直接拿它决定「加载更多」显不显示
	NextCursor *HistoryCursor `json:"next_cursor,omitempty"`
}

type InitChunkUploadRequest struct {
	// max=255 对齐 videos.title 的 VARCHAR(255)(该值会当草稿标题写进库):不在这拦住,
	// 用户传完 1GB 才会在建视频行时撞上「value too long」。varchar 与 validator 的 max 都按 rune 计数
	Filename string `json:"filename" binding:"required,max=255"`
	FileSize int64  `json:"file_size" binding:"required,min=1"` // 单位 Byte
	// FileHash 客户端算好的 sha256 hex,不做秒传,只用于合并后校验完整性:服务端会再算一遍比对,对不上就丢弃整个上传。
	// hexadecimal 不能省 —— validator 的 len 数 rune 不数 byte,len=64 会放过 64 个中文字符,base64 规则也认不出 hex
	FileHash string `json:"file_hash" binding:"required,len=64,hexadecimal"`
}

// InitChunkUploadResp 分片上传初始化响应。不做秒传,没有「文件已存在、不用传」这条分支,每次 init 都返回新会话
type InitChunkUploadResp struct {
	UploadID    string `json:"upload_id"`          // 后续分片与合并都要带上
	ChunkSize   int64  `json:"chunk_size"`         // 每片字节数由服务端定;最后一片可能更小
	TotalChunks int    `json:"total_chunks"`       // 分片总数 = ceil(file_size / chunk_size)
	Uploaded    []int  `json:"uploaded,omitempty"` // 已存在的分片序号(0 起),用于断点续传
}

// UploadChunkRequest 描述一次分片上传请求。元数据走 multipart 表单字段,必须用 form tag ——
// gin 解析 multipart 读的是 form 不是 json,写成 json tag 会绑定不到、UploadID 恒为空,再被 required 判成"参数无效"。
// ChunkIndex 用 int 且不加 required:0 是合法序号,加了会把第 0 片误判成"字段缺失"
type UploadChunkRequest struct {
	UploadID   string `form:"upload_id"   binding:"required"`
	ChunkIndex int    `form:"chunk_index" binding:"min=0"` // 分片序号,从 0 开始
}

// UploadChunkResp 返回分片上传结果和当前上传进度。
type UploadChunkResp struct {
	UploadID      string `json:"upload_id"`
	ChunkIndex    int    `json:"chunk_index"`
	Uploaded      bool   `json:"uploaded"`
	Completed     bool   `json:"completed"` // 收齐后可以调 CompleteChunkUpload
	UploadedCount int    `json:"uploaded_count"`
	TotalChunks   int    `json:"total_chunks"`
}

// CompleteChunkUploadReq 合并分片、完成上传。只带 upload_id:文件地址由服务端推导,
// 不接受客户端传 —— 否则客户端能指向任意 URL,包括别人的文件和外站资源
type CompleteChunkUploadReq struct {
	UploadID string `json:"upload_id" binding:"required"` // init 返回的会话 ID
}

// CompleteChunkUploadResp 合并结果。返回 video_id 而非 URL:文件已落盘并建成草稿视频,
// 客户端拿 ID 去调编辑接口补标题和封面,全程不接触存储路径
type CompleteChunkUploadResp struct {
	VideoID  int64 `json:"video_id"`
	FileSize int64 `json:"file_size"` // 合并后的实际字节数,与声明对不上说明合并有问题
}
