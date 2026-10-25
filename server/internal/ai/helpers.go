package ai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/klm/server/internal/labels/kod"
)

// 本文件放几处薄封装。都是「本来可以各写一份」的东西，集中在这里是为了
// 让 AI 模块与标签模块对同一件事只有一种理解。

// base64Std / base64StdDecode 是标准 Base64（带填充）。
//
// 不用 URL 安全变体：密文存在配置文件里，不走路径或查询串。
func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func base64StdDecode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// newInstanceID / validInstanceID 复用标签模块的 Crockford 实现。
//
// 实例码同时出现在二维码载荷（labels 用）与 AI 任务上下文里，
// 两处必须完全同源：AI 侧另写一份字母表迟早会漂移出一个扫不出的码。
func newInstanceID() (string, error) { return kod.NewInstanceID() }
func validInstanceID(id string) bool { return kod.ValidInstanceID(id) }

// VerifyPublicCode 校验一个公开码是否是本实例能签发的形态。
//
// 物品创建由核心服务负责，它拿到公开码之后可以用这个函数自检，
// 避免把 11 位的、带 I/L/O/U 的码写进库——那种码印在标签上扫不出来。
func VerifyPublicCode(code string) (string, error) {
	norm := kod.Normalize(code)
	if !kod.ValidPublicCode(norm) {
		return "", fmt.Errorf("%w: 公开码 %q 不是 %d 位 Crockford Base32",
			kod.ErrBadCode, code, kod.PublicCodeLen)
	}
	return norm, nil
}

// --- 小工具 ---

// clampInt 把 n 限制在 [lo, hi]。
func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// trimTo 按 rune 截断，超长时加省略号。
//
// 按 rune 而不是按字节：中文字符按字节截断会切出半个字，
// 在 JSON 里变成替换字符，还会让长度限制看起来时灵时不灵。
func trimTo(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	if max <= 1 {
		return string(rs[:max])
	}
	return string(rs[:max-1]) + "…"
}

// decodeStringMap 解析模型可能给出的各种「键值对」形态。
//
// 模型输出的字段值形态没法完全控制：可能是对象，可能是 JSON 字符串，
// 也可能把整个 observations 包成一个字符串。这里统一收敛，
// 但**只做形状解析，不执行任何内容**——解析结果随后必须过模板校验。
func decodeStringMap(raw json.RawMessage) (map[string]any, error) {
	raw = trimSpaceJSON(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	if raw[0] == '{' {
		// 可能是 {"fields": {...}} 也可能是纯字段对象。
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil {
			return nil, err
		}
		if inner, ok := outer["fields"]; ok && len(outer) == 1 {
			return decodeStringMap(inner)
		}
		out := make(map[string]any, len(outer))
		for k, v := range outer {
			var anyV any
			if err := json.Unmarshal(v, &anyV); err != nil {
				continue
			}
			out[k] = anyV
		}
		return out, nil
	}
	if raw[0] == '"' {
		// 字符串里再嵌一层 JSON：模型常见输出。
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return map[string]any{}, nil
		}
		return decodeStringMap(json.RawMessage(s))
	}
	return nil, errors.New("不是对象形态")
}

func trimSpaceJSON(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}
