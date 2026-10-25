-- 私有下载中心：导出产物落盘保存，只归发起者，可主动撤权。
CREATE TABLE IF NOT EXISTS export_file (
    id            uuid PRIMARY KEY,
    requested_by  uuid NOT NULL REFERENCES app_user(id),
    params        jsonb NOT NULL DEFAULT '{}'::jsonb,
    status        varchar(16) NOT NULL DEFAULT 'done' CHECK (status IN ('done','failed')),
    row_count     int NOT NULL DEFAULT 0,
    byte_size     bigint NOT NULL DEFAULT 0,
    file_name     text NOT NULL,
    error         text,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_export_file_requester ON export_file (requested_by, created_at DESC);
