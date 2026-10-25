-- 售价结构化。历史行不回填（price 留 NULL），新出售写入金额；
-- ReverseSale 不动 price（撤销凭证保留原价供审计）。
ALTER TABLE sale ADD COLUMN price numeric(12, 2);
