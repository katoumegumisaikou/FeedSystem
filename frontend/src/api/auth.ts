import { request } from './axios';
import type {
  ChangePasswordReq,
  LoginReq,
  LoginSuccessResp,
  RegisterReq,
  SendSmsCodeReq,
  SendSmsCodeResp,
  UserResp,
} from '../types/auth';

// 账号相关 API,路径对齐 backend/internal/model/account/router.go。
// token 由后端通过 HttpOnly cookie 下发,前端不存 localStorage(withCredentials 已在 axios 实例开启);
// request<T> 返回的是业务数据 T(拦截器已解包 {code,msg,data}),不是 AxiosResponse,别访问 .data/.status。

export function register(req: RegisterReq): Promise<LoginSuccessResp> {
  return request<LoginSuccessResp>({ url: '/auth/register', method: 'post', data: req });
}

export function login(req: LoginReq): Promise<LoginSuccessResp> {
  return request<LoginSuccessResp>({ url: '/auth/login', method: 'post', data: req });
}

// 后端从 cookie 读 token
export function logout(): Promise<{ logged_out: boolean }> {
  return request<{ logged_out: boolean }>({ url: '/auth/logout', method: 'post' });
}

// 需登录
export function getMyProfile(): Promise<UserResp> {
  return request<UserResp>({ url: '/users/me', method: 'get' });
}

// dev 模式下返回的 data.sms_code 有值,方便调试
export function sendSmsCode(req: SendSmsCodeReq): Promise<SendSmsCodeResp> {
  return request<SendSmsCodeResp>({ url: '/auth/sms-code', method: 'post', data: req });
}

// SMS 验证码流程,无需登录态
export function changePassword(req: ChangePasswordReq): Promise<null> {
  return request<null>({ url: '/auth/password', method: 'put', data: req });
}
