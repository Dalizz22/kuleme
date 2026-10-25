package templates

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Issue 是一条模板结构或字段值的问题。
//
// FieldID 为空表示问题在 manifest 整体上；Row 与 Column 供导入预览指出
// 「第几行第几列」。
type Issue struct {
	FieldID string `json:"field_id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Row     int    `json:"row,omitempty"`
	Column  string `json:"column,omitempty"`
}

// 问题码。稳定字符串，供前端做本地化与定位，不用中文比对。
const (
	IssueUnsupportedVersion = "UNSUPPORTED_SCHEMA_VERSION"
	IssueMissingKey         = "MISSING_TEMPLATE_KEY"
	IssueBadKey             = "INVALID_TEMPLATE_KEY"
	IssueUnknownGroup       = "UNKNOWN_GROUP"
	IssueOwnerMismatch      = "OWNER_NAMESPACE_MISMATCH"
	IssueFieldIDNamespace   = "FIELD_ID_NAMESPACE_MISMATCH"
	IssueReservedFieldID    = "RESERVED_FIELD_ID"
	IssueDuplicateFieldID   = "DUPLICATE_FIELD_ID"
	IssueUnknownType        = "UNKNOWN_FIELD_TYPE"
	IssueMissingLabel       = "MISSING_LABEL"
	IssueBadDefault         = "INVALID_DEFAULT"
	IssueEnumNeedsOptions   = "ENUM_WITHOUT_OPTIONS"
	IssueOptionsOnNonEnum   = "OPTIONS_ON_NON_ENUM"
	IssueBadUnit            = "INVALID_UNIT"
	IssueBadPrecision       = "INVALID_PRECISION"
	IssueUnknownColumn      = "COLUMN_FIELD_NOT_DECLARED"
	IssueUnknownFilterField = "FILTER_FIELD_NOT_DECLARED"
	IssueUnknownCoreColumn  = "FILTER_CORE_COLUMN_NOT_ALLOWED"
	IssueUnknownOp          = "UNKNOWN_FILTER_OPERATOR"
	IssueBadFilterValue     = "INVALID_FILTER_VALUE"
	IssueFilterNeedsTarget  = "FILTER_WITHOUT_TARGET"
	IssuePresetUnknownField = "PRESET_FIELD_NOT_DECLARED"
	IssueDuplicateKey       = "DUPLICATE_COLLECTION_KEY"
	IssueUnknownOperation   = "UNKNOWN_OPERATION"
)

// Validate 校验一个模板 manifest 的结构。
//
// 只做结构与本包常量表的比对，不做网络、数据库或表达式求值。
// 返回全部问题而不是遇到第一个就返回：模板作者一次能看到所有错误。
func Validate(m Manifest) []Issue {
	var issues []Issue
	add := func(fieldID, code, format string, args ...any) {
		issues = append(issues, Issue{
			FieldID: fieldID,
			Code:    code,
			Message: fmt.Sprintf(format, args...),
		})
	}

	if m.SchemaVersion != SchemaVersion {
		add("", IssueUnsupportedVersion,
			"schema_version 必须是 %d，实际 %d", SchemaVersion, m.SchemaVersion)
	}
	if m.TemplateKey == "" {
		add("", IssueMissingKey, "template_key 不能为空")
	} else if !validSlug(m.TemplateKey) {
		add("", IssueBadKey,
			"template_key %q 只能包含小写字母、数字和下划线", m.TemplateKey)
	}
	if strings.TrimSpace(m.Name) == "" {
		add("", IssueMissingLabel, "模板 name 不能为空")
	}

	switch m.Group {
	case GroupCore:
		if m.Owner != string(GroupCore) {
			add("", IssueOwnerMismatch,
				"内置模板 owner 必须是 core，实际 %q", m.Owner)
		}
		// plugin_ 前缀是插件模板的保留令牌，core 模板占了会让插件无处安名。
		if strings.HasPrefix(m.TemplateKey, "plugin_") {
			add("", IssueBadKey, "template_key %q 占用了插件保留前缀 plugin_", m.TemplateKey)
		}
	case GroupPlugin:
		if m.Owner == "" || m.Owner == string(GroupCore) {
			add("", IssueOwnerMismatch,
				"插件模板必须声明自己的 owner，不能用 core")
		} else if !validPluginID(m.Owner) {
			add("", IssueOwnerMismatch,
				"owner %q 必须是 com.example.name 形式的插件标识", m.Owner)
		}
	default:
		add("", IssueUnknownGroup, "group 必须是 core 或 plugin，实际 %q", m.Group)
	}

	switch m.Management {
	case ManagementSerial, ManagementQuantity:
	default:
		add("", IssueUnknownType, "management 必须是 serial 或 quantity，实际 %q", m.Management)
	}

	seen := map[string]bool{}
	names := make([]string, 0, len(m.Fields))
	for _, f := range m.Fields {
		if f.FieldID == "" {
			add("", IssueMissingKey, "字段 field_id 不能为空")
			continue
		}
		if seen[f.FieldID] {
			add(f.FieldID, IssueDuplicateFieldID, "字段 %s 重复声明", f.FieldID)
			continue
		}
		seen[f.FieldID] = true
		names = append(names, f.FieldID)

		owner, key, hasNS := splitFieldID(f.FieldID)
		switch {
		case hasNS:
			if !validSlug(key) {
				add(f.FieldID, IssueBadKey, "字段名 %q 只能包含小写字母、数字和下划线", key)
			}
			if owner != m.Owner {
				add(f.FieldID, IssueFieldIDNamespace, "字段 %s 的命名空间必须是本模板 owner %q",
					f.FieldID, m.Owner)
			}
		case m.Group == GroupPlugin:
			add(f.FieldID, IssueFieldIDNamespace,
				"插件模板字段 %s 必须带命名空间，形如 %s:field_key", f.FieldID, m.Owner)
		case reservedFieldID(f.FieldID):
			add(f.FieldID, IssueReservedFieldID, "字段 %s 属于核心业务列，不能作为扩展字段", f.FieldID)
		case !validSlug(f.FieldID):
			// 核心模板的无命名空间字段此前没有任何字符集校验，"f 1" 这类
			// 标识会一路漏到查询层才炸。
			add(f.FieldID, IssueBadKey, "字段标识 %q 只能包含小写字母、数字和下划线", f.FieldID)
		}

		if f.Owner == "" {
			add(f.FieldID, IssueOwnerMismatch, "字段 %s 必须声明 owner", f.FieldID)
		} else if hasNS && f.Owner != owner {
			add(f.FieldID, IssueOwnerMismatch, "字段 %s 的 owner 必须是 %q，实际 %q",
				f.FieldID, owner, f.Owner)
		} else if !hasNS && f.Owner != m.Owner {
			add(f.FieldID, IssueOwnerMismatch, "字段 %s 的 owner 必须是 %q，实际 %q",
				f.FieldID, m.Owner, f.Owner)
		}

		if strings.TrimSpace(f.Label) == "" {
			add(f.FieldID, IssueMissingLabel, "字段 %s 缺少 label", f.FieldID)
		}
		if !fieldTypeSet[f.Type] {
			add(f.FieldID, IssueUnknownType, "字段 %s 的类型 %q 不受支持", f.FieldID, f.Type)
		}
		if f.Type == TypeEnum {
			if len(f.Options) == 0 {
				add(f.FieldID, IssueEnumNeedsOptions, "枚举字段 %s 必须声明 options", f.FieldID)
			} else if dup := duplicateString(f.Options); dup != "" {
				add(f.FieldID, IssueDuplicateKey, "枚举字段 %s 的选项 %q 重复", f.FieldID, dup)
			}
		} else if len(f.Options) > 0 {
			add(f.FieldID, IssueOptionsOnNonEnum, "字段 %s 不是枚举类型，不能声明 options", f.FieldID)
		}
		if numericTypes[f.Type] && f.Unit == "" {
			add(f.FieldID, IssueBadUnit, "数值字段 %s 必须声明 unit", f.FieldID)
		}
		if !numericTypes[f.Type] && f.Unit != "" {
			add(f.FieldID, IssueBadUnit, "非数值字段 %s 不能声明 unit", f.FieldID)
		}
		switch f.Unit {
		case UnitNone, UnitItem, UnitCurrency, UnitPercent:
		default:
			add(f.FieldID, IssueBadUnit, "字段 %s 的 unit %q 不受支持", f.FieldID, f.Unit)
		}
		if f.Precision != nil && (*f.Precision < 0 || *f.Precision > 6) {
			add(f.FieldID, IssueBadPrecision, "字段 %s 的 precision 必须在 0..6", f.FieldID)
		}
		if f.Precision != nil && !numericTypes[f.Type] {
			add(f.FieldID, IssueBadPrecision, "非数值字段 %s 不能声明 precision", f.FieldID)
		}
		if f.MaxLen < 0 {
			add(f.FieldID, IssueBadPrecision, "字段 %s 的 max_len 不能为负", f.FieldID)
		}
		if f.Default != nil {
			if _, err := normalizeValue(f, f.Default); err != nil {
				add(f.FieldID, IssueBadDefault, "字段 %s 的默认值不合法：%v", f.FieldID, err)
			}
		}
	}

	// 列
	colSeen := map[string]bool{}
	for _, c := range m.Columns {
		if c.FieldID == "" {
			add("", IssueMissingKey, "列必须声明 field_id")
			continue
		}
		if colSeen[c.FieldID] {
			add(c.FieldID, IssueDuplicateKey, "列 %s 重复声明", c.FieldID)
			continue
		}
		colSeen[c.FieldID] = true
		if !coreColumnSet[c.FieldID] && !seen[c.FieldID] {
			add(c.FieldID, IssueUnknownColumn,
				"列 %s 既不是核心列也不是本模板声明的字段", c.FieldID)
		}
		if c.Width < 0 {
			add(c.FieldID, IssueBadPrecision, "列 %s 的 width 不能为负", c.FieldID)
		}
	}

	// 筛选
	filterSeen := map[string]bool{}
	for _, f := range m.Filters {
		if f.Key == "" {
			add("", IssueMissingKey, "筛选必须声明 key")
			continue
		}
		if filterSeen[f.Key] {
			add("", IssueDuplicateKey, "筛选 %s 重复声明", f.Key)
			continue
		}
		filterSeen[f.Key] = true
		if strings.TrimSpace(f.Label) == "" {
			add(f.FieldID, IssueMissingLabel, "筛选 %s 缺少 label", f.Key)
		}
		if !filterOps[f.Op] {
			add(f.FieldID, IssueUnknownOp, "筛选 %s 的操作符 %q 不受支持", f.Key, f.Op)
		}
		if f.FieldID == "" && f.CoreField == "" {
			add("", IssueFilterNeedsTarget, "筛选 %s 必须指定 field_id 或 core_field", f.Key)
			continue
		}
		if f.FieldID != "" && f.CoreField != "" {
			add(f.FieldID, IssueFilterNeedsTarget,
				"筛选 %s 只能指定 field_id 或 core_field 之一", f.Key)
		}
		if f.CoreField != "" && !coreColumnSet[f.CoreField] {
			add("", IssueUnknownCoreColumn,
				"筛选 %s 的核心列 %q 不在允许范围内", f.Key, f.CoreField)
		}
		var target Field
		var hasTarget bool
		if f.FieldID != "" {
			target, hasTarget = m.FieldByID(f.FieldID)
			if !hasTarget {
				add(f.FieldID, IssueUnknownFilterField,
					"筛选 %s 引用了未声明的字段 %s", f.Key, f.FieldID)
				continue
			}
		}
		if err := validateFilterValue(f, target, hasTarget); err != nil {
			add(f.FieldID, IssueBadFilterValue, "筛选 %s 的值不合法：%v", f.Key, err)
		}
		if f.Op == "lte_field" && !hasTarget {
			// 目标字段未声明时上面已经记过 ISSUE_UNKNOWN_FILTER_FIELD，
			// 这里不再重复报错。
		} else if f.Op == "lte_field" {
			if f.CompareWith == "" {
				add(f.FieldID, IssueBadFilterValue,
					"筛选 %s 使用 lte_field 时必须声明 compare_with", f.Key)
			} else if other, ok := m.FieldByID(f.CompareWith); !ok {
				add(f.CompareWith, IssueUnknownFilterField,
					"筛选 %s 的 compare_with %s 未声明", f.Key, f.CompareWith)
			} else if !numericTypes[target.Type] || !numericTypes[other.Type] {
				add(f.FieldID, IssueBadFilterValue,
					"筛选 %s 的 lte_field 只支持数值字段之间的比较", f.Key)
			}
		} else if f.CompareWith != "" {
			add(f.FieldID, IssueBadFilterValue,
				"筛选 %s 只有 lte_field 可以声明 compare_with", f.Key)
		}
	}

	// 共用操作
	opSeen := map[string]bool{}
	for _, op := range m.Operations {
		if op.Key == "" {
			add("", IssueMissingKey, "操作必须声明 key")
			continue
		}
		if opSeen[op.Key] {
			add("", IssueDuplicateKey, "操作 %s 重复声明", op.Key)
			continue
		}
		opSeen[op.Key] = true
		if !coreOperationSet[op.Key] {
			add("", IssueUnknownOperation,
				"操作 %q 不是核心定义的共用操作，模板不能声明新操作", op.Key)
			continue
		}
		if strings.TrimSpace(op.Label) == "" {
			add("", IssueMissingLabel, "操作 %s 缺少 label", op.Key)
		}
		// 只有一个位置的动作按钮在按量管理下没有意义。
		if op.Key == "core:adjust_quantity" && m.Management != ManagementQuantity {
			add("", IssueUnknownOperation,
				"操作 %s 只适用于按量管理的模板（当前 management=%s）", op.Key, m.Management)
		}
	}
	if len(m.Operations) == 0 {
		add("", IssueUnknownOperation, "模板必须至少声明一个共用操作")
	}

	// 预设
	for k := range m.Preset.Fields {
		if !seen[k] {
			add(k, IssuePresetUnknownField, "preset.fields 引用了未声明的字段 %s", k)
		}
	}

	// 问题顺序稳定，便于测试与 diff。
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].FieldID != issues[j].FieldID {
			return issues[i].FieldID < issues[j].FieldID
		}
		return issues[i].Code < issues[j].Code
	})
	return issues
}

// validateFilterValue 校验筛选比较值。值只能是字面量、字面量数组或日期占位符。
func validateFilterValue(f Filter, target Field, hasTarget bool) error {
	if f.Op == "empty" || f.Op == "not_empty" {
		if f.Value != nil {
			return fmt.Errorf("empty / not_empty 不接受比较值")
		}
		return nil
	}
	switch f.Op {
	case "between":
		arr, ok := f.Value.([]any)
		if !ok || len(arr) != 2 {
			return fmt.Errorf("between 需要长度为 2 的数组")
		}
		for _, v := range arr {
			if err := checkLiteral(v, target, hasTarget); err != nil {
				return err
			}
		}
		return nil
	case "lte_field":
		// 比较值来自同一条记录的另一个字段（CompareWith），不取字面量；
		// 字段间的声明与类型校验在上方筛选结构校验里完成。
		return nil
	default:
		if f.Value == nil {
			return fmt.Errorf("操作符 %s 需要比较值", f.Op)
		}
		return checkLiteral(f.Value, target, hasTarget)
	}
}

func checkLiteral(v any, target Field, hasTarget bool) error {
	s, ok := v.(string)
	if !ok {
		if _, isNum := toFloat(v); !isNum {
			return fmt.Errorf("比较值必须是字符串或数字")
		}
		return nil
	}
	if placeholderRe.MatchString(s) {
		// 只允许 __today__ 与 __today+<n>d__。
		if _, err := ResolvePlaceholder(s, time.Now()); err != nil {
			return err
		}
	}
	if hasTarget && target.Type == TypeDate && !strings.HasPrefix(s, "__") {
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return fmt.Errorf("日期比较值必须是 YYYY-MM-DD 形式")
		}
	}
	return nil
}

func validSlug(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}

// validPluginID 接受 com.example.name 形式的插件标识。
func validPluginID(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i := 0; i < len(p); i++ {
			c := p[i]
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// splitFieldID 拆出命名空间。内置字段不含冒号。
func splitFieldID(id string) (owner, key string, ok bool) {
	i := strings.Index(id, FieldNamespaceSep)
	if i <= 0 || i == len(id)-1 {
		return "", id, false
	}
	return id[:i], id[i+1:], true
}

// reservedFieldID 列出不能被扩展字段占用的核心列名。
//
// 与 coreColumnSet 同源，但额外排除几个「一看就像扩展字段」的名字，
// 避免模板用一个同名字段覆盖核心语义。
func reservedFieldID(id string) bool {
	if coreColumnSet[id] {
		return true
	}
	switch id {
	case "id", "version", "created_at", "updated_at", "deleted_at",
		"public_code", "custom_fields", "field_schema_version":
		return true
	}
	return false
}

func duplicateString(in []string) string {
	seen := map[string]bool{}
	for _, s := range in {
		if seen[s] {
			return s
		}
		seen[s] = true
	}
	return ""
}
