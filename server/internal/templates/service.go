package templates

import (
	"context"
	"sync"
)

// 本文件是模板模块对外的服务入口。
//
// 为什么单独一层：HTTP 处理器只做「解析请求 / 组装响应」，字段合并这类语义
// 必须能被别的包直接调用——物品创建与更新发生在 item 模块里，它需要
// 「按模板校验并合并 custom_fields」这个动作，但不该为此走一次 HTTP。
// 服务函数一律用 context.Context 而不是 *gin.Context，与核心模块的服务层一致。

var (
	catalogOnce sync.Once
	catalogVal  *Catalog
	catalogErr  error
)

// Builtin 返回进程内唯一的内置模板目录。
//
// 模板是启动期资产（go:embed），不需要每次请求重新读盘；失败结果也会被缓存，
// 避免每次请求都重复报同一个错。目录只读，多请求并发读是安全的。
func Builtin() (*Catalog, error) {
	catalogOnce.Do(func() {
		catalogVal, catalogErr = LoadBuiltin()
	})
	return catalogVal, catalogErr
}

// Service 是模板相关的领域服务。
//
// 不持有数据库句柄：内置模板来自嵌入文件，本服务不写库。将来插件模板落库时，
// 在这里加一个 store 字段即可，调用点的签名不用改。
type Service struct{}

// NewService 构造服务。
func NewService() *Service { return &Service{} }

// MergeFields 按模板合并一次字段提交。
//
// 这是 item 模块唯一需要的入口：给它「当前模板、已有值、本次提交」，
// 它返回「要写入的值 + 被拒绝的字段 + 未知字段」。调用方据此决定是否整单失败：
// Strict 为 true 时只要有 REJECTED 就应当返回 VALIDATION_FAILED。
func (s *Service) MergeFields(ctx context.Context, templateKey string, existing CustomFields, input map[string]any, opts MergeOptions) (Manifest, Resolution, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, Resolution{}, err
	}
	cat, err := Builtin()
	if err != nil {
		return Manifest{}, Resolution{}, err
	}
	m, err := cat.ValidateTemplateKey(templateKey)
	if err != nil {
		return Manifest{}, Resolution{}, err
	}
	return m, Merge(m, existing, input, opts), nil
}

// ApplyTemplateSwitch 处理「换模板」：保留原值，按新模板补预设与默认值。
//
// 换模板不删资料是硬约定：已有的未知字段原值保留，
// 新模板声明的字段若原来没有值则补默认/预设值。返回的 Resolution 里
// Decisions 会带 STALE_SCHEMA / APPLIED_PRESET 等结论，供界面解释。
func (s *Service) ApplyTemplateSwitch(ctx context.Context, templateKey string, existing CustomFields) (Manifest, Resolution, error) {
	cat, err := Builtin()
	if err != nil {
		return Manifest{}, Resolution{}, err
	}
	m, err := cat.ValidateTemplateKey(templateKey)
	if err != nil {
		return Manifest{}, Resolution{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, Resolution{}, err
	}
	// input 为空表示「没有新提交」，只做补齐与版本推进。
	return m, Merge(m, existing, nil, MergeOptions{Preset: &m.Preset}), nil
}

// ReadonlyView 组装「只读展示」需要的信息：已保存的值、字段显示名、未知字段。
//
// 模板被换掉、插件被停用或未安装时，普通资料页仍要能只读展示已保存的名称与值
// 。未知字段的显示名用标识本身兜底——
// 宁可显示一个内部标识，也不能把数据藏起来。
func (s *Service) ReadonlyView(ctx context.Context, templateKey string, existing CustomFields) (map[string]any, map[string]string, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	cat, err := Builtin()
	if err != nil {
		return nil, nil, nil, err
	}
	m, err := cat.ValidateTemplateKey(templateKey)
	if err != nil {
		return nil, nil, nil, err
	}
	labels := map[string]string{}
	declared := map[string]bool{}
	for _, f := range m.Fields {
		labels[f.FieldID] = f.Label
		declared[f.FieldID] = true
	}
	unknown := existing.UnknownFieldIDs(declared)
	for _, id := range unknown {
		labels[id] = id
	}
	return existing.Clone().Values, labels, unknown, nil
}
