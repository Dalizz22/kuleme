package idem

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// OperationResult 是写操作结果的恢复快照（契约 components.schemas.OperationResult）。
//
// 客户端把同一个 UUID 既当 Idempotency-Key 又当 operation_id：业务已提交但
// 响应丢失时用它取回终态，而不是换新键重提。
type OperationResult struct {
	OperationID string          `json:"operation_id"`
	Status      string          `json:"status"` // in_progress | succeeded | failed
	Result      json.RawMessage `json:"result"` // 成功时的原响应 data 快照；失败为 null
	ErrorCode   *string         `json:"error_code"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Result 按 actor 读幂等记录：别人的或未知的键一律 404（不区分，避免泄露存在性）。
func Result(ctx context.Context, db *store.DB, actorID, operationID string) (OperationResult, error) {
	notFound := httpx.NotFound("操作结果不存在")
	if db == nil || !db.Available() {
		return OperationResult{}, httpx.Unavailable("服务暂时不可用，请稍后重试")
	}
	var row struct {
		State        string
		ResponseBody []byte
		ErrorCode    *string
		CreatedAt    time.Time
		UpdatedAt    time.Time
	}
	if err := db.Gorm.WithContext(ctx).Raw(`
SELECT state, response_body, error_code, created_at, updated_at
FROM api_idempotency WHERE key = ? AND actor_id = ?`, operationID, actorID).Scan(&row).Error; err != nil {
		return OperationResult{}, err
	}
	if row.State == "" {
		// 占位行可能根本没写进去（进程在 claim 后、或终态落库前崩溃），
		// 业务证据仍能证明「已提交」，按已提交结果回答。
		if ev, ok, err := evidenceFor(ctx, db, actorID, operationID); err != nil {
			return OperationResult{}, err
		} else if ok {
			return recovered(operationID, ev), nil
		}
		return OperationResult{}, notFound
	}
	if row.State == "in_progress" {
		if ev, ok, err := evidenceFor(ctx, db, actorID, operationID); err != nil {
			return OperationResult{}, err
		} else if ok {
			return recovered(operationID, ev), nil
		}
	}
	out := OperationResult{
		OperationID: operationID,
		Status:      row.State,
		ErrorCode:   row.ErrorCode,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
	if row.State == "succeeded" && len(row.ResponseBody) > 0 {
		// 原响应体是 {data, request_id}，只回 data 快照。
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(row.ResponseBody, &env) == nil && len(env.Data) > 0 {
			out.Result = env.Data
		}
	}
	return out, nil
}

// recovered 把业务证据包装成终态结果：状态是 succeeded，result 即业务快照。
func recovered(operationID string, ev json.RawMessage) OperationResult {
	now := time.Now().UTC()
	return OperationResult{OperationID: operationID, Status: "succeeded", Result: ev,
		CreatedAt: now, UpdatedAt: now}
}

// envelope 是写恢复结果的统一外壳（与 httpx.Respond 同构）。
func envelope(data json.RawMessage) []byte {
	body, _ := json.Marshal(map[string]any{"data": data, "request_id": ""})
	return body
}

// settleFromEvidence 把「业务已提交、终态未落库」的占位补成 succeeded。
// 只补状态与快照，不触碰任何业务行——已提交事实不可删改。
func settleFromEvidence(ctx context.Context, db *store.DB, key string, ev json.RawMessage) error {
	return db.Gorm.WithContext(ctx).Exec(`
UPDATE api_idempotency
SET state = 'succeeded', http_status = 200, response_body = ?, error_code = NULL, updated_at = now()
WHERE key = ? AND state <> 'succeeded'`, envelope(ev), key).Error
}

// evidenceFor 按 operation_id 查业务提交证据（的四个写入口）。
//
// 证据是「事务已提交」的物证：物品 creation_ref、库存流水 operation_id、
// AI 候选 operation_id、维修记录 operation_id/close_operation_id。全部按
// actor 过滤——别人的操作结果不能靠 id 猜到。生成环境里这些列由各自入口
// isMissingTable 让证据查询在「业务表尚未迁移」的测试库上退化为「无证据」，
// 而不是把 42P01 冒泡成 500。生产库里这些表恒在。
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 42P01")
}

// 写入同一个 Idempotency-Key UUID。
func evidenceFor(ctx context.Context, db *store.DB, actorID, operationID string) (json.RawMessage, bool, error) {
	if _, err := uuid.Parse(operationID); err != nil {
		return nil, false, nil
	}
	var item struct {
		ID         string
		PublicCode string
	}
	if err := db.Gorm.WithContext(ctx).Raw(`
SELECT id, public_code FROM item
WHERE creation_ref::text = ? AND created_by::text = ? AND deleted_at IS NULL LIMIT 1`,
		operationID, actorID).Scan(&item).Error; err != nil && !isMissingTable(err) {
		return nil, false, err
	}
	if item.ID != "" {
		ev, _ := json.Marshal(map[string]any{"item_id": item.ID, "public_code": item.PublicCode})
		return ev, true, nil
	}

	var cand struct {
		ItemID     string
		PublicCode string
		MovementID *string
	}
	if err := db.Gorm.WithContext(ctx).Raw(`
SELECT c.item_id, i.public_code,
       (SELECT m.id FROM stock_movement m
         WHERE m.operation_id::text = c.operation_id::text AND m.kind = 'initial' LIMIT 1) AS movement_id
FROM ai_candidate c LEFT JOIN item i ON i.id = c.item_id
WHERE c.operation_id::text = ? AND c.confirmed_by::text = ? AND c.status = 'confirmed' AND c.item_id IS NOT NULL
LIMIT 1`, operationID, actorID).Scan(&cand).Error; err != nil && !isMissingTable(err) {
		return nil, false, err
	}
	if cand.ItemID != "" {
		ev, _ := json.Marshal(map[string]any{"item_id": cand.ItemID, "public_code": cand.PublicCode,
			"movement_id": cand.MovementID})
		return ev, true, nil
	}

	var mv struct {
		ID       string
		ItemID   string
		Kind     string
		Quantity string
	}
	if err := db.Gorm.WithContext(ctx).Raw(`
SELECT id, item_id, kind, rtrim(to_char(quantity, 'FM999999990.9999'), '.') AS quantity
FROM stock_movement WHERE operation_id::text = ? AND actor_id::text = ? ORDER BY created_at LIMIT 1`,
		operationID, actorID).Scan(&mv).Error; err != nil && !isMissingTable(err) {
		return nil, false, err
	}
	if mv.ID != "" {
		ev, _ := json.Marshal(map[string]any{"item_id": mv.ItemID, "operation_id": operationID,
			"movement_id": mv.ID, "kind": mv.Kind, "quantity": mv.Quantity})
		return ev, true, nil
	}

	var rec struct {
		ID       string
		ItemID   string
		Reason   string
		Note     string
		OpenedAt time.Time
		ClosedAt *time.Time
	}
	if err := db.Gorm.WithContext(ctx).Raw(`
SELECT id, item_id, reason, note, opened_at, closed_at FROM maintenance_record
WHERE (operation_id::text = ? AND opened_by::text = ?)
   OR (close_operation_id::text = ? AND closed_by::text = ?) LIMIT 1`,
		operationID, actorID, operationID, actorID).Scan(&rec).Error; err != nil && !isMissingTable(err) {
		return nil, false, err
	}
	if rec.ID != "" {
		ev, _ := json.Marshal(map[string]any{"id": rec.ID, "item_id": rec.ItemID, "reason": rec.Reason,
			"note": rec.Note, "opened_at": rec.OpenedAt, "closed_at": rec.ClosedAt})
		return ev, true, nil
	}

	// 导入确认：批次本身是业务事实；行明细客户端用 GET /imports/:id 刷新。
	var batch struct {
		ID          string
		Status      string
		Mode        string
		TemplateKey string
		Total       int64
		Ok          int64
		Failed      int64
	}
	if err := db.Gorm.WithContext(ctx).Raw(`
SELECT b.id, b.status, b.mode, b.template_key,
       count(r.line_no) AS total,
       count(r.line_no) FILTER (WHERE r.status = 'ok') AS ok,
       count(r.line_no) FILTER (WHERE r.status = 'failed') AS failed
FROM import_batch b LEFT JOIN import_row r ON r.batch_id = b.id
WHERE b.confirm_operation_id::text = ? AND b.created_by::text = ?
GROUP BY b.id, b.status, b.mode, b.template_key LIMIT 1`,
		operationID, actorID).Scan(&batch).Error; err != nil && !isMissingTable(err) {
		return nil, false, err
	}
	if batch.ID != "" {
		ev, _ := json.Marshal(map[string]any{"batch": map[string]any{
			"id": batch.ID, "status": batch.Status, "mode": batch.Mode,
			"template_key": batch.TemplateKey, "total": batch.Total, "ok": batch.Ok, "failed": batch.Failed,
		}})
		return ev, true, nil
	}
	return nil, false, nil
}

// Register 挂载操作结果恢复路由。
//
//	GET /api/v1/operation-results/:operation_id  本人写请求的处理中/已提交/明确失败结果
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	g := r.Group("", httpx.Auth(deps.Auth, true))
	g.GET("/operation-results/:operation_id", func(c *gin.Context) {
		id := c.Param("operation_id")
		// 非法 UUID 视同未知键：404，不把格式错误当参数校验失败暴露。
		if _, err := uuid.Parse(id); err != nil {
			httpx.Abort(c, httpx.NotFound("操作结果不存在"))
			return
		}
		res, err := Result(c.Request.Context(), deps.DB, httpx.CurrentActor(c).UserID, id)
		if err != nil {
			httpx.Abort(c, err)
			return
		}
		httpx.OK(c, res)
	})
}
