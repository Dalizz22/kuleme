// Package sale 路由：售卖与记录查询。
//
//	POST /api/v1/sales  售卖（预占释放 + 实物出库 + 落凭证）
//	GET  /api/v1/sales  分页查询（item_id 过滤）
package sale

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/audit"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/idem"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc := newService(deps.DB)
	aud := audit.New(deps.DB)
	g := r.Group("", httpx.Auth(deps.Auth, true))

	g.POST("/sales", idem.Middleware(deps.DB), handleCreate(svc, aud))
	g.GET("/sales", handleList(svc))
}

func handleCreate(svc *service, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateInput
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.Abort(c, err)
			return
		}
		row, err := svc.Create(c.Request.Context(), httpx.CurrentActor(c), &req)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "sale.create", "sale", &row.ID, nil, gin.H{"item_id": req.ItemID, "quantity": req.Quantity})
		httpx.Created(c, row)
	}
}

func handleList(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := httpx.ParsePagination(c)
		items, total, err := svc.List(c.Request.Context(), httpx.CurrentActor(c),
			c.Query("item_id"), p.Page, p.PageSize)
		if respondErr(c, err) {
			return
		}
		httpx.List(c, items, p.Page, p.PageSize, int(total))
	}
}

// respondErr 服务层错误 → 统一响应外壳，与 inventory 同一套映射。
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
