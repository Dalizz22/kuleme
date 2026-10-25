package templates

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/httpx"
)

// Register 挂载模板模块路由（docs/接口契约约定.md 第 6 节，前缀 /api/v1/templates）。
//
// 本模块自己的接口都是读或纯计算：模板来自进程内嵌的只读目录，没有写入路径，
// 因此不涉及 Idempotency-Key 与库权限——模板是全局资产，不属于任何一个库；
// 但只读也要求登录（契约口径：模板清单不对外匿名暴露）。
// 需要库权限的动作（把模板应用到某个物品）发生在 item 模块，它直接调用
// Service.MergeFields，不走本包 HTTP。
//
// 返回 *Service，因为 item 模块要用同一个实例做字段合并（HTTP 只是其中一种调用方式）。
func Register(v1 *gin.RouterGroup, deps appdeps.Deps) *Service {
	svc := NewService()
	g := v1.Group("/templates", httpx.Auth(deps.Auth, true))

	// 模板集合与单个模板。
	g.GET("", listTemplates)
	g.GET("/operations", listOperations)
	g.GET("/:key", getTemplate)
	// 字段合并与只读视图：给前端做「改字段前先看结论」，也方便排查
	// 「为什么这个值没保存」，以及换模板后展示保留下来的旧资料。
	g.POST("/:key/resolve", svc.resolveFields)
	g.POST("/:key/readonly", svc.readonlyFields)
	return svc
}

// --- 处理器 ---

func listTemplates(c *gin.Context) {
	cat, err := Builtin()
	if err != nil {
		abortTemplate(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": cat.List(), "default_key": cat.DefaultKey()})
}

func listOperations(c *gin.Context) {
	cat, err := Builtin()
	if err != nil {
		abortTemplate(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": cat.Operations()})
}

func getTemplate(c *gin.Context) {
	cat, err := Builtin()
	if err != nil {
		abortTemplate(c, err)
		return
	}
	m, err := cat.ValidateTemplateKey(c.Param("key"))
	if err != nil {
		abortTemplate(c, err)
		return
	}
	httpx.OK(c, gin.H{
		"manifest":   m,
		"operations": cat.Operations(),
		"labels":     cat.FieldLabels(),
	})
}

// resolveRequest 是一次字段解析请求。
type resolveRequest struct {
	// Values 是本次提交的字段值。字段标识缺失表示「不改」；
	// 显式给 null 或空字符串表示清空。
	Values map[string]any `json:"values"`
	// Current 是记录里已有的 custom_fields（带版本形态）。
	Current *CustomFields `json:"current,omitempty"`
	// Strict 为 true 时只要有字段被拒绝就整体失败。
	Strict bool `json:"strict,omitempty"`
	// ApplyPreset 为 true 时对空缺字段套用模板预设。
	ApplyPreset bool `json:"apply_preset,omitempty"`
}

func (s *Service) resolveFields(c *gin.Context) {
	key := c.Param("key")
	var req resolveRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Abort(c, err)
		return
	}
	m, err := builtinManifest(key)
	if err != nil {
		abortTemplate(c, err)
		return
	}
	existing := NewCustomFields(m.SchemaVersion)
	if req.Current != nil {
		existing = *req.Current
		if existing.Values == nil {
			existing.Values = map[string]any{}
		}
	}
	opts := MergeOptions{Strict: req.Strict}
	if req.ApplyPreset {
		opts.Preset = &m.Preset
	}
	// 走进服务层而不是直接调 Merge：这样「HTTP 合并」与「item 模块内的合并」
	// 一定走同一段代码，两边结论不会漂移。
	m, res, err := s.MergeFields(c.Request.Context(), key, existing, req.Values, opts)
	if err != nil {
		abortTemplate(c, err)
		return
	}
	if opts.Strict {
		if rejected := res.Rejected(); len(rejected) > 0 {
			httpx.Abort(c, validationError(res))
			return
		}
	}
	httpx.OK(c, resolveResponse(m, res))
}

// readonlyRequest 请求「换模板/停用插件后还能看到什么」。
type readonlyRequest struct {
	Current *CustomFields `json:"current,omitempty"`
}

// readonlyFields 返回已保存的值、字段显示名与未知字段标识。
//
// 这是「保留未知附加字段可只读展示」那条约定的落地点：即使当前模板不再声明
// 某个字段，它的名称与值仍然出现在这里，普通资料页照常显示。
func (s *Service) readonlyFields(c *gin.Context) {
	var req readonlyRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Abort(c, err)
		return
	}
	existing := NewCustomFields(SchemaVersion)
	if req.Current != nil {
		existing = *req.Current
		if existing.Values == nil {
			existing.Values = map[string]any{}
		}
	}
	values, labels, unknown, err := s.ReadonlyView(c.Request.Context(), c.Param("key"), existing)
	if err != nil {
		abortTemplate(c, err)
		return
	}
	httpx.OK(c, gin.H{
		"values":               values,
		"labels":               labels,
		"unknown_field_ids":    unknown,
		"field_schema_version": existing.FieldSchemaVersion,
	})
}

// resolveResponse 组装 /resolve 的响应体。
//
// 单独抽出来是为了让「响应形状」只有一处定义：调用方按 snake_case 解析，
// 字段名一改就是破坏性变更。
func resolveResponse(m Manifest, res Resolution) gin.H {
	return gin.H{
		"values":               res.Fields.Values,
		"unknown_field_ids":    res.Fields.UnknownFieldIDs(declaredSet(m)),
		"field_schema_version": res.Fields.FieldSchemaVersion,
		"decisions":            res.Decisions,
		"warnings":             res.Warnings(),
		"unrecognized_inputs":  res.UnrecognizedInputs,
		"labels":               res.Labels(m),
		"sources":              res.Sources,
		"schema_changed":       res.SchemaChanged,
		"template_key":         m.TemplateKey,
	}
}

// builtinManifest 取模板清单，并把「目录不可用」与「模板不存在」分开报。
func builtinManifest(key string) (Manifest, error) {
	cat, err := Builtin()
	if err != nil {
		return Manifest{}, err
	}
	return cat.ValidateTemplateKey(key)
}

// declaredSet 返回模板声明的字段标识集合。
func declaredSet(m Manifest) map[string]bool {
	out := make(map[string]bool, len(m.Fields))
	for _, f := range m.Fields {
		out[f.FieldID] = true
	}
	return out
}

// --- 错误映射 ---

// validationError 把被拒绝的字段放进 details，前端能定位到具体录入框。
func validationError(res Resolution) *httpx.Error {
	details := make([]gin.H, 0, len(res.Decisions))
	for _, d := range res.Rejected() {
		details = append(details, gin.H{"field_id": d.FieldID, "code": d.Code, "message": d.Message})
	}
	return httpx.Validation("部分字段未通过校验").WithDetails(map[string]any{
		"fields":              details,
		"unrecognized_inputs": res.UnrecognizedInputs,
	})
}

// abortTemplate 把模板错误映射成契约里的错误码。
//
// 模板不存在是 404：模板标识是路径的一部分，找不到就是找不到。
// 目录加载失败是 503：那是部署问题（常见于忘记执行 sync-templates.sh），
// 不该让前端以为是用户输入有错。
func abortTemplate(c *gin.Context, err error) {
	var he *httpx.Error
	if errors.As(err, &he) {
		httpx.Abort(c, he)
		return
	}
	if errors.Is(err, ErrUnknownTemplate) || strings.Contains(err.Error(), "不存在") {
		httpx.Abort(c, httpx.NotFound("模板不存在"))
		return
	}
	httpx.Abort(c, httpx.Unavailable("模板目录不可用"))
}
