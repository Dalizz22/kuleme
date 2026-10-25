// 导出下载中心：导出产物持久保存，仅发起者可见/可下载/可撤权。
//
// ponytail: 同步生成（≤2000 行的量级毫秒级完成），未做异步队列——
// 行数上限或耗时上来后换 jobs worker，接口形状不变（status 字段已留）。
package transfer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/klm/server/internal/audit"
	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/item"
	"github.com/klm/server/internal/store"
)

const maxExportRows = 2000

type exportRow struct {
	ID          string
	RequestedBy string `gorm:"column:requested_by"`
	Status      string
	RowCount    int    `gorm:"column:row_count"`
	ByteSize    int64  `gorm:"column:byte_size"`
	FileName    string `gorm:"column:file_name"`
	Error       string
	Params      string `gorm:"column:params"`
	CreatedAt   time.Time
}

// ExportView 是下载中心条目。
type ExportView struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	RowCount  int    `json:"row_count"`
	ByteSize  int64  `json:"byte_size"`
	FileName  string `json:"file_name"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
}

func (r exportRow) view() ExportView {
	return ExportView{ID: r.ID, Status: r.Status, RowCount: r.RowCount,
		ByteSize: r.ByteSize, FileName: r.FileName, Error: r.Error,
		CreatedAt: r.CreatedAt.Format(time.RFC3339)}
}

type exportService struct {
	db      *store.DB
	dataDir string
	authz   *authz.Checker
}

func newExportService(db *store.DB, dataDir string) *exportService {
	return &exportService{db: db, dataDir: dataDir, authz: authz.New(db)}
}

func (s *exportService) dir() string { return filepath.Join(s.dataDir, "exports") }

// Request 生成导出并落盘（生成时范围快照：结果即此刻权限与过滤下的数据）。
func (s *exportService) Request(ctx context.Context, actor httpx.Actor, templateKey, keyword string) (*ExportView, error) {
	items, err := item.NewService(s.db)
	if err != nil {
		return nil, err
	}
	views, _, err := items.List(ctx, actor, templateKey, keyword, nil, 0, maxExportRows+1)
	if err != nil {
		return nil, err
	}
	if len(views) > maxExportRows {
		return nil, httpx.Validation("导出结果超过 2000 行上限，请缩小过滤范围")
	}
	body, rowCount := renderExportCSV(views)
	id := uuid.NewString()
	name := "items-" + time.Now().Format("20060102-150405") + ".csv"
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(s.dir(), id+".csv"), body, 0o600); err != nil {
		return nil, err
	}
	// Request 时记录生成瞬间的可见库快照，Download 时据其复检撤权。
	visible, err := s.authz.VisibleSpaceIDs(ctx, actor)
	if err != nil {
		return nil, err
	}
	params := map[string]string{
		"template_key": templateKey,
		"keyword":      keyword,
		"space_ids":    mustJSON(visible),
	}
	row := exportRow{ID: id, RequestedBy: actor.UserID, Status: "done",
		RowCount: rowCount, ByteSize: int64(len(body)), FileName: name}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO export_file (id, requested_by, params, status, row_count, byte_size, file_name)
		 VALUES (?, ?, ?::jsonb, 'done', ?, ?, ?)`,
		row.ID, row.RequestedBy, mustJSON(params), row.RowCount, row.ByteSize, row.FileName).Error; err != nil {
		_ = os.Remove(filepath.Join(s.dir(), id+".csv"))
		return nil, err
	}
	audit.New(s.db).Record(ctx, actor, "export", "export_file", &id, nil,
		map[string]any{"rows": rowCount})
	return &ExportView{ID: id, Status: "done", RowCount: rowCount,
		ByteSize: row.ByteSize, FileName: name,
		CreatedAt: time.Now().Format(time.RFC3339)}, nil
}

// List 只返回发起者自己的导出（原表/报告只归发起者）。
func (s *exportService) List(ctx context.Context, actor httpx.Actor) ([]ExportView, error) {
	var rows []exportRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, requested_by, status, row_count, byte_size, file_name, COALESCE(error,'') AS error, created_at
		 FROM export_file WHERE requested_by = ?
		 ORDER BY created_at DESC, id DESC LIMIT 100`, actor.UserID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]ExportView, len(rows))
	for i, r := range rows {
		out[i] = r.view()
	}
	return out, nil
}

// Download 读回产物。非发起者按隐藏规则 404（撤权=记录不可见+文件读不到）。
// 发起者在 Request 后被撤销任一快照库的读权，同样 404 隐藏语义。
func (s *exportService) Download(ctx context.Context, actor httpx.Actor, id string) ([]byte, string, error) {
	row, err := s.load(ctx, actor, id)
	if err != nil {
		return nil, "", err
	}
	var params map[string]string
	_ = json.Unmarshal([]byte(row.Params), &params) // params 列恒为合法 jsonb
	if exportAccessRevoked(params["space_ids"], func(spaceID string) bool {
		return s.authz.Check(ctx, actor, spaceID, authz.ActionRead) == nil
	}) {
		return nil, "", httpx.NotFound("导出不存在")
	}
	body, err := os.ReadFile(filepath.Join(s.dir(), row.ID+".csv"))
	if err != nil {
		return nil, "", httpx.NotFound("导出文件已清理")
	}
	return body, row.FileName, nil
}

// exportAccessRevoked 纯判定：space_idsJSON 是 Request 时的可见库快照
// （JSON 数组），canRead 逐库复检当前读权——任一库失去读权即判撤权。
// 旧记录无快照（空串）放行（迁移兼容）；快照损坏按保守口径拒绝。
func exportAccessRevoked(spaceIDsJSON string, canRead func(spaceID string) bool) bool {
	if strings.TrimSpace(spaceIDsJSON) == "" {
		return false
	}
	var ids []string
	if err := json.Unmarshal([]byte(spaceIDsJSON), &ids); err != nil {
		return true
	}
	for _, sp := range ids {
		if !canRead(sp) {
			return true
		}
	}
	return false
}

// Delete 撤权：删除记录与文件，此后任何人（含发起者）都拿不到。
func (s *exportService) Delete(ctx context.Context, actor httpx.Actor, id string) error {
	row, err := s.load(ctx, actor, id)
	if err != nil {
		return err
	}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`DELETE FROM export_file WHERE id = ? AND requested_by = ?`, row.ID, actor.UserID).Error; err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(s.dir(), row.ID+".csv"))
	return nil
}

func (s *exportService) load(ctx context.Context, actor httpx.Actor, id string) (*exportRow, error) {
	var row exportRow
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, requested_by, status, row_count, byte_size, file_name, COALESCE(error,'') AS error,
		        COALESCE(params::text,'{}') AS params, created_at
		 FROM export_file WHERE id = ?`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" || row.RequestedBy != actor.UserID {
		return nil, httpx.NotFound("导出不存在")
	}
	return &row, nil
}
