import { request } from './axios';
import type { ListUserVideosResp, UserVideoCursor } from '../types/video';

// 某人的视频列表,路径对齐 backend/internal/model/video/router.go 的 GET /users/:id/videos。
//
// 游标分页:不传 cursor 拿第一页;拿到响应后把 next_cursor 再传回来拿下一页,
// 直到 next_cursor 为空。注意后端两个游标参数是 cursor_created_at / cursor_video_id,
// **必须成对传** —— 只带一个会被判「查询参数无效」。
//
// 这里把两个字段的拼装收在函数内部,调用方只需要传一个 UserVideoCursor 对象,
// 不用关心后端 query 参数叫什么 —— 后端改名时只改这一处。
export function listUserVideos(
  userId: number,
  cursor?: UserVideoCursor,
  limit = 20,
): Promise<ListUserVideosResp> {
  return request<ListUserVideosResp>({
    url: `/users/${userId}/videos`,
    method: 'get',
    // axios 的 params 会拼成 ?limit=…&cursor_created_at=…&cursor_video_id=…,
    // 值为 undefined 的键会被自动省略 —— 所以首页(cursor 为空)不会带上空游标参数
    params: {
      limit,
      cursor_created_at: cursor?.created_at,
      cursor_video_id: cursor?.video_id,
    },
  });
}
