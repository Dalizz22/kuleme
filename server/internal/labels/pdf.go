package labels

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/klm/server/internal/labels/qrcode"
)

// PDF 生成：排版逻辑自己写（renderLabel/drawQR），文档序列化交给 go-pdf/fpdf。
//
// 依赖取舍：曾经这里手写了整套 PDF 1.4 序列化（xref、CIDFontType2、ToUnicode
// CMap、Flate 内容流），连 TTF 解析都是手写的。手写子集化与 CMap 的出错面太大
// （能打开但字形错乱，静态评审看不出来），fPDF 系的 UTF-8 字体支持把嵌入、
// 子集化、ToUnicode 一次做对，于是只保留排版，序列化换库。
// 附带收益：fpdf 按用到的字形子集化字体，PDF 不再整份嵌入十几 MB 的中文字体。

const (
	pdfLineWidth  = 0.6  // 标签边框与描边线宽（pt）
	titleRatio    = 1.0  // 标题字号 = 基准字号
	subtitleRatio = 0.72 // 副标题字号比
	lineRatio     = 0.62 // 信息行字号比
	noteRatio     = 0.5  // 补打小字字号比
	lineLeading   = 1.25 // 行距倍数
	minFontPt     = 4.0  // 最小字号：再小打印机也印不清
	baselineRatio = 0.28 // 基线在行框内的下移比例（近似 ascent 修正）

	pdfFontFamily = "klm" // 嵌入字体的内部名
)

// ErrNoFont 表示没有配置可嵌入的字体。
//
// 这是硬错误而非降级：标签里有中文，用 PDF 标准字体名会印成空白纸。
// 静默产出空白比报错糟糕得多——用户只有在撕下标签贴上之后才会发现。
var ErrNoFont = errors.New("未配置标签字体：中文标签必须嵌入字体，否则会印成空白")

// RendererOptions 控制渲染细节。
type RendererOptions struct {
	// FontBytes 是嵌入的 TTF 字体文件字节，必填。fpdf 不支持 .ttc 集合字体。
	FontBytes []byte
	// BaseSizePt 是基准字号（pt）。为 0 时按标签高度自动推算。
	BaseSizePt float64
	// DrawBorder 为 true 时给每个标签画一圈边框。
	//
	// A4 不干胶标签纸通常不需要（纸本身有切缝），但自定义尺寸或印在普通纸上
	// 时，边框是唯一的对齐参照。
	DrawBorder bool
}

// RenderPDF 把若干张纸的排版计划渲染成一份多页 PDF。
//
// 返回的字节同时是「可下载的文件」和「发给设备的内容」：两者共用同一份字节，
// 保证预览看到什么就印出什么。写文件与发送设备由调用方决定，本包不碰 IO。
func RenderPDF(plans []Plan, opts RendererOptions) ([]byte, error) {
	if len(opts.FontBytes) == 0 {
		return nil, ErrNoFont
	}
	if len(plans) == 0 {
		return nil, ErrEmpty
	}
	for i, plan := range plans {
		if len(plan.Contents) == 0 {
			return nil, fmt.Errorf("第 %d 张纸没有标签: %w", i+1, ErrEmpty)
		}
		if len(plan.Slots) < len(plan.Contents) {
			return nil, fmt.Errorf("第 %d 张纸格子数 %d 少于标签数 %d",
				i+1, len(plan.Slots), len(plan.Contents))
		}
	}

	// fpdf.New 的 sizeStr 不支持自定义尺寸；每页都经 AddPageFormat 给出。
	pdf := fpdf.New("P", "pt", "", "")
	pdf.SetMargins(0, 0, 0)
	pdf.SetAutoPageBreak(false, 0)
	// AddUTF8FontFromBytes 不返回错误：字体坏不在这里炸，在最后的 Output 里炸。
	pdf.AddUTF8FontFromBytes(pdfFontFamily, "", opts.FontBytes)
	pdf.SetTitle("库了么标签", true)
	pdf.SetCreator("库了么", true)
	pdf.SetProducer("库了么", true)
	pdf.SetCreationDate(time.Now())

	w := &sheetWriter{pdf: pdf}
	for _, plan := range plans {
		layout, err := plan.Layout.Normalize()
		if err != nil {
			return nil, err
		}
		base := opts.BaseSizePt
		if base <= 0 {
			// 经验公式：基准字号约为标签高度的 9%，但不小于内边距，
			// 也不小于可印刷的最小字号。
			base = layout.LabelHeightMM * mm * 0.09
			if pad := layout.PaddingMM * mm; base < pad {
				base = pad
			}
			if base < minFontPt {
				base = minFontPt
			}
		}
		pdf.AddPageFormat("P", fpdf.SizeType{
			Wd: layout.PaperWidthMM * mm, Ht: layout.PaperHeightMM * mm,
		})
		w.pageH = layout.PaperHeightMM * mm
		for i := range plan.Contents {
			renderLabel(w, plan, i, base, opts.DrawBorder)
		}
	}
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("生成 PDF: %w", err)
	}
	return buf.Bytes(), nil
}

// sheetWriter 把排版层的绘制原语翻到 fpdf 上。
//
// 排版数学沿用 PDF 原生坐标系（左下原点、y 向上），fpdf 是左上原点、y 向下，
// 所以每个原语在这里做 y = pageH - y 的翻转，renderLabel/drawQR 一行不改。
type sheetWriter struct {
	pdf   *fpdf.Fpdf
	pageH float64
	// curSize 记录当前字号，避免每行文字都重发一次 Tf。
	curSize float64
}

// text 在 (x, y) 处画一行文字，y 是基线（与原实现一致）。
func (w *sheetWriter) text(s string, x, y, sizePt float64) {
	if w.curSize != sizePt {
		w.pdf.SetFontSize(sizePt)
		w.curSize = sizePt
	}
	w.pdf.Text(x, w.pageH-y, s)
}

func (w *sheetWriter) fillRect(x, y, rw, rh float64) {
	w.pdf.Rect(x, w.pageH-y-rh, rw, rh, "F")
}

func (w *sheetWriter) strokeRect(x, y, rw, rh float64) {
	w.pdf.SetLineWidth(pdfLineWidth)
	w.pdf.Rect(x, w.pageH-y-rh, rw, rh, "D")
}

// renderLabel 在指定格子里画一个标签。
func renderLabel(d *sheetWriter, plan Plan, idx int, base float64, border bool) {
	slot := plan.Slots[idx]
	c := plan.Contents[idx]
	pad := plan.Layout.PaddingMM * mm

	if border {
		d.strokeRect(slot.X+pad/2, slot.Y+pad/2, slot.W-pad, slot.H-pad)
	}

	innerX := slot.X + pad
	innerY := slot.Y + pad
	innerW := slot.W - pad*2
	innerH := slot.H - pad*2
	if innerW <= 0 || innerH <= 0 {
		return
	}

	qrSize := plan.Layout.QRSizeMM * mm
	if qrSize <= 0 || qrSize > innerH {
		qrSize = innerH
	}
	// 架构约定：二维码不超过标签可打印区域的一半，且 A4 多格按格约束。
	if maxQR := innerH / 2; qrSize > maxQR {
		qrSize = maxQR
	}

	textX := innerX + qrSize + pad
	textW := innerW - qrSize - pad
	if textW < base*2 {
		// 标签太窄放不下字：把二维码压到 40% 宽，让文字有位置。
		qrSize = innerW * 0.4
		if qrSize > innerH/2 {
			qrSize = innerH / 2
		}
		textX = innerX + qrSize + pad/2
		textW = innerW - qrSize - pad/2
	}
	qrY := innerY + (innerH-qrSize)/2
	drawQR(d, c.Payload, innerX, qrY, qrSize)

	titleSize := clampSize(base * titleRatio)
	subtitleSize := clampSize(base * subtitleRatio)
	infoSize := clampSize(base * lineRatio)
	noteSize := clampSize(base * noteRatio)

	type row struct {
		text string
		size float64
	}
	var rows []row
	add := func(text string, size float64, maxLines int) {
		for _, l := range wrapText(text, size, textW, maxLines) {
			rows = append(rows, row{text: l, size: size})
		}
	}
	add(c.Title, titleSize, 2)
	add(c.Subtitle, subtitleSize, 1)

	note := plan.ReprintNote(idx)
	noteH := 0.0
	if note != "" {
		noteH = noteSize * lineLeading
	}

	usedH := 0.0
	for _, r := range rows {
		usedH += r.size * lineLeading
	}
	avail := innerH - usedH - noteH
	budget := int(avail / (infoSize * lineLeading))
	if budget < 0 {
		budget = 0
	}
	added := 0
	for _, raw := range c.Lines {
		if added >= budget {
			break
		}
		// 每个信息行只占一行：标签上多行折行的信息不如省下空间给下一项。
		wrapped := wrapText(raw, infoSize, textW, 1)
		if len(wrapped) == 0 {
			continue
		}
		rows = append(rows, row{text: wrapped[0], size: infoSize})
		added++
	}

	totalH := noteH
	for _, r := range rows {
		totalH += r.size * lineLeading
	}
	y := slot.Y + (slot.H+totalH)/2
	for _, r := range rows {
		y -= r.size * lineLeading
		d.text(r.text, textX, y+r.size*(1-baselineRatio), r.size)
	}
	if note != "" {
		y -= noteSize * lineLeading
		d.text(note, textX, y+noteSize*(1-baselineRatio), noteSize)
	}
}

func clampSize(pt float64) float64 {
	if pt < minFontPt {
		return minFontPt
	}
	return pt
}

// QuietZoneModules 是 QR 标准要求的静区宽度（模块数）。
const QuietZoneModules = 4

// QuietZoneMM 返回给定二维码物理边长下应有的静区宽度（mm）。
//
// 导出它是为了让版面校验能回答「我的 PaddingMM 够不够当静区」。少于这个值
// 时光学扫描会偶发读不出，而静区不足是最容易在自研渲染里被忽略的一项。
func QuietZoneMM(qrSizeMM float64, version int) float64 {
	if version <= 0 {
		// 版本未知时按本实现支持的最大版本 4（33 模块）估算，最保守。
		version = 4
	}
	modules := float64(version*4 + 17)
	if modules <= 0 || qrSizeMM <= 0 {
		return 0
	}
	return qrSizeMM / modules * QuietZoneModules
}

// drawQR 把二维码画成一组填充矩形。
//
// 静区：qrcode.Code 的模块矩阵不含静区，这里靠版面的内边距充当静区
// （默认 1.5 mm，在 20 mm 码上约为 1.9 个模块）。调用方必须用
// QuietZoneMM 校验内边距；A4DefaultLayout 的内边距满足 4 模块要求需要
// 码宽 ≥ 24 mm，自定义小标签请显式调大 PaddingMM。
// 不额外画白底：画白底会在彩色标签纸上盖出一个显眼的白框。
func drawQR(d *sheetWriter, payload string, x, y, size float64) {
	code, err := qrcode.EncodeAny(payload)
	if err != nil {
		// 内容不合法时画一个空框而不是让整页失败：同一张纸上其余标签照常
		// 打印，用户能一眼看出哪一张有问题。Content.Validate 已在排版阶段
		// 拦过一次，走到这里只可能是内容被并发改过。
		d.strokeRect(x, y, size, size)
		return
	}
	module := size / float64(code.Size)
	for rowIdx := 0; rowIdx < code.Size; rowIdx++ {
		for col := 0; col < code.Size; col++ {
			if !code.Dark(rowIdx, col) {
				continue
			}
			// 排版层 y 轴向上，矩阵第 0 行在最上面。
			mx := x + float64(col)*module
			my := y + size - float64(rowIdx+1)*module
			d.fillRect(mx, my, module, module)
		}
	}
}
