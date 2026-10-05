package feed

import (
	"context"
	"time"

	"gorm.io/gorm"

	"feed-system/internal/model/video"
)

// FeedRepository 提供视频流所需的数据查询。
type FeedRepository interface {
	// ListLatestVideos 按 (created_at, id) 倒序取已发布视频。
	//
	// beforeID 为 0 表示首页,不设边界;否则只取严格排在 (before, beforeID) 之后的那批。
	// 用行值比较 (created_at, id) < (?, ?),和 ORDER BY 逐列对应,
	// 正好命中 008 迁移的 idx_videos_create_time_id。
	//
	// 只带 before 的话,同一时刻发布的视频没有确定顺序,翻页会漏条或重复
	ListLatestVideos(ctx context.Context, before time.Time, beforeID int64, limit int) ([]*video.Video, error)
	// ListVideosByLikes 按点赞数降序分页查询已发布视频。
	ListVideosByLikes(ctx context.Context, cursorLikesCount, cursorVideoID int64, limit int) ([]*video.Video, error)
	// ListVideosByIDs 按 ID 批量取视频,顺序不保证。用来把收件箱里的 video_id
	// 拼成卡片。只返回已发布且未软删的 —— 收件箱里可能残留后来被下架的视频。
	// 一次查完避免 N+1
	ListVideosByIDs(ctx context.Context, ids []int64) ([]*video.Video, error)

	// ListRecentPublishedVideoIDs 取某作者最近 n 条已发布视频的 (id, published_at)。
	// 新关注时补拉用。按 published_at 倒序 —— 补拉要用真实发布时刻当 score,
	// 用 created_at(上传时刻)会让长期草稿带着旧时刻补进来、被 7 天窗口裁掉。
	ListRecentPublishedVideoIDs(ctx context.Context, authorID int64, n int) ([]VideoRef, error)
}

// VideoRef 一条视频的最小引用:ID + 发布时刻。补拉时用来算 ZSET 的 score。
// 是 published_at 而非 created_at —— 后者是上传时刻,不可当排序键。
//
// PublishedAt 用指针:该列可空。虽然补拉查询带了 status = 1(本应排除草稿),但列
// 本身可空,历史遗留行或绕过发布路径写入的行都可能留下 NULL。用非指针 time.Time
// 扫 NULL 会让整条 Scan 直接报错、拖垮该作者的整次补拉 —— 所以按指针扫描,
// 由调用方逐条跳过 nil。
type VideoRef struct {
	ID          int64
	PublishedAt *time.Time
}

type feedRepository struct {
	db *gorm.DB
}

var _ FeedRepository = (*feedRepository)(nil)

func NewFeedRepository(db *gorm.DB) FeedRepository {
	return &feedRepository{db: db}
}

// ListLatestVideos 不加 deleted_at 条件:GORM 认出 video.Video 里嵌的 gorm.DeletedAt
// 会自动补上软删除过滤
func (r *feedRepository) ListLatestVideos(ctx context.Context, before time.Time, beforeID int64, limit int) ([]*video.Video, error) {
	query := r.db.WithContext(ctx).
		Where("status = ?", video.StatusPublished)
	// 行值比较:同一时刻发布的视频靠 id 决出先后,边界才没有缝
	if beforeID > 0 {
		query = query.Where("(created_at, id) < (?, ?)", before, beforeID)
	}

	var videos []*video.Video
	err := query.
		Order("created_at DESC").
		Order("id DESC").
		Limit(limit).
		Find(&videos).Error
	if err != nil {
		return nil, err
	}
	return videos, nil
}

func (r *feedRepository) ListVideosByLikes(ctx context.Context, cursorLikesCount, cursorVideoID int64, limit int) ([]*video.Video, error) {
	query := r.db.WithContext(ctx).Model(&video.Video{}).
		Where("status = ?", video.StatusPublished)
	if cursorVideoID > 0 {
		query = query.Where(
			"(likes_count < ? OR (likes_count = ? AND id < ?))",
			cursorLikesCount, cursorLikesCount, cursorVideoID,
		)
	}

	var videos []*video.Video
	err := query.
		Order("likes_count DESC").
		Order("id DESC").
		Limit(limit).
		Find(&videos).Error
	if err != nil {
		return nil, err
	}
	return videos, nil
}

// ListVideosByIDs 空切片直接返回:某些驱动的 IN () 是语法错误,和 video 包保持一致。
// 软删除过滤同样由 GORM 认出 video.Video 里嵌的 gorm.DeletedAt 自动补上
func (r *feedRepository) ListVideosByIDs(ctx context.Context, ids []int64) ([]*video.Video, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var videos []*video.Video
	err := r.db.WithContext(ctx).
		Where("id IN ?", ids).
		Where("status = ?", video.StatusPublished).
		Find(&videos).Error
	if err != nil {
		return nil, err
	}
	return videos, nil
}

// ListRecentPublishedVideoIDs 只取 id、published_at 两列,补拉用不到把十几列都拉回来。
// published_at 相同时按 id 兜底,顺序才确定。软删过滤由 GORM 按 video.Video 自动补。
//
// 关于 NULL:published_at 列可空,这里 Scan 进 VideoRef 的 *time.Time。status = 1 的
// 过滤「本应」排除草稿(草稿是 status = 4),011 迁移也回填过历史已发布行,但列本身可空 ——
// 历史遗留行、或绕过 MarkPublished 的写入仍可能留下 NULL。所以按指针扫描,由调用方跳过
// NULL,而不是让一条 NULL 打挂整次查询。
func (r *feedRepository) ListRecentPublishedVideoIDs(ctx context.Context, authorID int64, n int) ([]VideoRef, error) {
	var refs []VideoRef
	err := r.db.WithContext(ctx).Model(&video.Video{}).
		Select("id", "published_at").
		Where("author_id = ?", authorID).
		Where("status = ?", video.StatusPublished).
		Order("published_at DESC").
		Order("id DESC").
		Limit(n).
		Scan(&refs).Error
	if err != nil {
		return nil, err
	}
	return refs, nil
}
