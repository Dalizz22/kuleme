package httpx

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// AuthCarrier 是鉴权中间件与具体会话实现之间的唯一接口。
// 由核心后端（B）实现，httpx 不认识会话表、Cookie 或令牌格式。
type AuthCarrier interface {
	// Authenticate 解析请求中的凭据。无法识别或已失效时返回 ok=false。
	// 返回的错误用于区分「未登录」与「依赖不可用」。
	Authenticate(c *gin.Context) (Actor, bool, error)
}

// Auth 建立鉴权中间件，并把操作者写入请求上下文。
func Auth(carrier AuthCarrier, required bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok, err := carrier.Authenticate(c)
		if err != nil {
			Abort(c, Unavailable("服务暂时不可用，请稍后重试"))
			return
		}
		if ok {
			c.Request = c.Request.WithContext(ContextWithActor(c.Request.Context(), actor))
			c.Next()
			return
		}
		if required {
			// 会话令牌有值但查不到，视为过期而非未登录，客户端据此清理本地会话。
			if c.GetHeader("Authorization") != "" || hasSessionCookie(c) {
				Abort(c, New(http.StatusUnauthorized, CodeSessionExpired, "登录已失效，请重新登录"))
				return
			}
			Abort(c, Unauthenticated("请先登录"))
			return
		}
		c.Next()
	}
}

// hasSessionCookie 只判断是否携带会话 Cookie，不解释其内容。
func hasSessionCookie(c *gin.Context) bool {
	_, err := c.Cookie(SessionCookieName)
	return err == nil
}

// SessionCookieName 是 Web 同源会话的 Cookie 名，App 使用 Authorization: Bearer。
const SessionCookieName = "klm_session"

// CurrentActor 读取当前操作者，未登录时返回零值。
func CurrentActor(c *gin.Context) Actor { return ActorFrom(c.Request.Context()) }

// RequireActor 要求已登录。业务路由在 Auth(..., true) 之后仍可显式调用，
// 用于依赖 D 模块未挂中间件时的自保。
func RequireActor(c *gin.Context) (Actor, error) {
	actor := CurrentActor(c)
	if !actor.Authenticated() {
		return Actor{}, Unauthenticated("请先登录")
	}
	return actor, nil
}

// ErrUnauthenticated 供服务层在缺少操作者时返回。
var ErrUnauthenticated = errors.New("未登录")
