import { request } from './axios';
import type { PublicUserResp } from '../types/user';

// 查他人公开资料,路径对齐 backend/internal/model/account/router.go 的 GET /users/:id。
//
// 这条路由**不需要登录**(游客点开作者主页不该被要求登录),后端靠三点保安全:
//   1. 只返回 PublicUserResp,不含手机号 / 邮箱 / 最近登录时间
//   2. 按 IP 限流 60/min,防止按 ID 遍历拖库
// 所以前端也不需要做登录校验,直接调。
//
// request<T> 已被 axios 拦截器解包 {code,msg,data},返回的是 data 本身,别再访问 .data。
export function getUserProfile(id: number): Promise<PublicUserResp> {
  return request<PublicUserResp>({ url: `/users/${id}`, method: 'get' });
}

// 这里原来还有一个 getMyProfile():**没有人 import 它**,而且它调的是 /users/me 却**没有 silent**
// 开关 —— 真正在用的那一份是 api/auth.ts 的 getMyProfile({ silent })(两处探测登录态都靠它静默失败)。
// 两个同名函数里更弱的那一个留着只会被误用,所以删掉。