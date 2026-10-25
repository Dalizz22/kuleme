// Package labels 把物品/位置的标签渲染成可打印文件，并把「生成文件」与
// 「发送到设备」分成两件独立的事。
//
// 为什么分开（见接口契约约定第 8 节）：
//
//   - 生成是纯计算：给定标签内容与纸张尺寸，产出一份 PDF 字节 + 版面清单。
//     它不需要设备、不需要数据库，可以在任何地方重放与测试。
//   - 发送是有副作用的：写 USB / 蓝牙 / 打印服务队列。设备在现实里会缺纸、
//     离线、超时，把它和渲染耦合在一起会让「重打印」变成「重新渲染 + 推理
//     上次发到哪」。分开之后补打就是再发一次同一份文件。
//
// 因此本包只做两件事：Render 出字节，Transport 把字节发出去。调用方负责
// 把字节存到下载中心（文件生成的落点），也负责在发送后记录审计。
package labels

import (
	"errors"
	"fmt"

	"github.com/klm/server/internal/labels/qrcode"
)

// mm 是 1 毫米对应的 PDF 点数（1 pt = 1/72 英寸，1 英寸 = 25.4 mm）。
//
// 用浮点常量而不是整数：标签尺寸是物理量，0.1 mm 的误差在 30 格 A4 sheet 上
// 会累积成 3 mm，足够让最后一列贴出标签纸。精度保留在计算结果里，
// 只在写 PDF 时按 2 位小数舍入。
const mm = 72.0 / 25.4

// A4 纸张尺寸（mm）。条码标签的实际可用区域由边距决定。
const (
	A4WidthMM  = 210.0
	A4HeightMM = 297.0
)

// 标签纸几何的默认值（mm）。它们是「A4 不干胶标签纸」最常见的规格，
// 但都可以被 Coverage 覆盖——各家标签纸差 1 mm 就会错位，所以这些是默认值
// 不是常量表。
const (
	DefaultMarginTopMM   = 12.0
	DefaultMarginLeftMM  = 8.0
	DefaultGapXMM        = 2.5
	DefaultGapYMM        = 0.0
	DefaultLabelWidthMM  = 63.5
	DefaultLabelHeightMM = 38.1
	DefaultQRSizeMM      = 20.0
	DefaultPaddingMM     = 1.5
	MaxLabelsPerSheet    = 100
	MinLabelSizeMM       = 15.0
	MaxLabelSizeMM       = 200.0
)

// Layout 是单张标签纸的几何定义。
//
// Rows / Cols 为 0 表示「按纸张与边距自动排满」。显式给值时按给定值排，
// 允许左下角留空（有些标签纸是 3×8 而不是 3×7）。
type Layout struct {
	PaperWidthMM  float64 `json:"paper_width_mm"`
	PaperHeightMM float64 `json:"paper_height_mm"`

	MarginTopMM   float64 `json:"margin_top_mm"`
	MarginLeftMM  float64 `json:"margin_left_mm"`
	LabelWidthMM  float64 `json:"label_width_mm"`
	LabelHeightMM float64 `json:"label_height_mm"`

	GapXMM float64 `json:"gap_x_mm"`
	GapYMM float64 `json:"gap_y_mm"`

	// Rows / Cols 为 0 时自动计算。
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`

	// PaddingMM 是标签内容与标签边缘的留白。
	PaddingMM float64 `json:"padding_mm"`

	// QRSizeMM 是二维码边长。0 表示按标签高度自动取值。
	QRSizeMM float64 `json:"qr_size_mm,omitempty"`
}

// A4DefaultLayout 返回 A4 三列标签纸的默认几何。
func A4DefaultLayout() Layout {
	return Layout{
		PaperWidthMM:  A4WidthMM,
		PaperHeightMM: A4HeightMM,
		MarginTopMM:   DefaultMarginTopMM,
		MarginLeftMM:  DefaultMarginLeftMM,
		LabelWidthMM:  DefaultLabelWidthMM,
		LabelHeightMM: DefaultLabelHeightMM,
		GapXMM:        DefaultGapXMM,
		GapYMM:        DefaultGapYMM,
		PaddingMM:     DefaultPaddingMM,
		QRSizeMM:      DefaultQRSizeMM,
	}
}

// LegacyTapeLayout 返回单标签（标签机连续纸）的几何。
//
// 单标签打印是把「一张纸就是一个标签」的语义表达出来，而不是给 A4 版面
// 加一堆特例分支：纸张尺寸等于标签尺寸、边距为 0、1 行 1 列。
// tapeWidthMM 是标签机纸宽（常见 40 / 50 / 62 mm），必须显式给定——
// 猜错纸宽会直接把内容印到纸外。
func LegacyTapeLayout(tapeWidthMM, labelHeightMM float64) Layout {
	return Layout{
		PaperWidthMM:  tapeWidthMM,
		PaperHeightMM: labelHeightMM,
		LabelWidthMM:  tapeWidthMM,
		LabelHeightMM: labelHeightMM,
		Rows:          1,
		Cols:          1,
		PaddingMM:     DefaultPaddingMM,
	}
}

// ErrLayout 表示版面参数不合法，无法排版。
var ErrLayout = errors.New("标签版面参数不合法")

// Normalize 补默认值并校验范围，返回可直接参与计算的几何。
//
// 校验是必需的而不是礼貌：版面参数来自用户输入（自定义标签尺寸），
// 一个 0 或负数会算出无穷循环或把内容全排到纸外。
func (l Layout) Normalize() (Layout, error) {
	out := l
	if out.PaperWidthMM <= 0 {
		out.PaperWidthMM = A4WidthMM
	}
	if out.PaperHeightMM <= 0 {
		out.PaperHeightMM = A4HeightMM
	}
	if out.LabelWidthMM <= 0 {
		out.LabelWidthMM = DefaultLabelWidthMM
	}
	if out.LabelHeightMM <= 0 {
		out.LabelHeightMM = DefaultLabelHeightMM
	}
	if out.GapXMM < 0 || out.GapYMM < 0 {
		return Layout{}, fmt.Errorf("%w: 间距不能为负", ErrLayout)
	}
	if out.MarginTopMM < 0 || out.MarginLeftMM < 0 {
		return Layout{}, fmt.Errorf("%w: 边距不能为负", ErrLayout)
	}
	if out.PaddingMM < 0 {
		return Layout{}, fmt.Errorf("%w: 内边距不能为负", ErrLayout)
	}
	if out.LabelWidthMM < MinLabelSizeMM || out.LabelWidthMM > MaxLabelSizeMM {
		return Layout{}, fmt.Errorf("%w: 标签宽度必须在 %.1f~%.1f mm 之间，实际 %.1f",
			ErrLayout, MinLabelSizeMM, MaxLabelSizeMM, out.LabelWidthMM)
	}
	if out.LabelHeightMM < MinLabelSizeMM || out.LabelHeightMM > MaxLabelSizeMM {
		return Layout{}, fmt.Errorf("%w: 标签高度必须在 %.1f~%.1f mm 之间，实际 %.1f",
			ErrLayout, MinLabelSizeMM, MaxLabelSizeMM, out.LabelHeightMM)
	}
	if out.PaperWidthMM < out.LabelWidthMM || out.PaperHeightMM < out.LabelHeightMM {
		return Layout{}, fmt.Errorf("%w: 纸张（%.1f×%.1f mm）小于单个标签（%.1f×%.1f mm）",
			ErrLayout, out.PaperWidthMM, out.PaperHeightMM,
			out.LabelWidthMM, out.LabelHeightMM)
	}
	if out.PaddingMM*2 >= out.LabelWidthMM || out.PaddingMM*2 >= out.LabelHeightMM {
		return Layout{}, fmt.Errorf("%w: 内边距 %.1f mm 相对标签尺寸过大", ErrLayout, out.PaddingMM)
	}

	// 可用区域：纸张减去边距（左右对称，边距只给左上是因为标签纸通常
	// 从左上开始，右侧与下侧的余量由格子数自然吸收）。
	availW := out.PaperWidthMM - out.MarginLeftMM*2
	availH := out.PaperHeightMM - out.MarginTopMM*2
	if availW <= 0 || availH <= 0 {
		return Layout{}, fmt.Errorf("%w: 边距吃掉全部纸张", ErrLayout)
	}

	if out.Cols <= 0 {
		out.Cols = int((availW + out.GapXMM) / (out.LabelWidthMM + out.GapXMM))
	}
	if out.Rows <= 0 {
		out.Rows = int((availH + out.GapYMM) / (out.LabelHeightMM + out.GapYMM))
	}
	if out.Rows < 1 || out.Cols < 1 {
		return Layout{}, fmt.Errorf("%w: 当前尺寸在 %.1f×%.1f mm 纸张上排不下一个标签",
			ErrLayout, out.PaperWidthMM, out.PaperHeightMM)
	}
	if out.Rows*out.Cols > MaxLabelsPerSheet {
		return Layout{}, fmt.Errorf("%w: 单张标签数 %d 超过上限 %d",
			ErrLayout, out.Rows*out.Cols, MaxLabelsPerSheet)
	}

	// 硬约束：二维码不得超过可打印宽度的一半——另一半留给
	// 文字；同时不能高过可打印高度。小标签上 15 mm 的可靠扫描下限让位于上限。
	printableW := out.LabelWidthMM - out.PaddingMM*2
	printableH := out.LabelHeightMM - out.PaddingMM*2
	maxQR := printableW / 2
	if printableH < maxQR {
		maxQR = printableH
	}
	if out.QRSizeMM <= 0 {
		out.QRSizeMM = maxQR
		if out.QRSizeMM > 15 {
			out.QRSizeMM = 15
		}
	}
	if out.QRSizeMM > maxQR {
		out.QRSizeMM = maxQR
	}
	if out.QRSizeMM <= 0 {
		return Layout{}, fmt.Errorf("%w: 二维码尺寸算出来是非正数", ErrLayout)
	}
	return out, nil
}

// Capacity 返回一张纸的格子数。
func (l Layout) Capacity() int {
	n, err := l.Normalize()
	if err != nil {
		return 0
	}
	return n.Rows * n.Cols
}

// Slot 是一个标签在纸上的位置，单位为 PDF 点，原点在纸张左下角。
type Slot struct {
	Index int     `json:"index"`
	Row   int     `json:"row"`
	Col   int     `json:"col"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
}

// Slots 返回前 count 个格子的位置。
//
// 行序自上而下、列序自左而右，与阅读顺序一致——打印出来贴的时候按同样顺序数，
// 不会出现「第三行第一格其实是第六个」这种对不上的情况。
func (l Layout) Slots(count int) ([]Slot, error) {
	n, err := l.Normalize()
	if err != nil {
		return nil, err
	}
	if count < 0 {
		return nil, fmt.Errorf("%w: 标签数量不能为负", ErrLayout)
	}
	total := n.Rows * n.Cols
	if count > total {
		return nil, fmt.Errorf("%w: 需要 %d 个标签，本版面只有 %d 格",
			ErrLayout, count, total)
	}
	out := make([]Slot, 0, count)
	for i := 0; i < count; i++ {
		row := i / n.Cols
		col := i % n.Cols
		xMM := n.MarginLeftMM + float64(col)*(n.LabelWidthMM+n.GapXMM)
		// Y 轴向上：第 0 行在最上面。
		yTopMM := n.MarginTopMM + float64(row)*(n.LabelHeightMM+n.GapYMM)
		yMM := n.PaperHeightMM - yTopMM - n.LabelHeightMM
		out = append(out, Slot{
			Index: i,
			Row:   row,
			Col:   col,
			X:     round2(xMM * mm),
			Y:     round2(yMM * mm),
			W:     round2(n.LabelWidthMM * mm),
			H:     round2(n.LabelHeightMM * mm),
		})
	}
	return out, nil
}

// Content 是一个标签要印的内容。
//
// 字段数固定且都是字符串，不做「模板化布局」：标签尺寸小，靠一个固定的
// 信息层级（标题最大、副标题次之、其余行等宽）比可配置布局更不容易印坏。
type Content struct {
	// Payload 是二维码内容，形如 KLM1:<instance>:<kind>:<code>。
	Payload string `json:"payload"`
	// Title 是主标题（物品名称）。
	Title string `json:"title"`
	// Subtitle 是次标题（公开码或位置全路径）。
	Subtitle string `json:"subtitle"`
	// Lines 是附加信息行（分类、位置、余量…），按顺序渲染，超出部分截断。
	Lines []string `json:"lines,omitempty"`
	// Note 是底部小字（例如补打标记）。
	Note string `json:"note,omitempty"`
}

// Validate 校验单个标签内容，确保二维码可以生成。
//
// 二维码内容必须是本实现支持的 ASCII：payload 由 kod.QRPayload 产生，
// 天然满足。在这里检查而不是让渲染中途失败，是因为一张纸 30 个标签，
// 第 28 个失败会浪费整张纸。
func (c Content) Validate() error {
	if c.Payload == "" {
		return fmt.Errorf("标签缺少二维码内容")
	}
	if _, err := qrcode.EncodeAny(c.Payload); err != nil {
		return fmt.Errorf("二维码内容不合法: %w", err)
	}
	return nil
}

// Job 是一次打印任务：把 Contents 排到 Layout 上。
//
// ReprintOf 记录「这是对哪次任务的补打」。补打不是特殊分支，而是同一次渲染
// 加上溯源信息：这样补打出来的标签和原件内容一致，只是多一个可追溯的标记，
// 不需要维护「原件」与「副本」两套生成路径。
type Job struct {
	// SheetID 是本次打印的标识，由调用方生成（B 的 ID 生成器）。
	SheetID string `json:"sheet_id"`
	// ReprintOf 非空表示本次是补打，值是原任务 SheetID。
	ReprintOf string `json:"reprint_of,omitempty"`
	// ReprintLabel 是补打时印在标签上的小字。为空则用默认文案。
	ReprintLabel string    `json:"reprint_label,omitempty"`
	Layout       Layout    `json:"layout"`
	Contents     []Content `json:"contents"`
}

// DefaultReprintLabel 是补打标记的默认文案。
const DefaultReprintLabel = "补打"

// ErrEmpty 表示没有可打印的标签。
var ErrEmpty = errors.New("没有可打印的标签")

// ErrTooMany 表示一张纸排不下这么多标签。
var ErrTooMany = errors.New("一张纸排不下这么多标签")

// Plan 是排版结果，同时是渲染输入与打印前预览的数据源。
type Plan struct {
	Layout   Layout    `json:"layout"`
	Slots    []Slot    `json:"slots"`
	Contents []Content `json:"contents"`
	// Job 是溯源信息，渲染时写入 PDF 元数据与标签小字。
	Job Job `json:"job"`
}

// Sheets 把任务按纸张容量切分，返回每张纸一份计划。
//
// 超出单张容量不是错误而是分页：用户点「打印 40 个」时期望拿到两张纸，
// 而不是一个「一次最多 30 个」的报错。
func (j Job) Sheets() ([]Plan, error) {
	if len(j.Contents) == 0 {
		return nil, ErrEmpty
	}
	layout, err := j.Layout.Normalize()
	if err != nil {
		return nil, err
	}
	perSheet := layout.Rows * layout.Cols
	var plans []Plan
	for start := 0; start < len(j.Contents); start += perSheet {
		end := start + perSheet
		if end > len(j.Contents) {
			end = len(j.Contents)
		}
		chunk := j.Contents[start:end]
		for i, c := range chunk {
			if err := c.Validate(); err != nil {
				return nil, fmt.Errorf("第 %d 个标签：%w", start+i+1, err)
			}
		}
		slots, err := layout.Slots(len(chunk))
		if err != nil {
			return nil, err
		}
		plans = append(plans, Plan{
			Layout:   layout,
			Slots:    slots,
			Contents: chunk,
			Job:      j,
		})
	}
	return plans, nil
}

// ReprintNote 返回该标签应印的补打小字，空字符串表示不是补打。
func (p Plan) ReprintNote(idx int) string {
	if p.Job.ReprintOf == "" {
		return ""
	}
	if p.Contents[idx].Note != "" {
		return p.Contents[idx].Note
	}
	label := p.Job.ReprintLabel
	if label == "" {
		label = DefaultReprintLabel
	}
	return label
}

// round2 把点数保留 2 位小数。
//
// 不保留更多：PDF 里的坐标最终落在打印机 300~600 dpi 上，0.01 pt 约
// 0.0004 mm，早已小于任何打印机的定位精度；保留更多位只是让文件更长。
func round2(v float64) float64 {
	return float64(int(v*100+sign(v)*0.5)) / 100
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
