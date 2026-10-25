-- rule_run worker 认领租约：认领时一次性写 60s 到期时间；
-- status='running' 且租约过期视为 worker 崩溃，可被重新认领（崩溃回收）。
ALTER TABLE rule_run ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;
