// 规则执行引擎：入队 → worker（SKIP LOCKED 租约）→ 白名单动作 → 步骤留痕 → 撤回补偿。
package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
)

// TriggerFieldChange 在物品字段变化后入队 field_changed 规则。
// itemID 所属库的规则只操作本物件（不跨库联动）。失败仅记日志，不影响业务请求。
// itemVersion 参与去重键：同一物件同一版本只投递一次（重试安全，每次变更都会推进版本）。
func (s *Service) TriggerFieldChange(ctx context.Context, itemID, spaceID string, changedFields map[string]bool, itemVersion int) {
	if !s.db.Available() {
		return
	}
	var rows []ruleRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, space_id, name, enabled, trigger_type, config::text AS config, actions::text AS actions,
		        created_by, version, created_at, updated_at
		 FROM rule WHERE space_id = ? AND enabled = true AND deleted_at IS NULL AND trigger_type = 'field_changed'`, spaceID).
		Scan(&rows).Error
	if err != nil {
		slog.Warn("规则入队查询失败", "err", err)
		return
	}
	for _, r := range rows {
		cfg := map[string]any{}
		_ = json.Unmarshal([]byte(r.Config), &cfg)
		fid, _ := cfg["field_id"].(string)
		if !changedFields[fid] {
			continue
		}
		s.enqueue(ctx, r, itemID, fmt.Sprintf("fc:%s:%s:i%d", r.ID, itemID, itemVersion))
	}
}

// ScanDateRules 每天/启动时扫描 date 触发：字段日期 <= 今天+提前天数 → 入队提醒。
// run_key 唯一保证同一物件同一规则只发一次（改日期换 key 重新触发）。
func (s *Service) ScanDateRules(ctx context.Context) {
	if !s.db.Available() {
		return
	}
	var rows []struct {
		RuleID     string `gorm:"column:rule_id"`
		Version    int
		ItemID     string `gorm:"column:item_id"`
		CreatedBy  string `gorm:"column:created_by"`
		FieldValue string `gorm:"column:fv"`
	}
	err := s.db.Gorm.WithContext(ctx).Raw(`
SELECT r.id AS rule_id, r.version, r.created_by, i.id AS item_id,
       COALESCE(i.custom_fields->'values'->>(cfg->>'date_field_id'), '') AS fv
FROM rule r
JOIN item i ON i.space_id = r.space_id AND i.deleted_at IS NULL AND i.lifecycle = 'normal'
CROSS JOIN LATERAL (SELECT r.config AS cfg) x
WHERE r.enabled = true AND r.deleted_at IS NULL AND r.trigger_type = 'date'
  AND COALESCE(i.custom_fields->'values'->>(r.config->>'date_field_id'), '') <> ''
  AND COALESCE(i.custom_fields->'values'->>(r.config->>'date_field_id'), '') <= to_char(now() + COALESCE((r.config->>'advance_days')::int, 0) * interval '1 day', 'YYYY-MM-DD')`).
		Scan(&rows).Error
	if err != nil {
		slog.Warn("date 规则扫描失败", "err", err)
		return
	}
	for _, r := range rows {
		var rule ruleRow
		if err := s.db.Gorm.WithContext(ctx).Raw(
			`SELECT id, space_id, name, enabled, trigger_type, config::text AS config, actions::text AS actions,
			        created_by, version, created_at, updated_at
			 FROM rule WHERE id = ?`, r.RuleID).Scan(&rule).Error; err != nil || rule.ID == "" {
			continue
		}
		s.enqueue(ctx, rule, r.ItemID, fmt.Sprintf("date:%s:%s:%s", r.RuleID, r.ItemID, r.FieldValue))
	}
}

func (s *Service) enqueue(ctx context.Context, r ruleRow, itemID, runKey string) {
	// run_key 唯一冲突 = 已投递，幂等跳过。
	err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO rule_run (id, rule_id, rule_version, item_id, run_key)
		 VALUES (?, ?, ?, ?, ?) ON CONFLICT (run_key) DO NOTHING`,
		uuid.NewString(), r.ID, r.Version, itemID, runKey).Error
	if err != nil {
		slog.Warn("规则入队失败", "rule", r.ID, "err", err)
	}
}

// RunWorker 跑一轮：认领 due 任务并执行。返回本轮处理数。
func (s *Service) RunWorker(ctx context.Context) int {
	if !s.db.Available() {
		return 0
	}
	var runs []struct {
		ID string
	}
	// 认领租约：认领一次性写 60s 到期时间；running 且租约过期 = worker
	// 崩溃遗留，允许重新认领（崩溃回收）。SKIP LOCKED 防多 worker 抢同一行。
	err := s.db.Gorm.WithContext(ctx).Raw(fmt.Sprintf(`
UPDATE rule_run SET status='running', started_at=now(), attempts=attempts+1,
       lease_expires_at=now() + (? * interval '1 second')
WHERE id IN (
  SELECT id FROM rule_run
  WHERE status='queued' OR (status='running' AND lease_expires_at < now())
  ORDER BY created_at LIMIT 10
  FOR UPDATE SKIP LOCKED
) RETURNING id`), leaseSeconds).Scan(&runs).Error
	if err != nil || len(runs) == 0 {
		return 0
	}
	for _, r := range runs {
		s.executeRun(ctx, r.ID)
	}
	return len(runs)
}

// executeRun 执行单次运行：逐步留痕；规则删除/失权 → skipped。
func (s *Service) executeRun(ctx context.Context, runID string) {
	var run struct {
		ID       string
		RuleID   string `gorm:"column:rule_id"`
		ItemID   string `gorm:"column:item_id"`
		Attempts int
	}
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, rule_id, item_id, attempts FROM rule_run WHERE id = ? AND status = 'running'`, runID).
		Scan(&run).Error; err != nil || run.ID == "" {
		return
	}
	rule, err := s.load(ctx, run.RuleID)
	if err != nil || !rule.Enabled {
		s.finishRun(ctx, runID, "skipped", "规则已删除或停用")
		return
	}
	acts := rule.viewActions()
	cfg := map[string]any{}
	_ = json.Unmarshal([]byte(rule.Config), &cfg)

	// AUTO-04 复检：配置人当前必须仍有该库管理权；否则整轮跳过（只暂停该规则影响）。
	creator := httpx.Actor{UserID: rule.CreatedBy}
	if err := s.authz.Check(ctx, creator, rule.SpaceID, authz.ActionAdmin); err != nil {
		s.finishRun(ctx, runID, "skipped", "配置人已无该库管理权，规则暂停生效")
		return
	}

	// 跨库复检：物件可能在入队后被移动到其他库（或删除），动作循环前拦截。
	var itemSpace string
	s.db.Gorm.WithContext(ctx).Raw(
		`SELECT space_id FROM item WHERE id = ? AND deleted_at IS NULL`, run.ItemID).Scan(&itemSpace)
	if itemSpace != rule.SpaceID {
		s.finishRun(ctx, runID, "skipped", "物件已不在规则所属库（或已删除），本轮跳过")
		return
	}

	var steps []stepRecord
	failed := ""
	for i, a := range acts {
		rec := s.applyAction(ctx, creator, runID, i, run.ItemID, a)
		steps = append(steps, rec)
		if rec.status == "failed" && failed == "" {
			failed = rec.detail
		}
	}
	status := "succeeded"
	if failed != "" {
		status = "failed"
	}
	s.writeSteps(ctx, runID, steps)
	s.finishRun(ctx, runID, status, failed)
	// 瞬时失败且未超上限 → 重新排队（有限重试）。
	if status == "failed" {
		var attempts int
		_ = s.db.Gorm.WithContext(ctx).Raw(`SELECT attempts FROM rule_run WHERE id = ?`, runID).Scan(&attempts).Error
		if attempts < maxAttempts {
			s.db.Gorm.WithContext(ctx).Exec(
				`UPDATE rule_run SET status='queued', started_at=NULL WHERE id = ? AND status='failed'`, runID)
		}
	}
}

type stepRecord struct {
	seq    int
	action string
	target string
	ver    *int
	before any
	after  any
	status string
	detail string
}

func (s *Service) applyAction(ctx context.Context, actor httpx.Actor, runID string, seq int, itemID string, a Action) stepRecord {
	rec := stepRecord{seq: seq, action: a.Type, target: a.FieldID, status: "done"}
	switch a.Type {
	case "set_field":
		val := a.Value
		if s, ok := val.(string); ok && s == "__today__" {
			val = time.Now().Format("2006-01-02")
		}
		// 读当前值与版本（乐观撤回基准）。
		var cur struct {
			Version int             `gorm:"column:version"`
			SpaceID string          `gorm:"column:space_id"`
			Fields  json.RawMessage `gorm:"column:custom_fields"`
			Name    string
			Note    string
		}
		if err := s.db.Gorm.WithContext(ctx).Raw(
			`SELECT version, space_id, custom_fields, name, note FROM item WHERE id = ? AND deleted_at IS NULL AND lifecycle = 'normal'`, itemID).
			Scan(&cur).Error; err != nil || cur.Version == 0 {
			rec.status, rec.detail = "failed", "物件不存在或状态不允许"
			return rec
		}
		// 非核心字段必须在该库字段定义中存在（未删）且值形状匹配。
		if !coreWritableFields[a.FieldID] {
			var sf struct {
				Type    string          `gorm:"column:type"`
				Options json.RawMessage `gorm:"column:options"`
			}
			if err := s.db.Gorm.WithContext(ctx).Raw(
				`SELECT type, options FROM space_field WHERE space_id = ? AND field_id = ? AND deleted_at IS NULL`,
				cur.SpaceID, a.FieldID).Scan(&sf).Error; err != nil || sf.Type == "" {
				rec.status, rec.detail = "skipped", "字段未在该库定义（或已删除），跳过"
				return rec
			}
			if reason := fieldValueTypeOK(val, sf.Type, sf.Options); reason != "" {
				rec.status, rec.detail = "skipped", reason
				return rec
			}
		}
		var before any
		var res *gorm.DB
		if coreWritableFields[a.FieldID] {
			if a.FieldID == "name" {
				before = cur.Name
				res = s.db.Gorm.WithContext(ctx).Exec(
					`UPDATE item SET name = ?, version = version + 1, updated_at = now() WHERE id = ? AND version = ?`,
					fmt.Sprintf("%v", val), itemID, cur.Version)
			} else {
				before = cur.Note
				res = s.db.Gorm.WithContext(ctx).Exec(
					`UPDATE item SET note = ?, version = version + 1, updated_at = now() WHERE id = ? AND version = ?`,
					fmt.Sprintf("%v", val), itemID, cur.Version)
			}
		} else {
			fields := map[string]any{}
			_ = json.Unmarshal(cur.Fields, &fields)
			values, _ := fields["values"].(map[string]any)
			if values == nil {
				values = map[string]any{}
			}
			before = values[a.FieldID]
			values[a.FieldID] = val
			fields["values"] = values
			b, _ := json.Marshal(fields)
			res = s.db.Gorm.WithContext(ctx).Exec(
				`UPDATE item SET custom_fields = ?::jsonb, version = version + 1, updated_at = now()
				 WHERE id = ? AND version = ?`, string(b), itemID, cur.Version)
		}
		// 乐观锁 0 行 = 物件被并发修改，本步记 failed（不留版本，不进 undo）。
		if res.Error != nil || res.RowsAffected == 0 {
			rec.status, rec.detail = "failed", "物件已被并发修改（版本冲突），本步未生效"
			return rec
		}
		v := cur.Version + 1
		rec.ver, rec.before, rec.after = &v, before, val
	case "move":
		// 位置移动 = 库存余额搬家（on_hand=1 语义），事务内走台账。
		// 无余额（或仅未定位余额）视为已不在库 → skipped；目标位置不存在 → skipped。
		err := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var src struct {
				SpaceID    string  `gorm:"column:space_id"`
				LocationID *string `gorm:"column:location_id"`
				OnHand     float64 `gorm:"column:on_hand"`
			}
			if err := tx.Raw(
				`SELECT i.space_id, b.location_id, b.on_hand
				 FROM item i JOIN stock_balance b ON b.item_id = i.id
				 WHERE i.id = ? AND b.on_hand > 0
				 ORDER BY b.location_id NULLS LAST LIMIT 1`, itemID).Scan(&src).Error; err != nil {
				return err
			}
			if src.OnHand <= 0 || src.LocationID == nil || *src.LocationID == "" {
				rec.status, rec.detail = "skipped", "物件不在库，无法移动"
				return nil
			}
			from := *src.LocationID
			if from == a.LocationID {
				rec.status, rec.detail = "skipped", "已在目标位置"
				return nil
			}
			// 目标位置必须真实存在且未删除。
			var locCount int
			if err := tx.Raw(
				`SELECT count(*) FROM location WHERE id = ? AND deleted_at IS NULL`, a.LocationID).
				Scan(&locCount).Error; err != nil {
				return err
			}
			if locCount == 0 {
				rec.status, rec.detail = "skipped", "目标位置不存在或已删除，跳过"
				return nil
			}
			res := tx.Exec(
				`UPDATE stock_balance SET location_id = ?, updated_at = now()
				 WHERE item_id = ? AND location_id = ? AND on_hand > 0`, a.LocationID, itemID, from)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("余额已变化")
			}
			// 出库/入库各留一条流水，operation_id 用 run_id 保证可追溯。
			reason := "规则自动移动 " + runID
			for _, mv := range []struct {
				loc   string
				kind  string
				delta string
			}{{from, "move_out", "-1"}, {a.LocationID, "move_in", "1"}} {
				if err := tx.Exec(
					`INSERT INTO stock_movement (id, item_id, location_id, kind, quantity, on_hand_delta, reason, operation_id, actor_id, owning_space_id)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					uuid.NewString(), itemID, mv.loc, mv.kind, "1", mv.delta, reason, runID, actor.UserID, src.SpaceID).Error; err != nil {
					return err
				}
			}
			// 移动也推进物件版本，令同 run 后续 set_field 的乐观基准失效重读。
			if err := tx.Exec(
				`UPDATE item SET version = version + 1, updated_at = now() WHERE id = ?`, itemID).Error; err != nil {
				return err
			}
			rec.before, rec.after = from, a.LocationID
			return nil
		})
		if err != nil {
			rec.status, rec.detail = "failed", "移动失败："+err.Error()
		}
	case "notify":
		// 通知配置人（发起者本人）。
		title := a.Title
		body := fmt.Sprintf("规则自动提醒：物品 %s", itemID)
		s.db.Gorm.WithContext(ctx).Exec(
			`INSERT INTO message (id, recipient_id, title, body, run_id) VALUES (?, ?, ?, ?, ?)`,
			uuid.NewString(), actor.UserID, title, body, runID)
		rec.detail = "notified"
	}
	return rec
}

func (s *Service) writeSteps(ctx context.Context, runID string, steps []stepRecord) {
	for _, st := range steps {
		b, _ := json.Marshal(st.before)
		a, _ := json.Marshal(st.after)
		var ver *int = st.ver
		s.db.Gorm.WithContext(ctx).Exec(
			`INSERT INTO rule_step (id, run_id, seq, action, target, item_version, before_value, after_value, status, detail)
			 VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?::jsonb, ?, ?)
			 ON CONFLICT (run_id, seq) DO NOTHING`,
			uuid.NewString(), runID, st.seq, st.action, st.target, ver, string(b), string(a), st.status, st.detail)
	}
}

func (s *Service) finishRun(ctx context.Context, runID, status, errMsg string) {
	s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE rule_run SET status=?, error=NULLIF(?,''), finished_at=now() WHERE id=? AND status='running'`,
		status, errMsg, runID)
}

// RunView 是运行详情（含步骤）。
type RunView struct {
	ID       string     `json:"id"`
	RuleID   string     `json:"rule_id"`
	ItemID   string     `json:"item_id"`
	Status   string     `json:"status"`
	Error    string     `json:"error,omitempty"`
	Attempts int        `json:"attempts"`
	Steps    []StepView `json:"steps"`
}

type StepView struct {
	Seq     int             `json:"seq"`
	Action  string          `json:"action"`
	Target  string          `json:"target"`
	Status  string          `json:"status"`
	Version int             `json:"version,omitempty"`
	Before  json.RawMessage `json:"before,omitempty"`
	After   json.RawMessage `json:"after,omitempty"`
	Detail  string          `json:"detail,omitempty"`
	Undone  bool            `json:"undone"`
}

// GetRun 读运行（需该库读权）。
func (s *Service) GetRun(ctx context.Context, actor httpx.Actor, runID string) (*RunView, error) {
	var run struct {
		ID       string
		RuleID   string `gorm:"column:rule_id"`
		ItemID   string `gorm:"column:item_id"`
		Status   string
		Error    string
		Attempts int
		SpaceID  string `gorm:"column:space_id"`
	}
	err := s.db.Gorm.WithContext(ctx).Raw(`
SELECT rr.id, rr.rule_id, rr.item_id, rr.status, COALESCE(rr.error,'') AS error, rr.attempts, r.space_id
FROM rule_run rr JOIN rule r ON r.id = rr.rule_id WHERE rr.id = ?`, runID).Scan(&run).Error
	if err != nil {
		return nil, err
	}
	if run.ID == "" {
		return nil, httpx.NotFound("运行不存在")
	}
	if err := s.authz.Check(ctx, actor, run.SpaceID, authz.ActionRead); err != nil {
		return nil, err
	}
	var steps []struct {
		Seq      int `gorm:"column:seq"`
		Action   string
		Target   string
		Status   string
		Ver      *int            `gorm:"column:item_version"`
		Before   json.RawMessage `gorm:"column:before_value"`
		After    json.RawMessage `gorm:"column:after_value"`
		Detail   string
		UndoneAt *time.Time `gorm:"column:undone_at"`
	}
	s.db.Gorm.WithContext(ctx).Raw(
		`SELECT seq, action, target, status, item_version, before_value, after_value,
		        COALESCE(detail,'') AS detail, undone_at
		 FROM rule_step WHERE run_id = ? ORDER BY seq`, runID).Scan(&steps)
	views := make([]StepView, 0, len(steps))
	for _, st := range steps {
		v := 0
		if st.Ver != nil {
			v = *st.Ver
		}
		views = append(views, StepView{Seq: st.Seq, Action: st.Action, Target: st.Target,
			Status: st.Status, Version: v, Before: st.Before, After: st.After,
			Detail: st.Detail, Undone: st.UndoneAt != nil})
	}
	return &RunView{ID: run.ID, RuleID: run.RuleID, ItemID: run.ItemID, Status: run.Status,
		Error: run.Error, Attempts: run.Attempts, Steps: views}, nil
}

// Undo 逆序补偿（AUTO-05）：成功步骤倒序撤回。
// set_field：仅当物件版本仍等于本步落值后的版本时恢复 before（否则冲突保留并给原因）；
// move：把余额移回 before 位置；notify 无副作用无需撤。
func (s *Service) Undo(ctx context.Context, actor httpx.Actor, runID string) (*RunView, error) {
	run, err := s.GetRun(ctx, actor, runID)
	if err != nil {
		return nil, err
	}
	if run.Status == "undone" {
		return nil, httpx.New(409, httpx.CodeOpenBusiness, "运行已撤回，请手动重新发起")
	}
	if run.Status != "succeeded" && run.Status != "failed" {
		return nil, httpx.New(409, httpx.CodeOpenBusiness, "运行尚未结束，不能撤回")
	}
	// 需要写权限才能撤。
	var spaceID string
	s.db.Gorm.WithContext(ctx).Raw(`SELECT space_id FROM rule WHERE id = ?`, run.RuleID).Scan(&spaceID)
	if err := s.authz.Check(ctx, actor, spaceID, authz.ActionWrite); err != nil {
		return nil, err
	}
	conflicts := []string{}
	for i := len(run.Steps) - 1; i >= 0; i-- {
		st := run.Steps[i]
		if st.Status != "done" || st.Undone {
			continue
		}
		var undoneAt *time.Time
		switch st.Action {
		case "set_field":
			// 可恢复性按「步骤 after_value 与当前值一致」判定。
			// 逆序补偿每恢复一步版本 +1，版本比对（cur.Version == st.Version）会把
			// 本 run 多步 set_field 的自补偿误判为外部冲突；值不一致才保留并给原因。
			var cur struct {
				Version int             `gorm:"column:version"`
				Fields  json.RawMessage `gorm:"column:custom_fields"`
				Name    string
				Note    string
			}
			if err := s.db.Gorm.WithContext(ctx).Raw(
				`SELECT version, custom_fields, name, note FROM item WHERE id = ? AND deleted_at IS NULL`, run.ItemID).
				Scan(&cur).Error; err != nil || cur.Version == 0 {
				conflicts = append(conflicts, fmt.Sprintf("步骤%d：物件不存在", st.Seq))
				continue
			}
			var current any
			switch st.Target {
			case "name":
				current = cur.Name
			case "note":
				current = cur.Note
			default:
				fields := map[string]any{}
				_ = json.Unmarshal(cur.Fields, &fields)
				values, _ := fields["values"].(map[string]any)
				current = values[st.Target] // 字段缺失时为 nil，与 after=null 对齐
			}
			if !setFieldRecoverable(st.After, current) {
				conflicts = append(conflicts, fmt.Sprintf("步骤%d：字段当前值与本轮落值不一致（已被外部修改），保留不撤", st.Seq))
				continue
			}
			var before any
			_ = json.Unmarshal(st.Before, &before)
			if before == nil {
				before = ""
			}
			if st.Target == "name" || st.Target == "note" {
				col := st.Target
				s.db.Gorm.WithContext(ctx).Exec(
					fmt.Sprintf(`UPDATE item SET %s = ?, version = version + 1, updated_at = now() WHERE id = ?`, col),
					fmt.Sprintf("%v", before), run.ItemID)
			} else {
				fields := map[string]any{}
				_ = json.Unmarshal(cur.Fields, &fields)
				values, _ := fields["values"].(map[string]any)
				if values == nil {
					values = map[string]any{}
				}
				if s, ok := before.(string); ok && s == "" {
					delete(values, st.Target)
				} else {
					values[st.Target] = before
				}
				fields["values"] = values
				b, _ := json.Marshal(fields)
				s.db.Gorm.WithContext(ctx).Exec(
					`UPDATE item SET custom_fields = ?::jsonb, version = version + 1, updated_at = now() WHERE id = ?`,
					string(b), run.ItemID)
			}
			undoneAt = ptrTime(time.Now())
		case "move":
			var back string
			_ = json.Unmarshal(st.Before, &back)
			if back == "" {
				continue
			}
			res := s.db.Gorm.WithContext(ctx).Exec(
				`UPDATE stock_balance SET location_id = ?, updated_at = now()
				 WHERE item_id = ? AND location_id = ? AND on_hand > 0`, back, run.ItemID, st.After)
			if res.Error != nil || res.RowsAffected == 0 {
				conflicts = append(conflicts, fmt.Sprintf("步骤%d：移动撤回冲突，余额已变化", st.Seq))
				continue
			}
			undoneAt = ptrTime(time.Now())
		default:
			continue
		}
		if undoneAt != nil {
			s.db.Gorm.WithContext(ctx).Exec(
				`UPDATE rule_step SET undone_at = now() WHERE run_id = ? AND seq = ?`, runID, st.Seq)
		}
	}
	if len(conflicts) > 0 {
		// 部分冲突：保留已完成补偿的步骤结果（可再试剩余），运行不标 undone。
		return nil, httpx.New(409, httpx.CodeOpenBusiness, "部分步骤撤回冲突："+strings.Join(conflicts, "；"))
	}
	if run.Status == "succeeded" {
		s.db.Gorm.WithContext(ctx).Exec(
			`UPDATE rule_run SET status = 'undone' WHERE id = ?`, runID)
	}
	return s.GetRun(ctx, actor, runID)
}

// jsonValuesEqual 比较两个 JSON 反序列化后的值（数字统一 float64）。
func jsonValuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.DeepEqual(a, b)
}

// setFieldRecoverable 判定 set_field 步骤能否撤回：
// 步骤记录的 after_value 与当前值一致 → 当前值仍由本轮写入，可恢复；
// 不一致 → 视为外部修改，冲突保留。版本号不参与判定（自补偿会推进版本）。
func setFieldRecoverable(stepAfter json.RawMessage, current any) bool {
	var after any
	if err := json.Unmarshal(stepAfter, &after); err != nil {
		return false
	}
	return jsonValuesEqual(after, current)
}

// fieldValueTypeOK 校验动作值与库字段定义的类型存在性 + 基本形状。
// 通过返回 ""，否则返回 skipped 原因。optionsRaw 为 space_field.options（jsonb 数组）。
func fieldValueTypeOK(val any, ftype string, optionsRaw json.RawMessage) string {
	switch ftype {
	case "text", "date":
		if _, ok := val.(string); !ok {
			return "字段类型为 " + ftype + "，动作值需为字符串"
		}
	case "number":
		if _, ok := val.(float64); !ok {
			return "字段类型为 number，动作值需为数值"
		}
	case "select":
		v, ok := val.(string)
		if !ok {
			return "字段类型为 select，动作值需为字符串"
		}
		var opts []any
		_ = json.Unmarshal(optionsRaw, &opts)
		for _, o := range opts {
			if os, ok := o.(string); ok && os == v {
				return ""
			}
		}
		return "select 值不在字段选项内"
	case "multiselect":
		arr, ok := val.([]any)
		if !ok {
			return "字段类型为 multiselect，动作值需为数组"
		}
		optSet := map[string]bool{}
		var opts []any
		_ = json.Unmarshal(optionsRaw, &opts)
		for _, o := range opts {
			if os, ok := o.(string); ok {
				optSet[os] = true
			}
		}
		for _, v := range arr {
			vs, ok := v.(string)
			if !ok || !optSet[vs] {
				return "multiselect 值不在字段选项内"
			}
		}
	default:
		return "未知字段类型 " + ftype
	}
	return ""
}

var _ = gorm.ErrRecordNotFound

func ptrTime(t time.Time) *time.Time { return &t }
