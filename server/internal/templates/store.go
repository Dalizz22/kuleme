package templates

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// CustomFields 是 Item.custom_fields 这一 JSONB 列的读写封装。
//
// 分层约定：
//
//   - FieldSchemaVersion 记录写入这些值时的模板 schema 版本。版本变化时
//     可以定位「哪些记录还是旧语义」，而不是让新旧值混在一起无法区分。
//   - Values 是「字段标识 → 值」。未知字段标识一律原样保留：模板被换掉、
//     插件被停用或未安装时，普通资料页仍能只读展示已保存的名称与值。
//   - 不在这里做类型强转。展示端遇到不认识的类型时只读展示原始值即可，
//     在这里猜测类型会让「换个模板」变成一次数据改写。
type CustomFields struct {
	FieldSchemaVersion int            `json:"field_schema_version"`
	Values             map[string]any `json:"values"`
}

// NewCustomFields 构造空的附加字段容器。
func NewCustomFields(schemaVersion int) CustomFields {
	return CustomFields{FieldSchemaVersion: schemaVersion, Values: map[string]any{}}
}

// DecodeCustomFields 解析 JSONB 字节。
//
// 接受三种历史形态，避免一次格式调整就丢历史资料：
//  1. 无字段：nil / 空字节 → 空容器。
//  2. 只存值：{"series":"A"} → 值直接作为 Values。
//  3. 带版本：{"field_schema_version":1,"values":{...}}。
func DecodeCustomFields(raw []byte) (CustomFields, error) {
	out := NewCustomFields(SchemaVersion)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return out, nil
	}
	// 先按带版本形态尝试。
	var probe struct {
		FieldSchemaVersion *int           `json:"field_schema_version"`
		Values             map[string]any `json:"values"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&probe); err != nil {
		return CustomFields{}, fmt.Errorf("解析 custom_fields: %w", err)
	}
	if probe.FieldSchemaVersion != nil || probe.Values != nil {
		out.Values = probe.Values
		if out.Values == nil {
			out.Values = map[string]any{}
		}
		if probe.FieldSchemaVersion != nil {
			out.FieldSchemaVersion = *probe.FieldSchemaVersion
		}
		return out, nil
	}
	// 退化为「只存值」形态。
	dec = json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	plain := map[string]any{}
	if err := dec.Decode(&plain); err != nil {
		return CustomFields{}, fmt.Errorf("解析 custom_fields: %w", err)
	}
	out.Values = plain
	return out, nil
}

// Encode 序列化为 JSONB 载荷。
func (c CustomFields) Encode() ([]byte, error) {
	values := c.Values
	if values == nil {
		values = map[string]any{}
	}
	version := c.FieldSchemaVersion
	if version == 0 {
		version = SchemaVersion
	}
	return json.Marshal(struct {
		FieldSchemaVersion int            `json:"field_schema_version"`
		Values             map[string]any `json:"values"`
	}{version, values})
}

// Clone 深拷贝容器，避免调用方共享 map 后互相覆盖。
func (c CustomFields) Clone() CustomFields {
	out := NewCustomFields(c.FieldSchemaVersion)
	for k, v := range c.Values {
		out.Values[k] = v
	}
	return out
}

// UnknownFieldIDs 返回不属于 declared 集合的字段标识，按字典序排序。
//
// 这是「保留未知附加字段可只读展示」的实现依据：渲染器只认识 declared，
// 其余字段由调用方交给只读展示路径。
func (c CustomFields) UnknownFieldIDs(declared map[string]bool) []string {
	var out []string
	for id := range c.Values {
		if !declared[id] {
			out = append(out, id)
		}
	}
	return SortFieldIDs(out)
}
