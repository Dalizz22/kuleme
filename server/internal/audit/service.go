// Package audit 审计日志：业务模块写入操作留痕，管理员分页查询。
//
// 写入走 Service.Record（约定调用方吞掉错误，不阻断业务）；
// 查询只暴露一个管理员接口，见 router.go。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// 分页边界与 httpx 契约一致：page 从 1 起，page_size 默认 20、最大 100。
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// Entry 是审计日志的一行。object_id/space_id 可为空：不是所有动作都挂具体对象。
type Entry struct {
	ID         string          `json:"id"`
	ActorID    string          `json:"actor_id"`
	Action     string          `json:"action"`
	ObjectType string          `json:"object_type"`
	ObjectID   *string         `json:"object_id"`
	SpaceID    *string         `json:"space_id"`
	Detail     json.RawMessage `json:"detail"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Service 持有数据库依赖。
type Service struct {
	db *store.DB
}

// New 构造审计服务。
func New(db *store.DB) *Service { return &Service{db: db} }

// Try 是 HTTP handler 用的旁路记录：从 gin 上下文取当前操作者写一条审计。
// 约定与 Record 一致——任何失败只记 slog（应用默认 logger），不阻断业务。
func (s *Service) Try(c *gin.Context, action, objectType string, objectID, spaceID *string, detail map[string]any) {
	actor := httpx.CurrentActor(c)
	if err := s.Record(c.Request.Context(), actor, action, objectType, objectID, spaceID, detail); err != nil {
		slog.Warn("审计日志写入失败", "action", action, "actor", actor.UserID, "error", err)
	}
}

// Record 写一条审计日志。
//
// 约定：返回的 error 由调用方吞掉并记日志——审计是旁路，写失败不回滚业务。
func (s *Service) Record(ctx context.Context, actor httpx.Actor, action, objectType string,
	objectID, spaceID *string, detail map[string]any) error {
	if !actor.Authenticated() {
		return httpx.Unauthenticated("需要登录")
	}
	if err := validateEntry(action, objectType); err != nil {
		return err
	}
	data, err := json.Marshal(normalizeDetail(detail))
	if err != nil {
		return httpx.Validation("审计明细必须可序列化为 JSON")
	}
	if !s.db.Available() {
		return store.ErrUnavailable
	}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO audit_log (actor_id, action, object_type, object_id, space_id, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		actor.UserID, strings.TrimSpace(action), strings.TrimSpace(objectType),
		emptyID(objectID), emptyID(spaceID), string(data)).Error; err != nil {
		return fmt.Errorf("写入审计日志: %w", err)
	}
	return nil
}

// List 分页查询审计日志，created_at 倒序；object_type / object_id / actorID 过滤均可选。
func (s *Service) List(ctx context.Context, page, pageSize int, objectType, objectID, actorID string) ([]Entry, int64, error) {
	page, pageSize = clampPagination(page, pageSize)
	if !s.db.Available() {
		return nil, 0, store.ErrUnavailable
	}
	where := "WHERE true"
	args := []any{}
	if objectType != "" {
		where += " AND object_type = ?"
		args = append(args, objectType)
	}
	if objectID != "" {
		where += " AND object_id = ?"
		args = append(args, objectID)
	}
	if actorID != "" {
		where += " AND actor_id = ?"
		args = append(args, actorID)
	}
	var total int64
	if err := s.db.Gorm.WithContext(ctx).Raw(
		"SELECT count(*) FROM audit_log "+where, args...).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计审计日志: %w", err)
	}
	items := []Entry{}
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, actor_id, action, object_type, object_id, space_id, detail, created_at
		 FROM audit_log `+where+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT ? OFFSET ?`,
		append(args, pageSize, (page-1)*pageSize)...).Scan(&items).Error
	if err != nil {
		return nil, 0, fmt.Errorf("查询审计日志: %w", err)
	}
	return items, total, nil
}

// validateEntry 校验必填字段：action 与 object_type 都不能是空白。
func validateEntry(action, objectType string) error {
	if strings.TrimSpace(action) == "" {
		return httpx.Validation("action 不能为空")
	}
	if strings.TrimSpace(objectType) == "" {
		return httpx.Validation("object_type 不能为空")
	}
	return nil
}

// normalizeDetail nil 明细归一为空对象，保证 jsonb 列语义一致。
func normalizeDetail(detail map[string]any) map[string]any {
	if detail == nil {
		return map[string]any{}
	}
	return detail
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
