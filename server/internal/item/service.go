// Package item 实现核心资料：物品的增删改查、模板附加字段、标签与列表视图。
//
// 物品本体在 item 表；数量与位置在 stock_balance（由 inventory 服务独占修改）。
// 本模块的视图把两者聚合起来，给出前端可以直接渲染的形态：
// quantity（总量）、status（派生）、location_path（主位置全路径）、photo_file_id。
//
// 附加字段不在这里做业务校验：写入统一走 templates.Merge，按模板 manifest
// 合并与裁决，被拒字段在 Strict 模式下整单拒绝。
package item

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/catalog"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/inventory"
	"github.com/klm/server/internal/labels/kod"
	"github.com/klm/server/internal/store"
	"github.com/klm/server/internal/templates"
)

// View 是物品的对外形态，与 Web/Android 两端的 ItemView 类型对齐。
type View struct {
	ID           string  `json:"id"`
	TemplateKey  string  `json:"template_key"`
	Name         string  `json:"name"`
	PublicCode   string  `json:"public_code"`
	LocationID   *string `json:"location_id"`
	LocationPath string  `json:"location_path"`
	Quantity     string  `json:"quantity"`
	Status       string  `json:"status"`
	Version      int     `json:"version"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	PhotoFileID  *string `json:"photo_file_id"`
	Note         string  `json:"note"`
	Lifecycle    string  `json:"lifecycle"`
	ForSale      bool    `json:"for_sale"`
	MaintOpen    bool    `json:"maintenance_open" gorm:"column:maintenance_open"`
	// gorm:"-" 让 Raw+Scan 跳过 map 字段：gorm schema 解析会对未映射的 map
	// 字段打 ``unsupported data type: &map[]`` 的 error 日志（B31，功能无损）。
	Fields map[string]any `json:"fields" gorm:"-"`
	// 动作可用性只在详情里由服务端计算（actions.go），列表不填充。
	AllowedActions       map[string]bool   `json:"allowed_actions" gorm:"-"`
	ActionBlockedReasons map[string]string `json:"action_blocked_reasons" gorm:"-"`
}

// viewSQL 是列表与详情共用的聚合查询。:where 是过滤占位，由调用方拼接。
//
// quantity/reserved 取该物品全部余额之和；主位置取余额最大的一条；
// location_path 用递归 CTE 拼全路径。
//
// ponytail: 每行一个递归 CTE 拼路径，列表 20 行没压力；上万行再上闭包表。
const viewSQL = `
SELECT v.id, v.template_key, v.name, v.public_code, v.version, v.note,
       to_char(v.created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at,
       to_char(v.updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS updated_at,
       v.custom_fields, v.location_id, lp.location_path AS location_path, v.photo_file_id,
       v.quantity, v.lifecycle, v.for_sale, v.maintenance_open,
       CASE WHEN v.lifecycle <> 'normal' THEN v.lifecycle
            WHEN v.maintenance_open THEN 'maintenance'
            WHEN v.reserved > 0 THEN 'reserved'
            WHEN v.quantity = '0' THEN 'pending_stock'
            ELSE 'in_stock' END AS status
FROM (
    SELECT i.id, i.template_key, i.name, i.public_code, i.version,
           i.created_at, i.updated_at, i.custom_fields, i.note,
           i.lifecycle, i.for_sale, i.maintenance_open,
           rtrim(to_char(COALESCE(SUM(b.on_hand), 0), 'FM999999999990.9999'), '.') AS quantity,
           COALESCE(SUM(b.reserved), 0) AS reserved,
           (SELECT b2.location_id FROM stock_balance b2
            WHERE b2.item_id = i.id
            ORDER BY b2.on_hand DESC, b2.updated_at DESC LIMIT 1) AS location_id,
           p.file_id AS photo_file_id,
           '' AS location_path
    FROM item i
    LEFT JOIN stock_balance b ON b.item_id = i.id
    LEFT JOIN item_photo p ON p.item_id = i.id AND p.is_primary
    WHERE i.deleted_at IS NULL
    GROUP BY i.id, p.file_id
) v
LEFT JOIN LATERAL (
    WITH RECURSIVE up AS (
        SELECT id, parent_id, name, 0 AS depth FROM location WHERE id = v.location_id
        UNION ALL
        SELECT l.id, l.parent_id, l.name, up.depth + 1
        FROM location l JOIN up ON l.id = up.parent_id
    )
    SELECT COALESCE(string_agg(name, ' / ' ORDER BY depth DESC), '') AS location_path
    FROM up
) lp ON true
`

// CodeInvalidStatus 是列表 status 过滤的非法值（422，附带合法枚举）。
const CodeInvalidStatus = "INVALID_STATUS"

// CodeInvalidTransferTarget 是转库目标语义错误（同库转库）：请求本身不对，422。
const CodeInvalidTransferTarget = "INVALID_TRANSFER_TARGET"

// itemStatuses 是 viewSQL 的 CASE 能派生的全部状态；顺序即文档与报错里的顺序。
// 新增派生分支时同步这里，否则新状态无法被筛选。
var itemStatuses = []string{"in_stock", "pending_stock", "reserved", "maintenance", "sold", "scrapped", "lost"}

// ParseStatuses 解析 ?status=a,b&status=c：逗号分隔可重复，去重保序；
// 空/缺省 = 全部（nil）；非法值返回 422 INVALID_STATUS 并列出合法枚举。
func ParseStatuses(raw []string) ([]string, error) {
	allowed := make(map[string]bool, len(itemStatuses))
	for _, s := range itemStatuses {
		allowed[s] = true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, group := range raw {
		for _, s := range strings.Split(group, ",") {
			s = strings.TrimSpace(s)
			if s == "" || seen[s] {
				continue
			}
			if !allowed[s] {
				return nil, httpx.New(422, CodeInvalidStatus, "未知的状态筛选值: "+s+"；可选值: "+strings.Join(itemStatuses, ","))
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

type itemService struct {
	db    *store.DB
	authz *authz.Checker
	tcat  *templates.Catalog
	// rules 是 规则引擎（可选；bootstrap 接线，其他构造点为 nil 不触发）。
	rules RuleTrigger
}

// RuleTrigger 是规则引擎的窄接口，避免 item 直接依赖 rules 包实现细节。
type RuleTrigger interface {
	TriggerFieldChange(ctx context.Context, itemID, spaceID string, changedFields map[string]bool, itemVersion int)
}

// SetRuleTrigger 由 bootstrap 在装配时注入。
func (s *itemService) SetRuleTrigger(t RuleTrigger) { s.rules = t }

func NewService(db *store.DB) (*itemService, error) {
	tcat, err := templates.Builtin()
	if err != nil {
		return nil, err
	}
	return &itemService{db: db, authz: authz.New(db), tcat: tcat}, nil
}

// occupiedGate 维修占用闸门：出售、转库、删除、去重合并共用的前置校验。
// lifecycle≠normal 或维修未结（maintenance_open=true）一律 409 拒绝。
func occupiedGate(lifecycle string, maintenanceOpen bool) error {
	if lifecycle != "normal" || maintenanceOpen {
		return httpx.New(409, httpx.CodeOpenBusiness, "物品维修中/状态不允许该操作")
	}
	return nil
}

// RequireSpace 与 catalog 同款：库不存在/无权访问统一 404。
func (s *itemService) RequireSpace(ctx context.Context, actor httpx.Actor, spaceID string, action authz.Action) error {
	if err := s.authz.Check(ctx, actor, spaceID, action); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return httpx.NotFound("库不存在或无权访问")
		}
		return err
	}
	return nil
}

// mergeFields 用模板 manifest 校验合并附加字段，返回可落库的 custom_fields。
// Strict：任何被拒字段整单拒绝，details 带逐字段原因。
func (s *itemService) mergeFields(templateKey string, existing []byte, input map[string]any) (templates.CustomFields, error) {
	manifest, err := s.tcat.ValidateTemplateKey(templateKey)
	if err != nil {
		return templates.CustomFields{}, httpx.Validation("未知的模板标识: " + templateKey)
	}
	var old templates.CustomFields
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &old); err != nil {
			return templates.CustomFields{}, httpx.Validation("存量附加字段损坏，无法更新")
		}
	} else {
		old = templates.NewCustomFields(manifest.SchemaVersion)
	}
	res := templates.Merge(manifest, old, input, templates.MergeOptions{Strict: true})
	if rejected := res.Rejected(); len(rejected) > 0 {
		details := make(map[string]any, len(rejected))
		list := make([]map[string]string, 0, len(rejected))
		for _, d := range rejected {
			list = append(list, map[string]string{"field": d.FieldID, "reason": d.Message})
		}
		details["fields"] = list
		return templates.CustomFields{}, &httpx.Error{
			Status:  400,
			Code:    httpx.CodeValidationFailed,
			Message: "部分附加字段未通过校验",
			Details: details,
		}
	}
	return res.Fields, nil
}

// Create 新建物品。外部事务内建物品用 CreateTx，防孤儿档。
//
// 建档与初始库存必须同事务——直接拿连接池当 tx 时插入会先提交，
// 初始库存失败就留下一个零库存半成品。
func (s *itemService) Create(ctx context.Context, actor httpx.Actor, in *CreateInput) (*View, error) {
	var created *View
	err := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		v, err := s.CreateTx(ctx, tx, actor, in)
		if err != nil {
			return err
		}
		created = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// CreateTx 在调用方事务里建物品。public_code 由数据库唯一约束兜底，冲突换码重试。
func (s *itemService) CreateTx(ctx context.Context, tx *gorm.DB, actor httpx.Actor, in *CreateInput) (*View, error) {
	userID := actor.UserID
	// body 里的 uuid 字段必须在进 SQL 前校验：非法值会让 ?:uuid 抛 22P02 → 500。
	for _, f := range []struct {
		name     string
		value    string
		required bool
	}{
		{"space_id", in.SpaceID, true},
		{"category_id", deref(in.CategoryID), false},
		{"creation_ref", in.CreationRef, false},
	} {
		if err := httpx.ValidateUUID(f.name, f.value, f.required); err != nil {
			return nil, err
		}
	}
	if in.InitialLocationID != nil {
		if err := httpx.ValidateUUID("initial_location_id", *in.InitialLocationID, false); err != nil {
			return nil, err
		}
	}
	if err := s.RequireSpace(ctx, actor, in.SpaceID, authz.ActionWrite); err != nil {
		return nil, err
	}
	fields, err := s.mergeFields(in.TemplateKey, nil, in.Fields)
	if err != nil {
		return nil, err
	}
	if in.TemplateKey == "" {
		in.TemplateKey = s.tcat.DefaultKey()
	}
	// 逐件制：management 恒 serial（列保留缺省 'serial'，不再接受入参）。
	if in.Unit == "" {
		in.Unit = "件"
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	// 逐件制：管理方式恒 serial，数量恒由余额表达（0/1）。
	const insert = `
INSERT INTO item (id, public_code, space_id, category_id, name, note, management, unit, template_key, custom_fields, created_by, creation_ref)
VALUES (?, ?, ?, ?, ?, ?, 'serial', ?, ?, ?::jsonb, ?, NULLIF(?, '')::uuid)`
	for attempt := 0; attempt < 5; attempt++ {
		id := uuid.NewString()
		code, err := kod.NewPublicCode()
		if err != nil {
			return nil, err
		}
		err = tx.WithContext(ctx).Exec(insert,
			id, code, in.SpaceID, in.CategoryID, in.Name, in.Note,
			in.Unit, in.TemplateKey, string(raw), userID, in.CreationRef).Error
		if err == nil {
			// 初始库存与建物品同事务，失败整体回滚，不产生零库存半成品。
			if err := s.applyInitialStock(ctx, tx, actor, id, in); err != nil {
				return nil, err
			}
			return s.getOn(ctx, tx, id)
		}
		if isUniqueViolation(err) {
			// creation_ref 冲突 = 重复建档请求，直接 409（public_code 才重试换码）。
			if in.CreationRef != "" {
				var existID string
				_ = tx.WithContext(ctx).Raw(
					`SELECT id FROM item WHERE creation_ref = ?::uuid`, in.CreationRef).
					Scan(&existID).Error
				return nil, httpx.New(409, httpx.CodeIdempotencyConflict, "该创建请求已提交（物品 "+existID+"）")
			}
			continue // public_code 撞码，换一个重试
		}
		return nil, err
	}
	return nil, httpx.Internal(errors.New("public_code 连续冲突，请重试"))
}

// applyInitialStock 传入 InitialLocationID 时在调用方事务内建 initial 库存。
// 数量缺省 1；数量格式与 serial 逐件校验由 inventory.applyInner 统一承担。
// 复用 inventory.NewService（与 stocktake 同款），不额外加依赖面。
func (s *itemService) applyInitialStock(ctx context.Context, tx *gorm.DB, actor httpx.Actor, itemID string, in *CreateInput) error {
	if in.InitialLocationID == nil || *in.InitialLocationID == "" {
		return nil
	}
	q := in.InitialQuantity
	if q == "" {
		q = "1"
	}
	_, err := inventory.NewService(s.db).ApplyOpTx(ctx, tx, actor, &inventory.OpInput{
		ItemID:     itemID,
		Kind:       "initial",
		LocationID: in.InitialLocationID,
		Quantity:   q,
	})
	return err
}

// CreateInput 是建物品的请求体。
type CreateInput struct {
	SpaceID     string         `json:"space_id" binding:"required"`
	Name        string         `json:"name" binding:"required"`
	TemplateKey string         `json:"template_key"`
	Note        string         `json:"note"`
	CategoryID  *string        `json:"category_id"`
	CreationRef string         `json:"creation_ref"`
	Unit        string         `json:"unit"`
	Fields      map[string]any `json:"fields"`
	// 传 InitialLocationID 时在同一事务内建 initial 库存，
	// 建物品+期初库存要么都成、要么都回滚；不传则只建物品（待入库）。
	InitialLocationID *string `json:"initial_location_id,omitempty"`
	InitialQuantity   string  `json:"initial_quantity,omitempty"`
}

// UpdateInput 是更新物品的请求体：名称/备注可改，附加字段整体合并。
type UpdateInput struct {
	Name            string
	Note            *string
	Fields          map[string]any
	ExpectedVersion int
}

// Update 用乐观锁修改名称、备注与附加字段。
func (s *itemService) Update(ctx context.Context, actor httpx.Actor, id string, in *UpdateInput) (*View, error) {
	cur, err := s.loadRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.RequireSpace(ctx, actor, cur.SpaceID, authz.ActionWrite); err != nil {
		return nil, err
	}
	// 只有改到价格/金额字段才受「价格」开关约束，改名/改备注不受影响。
	if s.hasPriceField(cur.TemplateKey, in.Fields) {
		if err := s.featureGate(ctx, cur.SpaceID, catalog.FeaturePrice); err != nil {
			return nil, err
		}
	}
	fields, err := s.mergeFields(cur.TemplateKey, cur.CustomFields, in.Fields)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	name := cur.Name
	if in.Name != "" {
		name = in.Name
	}
	note := cur.Note
	if in.Note != nil {
		note = *in.Note
	}
	res := s.db.Gorm.WithContext(ctx).Exec(`
UPDATE item SET name = ?, note = ?, custom_fields = ?::jsonb,
                updated_at = now(), version = version + 1
WHERE id = ? AND deleted_at IS NULL AND version = ?`,
		name, note, string(raw), id, in.ExpectedVersion)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, httpx.New(409, httpx.CodeVersionConflict, "物品已被他人修改，请刷新后重试")
	}
	// 钩子：通知规则引擎哪些字段变了（失败只记日志不影响更新）。
	s.notifyRules(ctx, id, cur, in)
	return s.Get(ctx, actor, id)
}

// notifyRules 计算变更字段并投递规则触发。
func (s *itemService) notifyRules(ctx context.Context, id string, cur *rawItem, in *UpdateInput) {
	changed := map[string]bool{}
	if in.Name != "" && in.Name != cur.Name {
		changed["name"] = true
	}
	if in.Note != nil && *in.Note != cur.Note {
		changed["note"] = true
	}
	newFields := map[string]any{}
	_ = json.Unmarshal(cur.CustomFields, &newFields)
	if vals, ok := newFields["values"].(map[string]any); ok {
		for k, v := range in.Fields {
			if fmt.Sprintf("%v", vals[k]) != fmt.Sprintf("%v", v) {
				changed[k] = true
			}
		}
	}
	if s.rules != nil && len(changed) > 0 {
		s.rules.TriggerFieldChange(ctx, id, cur.SpaceID, changed, cur.Version)
	}
}

// rawItem 是更新前需要的原始行。
type rawItem struct {
	ID              string
	SpaceID         string
	TemplateKey     string
	Name            string
	Note            string
	Version         int
	CustomFields    []byte
	Lifecycle       string
	MaintenanceOpen bool `gorm:"column:maintenance_open"`
}

func (s *itemService) loadRaw(ctx context.Context, id string) (*rawItem, error) {
	var r rawItem
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, space_id, template_key, name, note, version, custom_fields, lifecycle, maintenance_open
		 FROM item WHERE id = ? AND deleted_at IS NULL`, id).Scan(&r).Error
	if err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, httpx.NotFound("物品不存在")
	}
	return &r, nil
}

// Delete 软删除物品；仍有在手余额时拒绝，防止库存悬空。
// 维修中或非正常状态的物品不允许删除。
func (s *itemService) Delete(ctx context.Context, actor httpx.Actor, id string) error {
	cur, err := s.loadRaw(ctx, id)
	if err != nil {
		return err
	}
	if err := s.RequireSpace(ctx, actor, cur.SpaceID, authz.ActionWrite); err != nil {
		return err
	}
	if err := occupiedGate(cur.Lifecycle, cur.MaintenanceOpen); err != nil {
		return err
	}
	var hasStock bool
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT true FROM stock_balance WHERE item_id = ? AND on_hand > 0 LIMIT 1`, id).
		Scan(&hasStock).Error; err != nil {
		return err
	}
	if hasStock {
		return httpx.New(409, httpx.CodeOpenBusiness, "物品仍有在手库存，请先通过库存操作清零")
	}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE item SET deleted_at = now(), updated_at = now(), version = version + 1
		 WHERE id = ? AND deleted_at IS NULL AND maintenance_open = false AND lifecycle = 'normal'`, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return httpx.NotFound("物品不存在")
	}
	return nil
}

// Get 详情：可见性 = 物品所属库对用户可见（与 List 同口径），不可见一律 404。
func (s *itemService) Get(ctx context.Context, actor httpx.Actor, id string) (*View, error) {
	visible, err := s.authz.VisibleSpaceIDs(ctx, actor)
	if err != nil {
		return nil, err
	}
	if len(visible) == 0 {
		return nil, httpx.NotFound("物品不存在")
	}
	views, err := s.query(ctx, actor, viewSQL+`
		WHERE v.id = ?
		  AND v.id IN (SELECT i.id FROM item i WHERE i.deleted_at IS NULL AND i.space_id IN (?))
		LIMIT 1`, id, visible)
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, httpx.NotFound("物品不存在")
	}
	// 动作可用性由服务端计算（actions.go），客户端只按结果显示。
	view := views[0]
	a := s.availability(ctx, actor, view.ID)
	view.AllowedActions = a.AllowedActions
	view.ActionBlockedReasons = a.ActionBlockedReasons
	return &view, nil
}

// listWhere 拼列表过滤条件：模板/关键字/可见库放进 w.id IN (SELECT ...) 的
// 子查询（可走 item 上的索引），派生状态走 listStatusWhere 加在外层。
func listWhere(templateKey, keyword string, visible []string) (string, []any) {
	where := ` WHERE w.id IN (SELECT i.id FROM item i WHERE i.deleted_at IS NULL`
	args := []any{}
	if templateKey != "" {
		where += ` AND i.template_key = ?`
		args = append(args, templateKey)
	}
	if keyword != "" {
		where += ` AND i.name ILIKE ?`
		args = append(args, "%"+keyword+"%")
	}
	where += ` AND i.space_id IN (?))`
	args = append(args, visible)
	return where, args
}

// listStatusWhere 返回派生状态过滤，**必须**拼在 listWhere 之后（视图外层）。
//
// status 是 viewSQL 外层由聚合结果算出的列。若把它拼进
// `w.id IN (SELECT ...)` 子查询，子查询会按 w.status 相关化：1000 件时
// EXPLAIN 显示 SubPlan 每行重扫一次 item（loops=1210），列表 p50 从 14ms
// 涨到 119ms（1000 件实测）。放在外层只是对已算好的列做过滤。
func listStatusWhere(statuses []string) (string, []any) {
	if len(statuses) == 0 {
		return "", nil
	}
	return ` AND w.status IN (?)`, []any{statuses}
}

// List 按可见库过滤，支持模板、关键字与派生状态，稳定排序 (created_at, id)。
// status 过滤在 SQL 层完成（对聚合后的派生列 in_stock/reserved/... 过滤），
// total 与分页使用同一 where，保证「过滤后的总数」。
func (s *itemService) List(ctx context.Context, actor httpx.Actor, templateKey, keyword string, statuses []string, offset, limit int) ([]View, int, error) {
	visible, err := s.authz.VisibleSpaceIDs(ctx, actor)
	if err != nil {
		return nil, 0, err
	}
	if len(visible) == 0 {
		return []View{}, 0, nil
	}
	where, args := listWhere(templateKey, keyword, visible)
	statusWhere, statusArgs := listStatusWhere(statuses)
	// 派生列 status 在 viewSQL 外层计算，过滤与 count 都要套在同一层别名上。
	from := ` FROM (` + viewSQL + `) w`
	fullWhere := where + statusWhere

	var total int
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT count(*)`+from+fullWhere, append(append([]any{}, args...), statusArgs...)...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	views, err := s.query(ctx, actor, `SELECT w.*`+from+fullWhere+
		` ORDER BY w.created_at DESC, w.id DESC OFFSET ? LIMIT ?`,
		append(append(append([]any{}, args...), statusArgs...), offset, limit)...)
	if err != nil {
		return nil, 0, err
	}
	return views, total, nil
}

// query 执行聚合视图查询并解码附加字段。
// getOn 在指定连接（可能是外层事务）上读单个物品视图；不走权限过滤，
// 仅供 CreateTx 刚插入后回读使用（授权已在事务外做过）。
func (s *itemService) getOn(ctx context.Context, db *gorm.DB, id string) (*View, error) {
	views, err := s.queryOn(ctx, db, viewSQL+` WHERE v.id = ? LIMIT 1`, id)
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, httpx.NotFound("物品不存在")
	}
	return &views[0], nil
}

func (s *itemService) query(ctx context.Context, actor httpx.Actor, sql string, args ...any) ([]View, error) {
	return s.queryOn(ctx, s.db.Gorm, sql, args...)
}

func (s *itemService) queryOn(ctx context.Context, db *gorm.DB, sql string, args ...any) ([]View, error) {
	var rows []struct {
		View
		CustomFields []byte `gorm:"column:custom_fields"`
	}
	if err := db.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, r := range rows {
		v := r.View
		v.Fields = map[string]any{}
		if len(r.CustomFields) > 0 {
			var cf templates.CustomFields
			if err := json.Unmarshal(r.CustomFields, &cf); err == nil && cf.Values != nil {
				v.Fields = cf.Values
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// ponytail: strings.Contains 判唯一冲突，与 catalog 同款；细分 pg 错误码等有第二种冲突再加。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

// deref 取可选字符串的值，nil 视为空。
func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
