package httpx

import (
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// CSRFProtection 保护 Cookie 会话免受跨站写请求：
// 不安全方法（写操作）若凭 Cookie 鉴权（无 Bearer 头），必须携带与请求
// Host 一致的 Origin 或 Referer；跨站浏览器会带上自己的 Origin，
// 缺失即拒绝。Bearer 客户端（App）不受影响。
//
// ponytail: SameSite=Lax Cookie + Origin 校验已覆盖 CSRF 主路径；
// 若未来出现无法携带 Origin 的合法客户端，再换成显式 CSRF Token。
func CSRFProtection() gin.HandlerFunc {
	unsafe := map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}
	return func(c *gin.Context) {
		if !unsafe[c.Request.Method] {
			c.Next()
			return
		}
		if c.GetHeader("Authorization") != "" {
			c.Next() // Bearer 无 Cookie 面
			return
		}
		if _, err := c.Cookie(SessionCookieName); err != nil {
			c.Next() // 匿名/未带会话的请求走各自的 Auth
			return
		}
		if originOK(c.GetHeader("Origin"), c) || originOK(c.GetHeader("Referer"), c) {
			c.Next()
			return
		}
		Abort(c, Forbidden("跨站请求被拒绝"))
	}
}

func originOK(raw string, c *gin.Context) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, c.Request.Host)
}
