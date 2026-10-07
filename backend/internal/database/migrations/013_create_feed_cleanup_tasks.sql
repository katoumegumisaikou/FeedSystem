-- +goose Up
-- +goose StatementBegin
-- 关注流缓存的过期清理任务:读侧惰性清理的**持久化**队列。
--
-- 为什么入库而不放在进程内存里:清理请求由读路径产生。放进内存队列有三个后果 ——
-- 进程重启后队列里的待清理项全部丢失;有容量上限,满了只能丢弃;丢弃与丢失都没有
-- 任何痕迹。而「某个 key 会一直留着过期成员」正是这个功能要解决的问题,把它的触发
-- 记在一次性的内存状态上,等于让问题换个形式回来。入库之后,每次读都会重新确认一次,
-- 丢一条也能自愈(下一次读重新入队)。
--
-- UNIQUE(cache_key) 是幂等的落点:一个 key 会被很多读者在同一瞬间判定为过期,
-- 只应生成一行。也正因为清理以 **key** 为粒度,「同一个大V 有多个清理任务」这件事
-- 在表结构上就不存在 —— 一个 key 一行,与作者无关。
--
-- 表里**没有** status / attempts / next_attempt_at,这是相对 fanout_tasks 的有意取舍:
-- 清理是**单步且幂等**的(一条 ZREMRANGEBYSCORE),重复执行永远安全,所以领取即删除,
-- 不需要「处理中」的租约(崩溃后重跑无害),也不需要重试计数(失败只是这一轮没清成,
-- 下一次读会重新入队)。fanout_tasks 需要那套状态机,是因为投递是**多步且不可重放**的
-- (推给 N 个粉丝,中途失败必须记住推到哪了),两者不是一回事。
--
-- 也就没有 attempts/死信这类「永久失败」的兜底:能进这张表的 key 必然是刚被读路径
-- ZRevRangeWithScores 成功读出来的(feed:inbox:* / feed:bigv:*),不可能是类型不对
-- 之类的永久错误;真正的失败只有 Redis 暂时不可达,而那属于自愈范围。
CREATE TABLE feed_cleanup_tasks (
    id             BIGSERIAL PRIMARY KEY,
    cache_key      TEXT NOT NULL,
    window_seconds INT NOT NULL,
    created_at     TIMESTAMP NOT NULL DEFAULT now(),
    CONSTRAINT uq_feed_cleanup_tasks_key UNIQUE (cache_key)
);
-- 不另建索引:领取按 id 升序 LIMIT,主键索引正好覆盖,而且表里每一行都是可领的
-- (不像 fanout_tasks 需要按 status 过滤,所以那边才有 partial index)。
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE feed_cleanup_tasks;
-- +goose StatementEnd
