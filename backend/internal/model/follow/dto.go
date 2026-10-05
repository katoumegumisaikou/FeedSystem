package follow

// FollowResp 关注 / 取关的结果。
type FollowResp struct {
	Following     bool  `json:"following"`      // 操作之后是否处于已关注状态
	FollowerCount int64 `json:"follower_count"` // 对方(被关注者)当前的粉丝数
}

// ListFolloweesResp 我关注的人。
type ListFolloweesResp struct {
	FolloweeIDs []int64 `json:"followee_ids"`
	// Limit 关注数上限(MaxFollowees)。一起返回是为了让前端在 len(FolloweeIDs) 触到它时
	// 置灰「关注」按钮,而不必在客户端硬编码这个阈值 —— 阈值改了前端不用跟着发版
	Limit int `json:"limit"`
}
