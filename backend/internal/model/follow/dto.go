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

// ListFollowersReq 谁关注了我(GET /users/me/followers)的查询参数。
//
// 必须分页:关注数有 MaxFollowees 封顶(关注列表能一次返回),但粉丝数没有上界 ——
// 一个大V 可能有几万名粉丝,整份返回会把响应体和服务端内存都撑爆
type ListFollowersReq struct {
	// Limit 每页条数,0 由 service 补默认值
	Limit int `form:"limit" binding:"omitempty,min=1,max=100"`
	// Cursor 上一页最后一条的 follower_id。首页不传
	Cursor *int64 `form:"cursor" binding:"omitempty,min=1"`
}

// ListFollowersResp 我的粉丝列表一页。
//
// 用 NextCursor 而非 has_more 决定要不要继续翻:与视频流同一套判据,
// next_cursor 为 null 即到底,前端不必多请求一次空页来确认
type ListFollowersResp struct {
	// FollowerIDs 本页粉丝 ID,升序。service 保证非 nil(空页返回 [] 而不是 null)
	FollowerIDs []int64 `json:"follower_ids"`
	// NextCursor 下一页游标(上一页最后一条的 follower_id)。到底了是 null
	NextCursor *int64 `json:"next_cursor,omitempty"`
}
