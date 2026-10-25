package labels

import (
	"strings"
	"unicode"
)

// 文本度量与折行。
//
// 为什么用「估算宽度」而不是读取字体里的 advance width：
//
//   - 标签上的文字只有标题与几行短信息，字体是内置的固定字体，字号也只有
//     三档。估算的最大误差是几个百分点，而标签排版留了内边距，不足以让文字
//     溢出标签边缘。
//   - 读 advance width 需要解析 hmtx/cmap，等于为了折行把字体解析器再实现
//     一遍；本包已经需要解析 name/cmap/head/hhea 来做 PDF 嵌入，但 hmtx 只
//     用于折行估算，收益不成比例。
//
// 代价写清楚：如果以后换用字宽差异很大的字体，或者用户抱怨折行位置难看，
// 就在 Font.Metrics 里接上 hmtx——接口位置已经留好（glyphWidth 是唯一入口）。
//
// 宽度单位是「em 的千分之一」，与 TrueType unitsPerEm 同一量纲，方便换算。

// emWidth 返回一个字符的估算宽度（em 千分比）。
//
// 规则：
//   - 全角字符（CJK、全角标点）= 1000，即一个汉字宽。
//   - ASCII 窄字符（i l j . , ' ! 等）= 300。
//   - 其余 ASCII 与拉丁字母 = 560。
//   - 空白 = 300。
//   - 其他 unicode（emoji 等）= 1000，按最宽处理，宁可折行早也不溢出。
func emWidth(r rune) int32 {
	switch {
	case r == ' ' || r == '\t':
		return 300
	case r < 0x80:
		switch r {
		case 'i', 'l', 'j', 'I', '!', '.', ',', ';', ':', '\'', '|', '(', ')', '[', ']', 'f', 't', 'r':
			return 300
		default:
			return 560
		}
	case isFullWidth(r):
		return 1000
	default:
		// 希腊、西里尔、阿拉伯等：介于两者之间，按 580 处理。
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) ||
			unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			return 1000
		}
		return 580
	}
}

// isFullWidth 判断是否为全角区段（大致覆盖 CJK 与全角标点）。
func isFullWidth(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0x303E, // CJK 部首、假名标点
		r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF, // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF, // CJK 基本区
		r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE4F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角形式
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B 及以上
		return true
	}
	return false
}

// splitGraphemes 把字符串切成「不可再分的最小排版单位」。
//
// 这里不做完整的 grapheme cluster 分割（那需要 Unicode 表）；只处理本场景
// 真正会出现的两种组合：变音符号与 emoji 的 ZWJ 连接。标签内容来自用户
// 输入的物品名称，可能出现 emoji，切成半个 emoji 会印出乱码。
func splitGraphemes(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == 0x200D: // ZWJ：与前后合成一个单位
			cur = append(cur, r)
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
			// 组合记号：附到前一个单位上。
			cur = append(cur, r)
		case r == 0xFE0F || r == 0xFE0E: // 变体选择符
			cur = append(cur, r)
		default:
			flush()
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// measure 估算一串文本在给定字号下的宽度（点）。
func measure(s string, sizePt float64) float64 {
	total := int32(0)
	for _, g := range splitGraphemes(s) {
		total += graphemeWidth(g)
	}
	return float64(total) / 1000.0 * sizePt
}

// graphemeWidth 取一个排版单位的宽度：取其中最大的字符宽度。
//
// 组合记号宽度为 0，ZWJ 序列取最大宽度——一个 emoji 家族（4 个码点 + 3 个
// ZWJ）按一个字符宽度算，不会被折成两半。
func graphemeWidth(g string) int32 {
	max := int32(0)
	for _, r := range g {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) ||
			r == 0x200D || r == 0xFE0F || r == 0xFE0E {
			continue
		}
		if w := emWidth(r); w > max {
			max = w
		}
	}
	if max == 0 {
		max = emWidth(' ')
	}
	return max
}

// wrapText 把文本折成不超过 maxWidthPt 的行。
//
// 折行规则（中文与西文混合场景的常规做法）：
//   - CJK 可以在任意两字之间断开；
//   - 西文以空格为断点，不在单词中间断开（除非单个单词本身就超宽，
//     此时按字符硬断，否则会无限宽）；
//   - 行首不出现常见的禁则标点（。，）」』！？：；%），遇到时把它
//     留在上一行末，减少「标点跑到下一行开头」的突兀。
//
// maxLines 为 0 表示不限行数；超出时截断并在末尾加省略号。
func wrapText(s string, sizePt, maxWidthPt float64, maxLines int) []string {
	s = strings.TrimSpace(s)
	if s == "" || maxWidthPt <= 0 {
		return nil
	}
	// 先按显式换行拆段——用户输入里的换行是刻意的，不该被折行逻辑吃掉。
	var out []string
	for _, para := range strings.Split(s, "\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		out = append(out, wrapParagraph(para, sizePt, maxWidthPt)...)
	}
	if maxLines > 0 && len(out) > maxLines {
		out = out[:maxLines]
		if len(out) > 0 {
			out[len(out)-1] = ellipsize(out[len(out)-1], sizePt, maxWidthPt)
		}
	}
	return out
}

func wrapParagraph(s string, sizePt, maxWidthPt float64) []string {
	var lines []string
	var cur []string
	curWidth := 0.0
	flush := func() {
		if len(cur) > 0 {
			lines = append(lines, strings.Join(cur, ""))
			cur = nil
			curWidth = 0
		}
	}

	// 把段落切成 token：连续的非 CJK 非空格字符算一个西文词，其余按单字。
	tokens := tokenize(s)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		w := measure(tok, sizePt)

		if tok == " " {
			// 空格：只有在行内已有内容时才占位，避免行首缩进。
			if len(cur) == 0 {
				continue
			}
			cur = append(cur, tok)
			curWidth += w
			continue
		}

		// 禁则：标点不能出现在行首，把它并到上一行。
		if len(cur) == 0 && len(lines) > 0 && isNoLineStart(tok) {
			lines[len(lines)-1] += tok
			continue
		}

		if curWidth+w <= maxWidthPt {
			cur = append(cur, tok)
			curWidth += w
			continue
		}

		// 放不下：换行。
		if len(cur) > 0 {
			flush()
			// 换行后行首是标点，或当前 token 是空格，跳过。
			if isNoLineStart(tok) && len(lines) > 0 {
				lines[len(lines)-1] += tok
				continue
			}
			if tok == " " {
				continue
			}
		}
		// 单个 token 本身就超宽（长英文单词 / 长号码）：按字符硬断。
		if w > maxWidthPt {
			pieces := hardSplit(tok, sizePt, maxWidthPt)
			for j, p := range pieces {
				if j < len(pieces)-1 {
					lines = append(lines, p)
				} else {
					cur = []string{p}
					curWidth = measure(p, sizePt)
				}
			}
			continue
		}
		cur = append(cur, tok)
		curWidth = w
	}
	flush()
	return lines
}

// tokenize 把段落切成折行单位。
func tokenize(s string) []string {
	var out []string
	var word []string
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, strings.Join(word, ""))
			word = nil
		}
	}
	for _, g := range splitGraphemes(s) {
		if g == " " || g == "\t" {
			flushWord()
			out = append(out, " ")
			continue
		}
		// CJK 与全角标点各自成 token；西文字符连成词。
		if isBreakable(g) {
			flushWord()
			out = append(out, g)
			continue
		}
		word = append(word, g)
	}
	flushWord()
	return out
}

// isBreakable 判断该排版单位是否可以独立成为断点。
func isBreakable(g string) bool {
	for _, r := range g {
		switch {
		case r >= 0x2E80 && r <= 0x9FFF, // CJK
			r >= 0x3000 && r <= 0x303F, // CJK 标点
			r >= 0xFF00 && r <= 0xFF60, // 全角
			r >= 0xAC00 && r <= 0xD7A3,
			r >= 0x3040 && r <= 0x30FF: // 假名
			return true
		}
	}
	return false
}

// noLineStart 是常见的行首禁则标点。
var noLineStart = []string{
	"。", "，", "、", "）", "」", "』", "】", "》", "〉",
	"！", "？", "：", "；", "％", "%", ".", ",", ")",
	"”", "’", "!", "?", ":", ";",
}

func isNoLineStart(tok string) bool {
	for _, p := range noLineStart {
		if tok == p {
			return true
		}
	}
	return false
}

// hardSplit 把一个超宽 token 按字符切成多段，每段不超过 maxWidthPt。
func hardSplit(tok string, sizePt, maxWidthPt float64) []string {
	var out []string
	cur := ""
	curWidth := 0.0
	for _, g := range splitGraphemes(tok) {
		w := measure(g, sizePt)
		if curWidth+w > maxWidthPt && cur != "" {
			out = append(out, cur)
			cur = ""
			curWidth = 0
		}
		cur += g
		curWidth += w
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// ellipsizeLine 在末尾加省略号，保证加完之后仍不超宽。
func ellipsize(s string, sizePt, maxWidthPt float64) string {
	const dots = "…"
	dw := measure(dots, sizePt)
	if dw >= maxWidthPt {
		return ""
	}
	budget := maxWidthPt - dw
	var b strings.Builder
	w := 0.0
	for _, g := range splitGraphemes(s) {
		gw := measure(g, sizePt)
		if w+gw > budget {
			break
		}
		b.WriteString(g)
		w += gw
	}
	return strings.TrimRight(b.String(), " ") + dots
}
