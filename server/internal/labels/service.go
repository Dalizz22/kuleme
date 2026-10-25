package labels

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/labels/kod"
)

// 本文件是标签模块的服务层：把「取内容 → 排版 → 渲染 → 存文件 → 出任务」
// 串起来，同时把三件不该混在一起的事分开：
//
//  1. 渲染：Render 只依赖一个 Font，产出 PDF 字节，不碰数据库、不碰文件系统。
//  2. 文件生成：CreateJob 经由 FileRecorder 落到下载中心（file_object，kind=label_pdf）。
//  3. 设备发送：Send 只产出任务描述，由 Transport 决定怎么交给设备。
//
// 分开的收益是可验证：渲染可以在没有数据库、没有打印机的环境里单测；
// 发送方式可以整体替换而不动渲染。

// 标签模块需要的窄接口。全部由核心服务实现，本包不自己建表、不自己存文件。
type (
	// FileRecorder 把生成的 PDF 落成一份可下载的受权限保护文件。
	//
	// 实现方（核心服务的文件服务）必须：按 space_id 做写权限校验、
	// 计算 sha256 与字节数、把文件放在受控目录下，并让这次校验在下载时
	// 重新发生一次。kind 取值 'label_pdf'（见 docs/接口契约约定.md 第 8 节）。
	FileRecorder interface {
		Create(ctx context.Context, actorID, spaceID string, req FileCreateRequest) (fileID string, err error)
	}

	// FileLocator 在下载/补打时重新检查权限并返回文件元信息。
	//
	// 「重新检查」是硬要求：权限变化后旧文件
	// 不应再能被没有权限的人下载。
	FileLocator interface {
		Locate(ctx context.Context, actorID, fileID string) (FileInfo, error)
	}

	// SpaceAuthorizer 复用核心服务的库权限检查，本包不复制鉴权逻辑。
	SpaceAuthorizer interface {
		RequireRead(ctx context.Context, actorID, spaceID string) error
	}

	// CodeResolver 把「解析出来的编码」换成对象引用。
	//
	// 标签模块只做格式解析（前缀、Crockford 归一、长度），对象是否存在、
	// 当前用户能不能看，都属于核心服务。返回 ResolvedCode 而不是数据库行，
	// 是为了不把 ORM 模型泄漏成对外类型（docs/接口契约约定.md 第 8 节）。
	CodeResolver interface {
		ResolveCode(ctx context.Context, actorID string, payload kod.QRPayload) (ResolvedCode, error)
	}
)

// FileCreateRequest 是一次文件落盘请求。
//
// 内容直接给字节：标签 PDF 一张纸约几 MB，不值得再引入一个流式接口。
// 大导出走 transfer 模块的后台任务，不在这里。
type FileCreateRequest struct {
	// Kind 固定为 KindLabelPDF。
	Kind string
	// ContentType 固定为 application/pdf。
	ContentType string
	// Filename 是建议的文件名（不含路径）。
	Filename string
	// Bytes 是文件内容。
	Bytes []byte
}

// FileInfo 是文件元信息，供下载与补打复用。
type FileInfo struct {
	ID          string
	SpaceID     string
	Kind        string
	ContentType string
	ByteSize    int64
	Filename    string
	// DownloadURL 是可直接交给浏览器/客户端的受权限保护地址。
	DownloadURL string
}

// KindLabelPDF 是 file_object.kind 里的标签文件类型。
const KindLabelPDF = "label_pdf"

// ErrMissingDependency 表示服务缺少必需的核心服务实现。
var ErrMissingDependency = errors.New("标签服务缺少必需的依赖实现")

// ErrCodeNotFound 表示码格式合法，但对应对象不存在或当前 actor 不可见。
//
// 它不是 HTTP 错误：路由层按要求回 200 + resolved=null（不泄露存在性）。
var ErrCodeNotFound = errors.New("编码对应的对象不存在或无权访问")

// Config 是服务配置。
type Config struct {
	// DefaultFontPath 是默认中文字体路径（.ttf）。
	//
	// 必须可配置而不是写死：各平台的中文字体位置与格式都不同，而且
	// 常见的 .ttc 集合暂时不支持（见 fonts 包）。留空时渲染请求必须自己
	// 指定字体，否则返回 ErrNoFont——宁可报错也不印出空白标签。
	DefaultFontPath string
	// AllowFontPathFromRequest 允许请求自带 font_path。
	//
	// 默认关闭：字体路径来自请求就是一个文件读取入口，只有在管理员
	// 明确知道自己在做什么时才开。
	AllowFontPathFromRequest bool
	// FontSearchPaths 是未配置字体时按序尝试的候选路径。
	FontSearchPaths []string
	// DrawBorderDefault 是未指定时的边框默认值。
	DrawBorderDefault bool
}

// DefaultFontSearchPaths 返回各平台常见的中文字体候选路径。
//
// 只列 .ttf：.ttc 是字体集合，需要额外选出一个 face，本实现不做。
// 这些路径只是「尽力而为」的候选，找到就提示，不保证一定存在。
func DefaultFontSearchPaths() []string {
	return []string{
		"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttf",
		"/usr/share/fonts/opentype/noto/NotoSansCJKsc-Regular.otf",
		"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
		"C:/Windows/Fonts/msyh.ttf",
		"C:/Windows/Fonts/simhei.ttf",
		"/System/Library/Fonts/Supplemental/Songti.ttc",
		"/Library/Fonts/Arial Unicode.ttf",
	}
}

// Service 是标签服务。
type Service struct {
	cfg     Config
	files   FileRecorder
	locator FileLocator
	authz   SpaceAuthorizer
	trans   *TransportRegistry
	// resolver 可以为 nil：扫码解析的格式部分仍然可用，只是不带对象归属。
	resolver CodeResolver
}

// NewService 构造标签服务。
//
// files / locator / authz 可以为 nil：那时渲染与排版仍然可用（预览不需要
// 落盘），但 CreateJob 与 Send 会返回 ErrMissingDependency 而不是假装成功。
func NewService(cfg Config, files FileRecorder, locator FileLocator, authz SpaceAuthorizer, trans *TransportRegistry) *Service {
	if trans == nil {
		trans = NewTransportRegistry(nil)
	}
	return &Service{
		cfg:     cfg,
		files:   files,
		locator: locator,
		authz:   authz,
		trans:   trans,
	}
}

// RenderInput 是一次渲染请求。
type RenderInput struct {
	// Format 是版面：A4 多格或自定义单标签尺寸。
	Format LayoutFormat
	// Contents 是要打印的标签（每项对应一个物品）。
	Contents []Content
	// ReprintOf 非空表示这是补打，值是原任务标识。
	ReprintOf string
	// ReprintLabel 覆盖补打小字。
	ReprintLabel string
	// SheetID 由调用方提供（核心服务的 ID 生成器）；为空时按补打标记生成。
	SheetID string
	// FontPath 覆盖默认字体路径。
	FontPath string
	// BaseSizePt 覆盖基准字号。
	BaseSizePt float64
	// DrawBorder 覆盖边框默认值（指针表示「未指定」）。
	DrawBorder *bool
}

// RenderResult 是一次渲染结果。
type RenderResult struct {
	PDF      []byte
	Sheets   int
	Filename string
	// QuietZoneWarning 非空时说明当前内边距不足以提供标准静区，
	// 应当提示用户调大内边距或标签尺寸。
	QuietZoneWarning string
}

// Render 排版并渲染 PDF。纯计算：不落盘、不发送。
func (s *Service) Render(ctx context.Context, in RenderInput) (RenderResult, error) {
	if err := ctx.Err(); err != nil {
		return RenderResult{}, err
	}
	layout, err := in.Format.Layout()
	if err != nil {
		return RenderResult{}, err
	}
	job := Job{
		SheetID:      in.SheetID,
		ReprintOf:    in.ReprintOf,
		ReprintLabel: in.ReprintLabel,
		Layout:       layout,
		Contents:     in.Contents,
	}
	plans, err := job.Sheets()
	if err != nil {
		return RenderResult{}, err
	}

	fontBytes, err := s.resolveFont(in.FontPath)
	if err != nil {
		return RenderResult{}, err
	}

	drawBorder := s.cfg.DrawBorderDefault
	if in.DrawBorder != nil {
		drawBorder = *in.DrawBorder
	}
	opts := RendererOptions{FontBytes: fontBytes, BaseSizePt: in.BaseSizePt, DrawBorder: drawBorder}

	// 一次渲染产出多页 PDF：一次下载、一次打印、只嵌一份字体，
	// 也避免自己写 PDF 合并（那需要重排对象编号，是另一类出错来源）。
	out, err := RenderPDF(plans, opts)
	if err != nil {
		return RenderResult{}, err
	}

	name := strings.TrimSpace(in.SheetID)
	if name == "" {
		if in.ReprintOf != "" {
			name = in.ReprintOf + "-reprint"
		} else {
			name = "labels"
		}
	}
	return RenderResult{
		PDF:              out,
		Sheets:           len(plans),
		Filename:         fmt.Sprintf("%s.pdf", sanitizeFilename(name)),
		QuietZoneWarning: quietZoneNote(layout),
	}, nil
}

// CreateJob 渲染并落成一份可下载文件，返回文件标识。
//
// 这里是「确认类」的写操作：需要 actor 与 space 以复用核心权限检查。
// 不做自己的幂等表：operation_id 由调用方（HTTP 层的 Idempotency-Key）
// 传给核心文件服务，重复提交由核心的幂等机制处理。
func (s *Service) CreateJob(ctx context.Context, actorID, spaceID string, in RenderInput) (FileInfo, error) {
	if s.files == nil {
		return FileInfo{}, fmt.Errorf("%w: FileRecorder", ErrMissingDependency)
	}
	if strings.TrimSpace(spaceID) == "" {
		return FileInfo{}, fmt.Errorf("缺少 space_id：标签文件必须归属一个库")
	}
	if s.authz == nil {
		return FileInfo{}, fmt.Errorf("%w: SpaceAuthorizer", ErrMissingDependency)
	}
	if err := s.authz.RequireRead(ctx, actorID, spaceID); err != nil {
		return FileInfo{}, err
	}
	res, err := s.Render(ctx, in)
	if err != nil {
		return FileInfo{}, err
	}
	fileID, err := s.files.Create(ctx, actorID, spaceID, FileCreateRequest{
		Kind:        KindLabelPDF,
		ContentType: "application/pdf",
		Filename:    res.Filename,
		Bytes:       res.PDF,
	})
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{
		ID:          fileID,
		SpaceID:     spaceID,
		Kind:        KindLabelPDF,
		ContentType: "application/pdf",
		ByteSize:    int64(len(res.PDF)),
		Filename:    res.Filename,
	}, nil
}

// SendInput 是一次发送请求。
type SendInput struct {
	// FileID 是 CreateJob（或历史补打）得到的文件标识。
	FileID string
	// ActorID 用于下载前重新检查权限。
	ActorID string
	// Transport 为空时用第一个可用的发送方式。
	Transport string
	// Copies 是份数。
	Copies int
	// Printer 是目标设备。
	Printer PrinterRef
}

// Send 把已生成的文件交给设备。不重新渲染，也不改库存。
func (s *Service) Send(ctx context.Context, in SendInput) (PrintTask, error) {
	if s.locator == nil {
		return PrintTask{}, fmt.Errorf("%w: FileLocator", ErrMissingDependency)
	}
	if strings.TrimSpace(in.FileID) == "" {
		return PrintTask{}, httpx.Validation("缺少必填字段 file_id")
	}
	// file_id 会作为 text 参数与 file_object.id（uuid 列）比较：
	// 非 uuid 直接进 SQL 会得到 SQLSTATE 22P02 → 500，这里提前按校验错误拒绝。
	if err := httpx.ValidateUUID("file_id", in.FileID, true); err != nil {
		return PrintTask{}, err
	}
	info, err := s.locator.Locate(ctx, in.ActorID, in.FileID)
	if err != nil {
		return PrintTask{}, err
	}
	if info.Kind != "" && info.Kind != KindLabelPDF {
		return PrintTask{}, fmt.Errorf("文件 %s 不是标签文件", in.FileID)
	}
	return s.trans.Send(ctx, in.Transport, SendRequest{
		FileID:   info.ID,
		Filename: info.Filename,
		URL:      info.DownloadURL,
		Copies:   in.Copies,
		Printer:  in.Printer,
	})
}

// Transports 返回发送方式注册表，供路由层列出可用能力。
func (s *Service) Transports() *TransportRegistry { return s.trans }

// WithResolver 注入扫码解析所需的对象查询实现，返回自身便于链式装配。
//
// 单独一个方法而不是构造函数参数：解析器属于核心服务，接入顺序可能晚于
// 标签服务构造；没有它时渲染与打印完全不受影响。
func (s *Service) WithResolver(r CodeResolver) *Service {
	s.resolver = r
	return s
}

// ResolveCode 解析并（在有实现时）补齐对象归属。
//
// 解析失败返回 ErrBadCode；对象不存在或不可见返回 nil resolved（不算错误，
// 契约要求 200 + resolved=null，不泄露存在性）；其它错误原样上抛。
func (s *Service) ResolveCode(ctx context.Context, actorID, raw string) (kod.QRPayload, *ResolvedCode, error) {
	payload, err := parseCodeInput(raw)
	if err != nil {
		return kod.QRPayload{}, nil, err
	}
	if s.resolver == nil {
		return payload, nil, nil
	}
	resolved, err := s.resolver.ResolveCode(ctx, actorID, payload)
	if err != nil {
		if errors.Is(err, ErrCodeNotFound) {
			return payload, nil, nil
		}
		return payload, nil, err
	}
	return payload, &resolved, nil
}

// resolveFont 取字体字节：请求指定 > 配置默认 > 搜索路径。
//
// 三级回退而不是两级，是因为「默认字体」在不同部署里位置不同；搜索路径
// 让开箱可用，配置项让生产可控，请求项留给高级用户。
// 不做解析缓存：序列化交给 fpdf 之后，这里只剩一次文件读，渲染频率撑不起缓存。
func (s *Service) resolveFont(requestPath string) ([]byte, error) {
	path := strings.TrimSpace(requestPath)
	if path != "" && !s.cfg.AllowFontPathFromRequest {
		return nil, fmt.Errorf("本部署不允许在请求里指定字体路径")
	}
	if path == "" {
		path = strings.TrimSpace(s.cfg.DefaultFontPath)
	}
	if path == "" {
		for _, cand := range s.cfg.FontSearchPaths {
			if _, err := os.Stat(cand); err == nil {
				path = cand
				break
			}
		}
	}
	if path == "" {
		return nil, ErrNoFont
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取字体 %s: %w", path, err)
	}
	return data, nil
}

// quietZoneNote 检查内边距是否够当静区。
//
// 静区不足不会报错：很多标签纸本身就带白边，实际能扫。这里只提示，
// 让使用者自己判断——把可用的标签判成不可用比提示更糟。
func quietZoneNote(layout Layout) string {
	qr := layout.QRSizeMM
	if qr <= 0 {
		qr = layout.LabelHeightMM
	}
	need := QuietZoneMM(qr, 0)
	if layout.PaddingMM >= need {
		return ""
	}
	return fmt.Sprintf("标签内边距 %.1f mm 小于二维码静区建议值 %.1f mm，扫码可能偶发失败；"+
		"建议把内边距调到 %.1f mm 或使用更大的标签", layout.PaddingMM, need, need)
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r > 0x7F:
			// 中文文件名在下载时是合法的，保留；只排除路径分隔符与控制字符。
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "labels"
	}
	return b.String()
}
