// Package stocktake 盘点：对某空间（或某位置）的余额快照、差异录入与确认调账。
//
// 流程（对应 stocktake/stocktake_line 表，不做草稿多次提交）：
//  1. POST /stocktakes  提交 scope + 每行实点数，服务端取当前余额为 expected，
//     落 draft 快照与差异预览；
//  2. POST /stocktakes/:id/confirm  事务内把每行 counted≠expected 的差异
//     用 correction 库存操作落账，盘点转 confirmed。
package stocktake

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/catalog"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/inventory"
	"github.com/klm/server/internal/store"
)

// LineInput 是创建盘点时的一行实点数。
type LineInput struct {
	ItemID     string  `json:"item_id" binding:"required"`
	LocationID *string `json:"location_id"`
	Counted    string  `json:"counted" binding:"required"`
}

// CreateInput 是 POST /stocktakes 请求体。
// lines 可选：缺省时对整个范围做快照（未找到仅差异）；给了的行更新对应快照行的实点数。
// validateUUIDs 校验 body 里的 uuid 字段：非法值 422，不进 SQL。
func (in *CreateInput) validateUUIDs() error {
	if err := httpx.ValidateUUID("space_id", in.SpaceID, true); err != nil {
		return err
	}
	if in.LocationID != nil {
		if err := httpx.ValidateUUID("location_id", *in.LocationID, false); err != nil {
			return err
		}
	}
	for _, ln := range in.Lines {
		if err := httpx.ValidateUUID("lines.item_id", ln.ItemID, true); err != nil {
			return err
		}
		if ln.LocationID != nil {
			if err := httpx.ValidateUUID("lines.location_id", *ln.LocationID, false); err != nil {
				return err
			}
		}
	}
	return nil
}

type CreateInput struct {
	SpaceID    string      `json:"space_id" binding:"required"`
	LocationID *string     `json:"location_id"`
	Lines      []LineInput `json:"lines"`
}

// Line 是盘点行视图。
type Line struct {
	ItemID     string  `json:"item_id"`
	LocationID *string `json:"location_id"`
	Expected   string  `json:"expected"`
	Counted    string  `json:"counted"`
	Confirmed  bool    `json:"confirmed"`
	Diff       string  `json:"diff"`
}

// Stocktake 是盘点单视图。
type Stocktake struct {
	ID         string  `json:"id"`
	SpaceID    string  `json:"space_id"`
	LocationID *string `json:"location_id"`
	Status     string  `json:"status"`
	DiffCount  int     `json:"diff_count"`
	CreatedBy  string  `json:"created_by"`
	CreatedAt  string  `json:"created_at"`
	Lines      []Line  `json:"lines,omitempty"`
}

type service struct {
	db    *store.DB
	authz *authz.Checker
}

// NewService 构造盘点服务。
func NewService(db *store.DB) *service {
	return &service{db: db, authz: authz.New(db)}
}

// Create 落一张 draft 盘点：expected 取当前余额，counted 用提交值。
func (s *service) Create(ctx context.Context, actor httpx.Actor, in *CreateInput) (*Stocktake, error) {
	if err := in.validateUUIDs(); err != nil {
		return nil, err
	}
	if err := s.checkSpace(ctx, actor, in.SpaceID); err != nil {
		return nil, err
	}
	// 盘点开关关闭时只拒绝新单，历史盘点单仍可查询。
	if err := catalog.CheckFeature(ctx, s.db, in.SpaceID, catalog.FeatureStocktake); err != nil {
		return nil, err
	}
	// 实点数量合法性先过一遍（范围快照模式 lines 可为空）。
	for i := range in.Lines {
		if !validQuantity(in.Lines[i].Counted) {
			return nil, httpx.Validation(fmt.Sprintf("第 %d 行实点数量格式不正确", i+1))
		}
	}
	st := &Stocktake{ID: uuid.NewString(), SpaceID: in.SpaceID, LocationID: in.LocationID, Status: "draft", CreatedBy: actor.UserID}
	err := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
			INSERT INTO stocktake (id, space_id, location_id, status, created_by)
			VALUES (?, ?, ?, 'draft', ?)`, st.ID, st.SpaceID, st.LocationID, actor.UserID).Error; err != nil {
			return fmt.Errorf("创建盘点单: %w", err)
		}
		// 范围快照：库（可选位置）内所有有库存的物品全部入行，
		// expected 取当前余额，counted 置 0 且 counted_confirmed=false（未找到）。
		snap := tx.Exec(`
			INSERT INTO stocktake_line (stocktake_id, item_id, location_id, expected, counted)
			SELECT ?, b.item_id, b.location_id, b.on_hand, 0
			  FROM stock_balance b JOIN item i ON i.id = b.item_id
			 WHERE i.space_id = ? AND i.deleted_at IS NULL AND b.on_hand > 0
			   AND (b.location_id = COALESCE(?, b.location_id))
			   AND (b.location_id IS NOT NULL)`,
			st.ID, st.SpaceID, st.LocationID)
		if snap.Error != nil {
			return fmt.Errorf("写入盘点快照: %w", snap.Error)
		}
		var total int64
		if err := tx.Raw(`SELECT count(*) FROM stocktake_line WHERE stocktake_id = ?`, st.ID).
			Scan(&total).Error; err != nil {
			return err
		}
		if total == 0 {
			return httpx.Validation("盘点范围内没有在库物品")
		}
		if total > 2000 {
			return httpx.Validation(fmt.Sprintf("盘点范围过大（%d 行），请缩小范围（上限 2000）", total))
		}
		// 提交的实点行更新对应快照行（逐件模式下 counted 即 0/1）。
		for i := range in.Lines {
			li := &in.Lines[i]
			res := tx.Exec(`
				UPDATE stocktake_line SET counted = ?::numeric, counted_confirmed = true
				 WHERE stocktake_id = ? AND item_id = ?`,
				li.Counted, st.ID, li.ItemID)
			if res.Error != nil {
				return fmt.Errorf("写入盘点行: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				return httpx.Validation(fmt.Sprintf("第 %d 行物品不在盘点范围内（没有在库余额）", i+1))
			}
		}
		lines, diff, err := loadLines(ctx, tx, st.ID)
		if err != nil {
			return err
		}
		st.Lines, st.DiffCount = lines, diff
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// Get 读一张盘点（含行）；空间不可见按 404 处理。
func (s *service) Get(ctx context.Context, actor httpx.Actor, id string) (*Stocktake, error) {
	var row struct {
		ID        string
		SpaceID   string
		Location  *string
		Status    string
		CreatedBy string
		CreatedAt string
	}
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT id, space_id, location_id AS location, status, created_by,
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at
		  FROM stocktake WHERE id = ?`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, httpx.NotFound("盘点单不存在")
	}
	if err := s.authz.Check(ctx, actor, row.SpaceID, authz.ActionRead); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return nil, httpx.NotFound("盘点单不存在")
		}
		return nil, err
	}
	st := &Stocktake{
		ID: row.ID, SpaceID: row.SpaceID, LocationID: row.Location, Status: row.Status,
		CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt,
	}
	st.Lines, st.DiffCount, err = loadLines(ctx, s.db.Gorm, id)
	return st, err
}

// List 分页列出可见空间的盘点单（不含行）。
func (s *service) List(ctx context.Context, actor httpx.Actor, spaceID string, page, pageSize int) ([]Stocktake, int64, error) {
	page, pageSize = clampPage(page, pageSize)
	visible, err := s.authz.VisibleSpaceIDs(ctx, actor)
	if err != nil {
		return nil, 0, err
	}
	if len(visible) == 0 {
		return []Stocktake{}, 0, nil
	}
	where := " WHERE space_id IN (?)"
	args := []any{visible}
	if spaceID != "" {
		where += " AND space_id = ?"
		args = append(args, spaceID)
	}
	var total int64
	if err := s.db.Gorm.WithContext(ctx).Raw("SELECT count(*) FROM stocktake"+where, args...).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计盘点单: %w", err)
	}
	rows := []Stocktake{}
	q := "SELECT id, space_id, location_id AS location, status, created_by, to_char(created_at,'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') AS created_at FROM stocktake" +
		where + " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	if err := s.db.Gorm.WithContext(ctx).Raw(q, append(args, pageSize, (page-1)*pageSize)...).Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("查询盘点单: %w", err)
	}
	return rows, total, nil
}

// Confirm 确认盘点：差异行逐条 correction 调账，整单转 confirmed。
func (s *service) Confirm(ctx context.Context, actor httpx.Actor, id string) (*Stocktake, error) {
	st, err := s.Get(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	// Get 只要求读权；确认会写状态并调账，必须再过一遍写权（viewer 不能结单）。
	if err := s.checkSpace(ctx, actor, st.SpaceID); err != nil {
		return nil, err
	}
	// 确认是新的调账命令，开关关闭时同样拒绝（历史单只读可查）。
	if err := catalog.CheckFeature(ctx, s.db, st.SpaceID, catalog.FeatureStocktake); err != nil {
		return nil, err
	}
	if st.Status != "draft" {
		return nil, httpx.New(409, httpx.CodeOpenBusiness, "只有草稿状态的盘点单可以确认")
	}
	err = s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 原子抢占：先翻转状态并查 RowsAffected，并发双确认只有一路能进来，
		// 调账在事务内后置——抢占失败的路径一条账都不会写。
		claim := tx.Exec(`
			UPDATE stocktake SET status = 'confirmed', confirmed_by = ?, confirmed_at = now()
			 WHERE id = ? AND status = 'draft'`, actor.UserID, id)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			return httpx.New(409, httpx.CodeOpenBusiness, "盘点单不在草稿状态")
		}
		// 至少实点过一件，防止手滑把整库盘亏。
		countedAny := false
		for i := range st.Lines {
			if st.Lines[i].Confirmed {
				countedAny = true
				break
			}
		}
		if !countedAny {
			return httpx.Validation("盘点单没有任何实点记录，请至少确认一件物品后再结单")
		}
		for i := range st.Lines {
			line := &st.Lines[i]
			if line.Diff == "0" {
				continue
			}
			if _, err := inventory.NewService(s.db).ApplyOpTx(ctx, tx, actor, &inventory.OpInput{
				ItemID:     line.ItemID,
				Kind:       "correction",
				LocationID: line.LocationID,
				Quantity:   line.Diff, // 带符号：正=盘盈入库，负=盘亏出库
				Reason:     "盘点调整 " + id,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, actor, id)
}

// balanceAt 读 (item, location) 当前在手，缺行按 0。
func balanceAt(ctx context.Context, tx *gorm.DB, itemID string, location *string) (string, error) {
	var v string
	err := tx.WithContext(ctx).Raw(`
		SELECT COALESCE(rtrim(to_char(on_hand, 'FM999999999990.9999'), '.'), '0') FROM stock_balance
		 WHERE item_id = ? AND location_id IS NOT DISTINCT FROM ?`, itemID, location).Scan(&v).Error
	if err != nil {
		return "", fmt.Errorf("读取余额: %w", err)
	}
	if v == "" {
		v = "0"
	}
	return v, nil
}

// loadLines 读盘点行并计算差异（counted-expected，带符号定长小数串）。
func loadLines(ctx context.Context, tx *gorm.DB, stocktakeID string) ([]Line, int, error) {
	rows := []Line{}
	err := tx.WithContext(ctx).Raw(`
		SELECT item_id, location_id,
		       rtrim(to_char(expected, 'FM999999999990.9999'), '.') AS expected,
		       rtrim(to_char(counted, 'FM999999999990.9999'), '.') AS counted,
		       counted_confirmed AS confirmed
		  FROM stocktake_line WHERE stocktake_id = ?
		 ORDER BY item_id, location_id NULLS FIRST`, stocktakeID).Scan(&rows).Error
	if err != nil {
		return nil, 0, fmt.Errorf("读取盘点行: %w", err)
	}
	diffCount := 0
	for i := range rows {
		// 未找到（未确认实点）按 0 计——差异=盘亏。
		if rows[i].Confirmed {
			rows[i].Diff = diff(rows[i].Expected, rows[i].Counted)
		} else {
			rows[i].Diff = "-" + strings.TrimPrefix(rows[i].Expected, "+")
		}
		if rows[i].Diff != "0" && rows[i].Diff != "" {
			diffCount++
		}
	}
	return rows, diffCount, nil
}

// diff 计算 counted-expected 的带符号串。内部用 4 位定点整数，避免浮点误差。
func diff(expected, counted string) string {
	e, ok1 := parseScaled(expected)
	c, ok2 := parseScaled(counted)
	if !ok1 || !ok2 {
		return "0"
	}
	return formatScaled(c - e)
}

// scale4 是 4 位小数的定点基数。
const scale4 = int64(10000)

// parseScaled 解析十进制小数为 4 位定点整数；非法返回 false。
func parseScaled(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
	}
	if len(fracPart) > 4 || intPart == "" && fracPart == "" {
		return 0, false
	}
	var v int64
	for _, r := range intPart {
		if r < '0' || r > '9' {
			return 0, false
		}
		v = v*10 + int64(r-'0')
	}
	frac := fracPart
	for i := len(fracPart); i < 4; i++ {
		frac += "0"
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return 0, false
		}
		v = v*10 + int64(r-'0')
	}
	if neg {
		v = -v
	}
	return v, true
}

// formatScaled 把 4 位定点整数格式化回小数串，去掉多余的零。
func formatScaled(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d.%04d", v/scale4, v%scale4)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	if neg {
		return "-" + s
	}
	return s
}

// checkSpace 盘点 = 对空间的 write 操作。
func (s *service) checkSpace(ctx context.Context, actor httpx.Actor, spaceID string) error {
	if err := s.authz.Check(ctx, actor, spaceID, authz.ActionWrite); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return httpx.NotFound("空间不存在或无权访问")
		}
		return err
	}
	return nil
}

// validQuantity 数量格式：非负，整数部分最多 18 位（防 numeric 溢出）、最多 4 位小数；
// 不接受空白与符号。
func validQuantity(q string) bool {
	if q == "" {
		return false
	}
	dot := strings.IndexByte(q, '.')
	frac := ""
	if dot >= 0 {
		frac = q[dot+1:]
		q = q[:dot]
		if len(frac) > 4 {
			return false
		}
	}
	if q == "" {
		q = "0"
	}
	// numeric(18,4)：总精度 18 含 4 位小数，整数部分上限是 14（18=14+4）。
	if len(q) > 14 {
		return false
	}
	for _, r := range q {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// clampPage 分页参数兜底。
func clampPage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
