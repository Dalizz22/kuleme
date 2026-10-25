-- 0010 AI 识别：任务与候选
--
-- AI 识别任务走同步执行（不做 worker 进程），表结构与持久任务基础一致：
-- 状态机 queued → running → succeeded/failed，execution_version 防过期写回。
-- 候选是模型产出的物品草稿；确认后回填 item_id，
-- 部分唯一索引保证「一个物品至多由一个候选确认产生」（唯一确认关系）。

CREATE TABLE ai_task (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id     uuid        NOT NULL,
    status       varchar(16) NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued','running','succeeded','failed')),
    payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    result       jsonb,
    error        text,
    -- 预留：任务完成后的代表候选（当前同步返回，未使用）。
    candidate_id uuid,
    created_by   uuid        NOT NULL REFERENCES app_user (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_ai_task_space ON ai_task (space_id, created_at DESC);
CREATE INDEX idx_ai_task_status ON ai_task (status);

CREATE TABLE ai_candidate (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id      uuid        NOT NULL REFERENCES ai_task (id),
    space_id     uuid        NOT NULL,
    -- 任务内序号，从 1 开始，保证界面顺序稳定。
    idx_no       integer     NOT NULL DEFAULT 1 CHECK (idx_no > 0),
    revision     bigint      NOT NULL DEFAULT 1,
    status       varchar(16) NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','confirmed','discarded')),
    -- 识别出的物品草稿（Candidate 全量 JSON，含字段建议与证据）。
    suggested    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- 确认后回填；部分唯一索引 = 候选与物品的唯一确认关系。
    item_id      uuid        REFERENCES item (id),
    -- 确认幂等键（Idempotency-Key）。
    operation_id uuid,
    created_by   uuid        NOT NULL REFERENCES app_user (id),
    confirmed_by uuid        REFERENCES app_user (id),
    confirmed_at timestamptz,
    discarded_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_ai_candidate_item ON ai_candidate (item_id) WHERE item_id IS NOT NULL;
CREATE UNIQUE INDEX uq_ai_candidate_operation ON ai_candidate (operation_id) WHERE operation_id IS NOT NULL;
CREATE INDEX idx_ai_candidate_task ON ai_candidate (task_id, idx_no);
CREATE INDEX idx_ai_candidate_space ON ai_candidate (space_id, status);
