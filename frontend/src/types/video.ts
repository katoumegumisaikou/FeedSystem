// 与后端 video 模块 DTO 对齐(backend/internal/model/video/dto.go)。

// 视频状态。后端是 int8,这里用常量对象给它起名字,
// 代码里写 VideoStatus.Draft 比写 4 可读得多(也和 types/auth.ts 里 ErrCode 的做法一致)。
export const VideoStatus = {
  Processing: 0, // 转码中
  Published: 1, // 已发布
  Failed: 2, // 转码失败
  Removed: 3, // 已下架
  Draft: 4, // 草稿(只有作者本人看得到)
} as const;

// 一条视频卡片,对应后端 VideoResp。
// is_like 在 Go 里是 *bool:匿名访问或服务端没算时为 nil,JSON 里字段消失 → TS 用可选。
export interface VideoResp {
  id: number;
  author_id: number;
  username: string;
  avatar_url?: string;
  title: string;
  description?: string;
  play_url: string; // 站内相对路径 /videos/{authorID}/{fileName}
  cover_url: string; // 站内相对路径 /covers/{videoID}/{fileName}
  created_at: string;
  status: number; // 见 VideoStatus
  play_count: number;
  likes_count: number;
  comment_count: number;
  is_like?: boolean;
}

// 某人视频列表的下一页游标,对应后端 UserVideoCursor。
//
// 注意后端用的是**复合游标** (created_at, video_id) 而不是单一时间戳:
// 同一微秒上传的两条记录,只按时间翻页会漏条或重复,带上 video_id 才能定出确定顺序。
// created_at 单位是 Unix **微秒**(和后端库里的 TIMESTAMP 精度齐平),不是毫秒 ——
// 前端只是原样回传,不要自己换算。
export interface UserVideoCursor {
  created_at: number; // Unix 微秒
  video_id: number;
}

// 某人一页视频,对应后端 ListUserVideosResp。
// 没有 has_more 字段:next_cursor 为空即到底,前端不用多请求一次空页来确认。
export interface ListUserVideosResp {
  items: VideoResp[];
  next_cursor?: UserVideoCursor;
}

// 点赞 / 取消点赞的结果,对应后端 LikeResp。
// 两个字段都回:前端乐观更新后要用服务端真值纠正 —— 只回 bool 的话,
// 并发点赞导致的计数变化就反映不出来
export interface LikeResp {
  is_like: boolean;
  likes_count: number;
}
