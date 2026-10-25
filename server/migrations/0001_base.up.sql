-- 0001 基础：实例身份、账号、会话、主密钥版本、迁移表索引
--
-- 所有表在首次部署即建立；后续功能表随对应迁移追加，不预建未来结构。

CREATE TABLE instance_meta (
    id            smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    instance_id   char(12)    NOT NULL UNIQUE,
    api_version   text        NOT NULL DEFAULT '1.0',
    initialized   boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app_user (
    id            uuid PRIMARY KEY,
    username      text        NOT NULL,
    display_name  text        NOT NULL DEFAULT '',
    password_hash text        NOT NULL,
    is_admin      boolean     NOT NULL DEFAULT false,
    disabled_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    version       integer     NOT NULL DEFAULT 1
);

-- 用户名大小写不敏感唯一，避免 Admin 与 admin 并存。
CREATE UNIQUE INDEX app_user_username_key ON app_user (lower(username));

-- 会话只保存令牌摘要；令牌本身不落库。注销、禁用账号、改密即撤销。
CREATE TABLE app_session (
    id           uuid PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    token_digest bytea       NOT NULL UNIQUE,
    user_agent   text        NOT NULL DEFAULT '',
    issued_at    timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz
);

CREATE INDEX app_session_user_idx ON app_session (user_id) WHERE revoked_at IS NULL;

-- 首次初始化的一次性凭据。初始化完成后入口关闭，记录保留供审计。
CREATE TABLE setup_credential (
    id          smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    secret_hash text        NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- 实例级设置：币种、主密钥版本等。
CREATE TABLE app_setting (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
