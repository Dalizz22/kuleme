-- 转库历史归原库。流水落账时记下归属库，物品日后转走也不改变历史授权。
ALTER TABLE stock_movement
    ADD COLUMN owning_space_id uuid REFERENCES space (id) ON DELETE RESTRICT;

-- 存量流水按物品当前库回填（迁移时点没有转库语义，现值即归属）。
UPDATE stock_movement m
SET owning_space_id = i.space_id
FROM item i
WHERE i.id = m.item_id;

CREATE INDEX idx_movement_owning_space ON stock_movement (owning_space_id, created_at DESC);
