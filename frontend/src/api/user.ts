import { request } from './axios';
import type { PublicUserResp } from '../types/user';
import type { UserResp } from '../types/auth';

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

export function getMyProfile():Promise<UserResp>{
  return request<UserResp>({ url: `/users/me`, method: 'get' });
}