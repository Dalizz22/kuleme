-- 盘点差异：行级实点确认标记。false=未找到（确认时按差异盘亏处理）。
ALTER TABLE stocktake_line ADD COLUMN counted_confirmed boolean NOT NULL DEFAULT false;
