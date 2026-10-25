package templates

import (
	"fmt"
	"sort"
	"strings"
)

// 字段处理结论。稳定字符串，前端按码本地化，不比对中文。
const (
	// DecisionApplied 本次提交了合法值。
	DecisionApplied = "APPLIED"
	// DecisionCleared 本次显式清空了字段。
	DecisionCleared = "CLEARED"
	// DecisionDefaulted 未提交值，按字段默认值补齐。
	DecisionDefaulted = "APPLIED_DEFAULT"
	// DecisionPresetApplied 未提交值，换了模板后按模板预设补齐。
	DecisionPresetApplied = "APPLIED_PRESET"
	// DecisionRejected 值不合法，本次提交被拒绝（该字段保持原值）。
	DecisionRejected = "REJECTED"
	// DecisionStale 值是旧 schema 版本写入的，当前模板不再声明该字段；
	// 原值保留，界面只读展示。这不是错误，也不提示用户处理。
	DecisionStale = "STALE_SCHEMA"
	// DecisionNeedsBackfill 当前模板把该字段标为必填，但记录里没有值。
	// 记录仍可读，只是需要补录；不因此拒绝整个更新。
	DecisionNeedsBackfill = "NEEDS_BACKFILL"
	// DecisionReadonlyKept 该字段由核心服务计算，客户端提交被忽略，保留原值。
	DecisionReadonlyKept = "READONLY_KEPT"
)

// Decision 是单个字段的处理结论。
type Decision struct {
	FieldID string `json:"field_id"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// ValueSource 标识值从哪来，供界面解释「为什么这个字段有值」。
type ValueSource string

const (
	SourceStored  ValueSource = "stored"
	SourceInput   ValueSource = "input"
	SourceDefault ValueSource = "default"
	SourcePreset  ValueSource = "preset"
)

// Resolution 是一次合并的结果。
type Resolution struct {
	Fields             CustomFields
	Decisions          []Decision
	UnrecognizedInputs []string
	SchemaChanged      bool
	Sources            map[string]ValueSource
}

// Rejected 返回被拒绝的字段结论，调用方据此组装 VALIDATION_FAILED 的 details。
func (r Resolution) Rejected() []Decision {
	var out []Decision
	for _, d := range r.Decisions {
		if d.Code == DecisionRejected {
			out = append(out, d)
		}
	}
	return out
}

// Warnings 返回不是错误但需要提示的结论（补录、只读字段被提交等）。
func (r Resolution) Warnings() []Decision {
	var out []Decision
	for _, d := range r.Decisions {
		switch d.Code {
		case DecisionStale, DecisionNeedsBackfill, DecisionReadonlyKept:
			out = append(out, d)
		}
	}
	return out
}

// Labels 返回字段标识 → 展示名。未声明的字段用标识本身兜底：宁可显示一个
// 内部标识，也不能把数据藏起来。
func (r Resolution) Labels(m Manifest) map[string]string {
	out := map[string]string{}
	for _, f := range m.Fields {
		out[f.FieldID] = f.Label
	}
	for _, id := range r.UnrecognizedInputs {
		if _, ok := out[id]; !ok {
			out[id] = id
		}
	}
	return out
}

// MergeOptions 控制一次合并的语义。
type MergeOptions struct {
	// Preset 在记录没有该字段值时按模板预设补齐（换模板场景）。
	// 为空表示不套用预设。
	Preset *Preset
	// Strict 为 true 时，任何被拒绝的字段都视为整体失败；调用方用它决定
	// 是「部分成功并返回警告」还是「整单拒绝」。
	Strict bool
}

// Merge 把客户端提交的字段值合并进已有 custom_fields。
//
// 语义（每条都有对应的验收点）：
//
//   - 未出现在 input 里的字段保持原值，不因为「这次没提交」而被清空。
//   - input 里显式出现 null 或空字符串表示清空该字段。
//   - input 里未声明的字段标识不写入、也不报错，放进 UnrecognizedInputs：
//     换模板后旧的提交方（或插件）仍可能发来旧字段，直接拒绝会让整个
//     更新失败，静默丢弃又会让人以为保存成功。
//   - 模板升级（schema_version 变化）不改写已有值，只更新记录里的版本号；
//     字段含义变化由「新增字段」承担。
//   - 已存在的未知字段（旧模板/已停用插件写入的）一律保留原值。
func Merge(m Manifest, existing CustomFields, input map[string]any, opts MergeOptions) Resolution {
	res := Resolution{
		Fields:  NewCustomFields(m.SchemaVersion),
		Sources: map[string]ValueSource{},
	}
	for id, v := range existing.Values {
		res.Fields.Values[id] = v
		if _, ok := m.FieldByID(id); ok {
			res.Sources[id] = SourceStored
		}
	}
	res.SchemaChanged = existing.FieldSchemaVersion != m.SchemaVersion

	seen := map[string]bool{}
	for _, f := range m.Fields {
		provided, has := input[f.FieldID]
		seen[f.FieldID] = true

		if has && f.Readonly {
			// 只读字段由核心服务写入（例如按位置汇总的余量）。客户端提交
			// 不生效，但也不报错——页面可能整表提交回来。
			res.Decisions = append(res.Decisions, Decision{
				FieldID: f.FieldID,
				Code:    DecisionReadonlyKept,
				Message: fmt.Sprintf("%s 由系统计算，已忽略提交值", f.Label),
			})
			continue
		}

		if has {
			norm, err := normalizeValue(f, provided)
			if err != nil {
				res.Decisions = append(res.Decisions, Decision{
					FieldID: f.FieldID,
					Code:    DecisionRejected,
					Message: fmt.Sprintf("%s：%v", f.Label, err),
				})
				continue
			}
			if norm == nil {
				delete(res.Fields.Values, f.FieldID)
				res.Decisions = append(res.Decisions, Decision{
					FieldID: f.FieldID,
					Code:    DecisionCleared,
					Message: fmt.Sprintf("%s 已清空", f.Label),
				})
				delete(res.Sources, f.FieldID)
				continue
			}
			res.Fields.Values[f.FieldID] = norm
			res.Sources[f.FieldID] = SourceInput
			res.Decisions = append(res.Decisions, Decision{
				FieldID: f.FieldID,
				Code:    DecisionApplied,
			})
			continue
		}

		// 未提交：只有原来没有值时才补默认值或预设，绝不覆盖已有资料。
		if _, ok := res.Fields.Values[f.FieldID]; ok {
			continue
		}
		if presetValue, ok := presetFieldValue(opts.Preset, f.FieldID); ok {
			norm, err := normalizeValue(f, presetValue)
			if err == nil && norm != nil {
				res.Fields.Values[f.FieldID] = norm
				res.Sources[f.FieldID] = SourcePreset
				res.Decisions = append(res.Decisions, Decision{
					FieldID: f.FieldID,
					Code:    DecisionPresetApplied,
					Message: fmt.Sprintf("%s 按模板预设补齐", f.Label),
				})
				continue
			}
		}
		if f.Default != nil {
			norm, err := normalizeValue(f, f.Default)
			if err == nil && norm != nil {
				res.Fields.Values[f.FieldID] = norm
				res.Sources[f.FieldID] = SourceDefault
				res.Decisions = append(res.Decisions, Decision{
					FieldID: f.FieldID,
					Code:    DecisionDefaulted,
					Message: fmt.Sprintf("%s 使用默认值", f.Label),
				})
				continue
			}
		}
	}

	// 未声明的输入：不写入，交给调用方决定是否提示。
	var unrecognized []string
	for id := range input {
		if !seen[id] {
			unrecognized = append(unrecognized, id)
		}
	}
	res.UnrecognizedInputs = SortFieldIDs(unrecognized)

	// 未知但已存在的字段：明确标为旧 schema 值，供界面只读展示。
	var stale []string
	declared := map[string]bool{}
	for _, f := range m.Fields {
		declared[f.FieldID] = true
	}
	for id := range res.Fields.Values {
		if !declared[id] {
			stale = append(stale, id)
		}
	}
	for _, id := range SortFieldIDs(stale) {
		res.Decisions = append(res.Decisions, Decision{
			FieldID: id,
			Code:    DecisionStale,
			Message: fmt.Sprintf("%s 由旧模板或已停用的扩展写入，保留原值只读展示", id),
		})
	}

	// 必填但缺值：提示补录，不拒绝整次更新。
	for _, f := range m.Fields {
		if !f.Required || f.Readonly {
			continue
		}
		if v, ok := res.Fields.Values[f.FieldID]; !ok || v == nil || v == "" {
			res.Decisions = append(res.Decisions, Decision{
				FieldID: f.FieldID,
				Code:    DecisionNeedsBackfill,
				Message: fmt.Sprintf("%s 为必填项，尚未填写", f.Label),
			})
		}
	}

	sort.SliceStable(res.Decisions, func(i, j int) bool {
		return res.Decisions[i].FieldID < res.Decisions[j].FieldID
	})
	return res
}

// presetFieldValue 读取模板预设里的字段值。预设显式写成 null 表示
// 「该字段留空」，不是「没有预设」，因此要区分 ok 与值为 nil。
func presetFieldValue(p *Preset, fieldID string) (any, bool) {
	if p == nil || p.Fields == nil {
		return nil, false
	}
	v, ok := p.Fields[fieldID]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

// ApplyPresetTags 把模板预设标签合并进物品标签列表。
//
// 标签是文本列表，不做去重之外的加工；已有标签永远排在前面，追加预设标签，
// 避免「换模板把用户自己打的标签挤掉」。
func ApplyPresetTags(existing []string, p *Preset) []string {
	if p == nil || len(p.Tags) == 0 {
		return existing
	}
	out := make([]string, 0, len(existing)+len(p.Tags))
	seen := map[string]bool{}
	for _, t := range existing {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range p.Tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
