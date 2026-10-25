package httpx

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/config"
)

// corsMethods 是允许的跨域方法，与路由实际使用的方法一致。
const corsMethods = "GET,POST,PUT,PATCH,DELETE,OPTIONS"

// corsHeaders 是允许的请求头：认证、幂等键、条件更新与 JSON 内容。
const corsHeaders = "Content-Type,Authorization,Idempotency-Key,If-Match"

// Cors 返回受限的跨域中间件。
//
// 只回显白名单里的来源（KLM_CORS_ORIGINS，精确匹配 scheme+主机+端口），
// 从不回 Access-Control-Allow-Credentials，也不接受 * ；
// development 模式额外放行局域网 http(s) 来源，方便手机真机联调。
// 预检直接 204 结束，避免落到 NoRoute 变成 404；
// 未列出的来源：预检 403，普通请求继续但不加任何 CORS 头。
func Cors(cfg config.Config) gin.HandlerFunc {
	allowed := make(map[string]bool, len(cfg.CORSOrigins))
	for _, o := range cfg.CORSOrigins {
		if norm := normalizeOrigin(o); norm != "" && norm != "*" {
			allowed[norm] = true
		}
	}
	development := !cfg.IsProduction()
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}
		permitted := allowed[normalizeOrigin(origin)]
		if !permitted && development && isLANOrigin(origin) {
			permitted = true
		}
		preflight := c.Request.Method == http.MethodOptions
		if !permitted {
			if preflight {
				Abort(c, Forbidden("该来源不被允许跨域访问"))
				return
			}
			c.Next()
			return
		}
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		if !preflight {
			c.Next()
			return
		}
		h.Set("Access-Control-Allow-Methods", corsMethods)
		h.Set("Access-Control-Allow-Headers", corsHeaders)
		h.Set("Access-Control-Max-Age", "600")
		c.AbortWithStatus(http.StatusNoContent)
	}
}

// normalizeOrigin 把来源统一成 scheme://host[:port] 形式，便于精确比较。
// 含路径或带 * 的值返回空串（视为配置错误，不放行）。
func normalizeOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// isLANOrigin 判断来源是否为局域网/环回地址的 http(s) 站点（开发模式专用）。
func isLANOrigin(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback())
}
