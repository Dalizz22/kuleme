// Package jobs 路由：任务入队与查询。
//
//	POST /api/v1/jobs  已登录，入队一个任务
//	GET  /api/v1/jobs  已登录，分页查询任务
package jobs

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。deps.Auth 存在时挂统一鉴权中间件；
// 为 nil 时每个 handler 用 requireActor 显式拒绝匿名（appdeps 约定的退化路径）。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc := New(deps.DB)
	g := r.Group("")
	if deps.Auth != nil {
		g = r.Group("", httpx.Auth(deps.Auth, true))
	}
	g.POST("/jobs", handleEnqueue(svc))
	g.GET("/jobs", handleList(svc))
}

func handleEnqueue(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !depsAvailable(c, svc) {
			return
		}
		actor, ok := requireActor(c)
		if !ok {
			return
		}
		var req struct {
			Kind    string          `json:"kind" binding:"required"`
			Payload json.RawMessage `json:"payload"`
			RunAt   *time.Time      `json:"run_at"`
		}
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.Abort(c, err)
			return
		}
		job, err := svc.Enqueue(c.Request.Context(), actor, req.Kind, req.Payload, req.RunAt)
		if err != nil {
			respondErr(c, err)
			return
		}
		// 202：任务已受理（同步执行完返回终态），不用 200 资源语义。
		httpx.Respond(c, http.StatusAccepted, gin.H{"job": job})
	}
}

func handleList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !depsAvailable(c, svc) {
			return
		}
		actor, ok := requireActor(c)
		if !ok {
			return
		}
		// 任务表没有 space/created_by 归属列（决定：不加列、不造权限模型），
		// 列表是实例级队列视图，只对实例管理员开放；详情类读取登录即可。
		if !actor.IsAdmin {
			httpx.Abort(c, httpx.Forbidden("仅实例管理员可查看任务队列"))
			return
		}
		p := httpx.ParsePagination(c)
		items, total, err := svc.List(c.Request.Context(), c.Query("status"), p.Page, p.PageSize)
		if err != nil {
			respondErr(c, err)
			return
		}
		httpx.List(c, items, p.Page, p.PageSize, int(total))
	}
}

// requireActor 显式拒绝匿名请求：deps.Auth 为 nil 时没有中间件兜底，
// 每个 handler 都从这里过一遍。
func requireActor(c *gin.Context) (httpx.Actor, bool) {
	actor := httpx.CurrentActor(c)
	if actor.UserID == "" {
		httpx.Abort(c, httpx.Unauthenticated("需要登录"))
		return actor, false
	}
	return actor, true
}

// depsAvailable 数据库不可用时统一返回 503。
func depsAvailable(c *gin.Context, svc *Service) bool {
	if svc.db.Available() {
		return true
	}
	respondErr(c, store.ErrUnavailable)
	return false
}

// respondErr 把服务层错误翻译成统一响应外壳。
func respondErr(c *gin.Context, err error) {
	if errors.Is(err, store.ErrUnavailable) {
		httpx.Abort(c, httpx.Unavailable("服务暂时不可用，请稍后重试"))
		return
	}
	var httpErr *httpx.Error
	if errors.As(err, &httpErr) {
		httpx.Abort(c, httpErr)
		return
	}
	httpx.Abort(c, httpx.Internal(err))
}
