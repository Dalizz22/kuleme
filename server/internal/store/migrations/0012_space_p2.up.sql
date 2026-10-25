-- 0012 空间配置：四项开关 + 每库字段定义
-- 开关 = 物品价格 / 出售 / 维修保养 / 盘点，库级生效；
-- 字段定义属于库的「配置」，manager 以上可改，配置有版本号乐观锁。
ALTER TABLE space
  ADD COLUMN IF NOT EXISTS config_version integer NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS settings jsonb NOT NULL DEFAULT '{}';

-- 每库字段定义：field_id 稳定不可变（历史数据引用），展示名/类型/选项可改，
-- 修改时 version+1。删除为软删，field_id 不允许复用（停用 ID 不重用）。
CREATE TABLE IF NOT EXISTS space_field (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  space_id    uuid NOT NULL REFERENCES space(id),
  field_id    text NOT NULL,
  label       text NOT NULL,
  type        text NOT NULL CHECK (type IN ('text','number','date','select','multiselect')),
  options     jsonb NOT NULL DEFAULT '[]',
  required    boolean NOT NULL DEFAULT false,
  sort        integer NOT NULL DEFAULT 0,
  version     integer NOT NULL DEFAULT 1,
  deleted_at  timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (space_id, field_id)
);
CREATE INDEX IF NOT EXISTS idx_space_field_space ON space_field(space_id) WHERE deleted_at IS NULL;
