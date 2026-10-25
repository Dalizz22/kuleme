-- 0007 借还：物品借出与归还的业务凭证
--
-- 借出经 inventory.ApplyOpTx(kind=loan_out) 扣在手，归还经 kind=loan_return
-- 回补；余额与流水由库存模块同事务落账，本表只承担借出状态机与留痕。
-- 整单归还，不拆分数量。

CREATE TABLE loan (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id       uuid        NOT NULL REFERENCES item (id),
    borrower_name varchar(120) NOT NULL,
    location_id   uuid,
    quantity      numeric     NOT NULL DEFAULT 1 CHECK (quantity > 0),
    status        varchar(16) NOT NULL DEFAULT 'out' CHECK (status IN ('out','returned')),
    due_date      date,
    note          text,
    created_by    uuid        NOT NULL REFERENCES app_user (id),
    returned_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- 列表查询：按物品聚合 / 按状态过滤。
CREATE INDEX idx_loan_item ON loan (item_id);
CREATE INDEX idx_loan_status ON loan (status);
