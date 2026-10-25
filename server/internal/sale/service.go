// Package sale 售卖管理：预占释放与实物出库一步完成，本模块落售卖凭证。
//
// 铁律：余额变化只能经 inventory.ApplyOpTx 完成，
// sale_deliver 在同一事务内释放预占并扣在手，售卖记录本身是业务凭证，
// 不做幂等表。
package sale

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/catalog"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/inventory"
	"github.com/klm/server/internal/store"
)

// 分页边界与 httpx 契约一致：page 从 1 起，page_size 默认 20、最大 100。
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// invApplier 是本模块对库存模块的最小依赖面。inventory.NewService 返回
// 未导出类型，用接口承接以便组合与替换。
type invApplier interface {
	ApplyOpTx(ctx context.Context, tx *gorm.DB, actor httpx.Actor, in *inventory.OpInput) (*inventory.OpResult, error)
}

// Sale 是售卖记录的只读视图，json 键与接口契约的下划线风格一致。
type Sale struct {
	ID         string    `json:"id"`
	ItemID     string    `json:"item_id"`
	LocationID *string   `json:"location_id"`
	Quantity   string    `json:"quantity"`
	Note       *string   `json:"note"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CreateInput 创建售卖记录的请求。
type CreateInput struct {
	ItemID     string  `json:"item_id" binding:"required"`
	LocationID *string `json:"location_id" binding:"required"`
	Quantity   string  `json:"quantity" binding:"required"`
	Note       *string `json:"note"`
}

type service struct {
	db    *store.DB
	inv   invApplier
	authz *authz.Checker
}

func newService(db *store.DB) *service {
	return &service{db: db, inv: inventory.NewService(db), authz: authz.New(db)}
}

// saleColumns 是售卖视图的公共 SELECT 列（numeric 用 to_char 归一）。
// 列一律带 sale. 前缀：List 会 JOIN item 过滤可见库，裸列名会产生 id 歧义。
const saleColumns = `sale.id, sale.item_id, sale.location_id,
       rtrim(to_char(sale.quantity, 'FM999999999990.9999'), '.') AS quantity, sale.note, sale.created_by,
       sale.created_at, sale.updated_at`

// Create 售卖：事务内先经库存层释放预占并扣在手，再落售卖凭证，
// 任一步失败整体回滚。
func (s *service) Create(ctx context.Context, actor httpx.Actor, in *CreateInput) (*Sale, error) {
	qty, err := parseQuantity(in.Quantity)
	if err != nil {
		return nil, err
	}
	if err := s.featureGate(ctx, actor, in.ItemID, catalog.FeatureSale); err != nil {
		return nil, err
	}
	var out *Sale
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		if _, err := s.inv.ApplyOpTx(ctx, tx, actor, &inventory.OpInput{
			ItemID:     in.ItemID,
			Kind:       "sale_deliver",
			LocationID: in.LocationID,
			Quantity:   qty,
		}); err != nil {
			return err
		}
		id := uuid.NewString()
		if err := tx.Exec(`
INSERT INTO sale (id, item_id, location_id, quantity, note, created_by)
VALUES (?, ?, ?, ?::numeric, ?, ?)`,
			id, in.ItemID, emptyID(in.LocationID), qty, in.Note, actor.UserID).Error; err != nil {
			return err
		}
		row, err := readSale(ctx, tx, id)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// featureGate 在命令入口强制库功能开关：关闭 → 403 FEATURE_DISABLED。
// 顺序有意为之：先解析物品所属库并校验写权限（无权限 → 404，不泄露开关状态），
// 再读开关；这样关闭的库不会变成权限探测的预言机。
func (s *service) featureGate(ctx context.Context, actor httpx.Actor, itemID, feature string) error {
	var spaceID string
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT space_id FROM item WHERE id = ? AND deleted_at IS NULL`, itemID).
		Scan(&spaceID).Error; err != nil {
		return err
	}
	if spaceID == "" {
		return httpx.NotFound("物品不存在")
	}
	if err := s.authz.Check(ctx, actor, spaceID, authz.ActionWrite); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return httpx.NotFound("物品不存在")
		}
		return err
	}
	return catalog.CheckFeature(ctx, s.db, spaceID, feature)
}

// List 分页查询售卖记录，created_at 倒序；itemID 过滤可选。
// 可见性 = 物品归属库对用户可见（JOIN item 取 space_id；不看 deleted_at，
// 售卖是历史凭证，物品软删后记录仍归原库）。
func (s *service) List(ctx context.Context, actor httpx.Actor, itemID string, page, pageSize int) ([]Sale, int64, error) {
	page, pageSize = clampPagination(page, pageSize)
	if !s.db.Available() {
		return nil, 0, store.ErrUnavailable
	}
	visible, err := s.authz.VisibleSpaceIDs(ctx, actor)
	if err != nil {
		return nil, 0, err
	}
	if len(visible) == 0 {
		return []Sale{}, 0, nil
	}
	from := "sale JOIN item ON item.id = sale.item_id"
	where := "WHERE item.space_id IN (?)"
	args := []any{visible}
	if itemID != "" {
		where += " AND sale.item_id = ?"
		args = append(args, itemID)
	}
	var total int64
	if err := s.db.Gorm.WithContext(ctx).Raw(
		"SELECT count(*) FROM "+from+" "+where, args...).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计售卖记录: %w", err)
	}
	items := []Sale{}
	err = s.db.Gorm.WithContext(ctx).Raw(
		`SELECT `+saleColumns+` FROM `+from+` `+where+`
		 ORDER BY sale.created_at DESC
		 LIMIT ? OFFSET ?`,
		append(args, pageSize, (page-1)*pageSize)...).Scan(&items).Error
	if err != nil {
		return nil, 0, fmt.Errorf("查询售卖记录: %w", err)
	}
	return items, total, nil
}

// readSale 按 ID 回读一行售卖记录。
func readSale(ctx context.Context, tx *gorm.DB, id string) (*Sale, error) {
	var row Sale
	err := tx.WithContext(ctx).Raw(
		`SELECT `+saleColumns+` FROM sale WHERE id = ?`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, httpx.NotFound("售卖记录不存在")
	}
	return &row, nil
}

// parseQuantity 校验数量：非负十进制、最多 4 位小数且必须为正。
func parseQuantity(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", httpx.Validation("缺少数量")
	}
	s = strings.TrimPrefix(s, "+")
	dot := strings.IndexByte(s, '.')
	intPart, fracPart := s, ""
	if dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
	}
	if (intPart == "" && fracPart == "") || len(fracPart) > 4 {
		return "", httpx.Validation("数量格式不正确，最多 4 位小数")
	}
	allZero := true
	for _, part := range []string{intPart, fracPart} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return "", httpx.Validation("数量格式不正确，最多 4 位小数")
			}
			if r != '0' {
				allZero = false
			}
		}
	}
	if allZero {
		return "", httpx.Validation("数量必须为正数")
	}
	return s, nil
}

// clampPagination page<1 归 1；pageSize<1 用默认、超过上限截断。
func clampPagination(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return page, pageSize
}

// emptyID 空串视为未提供，避免空串进 uuid 列报类型错误。
func emptyID(id *string) *string {
	if id == nil || *id == "" {
		return nil
	}
	return id
}
