// 与后端 feed 模块 DTO 对齐(backend/internal/model/feed/dto.go)。
// 注意:这是"视频流"里的条目(FeedItem),和 video 模块的 VideoResp 字段高度重合,
// 但多一个 published_at(首次发布时刻),所以单独建类型,不硬复用 VideoResp。

// 视频流里的一个视频条目,对应后端 FeedItem。
export interface FeedItem {
  id: number;
  author_id: number;
  username: string;
  avatar_url?: string;
  title: string;
  description?: string;
  play_url: string; // 站内相对路径 /videos/{authorID}/{fileName}
  cover_url: string; // 站内相对路径 /covers/{videoID}/{fileName}
  created_at: string;
  published_at?: string; // 首次发布时刻;只有关注流当排序依据
  status: number; // 见 types/video.ts 的 VideoStatus
  play_count: number;
  likes_count: number;
  comment_count: number;
  is_like?: boolean; // nil 表示未登录或未计算
}

// 流的分页游标,对应后端 FeedCursor。
// 复合 (created_at, video_id):同一微秒发布的视频只按时间翻页会漏/重。
// created_at 是 Unix 微秒(不是毫秒),前端只原样回传,不换算。
export interface FeedCursor {
  created_at: number; // Unix 微秒
  video_id: number;
}

// 最新视频流响应,对应后端 ListLatestResp。next_cursor 为空即到底。
export interface ListLatestResp {
  items: FeedItem[];
  next_cursor?: FeedCursor;
}

// 热门(点赞)流的游标,对应后端 LikeCursor。
// 排序键是 likes_count,不是 created_at —— 形状和 FeedCursor 不通用,所以另立一个类型。
export interface LikeCursor {
  likes_count: number;
  video_id: number;
}

// 热门(点赞)流响应,对应后端 ListLikeResp。next_cursor 为空即到底。
export interface ListLikeResp {
  items: FeedItem[];
  next_cursor?: LikeCursor;
}

// 关注流响应,对应后端 ListFollowingResp。
// 游标形状与最新流完全相同,复用 FeedCursor —— 但注意它的 created_at 字段承载的
// 是发布时刻 published_at(收件箱 ZSET 的 score),不是上传时刻。
//
// ⚠️ 判断「是否到底」只能看 next_cursor 为空,**绝不能看 items.length**:
// items 比 limit 短(甚至是空数组)都是合法情况 —— 本页候选被截断、窗口内条目已失效
// (视频下架/作者取关)时,游标之后确实还有内容。只看长度会提前停止并漏掉后面的内容。
// 注意别用 `=== null` 判空:后端 Go 侧是 `*FeedCursor` + `omitempty`,nil 时整个
// 键不发出来,前端拿到的是 undefined。统一用 `!next_cursor` 最稳。
export interface ListFollowingResp {
  items: FeedItem[];
  next_cursor?: FeedCursor;
}
