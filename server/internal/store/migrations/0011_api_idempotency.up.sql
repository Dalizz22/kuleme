-- 0011 HTTP 幂等：写端点通用幂等表
--
-- 与 inventory.idempotency_key 同构（占位 → 执行 → 落终态），但承接 HTTP 层：
-- 任何带 Idempotency-Key 头的写请求在进入业务前先按 key 原子占位。
-- request_digest 覆盖 actor + 请求体；response_body 存原始 JSON 字节（bytea），
-- 重放按原状态码原样返回。

CREATE TABLE api_idempotency (
    key            text        PRIMARY KEY,
    actor_id       uuid        NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    request_digest text        NOT NULL,
    state          text        NOT NULL CHECK (state IN ('in_progress', 'succeeded', 'failed')),
    http_status    integer,
    response_body  bytea,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_api_idempotency_actor ON api_idempotency (actor_id, created_at DESC);
