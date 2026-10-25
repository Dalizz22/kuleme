package ai

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/google/uuid"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/inventory"
	"github.com/klm/server/internal/item"
	"github.com/klm/server/internal/store"
	"github.com/klm/server/internal/templates"
)

// Service 是 AI 模块的编排层：设置、识别任务、候选确认。
//
// 当前同步执行：CreateTask 在请求内跑完识别再返回（202 + 终态任务），
// 不起 worker 进程；外部 AI 请求永远不发生在持有事务期间
// （docs/接口契约约定.md：网络请求不进持锁事务）。
type Service struct {
	db *store.DB

	tasks    *PgStore
	settings *FileSettingsStore
	images   ImageLoader
	hints    *templateHinter
	catalog  *templates.Catalog
	items    itemCreator
	inv      invApplier
	// authz 用于创建任务时校验 actor 对目标库的读权。
	authz *authz.Checker
}

// itemCreator 是 item 模块的窄接口。
//
// item.NewService 返回未导出类型，跨包持有必须经自己的接口声明。
// CreateTx 在调用方事务内建物品——确认流程整体一个事务。
type itemCreator interface {
	CreateTx(ctx context.Context, tx *gorm.DB, actor httpx.Actor, in *item.CreateInput) (*item.View, error)
}

// invApplier 与 loan/sale 同款：库存事务入口的窄接口
// （inventory.NewService 返回未导出类型）。
type invApplier interface {
	ApplyOpTx(ctx context.Context, tx *gorm.DB, actor httpx.Actor, in *inventory.OpInput) (*inventory.OpResult, error)
}

// errCandidateHandled 表示候选已 confirmed/discarded，重复处理返回 409。
var errCandidateHandled = fmt.Errorf("%w: 候选已被处理", ErrConflict)

// validationError 是入参校验错误（400 语义）；router 用 errors.As 识别。
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

func invalid(msg string) error { return &validationError{msg: msg} }

// NewService 构造服务。模板资产损坏时返回错误（启动期问题，参照 item 模块：
// 调用方记日志并不挂路由）。数据库不可用不挡启动，由健康检查暴露。
func NewService(deps appdeps.Deps) (*Service, error) {
	cat, err := templates.Builtin()
	if err != nil {
		return nil, err
	}
	items, err := item.NewService(deps.DB)
	if err != nil {
		return nil, err
	}
	inv := inventory.NewService(deps.DB)
	// 主密钥读不到时降级为无加密：设置可读可改非密钥项，
	// 保存密钥会报错并在读取时提示 needsRekey（settings.go 的语义）。
	cipher, _ := LoadCipher(deps.Config.SecretPath(), true)
	return &Service{
		db:       deps.DB,
		tasks:    NewPgStore(deps.DB),
		settings: NewFileSettingsStore(deps.Config.DataDir, cipher),
		images:   newFileLoader(deps.DB, deps.Config.MediaDir()),
		hints:    newTemplateHinter(cat),
		catalog:  cat,
		items:    items,
		inv:      inv,
		authz:    authz.New(deps.DB),
	}, nil
}

// --- 设置 ---

// GetSettings 返回脱敏后的提供者配置。
func (s *Service) GetSettings(ctx context.Context) (MaskedSettings, error) {
	cfg, err := s.settings.Load(ctx)
	if err != nil {
		return MaskedSettings{}, err
	}
	return cfg.maskedWithRekey(), nil
}

// TestConnection 用当前保存的设置做一次真实识别（1×1 样例图）。
// 返回 TestResult 而不是错误：连接失败是测试结果，不是接口失败。仅管理员。
func (s *Service) TestConnection(ctx context.Context, actor httpx.Actor) (TestResult, error) {
	if !actor.IsAdmin {
		return TestResult{}, ErrForbidden
	}
	cfg, err := s.settings.Load(ctx)
	if err != nil {
		return TestResult{}, err
	}
	if !cfg.Configured() {
		return TestResult{OK: false, Message: "尚未配置提供者或未启用"}, nil
	}
	start := time.Now()
	recog := NewProvider(time.Duration(cfg.TimeoutSeconds) * time.Second)
	reqCtx := WithSettings(ctx, cfg)
	resp, err := recog.Recognize(reqCtx, RecognizeRequest{
		Mode:          ModeGroup,
		Images:        []ImageBytes{{FileID: "test", ContentType: "image/png", Data: samplePNG}},
		MaxCandidates: 1,
	})
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		msg := "连接失败"
		switch {
		case errors.Is(err, ErrRateLimited):
			msg = "提供者限流，请稍后重试"
		case errors.Is(err, ErrTimeout):
			msg = "请求超时"
		case errors.Is(err, ErrNotConfigured), errors.Is(err, ErrDisabled):
			msg = "配置不完整或未启用"
		}
		return TestResult{
			OK:        false,
			Message:   msg,
			Detail:    err.Error(),
			ElapsedMS: elapsed,
		}, nil
	}
	return TestResult{
		OK:         true,
		Message:    "连接成功",
		Model:      resp.Model,
		SawImage:   len(resp.Observations) > 0,
		Candidates: len(resp.Observations),
		ElapsedMS:  elapsed,
	}, nil
}

// samplePNG 是 1×1 像素 PNG，连接测试用。
//
//go:embed testdata/sample.png
var samplePNG []byte

// --- 任务 ---

// createTaskInput 是 POST /ai/tasks 的请求体。
type createTaskInput struct {
	SpaceID     string   `json:"space_id"`
	ItemID      string   `json:"item_id"`
	FileIDs     []string `json:"file_ids"`
	TemplateKey string   `json:"template_key"`
	ObjectHint  string   `json:"object_hint"`
	CategoryID  string   `json:"category_id"`
	LocationID  string   `json:"location_id"`
}

// CreateTask 建任务并同步执行识别，返回终态任务与候选。
func (s *Service) CreateTask(ctx context.Context, actor httpx.Actor, in createTaskInput) (Task, []Candidate, error) {
	cfg, err := s.settings.Load(ctx)
	if err != nil {
		return Task{}, nil, err
	}
	if !cfg.Configured() {
		return Task{}, nil, ErrNotConfigured
	}
	if strings.TrimSpace(in.SpaceID) == "" || len(in.FileIDs) == 0 {
		return Task{}, nil, invalid("space_id 与 file_ids 必填")
	}
	// 创建前先校验 actor 对目标库的可用权限，无权直接 403/404 返回，
	// 绝不为越权请求调用外部 AI（无库归属的私有库一律 404，不泄露存在性）。
	if err := s.authz.Check(ctx, actor, in.SpaceID, authz.ActionRead); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return Task{}, nil, httpx.NotFound("库不存在或无权访问")
		}
		return Task{}, nil, err
	}
	if len(in.FileIDs) > cfg.MaxImages {
		// §2.1 参数类错误统一 422；稳定码让客户端能针对性提示，不用解析中文。
		return Task{}, nil, httpx.New(422, httpx.CodeInvalidImageCount,
			fmt.Sprintf("图片数量超过上限 %d", cfg.MaxImages))
	}
	task := Task{
		Kind:    TaskKindRecognize,
		SpaceID: in.SpaceID,
		ActorID: actor.UserID,
		State:   TaskQueued,
		Input: RecognizeInput{
			Mode:          ModeGroup,
			ImageIDs:      in.FileIDs,
			TemplateID:    in.TemplateKey,
			ObjectHint:    in.ObjectHint,
			CategoryID:    in.CategoryID,
			LocationID:    in.LocationID,
			SpaceID:       in.SpaceID,
			MaxCandidates: 0,
		},
	}
	id, err := s.tasks.Enqueue(ctx, task)
	if err != nil {
		return Task{}, nil, err
	}
	task.ID = id

	// 同步执行。网络请求在事务外；用 WithoutCancel 让客户端断开
	// 不至于把任务留在 running（识别结果仍会写回，供轮询读取）。
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(cfg.TimeoutSeconds)*time.Second+10*time.Second)
	defer cancel()

	if err := s.tasks.startTask(runCtx, id); err != nil {
		return Task{}, nil, err
	}
	task.State = TaskRunning
	result, runErr := s.runRecognition(runCtx, task, cfg)
	if finErr := s.tasks.Finish(runCtx, id, executionVersionFixed, result, runErr); finErr != nil {
		// 写回失败只留日志：任务已经跑完，接口仍返回当前状态。
		fmt.Printf("ai: 任务 %s 结果写回失败: %v\n", id, finErr)
	}
	final, err := s.tasks.GetTask(runCtx, id)
	if err != nil {
		return Task{}, nil, err
	}
	cands, err := s.tasks.ListByTask(runCtx, id)
	if err != nil {
		return final, nil, err
	}
	return final, cands, nil
}

// runRecognition 执行一次识别：加载图片 → 调提供者 → 按模板校验 → 落候选。
// 返回的 result 为 nil 时 runErr 必须非 nil（任务失败）。
func (s *Service) runRecognition(ctx context.Context, task Task, cfg Settings) (*RecognizeResult, error) {
	start := time.Now()
	provider := NewProvider(time.Duration(cfg.TimeoutSeconds) * time.Second)

	images := make([]ImageBytes, 0, len(task.Input.ImageIDs))
	for _, fid := range task.Input.ImageIDs {
		img, err := s.images.LoadForRecognition(ctx, task.ActorID, task.SpaceID, fid)
		if err != nil {
			return nil, fmt.Errorf("图片 %s 不可用: %w", fid, err)
		}
		images = append(images, img)
	}
	hints, err := s.hints.FieldHints(ctx, task.Input.TemplateID)
	if err != nil {
		return nil, err
	}
	resp, err := provider.Recognize(WithSettings(ctx, cfg), RecognizeRequest{
		Mode:          task.Input.Mode,
		Images:        images,
		ObjectHint:    task.Input.ObjectHint,
		FieldHints:    hints,
		MaxCandidates: task.Input.MaxCandidates,
	})
	if err != nil {
		return nil, err
	}

	manualIDs, _ := s.hints.ManualFieldIDs(ctx, task.Input.TemplateID)
	cands := make([]Candidate, 0, len(resp.Observations))
	rejected := 0
	for _, obs := range resp.Observations {
		values, applied, _, rejectedFields, err := s.hints.ApplyFields(ctx, task.Input.TemplateID, obs.Fields, nil)
		if err != nil {
			return nil, err
		}
		confidence, _ := obs.Confidence.(float64)
		c := Candidate{
			TaskID:         task.ID,
			SpaceID:        task.SpaceID,
			Index:          len(cands) + 1,
			Revision:       1,
			Name:           obs.Name,
			Summary:        obs.Summary,
			Notes:          obs.Notes,
			Fields:         values,
			Applied:        applied,
			ManualFields:   manualIDs,
			SourceImageIDs: task.Input.ImageIDs,
			Evidence:       obs.Evidence,
			Confidence:     confidence,
			State:          CandidatePending,
		}
		if len(rejectedFields) > 0 {
			c.Warnings = append(c.Warnings, rejectedFields...)
			// 没有名称且所有字段都被拒：这条候选没有可用信息，丢弃。
			if strings.TrimSpace(c.Name) == "" && len(values) == 0 {
				rejected++
				continue
			}
		}
		cands = append(cands, c)
	}
	if len(cands) > 0 {
		var err error
		cands, err = s.tasks.Insert(ctx, task.ID, cands)
		if err != nil {
			return nil, err
		}
	}
	return &RecognizeResult{
		CandidateCount: len(cands),
		Empty:          len(cands) == 0,
		Provider:       provider.Name(),
		Model:          resp.Model,
		ElapsedMS:      time.Since(start).Milliseconds(),
		Rejected:       rejected,
	}, nil
}

// GetTask 返回任务与它的候选。
func (s *Service) GetTask(ctx context.Context, actor httpx.Actor, taskID string) (Task, []Candidate, error) {
	task, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, nil, err
	}
	// AI 未入库内容仅发起者可见（管理员例外）。
	if !actor.IsAdmin && task.ActorID != actor.UserID {
		return Task{}, nil, ErrNotFound
	}
	cands, err := s.tasks.ListByTask(ctx, taskID)
	if err != nil {
		return task, nil, err
	}
	return task, cands, nil
}

// --- 候选确认 ---

// Confirm 确认候选：建物品并落唯一确认关系。
//
// 顺序是「先占位、再建物品、失败回滚」：占位 UPDATE 是原子的，
// 双击/并发只有一方能把它从 pending 改走，另一方拿到 0 行返回 409，
// 不会建出第二件物品。item.Create 自身提交（见 itemCreator 注释），
// 它失败时事务回滚把候选还原为 pending。
func (s *Service) Confirm(ctx context.Context, actor httpx.Actor, candidateID string, in ConfirmInput) (ConfirmResult, error) {
	if strings.TrimSpace(candidateID) == "" {
		return ConfirmResult{}, invalid("候选标识必填")
	}
	if _, err := uuid.Parse(candidateID); err != nil {
		return ConfirmResult{}, ErrNotFound
	}
	cand, err := s.tasks.GetCandidate(ctx, candidateID)
	if err != nil {
		return ConfirmResult{}, err
	}
	// 候选归属发起者；管理员例外。查询失败必须 fail-closed（原先 err 被吞掉，
	// 查不到归属就跳过权限判定）。
	if owner, err := s.candidateOwner(ctx, candidateID); err != nil {
		return ConfirmResult{}, ErrNotFound
	} else if !actor.IsAdmin && owner != actor.UserID {
		return ConfirmResult{}, ErrNotFound
	}
	if in.SpaceID != "" && in.SpaceID != cand.SpaceID {
		return ConfirmResult{}, ErrForbidden
	}
	operationID := in.OperationID
	if operationID == "" {
		operationID = uuid.NewString()
	}

	// 幂等命中：同一操作键已被本人确认过，直接返回原物品。
	// confirmed_by 过滤保证幂等键按用户隔离：A 确认过的键，B 重放不得复用。
	if confirmed, err := s.findByOperation(ctx, actor.UserID, operationID); err == nil && confirmed.ID != "" {
		publicCode, movementID := s.confirmedArtifacts(ctx, confirmed.ConfirmedItemID, operationID)
		return ConfirmResult{
			CandidateID:   confirmed.ID,
			ItemID:        confirmed.ConfirmedItemID,
			PublicCode:    publicCode,
			OperationID:   operationID,
			MovementID:    movementID,
			AlreadyExists: true,
		}, nil
	}

	// 目标模板：请求指定 > 任务输入 > 默认模板。
	task, err := s.tasks.GetTask(ctx, cand.TaskID)
	if err != nil {
		return ConfirmResult{}, err
	}
	templateKey := in.TemplateKey
	if templateKey == "" {
		templateKey = task.Input.TemplateID
	}
	m, err := s.hints.Manifest(templateKey)
	if err != nil {
		return ConfirmResult{}, err
	}
	// 人工订正覆盖模型建议，两者走同一套模板校验。
	values, _, _, _, err := s.hints.ApplyFields(ctx, m.TemplateKey, cand.Fields, in.Fields)
	if err != nil {
		return ConfirmResult{}, err
	}
	name := in.Name
	if name == "" {
		name = cand.Name
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ConfirmResult{}, invalid("物品名称不能为空")
	}
	categoryID := in.CategoryID
	if categoryID == "" {
		categoryID = task.Input.CategoryID
	}
	quantity := in.Quantity
	if quantity == "" {
		quantity, _ = s.hints.DefaultQuantity(ctx, m.TemplateKey)
	}
	if quantity == "" {
		quantity = "1"
	}
	photoIDs := resolvePhotoIDs(in.PhotoIDs, in.PhotosSet, in.SharePhotos, cand.SourceImageIDs)
	// 照片必须真实存在且本人可用，防止把他人/他库文件挂进物品。
	photoIDs, err = s.validatePhotos(ctx, actor, task.ActorID, cand.SpaceID, photoIDs)
	if err != nil {
		return ConfirmResult{}, err
	}

	var created *item.View
	var movementID string
	txErr := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// ① 原子占位：只有 pending 能被改走。
		exec := tx.Exec(`
			UPDATE ai_candidate
			SET status = ?, operation_id = ?::uuid, confirmed_by = ?::uuid,
			    confirmed_at = now(), updated_at = now()
			WHERE id = ?::uuid AND status = ?
		`, candConfirmed, operationID, actor.UserID, candidateID, candPending)
		if exec.Error != nil {
			return exec.Error
		}
		if exec.RowsAffected == 0 {
			return errCandidateHandled
		}
		// ② 建物品（CreateTx 在本事务内，失败整体回滚）。
		v, err := s.items.CreateTx(ctx, tx, actor, &item.CreateInput{
			SpaceID:     cand.SpaceID,
			Name:        name,
			TemplateKey: m.TemplateKey,
			Note:        cand.Summary,
			CategoryID:  optString(categoryID),
			Unit:        m.Unit,
			Fields:      values,
		})
		if err != nil {
			return err
		}
		created = v
		// ②b 照片挂到新物品（首张主图），文件归属收紧到该库。
		for i, fid := range photoIDs {
			if err := tx.Exec(`
				INSERT INTO item_photo (item_id, file_id, sort, is_primary)
				VALUES (?, ?, ?, ?)`, v.ID, fid, i, i == 0).Error; err != nil {
				return err
			}
		}
		if len(photoIDs) > 0 {
			if err := tx.Exec(`
				UPDATE file_object SET space_id = ?::uuid, attached = true
				 WHERE id IN ? AND kind = 'photo'`, cand.SpaceID, photoIDs).Error; err != nil {
				return err
			}
		}
		// ②c 有位置则同事务建 initial 库存；无位置 = 待入库（status pending_stock）。
		if in.LocationID != "" {
			op, err := s.inv.ApplyOpTx(ctx, tx, actor, &inventory.OpInput{
				ItemID:      v.ID,
				Kind:        "initial",
				LocationID:  optString(in.LocationID),
				Quantity:    quantity,
				OperationID: operationID,
			})
			if err != nil {
				return err
			}
			if len(op.Movements) > 0 {
				movementID = op.Movements[0].ID
			}
		}
		// ③ 回填唯一确认关系。
		return tx.Exec(`
			UPDATE ai_candidate SET item_id = ?::uuid, updated_at = now()
			WHERE id = ?::uuid
		`, v.ID, candidateID).Error
	})
	if txErr != nil {
		// ③ 回填 item_id 撞 uq_ai_candidate_item 部分唯一索引 = 该物品已被
		// 其他候选确认过，属幂等冲突而非服务器错误，按目录层同款判定收 409。
		if isUniqueViolation(txErr) {
			return ConfirmResult{}, httpx.Conflict(httpx.CodeIdempotencyConflict,
				"该物品已由其他识别候选确认，请勿重复确认")
		}
		return ConfirmResult{}, txErr
	}
	return ConfirmResult{
		CandidateID: candidateID,
		ItemID:      created.ID,
		PublicCode:  created.PublicCode,
		OperationID: operationID,
		MovementID:  movementID,
	}, nil
}

// resolvePhotoIDs 落实照片共享语义：
//   - photo_ids 非空 → 只关联所列图片；
//   - photo_ids 明确为空数组 → 不关联图片；
//   - photo_ids 缺失/null 且 share_photos != false → 共享任务全部来源图片；
//   - photo_ids 缺失/null 且 share_photos = false → 不关联图片。
func resolvePhotoIDs(list []string, set bool, share *bool, source []string) []string {
	if len(list) > 0 {
		return list
	}
	if set {
		return nil
	}
	if share != nil && !*share {
		return nil
	}
	return source
}

// confirmedArtifacts 为幂等重放补齐响应快照：物品编码 + 本次确认的 initial 流水。
// 流水按调用方指定的 operationID 反查（ApplyOpTx 以它为 operation_id）；
// 未带位置确认（待入库）时没有流水。
func (s *Service) confirmedArtifacts(ctx context.Context, itemID, operationID string) (publicCode, movementID string) {
	if itemID == "" {
		return "", ""
	}
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT public_code FROM item WHERE id = ?::uuid`, itemID).Scan(&publicCode).Error; err != nil {
		publicCode = ""
	}
	if err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT id FROM stock_movement
		WHERE item_id = ?::uuid AND operation_id = ?::uuid AND kind = 'initial'
		LIMIT 1`, itemID, operationID).Scan(&movementID).Error; err != nil {
		movementID = ""
	}
	return publicCode, movementID
}

// validatePhotos 确认关联的照片须为真实存在、kind=photo 且本人可用的
// 文件——未归属（space_id IS NULL）仅上传者（本人或任务发起者，覆盖管理员代确认）
// 可用；已归属则必须同库。统一报「照片不存在或不可用」，不泄露他库文件存在性。
// 返回去重保序后的列表。
func (s *Service) validatePhotos(ctx context.Context, actor httpx.Actor, taskOwnerID, spaceID string, ids []string) ([]string, error) {
	seen := make(map[string]bool, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return nil, invalid("照片不存在或不可用")
		}
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return nil, nil
	}
	var rows []photoRow
	if err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT id, space_id::text AS space_id, COALESCE(uploaded_by::text, '') AS uploaded_by
		FROM file_object WHERE id IN ? AND kind = 'photo'`, uniq).Scan(&rows).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]photoRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	for _, id := range uniq {
		r, ok := byID[id]
		if !ok {
			return nil, invalid("照片不存在或不可用")
		}
		if r.SpaceID != nil && *r.SpaceID == spaceID {
			continue
		}
		if r.SpaceID == nil && (r.UploadedBy == actor.UserID || r.UploadedBy == taskOwnerID) {
			continue
		}
		return nil, invalid("照片不存在或不可用")
	}
	return uniq, nil
}

type photoRow struct {
	ID         string  `gorm:"column:id"`
	SpaceID    *string `gorm:"column:space_id"`
	UploadedBy string  `gorm:"column:uploaded_by"`
}

// Discard 丢弃候选（只改状态；已确认的候选不在此回滚物品）。
func (s *Service) Discard(ctx context.Context, actor httpx.Actor, candidateID string) (Candidate, error) {
	if _, err := uuid.Parse(candidateID); err != nil {
		return Candidate{}, ErrNotFound
	}
	cand, err := s.tasks.GetCandidate(ctx, candidateID)
	if err != nil {
		return Candidate{}, err
	}
	// 仅发起者（或管理员）可丢弃候选。
	if owner, oerr := s.candidateOwner(ctx, candidateID); oerr == nil && !actor.IsAdmin && owner != actor.UserID {
		return Candidate{}, ErrNotFound
	}
	cand, err = s.tasks.Ignore(ctx, candidateID)
	if err != nil {
		return Candidate{}, err
	}
	_ = actor
	return cand, nil
}

// findByOperation 按幂等键查本人已确认候选。查不到或出错都按未命中处理。
func (s *Service) findByOperation(ctx context.Context, actorID, operationID string) (Candidate, error) {
	if operationID == "" {
		return Candidate{}, ErrNotFound
	}
	var rows []candRow
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT `+candColumns+`
		FROM ai_candidate
		WHERE operation_id = ?::uuid AND confirmed_by = ?::uuid AND status = ?
	`, operationID, actorID, candConfirmed).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return Candidate{}, ErrNotFound
	}
	return rows[0].toCandidate()
}

// optString 把空串转成 nil 指针（item.CreateInput 的可选字段形态）。
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ponytail: strings.Contains 判唯一冲突，与 catalog/item 同款；细分 pg 错误码等有第二种冲突再加。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

// --- ImageLoader 实现 ---

// maxImageBytes 是单张图片读入内存的上限。
const maxImageBytes = 20 << 20

// fileLoader 从 file_object + 媒体目录读取照片字节。
type fileLoader struct {
	db       *store.DB
	mediaDir string
}

func newFileLoader(db *store.DB, mediaDir string) *fileLoader {
	return &fileLoader{db: db, mediaDir: mediaDir}
}

// LoadForRecognition 读取一张已上传照片。
//
// 权限规则（ImageLoader 端口没有管理员标志，从简）：
// 未关联图片仅上传者本人可用；已关联（attached）或归属了同一库的图片可用。
// 返回错误一律 ErrBadImage，不区分「不存在」与「无权」（对外同语义）。
func (l *fileLoader) LoadForRecognition(ctx context.Context, actorID, spaceID, fileID string) (ImageBytes, error) {
	if l.db == nil || !l.db.Available() {
		return ImageBytes{}, store.ErrUnavailable
	}
	if _, err := uuid.Parse(fileID); err != nil {
		return ImageBytes{}, fmt.Errorf("%w: 图片标识不合法", ErrBadImage)
	}
	var row struct {
		ContentType string         `gorm:"column:content_type"`
		StoragePath string         `gorm:"column:storage_path"`
		UploadedBy  string         `gorm:"column:uploaded_by"`
		SpaceID     sql.NullString `gorm:"column:space_id"`
		ByteSize    int64          `gorm:"column:byte_size"`
	}
	err := l.db.Gorm.WithContext(ctx).Raw(`
		SELECT content_type, storage_path, uploaded_by, space_id, byte_size
		FROM file_object WHERE id = ?::uuid AND kind = 'photo'
	`, fileID).Scan(&row).Error
	if err != nil {
		return ImageBytes{}, fmt.Errorf("%w: %v", ErrBadImage, err)
	}
	if row.StoragePath == "" {
		return ImageBytes{}, fmt.Errorf("%w: 图片不存在", ErrBadImage)
	}
	if !strings.HasPrefix(row.ContentType, "image/") {
		return ImageBytes{}, fmt.Errorf("%w: 不支持的类型 %s", ErrBadImage, row.ContentType)
	}
	if row.SpaceID.Valid && row.SpaceID.String != "" && spaceID != "" && row.SpaceID.String != spaceID {
		return ImageBytes{}, fmt.Errorf("%w: 图片不属于目标库", ErrBadImage)
	}
	if !row.SpaceID.Valid || row.SpaceID.String == "" {
		// 未入库图片只对上传者可见。
		if row.UploadedBy != actorID {
			return ImageBytes{}, fmt.Errorf("%w: 图片不可用", ErrBadImage)
		}
	}
	if row.ByteSize > maxImageBytes {
		return ImageBytes{}, fmt.Errorf("%w: 图片超过 %d MB 上限", ErrBadImage, maxImageBytes>>20)
	}
	// filepath.Base 防御 storage_path 里的路径分隔符（库内值，双重保险）。
	data, err := os.ReadFile(filepath.Join(l.mediaDir, filepath.Base(row.StoragePath)))
	if err != nil {
		return ImageBytes{}, fmt.Errorf("%w: 读取文件失败", ErrBadImage)
	}
	if len(data) > maxImageBytes {
		return ImageBytes{}, fmt.Errorf("%w: 图片超过 %d MB 上限", ErrBadImage, maxImageBytes>>20)
	}
	return ImageBytes{FileID: fileID, ContentType: row.ContentType, Data: data}, nil
}

// candidateOwner 查候选归属（发起者），用于 可见性裁决。
func (s *Service) candidateOwner(ctx context.Context, candidateID string) (string, error) {
	var owner string
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT created_by FROM ai_candidate WHERE id = ?::uuid`, candidateID).Scan(&owner).Error
	if err != nil || owner == "" {
		return "", ErrNotFound
	}
	return owner, nil
}
