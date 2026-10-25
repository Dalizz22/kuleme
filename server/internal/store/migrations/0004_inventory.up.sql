-- 0004 库存：余额、流水、幂等键、盘点
--
-- 余额与流水同事务提交。库存变化只能经 inventory 服务完成，借还、售卖、
-- 导入确认与 AI 确认都调用同一入口，不各自改余额。

CREATE TABLE stock_balance (
    id          uuid PRIMARY KEY,
    item_id     uuid           NOT NULL REFERENCES item (id) ON DELETE CASCADE,
    -- 未定位是合法状态：location_id 为 NULL 表示尚未指定位置。
    location_id uuid           REFERENCES location (id) ON DELETE RESTRICT,
    on_hand     numeric(18, 4) NOT NULL DEFAULT 0 CHECK (on_hand >= 0),
    reserved    numeric(18, 4) NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    updated_at  timestamptz    NOT NULL DEFAULT now()
);

-- 同一物品与位置只一条余额记录；未定位余额同样唯一。
CREATE UNIQUE INDEX stock_balance_scope_key
    ON stock_balance (item_id, location_id) NULLS NOT DISTINCT;

CREATE INDEX stock_balance_location_idx ON stock_balance (location_id);

-- 幂等键与操作者、实例、命令类型及规范化请求摘要绑定。
-- 同一键同一内容返回原结果；同一键不同内容返回 IDEMPOTENCY_CONFLICT。
CREATE TABLE idempotency_key (
    key            text        PRIMARY KEY,
    actor_id       uuid        NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    command        text        NOT NULL,
    request_digest text        NOT NULL,
    state          text        NOT NULL CHECK (state IN ('in_progress', 'succeeded', 'failed')),
    http_status    integer,
    response_body  jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    completed_at   timestamptz
);

-- 流水是历史事实，不直接修改；纠错用关联原记录的 correction 操作。
CREATE TABLE stock_movement (
    id                uuid PRIMARY KEY,
    item_id           uuid           NOT NULL REFERENCES item (id) ON DELETE RESTRICT,
    location_id       uuid           REFERENCES location (id) ON DELETE RESTRICT,
    kind              text           NOT NULL CHECK (kind IN (
                          'initial', 'replenish', 'move_in', 'move_out',
                          'consume', 'scrap', 'loan_out', 'loan_return',
                          'sale_reserve', 'sale_release', 'sale_deliver', 'correction')),
    quantity          numeric(18, 4) NOT NULL,
    on_hand_delta     numeric(18, 4) NOT NULL DEFAULT 0,
    reserved_delta    numeric(18, 4) NOT NULL DEFAULT 0,
    reason            text           NOT NULL DEFAULT '',
    operation_id      text,
    corrects_movement uuid           REFERENCES stock_movement (id) ON DELETE RESTRICT,
    actor_id          uuid           NOT NULL REFERENCES app_user (id),
    created_at        timestamptz    NOT NULL DEFAULT now()
);

CREATE INDEX stock_movement_item_idx ON stock_movement (item_id, created_at DESC);
CREATE INDEX stock_movement_location_idx ON stock_movement (location_id, created_at DESC);
CREATE UNIQUE INDEX stock_movement_operation_key
    ON stock_movement (operation_id, item_id, kind) WHERE operation_id IS NOT NULL;

-- 盘点范围是库/位置子集；开始时快照余额版本，确认时比对是否被新流水覆盖。
CREATE TABLE stocktake (
    id             uuid PRIMARY KEY,
    space_id       uuid        NOT NULL REFERENCES space (id) ON DELETE RESTRICT,
    location_id    uuid        REFERENCES location (id) ON DELETE RESTRICT,
    status         text        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'confirmed', 'superseded')),
    scope_snapshot jsonb       NOT NULL DEFAULT '[]'::jsonb,
    diff_preview   jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_by     uuid        NOT NULL REFERENCES app_user (id),
    confirmed_by   uuid        REFERENCES app_user (id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    confirmed_at   timestamptz
);

CREATE TABLE stocktake_line (
    stocktake_id uuid           NOT NULL REFERENCES stocktake (id) ON DELETE CASCADE,
    item_id      uuid           NOT NULL REFERENCES item (id) ON DELETE RESTRICT,
    location_id  uuid           REFERENCES location (id) ON DELETE RESTRICT,
    expected     numeric(18, 4) NOT NULL,
    counted      numeric(18, 4) NOT NULL,
    PRIMARY KEY (stocktake_id, item_id, location_id)
);
