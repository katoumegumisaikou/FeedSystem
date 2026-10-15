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
