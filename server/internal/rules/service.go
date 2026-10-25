// Package rules 实现 有限自动规则。
//
// 白名单（D-边界）：触发=字段变化/日期临近；动作=改普通字段（含标签）、
// 移动位置、站内提醒。不做金额/归属库/库存/出售报废转库租赁。
// 执行器为同进程静态 worker（租约 PG SKIP LOCKED）；撤回走逆序补偿。
package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

const (
	maxActions       = 5
	maxConditionSize = 10
	workerInterval   = 30 * time.Second
	leaseSeconds     = 60
	maxAttempts      = 3
)

// Action 是白名单动作。
type Action struct {
	Type       string `json:"type"` // set_field | move | notify
	FieldID    string `json:"field_id,omitempty"`
	Value      any    `json:"value,omitempty"` // 字面值；"__today__" 表示当前日期
	LocationID string `json:"location_id,omitempty"`
	Title      string `json:"title,omitempty"`
}

// Rule 是规则定义视图。
type Rule struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Enabled   bool           `json:"enabled"`
	Trigger   string         `json:"trigger_type"`
	Config    map[string]any `json:"config"`
	Actions   []Action       `json:"actions"`
	CreatedBy string         `json:"created_by"`
	Version   int            `json:"version"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

type ruleRow struct {
	ID        string
	SpaceID   string `gorm:"column:space_id"`
	Name      string
	Enabled   bool
	Trigger   string `gorm:"column:trigger_type"`
	Config    string `gorm:"column:config"`
	Actions   string
	CreatedBy string `gorm:"column:created_by"`
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (r ruleRow) view() (*Rule, error) {
	cfg := map[string]any{}
	_ = json.Unmarshal([]byte(r.Config), &cfg)
	var acts []Action
	_ = json.Unmarshal([]byte(r.Actions), &acts)
	if acts == nil {
		acts = []Action{}
	}
	return &Rule{ID: r.ID, Name: r.Name, Enabled: r.Enabled, Trigger: r.Trigger,
		Config: cfg, Actions: acts, CreatedBy: r.CreatedBy, Version: r.Version,
		CreatedAt: r.CreatedAt.Format(time.RFC3339), UpdatedAt: r.UpdatedAt.Format(time.RFC3339)}, nil
}

var validFieldIDRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
var coreWritableFields = map[string]bool{
	"name": true, "note": true,
}

// forbiddenActionFields 是 set_field 的硬边界：金额/归属/码/库存/系统字段
// 永远不可经规则动作写（AUTO-01 白名单的反向兜底，与模板 reserved 集互补）。
var forbiddenActionFields = map[string]bool{
	"quantity": true, "purchase_price": true, "price": true,
	"public_code": true, "space_id": true, "lifecycle": true,
	"version": true, "created_at": true, "updated_at": true, "deleted_at": true,
}

// Service 提供规则 CRUD、触发与运行查询。
type Service struct {
	db    *store.DB
	authz *authz.Checker
}

func NewService(db *store.DB) *Service {
	return &Service{db: db, authz: authz.New(db)}
}

// validateRule 校验触发配置与动作白名单（AUTO-01）。
func validateRule(trigger string, cfg map[string]any, actions []Action) error {
	switch trigger {
	case "field_changed":
		fid, _ := cfg["field_id"].(string)
		if !validFieldIDRe.MatchString(fid) {
			return httpx.Validation("field_changed 触发需要合法的 field_id")
		}
	case "date":
		fid, _ := cfg["date_field_id"].(string)
		if !validFieldIDRe.MatchString(fid) {
			return httpx.Validation("date 触发需要合法的 date_field_id")
		}
		if adv, ok := cfg["advance_days"].(float64); ok && (adv < 0 || adv > 365) {
			return httpx.Validation("advance_days 需在 0..365")
		}
	default:
		return httpx.Validation("trigger_type 必须是 field_changed 或 date")
	}
	if len(cfg) > maxConditionSize {
		return httpx.Validation("触发条件过多")
	}
	if len(actions) == 0 || len(actions) > maxActions {
		return httpx.Validation("动作需 1..5 个")
	}
	for _, a := range actions {
		switch a.Type {
		case "set_field":
			if forbiddenActionFields[a.FieldID] {
				return httpx.Validation("动作字段 " + a.FieldID + " 属于金额/库存/系统字段，不允许规则改写")
			}
			if !validFieldIDRe.MatchString(a.FieldID) && !coreWritableFields[a.FieldID] {
				return httpx.Validation("动作字段 " + a.FieldID + " 不在白名单")
			}
			if a.Value == nil || fmt.Sprintf("%v", a.Value) == "" {
				return httpx.Validation("set_field 需要字面值（或 __today__）")
			}
		case "move":
			if _, err := uuid.Parse(a.LocationID); err != nil {
				return httpx.Validation("move 需要目标位置 id")
			}
		case "notify":
			if strings.TrimSpace(a.Title) == "" {
				return httpx.Validation("notify 需要标题")
			}
		default:
			return httpx.Validation("动作类型必须是 set_field/move/notify")
		}
	}
	return nil
}

// Create 新建规则。
func (s *Service) Create(ctx context.Context, actor httpx.Actor, spaceID string, in *Rule) (*Rule, error) {
	if err := s.authz.Check(ctx, actor, spaceID, authz.ActionAdmin); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 80 {
		return nil, httpx.Validation("规则名必填且不超过 80 字")
	}
	if err := validateRule(in.Trigger, in.Config, in.Actions); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	cfgB, _ := json.Marshal(in.Config)
	actB, _ := json.Marshal(in.Actions)
	err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO rule (id, space_id, name, enabled, trigger_type, config, actions, created_by)
		 VALUES (?, ?, ?, ?, ?, ?::jsonb, ?::jsonb, ?)`,
		id, spaceID, in.Name, in.Enabled, in.Trigger, string(cfgB), string(actB), actor.UserID).Error
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, actor, id)
}

// Get 读单条（需要库读权）。
func (s *Service) Get(ctx context.Context, actor httpx.Actor, id string) (*Rule, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authz.Check(ctx, actor, row.SpaceID, authz.ActionRead); err != nil {
		return nil, err
	}
	return row.view()
}

// ListSpace 列出库内规则。
func (s *Service) ListSpace(ctx context.Context, actor httpx.Actor, spaceID string) ([]*Rule, error) {
	if err := s.authz.Check(ctx, actor, spaceID, authz.ActionRead); err != nil {
		return nil, err
	}
	var rows []ruleRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, space_id, name, enabled, trigger_type, config::text AS config, actions::text AS actions,
		        created_by, version, created_at, updated_at
		 FROM rule WHERE space_id = ? AND deleted_at IS NULL ORDER BY created_at DESC`, spaceID).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]*Rule, 0, len(rows))
	for _, r := range rows {
		v, err := r.view()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Patch 局部更新（乐观锁）。nil 字段保持不变；enabled 仅显式传入才更新
// （PATCH 不带 enabled 不得静默把停用规则重新启用）。
func (s *Service) Patch(ctx context.Context, actor httpx.Actor, id string, in *Rule, expectedVersion int, enabled *bool) (*Rule, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authz.Check(ctx, actor, row.SpaceID, authz.ActionAdmin); err != nil {
		return nil, err
	}
	if expectedVersion < 1 {
		return nil, httpx.Validation("缺少版本")
	}
	newEnabled := row.Enabled
	if enabled != nil {
		newEnabled = *enabled
	}
	name := row.Name
	if in.Name != "" {
		name = in.Name
	}
	if in.Config != nil || len(in.Actions) > 0 || in.Trigger != "" {
		trigger := row.Trigger
		if in.Trigger != "" {
			trigger = in.Trigger
		}
		cfg := map[string]any{}
		_ = json.Unmarshal([]byte(row.Config), &cfg)
		if in.Config != nil {
			cfg = in.Config
		}
		acts := row.viewActions()
		if len(in.Actions) > 0 {
			acts = in.Actions
		}
		if err := validateRule(trigger, cfg, acts); err != nil {
			return nil, err
		}
		cfgB, _ := json.Marshal(cfg)
		actB, _ := json.Marshal(acts)
		res := s.db.Gorm.WithContext(ctx).Exec(
			`UPDATE rule SET name=?, enabled=?, trigger_type=?, config=?::jsonb, actions=?::jsonb,
			                version=version+1, updated_at=now()
			 WHERE id=? AND deleted_at IS NULL AND version=?`,
			name, newEnabled, trigger, string(cfgB), string(actB), id, expectedVersion)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			return nil, httpx.New(409, httpx.CodeVersionConflict, "规则已被他人修改，请刷新后重试")
		}
		return s.Get(ctx, actor, id)
	}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE rule SET name=?, enabled=?, version=version+1, updated_at=now()
		 WHERE id=? AND deleted_at IS NULL AND version=?`,
		name, newEnabled, id, expectedVersion)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, httpx.New(409, httpx.CodeVersionConflict, "规则已被他人修改，请刷新后重试")
	}
	return s.Get(ctx, actor, id)
}

func (r *ruleRow) viewActions() []Action {
	var acts []Action
	_ = json.Unmarshal([]byte(r.Actions), &acts)
	return acts
}

// Delete 软删规则（历史运行保留）。
func (s *Service) Delete(ctx context.Context, actor httpx.Actor, id string) error {
	row, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if err := s.authz.Check(ctx, actor, row.SpaceID, authz.ActionAdmin); err != nil {
		return err
	}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE rule SET deleted_at=now(), updated_at=now() WHERE id=? AND deleted_at IS NULL`, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return httpx.NotFound("规则不存在")
	}
	return nil
}

func (s *Service) load(ctx context.Context, id string) (*ruleRow, error) {
	var row ruleRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, space_id, name, enabled, trigger_type, config::text AS config, actions::text AS actions,
		        created_by, version, created_at, updated_at
		 FROM rule WHERE id = ? AND deleted_at IS NULL`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, httpx.NotFound("规则不存在")
	}
	return &row, nil
}
