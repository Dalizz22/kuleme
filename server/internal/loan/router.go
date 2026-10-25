// Package loan 路由：借出、归还与记录查询。
//
//	POST /api/v1/loans             借出（扣库存 + 落凭证）
//	GET  /api/v1/loans             分页查询（status / item_id 过滤）
//	POST /api/v1/loans/:id/return  整单归还（可选 body 指定归还位置）
package loan

import (
	"errors"
	"io"

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

	g.POST("/loans", idem.Middleware(deps.DB), handleCreate(svc, aud))
	g.GET("/loans", handleList(svc))
	g.POST("/loans/:id/return", idem.Middleware(deps.DB), handleReturn(svc, aud))
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
		aud.Try(c, "loan.create", "loan", &row.ID, nil, gin.H{"item_id": req.ItemID, "borrower_name": req.BorrowerName})
		httpx.Created(c, row)
	}
}

func handleList(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := httpx.ParsePagination(c)
		items, total, err := svc.List(c.Request.Context(), httpx.CurrentActor(c),
			c.Query("status"), c.Query("item_id"), p.Page, p.PageSize)
		if respondErr(c, err) {
			return
		}
		httpx.List(c, items, p.Page, p.PageSize, int(total))
	}
}

func handleReturn(svc *service, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		// 请求体可选，只承载归还位置：空 body（io.EOF）合法，其余解析失败拒绝。
		var req struct {
			LocationID *string `json:"location_id"`
		}
		if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
			httpx.Abort(c, httpx.Validation("请求内容格式不正确："+err.Error()))
			return
		}
		row, err := svc.Return(c.Request.Context(), httpx.CurrentActor(c), id, req.LocationID)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "loan.return", "loan", &id, nil, gin.H{"location_id": req.LocationID})
		httpx.OK(c, row)
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
