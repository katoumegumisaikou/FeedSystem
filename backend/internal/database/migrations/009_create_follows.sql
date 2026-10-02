-- +goose Up
-- +goose StatementBegin
-- 关注关系表。
-- 复合主键 (follower_id, followee_id) 一物两用:既是去重约束(同一对关注关系
-- 只会存在一行),其最左前缀 follower_id 又正好覆盖「我关注了谁」的查询。
-- CHECK 兜底禁止自己关注自己。
CREATE TABLE follows (
    follower_id BIGINT NOT NULL,
    followee_id BIGINT NOT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT now(),
    PRIMARY KEY (follower_id, followee_id),
    CONSTRAINT chk_follows_not_self CHECK (follower_id <> followee_id),
    -- 外键:关注双方都必须指向真实用户,拦住「关注一个不存在的 id」写进来的垃圾行
    -- (那种行会白占关注数配额,还让 FollowerCount 变成弱存在性探针)。
    -- 类型与 users.id(BIGSERIAL → bigint)一致,可直接引用。
    -- 代价:插入时对 users 的目标行加一把共享锁(SELECT ... FOR KEY SHARE)——
    -- 只与「删/改该用户主键」冲突,与并发的关注写入本身不冲突。
    -- ON DELETE CASCADE:硬删用户时连带清掉其关注关系(users 当前只有软删,暂不触发)。
    CONSTRAINT fk_follows_follower FOREIGN KEY (follower_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT fk_follows_followee FOREIGN KEY (followee_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX idx_follows_follower ON follows(follower_id);
-- 反方向索引服务粉丝侧场景:粉丝数统计、发布时按粉丝列表 fan-out 投递、
-- 以及大 V 的粉丝关系重算,都要按 followee_id 反查。
CREATE INDEX idx_follows_followee ON follows(followee_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE follows;
-- +goose StatementEnd
