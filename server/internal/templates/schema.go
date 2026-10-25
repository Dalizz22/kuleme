// Package templates 实现库模板的最小 manifest：字段类型、默认值、管理方式、
// 列、筛选与共用操作标识。
//
// 设计约束：
//
//   - 模板只声明数据，不是代码。服务端只做白名单校验，不解析、不执行任何
//     表达式、SQL 或脚本；manifest 里出现未知字段一律拒绝加载。
//   - 稳定标识优先：template_key 与 field_id 一旦发布不再改变语义；插件字段
//     必须带命名空间，不能占用内置标识。
//   - 字段定义与字段值分开存放：定义随模板版本走，值存在 Item.custom_fields
//     (JSONB)，并记录 field_schema_version。换模板只补默认值，不删除原资料。
package templates

import (
	"bytes"
	"encoding/json"
)

// 命名空间分隔符：插件字段形如 plugin_id:field_key，内置字段不含冒号。
const FieldNamespaceSep = ":"

// SchemaVersion 是本包认识的 manifest 格式版本。升级格式时新增常量，
// 不修改旧常量含义；未知版本一律拒绝加载，避免半解析。
const SchemaVersion = 1

// Group 是模板分组。目前只有核心内置模板，插件模板为后续预留。
type Group string

const (
	// GroupCore 是 owner 为 core 的内置模板，标识不可被后续模板占用。
	GroupCore Group = "core"
	// GroupPlugin 留给未来的第三方模板：owner 必须是 plugin_id，
	// 字段标识必须使用 plugin_id:field_key 命名空间。
	GroupPlugin Group = "plugin"
)

// Management 是物品的管理方式：按件或按量。它决定库存语义，
// 因此模板只能提供默认值，不能改变已有流水的物品。
type Management string

const (
	ManagementSerial   Management = "serial"
	ManagementQuantity Management = "quantity"
)

// FieldType 是受支持的字段类型白名单。
//
// 没有「表达式」「公式」「正则校验」类型：模板不执行任意逻辑。
// text 与 textarea 只影响录入控件，不影响校验规则。
type FieldType string

const (
	TypeText     FieldType = "text"
	TypeTextarea FieldType = "textarea"
	TypeNumber   FieldType = "number"
	TypeMoney    FieldType = "money"
	TypeDate     FieldType = "date"
	TypeBool     FieldType = "bool"
	TypeEnum     FieldType = "enum"
)

// NumberUnit 说明数值字段的计量语义。unit 必须与物品单位一致比对，
// 避免把「件」当成「个」直接相加。
type NumberUnit string

const (
	UnitNone     NumberUnit = ""
	UnitItem     NumberUnit = "item"     // 按物品单位（件/个/台），按件管理时恒为 1
	UnitCurrency NumberUnit = "currency" // 实例级币种
	UnitPercent  NumberUnit = "percent"
)

// Field 是一个受支持的类型化字段定义。
//
// 稳定性保证：FieldID 是永久标识，改名只改 Label；Type 一旦发布不改变，
// 需要不同语义时新增字段而不是就地改类型。
type Field struct {
	FieldID  string     `json:"field_id"`
	Label    string     `json:"label"`
	Type     FieldType  `json:"type"`
	Owner    string     `json:"owner"`
	Required bool       `json:"required,omitempty"`
	Default  any        `json:"default,omitempty"`
	Options  []string   `json:"options,omitempty"`
	Unit     NumberUnit `json:"unit,omitempty"`
	// Precision 是数值/金额的小数位，缺省表示不限；库存数量另有单位精度校验。
	Precision *int `json:"precision,omitempty"`
	// MaxLen 是文本长度上限，0 表示不限。
	MaxLen int `json:"max_len,omitempty"`
	// Help 是录入提示，纯展示，不参与校验。
	Help string `json:"help,omitempty"`
	// Readonly 表示该字段由核心服务计算写入（如按位置汇总的余量），
	// 不接受客户端提交；只读字段的值仍然照常保存与展示。
	Readonly bool `json:"readonly,omitempty"`
}

// Column 声明列表里展示哪些字段。列只影响展示：隐藏一列不会删除数据。
type Column struct {
	FieldID string `json:"field_id"`
	Label   string `json:"label,omitempty"`
	// Width 是桌面端建议列宽（像素），手机端按可用宽度重新分配。
	Width int `json:"width,omitempty"`
	// Hidden 为 true 表示默认折叠，用户可展开。
	Hidden bool `json:"hidden,omitempty"`
}

// Filter 声明一种筛选视图。值全部来自白名单标识，服务端把它们翻译成
// 固定的查询条件，模板自身不能提供 SQL 片段。
type Filter struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// FieldID 是筛选依据的字段；为空表示依据核心字段而不是扩展字段。
	FieldID string `json:"field_id,omitempty"`
	// Op 是受支持的操作符：eq、ne、contains、gt、lt、between、empty、not_empty。
	Op string `json:"op"`
	// Value 是固定比较值；between 用长度为 2 的数组。
	//
	// 允许出现的占位符只有两个，由服务端按查询时刻替换：
	// __today__ 表示当天，__today+<n>d__ 表示当天加 n 天（n 为正整数）。
	// 除这两个占位符外一律按字面值比较，不接受任何可执行内容。
	Value any `json:"value,omitempty"`
	// CoreField 指向核心列（如 management、unit），仅当 FieldID 为空时有效。
	CoreField string `json:"core_field,omitempty"`
	// CompareWith 是 lte_field 的另一个扩展字段标识。
	CompareWith string `json:"compare_with,omitempty"`
}

// Operation 是共用操作标识。模板只能引用已存在的操作，不能定义新操作，
// 也不能声明副作用：真正执行时一律走核心业务服务。
type Operation struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Preset 是模板带来的默认资料。换模板时只补空缺，不覆盖已有值，
// 因此这里的每个键都必须同时出现在 Fields 中。
type Preset struct {
	Category string         `json:"category,omitempty"`
	Tags     []string       `json:"tags,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
}

// Manifest 是一个模板的完整声明。
type Manifest struct {
	SchemaVersion int        `json:"schema_version"`
	TemplateKey   string     `json:"template_key"`
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	Group         Group      `json:"group"`
	Owner         string     `json:"owner"`
	Management    Management `json:"management"`
	Unit          string     `json:"unit,omitempty"`
	// IsDefault 标记新物品的默认模板；全局最多一个。
	IsDefault bool `json:"is_default,omitempty"`
	// Sort 决定模板在设置页的展示顺序。
	Sort       int         `json:"sort,omitempty"`
	Fields     []Field     `json:"fields"`
	Columns    []Column    `json:"columns"`
	Filters    []Filter    `json:"filters"`
	Operations []Operation `json:"operations"`
	Preset     Preset      `json:"preset,omitempty"`
}

// FieldByID 返回指定字段定义。
func (m Manifest) FieldByID(fieldID string) (Field, bool) {
	for _, f := range m.Fields {
		if f.FieldID == fieldID {
			return f, true
		}
	}
	return Field{}, false
}

// FieldIDs 返回本模板声明的全部字段标识，保持声明顺序。
func (m Manifest) FieldIDs() []string {
	out := make([]string, 0, len(m.Fields))
	for _, f := range m.Fields {
		out = append(out, f.FieldID)
	}
	return out
}

// coreColumnSet 是允许出现在 Filter.CoreField 的核心列白名单。
//
// 白名单是一道边界：模板不能借 core_field 触达任意数据库列。
// 这里同时收录两类列：
//
//   - 存放在 item 上的核心列（name、note、management、unit…）；
//   - 由核心服务按位置关系或媒体表汇总出来的展示列（location_path 是位置
//     全路径，photo / category 在此语境下指封面图与分类路径）。
//
// 第二类只允许作为列与筛选依据出现，不允许作为扩展字段标识（见 reservedFieldID）。
// 它们不落库到 item 上，因此模板引用它们不需要、也不能声明对应 Field。
var coreColumnSet = map[string]bool{
	"name":         true,
	"note":         true,
	"management":   true,
	"unit":         true,
	"template_key": true,
	"category_id":  true,
	"location_id":  true,
	"space_id":     true,
	"public_code":  true,
	"created_at":   true,
	"updated_at":   true,
	"custom_field": true, // 值取自 field_id 指定的扩展字段
	// 派生展示列。
	"location_path": true,
	"category":      true,
	"photo":         true,
}

// coreOperationSet 是模板可以引用的全部共用操作标识。
//
// 模板只能引用已存在的操作，不能定义新操作，也不能声明副作用：标识里的动作
// （领用、借出、移库…）在核心业务服务里有唯一实现，模板只是决定「这个模板的
// 列表页显示哪些动作按钮」。因此这里是一张白名单，不在表里的标识一律拒绝。
//
// 标识用 core: 前缀：一是与未来的插件自定义动作区分开，二是让「这是核心动作」
// 在日志和审计里一眼可见。
var coreOperationSet = map[string]bool{
	"core:view":            true,
	"core:edit":            true,
	"core:move":            true,
	"core:add_photo":       true,
	"core:print_label":     true,
	"core:export":          true,
	"core:adjust_quantity": true, // 仅按量管理的模板可用
}

// filterOps 是受支持的筛选操作符白名单。
var filterOps = map[string]bool{
	"eq": true, "ne": true, "contains": true,
	"gt": true, "lt": true, "between": true,
	"empty": true, "not_empty": true,
	// lte_field 比较两个扩展字段：字段值 ≤ 同一条记录的另一个字段值
	// （例如「余量 ≤ 最低库存」）。另一个字段用 CompareWith 指定。
	"lte_field": true,
}

// fieldTypeSet 是受支持的字段类型白名单。
var fieldTypeSet = map[FieldType]bool{
	TypeText: true, TypeTextarea: true, TypeNumber: true,
	TypeMoney: true, TypeDate: true, TypeBool: true, TypeEnum: true,
}

// numericTypes 需要 unit 与 precision 校验的类型。
var numericTypes = map[FieldType]bool{TypeNumber: true, TypeMoney: true}

// decodeManifest 严格解析 manifest：未知字段直接报错，避免拼写错误
// 被静默忽略后表现为「模板没生效」。
func decodeManifest(raw []byte) (Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
