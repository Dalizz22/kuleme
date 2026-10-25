package labels

import (
	"context"
	"errors"
	"time"
)

// PrinterRef 描述「要往哪台设备发」。
//
// 刻意不含连接细节（IP、队列名、驱动参数）：当前不直连打印机，走系统打印/分享流程。
// 将来接直连时，这些字段在设备注册表里
// 补，不需要改标签渲染与任务模型。
type PrinterRef struct {
	// ID 是设备标识。
	ID string `json:"id,omitempty"`
	// Name 是展示名（如「客厅标签机」）。
	Name string `json:"name,omitempty"`
}

// PrintTask 是一次「发送到设备」的结果。
//
// 它是描述而不是动作：本包只产生文件和任务描述，真正的发送由调用方
// （浏览器打印对话框、系统分享，或将来的打印代理）执行。这样标签模块
// 在没有任何打印机连接的情况下也能完整工作并接受测试。
type PrintTask struct {
	// FileID 是已保存的标签 PDF，供下载与补打。
	FileID string `json:"file_id"`
	// Transport 是本次可用的发送方式标识。
	Transport string `json:"transport"`
	// Kind 是任务的执行位置：client 表示由客户端完成。
	Kind TransportKind `json:"kind"`
	// URL 是客户端取文件或打开预览的地址。
	URL string `json:"url,omitempty"`
	// Filename 是建议的文件名，客户端打印对话框会用到。
	Filename string `json:"filename"`
	// Copies 是份数。
	Copies int `json:"copies"`
	// Printer 是目标设备；为空表示交给用户在选择对话框里挑。
	Printer PrinterRef `json:"printer,omitempty"`
	// Note 是给使用者的说明，例如「已生成文件，请在打印对话框中确认纸张」。
	Note string `json:"note,omitempty"`
	// CreatedAt 是任务生成时间。
	CreatedAt time.Time `json:"created_at"`
}

// TransportKind 说明任务在哪一侧执行。
type TransportKind string

const (
	// TransportClient 表示由客户端执行（首版唯一的实现路径）。
	TransportClient TransportKind = "client"
	// TransportServer 表示由服务端发送（尚未实现，预留）。
	TransportServer TransportKind = "server"
)

// Transport 是「把已生成的文件交给设备」的一种方式。
//
// 之所以是注册表而不是 if/else：将来加直连打印时，新增一个实现即可，
// 渲染与任务流程完全不动。这也是「生成文件与设备发送分离」的落点——
// 文件生成只依赖 Renderer，发送只依赖 Transport，两者互不知道对方。
type Transport interface {
	// Name 是稳定标识，出现在接口响应里。
	Name() string
	// Kind 说明任务由哪一侧执行。
	Kind() TransportKind
	// Available 报告在当前部署下是否可用。
	Available() bool
	// Send 依据已生成的文件产出任务描述。不返回错误以外的副作用：
	// 这里不做 IO，真正取文件走 URL。
	Send(ctx context.Context, req SendRequest) (PrintTask, error)
}

// SendRequest 是发送所需的最小信息。
type SendRequest struct {
	FileID   string
	Filename string
	URL      string
	Copies   int
	Printer  PrinterRef
}

// ErrTransportUnavailable 表示请求的发送方式在当前部署下不可用。
var ErrTransportUnavailable = errors.New("该发送方式当前不可用")

// DefaultTransports 返回首版内置的发送方式。
//
// 顺序即优先级：先浏览器打印（最通用），再系统分享（移动端友好），
// 最后直连占位（未实现，Available 为 false）。
func DefaultTransports() []Transport {
	return []Transport{
		browserPrintTransport{},
		shareTransport{},
		directTransport{},
	}
}

// browserPrintTransport 让客户端在浏览器里打开 PDF 并调起打印对话框。
//
// 选它作为默认：PDF 已经是最终版式，浏览器打印能正确按实际尺寸输出，
// 不需要我们实现任何驱动协议，也不需要用户安装东西。
type browserPrintTransport struct{}

func (browserPrintTransport) Name() string        { return "browser" }
func (browserPrintTransport) Kind() TransportKind { return TransportClient }
func (browserPrintTransport) Available() bool     { return true }

func (browserPrintTransport) Send(_ context.Context, req SendRequest) (PrintTask, error) {
	if req.URL == "" {
		return PrintTask{}, ErrTransportUnavailable
	}
	return PrintTask{
		FileID:    req.FileID,
		Transport: "browser",
		Kind:      TransportClient,
		URL:       req.URL,
		Filename:  req.Filename,
		Copies:    normalizeCopies(req.Copies),
		Printer:   req.Printer,
		Note:      "请在打印对话框中确认纸张尺寸与「实际大小」，不要用「适合页面」缩放，否则标签尺寸会偏。",
		CreatedAt: time.Now().UTC(),
	}, nil
}

// shareTransport 走系统分享/隔空打印流程，适合手机。
type shareTransport struct{}

func (shareTransport) Name() string        { return "share" }
func (shareTransport) Kind() TransportKind { return TransportClient }
func (shareTransport) Available() bool     { return true }

func (shareTransport) Send(_ context.Context, req SendRequest) (PrintTask, error) {
	if req.URL == "" {
		return PrintTask{}, ErrTransportUnavailable
	}
	return PrintTask{
		FileID:    req.FileID,
		Transport: "share",
		Kind:      TransportClient,
		URL:       req.URL,
		Filename:  req.Filename,
		Copies:    normalizeCopies(req.Copies),
		Printer:   req.Printer,
		Note:      "请在系统分享菜单中选择打印或目标应用。",
		CreatedAt: time.Now().UTC(),
	}, nil
}

// directTransport 是直连打印的占位实现。
//
// 存在的意义是把接口位置定下来并如实报告「不可用」，而不是让前端去猜
// 有没有这个能力。实现它需要设备注册表与具体协议（如 IPP/ESC-POS），
// 那属于后续按实际设备需求推进的工作。
type directTransport struct{}

func (directTransport) Name() string        { return "direct" }
func (directTransport) Kind() TransportKind { return TransportServer }
func (directTransport) Available() bool     { return false }

func (directTransport) Send(context.Context, SendRequest) (PrintTask, error) {
	return PrintTask{}, ErrTransportUnavailable
}

// TransportRegistry 是发送方式集合，按名称查找。
type TransportRegistry struct {
	byName map[string]Transport
	order  []string
}

// NewTransportRegistry 用给定实现构造注册表。传 nil 时用内置集合。
func NewTransportRegistry(impls []Transport) *TransportRegistry {
	if impls == nil {
		impls = DefaultTransports()
	}
	r := &TransportRegistry{byName: make(map[string]Transport, len(impls))}
	for _, t := range impls {
		if t == nil {
			continue
		}
		name := t.Name()
		if _, dup := r.byName[name]; dup {
			continue
		}
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r
}

// Get 按名称取发送方式。
func (r *TransportRegistry) Get(name string) (Transport, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Names 返回全部标识（含不可用的），供设置页如实列出能力。
func (r *TransportRegistry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Available 返回当前可用的发送方式标识。
func (r *TransportRegistry) Available() []string {
	out := make([]string, 0, len(r.order))
	for _, n := range r.order {
		if r.byName[n].Available() {
			out = append(out, n)
		}
	}
	return out
}

// Send 用指定方式产出任务描述。name 为空时用第一个可用的方式。
func (r *TransportRegistry) Send(ctx context.Context, name string, req SendRequest) (PrintTask, error) {
	if name == "" {
		for _, n := range r.order {
			if r.byName[n].Available() {
				return r.byName[n].Send(ctx, req)
			}
		}
		return PrintTask{}, ErrTransportUnavailable
	}
	t, ok := r.byName[name]
	if !ok || !t.Available() {
		return PrintTask{}, ErrTransportUnavailable
	}
	return t.Send(ctx, req)
}

// normalizeCopies 把份数收敛到 1..50，避免一次生成几千页。
func normalizeCopies(n int) int {
	if n <= 0 {
		return 1
	}
	if n > 50 {
		return 50
	}
	return n
}
