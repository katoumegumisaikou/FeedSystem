// 与后端 PublicUserResp 对齐(backend/internal/model/account/dto.go)。
//
// 为什么不用 types/auth.ts 里的 UserResp?两者是**两个不同的视图**:
//   - UserResp       = 我自己的资料,含 phone / email / last_login_at
//   - PublicUserResp = 别人的公开资料,后端故意不返回联系方式与「最近登录时间」
//     (活跃时段也是隐私),所以这里只有 id / user_name / avatar_url / created_at。
// 用一个类型硬塞两处,会让「哪些字段真的拿得到」变得含糊 —— 分开写更诚实。
export interface PublicUserResp {
  id: number;
  user_name: string;
  // 后端带 omitempty:没设头像时字段整个从 JSON 消失,所以 TS 侧是可选
  avatar_url?: string;
  created_at: string; // ISO 8601 UTC,如 "2026-09-12T16:50:53.025052Z"
}
