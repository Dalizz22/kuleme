-- 0025 「已遗失」终态：stock_movement.kind 补 lost
--
-- 0004 的 CHECK 漏了 lost，而 inventory/service.go 的 kinds 里已有 lost
-- （onHand -1、toLifecycle "lost"），0013 的 item.lifecycle 也允许 lost。
-- 结果：POST /inventory/operations {kind:"lost"} 在写流水时被 23514
-- stock_movement_kind_check 拒绝 → 500，「已遗失」状态永远不可达、筛选恒空。
--
-- 补约束而不是删操作：与 scrapped 一样是一次性终态，纠正用 correction。

ALTER TABLE stock_movement DROP CONSTRAINT IF EXISTS stock_movement_kind_check;
ALTER TABLE stock_movement ADD CONSTRAINT stock_movement_kind_check CHECK (kind IN (
    'initial', 'replenish', 'move_in', 'move_out',
    'consume', 'scrap', 'loan_out', 'loan_return',
    'sale_reserve', 'sale_release', 'sale_deliver', 'correction',
    'lost'));
