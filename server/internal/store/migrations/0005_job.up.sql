-- 0005 任务队列：异步任务入队记录
--
-- 目前只有入队与查询；执行器（worker）后续实现，表结构一次到位：
-- status + run_at 的部分索引就是将来执行器的取件路径。

CREATE TABLE job (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind        varchar(64) NOT NULL,
    payload     jsonb       NOT NULL DEFAULT '{}',
    status      varchar(16) NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    priority    integer     NOT NULL DEFAULT 0,
    run_at      timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz,
    error       text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- 执行器扫描：只取已就绪的排队任务。
CREATE INDEX idx_job_pick ON job (status, run_at) WHERE status = 'queued';

-- 列表查询：按创建时间倒序。
CREATE INDEX idx_job_created ON job (created_at DESC);
