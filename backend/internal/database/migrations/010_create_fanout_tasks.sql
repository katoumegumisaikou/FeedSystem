-- +goose Up
-- +goose StatementBegin
-- fan-out 任务表:视频发布后异步为每个粉丝投递一条动态。
-- status: 0 = 待处理 / 1 = 已完成。另有 2 = 死信、3 = 处理中,由
--   012_fanout_tasks_retry.sql 在补重试退避时追加;完整状态机以 012 为准,
--   这里不再把枚举抄第二遍,免得两处漂移。
-- task_type: 0 = 发布投递,1 = 降级重投放(大V 掉出阈值后把缓存里的视频补推给粉丝)。
-- UNIQUE(video_id, task_type) 让「同一视频 + 同一类型」保持幂等:重复发布不会生成
-- 第二条发布任务,而发布任务与降级重投放任务可以并存,互不压制。
CREATE TABLE fanout_tasks (
    id           BIGSERIAL PRIMARY KEY,
    video_id     BIGINT NOT NULL,
    author_id    BIGINT NOT NULL,
    published_at TIMESTAMP NOT NULL,
    task_type    SMALLINT NOT NULL DEFAULT 0,
    status       SMALLINT NOT NULL DEFAULT 0,
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT,
    created_at   TIMESTAMP NOT NULL DEFAULT now(),
    updated_at   TIMESTAMP NOT NULL DEFAULT now(),
    CONSTRAINT uq_fanout_tasks_video_type UNIQUE (video_id, task_type)
);
-- 部分索引只索引待处理行(status = 0),扫描待办队列时不必碰到已完成的堆积。
CREATE INDEX idx_fanout_pending ON fanout_tasks(status, id) WHERE status = 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE fanout_tasks;
-- +goose StatementEnd
