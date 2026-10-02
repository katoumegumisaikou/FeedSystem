-- +goose Up
-- +goose StatementBegin
-- 新增 published_at:视频「首次发布」的时刻,草稿期为 NULL,发布时写入。
-- 关注流按它排序,而不是 created_at —— created_at 是「上传」时刻,一个上传完
-- 搁置十几天才发布的草稿,若拿 created_at 当 score,会被 fan-out 的 7 天投递窗口
-- 当场裁掉,视频永远进不了任何粉丝的关注流。
ALTER TABLE videos ADD COLUMN published_at TIMESTAMP;
-- 回填:已发布(status = 1)的老数据无从得知真实发布时刻,用 created_at 作为
-- 最接近的近似值。草稿(status = 4)保持 NULL,发布时由 MarkPublished 写入。
UPDATE videos SET published_at = created_at WHERE status = 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE videos DROP COLUMN published_at;
-- +goose StatementEnd
