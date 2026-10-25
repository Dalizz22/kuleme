// 待售/出售/纠错（只分待售与已售，无成交交付阶段）。
//
// 待售 = item.for_sale 标记（档案层面，不动库存）；
// 出售 = 一个事务里：出库（余额 1→0）+ 流水（归属原库）+ 档案 lifecycle=sold
//
//   - 售卖留痕（sale 表）；复用 0008 表，loan/sale 路由已下线。
//
// 纠错 = lifecycle 回 normal + 同位置入库 + 流水 correction + 凭证打撤销标记。
package item

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/catalog"
	"github.com/klm/server/internal/httpx"
)

// SaleInput 出售请求。价格/备注可选（价格开关关闭的库可不填）。
type SaleInput struct {
	Price string `json:"price"`
	Note  string `json:"note"`
}

type saleItemRow struct {
	ID              string
	SpaceID         string
	Lifecycle       string
	Version         int
	MaintenanceOpen bool `gorm:"column:maintenance_open"`
}

// sellGate 出售/纠错共用的前置校验。
func (s *itemService) sellGate(ctx context.Context, actor httpx.Actor, id string) (*saleItemRow, error) {
	var row saleItemRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, space_id, lifecycle, version, maintenance_open FROM item
		 WHERE id = ? AND deleted_at IS NULL`, id).Scan(&row).Error
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

// primaryLocation 在手余额所在位置（逐件至多一行 on_hand=1）。
func (s *itemService) primaryLocation(ctx context.Context, tx *gorm.DB, itemID string) (string, error) {
	var loc string
	err := tx.WithContext(ctx).Raw(
		`SELECT location_id FROM stock_balance
		 WHERE item_id = ? AND on_hand > 0 AND location_id IS NOT NULL
		 ORDER BY updated_at DESC LIMIT 1`, itemID).Scan(&loc).Error
	return loc, err
}

// Sale 出售：物品必须在库（on_hand=1）且未进入终态。
func (s *itemService) Sale(ctx context.Context, actor httpx.Actor, id string, in *SaleInput) (*View, error) {
	row, err := s.sellGate(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	// 库关闭「出售」后拒绝新的出售命令（历史售卖记录只读可查）。
	if err := s.featureGate(ctx, row.SpaceID, catalog.FeatureSale); err != nil {
		return nil, err
	}
	// 维修中/非正常状态不允许出售。
	if err := occupiedGate(row.Lifecycle, row.MaintenanceOpen); err != nil {
		return nil, err
	}
	in.Price = strings.TrimSpace(in.Price)
	if in.Price != "" && !validMoney(in.Price) {
		return nil, httpx.Validation("价格格式不正确")
	}
	var view *View
	txErr := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		loc, err := s.primaryLocation(ctx, tx, id)
		if err != nil {
			return err
		}
		if loc == "" {
			return httpx.New(409, httpx.CodeOpenBusiness, "物品不在库，无法出售")
		}
		// 出库。
		res := tx.Exec(
			`UPDATE stock_balance SET on_hand = on_hand - 1, updated_at = now()
			 WHERE item_id = ? AND location_id = ? AND on_hand > 0`, id, loc)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return httpx.New(409, httpx.CodeOpenBusiness, "物品库存已变化，请刷新后重试")
		}
		// 流水（归属记原库）+ 售卖凭证 + 档案终态，同一事务。
		if err := tx.Exec(
			`INSERT INTO stock_movement (id, item_id, location_id, kind, quantity, on_hand_delta, reason, actor_id, owning_space_id)
			 VALUES (?, ?, ?, 'sale_deliver', 1, '-1', ?, ?, ?)`,
			uuid.NewString(), id, loc, saleReason(in), actor.UserID, row.SpaceID).Error; err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO sale (id, item_id, location_id, quantity, price, note, created_by)
			 VALUES (?, ?, ?, 1, NULLIF(?, '')::numeric, ?, ?)`,
			uuid.NewString(), id, loc, strings.TrimSpace(in.Price), noteOf(in), actor.UserID).Error; err != nil {
			return err
		}
		upd := tx.Exec(
			`UPDATE item SET lifecycle = 'sold', for_sale = false, version = version + 1, updated_at = now()
			 WHERE id = ? AND lifecycle = 'normal' AND maintenance_open = false`, id)
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			// 闸门后物品被并发抢占（送修/改状态）：整体回滚，不留「库存少 1 但仍是 normal」的脏账。
			return httpx.New(409, httpx.CodeOpenBusiness, "物品状态已变化（可能已送修或售出），出售未生效")
		}
		return s.reloadView(ctx, tx, id, &view)
	})
	if txErr != nil {
		return nil, txErr
	}
	return view, nil
}

// ReverseSale 纠错：撤销最近一次未撤销的出售，物品回原位置入库。
func (s *itemService) ReverseSale(ctx context.Context, actor httpx.Actor, id string) (*View, error) {
	row, err := s.sellGate(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if row.Lifecycle != "sold" {
		return nil, httpx.New(409, httpx.CodeOpenBusiness, "只有已售物品可以纠错")
	}
	var view *View
	txErr := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 最近一次有效凭证的位置。
		var loc string
		if err := tx.WithContext(ctx).Raw(
			`SELECT COALESCE(location_id::text,'') FROM sale
			 WHERE item_id = ? AND reversed_at IS NULL
			 ORDER BY created_at DESC LIMIT 1`, id).Scan(&loc).Error; err != nil {
			return err
		}
		if loc == "" {
			return httpx.New(409, httpx.CodeOpenBusiness, "没有可撤销的出售记录")
		}
		// 档案回正常态（防并发二次纠错）。
		res := tx.Exec(
			`UPDATE item SET lifecycle = 'normal', version = version + 1, updated_at = now()
			 WHERE id = ? AND lifecycle = 'sold'`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return httpx.New(409, httpx.CodeOpenBusiness, "物品状态已变化，请刷新后重试")
		}
		// 原位置入库（余额行保留着，0→1）。
		if err := tx.Exec(
			`INSERT INTO stock_balance (id, item_id, location_id, on_hand)
			 VALUES (?, ?, ?, 1)
			 ON CONFLICT (item_id, location_id)
			 DO UPDATE SET on_hand = 1, updated_at = now()`,
			uuid.NewString(), id, loc).Error; err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO stock_movement (id, item_id, location_id, kind, quantity, on_hand_delta, reason, actor_id, owning_space_id)
			 VALUES (?, ?, ?, 'correction', 1, '1', '出售纠错，撤销售卖', ?, ?)`,
			uuid.NewString(), id, loc, actor.UserID, row.SpaceID).Error; err != nil {
			return err
		}
		if err := tx.Exec(
			`UPDATE sale SET reversed_at = now(), updated_at = now()
			 WHERE id = (SELECT id FROM sale WHERE item_id = ? AND reversed_at IS NULL
			             ORDER BY created_at DESC LIMIT 1)`, id).Error; err != nil {
			return err
		}
		return s.reloadView(ctx, tx, id, &view)
	})
	if txErr != nil {
		return nil, txErr
	}
	return view, nil
}

// SetForSale 待售标记。终态物品不允许再挂待售。
func (s *itemService) SetForSale(ctx context.Context, actor httpx.Actor, id string, forSale bool) (*View, error) {
	row, err := s.sellGate(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	// 标记待售属出售流程，跟着「出售」开关走。
	if err := s.featureGate(ctx, row.SpaceID, catalog.FeatureSale); err != nil {
		return nil, err
	}
	// 维修中/非正常状态不允许挂待售。
	if err := occupiedGate(row.Lifecycle, row.MaintenanceOpen); err != nil {
		return nil, err
	}
	res := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE item SET for_sale = ?, version = version + 1, updated_at = now()
		 WHERE id = ? AND lifecycle = 'normal' AND maintenance_open = false AND deleted_at IS NULL`, forSale, id)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, httpx.New(409, httpx.CodeVersionConflict, "物品状态已变化，请刷新后重试")
	}
	var view *View
	if err := s.reloadView(ctx, s.db.Gorm, id, &view); err != nil {
		return nil, err
	}
	return view, nil
}

// reloadView 事务内回读视图。
func (s *itemService) reloadView(ctx context.Context, tx *gorm.DB, id string, out **View) error {
	views, err := s.queryOn(ctx, tx, viewSQL+` WHERE v.id = ? LIMIT 1`, id)
	if err != nil {
		return err
	}
	if len(views) == 0 {
		return httpx.NotFound("物品不存在")
	}
	*out = &views[0]
	return nil
}

func saleReason(in *SaleInput) string {
	if in.Note != "" {
		return "出售：" + in.Note
	}
	return "出售"
}

func noteOf(in *SaleInput) string {
	if in.Price == "" {
		return in.Note
	}
	if in.Note == "" {
		return "价格 " + in.Price
	}
	return in.Note + "（价格 " + in.Price + "）"
}

// validMoney 正定点金额，最多两位小数。
func validMoney(s string) bool {
	if s == "" {
		return false
	}
	i, frac, found := strings.Cut(s, ".")
	if i == "" && !found {
		return false
	}
	if i != "" {
		for j := 0; j < len(i); j++ {
			if i[j] < '0' || i[j] > '9' {
				return false
			}
		}
	}
	if found {
		if len(frac) > 2 {
			return false
		}
		for j := 0; j < len(frac); j++ {
			if frac[j] < '0' || frac[j] > '9' {
				return false
			}
		}
	}
	return i != "" || found
}
