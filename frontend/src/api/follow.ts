import { request } from './axios';
import type { FollowResp, ListFolloweesResp } from '../types/follow';

// 关注关系 API,路径对齐 backend/internal/model/follow/router.go。
//
// 三条都**需登录**(后端挂了 SetSensitive,无 token 直接 401)。
// request<T> 返回的是业务数据 T(拦截器已解包 {code,msg,data}),不是 AxiosResponse。

// 我关注的所有人。关注数有 MaxFollowees 封顶,所以整份一次返回,没有分页。
//
// silent:这是**后台读取** —— useFollowees 在每条 rail 挂载时自动触发它。游客进首页
// (spec §11:游客能看视频)必然拿 401,不静默的话开一次首页就弹一次"请先登录",
// 而他什么都没点。该提示的是"点关注"这个动作,不是页面加载(见下面两条)。
// 代价:已登录用户遇到网络错误时也不会弹 —— 界面停在"关注"这个保守态,不误导。
//
// ⚠️ 调用点只有 useFollowees,由它拉一次并缓存:**别按视频逐条调用** ——
// 一次响应就是整份列表,首页十几个关注按钮各拉一次就是把同一个接口打十几遍。
export function listFollowees(): Promise<ListFolloweesResp> {
  return request<ListFolloweesResp>({ url: '/users/me/followees', method: 'get', silent: true });
}

// 关注。响应里的 following 是操作后的真值(本任务不用 follower_count:不显示粉丝数)。
//
// 这里**不** silent:这是用户主动点的动作,失败必须说话(游客点关注 → "请先登录",
// spec §11),否则按钮看起来坏了。
export function followUser(id: number): Promise<FollowResp> {
  return request<FollowResp>({ url: `/users/${id}/follow`, method: 'post' });
}

// 取关。与关注同一路径,方法不同。
export function unfollowUser(id: number): Promise<FollowResp> {
  return request<FollowResp>({ url: `/users/${id}/follow`, method: 'delete' });
}
