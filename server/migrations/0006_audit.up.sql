-- 0006 审计日志：关键操作留痕，管理员可查
--
-- 只追加不修改：各业务模块在关键动作后调用 audit.Record 写一行，
-- 写失败只记日志不阻断业务。账号不提供删除（只禁用），历史归属始终可溯。

CREATE TABLE audit_log (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id    uuid        NOT NULL REFERENCES app_user (id),
    action      varchar(64) NOT NULL,
    object_type varchar(32) NOT NULL,
    object_id   uuid,
    space_id    uuid,
    detail      jsonb       NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- 列表查询：按创建时间倒序分页。
CREATE INDEX idx_audit_created ON audit_log (created_at DESC);

-- 按对象追溯：某类对象 / 某个对象发生过什么。
CREATE INDEX idx_audit_object ON audit_log (object_type, object_id);
