// inventory 路由：库存操作与流水查询。
//
//	POST /api/v1/inventory/operations   执行库存操作（Idempotency-Key 幂等）
//	GET  /api/v1/operations/:id         查询一条流水
//	GET  /api/v1/items/:id/balances     物品分位置余额
package inventory

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

	// 统一幂等：外层 api_idempotency 留业务结果快照，库存操作也能用
	// GET /operation-results/{operation_id} 恢复（库存自有幂等表只做去重）。
	g.POST("/inventory/operations", idem.Middleware(deps.DB), handleOperation(svc, aud))
	g.GET("/operations/:id", handleGetMovement(svc))
	g.GET("/items/:id/balances", handleBalances(svc))
}

func handleOperation(svc *inventoryService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req OpInput
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.AbortBind(c, err)
			return
		}
		// 契约第 4 节：可重试的库存操作必须带 Idempotency-Key。
		req.Key = c.GetHeader("Idempotency-Key")
		actor := httpx.CurrentActor(c)
		res, err := svc.ApplyOp(c.Request.Context(), actor, &req)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "inventory.operation", "item", &req.ItemID, nil,
			gin.H{"kind": req.Kind, "quantity": req.Quantity, "location_id": req.LocationID, "to_location_id": req.ToLocationID})
		httpx.OK(c, res)
	}
}

func handleGetMovement(svc *inventoryService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		actor := httpx.CurrentActor(c)
		mv, err := svc.GetMovement(c.Request.Context(), actor, id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, mv)
	}
}

func handleBalances(svc *inventoryService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		actor := httpx.CurrentActor(c)
		rows, err := svc.Balances(c.Request.Context(), actor, id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"items": rows})
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
