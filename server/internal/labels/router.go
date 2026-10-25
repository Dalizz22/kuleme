package labels

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/idem"
	"github.com/klm/server/internal/labels/kod"
)

// Register 挂载标签与扫码解析路由（docs/接口契约约定.md 第 6 节：
// 前缀 /api/v1/labels 与 /api/v1/codes）。
//
// 只读与纯计算接口（渲染预览、版面预设）不要求 Idempotency-Key：
// 渲染是确定性的，重复请求得到同样的字节，没有副作用。
// 会落盘（生成下载文件）或发往设备的接口要求它，与核心写操作一致。
//
// 返回构造好的 Service：扫码解析的对象查询实现属于核心服务，
// 由 bootstrap 在注册后注入（WithResolver），模块内部不查库。
func Register(v1 *gin.RouterGroup, deps appdeps.Deps, cfg Config, files FileRecorder, locator FileLocator, authz SpaceAuthorizer) *Service {
	svc := NewService(cfg, files, locator, authz, nil)
	// 全部 labels/codes 路由要求登录：渲染是服务端 CPU 活（PDF 生成），
	// 匿名开放等于把 DoS 入口挂在公网上。
	a := v1.Group("", httpx.Auth(deps.Auth, true))
	g := a.Group("/labels")
	g.GET("/layouts", listLayouts)
	g.POST("/render", svc.renderHandler)
	g.POST("/jobs", idem.Middleware(deps.DB), svc.createJobHandler)
	// 路径里的任务 id 直接作为 file_id 去查 file_object（uuid 列）：
	// 非 uuid 必须在进 SQL 前拦掉，否则 PostgreSQL 报 22P02 变成 500。
	g.POST("/jobs/:id/print", httpx.UUIDGuard("id"), svc.printHandler)
	g.POST("/reprint", svc.reprintHandler)
	g.GET("/transports", svc.listTransports)

	// 扫码解析：把 KLM1:<instance>:<kind>:<code> 换成对象引用。
	// 本模块只做「格式解析 + 归属校验」，真正的对象查询由核心服务完成。
	c := a.Group("/codes")
	c.GET("/resolve", svc.resolveCodeHandler)
	return svc
}

// --- 渲染与任务 ---

func listLayouts(c *gin.Context) {
	httpx.OK(c, gin.H{
		"items":      LayoutPresets(),
		"a4":         presetDetail(A4DefaultLayout()),
		"quiet_zone": QuietZoneModules,
	})
}

func presetDetail(l Layout) gin.H {
	n, err := l.Normalize()
	if err != nil {
		n = l
	}
	return gin.H{
		"rows":          n.Rows,
		"cols":          n.Cols,
		"capacity":      n.Rows * n.Cols,
		"qr_size_mm":    n.QRSizeMM,
		"padding_mm":    n.PaddingMM,
		"quiet_zone_mm": QuietZoneMM(n.QRSizeMM, 0),
		"description":   LayoutDescription(n),
	}
}

// renderRequest 是渲染请求体。
type renderRequest struct {
	Format LayoutFormat `json:"format"`
	// Contents 是要打印的标签内容（已由调用方按物品组装）。
	Contents []Content `json:"contents"`
	// SpaceID 仅落盘时需要，预览可省略。
	SpaceID string `json:"space_id,omitempty"`
	// ReprintOf 非空表示补打。
	ReprintOf string `json:"reprint_of,omitempty"`
	// ReprintLabel 覆盖补打小字。
	ReprintLabel string `json:"reprint_label,omitempty"`
	// SheetID 是任务标识，为空时由服务端按内容生成。
	SheetID string `json:"sheet_id,omitempty"`
	// FontPath 覆盖默认字体（仅当部署允许时生效）。
	FontPath string `json:"font_path,omitempty"`
	// BaseSizePt 覆盖基准字号。
	BaseSizePt float64 `json:"base_size_pt,omitempty"`
	// DrawBorder 覆盖边框默认值。
	DrawBorder *bool `json:"draw_border,omitempty"`
}

func (r renderRequest) input() RenderInput {
	return RenderInput{
		Format:       r.Format,
		Contents:     r.Contents,
		ReprintOf:    r.ReprintOf,
		ReprintLabel: r.ReprintLabel,
		SheetID:      r.SheetID,
		FontPath:     r.FontPath,
		BaseSizePt:   r.BaseSizePt,
		DrawBorder:   r.DrawBorder,
	}
}

// renderHandler 直接返回 PDF 字节。
//
// 为什么不套 JSON 外壳：这是文件接口，客户端要么把它交给打印对话框、
// 要么存成文件；包成 base64 会让一张 30 格的标签纸膨胀 33%，而且
// 浏览器无法把它当作 PDF 打开。错误仍然走统一 JSON 外壳。
func (s *Service) renderHandler(c *gin.Context) {
	var req renderRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Abort(c, err)
		return
	}
	res, err := s.Render(c.Request.Context(), req.input())
	if err != nil {
		abortLabel(c, err)
		return
	}
	if warn := res.QuietZoneWarning; warn != "" {
		c.Header("X-Label-Warning", "quiet-zone")
	}
	c.Header("Content-Disposition", contentDisposition(res.Filename))
	c.Header("X-Label-Sheets", fmt.Sprint(res.Sheets))
	c.Data(200, "application/pdf", res.PDF)
}

func (s *Service) createJobHandler(c *gin.Context) {
	actor, err := httpx.RequireActor(c)
	if err != nil {
		httpx.Abort(c, err)
		return
	}
	var req renderRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Abort(c, err)
		return
	}
	info, err := s.CreateJob(c.Request.Context(), actor.UserID, req.SpaceID, req.input())
	if err != nil {
		abortLabel(c, err)
		return
	}
	httpx.Created(c, gin.H{
		"file_id":   info.ID,
		"file_name": info.Filename,
		"byte_size": info.ByteSize,
		"kind":      info.Kind,
	})
}

// sendRequest 是发送到设备的请求体。
type sendRequest struct {
	FileID    string     `json:"file_id"`
	Transport string     `json:"transport,omitempty"`
	Copies    int        `json:"copies,omitempty"`
	Printer   PrinterRef `json:"printer,omitempty"`
}

func (s *Service) printHandler(c *gin.Context) {
	actor, err := httpx.RequireActor(c)
	if err != nil {
		httpx.Abort(c, err)
		return
	}
	var req sendRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Abort(c, err)
		return
	}
	// 路径里的 id 优先：它可能来自一个之前生成的任务。
	if id := strings.TrimSpace(c.Param("id")); id != "" {
		req.FileID = id
	} else if err := httpx.ValidateUUID("file_id", req.FileID, true); err != nil {
		httpx.Abort(c, err)
		return
	}
	task, err := s.Send(c.Request.Context(), SendInput{
		FileID:    req.FileID,
		ActorID:   actor.UserID,
		Transport: req.Transport,
		Copies:    req.Copies,
		Printer:   req.Printer,
	})
	if err != nil {
		abortLabel(c, err)
		return
	}
	httpx.Created(c, task)
}

// reprintHandler 补打：按原参数重新渲染一份新文件并出任务。
//
// 补打不复用旧文件：旧文件可能已被清理或权限已变。重新渲染的内容与原件一致，
// 只是带上溯源标记（ReprintOf / ReprintLabel），因此补打路径与首次打印
// 完全共用同一段代码。
func (s *Service) reprintHandler(c *gin.Context) {
	actor, err := httpx.RequireActor(c)
	if err != nil {
		httpx.Abort(c, err)
		return
	}
	var req renderRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Abort(c, err)
		return
	}
	if strings.TrimSpace(req.ReprintOf) == "" {
		httpx.Abort(c, httpx.Validation("补打必须提供 reprint_of（原任务标识）"))
		return
	}
	info, err := s.CreateJob(c.Request.Context(), actor.UserID, req.SpaceID, req.input())
	if err != nil {
		abortLabel(c, err)
		return
	}
	url := downloadURL(info)
	task, err := s.Send(c.Request.Context(), SendInput{
		FileID:    info.ID,
		ActorID:   actor.UserID,
		Transport: "browser",
	})
	if err != nil {
		// 文件已经生成，发送方式不可用不该让整个补打失败：
		// 返回文件信息让客户端自己取，比让用户重来一次好。
		httpx.Created(c, gin.H{
			"file_id":   info.ID,
			"file_name": info.Filename,
			"url":       url,
			"transport": "",
			"warning":   "已生成补打文件，但当前没有可用的发送方式，请手动下载打印",
		})
		return
	}
	httpx.Created(c, task)
}

func (s *Service) listTransports(c *gin.Context) {
	trans := s.Transports()
	items := make([]gin.H, 0, len(trans.Names()))
	for _, name := range trans.Names() {
		t, _ := trans.Get(name)
		items = append(items, gin.H{
			"name":      name,
			"kind":      t.Kind(),
			"available": t.Available(),
		})
	}
	// available 是名字列表而不是布尔值：客户端要能直接拿第一个可用的方式
	// 去调 /print，少一次自己挑的逻辑。
	httpx.OK(c, gin.H{"items": items, "available": trans.Available()})
}

func downloadURL(info FileInfo) string {
	if info.DownloadURL != "" {
		return info.DownloadURL
	}
	return "/api/v1/files/" + url.PathEscape(info.ID)
}

// contentDisposition 生成附件头，文件名同时给 ASCII 与 UTF-8 两种形式。
//
// filename* 用 RFC 5987 编码：中文文件名只用 filename= 会在部分客户端上乱码。
func contentDisposition(filename string) string {
	ascii := make([]byte, 0, len(filename))
	for i := 0; i < len(filename); i++ {
		c := filename[i]
		if c >= 0x20 && c < 0x7F && c != '"' && c != '\\' {
			ascii = append(ascii, c)
		} else {
			ascii = append(ascii, '_')
		}
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", string(ascii), url.PathEscape(filename))
}

// --- 扫码解析 ---

// ResolvedCode 是解析结果：告诉客户端「这个码指向哪个对象、该跳哪一页」。
type ResolvedCode struct {
	// Kind 是对象类型：item / space / location。
	Kind string `json:"kind"`
	// ObjectID 是对象标识（不透明字符串）。
	ObjectID string `json:"object_id"`
	// SpaceID 是所属库，客户端据此切换当前库。
	SpaceID string `json:"space_id,omitempty"`
	// Name 是展示名，扫码后直接显示，省一次跳转。
	Name string `json:"name,omitempty"`
	// Subtitle 是副标题（公开码、位置路径）。
	Subtitle string `json:"subtitle,omitempty"`
	// PageURL 是客户端可跳转的地址。
	PageURL string `json:"page_url,omitempty"`
	// ItemID 是物品码专用的快捷字段：客户端扫物品码后直接进详情页，
	// 不必再自己从 object_id 反推类型。仅 kind=item 时有值。
	ItemID string `json:"item_id,omitempty"`
}

// resolveCodeHandler 解析扫码内容。
//
// 三种输入都要能处理，因为「扫到的字符串」来源五花八门：
//   - 完整二维码 payload（KLM1:...）；
//   - 手工输入的短码或公开码（含 0/O、1/I/L 混淆）；
//   - 普通搜索词（此时明确返回「不是编码」，让前端走搜索而不是报错）。
//
// 本接口只做前两种：解析格式 + 通过注入的 CodeResolver 取对象归属。
// 它不查库、不判断可见性，因此不重复核心服务的权限逻辑。
func (s *Service) resolveCodeHandler(c *gin.Context) {
	raw := strings.TrimSpace(c.Query("value"))
	if raw == "" {
		httpx.Abort(c, httpx.Validation("缺少 value 参数"))
		return
	}
	// 需登录：登录后拿到对象归属；匿名请求被 Auth 中间件挡下。
	actorID := httpx.CurrentActor(c).UserID

	payload, resolved, err := s.ResolveCode(c.Request.Context(), actorID, raw)
	if err != nil {
		if !kod.LooksLikeQR(raw) && errors.Is(err, kod.ErrBadCode) {
			// 不是编码：不是错误，明确告诉前端去走搜索。
			httpx.OK(c, gin.H{"is_code": false, "value": raw})
			return
		}
		if errors.Is(err, kod.ErrBadCode) {
			httpx.Abort(c, httpx.InvalidCode("编码格式不正确"))
			return
		}
		abortLabel(c, err)
		return
	}
	out := gin.H{
		"is_code":     true,
		"instance_id": payload.InstanceID,
		"kind":        payload.Kind,
		"code":        payload.Code,
		"payload":     payload.Encode(),
		"resolved":    nil,
	}
	if resolved != nil {
		out["resolved"] = resolved
	}
	httpx.OK(c, out)
}

// parseCodeInput 把用户输入归一成二维码 payload。
//
// 公开码本身不带 kind 与 instance：此时按「物品公开码」处理，instance_id 留空，
// 由调用方用当前实例补齐（一个部署只会有一个实例码）。
func parseCodeInput(raw string) (kod.QRPayload, error) {
	if kod.LooksLikeQR(raw) {
		return kod.ParseQR(raw)
	}
	norm := kod.Normalize(raw)
	if norm == "" {
		return kod.QRPayload{}, kod.ErrBadCode
	}
	if kod.ValidPublicCode(norm) {
		return kod.QRPayload{Kind: kod.KindItem, Code: norm}, nil
	}
	if kod.ValidInstanceID(norm) {
		return kod.QRPayload{Kind: kod.KindSpace, Code: norm}, nil
	}
	return kod.QRPayload{}, kod.ErrBadCode
}

// --- 错误映射 ---

// abortLabel 把标签模块的错误映射成契约里的错误码。
func abortLabel(c *gin.Context, err error) {
	if err == nil {
		return
	}
	var he *httpx.Error
	if errors.As(err, &he) {
		httpx.Abort(c, he)
		return
	}
	// authz 的「库不在可见范围」在标签模块等价于任务不存在：报 404，不冒 500。
	if errors.Is(err, authz.ErrNoSpace) {
		httpx.Abort(c, httpx.NotFound("标签任务不存在"))
		return
	}
	switch {
	case errors.Is(err, ErrNoFont):
		// 这是部署配置问题，不是用户输入问题。
		httpx.Abort(c, httpx.Unavailable("服务器未配置标签字体，无法生成标签"))
	case errors.Is(err, ErrLayout), errors.Is(err, ErrUnknownPreset),
		errors.Is(err, ErrEmpty), errors.Is(err, ErrTooMany):
		httpx.Abort(c, httpx.Validation(strings.TrimPrefix(err.Error(), "标签")))
	case errors.Is(err, ErrMissingDependency):
		httpx.Abort(c, httpx.Unavailable("标签文件服务尚未就绪"))
	case errors.Is(err, ErrTransportUnavailable):
		httpx.Abort(c, httpx.Unavailable("该发送方式当前不可用"))
	default:
		httpx.Abort(c, err)
	}
}
