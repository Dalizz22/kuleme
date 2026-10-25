package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"gorm.io/gorm"

	"github.com/klm/server/internal/store"
)

// PgStore 用 Postgres 实现 TaskCoordinator 与 CandidateStore。
//
// 当前同步执行：CreateTask 在请求内 startTask → 识别 → Finish，不起 worker。
// Leased/Heartbeat 按任务基础协议保留，worker 化后零改动启用。
//
// 状态词表：任务用 queued/running/succeeded/failed（types.go）；
// 候选落库用 pending/confirmed/discarded（接口契约约定 第 6 节）。
// types.go 的 CandidateAccepted/Ignored 常量与契约词表不一致，
// 这里用字面量对齐契约，漂移已上报（types.go 不在本次边界内）。
type PgStore struct {
	db       *store.DB
	mu       sync.Mutex
	handlers map[string]TaskHandler
}

const (
	// executionVersionFixed 是同步执行期的固定执行版本：一次请求内
	// 领取与完成一一对应，不存在过期 worker。worker 化时换成真版本号。
	executionVersionFixed = int64(1)

	candPending   = "pending"
	candConfirmed = "confirmed"
	candDiscarded = "discarded"
)

func NewPgStore(db *store.DB) *PgStore {
	return &PgStore{db: db, handlers: map[string]TaskHandler{}}
}

// Register 静态注册处理器。同步执行期处理器由 service 直接调用，
// 这里只保存映射，worker 化后由 Leased 分发。
func (s *PgStore) Register(kind string, handler TaskHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[kind] = handler
}

// Enqueue 创建 queued 任务，返回任务标识。
func (s *PgStore) Enqueue(ctx context.Context, t Task) (string, error) {
	if s.db == nil || !s.db.Available() {
		return "", store.ErrUnavailable
	}
	payload, err := json.Marshal(t.Input)
	if err != nil {
		return "", err
	}
	var row struct {
		ID string `gorm:"column:id"`
	}
	err = s.db.Gorm.WithContext(ctx).Raw(`
		INSERT INTO ai_task (space_id, status, payload, created_by)
		VALUES (?::uuid, 'queued', ?::jsonb, ?::uuid)
		RETURNING id
	`, t.SpaceID, payload, t.ActorID).Scan(&row).Error
	if err != nil {
		return "", err
	}
	return row.ID, nil
}

// startTask 把一个 queued 任务置为 running（同步执行入口；worker 化后由 Leased 取代）。
func (s *PgStore) startTask(ctx context.Context, taskID string) error {
	if !s.db.Available() {
		return store.ErrUnavailable
	}
	exec := s.db.Gorm.WithContext(ctx).Exec(`
		UPDATE ai_task SET status = 'running', updated_at = now()
		WHERE id = ?::uuid AND status = 'queued'
	`, taskID)
	if exec.Error != nil {
		return exec.Error
	}
	if exec.RowsAffected == 0 {
		return fmt.Errorf("%w: 任务不在排队中", ErrConflict)
	}
	return nil
}

// Leased 领取最老的 queued 任务并置为 running（FOR UPDATE SKIP LOCKED）。
// 当前同步执行不经过这里；kind 参数在单模块队列下只做语义标注。
func (s *PgStore) Leased(ctx context.Context, kind string) (Task, bool, error) {
	if s.db == nil || !s.db.Available() {
		return Task{}, false, store.ErrUnavailable
	}
	var row taskRow
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT id, space_id, status, payload, result, error, created_by, created_at, updated_at
		FROM ai_task WHERE status = 'queued'
		ORDER BY created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&row).Error
	if err != nil {
		return Task{}, false, err
	}
	if row.ID == "" {
		return Task{}, false, nil
	}
	exec := s.db.Gorm.WithContext(ctx).Exec(`
		UPDATE ai_task SET status = 'running', updated_at = now()
		WHERE id = ?::uuid AND status = 'queued'
	`, row.ID)
	if exec.Error != nil {
		return Task{}, false, exec.Error
	}
	if exec.RowsAffected == 0 {
		return Task{}, false, nil
	}
	row.Status = "running"
	t, err := row.toTask(kind)
	if err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

// Heartbeat 续租：任务仍在 running 才算持有。
func (s *PgStore) Heartbeat(ctx context.Context, taskID string, version int64) (bool, error) {
	if s.db == nil || !s.db.Available() {
		return false, store.ErrUnavailable
	}
	exec := s.db.Gorm.WithContext(ctx).Exec(`
		UPDATE ai_task SET updated_at = now()
		WHERE id = ?::uuid AND status = 'running'
	`, taskID)
	return exec.RowsAffected > 0, exec.Error
}

// Finish 写回终态。running 才可写：终态一旦落定，过期调用方拿 0 行冲突。
// （worker 化时在此加 execution_version 列校验，见交付说明。）
func (s *PgStore) Finish(ctx context.Context, taskID string, version int64, res *RecognizeResult, taskErr error) error {
	if s.db == nil || !s.db.Available() {
		return store.ErrUnavailable
	}
	status := "succeeded"
	var result []byte
	errText := ""
	if taskErr != nil {
		status = "failed"
		errText = taskErr.Error()
	} else if res != nil {
		var err error
		result, err = json.Marshal(res)
		if err != nil {
			return err
		}
	}
	exec := s.db.Gorm.WithContext(ctx).Exec(`
		UPDATE ai_task
		SET status = ?, result = ?::jsonb, error = ?, updated_at = now()
		WHERE id = ?::uuid AND status = 'running'
	`, status, result, errText, taskID)
	if exec.Error != nil {
		return exec.Error
	}
	if exec.RowsAffected == 0 {
		return fmt.Errorf("%w: 任务不在运行中", ErrConflict)
	}
	return nil
}

// taskRow 是 ai_task 的扫描形态。
type taskRow struct {
	ID        string         `gorm:"column:id"`
	SpaceID   string         `gorm:"column:space_id"`
	Status    string         `gorm:"column:status"`
	Payload   []byte         `gorm:"column:payload"`
	Result    []byte         `gorm:"column:result"`
	Error     sql.NullString `gorm:"column:error"`
	CreatedBy string         `gorm:"column:created_by"`
	CreatedAt sql.NullTime   `gorm:"column:created_at"`
	UpdatedAt sql.NullTime   `gorm:"column:updated_at"`
}

func (r taskRow) toTask(kind string) (Task, error) {
	t := Task{
		ID:               r.ID,
		Kind:             kind,
		SpaceID:          r.SpaceID,
		ActorID:          r.CreatedBy,
		State:            TaskState(r.Status),
		Attempt:          1,
		ExecutionVersion: executionVersionFixed,
	}
	if r.CreatedAt.Valid {
		t.CreatedAt = r.CreatedAt.Time
	}
	if r.UpdatedAt.Valid {
		t.UpdatedAt = r.UpdatedAt.Time
	}
	if len(r.Payload) > 0 {
		if err := json.Unmarshal(r.Payload, &t.Input); err != nil {
			return t, fmt.Errorf("任务参数解析失败: %w", err)
		}
	}
	if len(r.Result) > 0 {
		res := &RecognizeResult{}
		if err := json.Unmarshal(r.Result, res); err != nil {
			return t, fmt.Errorf("任务结果解析失败: %w", err)
		}
		t.Result = res
	}
	t.Error = r.Error.String
	return t, nil
}

// candColumns 是候选查询的统一列清单。
const candColumns = `
	id, task_id, space_id, idx_no, revision, status, suggested,
	item_id, operation_id, confirmed_by, confirmed_at, discarded_at,
	created_by, created_at, updated_at`

// candRow 是 ai_candidate 的扫描形态；suggested 保留整条草稿，
// 状态列与标识列以数据库为准（草稿是快照，列是事实）。
type candRow struct {
	ID          string         `gorm:"column:id"`
	TaskID      string         `gorm:"column:task_id"`
	SpaceID     string         `gorm:"column:space_id"`
	Index       int            `gorm:"column:idx_no"`
	Revision    int64          `gorm:"column:revision"`
	Status      string         `gorm:"column:status"`
	Suggested   []byte         `gorm:"column:suggested"`
	ItemID      sql.NullString `gorm:"column:item_id"`
	OperationID sql.NullString `gorm:"column:operation_id"`
	ConfirmedBy sql.NullString `gorm:"column:confirmed_by"`
	ConfirmedAt sql.NullTime   `gorm:"column:confirmed_at"`
	DiscardedAt sql.NullTime   `gorm:"column:discarded_at"`
	CreatedBy   string         `gorm:"column:created_by"`
	CreatedAt   sql.NullTime   `gorm:"column:created_at"`
	UpdatedAt   sql.NullTime   `gorm:"column:updated_at"`
}

func (r candRow) toCandidate() (Candidate, error) {
	c := Candidate{}
	if len(r.Suggested) > 0 {
		if err := json.Unmarshal(r.Suggested, &c); err != nil {
			return c, fmt.Errorf("候选草稿解析失败: %w", err)
		}
	}
	c.ID = r.ID
	c.TaskID = r.TaskID
	c.SpaceID = r.SpaceID
	c.Index = r.Index
	c.Revision = r.Revision
	c.State = CandidateState(r.Status)
	c.ConfirmedItemID = r.ItemID.String
	c.ConfirmedOperationID = r.OperationID.String
	if r.ConfirmedAt.Valid {
		c.ConfirmedAt = r.ConfirmedAt.Time
	}
	if r.DiscardedAt.Valid {
		c.IgnoredAt = r.DiscardedAt.Time
	}
	if r.CreatedAt.Valid {
		c.CreatedAt = r.CreatedAt.Time
	}
	return c, nil
}

// Insert 在一个事务里写完整批候选（半个批次等于让界面展示残缺结果）。
// created_by 取自所属任务：识别是替任务发起人做的。
func (s *PgStore) Insert(ctx context.Context, taskID string, cands []Candidate) ([]Candidate, error) {
	if s.db == nil || !s.db.Available() {
		return nil, store.ErrUnavailable
	}
	out := make([]Candidate, 0, len(cands))
	txErr := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range cands {
			c := cands[i]
			c.TaskID = taskID
			if c.Index == 0 {
				c.Index = i + 1
			}
			if c.Revision == 0 {
				c.Revision = 1
			}
			c.State = CandidatePending
			payload, err := json.Marshal(c)
			if err != nil {
				return err
			}
			var row struct {
				ID string `gorm:"column:id"`
			}
			err = tx.Raw(`
				INSERT INTO ai_candidate
					(task_id, space_id, idx_no, revision, status, suggested, created_by)
				SELECT ?::uuid, ?, ?, ?, ?, ?::jsonb, created_by
				FROM ai_task WHERE id = ?::uuid
				RETURNING id
			`, taskID, c.SpaceID, c.Index, c.Revision, candPending, payload, taskID).Scan(&row).Error
			if err != nil {
				return err
			}
			c.ID = row.ID
			out = append(out, c)
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return out, nil
}

// ListByTask 按序号列出任务的候选。
func (s *PgStore) ListByTask(ctx context.Context, taskID string) ([]Candidate, error) {
	if s.db == nil || !s.db.Available() {
		return nil, store.ErrUnavailable
	}
	var rows []candRow
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT `+candColumns+` FROM ai_candidate
		WHERE task_id = ?::uuid ORDER BY idx_no
	`, taskID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return toCandidates(rows)
}

// ListBySpace 按库与状态列出候选（待校对列表）；states 为空表示全部。
func (s *PgStore) ListBySpace(ctx context.Context, spaceID string, states []CandidateState, limit, offset int) ([]Candidate, int, error) {
	if s.db == nil || !s.db.Available() {
		return nil, 0, store.ErrUnavailable
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	where := "space_id = ?::uuid"
	args := []any{spaceID}
	if len(states) > 0 {
		ph := make([]string, 0, len(states))
		for _, st := range states {
			ph = append(ph, "?")
			args = append(args, string(st))
		}
		where += " AND status IN (" + joinPlaceholders(ph) + ")"
	}
	var total int
	countArgs := append([]any{}, args...)
	err := s.db.Gorm.WithContext(ctx).Raw(
		"SELECT count(*) FROM ai_candidate WHERE "+where, countArgs...).Scan(&total).Error
	if err != nil {
		return nil, 0, err
	}
	var rows []candRow
	err = s.db.Gorm.WithContext(ctx).Raw(`
		SELECT `+candColumns+` FROM ai_candidate
		WHERE `+where+` ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...).Scan(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	out, err := toCandidates(rows)
	return out, total, err
}

func joinPlaceholders(ph []string) string {
	out := ""
	for i, p := range ph {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func toCandidates(rows []candRow) ([]Candidate, error) {
	out := make([]Candidate, 0, len(rows))
	for _, r := range rows {
		c, err := r.toCandidate()
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// GetTask 读一条任务；不存在返回 ErrNotFound。
func (s *PgStore) GetTask(ctx context.Context, taskID string) (Task, error) {
	if s.db == nil || !s.db.Available() {
		return Task{}, store.ErrUnavailable
	}
	var row taskRow
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT id, space_id, status, payload, result, error, created_by, created_at, updated_at
		FROM ai_task WHERE id = ?::uuid
	`, taskID).Scan(&row).Error
	if err != nil {
		return Task{}, err
	}
	if row.ID == "" {
		return Task{}, ErrNotFound
	}
	return row.toTask(TaskKindRecognize)
}

// GetCandidate 读一条候选。
func (s *PgStore) GetCandidate(ctx context.Context, candidateID string) (Candidate, error) {
	if s.db == nil || !s.db.Available() {
		return Candidate{}, store.ErrUnavailable
	}
	var rows []candRow
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT `+candColumns+` FROM ai_candidate WHERE id = ?::uuid
	`, candidateID).Scan(&rows).Error
	if err != nil {
		return Candidate{}, err
	}
	if len(rows) == 0 {
		return Candidate{}, ErrNotFound
	}
	return rows[0].toCandidate()
}

// Claim 把候选从 pending 标为 confirmed 并记下物品与操作标识。
// 返回 ErrConflict 表示已被处理（并发点击或重试）。
// AI 确认的真实路径在 service（要与建物品同事务）；这里是端口协议实现，
// 供导入等其它调用方复用。confirmed_by 未知时保持 NULL。
func (s *PgStore) Claim(ctx context.Context, candidateID, operationID, itemID string) (Candidate, error) {
	return s.claimWith(ctx, candidateID, operationID, itemID, "")
}

func (s *PgStore) claimWith(ctx context.Context, candidateID, operationID, itemID, actorID string) (Candidate, error) {
	if s.db == nil || !s.db.Available() {
		return Candidate{}, store.ErrUnavailable
	}
	exec := s.db.Gorm.WithContext(ctx).Exec(`
		UPDATE ai_candidate
		SET status = ?, operation_id = ?::uuid, item_id = ?::uuid,
		    confirmed_by = CASE WHEN ?::uuid IS NULL THEN confirmed_by ELSE ?::uuid END,
		    confirmed_at = now(), updated_at = now()
		WHERE id = ?::uuid AND status = ?
	`, candConfirmed, operationID, itemID, nullUUID(actorID), nullUUID(actorID), candidateID, candPending)
	if exec.Error != nil {
		return Candidate{}, exec.Error
	}
	if exec.RowsAffected == 0 {
		return Candidate{}, fmt.Errorf("%w: 候选已被处理", ErrConflict)
	}
	return s.GetCandidate(ctx, candidateID)
}

// Ignore 把候选标为 discarded；只有 pending 可以被丢弃。
func (s *PgStore) Ignore(ctx context.Context, candidateID string) (Candidate, error) {
	if s.db == nil || !s.db.Available() {
		return Candidate{}, store.ErrUnavailable
	}
	exec := s.db.Gorm.WithContext(ctx).Exec(`
		UPDATE ai_candidate
		SET status = ?, discarded_at = now(), updated_at = now()
		WHERE id = ?::uuid AND status = ?
	`, candDiscarded, candidateID, candPending)
	if exec.Error != nil {
		return Candidate{}, exec.Error
	}
	if exec.RowsAffected == 0 {
		return Candidate{}, fmt.Errorf("%w: 候选已被处理", ErrConflict)
	}
	return s.GetCandidate(ctx, candidateID)
}

// CountByTask 统计任务的候选数量。
func (s *PgStore) CountByTask(ctx context.Context, taskID string) (int, error) {
	if s.db == nil || !s.db.Available() {
		return 0, store.ErrUnavailable
	}
	var n int
	err := s.db.Gorm.WithContext(ctx).Raw(
		"SELECT count(*) FROM ai_candidate WHERE task_id = ?::uuid", taskID).Scan(&n).Error
	return n, err
}

// nullUUID 空串转 NULL（?:uuid 参数不接受空串）。
func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}
