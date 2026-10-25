-- 0008 售卖：售出凭证
--
-- 售卖经 inventory.ApplyOpTx(kind=sale_deliver) 一步完成预占释放与实物
-- 出库（需指定位置）；余额与流水由库存模块同事务落账，本表只承担售卖留痕。

CREATE TABLE sale (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id     uuid        NOT NULL REFERENCES item (id),
    location_id uuid,
    quantity    numeric     NOT NULL CHECK (quantity > 0),
    note        text,
    created_by  uuid        NOT NULL REFERENCES app_user (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- 列表查询：按物品聚合 / 按创建时间倒序分页。
CREATE INDEX idx_sale_item ON sale (item_id);
CREATE INDEX idx_sale_created ON sale (created_at DESC);
