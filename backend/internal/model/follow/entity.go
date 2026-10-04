package follow

import "time"

// Follow 关注关系。纯关系表,没有独立主键 —— (follower_id, followee_id) 就是主键。
//
// 只有 CreatedAt,没有 UpdatedAt,也不软删除:取关就是物理 DELETE,一行要么
// 存在(关注中)要么不存在,不需要「已取关」这种中间态。
//
// 索引不在 gorm tag 里声明 —— 本项目建表全走 goose 迁移、不用 AutoMigrate,
// tag 里的索引不生效,写了就是第二份会漂移的真相。真实定义见
// migrations/009_create_follows.sql:复合主键兼作去重约束,
// idx_follows_follower · idx_follows_followee 两个反向索引
type Follow struct {
	FollowerID int64     `gorm:"primaryKey" json:"follower_id"`
	FolloweeID int64     `gorm:"primaryKey" json:"followee_id"`
	CreatedAt  time.Time `gorm:"type:timestamp;not null" json:"created_at"`
}

func (Follow) TableName() string { return "follows" }
