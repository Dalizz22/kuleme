// Package kod 实现公开码、实例短码与二维码 payload。
//
// 公开码用 Crockford Base32，易于人工读取：排除 I、L、O、U，避免与 1、0 混淆。
// 实例短码固定 12 位，二维码 payload 约 30 字符，用更少数据量换取扫描成功率；
// 优化目标是数据量，不是打印尺寸。
package kod

import (
	"crypto/rand"
	"errors"
	"strings"
)

// Crockford Base32 字母表。数字在前，便于按键输入。
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// 长度约定。
const (
	PublicCodeLen = 10
	InstanceIDLen = 12
	// KindItem / KindSpace / KindLocation 是二维码中的对象类型。
	KindItem     = "I"
	KindSpace    = "S"
	KindLocation = "L"
)

// 二维码版本前缀。KLM1:<instance_id>:<kind>:<code>
const QRPrefix = "KLM1"

// ErrBadCode 表示输入不是合法的公开码。
var ErrBadCode = errors.New("编码格式不正确")

// NewPublicCode 生成一个 10 位公开码。调用方仍需数据库唯一约束兜底并在冲突时重试。
func NewPublicCode() (string, error) { return randomString(PublicCodeLen) }

// NewInstanceID 生成一个 12 位实例短码，不使用 UUID：二维码要控制数据量。
func NewInstanceID() (string, error) { return randomString(InstanceIDLen) }

func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		// 32 是字母表长度，直接取模会有极小的取模偏差；256/32 恰好整除，无偏差。
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// Normalize 把用户输入规整为规范形式：去空白、转大写，并把常见误输字符
// （I/L → 1，O → 0）还原，减少人工抄码失败。
func Normalize(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range code {
		switch r {
		case 'I', 'L':
			b.WriteByte('1')
		case 'O':
			b.WriteByte('0')
		case '-', ' ':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidPublicCode 校验规范化后的公开码。
func ValidPublicCode(code string) bool {
	return validFor(code, PublicCodeLen)
}

// ValidInstanceID 校验规范化后的实例短码。
func ValidInstanceID(code string) bool {
	return validFor(code, InstanceIDLen)
}

func validFor(code string, length int) bool {
	if len(code) != length {
		return false
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(alphabet, rune(code[i])) {
			return false
		}
	}
	return true
}

// QRPayload 是二维码承载的内容。它是标识，不是访问凭证。
type QRPayload struct {
	InstanceID string
	Kind       string
	Code       string
}

// Encode 生成二维码文本：KLM1:<instance_id>:<kind>:<code>。
func (p QRPayload) Encode() string {
	return QRPrefix + ":" + p.InstanceID + ":" + p.Kind + ":" + p.Code
}

// ParseQR 解析二维码文本。格式合法但类型未知时返回 ErrBadCode，
// 调用方据此提示「无法识别的库了么码」；完全不像二维码的文本按普通搜索处理。
func ParseQR(raw string) (QRPayload, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 4 {
		return QRPayload{}, ErrBadCode
	}
	if strings.ToUpper(parts[0]) != QRPrefix {
		return QRPayload{}, ErrBadCode
	}
	p := QRPayload{
		InstanceID: Normalize(parts[1]),
		Kind:       strings.ToUpper(parts[2]),
		Code:       Normalize(parts[3]),
	}
	if !ValidInstanceID(p.InstanceID) {
		return QRPayload{}, ErrBadCode
	}
	switch p.Kind {
	case KindItem, KindSpace:
		if !ValidPublicCode(p.Code) {
			return QRPayload{}, ErrBadCode
		}
	case KindLocation:
		// 位置码与公开码同长度同字符集。
		if !ValidPublicCode(p.Code) {
			return QRPayload{}, ErrBadCode
		}
	default:
		return QRPayload{}, ErrBadCode
	}
	return p, nil
}

// LooksLikeQR 判断文本是否声明为库了么码，用于区分「坏码」与「普通搜索词」。
func LooksLikeQR(raw string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(raw)), QRPrefix+":")
}
