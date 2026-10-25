// Package httpx 定义统一响应外壳、错误码与请求中间件。
//
// 前端只依赖这里声明的结构：成功 {data, request_id}，失败 {error{...}, request_id}。
// HTTP 状态码本身反映失败，不用 200 包裹错误。
package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

// 错误码。契约约定的最小集合，不在这里穷举业务枚举。
const (
	CodeValidationFailed    = "VALIDATION_FAILED"
	CodeInvalidCode         = "INVALID_CODE"
	CodeUnauthenticated     = "UNAUTHENTICATED"
	CodeSessionExpired      = "SESSION_EXPIRED"
	CodeForbidden           = "FORBIDDEN"
	CodeNotFound            = "NOT_FOUND"
	CodeVersionConflict     = "VERSION_CONFLICT"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeInsufficientStock   = "INSUFFICIENT_STOCK"
	CodeTreeCycle           = "TREE_CYCLE"
	CodeNodeInUse           = "NODE_IN_USE"
	CodeOpenBusiness        = "OPEN_BUSINESS"
	CodeFileTooLarge        = "FILE_TOO_LARGE"
	CodeFileInUse           = "FILE_IN_USE"
	CodeRateLimited         = "RATE_LIMITED"
	CodeServiceUnavailable  = "SERVICE_UNAVAILABLE"
	CodeMaintenance         = "MAINTENANCE"
	CodeInternal            = "INTERNAL"
	// CodeInvalidIdempotencyKey：Idempotency-Key 不是稳定 UUID 或超长。
	// 不提前挡住会让键一路进到 uuid 列，把 SQL 22P02 冒成 500。
	CodeInvalidIdempotencyKey = "INVALID_IDEMPOTENCY_KEY"
	// CodeInvalidImageCount：一次 AI 识别的图片数量超过配置上限（参数类 422）。
	CodeInvalidImageCount = "INVALID_IMAGE_COUNT"
	// CodePayloadTooLarge：请求体超过统一上限（1MB）。
	CodePayloadTooLarge = "PAYLOAD_TOO_LARGE"
)

// Error 是可映射为 HTTP 响应的业务错误。
type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// New 构造业务错误。
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// WithDetails 附加字段级或业务细节。
func (e *Error) WithDetails(details map[string]any) *Error {
	e.Details = details
	return e
}

// 常用错误构造器。文案面向使用者，不出现技术术语。
// Validation 是参数/请求体校验类错误：统一 422（请求可解析但语义不成立）。
// 400 不再作为业务错误出口，客户端按错误码映射即可。
func Validation(message string) *Error {
	return New(http.StatusUnprocessableEntity, CodeValidationFailed, message)
}

func InvalidCode(message string) *Error {
	return New(http.StatusUnprocessableEntity, CodeInvalidCode, message)
}

func Unauthenticated(message string) *Error {
	return New(http.StatusUnauthorized, CodeUnauthenticated, message)
}

func Forbidden(message string) *Error {
	return New(http.StatusForbidden, CodeForbidden, message)
}

func NotFound(message string) *Error {
	return New(http.StatusNotFound, CodeNotFound, message)
}

func Conflict(code, message string) *Error {
	return New(http.StatusConflict, code, message)
}

// Internal 500 统一出口。错误细节只进日志（slog 默认 logger 即应用日志），
// 响应体保持通用文案——err.Error 可能带 SQL/路径等敏感信息，不外泄。
func Internal(err error) *Error {
	if err != nil {
		slog.Error("内部错误", "error", err.Error())
	}
	return New(http.StatusInternalServerError, CodeInternal, "服务处理失败，请稍后重试")
}

func Unavailable(message string) *Error {
	return New(http.StatusServiceUnavailable, CodeServiceUnavailable, message)
}

func RateLimited(message string) *Error {
	return New(http.StatusTooManyRequests, CodeRateLimited, message)
}

// PayloadTooLarge 是请求体超过统一上限（413）。
func PayloadTooLarge(message string) *Error {
	return New(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, message)
}

// Respond 写入统一成功外壳。
func Respond(c *gin.Context, status int, data any) {
	c.JSON(status, gin.H{"data": data, "request_id": RequestID(c)})
}

// OK 是 status 200 的简写。
func OK(c *gin.Context, data any) { Respond(c, http.StatusOK, data) }

// Created 是 status 201 的简写。
func Created(c *gin.Context, data any) { Respond(c, http.StatusCreated, data) }

// List 写入分页列表外壳。
func List(c *gin.Context, items any, page, pageSize, total int) {
	OK(c, gin.H{
		"items":     items,
		"page":      page,
		"page_size": pageSize,
		"total":     total,
	})
}

// Abort 写入统一失败外壳并终止处理链。
func Abort(c *gin.Context, err error) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		apiErr = Internal(err)
	}
	payload := gin.H{
		"code":    apiErr.Code,
		"message": apiErr.Message,
		"details": apiErr.Details,
	}
	if apiErr.Details == nil {
		payload["details"] = gin.H{}
	}
	c.AbortWithStatusJSON(apiErr.Status, gin.H{
		"error":      payload,
		"request_id": RequestID(c),
	})
}

// maxJSONBody 是 JSON 请求体的统一上限：1MB。超限在解析阶段就 413，
// 不让超大字符串一路进 SQL（PG 会对超长索引行报 54000 → 500）。
const maxJSONBody = 1 << 20

// BindJSON 解析请求体，失败时返回统一的字段错误。
// 文案只用 json 字段名与中文说明：不回显 Go 结构体名、binding tag 或英文原文。
func BindJSON(c *gin.Context, target any) error {
	if c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBody)
	}
	if err := c.ShouldBindJSON(target); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return PayloadTooLarge("请求内容过大，不能超过 1MB")
		}
		return Validation(describeBindError(target, err))
	}
	return nil
}

// AbortBind 把 BindJSON 的失败原样写回：413（PAYLOAD_TOO_LARGE）必须保持 413，
// 422 保持 422 的中文文案。历史上各 handler 自己包一层 Validation(err.Error)，
// 把 413 改写成 422 并把码名拼进了 message（web 实测 1.5MB → 422）。
// 非 *httpx.Error 才降级成 422，且用中文兜底文案，不回显内部错误。
func AbortBind(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		Abort(c, apiErr)
		return true
	}
	Abort(c, Validation("请求内容格式不正确"))
	return true
}

// describeBindError 把 validator/JSON 错误翻译成中文，字段名用 json tag。
func describeBindError(target any, err error) string {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs) == 0 {
		return "请求内容格式不正确"
	}
	fe := verrs[0]
	name := jsonFieldName(target, fe.StructField())
	switch fe.Tag() {
	case "required":
		return "缺少必填字段 " + name
	case "uuid", "uuid4":
		return "字段 " + name + " 必须是合法 ID"
	case "oneof":
		return "字段 " + name + " 取值不合法"
	case "email":
		return "字段 " + name + " 不是合法邮箱"
	case "min":
		return "字段 " + name + " 过短"
	case "max":
		return "字段 " + name + " 过长"
	default:
		return "字段 " + name + " 格式不正确"
	}
}

// jsonFieldName 按结构体字段找到 json tag 名，找不到时退化为小写字段名。
func jsonFieldName(target any, structField string) string {
	t := reflect.TypeOf(target)
	for t != nil && (t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice) {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return strings.ToLower(structField)
	}
	if f, ok := t.FieldByName(structField); ok {
		if tag := f.Tag.Get("json"); tag != "" {
			if name := strings.Split(tag, ",")[0]; name != "" && name != "-" {
				return name
			}
		}
	}
	return strings.ToLower(structField)
}
