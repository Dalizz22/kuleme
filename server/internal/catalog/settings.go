// 空间配置：四项开关与每库字段定义。
// settings jsonb 缺项视为开（通用语义），显式 false 才关闭。
package catalog

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
)

// SpaceSettings 四项开关。缺项 = true。
type SpaceSettings struct {
	Price       bool `json:"price"`
	Sale        bool `json:"sale"`
	Maintenance bool `json:"maintenance"`
	Stocktake   bool `json:"stocktake"`
}

type spaceSettingsRow struct {
	Settings      map[string]bool `gorm:"serializer:json"`
	ConfigVersion int
}

func effectiveSettings(raw map[string]bool) SpaceSettings {
	// 缺项 = 开（通用语义），显式 false 才关。raw[k] 对缺项返回零值 false，
	// 直接判 != false 会把「没配过」当成「已关闭」——必须先看 ok。
	b := func(k string) bool { v, ok := raw[k]; return !ok || v }
	return SpaceSettings{Price: b("price"), Sale: b("sale"),
		Maintenance: b("maintenance"), Stocktake: b("stocktake")}
}

// GetSettings 返回库的四项开关与配置版本。
func (s *catalogService) GetSettings(ctx context.Context, actor httpx.Actor, spaceID string) (SpaceSettings, int, error) {
	if err := s.RequireSpace(ctx, actor, spaceID, authz.ActionRead); err != nil {
		return SpaceSettings{}, 0, err
	}
	var row spaceSettingsRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT settings, config_version FROM space WHERE id = ? AND deleted_at IS NULL`, spaceID).
		Scan(&row).Error
	if err != nil {
		return SpaceSettings{}, 0, err
	}
	return effectiveSettings(row.Settings), row.ConfigVersion, nil
}

// UpdateSettings 整体替换四项开关，配置版本乐观锁。
func (s *catalogService) UpdateSettings(ctx context.Context, actor httpx.Actor, spaceID string, in SpaceSettings, expectedVersion int) (SpaceSettings, int, error) {
	if err := s.RequireSpace(ctx, actor, spaceID, authz.ActionAdmin); err != nil {
		return SpaceSettings{}, 0, err
	}
	if expectedVersion < 1 {
		return SpaceSettings{}, 0, httpx.Validation("缺少配置版本")
	}
	raw := map[string]bool{"price": in.Price, "sale": in.Sale,
		"maintenance": in.Maintenance, "stocktake": in.Stocktake}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE space SET settings = ?::jsonb, config_version = config_version + 1, updated_at = now()
		 WHERE id = ? AND deleted_at IS NULL AND config_version = ?`,
		marshalJSON(raw), spaceID, expectedVersion)
	if res.Error != nil {
		return SpaceSettings{}, 0, res.Error
	}
	if res.RowsAffected == 0 {
		return SpaceSettings{}, 0, httpx.New(409, httpx.CodeVersionConflict, "配置已被他人修改，请刷新后重试")
	}
	return in, expectedVersion + 1, nil
}

// FieldDef 是库级字段定义。FieldID 稳定不可变；修改走 Patch 换 version。
type FieldDef struct {
	ID       string   `json:"id"`
	FieldID  string   `json:"field_id"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Options  []string `json:"options"`
	Required bool     `json:"required"`
	Sort     int      `json:"sort"`
	Version  int      `json:"version"`
}

type spaceFieldRow struct {
	ID       string
	FieldID  string
	Label    string
	Type     string
	Options  []string `gorm:"serializer:json"`
	Required bool
	Sort     int
	Version  int
}

func (r spaceFieldRow) def() FieldDef {
	return FieldDef{ID: r.ID, FieldID: r.FieldID, Label: r.Label, Type: r.Type,
		Options: r.Options, Required: r.Required, Sort: r.Sort, Version: r.Version}
}

var fieldTypes = map[string]bool{"text": true, "number": true, "date": true, "select": true, "multiselect": true}

// ListFields 返回库内全部有效字段定义（按 sort, field_id 排序）。
func (s *catalogService) ListFields(ctx context.Context, actor httpx.Actor, spaceID string) ([]FieldDef, error) {
	if err := s.RequireSpace(ctx, actor, spaceID, authz.ActionRead); err != nil {
		return nil, err
	}
	var rows []spaceFieldRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, field_id, label, type, options, required, sort, version
		 FROM space_field WHERE space_id = ? AND deleted_at IS NULL
		 ORDER BY sort, field_id`, spaceID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]FieldDef, len(rows))
	for i, r := range rows {
		out[i] = r.def()
	}
	return out, nil
}

// CreateField 新增字段定义。field_id 全库唯一（含软删行，停用 ID 不重用）。
func (s *catalogService) CreateField(ctx context.Context, actor httpx.Actor, spaceID string, f *FieldDef) (*FieldDef, error) {
	if err := s.RequireSpace(ctx, actor, spaceID, authz.ActionAdmin); err != nil {
		return nil, err
	}
	f.FieldID = strings.TrimSpace(f.FieldID)
	f.Label = strings.TrimSpace(f.Label)
	if !validFieldID(f.FieldID) {
		return nil, httpx.Validation("field_id 只能包含小写字母、数字和下划线，长度 1-40")
	}
	if f.Label == "" || len(f.Label) > 60 {
		return nil, httpx.Validation("字段名称必填且不超过 60 字")
	}
	if !fieldTypes[f.Type] {
		return nil, httpx.Validation("字段类型必须是 text/number/date/select/multiselect 之一")
	}
	if (f.Type == "select" || f.Type == "multiselect") && len(f.Options) == 0 {
		return nil, httpx.Validation("选择型字段至少需要一个选项")
	}
	var taken bool
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT true FROM space_field WHERE space_id = ? AND field_id = ?`, spaceID, f.FieldID).
		Scan(&taken).Error; err != nil {
		return nil, err
	}
	if taken {
		return nil, httpx.Validation("字段标识已存在（含已删除字段，标识不可复用）")
	}
	id := uuid.NewString()
	err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO space_field (id, space_id, field_id, label, type, options, required, sort)
		 VALUES (?, ?, ?, ?, ?, ?::jsonb, ?, ?)`,
		id, spaceID, f.FieldID, f.Label, f.Type, marshalJSON(f.Options), f.Required, f.Sort).Error
	if err != nil {
		return nil, err
	}
	return s.getField(ctx, spaceID, id)
}

// PatchField 修改展示名/类型/选项/必填/排序，field_id 不可变，乐观锁。
func (s *catalogService) PatchField(ctx context.Context, actor httpx.Actor, spaceID, fieldRowID string, f *FieldDef, expectedVersion int) (*FieldDef, error) {
	if err := s.RequireSpace(ctx, actor, spaceID, authz.ActionAdmin); err != nil {
		return nil, err
	}
	if expectedVersion < 1 {
		return nil, httpx.Validation("缺少字段版本")
	}
	if f.Type != "" && !fieldTypes[f.Type] {
		return nil, httpx.Validation("字段类型不合法")
	}
	if (f.Type == "select" || f.Type == "multiselect") && len(f.Options) == 0 {
		return nil, httpx.Validation("选择型字段至少需要一个选项")
	}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE space_field SET
		   label = COALESCE(NULLIF(trim(?), ''), label),
		   type = COALESCE(NULLIF(?, ''), type),
		   options = COALESCE(?::jsonb, options),
		   required = ?, sort = ?,
		   version = version + 1, updated_at = now()
		 WHERE id = ? AND space_id = ? AND deleted_at IS NULL AND version = ?`,
		f.Label, f.Type,
		mapToJSONOrNull(f.Options), f.Required, f.Sort,
		fieldRowID, spaceID, expectedVersion)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, httpx.New(409, httpx.CodeVersionConflict, "字段已被他人修改，请刷新后重试")
	}
	return s.getField(ctx, spaceID, fieldRowID)
}

// DeleteField 软删字段定义；标识不释放。
func (s *catalogService) DeleteField(ctx context.Context, actor httpx.Actor, spaceID, fieldRowID string) error {
	if err := s.RequireSpace(ctx, actor, spaceID, authz.ActionAdmin); err != nil {
		return err
	}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE space_field SET deleted_at = now(), updated_at = now()
		 WHERE id = ? AND space_id = ? AND deleted_at IS NULL`, fieldRowID, spaceID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return httpx.NotFound("字段不存在")
	}
	return nil
}

func (s *catalogService) getField(ctx context.Context, spaceID, fieldRowID string) (*FieldDef, error) {
	var row spaceFieldRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, field_id, label, type, options, required, sort, version
		 FROM space_field WHERE id = ? AND space_id = ? AND deleted_at IS NULL`,
		fieldRowID, spaceID).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, httpx.NotFound("字段不存在")
	}
	d := row.def()
	return &d, nil
}

func validFieldID(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// mapToJSONOrNull 空切片转 NULL，让 COALESCE 保留原值。
func mapToJSONOrNull(v []string) any {
	if len(v) == 0 {
		return nil
	}
	return marshalJSON(v)
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}
