-- 逐件档案：生命周期权威化 + creation_ref 防重复建档 + 逐件余额约束
-- 数据影响（显式列出）：存量测试数据 item.lifecycle 全部落 'normal'；
-- stock_balance.on_hand 大于 1 的钳到 1（逐件制每实物至多在库 1 份）。
ALTER TABLE item ADD COLUMN lifecycle text NOT NULL DEFAULT 'normal'
  CHECK (lifecycle IN ('normal','sold','scrapped','lost'));
ALTER TABLE item ADD COLUMN for_sale boolean NOT NULL DEFAULT false;
ALTER TABLE item ADD COLUMN maintenance_open boolean NOT NULL DEFAULT false;
ALTER TABLE item ADD COLUMN creation_ref uuid;
CREATE UNIQUE INDEX uq_item_creation_ref ON item (creation_ref) WHERE creation_ref IS NOT NULL;

UPDATE stock_balance SET on_hand = 1 WHERE on_hand > 1;
ALTER TABLE stock_balance ADD CONSTRAINT ck_stock_balance_serial CHECK (on_hand IN (0, 1));
