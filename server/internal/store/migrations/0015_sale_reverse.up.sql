-- 出售纠错：标记已撤销的售卖凭证（保留痕迹，不删记录）。
ALTER TABLE sale ADD COLUMN reversed_at timestamptz;
