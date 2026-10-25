// Package audit 路由：审计日志查询。
//
//	GET /api/v1/audit  仅管理员，分页查询操作留痕
//
// 没有写接口：审计记录由各业务模块在关键动作后调用 Service.Record 落库，
// 不开放 HTTP 创建，避免留痕被外部伪造。
package audit

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。deps.Auth 存在时挂统一鉴权中间件；
// 为 nil 时 handler 用 requireActor 显式拒绝匿名（appdeps 约定的退化路径）。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc := New(deps.DB)
	var g *gin.RouterGroup
	if deps.Auth != nil {
		g = r.Group("", httpx.Auth(deps.Auth, true))
	} else {
		g = r.Group("")
	}
	g.GET("/audit", handleList(svc))
}

func handleList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !svc.db.Available() {
			respondErr(c, store.ErrUnavailable)
			return
		}
		actor := httpx.CurrentActor(c)
		if actor.UserID == "" {
			httpx.Abort(c, httpx.Unauthenticated("需要登录"))
			return
		}
		if !actor.IsAdmin {
			httpx.Abort(c, httpx.Forbidden("仅管理员可查看审计日志"))
			return
		}
		p := httpx.ParsePagination(c)
		items, total, err := svc.List(c.Request.Context(), p.Page, p.PageSize,
			c.Query("object_type"), c.Query("object_id"), c.Query("actor_id"))
		if respondErr(c, err) {
			return
		}
		httpx.List(c, items, p.Page, p.PageSize, int(total))
	}
}

// respondErr 服务层错误 → 统一响应外壳。返回 true 表示已响应，handler 应返回。
func respondErr(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrUnavailable) {
		httpx.Abort(c, httpx.Unavailable("服务暂时不可用，请稍后重试"))
		return true
	}
	var httpErr *httpx.Error
	if errors.As(err, &httpErr) {
		httpx.Abort(c, httpErr)
		return true
	}
	httpx.Abort(c, httpx.Internal(err))
	return true
}
