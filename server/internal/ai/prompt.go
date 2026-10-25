package ai

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 本文件负责「提示词」与「模型返回文本的解析」。
//
// 唯一不变式：**模型返回的内容只是数据**。这里做的事只有三件——
// 从文本里找出 JSON、按结构解码、把字段名限制在白名单里。
// 没有一次 strings.Contains 被当作分支条件去改变系统行为，
// 也没有任何内容被回填进下一次请求。

// buildPrompt 生成识别提示词。
//
// 提示词里只出现 field_id、展示名、类型；不含默认值、不含枚举全集、
// 不含任何业务校验规则——那些留在服务端，模型猜不出来就不该猜。
func buildPrompt(req RecognizeRequest) string {
	var b strings.Builder
	switch req.Mode {
	case ModeSplit:
		b.WriteString("你是库存录入助手。用户可能拍了一张照片，照片里有多件不同物品（例如一箱东西摊开拍的一张）。\n")
		b.WriteString("请把照片里每一件可以单独记录、单独给别人看的物品各列一条。同一件物品的不同角度不要重复列。\n")
	default:
		b.WriteString("你是库存录入助手。用户拍的是同一件物品的若干张照片（正面、背面、铭牌、包装等）。\n")
		b.WriteString("请把这组照片理解为**一件**物品，输出恰好一条记录。\n")
	}
	if req.ObjectHint != "" {
		b.WriteString("用户补充说明：")
		b.WriteString(trimTo(req.ObjectHint, 200))
		b.WriteString("\n")
	}
	if len(req.FieldHints) > 0 {
		b.WriteString("\n可以填写的字段（键名必须原样使用）：\n")
		hints := append([]FieldHint(nil), req.FieldHints...)
		sort.SliceStable(hints, func(i, j int) bool { return hints[i].FieldID < hints[j].FieldID })
		for _, h := range hints {
			b.WriteString("- ")
			b.WriteString(h.FieldID)
			b.WriteString("（")
			b.WriteString(h.Label)
			b.WriteString("，类型 ")
			b.WriteString(h.Type)
			b.WriteString("）\n")
		}
		b.WriteString("字段值填不出来就留空，不要编造；不确定的写进 notes。\n")
	}
	b.WriteString("\n只输出一个 JSON 对象，不要 markdown 代码块、不要解释文字。结构：\n")
	b.WriteString(`{"observations":[{"name":"名称","summary":"一句话概括","notes":"铭牌或包装上的原文","evidence":"你据以判断的特征","confidence":0.0,"fields":{"field_id":"值"}}]}`)
	b.WriteString("\n")
	max := req.MaxCandidates
	if max <= 0 {
		max = 1
	}
	fmt.Fprintf(&b, "最多输出 %d 条。一件物品都认不出来时输出 {\"observations\":[]}。\n", max)
	return b.String()
}

// parseChatResponse 从兼容接口的响应里取出观察列表。
func parseChatResponse(raw []byte, cfg Settings) (RecognizeResponse, error) {
	var resp chatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return RecognizeResponse{}, fmt.Errorf("%w: 响应不是合法 JSON", ErrProviderFailed)
	}
	if resp.Error != nil {
		return RecognizeResponse{}, fmt.Errorf("%w: %s", ErrProviderFailed,
			sanitizeText(resp.Error.Message, cfg))
	}
	if len(resp.Choices) == 0 {
		return RecognizeResponse{}, fmt.Errorf("%w: 响应里没有 choices", ErrProviderFailed)
	}
	content := resp.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		// 空内容是「模型什么都没说」。按空结果处理而不是失败：
		// 用户看到的是「没识别出物品」，可以重拍，而不是一个吓人的错误。
		return RecognizeResponse{Model: resp.Model, Raw: json.RawMessage(raw)}, nil
	}
	obs, err := parseObservations(content)
	if err != nil {
		return RecognizeResponse{}, err
	}
	return RecognizeResponse{Observations: obs, Model: resp.Model, Raw: json.RawMessage(raw)}, nil
}

// observationEnvelope 是模型应该输出的顶层结构。
//
// 同时接受 observations / items 两个键名：不同模型对「列表」的命名习惯不同，
// 而这两者语义完全一致，拒绝其中一个只会带来无谓的失败重试。
type observationEnvelope struct {
	Observations []json.RawMessage `json:"observations"`
	Items        []json.RawMessage `json:"items"`
}

// parseObservations 从模型文本里解析观察列表。
func parseObservations(text string) ([]RawObservation, error) {
	payload, ok := extractJSONObject(text)
	if !ok {
		return nil, fmt.Errorf("%w: 模型没有返回可解析的 JSON", ErrProviderFailed)
	}
	var env observationEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		// 也可能直接就是一个数组。
		var arr []json.RawMessage
		if err2 := json.Unmarshal(payload, &arr); err2 == nil {
			env.Observations = arr
		} else {
			return nil, fmt.Errorf("%w: 模型返回的 JSON 结构不认识", ErrProviderFailed)
		}
	}
	list := env.Observations
	if len(list) == 0 {
		list = env.Items
	}
	out := make([]RawObservation, 0, len(list))
	for _, raw := range list {
		obs, err := parseObservation(raw)
		if err != nil {
			// 单条坏掉不该毁掉整次识别：跳过它并继续，
			// 剩下的候选仍然对用户有价值。
			continue
		}
		out = append(out, obs)
	}
	return out, nil
}

// parseObservation 解析一条观察，字段名只认白名单。
//
// 未知字段直接丢弃（不解码、不保留）：模型或网关多塞一个字段
// 不该变成系统里的一条数据。
func parseObservation(raw json.RawMessage) (RawObservation, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return RawObservation{}, err
	}
	var obs RawObservation
	obs.Name = rawString(m["name"])
	obs.Summary = rawString(m["summary"])
	obs.Notes = rawString(m["notes"])
	obs.Evidence = rawString(m["evidence"])
	obs.Confidence = decodeConfidence(m["confidence"])
	if fields, ok := m["fields"]; ok {
		parsed, err := decodeStringMap(fields)
		if err == nil {
			obs.Fields = parsed
		}
	}
	obs.Name = trimTo(strings.TrimSpace(obs.Name), 200)
	obs.Summary = trimTo(strings.TrimSpace(obs.Summary), 500)
	obs.Notes = trimTo(strings.TrimSpace(obs.Notes), 1000)
	obs.Evidence = trimTo(strings.TrimSpace(obs.Evidence), 1000)
	if obs.Name == "" && len(obs.Fields) == 0 {
		return RawObservation{}, fmt.Errorf("观察既没有名称也没有字段")
	}
	return obs, nil
}

// rawString 把 JSON 值转成字符串；数字与布尔也接受。
//
// 模型经常把 serial_no 里的数字按数字类型返回（"123" vs 123），
// 这里统一成字符串，后续由模板的字段类型决定怎么解释。
func rawString(raw json.RawMessage) string {
	raw = trimSpaceJSON(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// decodeConfidence 解析置信度，限制到 [0,1]。
//
// 模型有时给 0.9，有时给 90，有时给 "高"。统一收敛：
// 认不出来就当作 0（也就是「不说」），不让它影响流程。
func decodeConfidence(raw json.RawMessage) float64 {
	raw = trimSpaceJSON(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0
	}
	if f > 1 {
		f /= 100
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// maxExtractBytes 是解析前的文本上限。
//
// 带 markdown 说明的模型回答很少超过这个数；限制它的目的是让
// 「返回一兆字节废话」的端点无法把 worker 拖住。
const maxExtractBytes = 256 << 10

// extractJSONObject 从可能带包装的文本里取出第一个完整 JSON 对象或数组。
//
// 用括号配对扫描而不是正则：模型会在 JSON 前后加说明文字、markdown 代码块、
// 甚至示例，正则要么匹配到示例里的花括号，要么跨不过嵌套对象。
func extractJSONObject(text string) (json.RawMessage, bool) {
	if len(text) > maxExtractBytes {
		text = text[:maxExtractBytes]
	}
	text = stripFences(text)
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		if end := balancedEnd(trimmed); end > 0 {
			return json.RawMessage(trimmed[:end]), true
		}
	}
	// 兜底：在正文里找第一个能配对完的 { 或 [。
	start := -1
	for i, r := range trimmed {
		if r == '{' || r == '[' {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, false
	}
	if end := balancedEnd(trimmed[start:]); end > 0 {
		return json.RawMessage(trimmed[start : start+end]), true
	}
	return nil, false
}

// stripFences 去掉 markdown 代码块围栏。
func stripFences(s string) string {
	if !strings.Contains(s, "```") {
		return s
	}
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		// 围栏不成对（模型只给了一个开头），退回去人工处理。
		return strings.ReplaceAll(s, "```", "")
	}
	return b.String()
}

// balancedEnd 返回从头开始第一个配对完整的 JSON 值的字节长度；找不到返回 0。
//
// 会跳过字符串内部的括号与转义，因此 JSON 里的 "}}" 不会提前收尾。
func balancedEnd(s string) int {
	// 用栈记录每层期待的反括号：对象套数组（{"observations":[...]}）是模型
	// 最常见的输出形态，只记一个期望括号会把里层的 ] 当成不匹配。
	stack := make([]byte, 0, 8)
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				// 括号种类不匹配：不是我们要找的那个值。
				return 0
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1
			}
		}
	}
	return 0
}
