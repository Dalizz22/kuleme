package templates

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

// 内置模板 manifest 的唯一可编辑位置是 server/templates/builtin/。
// go:embed 不能引用包外路径，因此 internal/templates/builtin/ 保存一份逐字节相同的副本
// 供打包，修改后需手动复制过去——与迁移文件同一套做法，理由是同一个：
// 让「声明模板的地方」和「读模板的地方」是同一份文件，而不是两份要手工对齐的表。
//
//go:embed all:builtin
var manifestFS embed.FS

// BuiltinFS 暴露内置模板的只读视图。设置页与接口层展示模板来源时用它。
var BuiltinFS fs.FS = mustSub()

func mustSub() fs.FS {
	sub, err := fs.Sub(manifestFS, "builtin")
	if err != nil {
		panic(fmt.Sprintf("内置模板未打包：internal/templates/builtin 缺失或为空，请确认它与 server/templates/builtin 内容一致后重新编译: %v", err))
	}
	return sub
}

// builtinKeys 是内置模板的固定集合，顺序即默认展示顺序。
//
// 写死在这里而不是「读目录里有什么」：模板标识属于对外契约，多一个文件
// 就悄悄多一个模板不是我们想要的发布行为。
var builtinKeys = []string{"general", "collection", "gear", "library", "stock", "sale", "maintenance"}

// ErrUnknownTemplate 表示请求的模板标识不在内置目录里。
//
// 单独一个哨兵错误值而不是靠字符串匹配：路由层要把它翻成 404，
// 而「目录加载失败」要翻成 503，两者混淆会让部署问题看起来像用户输入错误。
var ErrUnknownTemplate = errors.New("模板不存在")

// Catalog 是加载并校验后的模板集合，只读。
type Catalog struct {
	mu        sync.RWMutex
	manifests map[string]Manifest
	order     []string
	issues    map[string][]Issue
}

// LoadBuiltin 加载并校验内置模板。
//
// 任一模板存在结构问题都会返回错误：模板是启动期资产，带着坏模板启动
// 会让后续每一次写入都落在错误的 schema 上。
func LoadBuiltin() (*Catalog, error) {
	c := &Catalog{
		manifests: make(map[string]Manifest, len(builtinKeys)),
		issues:    make(map[string][]Issue),
	}
	defaults := 0
	for _, key := range builtinKeys {
		raw, err := fs.ReadFile(BuiltinFS, key+".json")
		if err != nil {
			return nil, fmt.Errorf("读取内置模板 %s: %w", key, err)
		}
		m, err := decodeManifest(raw)
		if err != nil {
			return nil, fmt.Errorf("解析内置模板 %s: %w", key, err)
		}
		if m.TemplateKey != key {
			return nil, fmt.Errorf("内置模板文件 %s.json 声明了 template_key=%q，两者必须一致",
				key, m.TemplateKey)
		}
		if issues := Validate(m); len(issues) > 0 {
			c.issues[key] = issues
			return nil, fmt.Errorf("内置模板 %s 校验失败：%s", key, issues[0].Message)
		}
		if m.Group != GroupCore || m.Owner != string(GroupCore) {
			return nil, fmt.Errorf("内置模板 %s 必须声明 group=core、owner=core", key)
		}
		if m.IsDefault {
			defaults++
		}
		c.manifests[key] = m
		c.order = append(c.order, key)
	}
	if defaults != 1 {
		return nil, fmt.Errorf("内置模板必须恰好有一个 is_default，实际 %d 个", defaults)
	}
	return c, nil
}

// Manifest 返回指定模板，不存在时第二个返回值为 false。
func (c *Catalog) Manifest(key string) (Manifest, bool) {
	if c == nil {
		return Manifest{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.manifests[key]
	return m, ok
}

// Keys 按展示顺序返回模板标识。
func (c *Catalog) Keys() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.order...)
}

// List 按展示顺序返回全部模板。
func (c *Catalog) List() []Manifest {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Manifest, 0, len(c.order))
	for _, k := range c.order {
		out = append(out, c.manifests[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sort < out[j].Sort })
	return out
}

// DefaultKey 返回默认模板标识。
func (c *Catalog) DefaultKey() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, k := range c.order {
		if c.manifests[k].IsDefault {
			return k
		}
	}
	return ""
}

// Operations 返回所有内置模板共用操作标识的交集，供「批量操作」入口取用。
//
// 取交集而不是并集：模板之间操作集合可能不同，批量入口只能展示所有涉及模板
// 都支持的操作。当前五个模板的操作集合一致，因此交集即全集。
func (c *Catalog) Operations() []Operation {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Operation
	first := true
	seen := map[string]int{}
	for _, k := range c.order {
		for _, op := range c.manifests[k].Operations {
			seen[op.Key]++
		}
		if first {
			out = append(out, c.manifests[k].Operations...)
			first = false
		}
	}
	var kept []Operation
	for _, op := range out {
		if seen[op.Key] == len(c.order) {
			kept = append(kept, op)
		}
	}
	return kept
}

// FieldLabels 返回「字段标识 → 展示名」映射，用于插件模板缺失时
// 只读展示已保存的字段名。
//
// 未声明的字段回退为「字段标识」本身：宁可显示一个内部标识，也不隐藏数据。
func (c *Catalog) FieldLabels() map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, k := range c.order {
		for _, f := range c.manifests[k].Fields {
			if _, ok := out[f.FieldID]; !ok {
				out[f.FieldID] = f.Label
			}
		}
	}
	return out
}

// ValidateTemplateKey 校验模板标识存在，返回它对应的 manifest。
func (c *Catalog) ValidateTemplateKey(key string) (Manifest, error) {
	m, ok := c.Manifest(key)
	if !ok {
		return Manifest{}, fmt.Errorf("%w: %q", ErrUnknownTemplate, strings.TrimSpace(key))
	}
	return m, nil
}
