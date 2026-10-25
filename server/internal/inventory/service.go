// Package inventory 实现库存操作：余额变更、流水与幂等。
//
// 铁律：余额与流水同事务提交；库存变化只能经本服务完成，
// 借还、售卖、导入确认都调用 ApplyOp，不各自改余额。幂等键与操作者、命令、
// 请求摘要绑定：同一键同一内容重放原结果，不同内容 409。
package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// newOperationID 生成操作/流水/余额行的随机 ID。流水 ID 与操作 ID 都不
// 承担业务语义，唯一性即可。
func newOperationID() string { return uuid.NewString() }

// OpInput 是一次库存操作的请求。
type OpInput struct {
	ItemID       string  `json:"item_id" binding:"required"`
	Kind         string  `json:"kind" binding:"required"`
	LocationID   *string `json:"location_id"`
	ToLocationID *string `json:"to_location_id"`
	Quantity     string  `json:"quantity"`
	Reason       string  `json:"reason"`
	// Key 是幂等键（来自 Idempotency-Key 头）；空表示不幂等（纯查询类系统调用）。
	Key string `json:"-"`
	// OperationID 是调用方指定的操作 ID（仅 ApplyOpTx 使用），用于事后按操
	// 作 ID 反查流水（如 AI 确认重放取 movement_id）；空则随机生成。
	OperationID string `json:"-"`
}

// OpResult 是操作结果：流水行与操作后的在手总量。
type OpResult struct {
	ItemID      string         `json:"item_id"`
	OperationID string         `json:"operation_id"`
	Movements   []movementView `json:"movements"`
	OnHandTotal string         `json:"on_hand_total"`
}

type movementView struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	LocationID *string `json:"location_id"`
	Quantity   string  `json:"quantity"`
}

// kindSem 描述每种操作对余额的作用方向。
type kindSem struct {
	onHand      int // +1 入库方向 / -1 出库方向 / 0 特殊（move/correction/reserved）
	reserved    int // +1 预占方向 / -1 释放方向 / 0 不动预占
	move        bool
	correction  bool
	deliver     bool   // sale_deliver：on_hand -q 且 reserved -q
	toLifecycle string // 终态动作随库存操作同事务改物品生命周期
}

var kinds = map[string]kindSem{
	"initial":      {onHand: 1},
	"replenish":    {onHand: 1},
	"move_in":      {onHand: 1},
	"move_out":     {onHand: -1},
	"consume":      {onHand: -1},
	"scrap":        {onHand: -1, toLifecycle: "scrapped"},
	"lost":         {onHand: -1, toLifecycle: "lost"},
	"move":         {move: true},
	"correction":   {correction: true},
	"loan_out":     {onHand: -1},
	"loan_return":  {onHand: 1},
	"sale_reserve": {reserved: 1},
	"sale_release": {reserved: -1},
	"sale_deliver": {deliver: true, toLifecycle: "sold"},
}

type inventoryService struct {
	db    *store.DB
	authz *authz.Checker
}

func newService(db *store.DB) *inventoryService {
	return &inventoryService{db: db, authz: authz.New(db)}
}

// CodeInvalidLocation 位置不存在或已删除：用户可修复的输入问题，按 422 返回。
const CodeInvalidLocation = "INVALID_LOCATION"

// invalidLocation 是位置类校验的统一出口（建档初始库存、导入直接入库都用它）。
func invalidLocation() *httpx.Error {
	return httpx.New(422, CodeInvalidLocation, "位置不存在或不可用，请重新选择")
}

// CheckLocation 校验位置存在且未删除，供建档/导入等入口在写库前统一拒绝坏位置。
func CheckLocation(ctx context.Context, db *store.DB, id string) error {
	return checkLocation(ctx, db.Gorm, id)
}

func checkLocation(ctx context.Context, db *gorm.DB, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return invalidLocation()
	}
	var n int64
	if err := db.WithContext(ctx).Raw(
		`SELECT count(*) FROM location WHERE id = ? AND deleted_at IS NULL`, id).Scan(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return invalidLocation()
	}
	return nil
}

// checkLocations 校验本次操作涉及的全部位置（含移库目标位）。
func checkLocations(ctx context.Context, db *gorm.DB, in *OpInput) error {
	seen := map[string]bool{}
	for _, loc := range []*string{in.LocationID, in.ToLocationID} {
		if loc == nil || *loc == "" || seen[*loc] {
			continue
		}
		seen[*loc] = true
		if err := checkLocation(ctx, db, *loc); err != nil {
			return err
		}
	}
	return nil
}

// ApplyOp 校验并执行一次库存操作。幂等键存在时整个流程在事务内完成，
// 重放返回原结果。
func (s *inventoryService) ApplyOp(ctx context.Context, actor httpx.Actor, in *OpInput) (*OpResult, error) {
	sem, ok := kinds[in.Kind]
	if !ok {
		return nil, httpx.Validation("未知的库存操作类型: " + in.Kind)
	}
	if err := s.validateInput(in, sem); err != nil {
		return nil, err
	}
	if err := validateOpUUIDs(in); err != nil {
		return nil, err
	}
	if err := checkLocations(ctx, s.db.Gorm, in); err != nil {
		return nil, err
	}
	// 物品与权限：操作库存 = 对归属库 write。
	item, err := s.loadItem(ctx, nil, in.ItemID)
	if err != nil {
		return nil, err
	}
	if err := s.checkSpace(ctx, actor, item.SpaceID); err != nil {
		return nil, err
	}

	if in.Key != "" {
		return s.applyIdempotent(ctx, actor, item, in, sem)
	}
	return s.apply(ctx, nil, actor, item, in, sem, newOperationID())
}

// ApplyOpTx 在调用方事务内执行一次库存操作（借还/售卖复用，不做幂等——
// 业务记录本身承担幂等）。tx 为 nil 时行为同 ApplyOp。
func (s *inventoryService) ApplyOpTx(ctx context.Context, tx *gorm.DB, actor httpx.Actor, in *OpInput) (*OpResult, error) {
	sem, ok := kinds[in.Kind]
	if !ok {
		return nil, httpx.Validation("未知的库存操作类型: " + in.Kind)
	}
	if err := s.validateInput(in, sem); err != nil {
		return nil, err
	}
	locDB := s.db.Gorm
	if tx != nil {
		locDB = tx
	}
	if err := checkLocations(ctx, locDB, in); err != nil {
		return nil, err
	}
	// tx 为 nil 时退回连接池；非 nil 时必须用 tx 读物品——建物品与初始库存
	// 在同一事务内时，池连接看不到未提交的行（会误报 404 物品不存在）。
	item, err := s.loadItem(ctx, tx, in.ItemID)
	if err != nil {
		return nil, err
	}
	if err := s.checkSpace(ctx, actor, item.SpaceID); err != nil {
		return nil, err
	}
	opID := in.OperationID
	if opID == "" {
		opID = newOperationID()
	}
	return s.apply(ctx, tx, actor, item, in, sem, opID)
}

// validateInput 数量与位置的组合校验。
// validateOpUUIDs 校验 body 里的 uuid：非法值会把 22P02 冒成 500。
func validateOpUUIDs(in *OpInput) error {
	if err := httpx.ValidateUUID("item_id", in.ItemID, true); err != nil {
		return err
	}
	if in.LocationID != nil {
		if err := httpx.ValidateUUID("location_id", *in.LocationID, false); err != nil {
			return err
		}
	}
	if in.ToLocationID != nil {
		if err := httpx.ValidateUUID("to_location_id", *in.ToLocationID, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *inventoryService) validateInput(in *OpInput, sem kindSem) error {
	if in.Quantity == "" {
		return httpx.Validation("缺少数量")
	}
	neg := strings.HasPrefix(strings.TrimSpace(in.Quantity), "-")
	if !sem.correction && neg {
		return httpx.Validation("该操作的数量不能为负，纠错请用 correction")
	}
	if in.Kind == "correction" && in.Reason == "" {
		return httpx.Validation("correction 必须填写原因")
	}
	if sem.move {
		if in.LocationID == nil || *in.LocationID == "" || in.ToLocationID == nil || *in.ToLocationID == "" {
			return httpx.Validation("move 需要来源位置与目标位置")
		}
		if *in.LocationID == *in.ToLocationID {
			return httpx.Validation("来源位置与目标位置相同")
		}
	} else if sem.onHand != 0 || sem.reserved != 0 || sem.deliver {
		if in.LocationID == nil || *in.LocationID == "" {
			return httpx.Validation("该操作需要指定位置")
		}
	}
	return nil
}

// applyIdempotent 处理幂等键：原子占位 → 执行业务 → 落终态。
//
// 旧实现「事务外 SELECT + 事务后 UPSERT」有竞态：两个同键并发请求都在
// SELECT 落空后各自执行，库存被双扣。改为先以 INSERT ... ON CONFLICT
// DO NOTHING 抢占 in_progress 占位行：唯一索引就是并发串行化点——抢到者
// 独占执行，未抢到者读行重放或 409。in_progress 由此真正可达（并发第二路
// 或进程崩溃残留），统一 409 轮询语义（残留行需运维清键后重试）。
func (s *inventoryService) applyIdempotent(ctx context.Context, actor httpx.Actor, item *itemRow, in *OpInput, sem kindSem) (*OpResult, error) {
	// 流水 operation_id 与幂等键对齐，崩溃恢复可按 operation_id 反查流水。
	// 非 UUID 键（内部调用）不能进 uuid 列，保留原随机 operation id。
	if _, err := uuid.Parse(in.Key); err == nil {
		in.OperationID = in.Key
	}
	digest := requestDigest(actor.UserID, in)
	claim := s.db.Gorm.WithContext(ctx).Exec(`
INSERT INTO idempotency_key (key, actor_id, command, request_digest, state, created_at)
VALUES (?, ?, 'inventory.operation', ?, 'in_progress', now())
ON CONFLICT (key) DO NOTHING`, in.Key, actor.UserID, digest)
	if claim.Error != nil {
		return nil, claim.Error
	}
	if claim.RowsAffected == 0 {
		return s.replayOrTakeover(ctx, actor, item, in, sem, digest)
	}
	return s.runClaimed(ctx, actor, item, in, sem, digest)
}

// replayOrTakeover 处理已存在的幂等键：成功 → 重放；不同内容 → 409；
// 进行中 → 409；failed → 条件更新重新占位后重跑（并发重试只有一路能占到）。
func (s *inventoryService) replayOrTakeover(ctx context.Context, actor httpx.Actor, item *itemRow, in *OpInput, sem kindSem, digest string) (*OpResult, error) {
	var prior struct {
		State         string
		RequestDigest string
		ResponseBody  []byte
		HTTPStatus    *int
	}
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT state, request_digest, response_body, http_status
		 FROM idempotency_key WHERE key = ?`, in.Key).Scan(&prior).Error
	if err != nil {
		return nil, err
	}
	if prior.RequestDigest != "" && prior.RequestDigest != digest {
		return nil, httpx.New(409, httpx.CodeIdempotencyConflict, "同一幂等键已被不同内容使用")
	}
	if prior.State == "succeeded" && len(prior.ResponseBody) > 0 {
		var res OpResult
		if err := json.Unmarshal(prior.ResponseBody, &res); err == nil {
			return &res, nil
		}
	}
	if prior.State != "failed" {
		return nil, httpx.New(409, httpx.CodeIdempotencyConflict, "相同操作正在处理中，请稍后查询")
	}
	take := s.db.Gorm.WithContext(ctx).Exec(`
UPDATE idempotency_key
SET state = 'in_progress', http_status = NULL, response_body = NULL, completed_at = NULL
WHERE key = ? AND state = 'failed'`, in.Key)
	if take.Error != nil {
		return nil, take.Error
	}
	if take.RowsAffected == 0 {
		return nil, httpx.New(409, httpx.CodeIdempotencyConflict, "相同操作正在处理中，请稍后查询")
	}
	return s.runClaimed(ctx, actor, item, in, sem, digest)
}

// runClaimed 在已持有占位的前提下执行业务，并把终态写回幂等行。
func (s *inventoryService) runClaimed(ctx context.Context, actor httpx.Actor, item *itemRow, in *OpInput, sem kindSem, digest string) (*OpResult, error) {
	opID := in.OperationID
	if opID == "" {
		opID = newOperationID()
	}
	res, applyErr := s.apply(ctx, nil, actor, item, in, sem, opID)

	// 落终态（独立于业务事务：业务失败也要留下 failed 状态防重放错位）。
	recordState, recordStatus, recordBody := "failed", 500, []byte(nil)
	if applyErr == nil {
		recordState, recordStatus = "succeeded", 200
		if b, err := json.Marshal(res); err == nil {
			recordBody = b
		}
	} else if httpErr := (*httpx.Error)(nil); errors.As(applyErr, &httpErr) {
		recordStatus = httpErr.Status
	}
	finalize := s.db.Gorm.WithContext(ctx).Exec(`
UPDATE idempotency_key
SET state = ?, http_status = ?, response_body = ?, completed_at = now()
WHERE key = ?`, recordState, recordStatus, recordBody, in.Key)
	if finalize.Error != nil {
		if res != nil {
			return res, nil // 业务成功，幂等记录失败不回滚用户操作
		}
		return nil, finalize.Error
	}
	return res, applyErr
}

// apply 在事务内写余额与流水。tx 非 nil 时沿用外部事务（借还/售卖复用入口）。
func (s *inventoryService) apply(ctx context.Context, tx *gorm.DB, actor httpx.Actor, item *itemRow, in *OpInput, sem kindSem, opID string) (*OpResult, error) {
	if tx == nil {
		var out *OpResult
		err := s.db.Gorm.WithContext(ctx).Transaction(func(t *gorm.DB) error {
			res, err := s.applyInner(ctx, t, actor, item, in, sem, opID)
			out = res
			return err
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	return s.applyInner(ctx, tx, actor, item, in, sem, opID)
}

func (s *inventoryService) applyInner(ctx context.Context, tx *gorm.DB, actor httpx.Actor, item *itemRow, in *OpInput, sem kindSem, opID string) (*OpResult, error) {
	// serial 物品单次操作数量恒为 1；盘点修正（correction）按实盘差异整批落账，不受此限。
	q := strings.TrimSpace(in.Quantity)
	if item.Management == "serial" && !sem.correction && q != "1" {
		return nil, httpx.Validation("按件管理的物品每次操作数量只能是 1")
	}
	if !validQuantity(in.Quantity) {
		return nil, httpx.Validation("数量格式不正确，最多 4 位小数")
	}
	type step struct {
		location *string
		kind     string
		delta    string // 在手增量（带符号；纯预占操作为 "0"）
		resDelta string // 预占增量（带符号；不动预占为 ""）
		clampRes bool   // sale_deliver：预占扣到 0 为止
	}
	q = in.Quantity
	var steps []step
	switch {
	case sem.move:
		steps = []step{
			{in.LocationID, "move_out", "-" + q, "", false},
			{in.ToLocationID, "move_in", q, "", false},
		}
	case sem.correction:
		steps = []step{{in.LocationID, "correction", q, "", false}}
	case sem.deliver: // 预占释放（无预占按 0 兜底）+ 实物出库
		steps = []step{{in.LocationID, in.Kind, "-" + q, "-" + q, true}}
	case sem.reserved != 0:
		sign := "+"
		if sem.reserved < 0 {
			sign = "-"
		}
		steps = []step{{in.LocationID, in.Kind, "0", sign + q, false}}
	default:
		sign := ""
		if sem.onHand < 0 {
			sign = "-"
		}
		steps = []step{{in.LocationID, in.Kind, sign + q, "", false}}
	}

	// 生命周期闸门：入库/移库要求 normal；终态动作只允许从 normal 出发。
	if item.Lifecycle != "normal" && item.Lifecycle != "" {
		return nil, httpx.New(409, httpx.CodeOpenBusiness, "物品当前状态（"+item.Lifecycle+"）不允许库存操作")
	}

	res := &OpResult{ItemID: item.ID, OperationID: opID}
	for _, st := range steps {
		if err := applyBalance(tx, item.ID, st.location, st.delta, st.resDelta, st.clampRes); err != nil {
			return nil, err
		}
		mvID := newOperationID()
		if err := tx.Exec(`
INSERT INTO stock_movement (id, item_id, location_id, kind, quantity, on_hand_delta, reason, operation_id, actor_id, owning_space_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			mvID, item.ID, st.location, st.kind, quantityAbs(st.delta),
			st.delta, in.Reason, opID, actor.UserID, item.SpaceID).Error; err != nil {
			return nil, err
		}
		res.Movements = append(res.Movements, movementView{ID: mvID, Kind: st.kind, LocationID: st.location, Quantity: quantityAbs(st.delta)})
	}
	// 终态动作：余额落账成功后同事务写生命周期（版本 +1 供乐观锁失效）。
	if sem.toLifecycle != "" {
		lc := tx.Exec(`
UPDATE item SET lifecycle = ?, for_sale = false, version = version + 1, updated_at = now()
 WHERE id = ? AND lifecycle = 'normal'`, sem.toLifecycle, item.ID)
		if lc.Error != nil {
			return nil, lc.Error
		}
		if lc.RowsAffected == 0 {
			return nil, httpx.New(409, httpx.CodeOpenBusiness, "物品状态已变化，操作被拒绝")
		}
	}
	total, err := totalOnHand(ctx, tx, item.ID)
	if err != nil {
		return nil, err
	}
	res.OnHandTotal = total
	return res, nil
}

// quantityAbs 去掉增量的符号得到数量（"0" 原样返回，纯预占操作记 0 出库）。
func quantityAbs(delta string) string {
	return strings.TrimPrefix(strings.TrimPrefix(delta, "-"), "+")
}

// applyBalance 对 (item, location) 余额做在手/预占增量，越界时 409。
// location 为 nil 表示「未定位」余额行；onHandDelta/resDelta 为带符号数值串。
func applyBalance(tx *gorm.DB, itemID string, location *string, onHandDelta, resDelta string, clampRes bool) error {
	// 空串打到 numeric 会 500；不动的那一路归一成 0。
	if onHandDelta == "" {
		onHandDelta = "0"
	}
	if resDelta == "" {
		resDelta = "0"
	}
	// clampRes：sale_deliver 一卖出库，预占没有就按 0 兜底（卖空不许把预占打成负数）。
	resExpr := "reserved = reserved + ?::numeric"
	if clampRes {
		resExpr = "reserved = GREATEST(reserved + ?::numeric, 0)"
	}
	res := tx.Exec(`
UPDATE stock_balance
   SET on_hand = on_hand + ?::numeric, `+resExpr+`, updated_at = now()
 WHERE item_id = ? AND location_id IS NOT DISTINCT FROM ?`,
		onHandDelta, resDelta, itemID, location)
	if res.Error != nil {
		if strings.Contains(res.Error.Error(), "check constraint") || strings.Contains(res.Error.Error(), "violates check") {
			return httpx.New(409, httpx.CodeOpenBusiness, "库存不足或数量非法，操作被拒绝")
		}
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	// 无余额行：任一负增量撞不存在的行 = 无库存/无预占可扣。
	if strings.HasPrefix(onHandDelta, "-") || strings.HasPrefix(resDelta, "-") {
		return httpx.New(409, httpx.CodeOpenBusiness, "该位置没有可扣减的库存或预占")
	}
	// ponytail: GREATEST 兜住 sale_reserve 对新行的 on_hand=0 插入；语义再复杂再拆
	err := tx.Exec(`
INSERT INTO stock_balance (id, item_id, location_id, on_hand, reserved)
VALUES (?, ?, ?, GREATEST(?::numeric, 0), GREATEST(?::numeric, 0))`,
		newOperationID(), itemID, location, quantityAbs(onHandDelta), quantityAbs(resDelta)).Error
	// item/location 任一不存在 → FK violation；走到这里说明位置在校验后
	// 被并发删除，仍按位置类输入错误返回 422（不给 500）。
	if err != nil && strings.Contains(err.Error(), "violates foreign key") {
		return invalidLocation()
	}
	return err
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// MovementDetail 是 /operations/{id} 的返回形态。
type MovementDetail struct {
	ID          string  `json:"id"`
	ItemID      string  `json:"item_id"`
	LocationID  *string `json:"location_id"`
	Kind        string  `json:"kind"`
	Quantity    string  `json:"quantity"`
	OnHandDelta string  `json:"on_hand_delta"`
	Reason      string  `json:"reason"`
	OperationID string  `json:"operation_id"`
	ActorID     string  `json:"actor_id"`
	CreatedAt   string  `json:"created_at"`
}

// GetMovement 按流水 ID 查询，校验物品归属库对用户可见。
func (s *inventoryService) GetMovement(ctx context.Context, actor httpx.Actor, id string) (*MovementDetail, error) {
	var row struct {
		MovementDetail
		SpaceID string `gorm:"column:space_id"`
	}
	err := s.db.Gorm.WithContext(ctx).Raw(`
SELECT m.id, m.item_id, m.location_id, m.kind, rtrim(to_char(m.quantity, 'FM999999999990.9999'), '.') AS quantity,
       rtrim(to_char(m.on_hand_delta, 'FM999999999990.9999'), '.') AS on_hand_delta,
       m.reason, COALESCE(m.operation_id, '') AS operation_id, m.actor_id,
       to_char(m.created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at,
       COALESCE(m.owning_space_id, i.space_id) AS space_id
FROM stock_movement m JOIN item i ON i.id = m.item_id
WHERE m.id = ?`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, httpx.NotFound("操作记录不存在")
	}
	if err := s.checkSpace(ctx, actor, row.SpaceID); err != nil {
		return nil, err
	}
	return &row.MovementDetail, nil
}

// Balances 返回物品在各位置的余额（详情页展示用）。
func (s *inventoryService) Balances(ctx context.Context, actor httpx.Actor, itemID string) ([]map[string]any, error) {
	item, err := s.loadItem(ctx, nil, itemID)
	if err != nil {
		return nil, err
	}
	if err := s.checkSpace(ctx, actor, item.SpaceID); err != nil {
		return nil, err
	}
	rows := []map[string]any{} // 空表必须是 [] 而不是 null（§2 列表契约）
	err = s.db.Gorm.WithContext(ctx).Raw(`
SELECT b.location_id, l.name AS location_name,
       rtrim(to_char(b.on_hand, 'FM999999999990.9999'), '.') AS on_hand,
       rtrim(to_char(b.reserved, 'FM999999999990.9999'), '.') AS reserved
FROM stock_balance b LEFT JOIN location l ON l.id = b.location_id
WHERE b.item_id = ? ORDER BY b.updated_at DESC`, itemID).Scan(&rows).Error
	return rows, err
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

type itemRow struct {
	ID         string
	SpaceID    string
	Management string
	Lifecycle  string
}

func (s *inventoryService) loadItem(ctx context.Context, db *gorm.DB, id string) (*itemRow, error) {
	if db == nil {
		db = s.db.Gorm
	}
	var r itemRow
	err := db.WithContext(ctx).Raw(
		`SELECT id, space_id, management, lifecycle FROM item WHERE id = ? AND deleted_at IS NULL`, id).
		Scan(&r).Error
	if err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, httpx.NotFound("物品不存在")
	}
	return &r, nil
}

func (s *inventoryService) checkSpace(ctx context.Context, actor httpx.Actor, spaceID string) error {
	if err := s.authz.Check(ctx, actor, spaceID, authz.ActionWrite); err != nil {
		if errors.Is(err, authz.ErrNoSpace) {
			return httpx.NotFound("物品不存在或无权访问")
		}
		return err
	}
	return nil
}

func totalOnHand(ctx context.Context, tx *gorm.DB, itemID string) (string, error) {
	var total string
	err := tx.WithContext(ctx).Raw(
		`SELECT rtrim(to_char(COALESCE(SUM(on_hand),0), 'FM999999999990.9999'), '.') FROM stock_balance WHERE item_id = ?`,
		itemID).Scan(&total).Error
	return total, err
}

func validQuantity(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" || s == "-" || s == "+" {
		return false
	}
	body := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	if body == "" {
		return false
	}
	dot := strings.IndexByte(body, '.')
	intPart, fracPart := body, ""
	if dot >= 0 {
		intPart, fracPart = body[:dot], body[dot+1:]
	}
	if intPart == "" && fracPart == "" {
		return false
	}
	// numeric(18,4)：总精度 18 含 4 位小数，整数部分上限是 14（18=14+4）。
	if len(intPart) > 14 {
		return false
	}
	for _, r := range intPart + fracPart {
		if r < '0' || r > '9' {
			return false
		}
	}
	if len(fracPart) > 4 {
		return false
	}
	return true
}

// requestDigest 规范化请求摘要：同键不同内容要能被识别。
func requestDigest(actorID string, in *OpInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%s",
		actorID, in.ItemID, in.Kind, deref(in.LocationID), deref(in.ToLocationID),
		strings.TrimSpace(in.Quantity), in.Reason)
	return hex.EncodeToString(h.Sum(nil))
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// NewService 导出构造，供 loan/sale 复用 ApplyOpTx 事务入口。
func NewService(db *store.DB) *inventoryService { return newService(db) }
