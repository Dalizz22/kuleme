package httpx

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const requestIDKey = "request_id"

// RequestIDMiddleware 为每个请求生成标识，响应头 X-Request-ID 与响应体使用同一个值。
// 客户端传入的 X-Request-ID 会被沿用，便于端到端串联日志。
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(requestIDKey, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// RequestID 读取当前请求标识，缺失时返回空串。
func RequestID(c *gin.Context) string {
	if v, ok := c.Get(requestIDKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// AccessLog 记录方法、路径、状态与耗时。不记录请求体、密码、令牌或 AI key。
func AccessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Info("http",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"ms", time.Since(start).Milliseconds(),
			"request_id", RequestID(c),
		)
	}
}

// Recover 把 panic 转成 500，不让单个请求结束进程。
func Recover(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic", "path", c.Request.URL.Path, "request_id", RequestID(c), "panic", r)
				Abort(c, Internal(nil))
			}
		}()
		c.Next()
	}
}

// NoRoute 对未声明路径返回统一错误，不回落成 HTML。
func NoRoute(c *gin.Context) {
	Abort(c, NotFound("接口不存在"))
}

// Middleware 返回全站通用中间件，顺序固定：先有请求标识，再记录访问日志，
// 最后兜底 panic。鉴权由各模块按路由需要单独挂载。
func Middleware(log *slog.Logger) []gin.HandlerFunc {
	return []gin.HandlerFunc{RequestIDMiddleware(), AccessLog(log), Recover(log)}
}
