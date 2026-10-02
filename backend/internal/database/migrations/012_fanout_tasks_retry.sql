-- +goose Up
-- +goose StatementBegin
-- fan-out 任务补上「重试退避」与「死信」两块状态。
-- status: 0 = 待处理 / 1 = 已完成 / 2 = 死信 / 3 = 处理中
-- next_attempt_at: 该任务下次可被领取的时刻,退避状态就落在这里。
-- 默认 now():新入队的行立即可领。
ALTER TABLE fanout_tasks ADD COLUMN next_attempt_at TIMESTAMP NOT NULL DEFAULT now();
-- 领取查询按 (next_attempt_at, id) 排序、只扫 status IN (0, 3)(待处理 + 过期的处理中),
-- 这个部分索引正好覆盖,不必碰到已完成/死信的堆积。
CREATE INDEX idx_fanout_claimable ON fanout_tasks(next_attempt_at, id) WHERE status IN (0, 3);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_fanout_claimable;
ALTER TABLE fanout_tasks DROP COLUMN next_attempt_at;
-- +goose StatementEnd
