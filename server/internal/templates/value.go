package templates

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 比较值里允许出现的日期占位符。除这一形式外，任何以 __ 开头的值都会在校验时
// 报错，避免把占位符机制做成一个半吊子表达式引擎。
var placeholderRe = regexp.MustCompile(`^__(today|today\+\d+d)__$`)

// ResolvePlaceholder 把 __today__ / __today+<n>d__ 解析为 YYYY-MM-DD。
//
// 不含占位符的输入原样返回。当前时间由调用方传入（服务端按实例时区取），
// 这里只做纯计算，便于测试与复现。
func ResolvePlaceholder(s string, now time.Time) (string, error) {
	if !strings.HasPrefix(s, "__") {
		return s, nil
	}
	if s == "__today__" {
		return now.Format(time.DateOnly), nil
	}
	const pre, suf = "__today+", "d__"
	if strings.HasPrefix(s, pre) && strings.HasSuffix(s, suf) {
		num := s[len(pre) : len(s)-len(suf)]
		n, err := strconv.Atoi(num)
		if err != nil || n < 0 || n > 3650 {
			return "", fmt.Errorf("占位符 %q 的天数必须是 0..3650 的整数", s)
		}
		return now.AddDate(0, 0, n).Format(time.DateOnly), nil
	}
	return "", fmt.Errorf("占位符 %q 不受支持，只能用 __today__ 或 __today+<n>d__", s)
}

// --- 精确十进制 ---

// Decimal 是定点十进制数：未缩放整数 + 小数位数。
//
// 数量、金额一律走它，不用 float64：契约要求金额与数量是十进制字符串，
// 而 0.1+0.2 这类误差在库存累加里会变成「差一个」的幽灵库存。
type Decimal struct {
	// Units 是未缩放整数（真实值 = Units / 10^Scale）。
	Units int64
	// Scale 是小数位数。
	Scale int
}

// ParseDecimal 解析十进制字符串，最多允许 maxScale 位小数。
//
// 小数位超出时返回错误而不是静默截断：被悄悄改小的金额比一条报错危险得多。
func ParseDecimal(raw string, maxScale int) (Decimal, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Decimal{}, fmt.Errorf("空值不是有效数字")
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return Decimal{}, fmt.Errorf("空值不是有效数字")
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if !allDigits(intPart) || (fracPart != "" && !allDigits(fracPart)) {
		return Decimal{}, fmt.Errorf("%q 不是有效数字", raw)
	}
	if len(fracPart) > maxScale {
		return Decimal{}, fmt.Errorf("%q 小数位超过 %d 位", raw, maxScale)
	}
	units, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return Decimal{}, fmt.Errorf("%q 超出可表示范围", raw)
	}
	if neg {
		units = -units
	}
	return Decimal{Units: units, Scale: len(fracPart)}, nil
}

// String 输出十进制字符串：至少 minScale 位小数，超出有效精度的尾随 0 去掉。
//
// minScale 让「数量 1」在两位精度的字段里显示为 "1.00"，避免同一条记录
// 在不同端显示成不同形态。
func (d Decimal) String(minScale int) string {
	scale := minScale
	if d.Scale > scale {
		scale = d.Scale
	}
	units := d.Units
	if d.Scale < scale {
		// 低位存储的值重缩放到目标 scale：12（scale 0）→ 1200（scale 2），
		// 否则会左补零变成 0.12。
		units *= pow10(scale - d.Scale)
	}
	neg := units < 0
	if neg {
		units = -units
	}
	digits := strconv.FormatInt(units, 10)
	for len(digits) <= scale {
		digits = "0" + digits
	}
	intPart := digits[:len(digits)-scale]
	fracPart := digits[len(digits)-scale:]
	// 去掉超出有效精度的尾随 0（例如已降位存储的 1.10 → 1.1），
	// 但至少保留 minScale 位。
	for len(fracPart) > minScale && strings.HasSuffix(fracPart, "0") {
		fracPart = fracPart[:len(fracPart)-1]
	}
	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	if neg && strings.Trim(out, "0.") != "" {
		out = "-" + out
	}
	return out
}

// Float 仅供比较大小使用，不用于输出或累加。
func (d Decimal) Float() float64 { return float64(d.Units) / math.Pow10(d.Scale) }

// Cmp 比较两个十进制数，自动对齐 scale。
func (d Decimal) Cmp(o Decimal) int {
	a, b := d, o
	if a.Scale < b.Scale {
		a.Units *= pow10(b.Scale - a.Scale)
	} else if b.Scale < a.Scale {
		b.Units *= pow10(a.Scale - b.Scale)
	}
	switch {
	case a.Units < b.Units:
		return -1
	case a.Units > b.Units:
		return 1
	default:
		return 0
	}
}

func pow10(n int) int64 {
	out := int64(1)
	for i := 0; i < n; i++ {
		out *= 10
	}
	return out
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// numericScaleAtLeast 返回字段允许的小数位数下限：
// 金额 2 位、数量按单位精度（未声明时 2 位）。
func numericScaleAtLeast(f Field) int {
	if f.Precision != nil {
		return *f.Precision
	}
	if f.Unit == UnitPercent {
		return 2
	}
	return 2
}

// --- 字段值归一化 ---

// normalizeValue 把输入值转换为按类型规范化的可存储形式。
//
// 返回 nil 表示「清空该字段」。任何无法安全转换的输入都返回错误，不做猜测式
// 兜底：猜错的值会静默污染库存与账目，比一条明确报错贵得多。
func normalizeValue(f Field, in any) (any, error) {
	if in == nil {
		return nil, nil
	}
	switch f.Type {
	case TypeText, TypeTextarea:
		s, err := asString(in)
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		if f.MaxLen > 0 && len([]rune(s)) > f.MaxLen {
			return nil, fmt.Errorf("最多 %d 个字符", f.MaxLen)
		}
		return s, nil

	case TypeNumber, TypeMoney:
		scale := numericScaleAtLeast(f)
		s, err := asString(in)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		d, err := ParseDecimal(s, scale)
		if err != nil {
			return nil, err
		}
		return d.String(scale), nil

	case TypeDate:
		s, err := asString(in)
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		t, err := parseDate(s)
		if err != nil {
			return nil, err
		}
		return t.Format(time.DateOnly), nil

	case TypeBool:
		switch v := in.(type) {
		case bool:
			return v, nil
		case string:
			b, ok := parseBoolString(v)
			if !ok {
				return nil, fmt.Errorf("%q 不是有效的布尔值", v)
			}
			return b, nil
		default:
			if n, ok := toFloat(in); ok {
				return n != 0, nil
			}
			return nil, fmt.Errorf("不是有效的布尔值")
		}

	case TypeEnum:
		s, err := asString(in)
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		for _, opt := range f.Options {
			if opt == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%q 不在允许的选项中（%s）", s, strings.Join(f.Options, "、"))
	}
	return nil, fmt.Errorf("字段类型 %q 不受支持", f.Type)
}

// dateLayouts 是接受的日期输入形式。导入表格里 2024/1/5、2024.1.5、20240105
// 都很常见，统一在这里归一为 YYYY-MM-DD；其余形式明确报错，不猜。
var dateLayouts = []string{
	time.DateOnly,
	"2006-1-2",
	"2006/1/2",
	"2006.1.2",
	"20060102",
	"2006年1月2日",
}

func parseDate(s string) (time.Time, error) {
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q 不是有效日期，请用 YYYY-MM-DD", s)
}

func parseBoolString(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "y", "是", "有":
		return true, true
	case "false", "0", "no", "n", "否", "无":
		return false, true
	}
	return false, false
}

// asString 把标量转成字符串。JSON 数字不经 float64 中转：用 json.Number
// 保留原始十进制字面量，避免 1.10 变成 "1.1"、大整数丢精度。
func asString(in any) (string, error) {
	switch v := in.(type) {
	case string:
		return v, nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		return v.String(), nil
	case float64:
		// 调用方（例如 csv 解析）已经解析成 float64 时保留最短表示。
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	}
	return "", fmt.Errorf("值类型 %T 不能作为字段值", in)
}

func toFloat(in any) (float64, bool) {
	switch v := in.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

// SortFieldIDs 返回排序后的字段标识，供稳定输出与 diff 使用。
func SortFieldIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}
