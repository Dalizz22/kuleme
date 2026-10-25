// Package catalog 实现三棵树：归属库（space）、分类（category）、位置（location）。
//
// 三棵树共享同一套层级规则：parent_id + sort，同级名称唯一，软删除，
// 删除被引用节点返回 NODE_IN_USE，移动带环检测。差异只有各自的附加列：
// space 有 kind/is_private/created_by 与成员表，location 有 code。
//
// 分类与位置是实例共用目录：查看对所有登录用户开放，写操作要求管理员；
// 库的读写走 authz 库级权限。
package catalog

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Node 是三棵树共用的节点形态。
type Node struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parent_id"`
	Name     string  `json:"name"`
	Sort     int     `json:"sort"`
	Version  int     `json:"version"`
}

// SpaceNode 是库节点，附加库特有字段。
type SpaceNode struct {
	Node
	Kind      string `json:"kind"`
	IsPrivate bool   `json:"is_private"`
	CreatedBy string `json:"created_by"`
}

// LocationNode 是位置节点，附加编码。
type LocationNode struct {
	Node
	Code *string `json:"code"`
}

// catalogService 持有依赖。三棵树的查询都以它为接收者。
type catalogService struct {
	db    *store.DB
	authz *authz.Checker
}

func newService(db *store.DB) *catalogService {
	return &catalogService{db: db, authz: authz.New(db)}
}

// RequireSpace 校验用户对库的操作级别，库不存在/无权访问统一 404，
// 权限不足 403。路由层在每个空间级操作前调用。
func (s *catalogService) RequireSpace(ctx context.Context, actor httpx.Actor, spaceID string, action authz.Action) error {
	err := s.authz.Check(ctx, actor, spaceID, action)
	if errors.Is(err, authz.ErrNoSpace) {
		return httpx.NotFound("库不存在或无权访问")
	}
	return err
}

// isUniqueViolation 判断是否唯一约束冲突。字符串匹配而非解析 pg 错误码，
// 冲突文案足够稳定；需要细分时再换 pgconn.PgError。
//
// ponytail: strings.Contains 判重，pg 错误码细分等真有第二种冲突再加。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

// ---------------------------------------------------------------------------
// 通用层级操作（space / category / location 共用）
// ---------------------------------------------------------------------------

// listTree 返回整棵树的存活节点，按父节点与顺序排序，客户端自行组装层级。
func (s *catalogService) listTree(ctx context.Context, table string) ([]Node, error) {
	nodes := []Node{} // 空表必须是 [] 而不是 null（§2 列表契约）
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, parent_id, name, sort, version FROM ` + table +
			` WHERE deleted_at IS NULL ORDER BY parent_id NULLS FIRST, sort, created_at`).Scan(&nodes).Error
	return nodes, err
}

// nodeExists 确认节点存在且未删除。
func (s *catalogService) nodeExists(ctx context.Context, table, id string) (bool, error) {
	var ok bool
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT true FROM `+table+` WHERE id = ? AND deleted_at IS NULL`, id).Scan(&ok).Error
	return ok, err
}

// cycleExists 检查把 node 移到 newParent 下是否成环（newParent 是 node 自身或其后代）。
func (s *catalogService) cycleExists(ctx context.Context, table, nodeID, newParent string) (bool, error) {
	var ok bool
	err := s.db.Gorm.WithContext(ctx).Raw(
		`WITH RECURSIVE down AS (
			SELECT id FROM `+table+` WHERE id = ? AND deleted_at IS NULL
			UNION ALL
			SELECT c.id FROM `+table+` c JOIN down d ON c.parent_id = d.id
			WHERE c.deleted_at IS NULL
		)
		SELECT true FROM down WHERE id = ? LIMIT 1`, newParent, nodeID).Scan(&ok).Error
	return ok, err
}

// childrenExist 报告节点是否有存活子节点。有子节点时删除被拒绝。
func (s *catalogService) childrenExist(ctx context.Context, table, id string) (bool, error) {
	var ok bool
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT true FROM `+table+` WHERE parent_id = ? AND deleted_at IS NULL LIMIT 1`, id).Scan(&ok).Error
	return ok, err
}

// deleteNode 软删除节点：无子节点且未被引用时才允许。guards 是「引用存在」
// 检查 SQL 列表，参数为节点 id，命中任意一个即拒绝。
func (s *catalogService) deleteNode(ctx context.Context, table, id string, guards []string) error {
	if has, err := s.childrenExist(ctx, table, id); err != nil {
		return err
	} else if has {
		return httpx.New(409, httpx.CodeNodeInUse, "节点下还有子节点，先处理子节点")
	}
	for _, guard := range guards {
		var ok bool
		if err := s.db.Gorm.WithContext(ctx).Raw(guard, id).Scan(&ok).Error; err != nil {
			return err
		}
		if ok {
			return httpx.New(409, httpx.CodeNodeInUse, "节点仍被物品或库存引用，不能删除")
		}
	}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE `+table+` SET deleted_at = now(), version = version + 1
		 WHERE id = ? AND deleted_at IS NULL`, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return httpx.NotFound("节点不存在")
	}
	return nil
}

// patchNameSort 应用改名与排序，带乐观锁。
func (s *catalogService) patchNameSort(ctx context.Context, table, id, name string, sort *int, version int) error {
	set := strings.Builder{}
	args := []any{}
	if name != "" {
		set.WriteString("name = ?, ")
		args = append(args, name)
	}
	if sort != nil {
		set.WriteString("sort = ?, ")
		args = append(args, *sort)
	}
	if set.Len() == 0 {
		return httpx.Validation("没有需要修改的字段")
	}
	args = append(args, id, version)
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE `+table+` SET `+set.String()+`updated_at = now(), version = version + 1
		 WHERE id = ? AND deleted_at IS NULL AND version = ?`, args...)
	if res.Error != nil {
		if isUniqueViolation(res.Error) {
			return httpx.Validation("同级名称已存在")
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return httpx.New(409, httpx.CodeVersionConflict, "内容已被他人修改，请刷新后重试")
	}
	return nil
}

// moveNode 把节点移动到新父节点下（nil 或空串表示移到根），带环检测。
func (s *catalogService) moveNode(ctx context.Context, table, id string, newParent *string) error {
	if newParent == nil || *newParent == "" {
		res := s.db.Gorm.WithContext(ctx).Exec(
			`UPDATE `+table+` SET parent_id = NULL, updated_at = now(), version = version + 1
			 WHERE id = ? AND deleted_at IS NULL`, id)
		return finishMove(res)
	}
	if *newParent == id {
		return httpx.Validation("不能把节点移动到自己下面")
	}
	ok, err := s.nodeExists(ctx, table, *newParent)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("目标父节点不存在")
	}
	cycle, err := s.cycleExists(ctx, table, id, *newParent)
	if err != nil {
		return err
	}
	if cycle {
		return httpx.New(409, httpx.CodeTreeCycle, "不能把节点移动到它自己的子节点下面")
	}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE `+table+` SET parent_id = ?, updated_at = now(), version = version + 1
		 WHERE id = ? AND deleted_at IS NULL`, *newParent, id)
	return finishMove(res)
}

func finishMove(res *gorm.DB) error {
	if res.Error != nil {
		if isUniqueViolation(res.Error) {
			return httpx.Validation("同级名称已存在")
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return httpx.NotFound("节点不存在")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 库（space）
// ---------------------------------------------------------------------------

// listSpaces 返回当前用户可见的库（含继承可见的子库）。
func (s *catalogService) listSpaces(ctx context.Context, actor httpx.Actor) ([]SpaceNode, error) {
	visible, err := s.authz.VisibleSpaceIDs(ctx, actor)
	if err != nil {
		return nil, err
	}
	if len(visible) == 0 {
		return []SpaceNode{}, nil
	}
	var nodes []SpaceNode
	err = s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, parent_id, name, kind, is_private, sort, created_by, version
		 FROM space WHERE deleted_at IS NULL ORDER BY parent_id NULLS FIRST, sort, created_at`).
		Scan(&nodes).Error
	if err != nil {
		return nil, err
	}
	allow := make(map[string]bool, len(visible))
	for _, id := range visible {
		allow[id] = true
	}
	out := make([]SpaceNode, 0, len(visible))
	for _, n := range nodes {
		if allow[n.ID] {
			out = append(out, n)
		}
	}
	return out, nil
}

// spaceKinds 是库类型的合法取值：general 是迁移里的默认值，home/shared
// 是 SpaceInput 契约里的枚举；不在此列的任意字符串一律 422（不允许脏数据落库）。
var spaceKinds = map[string]bool{"general": true, "home": true, "shared": true}

// createSpace 建库。根库任何登录用户可建（自动成为 owner）；子库要求父库 admin。
func (s *catalogService) createSpace(ctx context.Context, actor httpx.Actor, n *SpaceNode) (*SpaceNode, error) {
	userID := actor.UserID
	if n.ParentID != nil && *n.ParentID != "" {
		if err := httpx.ValidateUUID("parent_id", *n.ParentID, false); err != nil {
			return nil, err
		}
		if err := s.authz.Check(ctx, actor, *n.ParentID, authz.ActionAdmin); err != nil {
			if errors.Is(err, authz.ErrNoSpace) {
				return nil, httpx.NotFound("父库不存在或无权访问")
			}
			return nil, err
		}
	} else {
		n.ParentID = nil
	}
	if n.Kind == "" {
		n.Kind = "general"
	}
	if !spaceKinds[n.Kind] {
		return nil, httpx.Validation("kind 只能是 general、home 或 shared")
	}
	n.ID = uuid.NewString()
	n.CreatedBy = userID
	err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO space (id, parent_id, name, kind, is_private, sort, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.ParentID, n.Name, n.Kind, n.IsPrivate, n.Sort, userID).Error
	if err != nil {
		if isUniqueViolation(err) {
			return nil, httpx.Validation("同级库名已存在")
		}
		return nil, err
	}
	// 建库人自动成为 owner（子库继承父库权限，仅根库需要成员记录）。
	if n.ParentID == nil {
		if err := s.db.Gorm.WithContext(ctx).Exec(
			`INSERT INTO space_member (space_id, user_id, role) VALUES (?, ?, 'owner')`,
			n.ID, userID).Error; err != nil {
			return nil, err
		}
	}
	return n, nil
}

// patchSpace 修改库名称、排序、移动。要求 admin。
func (s *catalogService) patchSpace(ctx context.Context, actor httpx.Actor, id string, n *SpaceNode, sort *int, move bool) error {
	if err := s.authz.Check(ctx, actor, id, authz.ActionAdmin); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return httpx.NotFound("库不存在或无权访问")
		}
		return err
	}
	if move {
		if err := s.moveNode(ctx, "space", id, n.ParentID); err != nil {
			return err
		}
	}
	return s.patchNameSort(ctx, "space", id, n.Name, sort, n.Version)
}

var spaceDeleteGuards = []string{
	`SELECT true FROM item WHERE space_id = ? AND deleted_at IS NULL LIMIT 1`,
}

// deleteSpace 删除库：要求 own 级别，且库内没有物品。
func (s *catalogService) deleteSpace(ctx context.Context, actor httpx.Actor, id string) error {
	if err := s.authz.Check(ctx, actor, id, authz.ActionOwn); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return httpx.NotFound("库不存在或无权访问")
		}
		return err
	}
	return s.deleteNode(ctx, "space", id, spaceDeleteGuards)
}

// ---------------------------------------------------------------------------
// 成员（space_member）
// ---------------------------------------------------------------------------

// Member 是库成员的对外形态。
type Member struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func (s *catalogService) listMembers(ctx context.Context, spaceID string) ([]Member, error) {
	// 读写对称：成员行只挂根库，按传入 id 查子空间会拿到空列表。
	rootID, err := s.rootSpaceID(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	members := []Member{} // 空表必须是 [] 而不是 null（§2 列表契约）
	err = s.db.Gorm.WithContext(ctx).Raw(
		`SELECT sm.user_id, u.username, u.display_name, sm.role
		 FROM space_member sm JOIN app_user u ON u.id = sm.user_id
		 WHERE sm.space_id = ? ORDER BY sm.role, u.username`, rootID).Scan(&members).Error
	return members, err
}

// rootSpaceID 向上追溯所在根库。成员关系（space_member）只挂在根库上，
// 读写成员都必须落到根库 id，传子空间 id 时不能各查各的。
func (s *catalogService) rootSpaceID(ctx context.Context, spaceID string) (string, error) {
	var rootID string
	err := s.db.Gorm.WithContext(ctx).Raw(
		`WITH RECURSIVE up AS (
			SELECT id, parent_id FROM space WHERE id = ? AND deleted_at IS NULL
			UNION ALL
			SELECT s.id, s.parent_id FROM space s JOIN up ON s.id = up.parent_id
			WHERE s.deleted_at IS NULL
		)
		SELECT id FROM up WHERE parent_id IS NULL LIMIT 1`, spaceID).Scan(&rootID).Error
	return rootID, err
}

// setMember 添加或更新成员角色。成员写入根库；把根库最后一名 owner
// 降级/改角色会让库失去管理员，返回 409。
func (s *catalogService) setMember(ctx context.Context, spaceID, targetUserID, role string) error {
	if role != "owner" && role != "manager" && role != "editor" && role != "viewer" {
		return httpx.Validation("角色只能是 owner/manager/editor/viewer")
	}
	var ok bool
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT true FROM app_user WHERE id = ? AND disabled_at IS NULL`, targetUserID).
		Scan(&ok).Error; err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("用户不存在")
	}
	rootID, err := s.rootSpaceID(ctx, spaceID)
	if err != nil {
		return err
	}
	if role != "owner" {
		// 事务 + FOR UPDATE 锁根库 owner 行：并发双降级在锁上排队，
		// 后到者看到锁释放后的最新计数，不会把 owner 清成 0（check-then-act 修复）。
		return s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			ownerIDs, err := lockOwners(ctx, tx, rootID)
			if err != nil {
				return err
			}
			if isOwnerAmong(ownerIDs, targetUserID) && len(ownerIDs) <= 1 {
				return httpx.New(409, httpx.CodeOpenBusiness, "不能移除根库的最后一名 owner")
			}
			return tx.Exec(
				`INSERT INTO space_member (space_id, user_id, role) VALUES (?, ?, ?)
				 ON CONFLICT (space_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
				rootID, targetUserID, role).Error
		})
	}
	return s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO space_member (space_id, user_id, role) VALUES (?, ?, ?)
		 ON CONFLICT (space_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		rootID, targetUserID, role).Error
}

// lockOwners 锁定根库全部 owner 行并返回其 user_id（SELECT ... FOR UPDATE）。
func lockOwners(ctx context.Context, tx *gorm.DB, rootID string) ([]string, error) {
	var ownerIDs []string
	err := tx.WithContext(ctx).Raw(
		`SELECT user_id FROM space_member WHERE space_id = ? AND role = 'owner' FOR UPDATE`,
		rootID).Scan(&ownerIDs).Error
	return ownerIDs, err
}

func isOwnerAmong(ownerIDs []string, userID string) bool {
	for _, id := range ownerIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// removeMember 移除成员（成员行在根库，按根库 id 删除）。
// 根库的最后一个 owner 不能被移除，否则库会失去管理员。
func (s *catalogService) removeMember(ctx context.Context, spaceID, targetUserID string) error {
	rootID, err := s.rootSpaceID(ctx, spaceID)
	if err != nil {
		return err
	}
	// 同 setMember：锁 owner 行后再判定，堵并发窗口。
	return s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ownerIDs, err := lockOwners(ctx, tx, rootID)
		if err != nil {
			return err
		}
		var role string
		if err := tx.WithContext(ctx).Raw(
			`SELECT role FROM space_member WHERE space_id = ? AND user_id = ?`,
			rootID, targetUserID).Scan(&role).Error; err != nil {
			return err
		}
		if role == "owner" && len(ownerIDs) <= 1 {
			return httpx.New(409, httpx.CodeOpenBusiness, "不能移除根库的最后一名 owner")
		}
		res := tx.Exec(
			`DELETE FROM space_member WHERE space_id = ? AND user_id = ?`, rootID, targetUserID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return httpx.NotFound("成员不存在")
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// 分类与位置（实例共用目录）
// ---------------------------------------------------------------------------

// createDirectory 在分类树建节点（分类无附加列）。
func (s *catalogService) createDirectory(ctx context.Context, table, name string, parentID *string) (*Node, error) {
	parentID, err := s.resolveParent(ctx, table, parentID)
	if err != nil {
		return nil, err
	}
	n := &Node{ID: uuid.NewString(), ParentID: parentID, Name: name}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO `+table+` (id, parent_id, name) VALUES (?, ?, ?)`,
		n.ID, n.ParentID, n.Name).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, httpx.Validation("同级名称已存在")
		}
		return nil, err
	}
	return s.getNode(ctx, table, n.ID)
}

// createLocation 建位置节点（带可选编码）。
func (s *catalogService) createLocation(ctx context.Context, name string, parentID, code *string) (*LocationNode, error) {
	parentID, err := s.resolveParent(ctx, "location", parentID)
	if err != nil {
		return nil, err
	}
	n := &LocationNode{Node: Node{ID: uuid.NewString(), ParentID: parentID, Name: name}, Code: code}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO location (id, parent_id, name, code) VALUES (?, ?, ?, ?)`,
		n.ID, n.ParentID, n.Name, n.Code).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, httpx.Validation("同级名称或位置编码已存在")
		}
		return nil, err
	}
	return s.getLocation(ctx, n.ID)
}

// listLocations 返回整棵位置树（带编码；列表无权限维度，登录即可见）。
func (s *catalogService) listLocations(ctx context.Context) ([]LocationNode, error) {
	nodes := []LocationNode{} // 空表必须是 [] 而不是 null（§2 列表契约）
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, parent_id, name, code, sort, version FROM location
		 WHERE deleted_at IS NULL ORDER BY parent_id NULLS FIRST, sort, created_at`).Scan(&nodes).Error
	return nodes, err
}

// resolveParent 校验父节点并归一化（空串视为移到根）。
func (s *catalogService) resolveParent(ctx context.Context, table string, parentID *string) (*string, error) {
	if parentID == nil || *parentID == "" {
		return nil, nil
	}
	if err := httpx.ValidateUUID("parent_id", *parentID, false); err != nil {
		return nil, err
	}
	ok, err := s.nodeExists(ctx, table, *parentID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.NotFound("父节点不存在")
	}
	return parentID, nil
}

// getNode 读取分类/位置单节点。
func (s *catalogService) getNode(ctx context.Context, table, id string) (*Node, error) {
	var n Node
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, parent_id, name, sort, version FROM `+table+
			` WHERE id = ? AND deleted_at IS NULL`, id).Scan(&n).Error
	if err != nil {
		return nil, err
	}
	if n.ID == "" {
		return nil, httpx.NotFound("节点不存在")
	}
	return &n, nil
}

// getLocation 读取位置单节点（带编码）。
func (s *catalogService) getLocation(ctx context.Context, id string) (*LocationNode, error) {
	var n LocationNode
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, parent_id, name, code, sort, version FROM location
		 WHERE id = ? AND deleted_at IS NULL`, id).Scan(&n).Error
	if err != nil {
		return nil, err
	}
	if n.ID == "" {
		return nil, httpx.NotFound("节点不存在")
	}
	return &n, nil
}

// patchLocation 修改位置名称、编码、排序、移动。
func (s *catalogService) patchLocation(ctx context.Context, id string, name string, code *string, sort *int, parentID *string, version int, move bool) error {
	if move {
		parentID, err := s.resolveParent(ctx, "location", parentID)
		if err != nil {
			return err
		}
		if parentID != nil && *parentID == id {
			return httpx.Validation("不能把节点移动到自己下面")
		}
		if cycle, err := s.cycleExists(ctx, "location", id, deref(parentID)); err != nil {
			return err
		} else if cycle {
			return httpx.New(409, httpx.CodeTreeCycle, "不能把节点移动到它自己的子节点下面")
		}
		res := s.db.Gorm.WithContext(ctx).Exec(
			`UPDATE location SET parent_id = ?, updated_at = now(), version = version + 1
			 WHERE id = ? AND deleted_at IS NULL`, parentID, id)
		if err := finishMove(res); err != nil {
			return err
		}
	}
	set, args := buildLocationSet(name, code, sort)
	if set == "" {
		return nil // 只有移动时没有其他字段
	}
	args = append(args, id, version)
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE location SET `+set+`updated_at = now(), version = version + 1
		 WHERE id = ? AND deleted_at IS NULL AND version = ?`, args...)
	if res.Error != nil {
		if isUniqueViolation(res.Error) {
			return httpx.Validation("同级名称或位置编码已存在")
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return httpx.New(409, httpx.CodeVersionConflict, "内容已被他人修改，请刷新后重试")
	}
	return nil
}

func buildLocationSet(name string, code *string, sort *int) (string, []any) {
	set := strings.Builder{}
	args := []any{}
	if name != "" {
		set.WriteString("name = ?, ")
		args = append(args, name)
	}
	if code != nil {
		set.WriteString("code = ?, ")
		args = append(args, *code)
	}
	if sort != nil {
		set.WriteString("sort = ?, ")
		args = append(args, *sort)
	}
	return set.String(), args
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

var (
	categoryDeleteGuards = []string{
		`SELECT true FROM item WHERE category_id = ? AND deleted_at IS NULL LIMIT 1`,
	}
	locationDeleteGuards = []string{
		`SELECT true FROM stock_balance WHERE location_id = ? LIMIT 1`,
		`SELECT true FROM stocktake WHERE location_id = ? AND deleted_at IS NULL LIMIT 1`,
	}
)

// requireDirectoryAdmin 共用目录写操作的管理员校验。
func requireDirectoryAdmin(isAdmin bool) error {
	if !isAdmin {
		return httpx.Forbidden("只有管理员可以维护共用目录")
	}
	return nil
}
