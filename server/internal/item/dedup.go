// 同库手动去重与疑似重复提示。
//
// 系统只提示不自动合并：疑似=非空序列号类字段值相同（依据），同名辅助核对；
// 手动去重限定同一具体 space_id（父子库也不算同库）；来源进回收站，
// 复杂业务历史拒绝拼接。
package item

import (
	"context"
	"encoding/json"

	"github.com/klm/server/internal/templates"
	"strings"

	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
)

// DuplicateHint 是疑似重复候选及依据。
type DuplicateHint struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	PublicCode string   `json:"public_code"`
	Reasons    []string `json:"reasons"`
}

// dedupRow 是去重操作用到的档案行。
type dedupRow struct {
	ID              string
	SpaceID         string `gorm:"column:space_id"`
	Name            string
	TemplateKey     string `gorm:"column:template_key"`
	FieldsRaw       string `gorm:"column:custom_fields"`
	Lifecycle       string
	MaintenanceOpen bool `gorm:"column:maintenance_open"`
}

// photoReattachGate 照片只在当前挂在来源物品集合上时才允许重挂，
// 防止调用方拿别人物品的 photo/file id 越权重挂到保留项。
func photoReattachGate(ownerItemID string, sourceIDs []string) bool {
	for _, id := range sourceIDs {
		if id == ownerItemID {
			return true
		}
	}
	return false
}

// fields 解析 custom_fields 的 values 层（存储是 templates.CustomFields 包装）。
func (r *dedupRow) fields() map[string]any {
	var cf templates.CustomFields
	if r.FieldsRaw != "" {
		_ = json.Unmarshal([]byte(r.FieldsRaw), &cf)
	}
	if cf.Values == nil {
		return map[string]any{}
	}
	return cf.Values
}

// DuplicateHints 疑似重复候选：同库未删档案中，与指定档案共享非空
// 序列号类字段值（field_id 含 serial/isbn，精确匹配）或同名者。
func (s *itemService) DuplicateHints(ctx context.Context, actor httpx.Actor, id string) ([]DuplicateHint, error) {
	self, herr := s.dedupGate(ctx, actor, id)
	if herr != nil {
		return nil, herr
	}
	_, selfSerials := serialFields(self.fields())
	var cands []dedupRow
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT id, space_id, name, template_key, custom_fields, lifecycle
		 FROM item WHERE space_id = ? AND deleted_at IS NULL AND id <> ?
		 LIMIT 200`, self.SpaceID, id).Scan(&cands).Error
	if err != nil {
		return nil, err
	}
	selfName := normName(self.Name)
	out := make([]DuplicateHint, 0, len(cands))
	for _, c := range cands {
		if c.Lifecycle != "normal" {
			continue
		}
		var reasons []string
		if normName(c.Name) == selfName && selfName != "" {
			reasons = append(reasons, "同名")
		}
		_, candSerials := serialFields(c.fields())
		for k, v := range selfSerials {
			if cv, ok := candSerials[k]; ok && cv == v && v != "" {
				reasons = append(reasons, "序列号相同（"+k+"）")
				break
			}
		}
		if len(reasons) > 0 {
			out = append(out, DuplicateHint{ID: c.ID, Name: c.Name, Reasons: reasons})
		}
	}
	return out, nil
}

// MergePreviewInput 去重预览/确认请求。
type MergePreviewInput struct {
	KeepID   string            `json:"keep_id" binding:"required"`
	MergeIDs []string          `json:"merge_ids" binding:"required,min=1,max=5,dive"`
	Fields   map[string]string `json:"fields"`
	PhotoIDs []string          `json:"photo_ids"`
}

// PreviewMerge 返回合并后的档案样子（不落库）。
func (s *itemService) PreviewMerge(ctx context.Context, actor httpx.Actor, in *MergePreviewInput) (*View, error) {
	keep, sources, herr := s.validateMerge(ctx, actor, in)
	if herr != nil {
		return nil, herr
	}
	merged := map[string]any{}
	for k, v := range keep.fields() {
		merged[k] = v
	}
	for k, v := range in.Fields {
		if v != "" {
			merged[k] = v // 未选字段不清空：空串视为不覆盖
		}
	}
	// 预览视图借用真实查询结果，字段覆盖仅展示。
	view := &View{}
	views, err := s.queryOn(ctx, s.db.Gorm, viewSQL+` WHERE v.id = ? LIMIT 1`, keep.ID)
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, httpx.NotFound("物品不存在")
	}
	*view = views[0]
	view.Fields = merged
	_ = sources
	return view, nil
}

// Merge 执行去重：一个事务——资料/照片并入保留项，来源全部进回收站。
// 库存不相加：来源余额行直接移除（保留项余额原样）。
func (s *itemService) Merge(ctx context.Context, actor httpx.Actor, in *MergePreviewInput) (*View, error) {
	keep, sources, herr := s.validateMerge(ctx, actor, in)
	if herr != nil {
		return nil, herr
	}
	var view *View
	txErr := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 事务内重校验来源仍是简单历史且未被动过（重试安全；维修闸门同查）。
		for i := range sources {
			src := &sources[i]
			fresh, herr := s.loadForDedup(ctx, tx, src.ID)
			if herr != nil {
				return herr
			}
			if fresh.SpaceID != keep.SpaceID {
				return httpx.New(409, httpx.CodeOpenBusiness, "来源档案状态已变化，请重新核对")
			}
			if err := occupiedGate(fresh.Lifecycle, fresh.MaintenanceOpen); err != nil {
				return httpx.New(409, httpx.CodeOpenBusiness, "来源档案状态已变化，请重新核对")
			}
			complex, err := hasComplexHistory(ctx, tx, src.ID)
			if err != nil {
				return err
			}
			if complex {
				return httpx.New(409, httpx.CodeOpenBusiness,
					"来源档案（"+fresh.Name+"）存在业务记录，不能自动拼接，请先处理")
			}
		}
		// 字段并入保留项：走模板合并（未选不清空，保留 CustomFields 包装）。
		anyFields := make(map[string]any, len(in.Fields))
		for k, v := range in.Fields {
			if v != "" { // 空串视为不覆盖
				anyFields[k] = v
			}
		}
		fields, herr := s.mergeFields(keep.TemplateKey, []byte(keep.FieldsRaw), anyFields)
		if herr != nil {
			return herr
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		// 保留项在事务内重读版本，带版本条件更新——来源并入期间
		// 保留项若被并发修改，拒绝覆盖（0 行 = 409 VERSION_CONFLICT）。
		var keepVersion int
		if err := tx.Raw(
			`SELECT version FROM item WHERE id = ? AND deleted_at IS NULL`, keep.ID).
			Scan(&keepVersion).Error; err != nil {
			return err
		}
		res := tx.Exec(
			`UPDATE item SET custom_fields = ?::jsonb, version = version + 1, updated_at = now()
			 WHERE id = ? AND version = ?`, string(raw), keep.ID, keepVersion)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return httpx.New(409, httpx.CodeVersionConflict, "保留档案已被他人修改，请刷新后重试")
		}
		// 照片重挂到保留项（primary 冲突降级为非主图）。
		// 只允许重挂当前确实挂在来源物品上的照片，归属判定
		// photoReattachGate 纯函数把关，SQL 条件再兜一层并发窗口。
		if len(in.PhotoIDs) > 0 {
			srcIDs := make([]string, len(sources))
			for i := range sources {
				srcIDs[i] = sources[i].ID
			}
			var keepPrimary bool
			if err := tx.Raw(`SELECT true FROM item_photo WHERE item_id = ? AND is_primary LIMIT 1`, keep.ID).
				Scan(&keepPrimary).Error; err != nil {
				return err
			}
			for _, pid := range in.PhotoIDs {
				var owner string
				if err := tx.Raw(
					`SELECT item_id::text FROM item_photo WHERE file_id = ? LIMIT 1`, pid).
					Scan(&owner).Error; err != nil {
					return err
				}
				if !photoReattachGate(owner, srcIDs) {
					continue // 不是来源物品的照片：忽略（越权/无效 id 一律不重挂）
				}
				if err := tx.Exec(
					`UPDATE item_photo SET item_id = ?, is_primary = false
					 WHERE file_id = ? AND item_id IN (?)`, keep.ID, pid, srcIDs).Error; err != nil {
					return err
				}
			}
			if !keepPrimary {
				// 保留项没主图则把第一张并入的设为主图。
				tx.Exec(`UPDATE item_photo SET is_primary = true
				 WHERE item_id = ?
				 ORDER BY sort, file_id LIMIT 1`, keep.ID)
			}
		}
		// 来源：余额移除（库存不相加）→ 软删进回收站。
		for i := range sources {
			if err := tx.Exec(`DELETE FROM stock_balance WHERE item_id = ?`, sources[i].ID).Error; err != nil {
				return err
			}
			if err := tx.Exec(
				`UPDATE item SET deleted_at = now(), updated_at = now()
				 WHERE id = ? AND deleted_at IS NULL AND maintenance_open = false AND lifecycle = 'normal'`, sources[i].ID).Error; err != nil {
				return err
			}
		}
		return s.reloadView(ctx, tx, keep.ID, &view)
	})
	if txErr != nil {
		return nil, txErr
	}
	return view, nil
}

// validateMerge 合并前置校验：同库（父子不算）、终态拒绝、来源简单历史预检。
func (s *itemService) validateMerge(ctx context.Context, actor httpx.Actor, in *MergePreviewInput) (*dedupRow, []dedupRow, error) {
	// body 里的 uuid 先校验：非法值进 SQL 会把 22P02 冒成 500。
	if err := httpx.ValidateUUID("keep_id", in.KeepID, true); err != nil {
		return nil, nil, err
	}
	for _, mid := range in.MergeIDs {
		if err := httpx.ValidateUUID("merge_ids", mid, true); err != nil {
			return nil, nil, err
		}
	}
	for _, pid := range in.PhotoIDs {
		if err := httpx.ValidateUUID("photo_ids", pid, true); err != nil {
			return nil, nil, err
		}
	}
	keep, herr := s.dedupGate(ctx, actor, in.KeepID)
	if herr != nil {
		return nil, nil, herr
	}
	if keep.Lifecycle != "normal" {
		return nil, nil, httpx.New(409, httpx.CodeOpenBusiness, "保留档案必须是正常在册状态")
	}
	// 维修中的档案不允许作为合并目标。
	if err := occupiedGate(keep.Lifecycle, keep.MaintenanceOpen); err != nil {
		return nil, nil, err
	}
	sources := make([]dedupRow, 0, len(in.MergeIDs))
	for _, mid := range in.MergeIDs {
		if mid == keep.ID {
			return nil, nil, httpx.Validation("来源档案不能包含保留档案")
		}
		src, herr := s.dedupGate(ctx, actor, mid)
		if herr != nil {
			return nil, nil, herr
		}
		// 只支持相同具体仓库 ID，父子库不视为同库。
		if src.SpaceID != keep.SpaceID {
			return nil, nil, httpx.New(409, httpx.CodeOpenBusiness, "只能合并同一仓库内的档案")
		}
		if src.Lifecycle != "normal" {
			return nil, nil, httpx.New(409, httpx.CodeOpenBusiness, "来源档案必须是正常在册状态")
		}
		// 维修中的来源档案不允许并入。
		if err := occupiedGate(src.Lifecycle, src.MaintenanceOpen); err != nil {
			return nil, nil, err
		}
		complex, err := hasComplexHistory(ctx, s.db.Gorm, mid)
		if err != nil {
			return nil, nil, err
		}
		if complex {
			return nil, nil, httpx.New(409, httpx.CodeOpenBusiness,
				"来源档案（"+src.Name+"）存在业务记录，不能自动拼接，请先处理")
		}
		sources = append(sources, *src)
	}
	return keep, sources, nil
}

// dedupGate 读档案 + 库写权限。
func (s *itemService) dedupGate(ctx context.Context, actor httpx.Actor, id string) (*dedupRow, error) {
	row, herr := s.loadForDedup(ctx, s.db.Gorm, id)
	if herr != nil {
		return nil, herr
	}
	if err := s.RequireSpace(ctx, actor, row.SpaceID, authz.ActionWrite); err != nil {
		return nil, err
	}
	return row, nil
}

func (s *itemService) loadForDedup(ctx context.Context, tx *gorm.DB, id string) (*dedupRow, error) {
	var row dedupRow
	err := tx.WithContext(ctx).Raw(
		`SELECT id, space_id, name, template_key, custom_fields, lifecycle, maintenance_open
		 FROM item WHERE id = ? AND deleted_at IS NULL`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, httpx.NotFound("物品不存在")
	}
	return &row, nil
}

// hasComplexHistory 来源是否已有初始入库之外的业务痕迹
// （流水多于一条、出售/维修记录都算复杂历史，拒绝拼接）。
func hasComplexHistory(ctx context.Context, tx *gorm.DB, itemID string) (bool, error) {
	var movements, sales, maintenance int64
	if err := tx.Raw(`SELECT count(*) FROM stock_movement WHERE item_id = ?`, itemID).
		Scan(&movements).Error; err != nil {
		return false, err
	}
	if movements > 1 {
		return true, nil
	}
	if err := tx.Raw(`SELECT count(*) FROM sale WHERE item_id = ?`, itemID).Scan(&sales).Error; err != nil {
		return false, err
	}
	if err := tx.Raw(`SELECT count(*) FROM maintenance_record WHERE item_id = ?`, itemID).
		Scan(&maintenance).Error; err != nil {
		return false, err
	}
	return sales > 0 || maintenance > 0, nil
}

// serialFields 抽取序列号类字段（field_id 含 serial 或等于 isbn）。
func serialFields(fields map[string]any) ([]string, map[string]string) {
	out := map[string]string{}
	var order []string
	for k, v := range fields {
		lk := strings.ToLower(k)
		if !strings.Contains(lk, "serial") && lk != "isbn" && lk != "sn" {
			continue
		}
		sv, ok := v.(string)
		if !ok || sv == "" {
			continue
		}
		out[k] = sv
		order = append(order, k)
	}
	return order, out
}

func normName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
