-- 0024 幂等恢复的业务证据
--
-- 业务事务提交后、api_idempotency 终态落库前崩溃是真实窗口：恢复时必须能从
-- 业务行反查「已提交的事实」，而不是按时间猜。maintenance_record 补两个
-- 操作 ID 列（送修/结清各一），与 item.creation_ref、stock_movement.operation_id、
-- ai_candidate.operation_id、import_batch.confirm_operation_id 一起构成五类写入口的提交证据。

ALTER TABLE maintenance_record ADD COLUMN IF NOT EXISTS operation_id uuid;
ALTER TABLE maintenance_record ADD COLUMN IF NOT EXISTS close_operation_id uuid;

-- import_batch：确认动作的记录（confirm 也是业务事务，同样的崩溃窗口）。
ALTER TABLE import_batch ADD COLUMN IF NOT EXISTS confirm_operation_id uuid;
CREATE INDEX IF NOT EXISTS idx_import_batch_confirm_operation ON import_batch(confirm_operation_id)
	WHERE confirm_operation_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_maintenance_operation ON maintenance_record(operation_id)
	WHERE operation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_maintenance_close_operation ON maintenance_record(close_operation_id)
	WHERE close_operation_id IS NOT NULL;
