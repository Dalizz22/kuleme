-- 0023 导入二选一：目录导入 / 直接入库
--
-- 契约：POST /imports 带 mode=catalog|stock；stock 必须带 location_id。
-- 模式与位置在创建批次时就固定下来，确认阶段不再接受第二个位置参数，
-- 避免「预览时目录、确认时入库」的语义漂移。

ALTER TABLE import_batch ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'catalog';
ALTER TABLE import_batch ADD COLUMN IF NOT EXISTS location_id uuid
	REFERENCES location(id) ON DELETE RESTRICT;

DO $$ BEGIN
	ALTER TABLE import_batch ADD CONSTRAINT import_batch_mode_check
		CHECK (mode IN ('catalog', 'stock'));
EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL; END $$;

-- 行级失败的稳定错误码（契约：{error_code, message} 分开给）。
ALTER TABLE import_row ADD COLUMN IF NOT EXISTS error_code text;
