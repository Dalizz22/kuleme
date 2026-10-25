// Package authz 是库权限判定的唯一入口。
//
// 角色只在顶层库授权，子库继承其权限范围；需要判权限时向上找到有成员记录的
// 根库，而不是为每个子库各存一份成员表。
//
// 权限不足返回 403；对象根本不在用户可见范围时由查询侧返回 404，两者判据见
// docs/接口契约约定.md 第 3 节。
package authz

import (
	"context"
	"errors"
	"sort"

	"gorm.io/gorm"

	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Action 是需要判定的操作级别。
type Action string

const (
	ActionRead  Action = "read"  // 查看库、物品与统计
	ActionWrite Action = "write" // 新建/修改物品、库存操作
	ActionAdmin Action = "admin" // 改名、移动、成员与共享设置
	ActionOwn   Action = "own"   // 删除库
)

// roleRank 表示角色可执行到哪一级操作。
var roleRank = map[string]int{
	"viewer":  1,
	"editor":  2,
	"manager": 3,
	"owner":   4,
}

var actionRank = map[Action]int{
	ActionRead:  1,
	ActionWrite: 2,
	ActionAdmin: 3,
	ActionOwn:   4,
}

// ErrNoSpace 表示库不存在或不在可见范围。调用方据此返回 404，不泄漏私人库是否存在。
var ErrNoSpace = errors.New("库不存在或无权访问")

// Checker 持有数据库连接，提供权限检查。
type Checker struct{ DB *store.DB }

// New 构造 Checker。
func New(db *store.DB) *Checker { return &Checker{DB: db} }

// RoleIn 返回用户在库 effective 的继承角色，没有权限时返回空串。
func (k *Checker) RoleIn(ctx context.Context, userID, spaceID string) (string, error) {
	if k == nil || k.DB == nil || !k.DB.Available() || userID == "" || spaceID == "" {
		return "", nil
	}
	var role string
	err := k.DB.Gorm.WithContext(ctx).Raw(`
		WITH RECURSIVE up AS (
			SELECT id, parent_id, 0 AS depth FROM space WHERE id = ? AND deleted_at IS NULL
			UNION ALL
			SELECT s.id, s.parent_id, up.depth + 1
			FROM space s JOIN up ON s.id = up.parent_id
			WHERE s.deleted_at IS NULL
		)
		SELECT sm.role
		FROM up JOIN space_member sm ON sm.space_id = up.id
		WHERE sm.user_id = ?
		ORDER BY up.depth ASC
		LIMIT 1`, spaceID, userID).Scan(&role).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	return role, nil
}

// Check 判定操作级别。库不存在或无权访问时返回 ErrNoSpace，权限足够返回 nil。
// 系统管理员（is_admin）视为全库最高权限，直接放行（管理员可见可管全部库）。
func (k *Checker) Check(ctx context.Context, actor httpx.Actor, spaceID string, action Action) error {
	if actor.IsAdmin {
		return nil
	}
	role, err := k.RoleIn(ctx, actor.UserID, spaceID)
	if err != nil {
		return httpx.Internal(err)
	}
	need, ok := actionRank[action]
	if !ok {
		return httpx.Internal(errors.New("未知操作级别: " + string(action)))
	}
	if roleRank[role] >= need {
		return nil
	}
	if role == "" {
		return ErrNoSpace
	}
	return httpx.Forbidden("当前账号没有执行该操作的权限")
}

// VisibleSpaceIDs 返回用户可见的全部库（含继承的子库），供列表、计数与导出共用。
//
// ponytail: 每次查询递归取全集，库数量级远小于物品，先做正确性；万级库再改成闭包表或物化路径。
func (k *Checker) VisibleSpaceIDs(ctx context.Context, actor httpx.Actor) ([]string, error) {
	if k == nil || k.DB == nil || !k.DB.Available() || actor.UserID == "" {
		return nil, nil
	}
	if actor.IsAdmin {
		var all []string
		err := k.DB.Gorm.WithContext(ctx).Raw(
			`SELECT id FROM space WHERE deleted_at IS NULL`).Scan(&all).Error
		if err != nil {
			return nil, err
		}
		sort.Strings(all)
		return all, nil
	}
	var ids []string
	err := k.DB.Gorm.WithContext(ctx).Raw(`
		WITH RECURSIVE roots AS (
			SELECT s.id FROM space s JOIN space_member sm ON sm.space_id = s.id
			WHERE sm.user_id = ? AND s.deleted_at IS NULL
		), tree AS (
			SELECT id FROM roots
			UNION ALL
			SELECT c.id FROM space c JOIN tree t ON c.parent_id = t.id
			WHERE c.deleted_at IS NULL
		)
		SELECT id FROM tree`, actor.UserID).Scan(&ids).Error
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

// RequireMember 校验用户在库内拥有至少指定级别，是路由层最常用的入口。
func (k *Checker) RequireMember(c *ginHTTP, spaceID string, action Action) error {
	actor := httpx.CurrentActor(c)
	if !actor.Authenticated() {
		return httpx.Unauthenticated("请先登录")
	}
	if err := k.Check(c.Request.Context(), actor, spaceID, action); err != nil {
		if errors.Is(err, ErrNoSpace) {
			return httpx.NotFound("库不存在或无权访问")
		}
		return err
	}
	return nil
}
