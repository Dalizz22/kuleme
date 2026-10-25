-- 0022 幂等结果快照：失败终态留稳定业务码
--
-- GET /operation-results/{operation_id} 要在 failed 时回 error_code（客户端据此
-- 决定是否用新键重开一次）。原表只有 response_body（含错误信封），但取业务码
-- 需要解析 JSON 且历史行可能没有 body，故单列落库；succeeded 恒为 NULL。

ALTER TABLE api_idempotency ADD COLUMN IF NOT EXISTS error_code text;
