package ai

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// 本文件定义 AI 模块的数据形状与对外错误。
//
// 设计上要区分三类东西，混在一起是最常见的返工原因：
//
//  1. 任务执行状态（TaskState）——排队/运行/成功/失败，由任务基础决定；
//  2. 候选处理状态（CandidateState）——待校对/已入库/已忽略，由使用者决定；
//  3. 识别结果本身（Candidate）——模型看到了什么。
//
// 一次「成功但没发现物品」的识别：任务状态是 succeeded，候选数为 0。
// 它不是失败，也不该永远停在「待校对」里。

// TaskKind 是任务类型标识。稳定字符串，接口与任务基础按它分发。
const TaskKindRecognize = "ai.recognize"

// TaskState 是任务执行状态。
type TaskState string

const (
	TaskQueued  TaskState = "queued"
	TaskRunning TaskState = "running"
	TaskSuccess TaskState = "succeeded"
	TaskFailed  TaskState = "failed"
)

// Terminal 报告任务是否已结束（成功或失败都不再被 worker 领取）。
func (s TaskState) Terminal() bool { return s == TaskSuccess || s == TaskFailed }

// CandidateState 是候选的处理状态，与任务状态正交。
type CandidateState string

const (
	CandidatePending  CandidateState = "pending"  // 待校对
	CandidateAccepted CandidateState = "accepted" // 已入库
	CandidateIgnored  CandidateState = "ignored"  // 已忽略
)

// Mode 是识别模式。
type Mode string

const (
	// ModeGroup 一件多图：同一实物的多张照片合成一个候选。
	ModeGroup Mode = "group"
	// ModeSplit 一图多件：每张照片可拆出多个候选。
	ModeSplit Mode = "split"
)

// Valid 报告模式是否是已知取值。
func (m Mode) Valid() bool { return m == ModeGroup || m == ModeSplit }

// Task 是一次识别任务。
//
// 不内嵌 core 的任务结构：任务基础由核心后端提供，本模块只按它的
// 领取/心跳/完成协议驱动自己的处理器，两边的结构保持独立，避免
// 「改一处要动两边」。
type Task struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	SpaceID string    `json:"space_id"`
	ActorID string    `json:"actor_id"`
	State   TaskState `json:"state"`
	// Attempt 是执行次数，用于退避与「放弃」判断。
	Attempt int `json:"attempt"`
	// ExecutionVersion 是本次执行的版本。写结果时必须带上它：
	// 过期 worker 的结果不能被接受。
	ExecutionVersion int64     `json:"execution_version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	// Input 是任务参数（模式、图片、模板等）。
	Input RecognizeInput `json:"input"`
	// Result 是成功后的汇总（候选数量），失败时为 nil。
	Result *RecognizeResult `json:"result,omitempty"`
	// Error 是失败原因（人类可读），成功时为 nil。
	Error string `json:"error,omitempty"`
}

// RecognizeInput 是一次识别的参数。
type RecognizeInput struct {
	Mode       Mode     `json:"mode"`
	ImageIDs   []string `json:"image_ids"`
	TemplateID string   `json:"template_id,omitempty"`
	// ObjectHint 是用户已经知道的信息（例如「这是万智牌」），
	// 用来提高准确率；不是指令，会被当作用户数据放进提示词。
	ObjectHint string `json:"object_hint,omitempty"`
	// CategoryID 是建议的目标分类；确认时可由用户改。
	CategoryID string `json:"category_id,omitempty"`
	// LocationID 是建议的目标位置。
	LocationID string `json:"location_id,omitempty"`
	// SpaceID 是目标库。
	SpaceID string `json:"space_id,omitempty"`
	// MaxCandidates 是单张图片最多接受的候选数，0 表示用默认值。
	MaxCandidates int `json:"max_candidates,omitempty"`
}

// Candidate 是一条识别候选。
//
// 一个 Candidate 恰好来自同一张图片（Group 模式下 SourceImageIDs 只有一项）；
// 「一图多件」时同一张图片会产出多条。这样「候选 → 来源图片」始终是明确的，
// 界面上可以并排显示原图与候选。
type Candidate struct {
	ID      string `json:"id"`
	TaskID  string `json:"task_id"`
	SpaceID string `json:"space_id"`
	// Index 是任务内的序号，从 1 开始，保证界面顺序稳定。
	Index int `json:"index"`
	// Revision 在同任务重跑时递增，避免旧结果被当成新的。
	Revision int64 `json:"revision"`

	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	// Name 与 Summary 是给用户看的两层信息：名字用于列表，摘要用于校对。
	// Notes 是可选的补充观察（铭牌文字、批号等）。
	Notes string `json:"notes,omitempty"`

	// Fields 建议的模板字段值（field_id → 值）。
	// 已经过模板校验与归一，界面可以直接填进表单。
	Fields map[string]any `json:"fields,omitempty"`
	// Applied 是字段结论：applied / applied_default / manual / rejected，
	// 让界面能说清「这一项为什么没填上」。
	Applied map[string]string `json:"applied,omitempty"`
	// ManualFields 是模板声明为手动填写的字段，模型给的猜测不会被采纳。
	ManualFields []string `json:"manual_fields,omitempty"`

	// SourceImageIDs 是这条候选的证据图片。
	SourceImageIDs []string `json:"source_image_ids"`
	// Evidence 是模型给出的判断依据（型号、铭牌、外观特征），供人工复核。
	Evidence string `json:"evidence,omitempty"`
	// Confidence 是模型自报的置信度，0..1；不参与自动决策，只影响排序与提示。
	Confidence float64 `json:"confidence,omitempty"`
	// Warnings 是校验时发现的问题（例如模型给的枚举值不认识）。
	Warnings []string `json:"warnings,omitempty"`

	State CandidateState `json:"state"`
	// ConfirmedItemID 是确认后创建的物品标识；已确认的候选永远保留这个引用。
	ConfirmedItemID string `json:"confirmed_item_id,omitempty"`
	// ConfirmedOperationID 是确认时的幂等键，用于「重复点击返回原物品」。
	ConfirmedOperationID string    `json:"confirmed_operation_id,omitempty"`
	ConfirmedAt          time.Time `json:"confirmed_at,omitempty"`
	IgnoredAt            time.Time `json:"ignored_at,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// RecognizeResult 是任务成功后的汇总。
type RecognizeResult struct {
	CandidateCount int `json:"candidate_count"`
	// Empty 表示这是一次「成功但没发现物品」的识别。
	// 单独一个布尔而不是靠 count==0 判断：将来解析出一堆被拒的候选时，
	// 「模型什么都没看到」与「模型看到了但我们全拒了」是两件事。
	Empty bool `json:"empty"`
	// Provider 与 Model 记录这次识别用的模型，便于回溯与计费核对。
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// ElapsedMS 是单次识别耗时。
	ElapsedMS int64 `json:"elapsed_ms,omitempty"`
	// Rejected 是被模板校验拒掉的候选数量。
	Rejected int `json:"rejected,omitempty"`
}

// ConfirmInput 是确认一条候选所需的全部信息。
type ConfirmInput struct {
	// ActorID 是操作者，用于派生物品所有权与权限复核。
	ActorID string
	// OperationID 是幂等键（与 Idempotency-Key 同值），保证重复点击不建第二件。
	OperationID string
	// CandidateID 是要确认的候选。
	CandidateID string
	// SpaceID 是目标库。
	SpaceID string
	// TemplateKey 是目标模板；为空时用任务输入里的模板。
	TemplateKey string
	// Name 允许人工改名义后入库；为空用候选名称。
	Name string
	// CategoryID / LocationID 是人工确认后的归属，覆盖候选建议。
	CategoryID string
	LocationID string
	// Fields 是人工订正后的字段值（覆盖候选建议）。
	Fields map[string]any
	// Quantity 是初始库存数量（十进制字符串），为空按模板默认。
	Quantity string
	// PhotoIDs 是要关联的图片。PhotosSet 区分「请求明确给了数组」与「没给/
	// 给了 null」：前者以 PhotoIDs 为准（空数组 = 不关联图片），后者才回落到
	// 来源图片共享语义（受 SharePhotos 约束）。
	PhotoIDs []string
	// PhotosSet 表示请求体里出现了 photo_ids 数组（含空数组）。
	PhotosSet bool
	// SharePhotos 是 share_photos 的三态：nil=默认共享，false=明确不共享。
	SharePhotos *bool
}

// ConfirmResult 是确认结果。
type ConfirmResult struct {
	CandidateID   string `json:"candidate_id"`
	ItemID        string `json:"item_id"`
	PublicCode    string `json:"public_code,omitempty"`
	OperationID   string `json:"operation_id"`
	AlreadyExists bool   `json:"already_exists"`
	// MovementID 是初始库存流水，便于界面跳转。
	MovementID string `json:"movement_id,omitempty"`
}

// Settings 是 AI 提供者配置。
//
// APIKey 是明文形式，只在进程内流动：写库前必须加密（见 cipher.go），
// 从接口读出来时必须是脱敏形式（见 Masked）。
type Settings struct {
	// Provider 是提供者标识，首版内置 "openai-compatible" 与 "mock"。
	Provider string `json:"provider"`
	// BaseURL 是接口地址，例如 https://api.example.com/v1。
	BaseURL string `json:"base_url"`
	// APIKey 是密钥明文（仅内部使用）。
	APIKey string `json:"api_key,omitempty"`
	// Model 是模型标识。
	Model string `json:"model"`
	// TimeoutSeconds 是单次请求超时，默认 60。
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// MaxImages 是单次任务最多发几张图，默认 8。
	MaxImages int `json:"max_images,omitempty"`
	// Enabled 为 false 时识别接口直接返回 503，不发外部请求。
	Enabled bool `json:"enabled"`
	// UpdatedAt 是最后修改时间。
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// needsRekey 表示「库里存着的密钥解不开，需要重新填一次」。
	// 非导出：它是存储层的一次读取结果，不是配置的一部分，
	// 也不该被序列化到任何地方。
	needsRekey bool
}

// Masked 返回用于接口响应的脱敏副本。
//
// 只回显「是否设置过」与末四位：日志与响应里出现完整密钥是最常见的
// 泄漏途径，而调用方从来不需要知道原值。
func (s Settings) Masked() MaskedSettings {
	return MaskedSettings{
		Provider:       s.Provider,
		BaseURL:        s.BaseURL,
		Model:          s.Model,
		TimeoutSeconds: s.TimeoutSeconds,
		MaxImages:      s.MaxImages,
		Enabled:        s.Enabled,
		APIKeySet:      s.APIKey != "",
		APIKeyHint:     hint(s.APIKey),
		UpdatedAt:      s.UpdatedAt,
	}
}

func hint(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

// MaskedSettings 是脱敏后的提供者配置。
type MaskedSettings struct {
	Provider       string    `json:"provider"`
	BaseURL        string    `json:"base_url"`
	Model          string    `json:"model"`
	TimeoutSeconds int       `json:"timeout_seconds"`
	MaxImages      int       `json:"max_images"`
	Enabled        bool      `json:"enabled"`
	APIKeySet      bool      `json:"api_key_set"`
	APIKeyHint     string    `json:"api_key_hint,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
	// RekeyNeeded 表示存着的密钥解不开（主密钥换过），需要在设置页重填。
	RekeyNeeded bool `json:"rekey_needed,omitempty"`
}

// TestResult 是连接测试结果。
type TestResult struct {
	OK bool `json:"ok"`
	// Message 是给使用者看的结论，中文，不含密钥。
	Message string `json:"message"`
	// Model 是实际响应的模型标识，可能与配置不同（服务端会改写）。
	Model string `json:"model,omitempty"`
	// SawImage 表示这次识别确实看到了样例图的内容。
	// 「接口通了但没看图」与「真的看图了」必须能分辨：
	// 兼容接口不代表模型支持图片。
	SawImage bool `json:"saw_image"`
	// Candidates 是样例图识别出的候选数量。
	Candidates int `json:"candidates"`
	// ElapsedMS 是耗时。
	ElapsedMS int64 `json:"elapsed_ms,omitempty"`
	// Detail 是失败细节（错误类型、HTTP 状态），不含请求体。
	Detail string `json:"detail,omitempty"`
}

// RawObservation 是提供者返回的一条原始观察。
//
// 这是「模型输出」与「系统数据」之间的边界类型：它只有字符串，
// 没有标识符、没有枚举、没有命令。所有后续处理都必须先按模板校验，
// 绝不能把这里的任何字段当作控制流：
// 模型返回内容不能作为程序指令执行。
type RawObservation struct {
	Name       string `json:"name"`
	Summary    string `json:"summary"`
	Notes      string `json:"notes"`
	Evidence   string `json:"evidence"`
	Confidence any    `json:"confidence"`
	// Fields 是模型猜测的字段值，键是 field_id。
	Fields map[string]any `json:"fields"`
}

// Recognizer 是识别提供者的窄接口。
//
// 只有两个方法，因为这是「一次识别」真正需要的全部：把图片和期望的
// 字段清单发出去，拿回一组原始观察。不暴露数据库句柄、不暴露配置存储、
// 不暴露任务状态——提供者实现（内置客户端或将来的第三方）无法影响
// 任务流程与库存。
type Recognizer interface {
	// Name 是提供者标识，出现在设置与任务结果里。
	Name() string
	// Recognize 执行一次识别。实现必须：
	//   - 不修改入参；
	//   - 返回 ErrRateLimited / ErrTimeout / ErrProviderFailed 之一作为可分类错误；
	//   - 不把模型返回的自然语言当作指令解析。
	Recognize(ctx context.Context, req RecognizeRequest) (RecognizeResponse, error)
}

// RecognizeRequest 是一次识别请求。
type RecognizeRequest struct {
	// Mode 是识别模式。提供者据此决定提示词，但不得改变返回结构。
	Mode Mode
	// Images 是已授权的图片字节。
	Images []ImageBytes
	// ObjectHint 是用户提供的线索。
	ObjectHint string
	// FieldHints 是期望模型填写的字段清单（field_id + 展示名 + 类型）。
	// 只发 field_id 与展示名与类型名；不发默认值、不发枚举全集，
	// 避免把业务规则泄漏给外部服务，也避免模型照抄默认值。
	FieldHints []FieldHint
	// MaxCandidates 是数量上限。
	MaxCandidates int
}

// ImageBytes 是一张图片的内容。
//
// 只在内存里传递；不落盘（原图已由文件服务保管，这里只是借用一次）。
type ImageBytes struct {
	FileID      string
	ContentType string
	Data        []byte
}

// FieldHint 是发给模型的字段说明。
type FieldHint struct {
	FieldID string `json:"field_id"`
	Label   string `json:"label"`
	Type    string `json:"type"`
}

// RecognizeResponse 是一次识别的原始结果。
type RecognizeResponse struct {
	Observations []RawObservation `json:"observations"`
	// Model 是实际使用的模型标识。
	Model string `json:"model,omitempty"`
	// Raw 是原始响应体，仅在失败排查时短暂保留（不落库、不记日志）。
	Raw json.RawMessage `json:"-"`
}

// 可分类错误。调用方据此决定「重试 / 退避 / 直接告诉用户」。
var (
	// ErrRateLimited 表示提供者限流。任务应当按 Retry-After 退避重试。
	ErrRateLimited = errors.New("AI 提供者限流")
	// ErrTimeout 表示请求超时。可重试，但不应立即重试。
	ErrTimeout = errors.New("AI 请求超时")
	// ErrProviderFailed 表示提供者返回了错误或不可解析的响应。
	ErrProviderFailed = errors.New("AI 提供者调用失败")
	// ErrDisabled 表示 AI 识别在设置里被关闭。
	ErrDisabled = errors.New("AI 识别未启用")
	// ErrNotConfigured 表示还没有配置提供者。
	ErrNotConfigured = errors.New("尚未配置 AI 提供者")
	// ErrEmptyResult 不是错误：它表示一次成功但没发现物品的识别。
	ErrEmptyResult = errors.New("识别完成，未发现物品")
)

// RetryAfter 从错误里提取提供者建议的等待时长。
func RetryAfter(err error) (time.Duration, bool) {
	var rl *RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return rl.RetryAfter, true
	}
	return 0, false
}

// RateLimitError 是带等待时长的限流错误。
type RateLimitError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *RateLimitError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return ErrRateLimited.Error()
}
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }
