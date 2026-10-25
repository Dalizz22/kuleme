// Package item 路由：核心资料 CRUD。
//
//	GET    /api/v1/items                 列表（可见库过滤 + template_key + keyword + 分页）
//	POST   /api/v1/items                 新建（要求对库 write）
//	GET    /api/v1/items/:id             详情
//	PUT    /api/v1/items/:id             更新（expected_version 乐观锁）
//	DELETE /api/v1/items/:id             软删除（有在手库存则 409）
package item

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/audit"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/idem"
	"github.com/klm/server/internal/rules"
)

// Register 挂载本模块路由。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc, err := NewService(deps.DB)
	if err == nil {
		// field_changed 规则触发接线（item→rules 无 import 环）。
		svc.SetRuleTrigger(rules.NewService(deps.DB))
	}
	if err != nil {
		// 模板资产损坏是启动期问题：不挂路由，让健康检查与日志暴露。
		deps.Logger.Error("item 模块初始化失败", "err", err)
		return
	}
	g := r.Group("", httpx.Auth(deps.Auth, true))
	aud := audit.New(deps.DB)

	g.GET("/items", handleList(svc))
	g.POST("/items", idem.Middleware(deps.DB), handleCreate(svc, aud))
	// 所有 /items/:id 入口先过 uuid 守卫：垃圾路径参数 404，不进 SQL。
	byID := g.Group("/items/:id", httpx.UUIDGuard("id"))
	byID.GET("", handleGet(svc))
	byID.PUT("", handleUpdate(svc, aud))
	byID.DELETE("", handleDelete(svc, aud))
	byID.POST("/sale", idem.Middleware(deps.DB), handleSale(svc, aud))
	byID.POST("/reverse", idem.Middleware(deps.DB), handleReverseSale(svc, aud))
	byID.PUT("/for-sale", handleForSale(svc))
	byID.POST("/transfer-preview", handleTransferPreview(svc))
	byID.POST("/transfer", idem.Middleware(deps.DB), handleTransfer(svc, aud))
	byID.POST("/maintenance", idem.Middleware(deps.DB), handleStartMaintenance(svc, aud))
	byID.POST("/maintenance/close", idem.Middleware(deps.DB), handleCloseMaintenance(svc, aud))
	byID.GET("/maintenance", handleMaintenanceHistory(svc))
	byID.GET("/duplicate-hints", handleDuplicateHints(svc))
	byID.GET("/photos", handleListPhotos(svc))
	byID.POST("/photos", handleLinkPhotos(svc, aud))
	byID.DELETE("/photos/:file_id", httpx.UUIDGuard("file_id"), handleUnlinkPhoto(svc, aud))
	byID.PUT("/photos/order", handleReorderPhotos(svc, aud))
	byID.PUT("/photos/:file_id/primary", httpx.UUIDGuard("file_id"), handleSetPrimaryPhoto(svc, aud))
	g.POST("/dedup/preview", handleDedupPreview(svc))
	g.POST("/dedup/merge", idem.Middleware(deps.DB), handleDedupMerge(svc, aud))
}

func handleList(svc *itemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		page := httpx.ParsePagination(c)
		statuses, err := ParseStatuses(c.QueryArray("status"))
		if respondErr(c, err) {
			return
		}
		actor := httpx.CurrentActor(c)
		views, total, err := svc.List(c.Request.Context(), actor,
			c.Query("template_key"), c.Query("keyword"), statuses, page.Offset, page.Limit())
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{
			"items":     views,
			"page":      page.Page,
			"page_size": page.PageSize,
			"total":     total,
		})
	}
}

func handleCreate(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateInput
		if err := httpx.BindJSON(c, &req); respondBind(c, err) {
			return
		}
		if req.TemplateKey == "" {
			req.TemplateKey = svc.tcat.DefaultKey()
		}
		// 逐件制：management 已从 CreateInput 移除，服务端恒 serial。
		if req.Unit == "" {
			req.Unit = "件"
		}
		// 没显式给 creation_ref 时用幂等键兜底，崩溃后可按 operation_id
		// 反查已提交物品（唯一约束也挡住同键双建档）。
		if req.CreationRef == "" {
			req.CreationRef = idem.KeyFrom(c)
		}
		actor := httpx.CurrentActor(c)
		view, err := svc.Create(c.Request.Context(), actor, &req)
		if respondErr(c, err) {
			return
		}
		// 201 响应与详情同构，动作可用性恒为对象（Item schema 要求）。
		if a := svc.availability(c.Request.Context(), actor, view.ID); a.AllowedActions != nil {
			view.AllowedActions = a.AllowedActions
			view.ActionBlockedReasons = a.ActionBlockedReasons
		}
		spaceID := req.SpaceID
		aud.Try(c, "item.create", "item", &view.ID, &spaceID,
			gin.H{"name": req.Name, "template_key": req.TemplateKey})
		httpx.Created(c, view)
	}
}

func handleGet(svc *itemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		actor := httpx.CurrentActor(c)
		view, err := svc.Get(c.Request.Context(), actor, id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, view)
	}
}

func handleUpdate(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		var req struct {
			Name            string         `json:"name"`
			Note            *string        `json:"note"`
			Fields          map[string]any `json:"fields"`
			ExpectedVersion int            `json:"expected_version" binding:"required,min=1"`
		}
		if err := httpx.BindJSON(c, &req); respondBind(c, err) {
			return
		}
		actor := httpx.CurrentActor(c)
		in := &UpdateInput{Name: req.Name, Note: req.Note, Fields: req.Fields, ExpectedVersion: req.ExpectedVersion}
		view, err := svc.Update(c.Request.Context(), actor, id, in)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.update", "item", &id, nil, gin.H{"expected_version": req.ExpectedVersion})
		httpx.OK(c, view)
	}
}

func handleDelete(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		actor := httpx.CurrentActor(c)
		if respondErr(c, svc.Delete(c.Request.Context(), actor, id)) {
			return
		}
		aud.Try(c, "item.delete", "item", &id, nil, nil)
		httpx.OK(c, nil)
	}
}

func respondBind(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	// BindJSON 已经返回带状态码的 *httpx.Error（422 校验 / 413 超限），原样透传，
	// 不再包一层 Validation 把 413 改写成 422，也不回显内部错误文本。
	httpx.Abort(c, err)
	return true
}

// respondErr 服务层错误 → 统一响应外壳，返回 true 表示已响应。
func respondErr(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	var httpErr *httpx.Error
	if errors.As(err, &httpErr) {
		httpx.Abort(c, httpErr)
		return true
	}
	httpx.Abort(c, httpx.Internal(err))
	return true
}

func handleTransferPreview(svc *itemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in TransferInput
		if err := c.ShouldBindJSON(&in); err != nil {
			httpx.Abort(c, httpx.Validation("请求体不完整：需要 to_space_id、to_location_id 与 expected_version"))
			return
		}
		preview, err := svc.TransferPreview(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"), &in)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"preview": preview})
	}
}

func handleTransfer(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in TransferInput
		if err := c.ShouldBindJSON(&in); err != nil {
			httpx.Abort(c, httpx.Validation("请求体不完整：需要 to_space_id、to_location_id 与 expected_version"))
			return
		}
		actor := httpx.CurrentActor(c)
		view, err := svc.Transfer(c.Request.Context(), actor, c.Param("id"), &in)
		if respondErr(c, err) {
			return
		}
		toSpace := in.ToSpaceID
		aud.Try(c, "item.transfer", "item", &view.ID, &toSpace,
			gin.H{"to_space_id": in.ToSpaceID, "to_location_id": in.ToLocationID})
		httpx.OK(c, view)
	}
}

func handleSale(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in SaleInput
		if err := httpx.BindJSON(c, &in); respondBind(c, err) {
			return
		}
		actor := httpx.CurrentActor(c)
		view, err := svc.Sale(c.Request.Context(), actor, c.Param("id"), &in)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.sale", "item", &view.ID, nil, gin.H{"price": in.Price})
		httpx.OK(c, view)
	}
}

func handleReverseSale(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := httpx.CurrentActor(c)
		view, err := svc.ReverseSale(c.Request.Context(), actor, c.Param("id"))
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.sale_reverse", "item", &view.ID, nil, nil)
		httpx.OK(c, view)
	}
}

func handleForSale(svc *itemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			ForSale bool `json:"for_sale"`
		}
		if err := httpx.BindJSON(c, &in); respondBind(c, err) {
			return
		}
		view, err := svc.SetForSale(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"), in.ForSale)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, view)
	}
}

func handleStartMaintenance(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in maintenanceInput
		if err := httpx.BindJSON(c, &in); respondBind(c, err) {
			return
		}
		in.OperationID = idem.KeyFrom(c)
		actor := httpx.CurrentActor(c)
		rec, err := svc.StartMaintenance(c.Request.Context(), actor, c.Param("id"), &in)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.maintenance_start", "item", &rec.ItemID, nil, gin.H{"reason": rec.Reason})
		httpx.Created(c, rec)
	}
}

func handleCloseMaintenance(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := httpx.CurrentActor(c)
		rec, err := svc.CloseMaintenance(c.Request.Context(), actor, c.Param("id"), idem.KeyFrom(c))
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.maintenance_close", "item", &rec.ItemID, nil, nil)
		httpx.OK(c, rec)
	}
}

func handleMaintenanceHistory(svc *itemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := httpx.CurrentActor(c)
		recs, err := svc.MaintenanceHistory(c.Request.Context(), actor, c.Param("id"))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"records": recs})
	}
}

func handleDuplicateHints(svc *itemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := httpx.CurrentActor(c)
		hints, err := svc.DuplicateHints(c.Request.Context(), actor, c.Param("id"))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"hints": hints})
	}
}

func handleDedupPreview(svc *itemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in MergePreviewInput
		if err := httpx.BindJSON(c, &in); respondBind(c, err) {
			return
		}
		view, err := svc.PreviewMerge(c.Request.Context(), httpx.CurrentActor(c), &in)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, view)
	}
}

func handleDedupMerge(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in MergePreviewInput
		if err := httpx.BindJSON(c, &in); respondBind(c, err) {
			return
		}
		view, err := svc.Merge(c.Request.Context(), httpx.CurrentActor(c), &in)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.dedup_merge", "item", &view.ID, nil, gin.H{"merge_ids": in.MergeIDs})
		httpx.OK(c, view)
	}
}

// ---- 照片：关联/解除/排序/主图 ----
//
// 响应统一 data {photos: [PhotoRef]}，按 sort 升序，恰好一条 is_primary=true。
// 不挂 idem.Middleware：关联/解除/排序/设主图天然幂等（重复请求得到同一终态），
// 且失败不留半关联，重试安全。

func handleListPhotos(svc *itemService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		photos, err := svc.ListPhotos(c.Request.Context(), httpx.CurrentActor(c), id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"photos": photos})
	}
}

func handleLinkPhotos(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		var req photoLinkInput
		if err := httpx.BindJSON(c, &req); respondBind(c, err) {
			return
		}
		photos, err := svc.LinkPhotos(c.Request.Context(), httpx.CurrentActor(c), id, req)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.photos.link", "item", &id, nil, gin.H{"file_ids": req.FileIDs})
		httpx.OK(c, gin.H{"photos": photos})
	}
}

func handleUnlinkPhoto(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		fileID := c.Param("file_id")
		photos, err := svc.UnlinkPhoto(c.Request.Context(), httpx.CurrentActor(c), id, fileID)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.photos.unlink", "item", &id, nil, gin.H{"file_id": fileID})
		httpx.OK(c, gin.H{"photos": photos})
	}
}

func handleReorderPhotos(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		var req photoOrderInput
		if err := httpx.BindJSON(c, &req); respondBind(c, err) {
			return
		}
		photos, err := svc.ReorderPhotos(c.Request.Context(), httpx.CurrentActor(c), id, req)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.photos.order", "item", &id, nil, gin.H{"file_ids": req.FileIDs})
		httpx.OK(c, gin.H{"photos": photos})
	}
}

func handleSetPrimaryPhoto(svc *itemService, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		fileID := c.Param("file_id")
		photos, err := svc.SetPrimaryPhoto(c.Request.Context(), httpx.CurrentActor(c), id, fileID)
		if respondErr(c, err) {
			return
		}
		aud.Try(c, "item.photos.primary", "item", &id, nil, gin.H{"file_id": fileID})
		httpx.OK(c, gin.H{"photos": photos})
	}
}
