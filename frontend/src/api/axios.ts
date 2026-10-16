import axios, { AxiosError, type AxiosRequestConfig, type AxiosResponse } from 'axios';
import type { MessageInstance } from 'antd/es/message/interface';
import type { ApiResp } from '../types/auth';

// 全局 message 实例:本模块是模块级代码、不在 React 组件树内,用不了 App.useApp(),
// 由根组件 App.tsx 挂载时注入。好处是能读到 ConfigProvider 的主题/语言包(静态 message 读不到)。
let globalMessage: MessageInstance | null = null;

export function setGlobalMessage(instance: MessageInstance) {
  globalMessage = instance;
}

// 在 axios 配置上扩展一个 silent 开关:带它的请求失败时,拦截器不弹错误提示。
// 用于"后台探测"类请求(比如判断有没有登录)—— 未登录时 401 不该打扰用户
export type RequestConfig = AxiosRequestConfig & { silent?: boolean };

// baseURL 用相对路径,开发环境由 vite.config.ts 代理到后端;
// withCredentials 让后端 SetTokenCookies 的 cookie 跨端口生效。
const http = axios.create({
  baseURL: '/api/v1',
  timeout: 10000,
  withCredentials: true,
});

// 「未登录」的业务码 = 后端 internal/pkg/errs/errs.go 的 ErrUnauthorized。
// 为什么要认识它:后端的错误有**两种**落地形态(见下面 isUnauthorized 的注释),
// 只认 HTTP 状态会漏掉一半。
const CODE_UNAUTHORIZED = 10002;

/** 业务错误的错误对象:把 body 里的 code 带在身上。
 *
 *  为什么非带不可:这类响应的 HTTP 状态是 200(见 backend/internal/pkg/response/response.go
 *  的 Error —— 它一律 c.JSON(http.StatusOK, ...)),所以光看状态码分不出"未登录"和
 *  "服务器内部错误",光看 msg 又是在猜文案。调用方要按码分类(isUnauthorized 认 10002),
 *  分类的依据就必须由这里递出去。
 *
 *  仍然是 Error 的子类:原来 `catch (e) { e.message }` / `rejects.toThrow(msg)` 的调用方一处不用改。 */
export class ApiError extends Error {
  readonly code: number;

  constructor(code: number, msg: string) {
    super(msg);
    this.name = 'ApiError';
    this.code = code;
  }
}

/** 这个失败是不是「未登录」?
 *
 *  两种形态都算:
 *    ① HTTP 401 —— 中间件直接拒掉(auth.go 的 abortUnauthorized,SetSensitive 路由无 token /
 *       token 失效都走它);
 *    ② 业务码 10002 —— handler / service 层返回 errs.ErrUnauthorized,经 response.Error 落成
 *       **HTTP 200 + body.code**(ApiError 那个 code)。
 *  其余(5xx / 网络失败 / 10s 超时 / 400)一律**不是**:把它们当成游客,会把一个已登录的人
 *  莫名送回登录页 —— 他看到的是一次无法解释的跳转,而且理由根本不存在。
 *
 *  判定收在这一处:守卫(router.tsx)、关注按钮(FollowButton.tsx)用的是同一个事实,
 *  各写各的就迟早会分叉。
 */
export function isUnauthorized(err: unknown): boolean {
  if (!err || typeof err !== 'object') return false;
  const e = err as { response?: { status?: number }; code?: unknown };
  return e.response?.status === 401 || e.code === CODE_UNAUTHORIZED;
}

// 拦截器解包后端统一格式 {code, msg, data}:成功返回 data,失败弹错并 reject
http.interceptors.response.use(
  (resp: AxiosResponse<ApiResp>) => {
    const body = resp.data;
    const silent = (resp.config as RequestConfig).silent;
    if (body && typeof body === 'object' && 'code' in body) {
      if (body.code === 0) {
        return body.data as unknown as AxiosResponse;
      }
      if (!silent) globalMessage?.error(body.msg || '请求失败');
      // 带上 code:调用方要按码分类(见 isUnauthorized),不能只给一句文案。
      return Promise.reject(new ApiError(body.code, body.msg || `code=${body.code}`));
    }
    // 兼容没有 code 的响应
    return resp;
  },
  (err: AxiosError<ApiResp>) => {
    const silent = (err.config as RequestConfig | undefined)?.silent;
    const msg = err.response?.data?.msg || err.message || '网络错误';
    if (!silent) globalMessage?.error(msg);
    return Promise.reject(err);
  },
);

// 返回业务数据 T。axios 拦截器的类型签名要求"进什么类型出什么类型",但这里已把
// {code, msg, data} 解包成 data,与 AxiosResponse 不符;把 as 强转收拢在此函数,
// 调用方(api/*.ts)才能拿到诚实的 T,避免"类型是 AxiosResponse、运行时是业务数据"的错位扩散。
export function request<T>(config: RequestConfig): Promise<T> {
  return http.request(config) as unknown as Promise<T>;
}

export default http;