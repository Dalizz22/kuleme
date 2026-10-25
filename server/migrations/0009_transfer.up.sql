-- 导入批次与行：draft→confirming→done；行级状态支撑逐行结果与失败重试。
CREATE TABLE IF NOT EXISTS import_batch (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id     uuid NOT NULL REFERENCES space(id),
    template_key varchar(64) NOT NULL,
    mapping      jsonb NOT NULL DEFAULT '{}'::jsonb,
    headers      jsonb NOT NULL DEFAULT '[]'::jsonb,
    status       varchar(16) NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft','confirming','done')),
    created_by   uuid NOT NULL REFERENCES app_user(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS import_row (
    batch_id uuid NOT NULL REFERENCES import_batch(id) ON DELETE CASCADE,
    line_no  int  NOT NULL,
    payload  jsonb NOT NULL,
    status   varchar(16) NOT NULL DEFAULT 'pending'
             CHECK (status IN ('pending','ok','failed')),
    item_id  uuid REFERENCES item(id),
    error    text,
    PRIMARY KEY (batch_id, line_no)
);

CREATE INDEX IF NOT EXISTS idx_import_batch_created ON import_batch (created_at DESC);
