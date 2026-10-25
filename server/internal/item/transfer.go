// 转库：把物品档案与在手库存整体移到另一个库。
//
// 历史归原库：转库产生的两条流水（move_out/move_in）的 owning_space_id
// 记原库，原库成员永远能查到自己任期内的历史。
// 逐件模型：在手余额只能是 0/1，转库就是把 1 从原位置搬到目标位置。
package item

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
)

// TransferInput 转库请求。expected_version 乐观锁防并发档案改动。
type TransferInput struct {
	ToSpaceID       string `json:"to_space_id" binding:"required"`
	ToLocationID    string `json:"to_location_id" binding:"required"`
	ExpectedVersion int    `json:"expected_version" binding:"required,min=1"`
}

// TransferPreview 是转库前的干跑结果，不落任何数据。
type TransferPreview struct {
	ItemID       string   `json:"item_id"`
	ItemName     string   `json:"item_name"`
	FromSpaceID  string   `json:"from_space_id"`
	FromLocID    string   `json:"from_location_id"`
	ToSpaceID    string   `json:"to_space_id"`
	ToLocationID string   `json:"to_location_id"`
	Warnings     []string `json:"warnings"`
}

type transferItemRow struct {
	ID              string
	Name            string
	SpaceID         string
	Lifecycle       string
	ForSale         bool
	Version         int
	MaintenanceOpen bool `gorm:"column:maintenance_open"`
}

// validateTransfer 共用校验：可见性、生命周期、目标库与目标位置。
// 返回源位置（可能在库）与警告列表；inStock=false 表示档案不在库。
func (s *itemService) validateTransfer(ctx context.Context, actor httpx.Actor, id string, in *TransferInput) (*transferItemRow, string, bool, []string, error) {
	var row transferItemRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, name, space_id, lifecycle, for_sale, version, maintenance_open
		 FROM item WHERE id = ? AND deleted_at IS NULL`, id).Scan(&row).Error
	if err != nil {
		return nil, "", false, nil, err
	}
	if row.ID == "" {
		return nil, "", false, nil, httpx.NotFound("物品不存在")
	}
	// 发起人对原库要有写权（转库动原库的账）。
	if err := s.RequireSpace(ctx, actor, row.SpaceID, authz.ActionWrite); err != nil {
		return nil, "", false, nil, err
	}
	// 维修中/非正常状态不允许转库。
	if err := occupiedGate(row.Lifecycle, row.MaintenanceOpen); err != nil {
		return nil, "", false, nil, err
	}
	// 目标库同样要有写权。
	if err := s.RequireSpace(ctx, actor, in.ToSpaceID, authz.ActionWrite); err != nil {
		return nil, "", false, nil, err
	}
	if in.ToSpaceID == row.SpaceID {
		return nil, "", false, nil, httpx.New(422, CodeInvalidTransferTarget, "目标库与当前库相同，库内移位请用库存移动")
	}
	// 目标位置须存在。位置树是全局共享的树（没有库归属列），
	// 与 inventory.move 同一信任级别：不校验位置-库归属（已知限制，字段化后收口）。
	var locID string
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id FROM location WHERE id = ? AND deleted_at IS NULL`, in.ToLocationID).
		Scan(&locID).Error; err != nil {
		return nil, "", false, nil, err
	}
	if locID == "" {
		return nil, "", false, nil, httpx.NotFound("目标位置不存在")
	}
	// 源位置：在手余额行（逐件最多一行 on_hand=1）。
	var srcLoc string
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT location_id FROM stock_balance
		 WHERE item_id = ? AND on_hand > 0 AND location_id IS NOT NULL
		 ORDER BY updated_at DESC LIMIT 1`, id).Scan(&srcLoc).Error; err != nil {
		return nil, "", false, nil, err
	}
	warnings := []string{}
	inStock := srcLoc != ""
	if !inStock {
		warnings = append(warnings, "物品当前不在库（无在手余额），将只转移档案")
	}
	return &row, srcLoc, inStock, warnings, nil
}

// TransferPreview 干跑转库，返回将发生的变化与警告，不写任何数据。
func (s *itemService) TransferPreview(ctx context.Context, actor httpx.Actor, id string, in *TransferInput) (*TransferPreview, error) {
	row, srcLoc, _, warnings, err := s.validateTransfer(ctx, actor, id, in)
	if err != nil {
		return nil, err
	}
	return &TransferPreview{
		ItemID: row.ID, ItemName: row.Name,
		FromSpaceID: row.SpaceID, FromLocID: srcLoc,
		ToSpaceID: in.ToSpaceID, ToLocationID: in.ToLocationID,
		Warnings: warnings,
	}, nil
}

// Transfer 执行转库：档案换库 + 在手余额迁移 + 两条流水（历史归原库）。
func (s *itemService) Transfer(ctx context.Context, actor httpx.Actor, id string, in *TransferInput) (*View, error) {
	row, srcLoc, inStock, _, err := s.validateTransfer(ctx, actor, id, in)
	if err != nil {
		return nil, err
	}
	fromSpace := row.SpaceID
	var view *View
	txErr := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if inStock {
			// 原位置出库（余额行必在，0 行=并发变动）。
			res := tx.Exec(
				`UPDATE stock_balance SET on_hand = on_hand - 1, updated_at = now()
				 WHERE item_id = ? AND location_id = ? AND on_hand > 0`, id, srcLoc)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return httpx.New(409, httpx.CodeOpenBusiness, "物品库存已变化，请刷新后重试")
			}
			// 目标位置入库（新行或已有 0 行升 1）。
			if err := tx.Exec(
				`INSERT INTO stock_balance (id, item_id, location_id, on_hand)
				 VALUES (?, ?, ?, 1)
				 ON CONFLICT (item_id, location_id)
				 DO UPDATE SET on_hand = 1, updated_at = now()`,
				uuid.NewString(), id, in.ToLocationID).Error; err != nil {
				return err
			}
			// 两条流水：归属库记原库。
			for _, mv := range []struct {
				kind  string
				loc   string
				delta string
			}{{"move_out", srcLoc, "-1"}, {"move_in", in.ToLocationID, "1"}} {
				if err := tx.Exec(
					`INSERT INTO stock_movement (id, item_id, location_id, kind, quantity, on_hand_delta, reason, actor_id, owning_space_id)
					 VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)`,
					uuid.NewString(), id, mv.loc, mv.kind, mv.delta,
					"转库至其他库", actor.UserID, fromSpace).Error; err != nil {
					return err
				}
			}
		}
		// 档案换库，乐观锁失效其他在途编辑（维修占用防并发窗口）。
		res := tx.Exec(
			`UPDATE item SET space_id = ?, version = version + 1, updated_at = now()
			 WHERE id = ? AND version = ? AND deleted_at IS NULL AND maintenance_open = false`,
			in.ToSpaceID, id, in.ExpectedVersion)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return httpx.New(409, httpx.CodeVersionConflict, "物品已被他人修改，请刷新后重试")
		}
		// 照片归属随物品换库。media.AuthorizeDownload 取「物品所在库 ∪ 文件
		// space_id」并集，不迁就会让原库成员继续下载已转走的照片。
		if err := tx.Exec(
			`UPDATE file_object SET space_id = ?
			 WHERE kind = 'photo' AND id IN (SELECT file_id FROM item_photo WHERE item_id = ?)`,
			in.ToSpaceID, id).Error; err != nil {
			return err
		}
		// 在新库语境下回读视图（授权已在上面的目标库写权检查里做过）。
		views, qerr := s.queryOn(ctx, tx, viewSQL+` WHERE v.id = ? LIMIT 1`, id)
		if qerr != nil {
			return qerr
		}
		if len(views) == 0 {
			return httpx.NotFound("物品不存在")
		}
		view = &views[0]
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return view, nil
}
