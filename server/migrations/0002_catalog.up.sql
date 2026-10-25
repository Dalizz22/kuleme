-- 0002 三棵树：归属库、成员角色、分类树、位置树
--
-- Space、Category、Location 分别建表，各自用 parent_id + sort 保存层级。
-- 分类与位置是实例共用目录：目录名称可见，其中的物品按库权限过滤。

CREATE TABLE space (
    id          uuid PRIMARY KEY,
    parent_id   uuid REFERENCES space (id) ON DELETE RESTRICT,
    name        text        NOT NULL,
    kind        text        NOT NULL DEFAULT 'general',  -- 默认模板 key
    is_private  boolean     NOT NULL DEFAULT true,
    sort        integer     NOT NULL DEFAULT 0,
    created_by  uuid        NOT NULL REFERENCES app_user (id),
    deleted_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    version     integer     NOT NULL DEFAULT 1
);

-- 同一父节点下名称唯一；根节点名称也唯一。NULL 不参与唯一比较，
-- 因此根节点用部分唯一索引单独约束。
CREATE UNIQUE INDEX space_sibling_name_key
    ON space (parent_id, lower(name)) WHERE deleted_at IS NULL AND parent_id IS NOT NULL;
CREATE UNIQUE INDEX space_root_name_key
    ON space (lower(name)) WHERE deleted_at IS NULL AND parent_id IS NULL;

-- 成员角色只在顶层库授权，子库继承同一权限范围（首版简化）。
CREATE TABLE space_member (
    space_id   uuid        NOT NULL REFERENCES space (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    role       text        NOT NULL CHECK (role IN ('owner', 'manager', 'editor', 'viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, user_id)
);

CREATE INDEX space_member_user_idx ON space_member (user_id);

CREATE TABLE category (
    id         uuid PRIMARY KEY,
    parent_id  uuid REFERENCES category (id) ON DELETE RESTRICT,
    name       text        NOT NULL,
    sort       integer     NOT NULL DEFAULT 0,
    deleted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version    integer     NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX category_sibling_name_key
    ON category (parent_id, lower(name)) WHERE deleted_at IS NULL AND parent_id IS NOT NULL;
CREATE UNIQUE INDEX category_root_name_key
    ON category (lower(name)) WHERE deleted_at IS NULL AND parent_id IS NULL;

-- 位置独立于库：同一位置可以放不同库的物品，结果按库权限过滤。
CREATE TABLE location (
    id         uuid PRIMARY KEY,
    parent_id  uuid REFERENCES location (id) ON DELETE RESTRICT,
    name       text        NOT NULL,
    code       char(10),
    sort       integer     NOT NULL DEFAULT 0,
    deleted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    version    integer     NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX location_sibling_name_key
    ON location (parent_id, lower(name)) WHERE deleted_at IS NULL AND parent_id IS NOT NULL;
CREATE UNIQUE INDEX location_root_name_key
    ON location (lower(name)) WHERE deleted_at IS NULL AND parent_id IS NULL;
CREATE UNIQUE INDEX location_code_key ON location (code) WHERE code IS NOT NULL;
