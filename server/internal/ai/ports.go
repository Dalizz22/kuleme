package ai

import (
	"context"
	"errors"
	"time"
)

// 本文件定义 AI 模块需要的全部窄接口（ports）。
//
// 全部由核心后端（B）实现，或者由 bootstrap 按当前部署装配。
// 本模块不自己建表、不自己建队列、不复制鉴权：
// 任务基础、文件服务、库权限、事务入口都用核心后端已有的实现。
//
// 每个接口都刻意做窄：只声明本模块真正调用的方法。窄接口的价值不在于
// 抽象漂亮，而在于「将来换成别的东西」时改动面是可见的——一个方法。

// 本模块需要的错误哨兵。
var (
	// ErrNotFound 表示对象不存在（或调用方无权看到它，两者对外同样处理）。
	ErrNotFound = errors.New("对象不存在")
	// ErrConflict 表示并发冲突（例如任务执行版本已过期、物品已被别人修改）。
	ErrConflict = errors.New("并发冲突")
	// ErrForbidden 表示权限不足。
	ErrForbidden = errors.New("权限不足")
	// ErrBadImage 表示图片无法作为识别输入（不存在、类型不支持、太大）。
	ErrBadImage = errors.New("图片不可用")
)

// SettingsStore 持久化 AI 提供者配置。
//
// 实现必须保证密钥密文与主密钥分开存放：密文可以进数据库或文件，
// 主密钥只在配置卷里。
type SettingsStore interface {
	Load(ctx context.Context) (Settings, error)
	Save(ctx context.Context, s Settings) error
}

// TaskCoordinator 是核心后端「持久任务基础」的窄接口。
//
// 核心后端负责队列、租约、并发上限、超时退避、重启回收与执行版本校验；
// 本模块只负责「领取一个任务 → 算出结果 → 写回结果」。这样 AI、标签、
// 导入导出三种任务共用同一套调度，不需要各自复制一份。
//
// 关键约定：Finish 必须携带领取时拿到的 ExecutionVersion，核心后端
// 在版本变化时返回 ErrConflict，从而阻止过期 worker 覆盖新结果。
type TaskCoordinator interface {
	// Register 静态注册处理器。只注册内置任务类型，
	// 不借此实现第三方代码加载（docs/接口契约约定.md 第 8 节）。
	Register(kind string, handler TaskHandler)
	// Enqueue 创建一个任务并返回任务标识。
	Enqueue(ctx context.Context, t Task) (string, error)
	// Leased 领取一个任务；返回 false 表示当前没有可领的任务。
	Leased(ctx context.Context, kind string) (Task, bool, error)
	// Heartbeat 续租，返回是否仍然持有租约。
	Heartbeat(ctx context.Context, taskID string, version int64) (bool, error)
	// Finish 写回结果。version 过期时返回 ErrConflict。
	Finish(ctx context.Context, taskID string, version int64, res *RecognizeResult, taskErr error) error
	// Get 读任务当前状态，用于接口轮询。
	Get(ctx context.Context, taskID string) (Task, error)
}

// TaskHandler 是一次任务执行。
//
// 处理器由本模块实现并静态注册；它在自己内部完成「发外部请求」与
// 「短事务写结果」两件事，外部请求绝不发生在持有库存事务期间
type TaskHandler func(ctx context.Context, t Task) (*RecognizeResult, error)

// CandidateStore 持久化候选。
type CandidateStore interface {
	// Insert 写入一批候选，返回带标识的候选。
	// 实现必须在一个事务内写完整批：半个批次的候选会让界面显示
	// 一个残缺的结果，而用户无从知道缺了什么。
	Insert(ctx context.Context, taskID string, cands []Candidate) ([]Candidate, error)
	// ListByTask 按任务列出候选（含序号排序）。
	ListByTask(ctx context.Context, taskID string) ([]Candidate, error)
	// ListBySpace 按库与处理状态列出候选，供待校对列表使用。
	ListBySpace(ctx context.Context, spaceID string, states []CandidateState, limit, offset int) ([]Candidate, int, error)
	// Get 读一条候选。
	Get(ctx context.Context, candidateID string) (Candidate, error)
	// Claim 原子地把候选从 pending 标记为 accepted，并记下物品与操作标识。
	//
	// 返回 ErrConflict 表示这条候选已经被确认过（并发点击或重试）——
	// 调用方据此返回已存在的结果，而不是再建一件物品。
	Claim(ctx context.Context, candidateID, operationID, itemID string) (Candidate, error)
	// Ignore 把候选标记为已忽略。
	Ignore(ctx context.Context, candidateID string) (Candidate, error)
	// CountByTask 统计某任务的候选数量，用于任务结果汇总。
	CountByTask(ctx context.Context, taskID string) (int, error)
}

// ImageLoader 读取已授权图片的字节。
//
// 实现方（核心文件服务）必须自己完成鉴权：本模块只交出图片标识。
// 返回的字节只在内存里存在一次请求的时间；不写日志、不落盘。
type ImageLoader interface {
	LoadForRecognition(ctx context.Context, actorID, spaceID, fileID string) (ImageBytes, error)
}

// TemplateHinter 提供「这个模板有哪些字段可以建议模型填写」。
//
// 只暴露 field_id / 展示名 / 类型：默认值、枚举全集、校验规则都不外发，
// 免得把业务规则泄漏给外部服务，也免得模型照抄默认值当成识别结果。
type TemplateHinter interface {
	// FieldHints 返回模板声明的字段说明。templateKey 为空时用默认模板。
	FieldHints(ctx context.Context, templateKey string) ([]FieldHint, error)
	// ApplyFields 按模板校验并归一模型给的值，返回可写入的值与字段结论。
	//
	// 人工订正过的值也走这条路径：这样「模型给的」与「人改的」经过
	// 完全相同的校验，不会出现「人工填的能过、模型填的被拒」这种不一致。
	ApplyFields(ctx context.Context, templateKey string, values map[string]any, manual map[string]any) (map[string]any, map[string]string, []string, []string, error)
	// ManualFieldIDs 返回模板里声明为「手动填写」的字段标识。
	ManualFieldIDs(ctx context.Context, templateKey string) ([]string, error)
	// DefaultQuantity 返回模板的默认数量（十进制字符串），用于初始库存。
	DefaultQuantity(ctx context.Context, templateKey string) (string, error)
}

// CreateItemRequest 是一次「在调用方事务内创建物品」的请求。
//
// 这是 D 与 B 之间最重要的一个接口：确认候选、导入确认都必须调用它，
// 而且必须能被包在调用方开启的事务里（docs/接口契约约定.md 第 8 节）。
type CreateItemRequest struct {
	// ActorID 是操作者。
	ActorID string
	// SpaceID 是目标库。
	SpaceID string
	// TemplateKey 是模板标识。
	TemplateKey string
	// FieldSchemaVersion 是要写进 custom_fields 的模板版本。
	FieldSchemaVersion int
	// Name 是物品名称。
	Name string
	// CategoryID / LocationID 是归属，可为空。
	CategoryID string
	LocationID string
	// Management 是管理方式（serial / quantity），由模板决定。
	Management string
	// Unit 是单位。
	Unit string
	// CustomFields 是已校验的扩展字段值（fields.values 的形态）。
	CustomFields map[string]any
	// Quantity 是初始库存数量（十进制字符串）。按件管理时只能是 "1"。
	Quantity string
	// PhotoIDs 要关联的图片（已授权、已在文件服务里）。
	PhotoIDs []string
	// OperationID 是幂等键，与 Idempotency-Key 同值。
	OperationID string
	// CandidateID 非空时，实现方要在同一事务里把它标记为已确认。
	CandidateID string
}

// CreatedItem 是创建结果。
type CreatedItem struct {
	ItemID string `json:"item_id"`
	// PublicCode 是分配到的公开码。
	PublicCode string `json:"public_code,omitempty"`
	// MovementID 是初始库存流水标识。
	MovementID string `json:"movement_id,omitempty"`
	// AlreadyExists 表示这次是幂等命中，返回的是既有物品。
	AlreadyExists bool `json:"already_exists,omitempty"`
}

// ItemCreator 是核心后端的物品创建入口。
//
// 契约（三条都必须满足，否则 AI 确认与导入确认会写出不一致的数据）：
//
//  1. CreateInTx 在调用方给出的事务里执行，绝不自己提交：候选标记为
//     已确认、物品创建、初始库存流水必须同生共死；
//  2. 余额只由核心库存服务改；本模块只提供数量意图；
//  3. 同一 OperationID 重复调用返回第一次的结果并置 AlreadyExists。
type ItemCreator interface {
	CreateInTx(ctx context.Context, tx Tx, req CreateItemRequest) (CreatedItem, error)
}

// Tx 是本模块能看到的事务句柄。
//
// 它是一个不透明接口而不是 *gorm.DB：业务集成模块不该依赖 ORM 类型，
// 核心后端也不该把 ORM 模型暴露成跨模块契约（docs/接口契约约定.md 第 8 节）。
type Tx interface {
	// ID 是事务标识，仅用于日志关联（不落业务表）。
	ID() string
}

// Transactor 提供「开一个事务并在里面调用多个入口」的能力。
//
// 它就是核心后端的 store.DB.Tx 的窄化封装：调用方把自己的逻辑作为
// 回调传入，核心后端保证回调内的写入同事务提交。
type Transactor interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Clock 可替换时间来源，便于测试与确定性用法。
type Clock func() time.Time
