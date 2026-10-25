-- 有限自动规则（白名单版）
CREATE TABLE IF NOT EXISTS rule (
    id         uuid PRIMARY KEY,
    space_id   uuid NOT NULL REFERENCES space(id),
    name       varchar(80) NOT NULL,
    enabled    boolean NOT NULL DEFAULT true,
    trigger_type varchar(16) NOT NULL CHECK (trigger_type IN ('field_changed','date')),
    config     jsonb NOT NULL DEFAULT '{}'::jsonb,
    actions    jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_by uuid NOT NULL REFERENCES app_user(id),
    version    int NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE INDEX IF NOT EXISTS idx_rule_space ON rule (space_id) WHERE deleted_at IS NULL;

-- 一次运行 = 一个触发物件的执行；run_key 去重防重复投递/防循环。
CREATE TABLE IF NOT EXISTS rule_run (
    id         uuid PRIMARY KEY,
    rule_id    uuid NOT NULL REFERENCES rule(id),
    rule_version int NOT NULL,
    item_id    uuid NOT NULL REFERENCES item(id),
    run_key    varchar(120) NOT NULL,
    status     varchar(16) NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','succeeded','failed','undone')),
    attempts   int NOT NULL DEFAULT 0,
    error      text,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    UNIQUE (run_key)
);

-- 每步实际改动（before/after）供撤回补偿。
CREATE TABLE IF NOT EXISTS rule_step (
    id         uuid PRIMARY KEY,
    run_id     uuid NOT NULL REFERENCES rule_run(id),
    seq        int NOT NULL,
    action     varchar(16) NOT NULL CHECK (action IN ('set_field','move','notify')),
    target     varchar(80) NOT NULL DEFAULT '',
    item_version int,
    before_value jsonb,
    after_value  jsonb,
    status     varchar(16) NOT NULL DEFAULT 'done' CHECK (status IN ('done','skipped','failed')),
    detail     text,
    undone_at  timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_rule_step_run_seq ON rule_step (run_id, seq);

-- 收件人私有站内消息。
CREATE TABLE IF NOT EXISTS message (
    id           uuid PRIMARY KEY,
    recipient_id uuid NOT NULL REFERENCES app_user(id),
    title        varchar(200) NOT NULL,
    body         text NOT NULL,
    run_id       uuid,
    read_at      timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_message_recipient ON message (recipient_id, created_at DESC);
