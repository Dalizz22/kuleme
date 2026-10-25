// Package transfer 实现 CSV 导入（映射预览、逐行结果、失败重试）与 CSV 导出。
//
// 设计边界（ponytail）：只支持 CSV（UTF-8，Excel 兼容 BOM），XLSX 待真实需求再接 excelize；
// 导出为同步流式响应（有界行数），下载中心/异步导出等 jobs 接入后再做；
// 导入建物品（名称+模板字段+备注）；确认时可选位置建 initial 库存，
// 不选位置 = 仅导入目录。
package transfer

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"

	"errors"
	"fmt"
	"github.com/xuri/excelize/v2"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/inventory"
	"github.com/klm/server/internal/item"
	"github.com/klm/server/internal/store"
	"github.com/klm/server/internal/templates"
)

// maxImportRows 单批导入行数上限，防止一次请求拖垮事务。
const maxImportRows = 1000

// importConfirmStaleAfter 是「确认中途崩溃」残留 confirming 的续跑阈值。
// 与 idem.StaleAfter 同量级：太短会抢跑正常慢确认，太长会让用户干等。
const importConfirmStaleAfter = 5 * time.Minute

// previewRows 预览返回的行数。
const previewRows = 10

// itemCreator 是 transfer 对物品创建能力的最小依赖（item.NewService 返回未导出类型）。
type itemCreator interface {
	CreateTx(ctx context.Context, tx *gorm.DB, actor httpx.Actor, in *item.CreateInput) (*item.View, error)
}

type service struct {
	db    *store.DB
	authz *authz.Checker
	items itemCreator
}

// NewService 构造导入导出服务。
func NewService(db *store.DB) (*service, error) {
	items, err := item.NewService(db)
	if err != nil {
		return nil, err
	}
	return &service{db: db, authz: authz.New(db), items: items}, nil
}

// ---- 视图 ----

// BatchView 导入批次视图。
type BatchView struct {
	ID          string            `json:"id"`
	SpaceID     string            `json:"space_id"`
	TemplateKey string            `json:"template_key"`
	Mapping     map[string]string `json:"mapping"`
	Headers     []string          `json:"headers"`
	Status      string            `json:"status"`
	// Mode 是导入二选一：catalog（仅目录）| stock（同时建初始库存）。
	Mode string `json:"mode"`
	// LocationID 仅 mode=stock 时非空，创建批次时固定。
	LocationID *string   `json:"location_id,omitempty"`
	Total      int64     `json:"total"`
	Ok         int64     `json:"ok"`
	Failed     int64     `json:"failed"`
	CreatedAt  time.Time `json:"created_at"`
	// UpdatedAt 用于识别「确认中途崩溃」残留的 confirming，不对外暴露。
	UpdatedAt time.Time `json:"-"`
}

// RowView 导入行结果（契约）。
// status 取值为批次模式本身（catalog|stock）或 failed；
// 失败行带 error_code（服务端稳定码，如 INVALID_LOCATION）与 message（中文原因）。
type RowView struct {
	RowNo      int    `json:"row_no"`
	Status     string `json:"status"`
	ItemID     string `json:"item_id,omitempty"`
	PublicCode string `json:"public_code,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	Message    string `json:"message,omitempty"`
}

// ---- 导入 ----

// ImportInput 创建导入批次的入参（mode 二选一，stock 必须带位置）。
type ImportInput struct {
	SpaceID     string
	TemplateKey string
	Content     string
	Mode        string
	LocationID  *string
}

// resolveImportMode 校验 mode/位置组合，返回规范化后的模式。
// 位置作整批前置条件：坏位置整批拒绝（422 INVALID_LOCATION），不产生半批库存。
func (s *service) resolveImportMode(ctx context.Context, mode string, locationID *string) (string, error) {
	switch mode {
	case "catalog", "stock":
	default:
		return "", httpx.Validation("mode 必须是 catalog 或 stock")
	}
	if mode == "catalog" {
		return "catalog", nil
	}
	if locationID == nil || strings.TrimSpace(*locationID) == "" {
		return "", httpx.New(422, inventory.CodeInvalidLocation, "直接入库必须选择位置")
	}
	loc := strings.TrimSpace(*locationID)
	if _, err := uuid.Parse(loc); err != nil {
		return "", httpx.New(422, inventory.CodeInvalidLocation, "位置不存在或不可用，请重新选择")
	}
	var n int64
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT count(*) FROM location WHERE id = ? AND deleted_at IS NULL`, loc).Scan(&n).Error; err != nil {
		return "", err
	}
	if n == 0 {
		return "", httpx.New(422, inventory.CodeInvalidLocation, "位置不存在或不可用，请重新选择")
	}
	*locationID = loc
	return "stock", nil
}

// CreateImport 解析 CSV，建 draft 批次并给出列映射猜测与预览。
func (s *service) CreateImport(ctx context.Context, actor httpx.Actor, in ImportInput) (*BatchView, []RowView, error) {
	spaceID, templateKey, content := in.SpaceID, in.TemplateKey, in.Content
	userID := actor.UserID
	if err := httpx.ValidateUUID("space_id", spaceID, true); err != nil {
		return nil, nil, err
	}
	if err := s.authz.Check(ctx, actor, spaceID, authz.ActionWrite); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return nil, nil, httpx.NotFound("库不存在")
		}
		return nil, nil, err
	}
	mode, err := s.resolveImportMode(ctx, in.Mode, in.LocationID)
	if err != nil {
		return nil, nil, err
	}
	cat, err := templates.Builtin()
	if err != nil {
		return nil, nil, err
	}
	manifest, err := cat.ValidateTemplateKey(templateKey)
	if err != nil {
		return nil, nil, httpx.Validation("模板不存在: " + templateKey)
	}

	headers, records, err := parseCSV(content)
	if err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		return nil, nil, httpx.Validation("CSV 没有数据行")
	}
	mapping := guessMapping(headers, manifest)

	batch := &BatchView{SpaceID: spaceID, TemplateKey: templateKey, Mapping: mapping, Headers: headers,
		Status: "draft", Mode: mode, LocationID: in.LocationID}
	err = s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// map 插入不走 serializer：jsonb 列必须显式转文本，id 在 Go 侧生成（无 RETURNING）。
		mappingJSON, _ := json.Marshal(mapping)
		headersJSON, _ := json.Marshal(headers)
		batch.ID = uuid.NewString()
		row := map[string]any{
			"id":       batch.ID,
			"space_id": spaceID, "template_key": templateKey,
			"mapping": gorm.Expr("?::jsonb", string(mappingJSON)),
			"headers": gorm.Expr("?::jsonb", string(headersJSON)),
			"status":  "draft", "created_by": userID,
			"mode": mode, "location_id": in.LocationID,
		}
		if err := tx.Table("import_batch").Create(row).Error; err != nil {
			return err
		}
		batch.CreatedAt = time.Now()
		lines := make([]map[string]any, 0, len(records))
		for i, rec := range records {
			payload := map[string]string{}
			for ci, cell := range rec {
				if ci < len(headers) && strings.TrimSpace(cell) != "" {
					payload[headers[ci]] = cell
				}
			}
			buf, _ := json.Marshal(payload)
			lines = append(lines, map[string]any{"batch_id": batch.ID, "line_no": i + 1,
				"payload": gorm.Expr("?::jsonb", string(buf))})
		}
		return tx.Table("import_row").Create(&lines).Error
	})
	if err != nil {
		return nil, nil, err
	}
	batch.Total = int64(len(records))

	// 预览：前 N 行标注状态（缺名称的行确认时必失败，只提示不阻断）。
	rev := map[string]string{}
	for col, key := range mapping {
		rev[key] = col
	}
	var raws []struct {
		LineNo  int               `gorm:"column:line_no"`
		Payload map[string]string `gorm:"column:payload;serializer:json"`
	}
	if err := s.db.Gorm.WithContext(ctx).Table("import_row").
		Where("batch_id = ?", batch.ID).Order("line_no").Limit(previewRows).Scan(&raws).Error; err != nil {
		return nil, nil, err
	}
	preview := make([]RowView, 0, len(raws))
	for _, raw := range raws {
		rv := RowView{RowNo: raw.LineNo, Status: mode}
		if rev["name"] != "" && strings.TrimSpace(raw.Payload[rev["name"]]) == "" {
			rv.Status = "failed"
			rv.ErrorCode = httpx.CodeValidationFailed
			rv.Message = "名称为空"
		}
		preview = append(preview, rv)
	}
	return batch, preview, nil
}

// GetImport 返回批次与统计。
func (s *service) GetImport(ctx context.Context, actor httpx.Actor, id string) (*BatchView, []RowView, error) {
	batch, err := s.loadBatch(ctx, actor, id)
	if err != nil {
		return nil, nil, err
	}
	rows, _ := s.rows(ctx, id, batch.Mode, maxImportRows)
	return batch, rows, nil
}

// ConfirmInput 确认入参：允许调用方微调映射后逐行建物品。
// LocationID 非空时每行同事务建 initial 库存（直接入库模式）。
type ConfirmInput struct {
	Mapping map[string]string `json:"mapping"`
	// InitialQuantity 覆盖每行初始数量，缺省 1（serial 逐件校验由 inventory 承担）。
	InitialQuantity string `json:"initial_quantity,omitempty"`
	// OperationID 是 HTTP 幂等键：落在 import_batch.confirm_operation_id，
	// 供「确认事务已提交、幂等终态未写」的崩溃恢复反查。
	OperationID string `json:"-"`
}

// ConfirmImport 按映射逐行建物品：每行独立事务（行级原子），失败行记录错误可重试。
// 原子抢占：只有 draft 能进入 confirming，并发第二路/重入在 claim 处 409。
// 失败路径（抢占成功后任一非行级错误）：批次回写 draft 允许重试；
// done 只能从 confirming 翻转（WHERE status='confirming'），杜绝旁路状态。
// 幂等由路由层 idem.Middleware 兜底（Idempotency-Key）。
func (s *service) ConfirmImport(ctx context.Context, actor httpx.Actor, id string, in *ConfirmInput) (batchView *BatchView, rowViews []RowView, err error) {
	batch, err := s.loadBatch(ctx, actor, id)
	if err != nil {
		return nil, nil, err
	}
	// 失败行可单独重试：done 且仍有 failed 行时允许再次确认（只重跑 pending/failed）。
	// confirming 超过阈值视为「确认中途进程崩溃」的残留：允许续跑。已 ok 的行
	// 不回到 pending/failed，所以续跑不会重复建档/重复入底账（崩溃窗口）。
	if batch.Status == "confirming" && time.Since(batch.UpdatedAt) < importConfirmStaleAfter {
		return nil, nil, httpx.Conflict(httpx.CodeOpenBusiness, "批次正在确认中，请稍后再查")
	}
	if batch.Status == "done" && batch.Failed == 0 {
		return nil, nil, httpx.New(409, httpx.CodeOpenBusiness, "批次已确认完成")
	}
	mapping := batch.Mapping
	if len(in.Mapping) > 0 {
		mapping = in.Mapping
	}
	// mapping 是 header→字段ID；payload 按表头存，取值要反查。
	rev := map[string]string{}
	for col, key := range mapping {
		rev[key] = col
	}
	if rev["name"] == "" {
		return nil, nil, httpx.Validation("映射缺少 name 列")
	}
	// 模式与位置在创建批次时固定；确认时只复核位置仍可用。
	// 位置坏 = 整批拒绝（422 INVALID_LOCATION），不半提交到底账。
	if batch.Mode == "stock" {
		modeLoc := batch.LocationID
		if _, err := s.resolveImportMode(ctx, "stock", modeLoc); err != nil {
			return nil, nil, err
		}
		batch.LocationID = modeLoc
	}

	claimed, committed := false, false
	prevStatus := batch.Status
	if prevStatus == "confirming" {
		// 续跑失败时回到 draft（而不是再次卡回 confirming），已 ok 的行不受影响。
		prevStatus = "draft"
	}
	defer func() {
		// 抢占后、翻 done 前的任何错误：回写原状态，避免批次卡死在 confirming。
		// 行级失败不算错误（产品语义：失败行可重试）。进程崩溃仍会残留
		// confirming，defer 管不到——需运维复位或后续超时回收。
		if claimed && !committed && err != nil {
			_ = s.db.Gorm.WithContext(ctx).Table("import_batch").
				Where("id = ? AND status = 'confirming'", id).
				Update("status", prevStatus).Error
		}
	}()

	claim := s.db.Gorm.WithContext(ctx).Model(&struct{}{}).Table("import_batch").
		Where(`id = ? AND (status IN ('draft', 'done')
			OR (status = 'confirming' AND updated_at < ?))`, id, time.Now().Add(-importConfirmStaleAfter)).
		Updates(map[string]any{
			"status": "confirming", "mapping": gorm.Expr("?::jsonb", mustJSON(mapping)),
			"updated_at":           time.Now(),
			"confirm_operation_id": gorm.Expr("NULLIF(?::text, '')::uuid", in.OperationID),
		})
	if claim.Error != nil {
		return nil, nil, claim.Error
	}
	if claim.RowsAffected == 0 {
		return nil, nil, httpx.Conflict(httpx.CodeOpenBusiness, "批次不在待确认状态，可能已确认或正在确认中")
	}
	claimed = true

	var raws []struct {
		LineNo  int               `gorm:"column:line_no"`
		Payload map[string]string `gorm:"column:payload;serializer:json"`
	}
	if err := s.db.Gorm.WithContext(ctx).Table("import_row").
		Where("batch_id = ? AND status IN ('pending','failed')", id).
		Order("line_no").Limit(maxImportRows).Scan(&raws).Error; err != nil {
		return nil, nil, err
	}

	// 直接入库模式（batch.Mode=stock）的公共入参；目录模式不建库存。
	initialLoc := batch.LocationID
	if batch.Mode != "stock" {
		initialLoc = nil
	}
	initialQty := in.InitialQuantity

	for _, raw := range raws {
		// 每行独立事务：物品建档与 import_row 状态原子落账，失败行可重试。
		rowErr := s.db.Gorm.WithContext(ctx).Transaction(func(rowTx *gorm.DB) error {
			name := strings.TrimSpace(raw.Payload[rev["name"]])
			if name == "" {
				return httpx.Validation("名称为空")
			}
			fields := map[string]any{}
			for col, key := range mapping {
				if key == "name" || key == "note" || key == "unit" {
					continue
				}
				if v, ok := raw.Payload[col]; ok && strings.TrimSpace(v) != "" {
					fields[key] = strings.TrimSpace(v)
				}
			}
			in := &item.CreateInput{
				SpaceID: batch.SpaceID, Name: name, TemplateKey: batch.TemplateKey,
				Fields: fields,
			}
			if h := rev["note"]; h != "" {
				if v, ok := raw.Payload[h]; ok {
					in.Note = v
				}
			}
			if h := rev["unit"]; h != "" {
				if v, ok := raw.Payload[h]; ok {
					in.Unit = v
				}
			}
			// 选了位置 = 直接入库，建档与 initial 库存同一行级事务。
			in.InitialLocationID = initialLoc
			in.InitialQuantity = initialQty
			view, err := s.items.CreateTx(ctx, rowTx, actor, in)
			if err != nil {
				return err
			}
			return rowTx.Table("import_row").
				Where("batch_id = ? AND line_no = ?", id, raw.LineNo).
				Updates(map[string]any{"status": "ok", "item_id": view.ID, "error": nil}).Error
		})
		if rowErr != nil {
			msg, code := rowErr.Error(), httpx.CodeInternal
			var httpErr *httpx.Error
			if errors.As(rowErr, &httpErr) {
				msg, code = httpErr.Message, httpErr.Code
			}
			_ = s.db.Gorm.WithContext(ctx).Table("import_row").
				Where("batch_id = ? AND line_no = ?", id, raw.LineNo).
				Updates(map[string]any{"status": "failed", "error": msg, "error_code": code}).Error
		}
	}

	done := s.db.Gorm.WithContext(ctx).Table("import_batch").
		Where("id = ? AND status = 'confirming'", id).
		Update("status", "done")
	if done.Error != nil {
		return nil, nil, done.Error
	}
	if done.RowsAffected == 0 {
		// 理论上不可达（claim 已持 confirming），防御性兜底。
		return nil, nil, httpx.Conflict(httpx.CodeOpenBusiness, "批次状态已变化，请刷新后重试")
	}
	committed = true

	batchView, rowViews, gerr := s.GetImport(ctx, actor, id)
	if gerr != nil {
		return nil, nil, gerr
	}
	return batchView, rowViews, nil
}

// ---- 导出 ----

// renderExportCSV 把视图渲染成带 BOM 的 CSV；返回字节与行数。
// 单元格做公式注入消毒（= + - @ 开头加 ' 前缀，Excel 按文本处理）。
func renderExportCSV(views []item.View) ([]byte, int) {
	fieldIDs := []string{}
	seen := map[string]bool{}
	for _, v := range views {
		ids := make([]string, 0, len(v.Fields))
		for k := range v.Fields {
			if !seen[k] {
				ids = append(ids, k)
			}
		}
		sort.Strings(ids)
		for _, k := range ids {
			seen[k] = true
			fieldIDs = append(fieldIDs, k)
		}
	}
	buf := &bytes.Buffer{}
	buf.WriteString("\uFEFF") // UTF-8 BOM：Excel 直接打开不乱码
	w := csv.NewWriter(buf)
	header := append([]string{"public_code", "name", "template_key", "quantity", "location_path", "status", "note"}, fieldIDs...)
	_ = w.Write(header)
	for _, v := range views {
		// 核心列同样过 csvSafe（数量/编码等以 - 开头会被 Excel 当公式）；
		// note 之前漏写导致数据行比表头少一列，一并修复。
		row := []string{
			csvSafe(v.PublicCode), csvSafe(v.Name), csvSafe(v.TemplateKey),
			csvSafe(v.Quantity), csvSafe(v.LocationPath), csvSafe(v.Status), csvSafe(v.Note),
		}
		for _, k := range fieldIDs {
			row = append(row, csvSafe(fmt.Sprintf("%v", v.Fields[k])))
		}
		_ = w.Write(row)
	}
	w.Flush()
	return buf.Bytes(), len(views)
}

// csvSafe 防公式注入：危险前缀加单引号。
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t':
		return "'" + s
	}
	return s
}

// ---- 内部 ----

// parseCSV 解析 UTF-8 CSV：首行表头，最多 maxImportRows 数据行。
func parseCSV(content string) ([]string, [][]string, error) {
	r := csv.NewReader(strings.NewReader(content))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, nil, httpx.Validation("CSV 解析失败: " + err.Error())
	}
	if len(records) == 0 {
		return nil, nil, httpx.Validation("CSV 为空")
	}
	headers := make([]string, len(records[0]))
	for i, h := range records[0] {
		headers[i] = strings.TrimSpace(h)
	}
	data := records[1:]
	if len(data) > maxImportRows {
		return nil, nil, httpx.Validation(fmt.Sprintf("最多支持 %d 行，当前 %d 行", maxImportRows, len(data)))
	}
	return headers, data, nil
}

// guessMapping 按表头猜映射：核心列名直配，其余匹配模板字段 id 或 label。
func guessMapping(headers []string, manifest templates.Manifest) map[string]string {
	m := map[string]string{}
	core := map[string]string{"名称": "name", "name": "name", "备注": "note", "note": "note", "单位": "unit", "unit": "unit"}
	for _, h := range headers {
		key := strings.ToLower(strings.TrimSpace(h))
		if core[key] != "" {
			m[h] = core[key]
			continue
		}
		for _, f := range manifest.Fields {
			if key == f.FieldID || key == strings.ToLower(f.Label) {
				m[h] = f.FieldID
				break
			}
		}
	}
	// 找不到 name 列时取第一列兜底。
	if m[nameHeader(headers)] != "name" {
		for _, h := range headers {
			if _, used := m[h]; !used {
				m[h] = "name"
				break
			}
		}
	}
	return m
}

func nameHeader(headers []string) string {
	for _, h := range headers {
		if strings.EqualFold(strings.TrimSpace(h), "name") || strings.TrimSpace(h) == "名称" {
			return h
		}
	}
	if len(headers) > 0 {
		return headers[0]
	}
	return ""
}

func (s *service) loadBatch(ctx context.Context, actor httpx.Actor, id string) (*BatchView, error) {
	var raw struct {
		ID          string
		SpaceID     string
		TemplateKey string
		Mapping     map[string]string `gorm:"serializer:json"`
		Headers     []string          `gorm:"serializer:json"`
		Status      string
		Mode        string
		LocationID  *string
		CreatedAt   time.Time
		UpdatedAt   time.Time
	}
	err := s.db.Gorm.WithContext(ctx).Table("import_batch").Where("id = ?", id).Scan(&raw).Error
	if err != nil {
		return nil, err
	}
	if raw.ID == "" {
		return nil, httpx.NotFound("导入批次不存在")
	}
	if err := s.authz.Check(ctx, actor, raw.SpaceID, authz.ActionWrite); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return nil, httpx.NotFound("导入批次不存在")
		}
		return nil, err
	}
	stats := struct{ Total, Ok, Failed int64 }{}
	_ = s.db.Gorm.WithContext(ctx).Table("import_row").Select(
		"COUNT(*) AS total, COUNT(*) FILTER (WHERE status='ok') AS ok, COUNT(*) FILTER (WHERE status='failed') AS failed",
	).Where("batch_id = ?", id).Scan(&stats).Error
	mode := raw.Mode
	if mode == "" {
		mode = "catalog"
	}
	return &BatchView{
		ID: raw.ID, SpaceID: raw.SpaceID, TemplateKey: raw.TemplateKey, Mapping: raw.Mapping,
		Headers: raw.Headers, Status: raw.Status, Mode: mode, LocationID: raw.LocationID,
		Total: stats.Total, Ok: stats.Ok, Failed: stats.Failed,
		CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt,
	}, nil
}

func (s *service) rows(ctx context.Context, batchID, mode string, limit int) ([]RowView, error) {
	if mode == "" {
		mode = "catalog"
	}
	var raws []struct {
		LineNo     int               `gorm:"column:line_no"`
		Payload    map[string]string `gorm:"column:payload;serializer:json"`
		Status     string
		ItemID     *string
		Error      *string
		ErrorCode  *string
		PublicCode *string
	}
	err := s.db.Gorm.WithContext(ctx).Table("import_row").
		Select(`import_row.*, (SELECT i.public_code FROM item i WHERE i.id = import_row.item_id) AS public_code`).
		Where("batch_id = ?", batchID).Order("line_no").Limit(limit).Scan(&raws).Error
	if err != nil {
		return nil, err
	}
	rows := make([]RowView, 0, len(raws))
	for _, r := range raws {
		rv := RowView{RowNo: r.LineNo, Status: r.Status}
		switch r.Status {
		case "ok":
			rv.Status = mode
		case "failed":
			rv.Status = "failed"
		}
		if r.ItemID != nil {
			rv.ItemID = *r.ItemID
		}
		if r.PublicCode != nil {
			rv.PublicCode = *r.PublicCode
		}
		if r.Error != nil {
			rv.Message = *r.Error
		}
		if r.ErrorCode != nil {
			rv.ErrorCode = *r.ErrorCode
		}
		rows = append(rows, rv)
	}
	return rows, err
}

// mustJSON 序列化 jsonb 参数用；映射都是纯 map，失败只可能来自编程错误。
func mustJSON(v any) string {
	buf, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(buf)
}

// xlsxToCSV 用 excelize 读第一张工作表并转成 CSV 文本，
// 复用 CSV 的映射/确认链路；无宏执行、不解析公式结果（读缓存值）。
func xlsxToCSV(data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", httpx.Validation("表格文件无法解析，请确认是有效的 xlsx")
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return "", httpx.Validation("表格没有工作表")
	}
	rows, err := f.GetRows(sheets[0])
	if err != nil {
		return "", httpx.Validation("读取工作表失败")
	}
	if len(rows) > maxImportRows+1 {
		return "", httpx.Validation(fmt.Sprintf("表格最多 %d 行数据", maxImportRows))
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			return "", httpx.Internal(err)
		}
	}
	w.Flush()
	return buf.String(), w.Error()
}
