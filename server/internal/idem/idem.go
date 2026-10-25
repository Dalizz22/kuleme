// Package idem 通用 HTTP 幂等：占位 → 执行 → 落终态。
//
// 与 inventory 的幂等键同构（唯一索引就是并发串行化点），但作用在传输层：
// 路由级中间件读 Idempotency-Key 头，无键直通；有键则以 key 原子占位后
// 才放行业务 handler，重放按原状态码原样返回历史响应体。
package idem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// HeaderKey 是幂等键请求头。
const HeaderKey = "Idempotency-Key"

// ContextKey 把幂等键放进 gin context，业务 handler 需要把同一个 UUID 写进
// 业务表（creation_ref / operation_id）时取用；崩溃恢复靠这个证据对齐。
const ContextKey = "idem.operation_id"

// KeyFrom 返回当前请求的幂等键（无键时为空串）。
func KeyFrom(c *gin.Context) string {
	return c.GetString(ContextKey)
}

// Digest 幂等请求摘要：actor + 方法 + 路由 + 规范化请求体（键排序）。
// 同键不同内容必须 409；绑定方法与路由，避免同一个键跨接口复用命中错误结果。
func Digest(actorID, method, route string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(actorID))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToUpper(method)))
	h.Write([]byte{0})
	h.Write([]byte(route))
	h.Write([]byte{0})
	h.Write(canonicalJSON(body))
	return hex.EncodeToString(h.Sum(nil))
}

// MaxKeyLen 是幂等键长度上限（契约：客户端稳定 operation UUID）。
const MaxKeyLen = 128

// ValidateKey 校验幂等键：必须是 UUID 且不超长。
//
// 键同时被业务层当 operation_id 写进 uuid 列，格式不对会让 SQL 22P02 冒成
// 500；这里提前挡成 400 INVALID_IDEMPOTENCY_KEY。
func ValidateKey(key string) error {
	if len(key) > MaxKeyLen {
		return httpx.New(422, httpx.CodeInvalidIdempotencyKey,
			fmt.Sprintf("Idempotency-Key 过长（最多 %d 字符）", MaxKeyLen))
	}
	if _, err := uuid.Parse(key); err != nil {
		return httpx.New(422, httpx.CodeInvalidIdempotencyKey, "Idempotency-Key 必须是 UUID")
	}
	return nil
}

// canonicalJSON 把 JSON 对象键按字典序重排后再序列化；解析失败退回原文。
func canonicalJSON(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// Execute 以 key 做原子占位后执行 run：抢到占位才执行，未抢到按已存终态
// 重放（fromStore=true）或返回 409。key 为空由调用方短路，不经此函数。
// run 返回的响应已由调用方写出（正常与失败都是）；Execute 只负责占位与
// 记账，失败请求落 failed 状态以允许重试。
func Execute(ctx context.Context, db *store.DB, key, actorID, digest string, run func(context.Context) (int, []byte, error)) (status int, body []byte, fromStore bool, err error) {
	claim := db.Gorm.WithContext(ctx).Exec(`
INSERT INTO api_idempotency (key, actor_id, request_digest, state, created_at, updated_at)
VALUES (?, ?, ?, 'in_progress', now(), now())
ON CONFLICT (key) DO NOTHING`, key, actorID, digest)
	if claim.Error != nil {
		return 0, nil, false, claim.Error
	}
	if claim.RowsAffected == 0 {
		return replayOrTakeover(ctx, db, key, actorID, digest, run)
	}
	return runAndRecord(ctx, db, key, run)
}

// runAndRecord 执行业务并落终态。run 的响应由调用方透传给客户端（正常与
// 失败都是）；失败请求落 failed 状态以允许同键重试。
func runAndRecord(ctx context.Context, db *store.DB, key string, run func(context.Context) (int, []byte, error)) (status int, body []byte, fromStore bool, err error) {
	status, body, runErr := run(ctx)
	recordState, recordStatus, errorCode := "failed", 500, httpx.CodeInternal
	if runErr == nil {
		recordState, recordStatus, errorCode = "succeeded", status, ""
	} else if he := (*httpx.Error)(nil); errors.As(runErr, &he) {
		recordStatus, errorCode = he.Status, he.Code
	}
	// error_code 与 response_body 一起落终态：GET /operation-results 靠它告诉
	// 客户端「失败原因稳定码」（契约 OperationResult.error_code）。
	finalize := db.Gorm.WithContext(ctx).Exec(`
UPDATE api_idempotency
SET state = ?, http_status = ?, response_body = ?, error_code = NULLIF(?, ''), updated_at = now()
WHERE key = ?`, recordState, recordStatus, body, errorCode, key)
	if finalize.Error != nil && runErr == nil {
		// 终态落库失败：响应已产生，按业务结果返回（与 inventory 口径一致）。
		return status, body, false, nil
	}
	return status, body, false, runErr
}

// StaleAfter 是 in_progress 占位的安全接管阈值。
//
// 进程崩溃会留下永远 in_progress 的占位；超过阈值后按「无业务提交证据」
// 处理并条件接管重跑（并发重试只有一路能占到）。阈值取保守的 5 分钟：
// ponytail: 纯时间判定，慢请求 >5 分钟仍可能被接管；因此接管前先查业务证据
// （creation_ref / operation_id 唯一约束兜底不双写），证据命中就不再重跑。
const StaleAfter = 5 * time.Minute

// replayOrTakeover 处理已存在的键：成功 → 重放；不同内容 → 409；进行中
// → 未超时 409、超时则条件接管重跑；failed → 条件更新重新占位后重跑。
func replayOrTakeover(ctx context.Context, db *store.DB, key, actorID, digest string, run func(context.Context) (int, []byte, error)) (int, []byte, bool, error) {
	var prior struct {
		State         string
		RequestDigest string
		ResponseBody  []byte
		HTTPStatus    *int
		UpdatedAt     time.Time
	}
	if err := db.Gorm.WithContext(ctx).Raw(
		`SELECT state, request_digest, response_body, http_status, updated_at
		 FROM api_idempotency WHERE key = ?`, key).Scan(&prior).Error; err != nil {
		return 0, nil, false, err
	}
	if prior.RequestDigest != "" && prior.RequestDigest != digest {
		return 0, nil, false, httpx.New(409, httpx.CodeIdempotencyConflict, "同一幂等键已被不同请求使用")
	}
	if prior.State == "succeeded" {
		st := 200
		if prior.HTTPStatus != nil {
			st = *prior.HTTPStatus
		}
		return st, prior.ResponseBody, true, nil
	}
	if prior.State == "in_progress" {
		// 先查业务提交证据。in_progress 可能停在「业务已提交、终态没写进
		// api_idempotency」的崩溃窗口——此时重跑会双写，必须按已提交事实收口。
		if ev, ok, err := evidenceFor(ctx, db, actorID, key); err != nil {
			return 0, nil, false, err
		} else if ok {
			if err := settleFromEvidence(ctx, db, key, ev); err != nil {
				return 0, nil, false, err
			}
			return 200, envelope(ev), true, nil
		}
		if time.Since(prior.UpdatedAt) < StaleAfter {
			return 0, nil, false, httpx.New(409, httpx.CodeIdempotencyConflict, "相同请求正在处理中，请稍后重试")
		}
		taken := db.Gorm.WithContext(ctx).Exec(`
UPDATE api_idempotency SET updated_at = now()
WHERE key = ? AND state = 'in_progress' AND updated_at < ?`,
			key, time.Now().Add(-StaleAfter))
		if taken.Error != nil {
			return 0, nil, false, taken.Error
		}
		if taken.RowsAffected == 0 {
			return 0, nil, false, httpx.New(409, httpx.CodeIdempotencyConflict, "相同请求正在处理中，请稍后重试")
		}
		return runAndRecord(ctx, db, key, run)
	}
	take := db.Gorm.WithContext(ctx).Exec(`
UPDATE api_idempotency
SET state = 'in_progress', http_status = NULL, response_body = NULL,
    error_code = NULL, updated_at = now()
WHERE key = ? AND state = 'failed'`, key)
	if take.Error != nil {
		return 0, nil, false, take.Error
	}
	if take.RowsAffected == 0 {
		return 0, nil, false, httpx.New(409, httpx.CodeIdempotencyConflict, "相同请求正在处理中，请稍后重试")
	}
	// 直接重跑业务并落终态；不可递归回 Execute——占位行已存在，会误入
	// in_progress 409 分支，把 failed 重试变成死路。
	return runAndRecord(ctx, db, key, run)
}

// Middleware 路由级幂等：挂在 Auth 之后（需要 actor）。无 Idempotency-Key
// 或匿名请求直通；有键先占位，handler 正常写响应，重放时回放历史响应。
func Middleware(db *store.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimSpace(c.GetHeader(HeaderKey))
		actorID := httpx.CurrentActor(c).UserID
		// DB 不可用时直通：后续业务层各自返回 503，幂等层不添加新失败面。
		if key == "" || actorID == "" || db == nil || !db.Available() {
			c.Next()
			return
		}
		if err := ValidateKey(key); err != nil {
			httpx.Abort(c, err)
			return
		}
		c.Set(ContextKey, key)
		var body []byte
		if c.Request.Body != nil {
			b, err := io.ReadAll(c.Request.Body)
			if err != nil {
				httpx.Abort(c, httpx.Validation("无法读取请求体"))
				return
			}
			body = b
			// 回填请求体，业务 handler 还要再读一次。
			c.Request.Body = io.NopCloser(bytes.NewReader(b))
		}
		digest := Digest(actorID, c.Request.Method, routeOf(c), body)
		cw := &captureWriter{ResponseWriter: c.Writer}
		c.Writer = cw
		status, respBody, fromStore, err := Execute(c.Request.Context(), db, key, actorID, digest,
			func(ctx context.Context) (int, []byte, error) {
				c.Request = c.Request.WithContext(ctx)
				c.Next()
				st := cw.Status()
				// handler 主动 Abort 的失败响应（校验 422、业务 409 等）落 failed，
				// 允许修正后重试。错误码必须取响应信封里的真实业务码：前端 failed
				// 分支会把 error_code 回显，和首次响应不一致会自相矛盾。
				if c.IsAborted() && st >= 400 {
					return st, cw.body.Bytes(), abortedError(st, cw.body.Bytes())
				}
				return st, cw.body.Bytes(), nil
			})
		if err != nil {
			// handler 自己写出的失败响应（校验 400、库存 409 等）已经透传给
			// 客户端，此刻再 Abort 会追加第二个 JSON 信封毁掉响应体——只在
			// 尚未向客户端写过头（占位/冲突/重放失败路径）时才补错误响应。
			if !cw.Written() {
				httpx.Abort(c, err)
			}
			return
		}
		if fromStore {
			// 重放：按原状态码原样返回历史响应体（request_id 为原值，即原始响应）。
			c.Data(status, "application/json; charset=utf-8", respBody)
			c.Abort()
		}
	}
}

// abortedError 还原 handler 已写出的错误信封，用于幂等表落 error_code。
// 恢复结果要和用户当时看到的码一致，所以优先取响应体里的 error.code；
// 取不到信封（handler 没写统一外壳）才退回 INTERNAL。
func abortedError(status int, body []byte) *httpx.Error {
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		return &httpx.Error{Status: status, Code: env.Error.Code, Message: env.Error.Message, Details: env.Error.Details}
	}
	return httpx.New(status, httpx.CodeInternal, "请求处理失败")
}

// routeOf 返回幂等摘要用的路由标识：优先 gin 路由模板（/items/:id），
// 未命中路由时退回原始路径，保证摘要仍随请求变化。
func routeOf(c *gin.Context) string {
	if p := c.FullPath(); p != "" {
		return p
	}
	return c.Request.URL.Path
}

// captureWriter 在向客户端透传的同时缓存响应体，供幂等表留存重放。
type captureWriter struct {
	gin.ResponseWriter
	body bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}
