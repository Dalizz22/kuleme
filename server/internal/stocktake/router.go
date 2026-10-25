// stocktake 路由：盘点单的创建、查询与确认调账。
//
//	POST /api/v1/stocktakes           创建 draft 盘点（scope+实点行）
//	GET  /api/v1/stocktakes           分页列表（space_id 过滤）
//	GET  /api/v1/stocktakes/:id       详情（含行与差异）
//	POST /api/v1/stocktakes/:id/confirm  确认：差异 correction 调账
package stocktake

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/idem"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc := NewService(deps.DB)
	g := r.Group("", httpx.Auth(deps.Auth, true))

	g.POST("/stocktakes", idem.Middleware(deps.DB), handleCreate(svc))
	g.GET("/stocktakes", handleList(svc))
	st := g.Group("/stocktakes/:id", httpx.UUIDGuard("id"))
	st.GET("", handleGet(svc))
	st.POST("/confirm", idem.Middleware(deps.DB), handleConfirm(svc))
}

func handleCreate(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateInput
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.AbortBind(c, err)
			return
		}
		st, err := svc.Create(c.Request.Context(), httpx.CurrentActor(c), &req)
		if respondErr(c, err) {
			return
		}
		httpx.Created(c, st)
	}
}

func handleList(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := httpx.ParsePagination(c)
		rows, total, err := svc.List(c.Request.Context(), httpx.CurrentActor(c), c.Query("space_id"), p.Page, p.PageSize)
		if respondErr(c, err) {
			return
		}
		httpx.List(c, rows, p.Page, p.PageSize, int(total))
	}
}

func handleGet(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		st, err := svc.Get(c.Request.Context(), httpx.CurrentActor(c), id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, st)
	}
}

func handleConfirm(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		st, err := svc.Confirm(c.Request.Context(), httpx.CurrentActor(c), id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, st)
	}
}

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
