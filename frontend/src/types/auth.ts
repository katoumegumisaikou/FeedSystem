// 与后端 Go DTO 对齐的类型定义(backend/internal/model/account/dto.go)

// 后端统一响应格式
export interface ApiResp<T = unknown> {
  code: number;
  msg: string;
  data: T;
}

export interface RegisterReq {
  user_name: string;
  password: string;
  phone: string;
  email?: string;
}

export interface LoginReq {
  phone: string;
  password: string;
}

export interface ChangePasswordReq {
  phone: string;
  sms_code: string;
  password: string;
}

export interface SendSmsCodeReq {
  phone: string;
}

// dev 模式会把验证码放进 data.sms_code
export interface SendSmsCodeResp {
  sms_code?: string;
}

export interface LoginSuccessResp {
  logged_in: boolean;
}

// 后端 UserResp,见 backend/internal/model/account/dto.go。
// Go 侧 Email/AvatarURL/LastLoginAt 带 omitempty,值为空时字段整个从 JSON 消失,故 TS 用可选。
export interface UserResp {
  id: number;
  user_name: string;
  phone: string;
  email?: string;
  avatar_url?: string;
  created_at: string; // ISO 8601 UTC,如 "2026-09-12T16:50:53.025052Z"
  last_login_at?: string;
}

// 后端 PublicUserResp(GET /users/:id)不在这个文件里 —— 它是**别人的公开资料**,与"我自己的"
// UserResp 是两个视图(见 types/user.ts 的说明)。这里原来重复声明过一份,没有人用,已删:
// 同一个名字有两处定义,改一处忘一处就是那种说不清哪个是真的错。

// 后端错误码常量(对应 errs.ServiceErr.Code)
export const ErrCode = {
  InvalidParam: 40001,
  Unauthorized: 40101,
  NotFound: 40401,
  Conflict: 40901,
  TooFrequent: 42901,
  Internal: 50001,
} as const;