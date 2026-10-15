import { request } from './axios';
import type { FeedCursor, ListLatestResp } from '../types/feed';

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
