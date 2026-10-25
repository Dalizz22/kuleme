package labels

import (
	"fmt"
	"strings"
)

// LayoutFormat 是一份「要印成什么样」的版面参数。
//
// 之所以是一个可序列化的结构而不是几个函数参数：版面参数会出现在接口请求体、
// 打印任务的存档、以及以前的打印记录里。存成结构才能在补打时按原参数重放，
// 而不是靠调用方记得当时的尺寸。
//
// 两种形态都由同一份 Layout 表达，没有「A4 模式」与「单标签模式」两套代码：
//
//	{
//	  "layout": "a4"                                → A4 多格（默认 3 列标签纸）
//	  { "paper_width_mm": 50, "paper_height_mm": 30,
//	    "label_width_mm": 50, "label_height_mm": 30 } → 自定义单标签（含 30×50 mm 示例）
type LayoutFormat struct {
	// Preset 是预设名：a4（默认）、single、直传 custom。
	Preset string `json:"preset,omitempty"`

	// 自定义尺寸（mm）。为零时按预设补默认值。
	PaperWidthMM  float64 `json:"paper_width_mm,omitempty"`
	PaperHeightMM float64 `json:"paper_height_mm,omitempty"`
	LabelWidthMM  float64 `json:"label_width_mm,omitempty"`
	LabelHeightMM float64 `json:"label_height_mm,omitempty"`

	// MarginTopMM / MarginLeftMM 是标签区相对纸张的偏移。
	MarginTopMM  float64 `json:"margin_top_mm,omitempty"`
	MarginLeftMM float64 `json:"margin_left_mm,omitempty"`

	// GapXMM / GapYMM 是标签之间的间距。
	GapXMM float64 `json:"gap_x_mm,omitempty"`
	GapYMM float64 `json:"gap_y_mm,omitempty"`

	// Rows / Cols 为 0 时按纸张自动排满。
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`

	// PaddingMM 是标签内边距。它同时决定二维码静区宽度，默认 1.5 mm。
	PaddingMM float64 `json:"padding_mm,omitempty"`

	// QRSizeMM 是二维码边长；0 表示按标签高度自动取值。
	QRSizeMM float64 `json:"qr_size_mm,omitempty"`
}

// 预设名。稳定字符串，接口与前端按名引用。
const (
	PresetA4     = "a4"
	PresetSingle = "single"
	PresetCustom = "custom"
)

// ErrUnknownPreset 表示版面预设名不认识。
var ErrUnknownPreset = fmt.Errorf("%w: 未知的版面预设", ErrLayout)

// Layout 解析成可计算的几何。
//
// single 预设要求给出目标标签尺寸（label_width_mm / label_height_mm），
// 纸张尺寸默认等于标签尺寸——单标签打印就是「一张纸一个标签」，
// 由标签机的纸宽决定内容宽度。猜错纸宽会把内容印到纸外，所以宁可要求显式给值。
func (f LayoutFormat) Layout() (Layout, error) {
	preset := strings.ToLower(strings.TrimSpace(f.Preset))
	switch preset {
	case "", PresetA4:
		out := A4DefaultLayout()
		applyOverrides(&out, f)
		return out.Normalize()
	case PresetSingle, PresetCustom:
		if f.LabelWidthMM <= 0 || f.LabelHeightMM <= 0 {
			return Layout{}, fmt.Errorf("%w: %s 预设必须给出 label_width_mm 与 label_height_mm",
				ErrLayout, preset)
		}
		out := Layout{
			PaperWidthMM:  f.LabelWidthMM,
			PaperHeightMM: f.LabelHeightMM,
			LabelWidthMM:  f.LabelWidthMM,
			LabelHeightMM: f.LabelHeightMM,
			Rows:          1,
			Cols:          1,
			PaddingMM:     DefaultPaddingMM,
		}
		applyOverrides(&out, f)
		return out.Normalize()
	default:
		return Layout{}, fmt.Errorf("%w: %q", ErrUnknownPreset, f.Preset)
	}
}

// applyOverrides 把非零的自定义值覆盖到预设上。
//
// 零值表示「没指定」而不是「设为 0」：这些尺寸为 0 时版面本身无意义，
// 因此 0 只能理解为缺省。想要把边距改成 0，给一个明确的 0 也没用——
// 这在 Normalize 的校验里是允许的（边距可以为 0），所以用指针会更严谨；
// 这里选择「0 = 缺省」是因为边距 0 与默认 12 mm 的差别在一次打印里
// 肉眼就能发现，代价是使用者想设 0 时要显式说明。
func applyOverrides(out *Layout, f LayoutFormat) {
	if f.PaperWidthMM > 0 {
		out.PaperWidthMM = f.PaperWidthMM
	}
	if f.PaperHeightMM > 0 {
		out.PaperHeightMM = f.PaperHeightMM
	}
	if f.LabelWidthMM > 0 {
		out.LabelWidthMM = f.LabelWidthMM
	}
	if f.LabelHeightMM > 0 {
		out.LabelHeightMM = f.LabelHeightMM
	}
	if f.MarginTopMM > 0 {
		out.MarginTopMM = f.MarginTopMM
	}
	if f.MarginLeftMM > 0 {
		out.MarginLeftMM = f.MarginLeftMM
	}
	if f.GapXMM > 0 {
		out.GapXMM = f.GapXMM
	}
	if f.GapYMM > 0 {
		out.GapYMM = f.GapYMM
	}
	if f.Rows > 0 {
		out.Rows = f.Rows
	}
	if f.Cols > 0 {
		out.Cols = f.Cols
	}
	if f.PaddingMM > 0 {
		out.PaddingMM = f.PaddingMM
	}
	if f.QRSizeMM > 0 {
		out.QRSizeMM = f.QRSizeMM
	}
}

// LayoutPresets 返回内置版面的说明，供设置页与接口展示。
//
// 说明里带着 mm 数值而不是只有名字：使用者要拿尺子量标签纸，
// 看到「63.5 × 38.1 mm」比看到「A4 三列」有用得多。
func LayoutPresets() []Preset {
	a4 := A4DefaultLayout()
	single := LayoutFormat{Preset: PresetSingle, LabelWidthMM: 30, LabelHeightMM: 50}
	return []Preset{
		{
			Key:         PresetA4,
			Label:       "A4 多格标签纸",
			Description: fmt.Sprintf("A4 纸，默认 %g × %g mm/格，自动排满整页", a4.LabelWidthMM, a4.LabelHeightMM),
			Layout:      LayoutFormat{Preset: PresetA4},
		},
		{
			Key:         PresetSingle,
			Label:       "单标签（标签机）",
			Description: "一张纸一个标签，尺寸按标签机纸宽设定，例如 30 × 50 mm",
			Layout:      single,
		},
	}
}

// Preset 是版面预设的说明。
type Preset struct {
	Key         string       `json:"key"`
	Label       string       `json:"label"`
	Description string       `json:"description"`
	Layout      LayoutFormat `json:"layout"`
}

// LayoutDescription 把解析后的几何转成一段人话，用于打印确认。
func LayoutDescription(l Layout) string {
	return fmt.Sprintf("纸张 %g × %g mm，标签 %g × %g mm，%d 行 × %d 列，共 %d 格，内边距 %g mm，二维码 %g mm",
		l.PaperWidthMM, l.PaperHeightMM, l.LabelWidthMM, l.LabelHeightMM,
		l.Rows, l.Cols, l.Rows*l.Cols, l.PaddingMM, l.QRSizeMM)
}
