import { request } from './axios';
import type { FeedCursor, LikeCursor, ListFollowingResp, ListLatestResp, ListLikeResp } from '../types/feed';

// 最新视频流,路径对齐 backend/internal/model/feed/router.go 的 GET /videos/latest。
//
// 游客可看(软鉴权),带 cookie 时后端会额外带上"我是否点过赞"(is_like)。
// 游标分页:不传 cursor 拿第一页;next_cursor 为空即到底。
export function listLatestVideos(cursor?: FeedCursor, limit = 10): Promise<ListLatestResp> {
  return request<ListLatestResp>({
    url: '/videos/latest',
    method: 'get',
    // 两个游标参数必须成对传;undefined 会被 axios 自动省略(首页不带游标)
    params: {
      limit,
      cursor_created_at: cursor?.created_at,
      cursor_video_id: cursor?.video_id,
    },
  });
}

// 热门(点赞)流,路径对齐 backend/internal/model/feed/router.go 的 GET /videos/like。
//
// 游客可看,带 cookie 时后端会额外带上"我是否点过赞"(is_like)。
// 游标是 (likes_count, video_id) —— 排序键是点赞数,和最新流的 (created_at, video_id)
// 不是一回事,所以不能和 listLatestVideos 共用一个函数。首页不传游标。
export function listHotVideos(cursor?: LikeCursor, limit = 10): Promise<ListLikeResp> {
  return request<ListLikeResp>({
    url: '/videos/like',
    method: 'get',
    // 两个游标参数必须成对传;undefined 会被 axios 自动省略(首页不带游标)
    params: {
      limit,
      cursor_likes_count: cursor?.likes_count,
      cursor_video_id: cursor?.video_id,
    },
  });
}

// 关注流,路径对齐 backend/internal/model/feed/router.go 的 GET /videos/following。
//
// 强制登录:后端挂了 SetSensitive,无 token 直接 401。
// 游标形状与最新流同构,但 created_at 字段承载的是发布时刻(ZSET score),不是上传时刻。
//
// ⚠️ 是否到底只看响应里的 next_cursor 是否为空,**绝不能看 items.length** ——
// items 比 limit 短甚至为空,都可能是"游标之后仍有内容"。这条判断落在 Task 8/10。
// 判空用 `!next_cursor`:后端 Go 侧是 `omitempty`,到底时该键整个不发出来(undefined)。
export function listFollowingVideos(cursor?: FeedCursor, limit = 10): Promise<ListFollowingResp> {
  return request<ListFollowingResp>({
    url: '/videos/following',
    method: 'get',
    // 两个游标参数必须成对传;undefined 会被 axios 自动省略(首页不带游标)
    params: {
      limit,
      cursor_created_at: cursor?.created_at,
      cursor_video_id: cursor?.video_id,
    },
  });
}
