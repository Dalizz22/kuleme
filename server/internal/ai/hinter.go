package ai

import (
	"context"
	"fmt"

	"github.com/klm/server/internal/templates"
)

// templateHinter 是 TemplateHinter 端口的内置实现：直接读内置模板目录。
//
// 只把 field_id / 展示名 / 类型名发给外部服务；默认值、枚举全集、
// 校验规则都留在服务端。
type templateHinter struct {
	cat *templates.Catalog
}

func newTemplateHinter(cat *templates.Catalog) *templateHinter {
	return &templateHinter{cat: cat}
}

// resolve 解析模板标识；为空用默认模板。
func (h *templateHinter) resolve(templateKey string) (templates.Manifest, error) {
	if h.cat == nil {
		return templates.Manifest{}, fmt.Errorf("模板目录不可用")
	}
	if templateKey == "" {
		templateKey = h.cat.DefaultKey()
	}
	return h.cat.ValidateTemplateKey(templateKey)
}

// Manifest 返回模板元数据（service 层组装物品入参用）。
func (h *templateHinter) Manifest(templateKey string) (templates.Manifest, error) {
	return h.resolve(templateKey)
}

// FieldHints 返回可建议给模型的字段说明（跳过只读字段）。
func (h *templateHinter) FieldHints(ctx context.Context, templateKey string) ([]FieldHint, error) {
	m, err := h.resolve(templateKey)
	if err != nil {
		return nil, err
	}
	out := make([]FieldHint, 0, len(m.Fields))
	for _, f := range m.Fields {
		if f.Readonly {
			continue
		}
		out = append(out, FieldHint{FieldID: f.FieldID, Label: f.Label, Type: string(f.Type)})
	}
	return out, nil
}

// ApplyFields 按模板校验并归一字段值；manual 覆盖模型建议，两者走同一套校验。
//
// 返回：可写入值、字段结论（applied / applied_default / manual / rejected）、
// 手动字段标识、被拒字段标识、错误。
func (h *templateHinter) ApplyFields(ctx context.Context, templateKey string, values map[string]any, manual map[string]any) (map[string]any, map[string]string, []string, []string, error) {
	m, err := h.resolve(templateKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	merged := make(map[string]any, len(values)+len(manual))
	for k, v := range values {
		merged[k] = v
	}
	for k, v := range manual {
		merged[k] = v
	}
	// 非严格合并：模型给的个别坏字段只标 rejected，不毁掉整条候选；
	// 人工订正走同一校验，坏值同样被拒并在结论里可见。
	res := templates.Merge(m, templates.NewCustomFields(m.SchemaVersion), merged, templates.MergeOptions{})
	applied := map[string]string{}
	var rejected []string
	for _, d := range res.Decisions {
		switch d.Code {
		case templates.DecisionApplied:
			if _, isManual := manual[d.FieldID]; isManual {
				applied[d.FieldID] = "manual"
			} else {
				applied[d.FieldID] = "applied"
			}
		case templates.DecisionDefaulted, templates.DecisionPresetApplied:
			applied[d.FieldID] = "applied_default"
		case templates.DecisionRejected:
			applied[d.FieldID] = "rejected"
			rejected = append(rejected, d.FieldID)
		case templates.DecisionReadonlyKept:
			applied[d.FieldID] = "manual"
		}
	}
	rejected = append(rejected, res.UnrecognizedInputs...)
	manualIDs, _ := h.ManualFieldIDs(ctx, templateKey)
	return res.Fields.Values, applied, manualIDs, rejected, nil
}

// ManualFieldIDs 返回模板里声明为手动填写的字段。
// 当前模板 schema 没有该声明，返回空；保留接口以对齐 ports。
func (h *templateHinter) ManualFieldIDs(ctx context.Context, templateKey string) ([]string, error) {
	return []string{}, nil
}

// DefaultQuantity 返回模板默认数量。
// 确认不落初始库存（item.CreateInput 无数量入口），统一按 1。
func (h *templateHinter) DefaultQuantity(ctx context.Context, templateKey string) (string, error) {
	return "1", nil
}
