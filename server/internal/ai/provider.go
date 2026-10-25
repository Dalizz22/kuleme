package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 本文件实现内置的识别提供者：一个「OpenAI 兼容」的视觉模型客户端。
//
// 为什么一个客户端就够了：只要求「用户自配 base_url / api_key / model」。
// 各家兼容接口的差异集中在三处（鉴权头、图片编码、结构化输出的字段名），
// 与其为每家写一个客户端，不如把这三处做成可选项并如实报错。
//
// 三条安全约定：
//
//  1. 图片只在用户触发识别时发送，且只发这次任务引用的图片；
//  2. 密钥不出现在日志、错误信息、返回体里；
//  3. 模型返回的文本**只当数据**：先解 JSON，再逐字段校验，
//     绝不把它拼进下一个请求，也绝不当成指令执行。

// ProviderName 是内置提供者的稳定标识。
const ProviderName = "openai-compatible"

// providerClient 是 OpenAI 兼容聊天补全客户端的实现。
type providerClient struct {
	http    *http.Client
	timeout time.Duration
}

// NewProvider 构造内置提供者客户端。
//
// timeout 为 0 时用 DefaultTimeoutSeconds：视觉模型的响应时间波动很大，
// 太短会把「模型慢」误报成「服务坏了」，太长会让 worker 白占名额。
func NewProvider(timeout time.Duration) Recognizer {
	if timeout <= 0 {
		timeout = time.Duration(DefaultTimeoutSeconds) * time.Second
	}
	return &providerClient{
		// 不用 http.DefaultClient：它没有超时，一个挂住的外部服务能把
		// worker 永久占住。Transport 复用默认值以获得连接池。
		http:    &http.Client{Timeout: timeout},
		timeout: timeout,
	}
}

func (p *providerClient) Name() string { return ProviderName }

// chatRequest 是请求体。
//
// 只用到兼容接口的最小公共子集：model + messages + max_tokens + temperature。
// 不用各家私有的结构化输出扩展：不支持的接口会直接报参数错误，
// 而那正是用户需要看到的信号——「这个模型/网关不支持我们要的用法」。
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Recognize 调用一次视觉识别。
//
// 返回值里的 Observations 是**未校验的原始观察**；调用方必须再过模板校验。
func (p *providerClient) Recognize(ctx context.Context, req RecognizeRequest) (RecognizeResponse, error) {
	if len(req.Images) == 0 {
		return RecognizeResponse{}, fmt.Errorf("%w: 没有可识别的图片", ErrProviderFailed)
	}
	cfg, ok := SettingsFromContext(ctx)
	if !ok || cfg.BaseURL == "" || cfg.Model == "" {
		return RecognizeResponse{}, ErrNotConfigured
	}
	if !cfg.Enabled {
		return RecognizeResponse{}, ErrDisabled
	}

	body, err := json.Marshal(p.buildRequest(cfg, req))
	if err != nil {
		return RecognizeResponse{}, fmt.Errorf("%w: %v", ErrProviderFailed, err)
	}

	// 用户常把完整 endpoint 复制进 base_url（…/v1 或 …/v1/chat/completions）。
	// 已含 /chat/completions 直接用；否则视为 base，拼接 endpoint。
	url := strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(url, "/chat/completions") {
		url += "/chat/completions"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return RecognizeResponse{}, fmt.Errorf("%w: %v", ErrProviderFailed, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	// 兼容网关常用这个头做路由，不传也不影响直连大厂接口。
	httpReq.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := p.http.Do(httpReq)
	if err != nil {
		// 超时与限流要分开：超时可重试，限流要按 Retry-After 退避。
		if isTimeout(err) {
			return RecognizeResponse{}, fmt.Errorf("%w: %v", ErrTimeout, sanitizeErr(err, cfg))
		}
		return RecognizeResponse{}, fmt.Errorf("%w: %v", ErrProviderFailed, sanitizeErr(err, cfg))
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return RecognizeResponse{}, fmt.Errorf("%w: 读取响应失败", ErrProviderFailed)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return RecognizeResponse{}, &RateLimitError{
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Message:    "AI 提供者限流",
		}
	}
	if resp.StatusCode >= 400 {
		return RecognizeResponse{}, fmt.Errorf("%w: HTTP %d%s",
			ErrProviderFailed, resp.StatusCode, providerErrorDetail(raw, cfg))
	}

	out, err := parseChatResponse(raw, cfg)
	if err != nil {
		return RecognizeResponse{}, err
	}
	_ = start
	return out, nil
}

// maxResponseBytes 是响应体上限。
//
// 砍在 8 MB：模型回一大段废话很正常，但不该让一个坏掉（或被劫持）的
// 端点把 worker 的内存吃掉。
const maxResponseBytes = 8 << 20

func (p *providerClient) buildRequest(cfg Settings, req RecognizeRequest) chatRequest {
	parts := make([]contentPart, 0, len(req.Images)+1)
	parts = append(parts, contentPart{Type: "text", Text: buildPrompt(req)})
	for _, img := range req.Images {
		ct := img.ContentType
		if ct == "" {
			ct = "image/jpeg"
		}
		parts = append(parts, contentPart{
			Type: "image_url",
			ImageURL: &imageURL{
				// data URL：图片不经过第三方图床，一次请求内自包含。
				URL: "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	return chatRequest{
		Model:       cfg.Model,
		Messages:    []chatMessage{{Role: "user", Content: parts}},
		MaxTokens:   2048,
		Temperature: 0.2,
	}
}

// parseRetryAfter 解析 Retry-After 头（秒数或 HTTP 日期）。
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		if secs > 3600 {
			secs = 3600
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 && d <= time.Hour {
			return d
		}
	}
	return 0
}

// providerErrorDetail 从错误响应里抽出可用信息，并确保密钥不外泄。
//
// 只取 error.message 与 error.type，且过一遍 sanitize：有些网关会把
// 请求里的 Authorization 头回显在错误体里，直接透传等于把密钥写进日志。
func providerErrorDetail(raw []byte, cfg Settings) string {
	var resp chatResponse
	if err := json.Unmarshal(raw, &resp); err == nil && resp.Error != nil {
		msg := sanitizeText(resp.Error.Message, cfg)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if resp.Error.Type != "" {
			return "（" + resp.Error.Type + "：" + msg + "）"
		}
		return "（" + msg + "）"
	}
	// 非 JSON 错误体：不要透传原文，它可能很长也可能带密钥。
	snippet := sanitizeText(string(raw), cfg)
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	if snippet == "" {
		return ""
	}
	return "（" + snippet + "）"
}

// sanitizeErr 把网络错误里可能出现的密钥抹掉。
func sanitizeErr(err error, cfg Settings) error {
	if err == nil || cfg.APIKey == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), cfg.APIKey, "***")
	return fmt.Errorf("%s", msg)
}

func sanitizeText(s string, cfg Settings) string {
	if cfg.APIKey != "" {
		s = strings.ReplaceAll(s, cfg.APIKey, "***")
	}
	return strings.TrimSpace(s)
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	if errors.As(err, &t) {
		return t.Timeout()
	}
	return strings.Contains(err.Error(), "context deadline exceeded")
}
