// 与后端 follow 模块 DTO 对齐(backend/internal/model/follow/dto.go)。

// 关注 / 取关的结果。following 是**操作之后**的真值 —— 前端直接采用它,不自己取反:
// 关注数有上限(MaxFollowees),服务端可能拒绝(回 false),本地取反就会把
// "其实没关注上"显示成"已关注"。
export interface FollowResp {
  following: boolean;
  follower_count: number;
}

// 我关注的人。关注数有上限(MaxFollowees),整份列表一次就能给全 —— 这正是前端
// 能拿它判断"我是否关注了某人"的前提(后端没有单个人的已关注读接口)。
export interface ListFolloweesResp {
  followee_ids: number[];
  // 关注数上限。后端一起返回,前端不必硬编码阈值(阈值改了前端不用跟着发版)。
  limit: number;
}
