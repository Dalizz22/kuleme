// 维修保养：一件至多一条未结维修。
// 维修不搬库存——物品仍在库内，只是档案状态变 maintenance（viewSQL 派生）。
package item

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/catalog"
	"github.com/klm/server/internal/httpx"
)

// MaintenanceRecord 维修记录视图。
type MaintenanceRecord struct {
	ID       string     `json:"id"`
	ItemID   string     `json:"item_id"`
	Reason   string     `json:"reason"`
	Note     string     `json:"note"`
	OpenedBy string     `json:"opened_by"`
	OpenedAt string     `json:"opened_at"`
	ClosedBy *string    `json:"closed_by"`
	ClosedAt *time.Time `json:"closed_at"`
}

type maintenanceInput struct {
	Reason string `json:"reason" binding:"required,max=200"`
	Note   string `json:"note" binding:"max=500"`
	// OperationID 是幂等键（提交证据），不进请求体。
	OperationID string `json:"-"`
}

type openRecordRow struct {
	ID        string
	SpaceID   string `gorm:"column:space_id"`
	Lifecycle string
	Reason    string
	Note      string
	OpenedAt  time.Time
}

// StartMaintenance 送修。终态物品不可送修；已有未结维修 409。
func (s *itemService) StartMaintenance(ctx context.Context, actor httpx.Actor, id string, in *maintenanceInput) (*MaintenanceRecord, error) {
	row, herr := s.maintenanceGate(ctx, actor, id)
	if herr != nil {
		return nil, herr
	}
	// 库关闭「维修」后拒绝新的送修；已开工的维修必须能关门（CloseMaintenance 不拦）。
	if err := s.featureGate(ctx, row.SpaceID, catalog.FeatureMaintenance); err != nil {
		return nil, err
	}
	if row.Lifecycle != "normal" {
		return nil, httpx.New(409, httpx.CodeOpenBusiness, "物品当前状态不允许送修")
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		return nil, httpx.Validation("维修原因必填")
	}
	var rec *MaintenanceRecord
	txErr := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 原子抢占：maintenance_open false→true，双送修一胜一败。
		res := tx.Exec(
			`UPDATE item SET maintenance_open = true, version = version + 1, updated_at = now()
			 WHERE id = ? AND maintenance_open = false AND lifecycle = 'normal' AND deleted_at IS NULL`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return httpx.New(409, httpx.CodeOpenBusiness, "该物品已有未结维修或状态已变化")
		}
		recID := uuid.NewString()
		if err := tx.Exec(
			`INSERT INTO maintenance_record (id, item_id, reason, note, opened_by, operation_id)
			 VALUES (?, ?, ?, ?, ?, NULLIF(?::text, '')::uuid)`,
			recID, id, in.Reason, in.Note, actor.UserID, in.OperationID).Error; err != nil {
			return err
		}
		rec = &MaintenanceRecord{ID: recID, ItemID: id, Reason: in.Reason, Note: in.Note,
			OpenedBy: actor.UserID, OpenedAt: time.Now().Format(time.RFC3339)}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rec, nil
}

// CloseMaintenance 结清维修。只影响 maintenance_open 标记与记录行。
func (s *itemService) CloseMaintenance(ctx context.Context, actor httpx.Actor, id, operationID string) (*MaintenanceRecord, error) {
	if _, herr := s.maintenanceGate(ctx, actor, id); herr != nil {
		return nil, herr
	}
	var rec *MaintenanceRecord
	txErr := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE item SET maintenance_open = false, version = version + 1, updated_at = now()
			 WHERE id = ? AND maintenance_open = true`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return httpx.New(409, httpx.CodeOpenBusiness, "该物品没有未结维修")
		}
		if err := tx.Exec(
			`UPDATE maintenance_record SET closed_by = ?, closed_at = now(),
			        close_operation_id = NULLIF(?::text, '')::uuid
			 WHERE id = (SELECT id FROM maintenance_record
			             WHERE item_id = ? AND closed_at IS NULL
			             ORDER BY opened_at DESC LIMIT 1)`,
			actor.UserID, operationID, id).Error; err != nil {
			return err
		}
		var row struct {
			ID       string
			Reason   string
			Note     string
			OpenedAt time.Time
			ClosedAt *time.Time
		}
		if err := tx.WithContext(ctx).Raw(
			`SELECT id, reason, note, opened_at, closed_at FROM maintenance_record
			 WHERE item_id = ? ORDER BY opened_at DESC LIMIT 1`, id).Scan(&row).Error; err != nil {
			return err
		}
		rec = &MaintenanceRecord{ID: row.ID, ItemID: id, Reason: row.Reason, Note: row.Note,
			OpenedBy: actor.UserID, OpenedAt: row.OpenedAt.Format(time.RFC3339), ClosedAt: row.ClosedAt}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rec, nil
}

// MaintenanceHistory 维修历史。
func (s *itemService) MaintenanceHistory(ctx context.Context, actor httpx.Actor, id string) ([]MaintenanceRecord, error) {
	if _, herr := s.maintenanceGate(ctx, actor, id); herr != nil {
		return nil, herr
	}
	var rows []struct {
		ID       string
		Reason   string
		Note     string
		OpenedBy string `gorm:"column:opened_by"`
		OpenedAt time.Time
		ClosedBy *string
		ClosedAt *time.Time
	}
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, reason, note, opened_by, opened_at, closed_by, closed_at
		 FROM maintenance_record WHERE item_id = ? ORDER BY opened_at DESC LIMIT 50`, id).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]MaintenanceRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, MaintenanceRecord{ID: r.ID, ItemID: id, Reason: r.Reason, Note: r.Note,
			OpenedBy: r.OpenedBy, OpenedAt: r.OpenedAt.Format(time.RFC3339),
			ClosedBy: r.ClosedBy, ClosedAt: r.ClosedAt})
	}
	return out, nil
}

// maintenanceGate 物品存在 + 库写权限（维修是写操作）。
func (s *itemService) maintenanceGate(ctx context.Context, actor httpx.Actor, id string) (*openRecordRow, error) {
	var row openRecordRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT i.id, i.space_id, i.lifecycle, COALESCE(r.reason,'') AS reason,
		        COALESCE(r.note,'') AS note, COALESCE(r.opened_at, now()) AS opened_at
		 FROM item i
		 LEFT JOIN maintenance_record r ON r.item_id = i.id AND r.closed_at IS NULL
		 WHERE i.id = ? AND i.deleted_at IS NULL`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, httpx.NotFound("物品不存在")
	}
	if err := s.RequireSpace(ctx, actor, row.SpaceID, authz.ActionWrite); err != nil {
		return nil, err
	}
	return &row, nil
}
