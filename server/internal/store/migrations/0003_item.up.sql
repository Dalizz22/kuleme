-- 0003 物品、文件、标签
--
-- 一件物品只有一个归属库；分类与位置分别回答「是什么」「实际放在哪」。
-- 公开码由安全随机数生成、数据库唯一约束兜底；有流水后不再切换管理方式。

CREATE TABLE item (
    id                   uuid PRIMARY KEY,
    public_code          char(10)    NOT NULL UNIQUE,
    space_id             uuid        NOT NULL REFERENCES space (id) ON DELETE RESTRICT,
    category_id          uuid        REFERENCES category (id) ON DELETE RESTRICT,
    name                 text        NOT NULL,
    note                 text        NOT NULL DEFAULT '',
    management           text        NOT NULL CHECK (management IN ('serial', 'quantity')),
    unit                 text        NOT NULL DEFAULT '件',
    template_key         text        NOT NULL DEFAULT 'general',
    custom_fields        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    field_schema_version integer     NOT NULL DEFAULT 1,
    created_by           uuid        NOT NULL REFERENCES app_user (id),
    deleted_at           timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    version              integer     NOT NULL DEFAULT 1
);

CREATE INDEX item_space_idx ON item (space_id) WHERE deleted_at IS NULL;
CREATE INDEX item_category_idx ON item (category_id) WHERE deleted_at IS NULL;
CREATE INDEX item_created_idx ON item (created_at, id);

-- 搜索首版用 ILIKE；索引是否生效按实测查询计划决定，不预先上 pg_trgm 扩展。
CREATE INDEX item_name_idx ON item (lower(name));

-- 文件元数据。原图与缩略图都走鉴权接口，随机 ID 不替代权限检查。
CREATE TABLE file_object (
    id           uuid PRIMARY KEY,
    space_id     uuid REFERENCES space (id) ON DELETE SET NULL,
    uploaded_by  uuid        NOT NULL REFERENCES app_user (id),
    kind         text        NOT NULL CHECK (kind IN ('photo', 'thumbnail', 'label_pdf', 'export', 'import')),
    content_type text        NOT NULL,
    byte_size    bigint      NOT NULL,
    width        integer     NOT NULL DEFAULT 0,
    height       integer     NOT NULL DEFAULT 0,
    storage_path text        NOT NULL,
    sha256       text        NOT NULL DEFAULT '',
    -- 未入库图片只对上传者及明确授权者可见；关联到业务对象后按该对象权限判断。
    attached     boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX file_object_uploader_idx ON file_object (uploaded_by);

CREATE TABLE item_photo (
    item_id    uuid    NOT NULL REFERENCES item (id) ON DELETE CASCADE,
    file_id    uuid    NOT NULL REFERENCES file_object (id) ON DELETE CASCADE,
    sort       integer NOT NULL DEFAULT 0,
    is_primary boolean NOT NULL DEFAULT false,
    PRIMARY KEY (item_id, file_id)
);

CREATE UNIQUE INDEX item_photo_primary_key ON item_photo (item_id) WHERE is_primary;

-- 标签独立存储，支撑筛选与计数；没有标签的物品依然合法存在。
CREATE TABLE tag (
    id         uuid PRIMARY KEY,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX tag_name_key ON tag (lower(name));

CREATE TABLE item_tag (
    item_id uuid NOT NULL REFERENCES item (id) ON DELETE CASCADE,
    tag_id  uuid NOT NULL REFERENCES tag (id) ON DELETE CASCADE,
    PRIMARY KEY (item_id, tag_id)
);

CREATE INDEX item_tag_tag_idx ON item_tag (tag_id);
