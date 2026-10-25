-- 维修保养：维修记录表（一件至多一条未结）。
CREATE TABLE maintenance_record (
    id         uuid PRIMARY KEY,
    item_id    uuid NOT NULL REFERENCES item(id),
    reason     text NOT NULL,
    note       text NOT NULL DEFAULT '',
    opened_by  uuid NOT NULL REFERENCES app_user(id),
    opened_at  timestamptz NOT NULL DEFAULT now(),
    closed_by  uuid REFERENCES app_user(id),
    closed_at  timestamptz
);
CREATE INDEX idx_maintenance_item ON maintenance_record(item_id, opened_at DESC);
