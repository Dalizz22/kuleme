// Package jobs 异步任务队列：目前只有入队与查询。
//
// 任务就是 job 表里的一行：入队即插入 queued 记录，谁消费、怎么消费
// 留待后续执行器，这里不预建任何执行框架。
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Job 状态机字符串，与迁移 0005 的 CHECK 约束一致。
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// 分页边界与 httpx 契约一致：page 从 1 起，page_size 默认 20、最大 100。
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// jobStatuses 是合法状态集合，List 的 status 过滤参数据此校验。
var jobStatuses = map[string]bool{
	StatusQueued: true, StatusRunning: true, StatusSucceeded: true, StatusFailed: true,
}

// Job 是队列表的一行。Payload 用 json.RawMessage 原样存取，
// 各任务类型的结构由生产方与消费方自行约定。
type Job struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	Status     string          `json:"status"`
	Priority   int             `json:"priority"`
	RunAt      time.Time       `json:"run_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Error      *string         `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// Service 持有数据库依赖。
type Service struct {
	db *store.DB
}

// New 构造任务服务。
func New(db *store.DB) *Service { return &Service{db: db} }

// Enqueue 入队一个任务。payload 为空时按 {} 落库；runAt 为 nil 表示立即就绪。
// actor 只做登录校验：不记录操作者列，不预建用不上的字段。
func (s *Service) Enqueue(ctx context.Context, actor httpx.Actor, kind string, payload json.RawMessage, runAt *time.Time) (Job, error) {
	if !actor.Authenticated() {
		return Job{}, httpx.Unauthenticated("需要登录")
	}
	if err := validateKind(kind); err != nil {
		return Job{}, err
	}
	payload = normalizePayload(payload)
	if !json.Valid(payload) {
		return Job{}, httpx.Validation("任务负载必须是合法 JSON")
	}
	if !s.db.Available() {
		return Job{}, store.ErrUnavailable
	}
	run := time.Now()
	if runAt != nil {
		run = *runAt
	}
	var row struct {
		ID        string
		Kind      string
		Payload   json.RawMessage
		Status    string
		RunAt     time.Time
		CreatedAt time.Time
		UpdatedAt time.Time
	}
	err := s.db.Gorm.WithContext(ctx).Raw(
		`INSERT INTO job (kind, payload, run_at)
		 VALUES (?, ?, ?)
		 RETURNING id, kind, payload, status, run_at, created_at, updated_at`,
		strings.TrimSpace(kind), string(payload), run).Scan(&row).Error
	if err != nil {
		return Job{}, fmt.Errorf("写入任务: %w", err)
	}
	return Job{
		ID:        row.ID,
		Kind:      row.Kind,
		Payload:   row.Payload,
		Status:    row.Status,
		RunAt:     row.RunAt,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

// List 分页查询任务。status 为空返回全部，按 created_at 倒序。
func (s *Service) List(ctx context.Context, status string, page, pageSize int) ([]Job, int64, error) {
	if err := validateStatus(status); err != nil {
		return nil, 0, err
	}
	page, pageSize = clampPagination(page, pageSize)
	if !s.db.Available() {
		return nil, 0, store.ErrUnavailable
	}
	g := s.db.Gorm.WithContext(ctx)
	scoped := func() *gorm.DB {
		q := g.Table("job")
		if status != "" {
			q = q.Where("status = ?", status)
		}
		return q
	}
	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计任务: %w", err)
	}
	rows := []Job{}
	if err := scoped().Order("created_at DESC, id DESC").
		Limit(pageSize).Offset((page - 1) * pageSize).
		Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("查询任务: %w", err)
	}
	return rows, total, nil
}

// validateKind 校验任务类型：非空且不超过列宽 64，避免撞库报 500。
func validateKind(kind string) error {
	if strings.TrimSpace(kind) == "" {
		return httpx.Validation("任务类型不能为空")
	}
	if len(kind) > 64 {
		return httpx.Validation("任务类型过长")
	}
	return nil
}

// normalizePayload 空负载归一为 {}。
func normalizePayload(payload json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(payload)) == 0 {
		return json.RawMessage("{}")
	}
	return payload
}

// validateStatus 校验列表过滤状态：空表示全部。
func validateStatus(status string) error {
	if status == "" || jobStatuses[status] {
		return nil
	}
	return httpx.Validation("任务状态不正确")
}

// clampPagination 钳制分页参数：page<1 归 1，pageSize<1 取默认，上限 100。
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
