// Package loan 借还管理：借出扣库存、归还回库存，本模块落业务凭证。
//
// 铁律：余额变化只能经 inventory.ApplyOpTx 完成，
// 借还记录本身是业务凭证，不做幂等表。整单归还，不拆分数量——
// 归还数量即借出数量，拆分待有真实场景再引入（YAGNI）。
package loan

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
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

// Loan 是借出记录的只读视图，json 键与接口契约的下划线风格一致。
type Loan struct {
	ID           string     `json:"id"`
	ItemID       string     `json:"item_id"`
	BorrowerName string     `json:"borrower_name"`
	LocationID   *string    `json:"location_id"`
	Quantity     string     `json:"quantity"`
	Status       string     `json:"status"`
	DueDate      *string    `json:"due_date"`
	Note         *string    `json:"note"`
	CreatedBy    string     `json:"created_by"`
	ReturnedAt   *time.Time `json:"returned_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// CreateInput 创建借出记录的请求。
type CreateInput struct {
	ItemID       string  `json:"item_id" binding:"required"`
	BorrowerName string  `json:"borrower_name" binding:"required"`
	LocationID   *string `json:"location_id"`
	Quantity     string  `json:"quantity"`
	DueDate      *string `json:"due_date"`
	Note         *string `json:"note"`
}

type service struct {
	db    *store.DB
	inv   invApplier
	authz *authz.Checker
}

func newService(db *store.DB) *service {
	return &service{db: db, inv: inventory.NewService(db), authz: authz.New(db)}
}

// loanColumns 是借出视图的公共 SELECT 列（numeric/date 用 to_char 归一）。
// 列一律带 loan. 前缀：List 会 JOIN item 过滤可见库，裸列名会产生 id 歧义。
const loanColumns = `loan.id, loan.item_id, loan.borrower_name, loan.location_id,
       rtrim(to_char(loan.quantity, 'FM999999999990.9999'), '.') AS quantity, loan.status,
       to_char(loan.due_date, 'YYYY-MM-DD') AS due_date, loan.note, loan.created_by,
       loan.returned_at, loan.created_at, loan.updated_at`

// Create 借出：事务内先扣库存再落借出凭证，任一步失败整体回滚。
func (s *service) Create(ctx context.Context, actor httpx.Actor, in *CreateInput) (*Loan, error) {
	name, qty, dueDate, err := validateCreate(in)
	if err != nil {
		return nil, err
	}
	var out *Loan
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		if _, err := s.inv.ApplyOpTx(ctx, tx, actor, &inventory.OpInput{
			ItemID:     in.ItemID,
			Kind:       "loan_out",
			LocationID: in.LocationID,
			Quantity:   qty,
		}); err != nil {
			return err
		}
		id := uuid.NewString()
		if err := tx.Exec(`
INSERT INTO loan (id, item_id, borrower_name, location_id, quantity, due_date, note, created_by)
VALUES (?, ?, ?, ?, ?::numeric, ?::date, ?, ?)`,
			id, in.ItemID, name, emptyID(in.LocationID), qty, dueDate, in.Note, actor.UserID).Error; err != nil {
			return err
		}
		row, err := readLoan(ctx, tx, id)
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

// Return 归还：整单归还（数量即借出数量，不拆分）。归还位置未传时
// 沿用借出位置；两者都为空时由库存层校验拒绝。FOR UPDATE 锁行防并发双归还。
func (s *service) Return(ctx context.Context, actor httpx.Actor, loanID string, locationID *string) (*Loan, error) {
	var out *Loan
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		var row struct {
			ID         string
			ItemID     string
			LocationID *string
			Quantity   string
			Status     string
		}
		err := tx.WithContext(ctx).Raw(`
SELECT id, item_id, location_id,
       rtrim(to_char(quantity, 'FM999999999990.9999'), '.') AS quantity, status
FROM loan WHERE id = ? FOR UPDATE`, loanID).Scan(&row).Error
		if err != nil {
			return err
		}
		if row.ID == "" {
			return httpx.New(404, httpx.CodeNotFound, "借出记录不存在")
		}
		if row.Status == "returned" {
			return httpx.New(409, httpx.CodeOpenBusiness, "该物品已归还")
		}
		loc := locationID
		if loc == nil || *loc == "" {
			loc = row.LocationID
		}
		if _, err := s.inv.ApplyOpTx(ctx, tx, actor, &inventory.OpInput{
			ItemID:     row.ItemID,
			Kind:       "loan_return",
			LocationID: loc,
			Quantity:   row.Quantity,
		}); err != nil {
			return err
		}
		if err := tx.Exec(`
UPDATE loan SET status = 'returned', returned_at = now(), updated_at = now()
 WHERE id = ?`, loanID).Error; err != nil {
			return err
		}
		view, err := readLoan(ctx, tx, loanID)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// List 分页查询借出记录，created_at 倒序；status / itemID 过滤可选。
// 可见性 = 物品归属库对用户可见（JOIN item 取 space_id；不看 deleted_at，
// 借出是历史凭证，物品软删后记录仍归原库）。
func (s *service) List(ctx context.Context, actor httpx.Actor, status, itemID string, page, pageSize int) ([]Loan, int64, error) {
	page, pageSize = clampPagination(page, pageSize)
	if !s.db.Available() {
		return nil, 0, store.ErrUnavailable
	}
	visible, err := s.authz.VisibleSpaceIDs(ctx, actor)
	if err != nil {
		return nil, 0, err
	}
	if len(visible) == 0 {
		return []Loan{}, 0, nil
	}
	from := "loan JOIN item ON item.id = loan.item_id"
	where := "WHERE item.space_id IN (?)"
	args := []any{visible}
	if status != "" {
		where += " AND loan.status = ?"
		args = append(args, status)
	}
	if itemID != "" {
		where += " AND loan.item_id = ?"
		args = append(args, itemID)
	}
	var total int64
	if err := s.db.Gorm.WithContext(ctx).Raw(
		"SELECT count(*) FROM "+from+" "+where, args...).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计借出记录: %w", err)
	}
	items := []Loan{}
	err = s.db.Gorm.WithContext(ctx).Raw(
		`SELECT `+loanColumns+` FROM `+from+` `+where+`
		 ORDER BY loan.created_at DESC
		 LIMIT ? OFFSET ?`,
		append(args, pageSize, (page-1)*pageSize)...).Scan(&items).Error
	if err != nil {
		return nil, 0, fmt.Errorf("查询借出记录: %w", err)
	}
	return items, total, nil
}

// readLoan 按 ID 回读一行借出记录。
func readLoan(ctx context.Context, tx *gorm.DB, id string) (*Loan, error) {
	var row Loan
	err := tx.WithContext(ctx).Raw(
		`SELECT `+loanColumns+` FROM loan WHERE id = ?`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, httpx.NotFound("借出记录不存在")
	}
	return &row, nil
}

// validateCreate 校验借出请求：borrower_name 1-120 字符、数量为正（serial
// 物品恒 1 由库存层兜底强制）、due_date 形如 YYYY-MM-DD。返回归一化后的
// 借用人姓名、数量与归还日期。
func validateCreate(in *CreateInput) (name, qty string, dueDate *string, err error) {
	name = strings.TrimSpace(in.BorrowerName)
	if n := utf8.RuneCountInString(name); n < 1 || n > 120 {
		return "", "", nil, httpx.Validation("借用人姓名需为 1-120 个字符")
	}
	if qty, err = parseQuantity(in.Quantity); err != nil {
		return "", "", nil, err
	}
	if d := emptyID(in.DueDate); d != nil {
		if _, perr := time.Parse("2006-01-02", *d); perr != nil {
			return "", "", nil, httpx.Validation("归还日期格式应为 YYYY-MM-DD")
		}
		dueDate = d
	}
	return name, qty, dueDate, nil
}

// parseQuantity 校验数量：非负十进制、最多 4 位小数且必须为正；空串默认 1。
func parseQuantity(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "1", nil
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

// emptyID 空串视为未提供，避免空串进 uuid/date 列报类型错误。
func emptyID(id *string) *string {
	if id == nil || *id == "" {
		return nil
	}
	return id
}
