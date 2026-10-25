// Package catalog 路由：库（space）、分类（category）、位置（location）三棵树与库成员。
//
// 路径以 docs/接口契约约定.md 第 6 节为准，全部要求登录：
//
//	GET    /api/v1/spaces                     可见库列表（按成员关系过滤）
//	POST   /api/v1/spaces                     建库（根库自动成为 owner）
//	PATCH  /api/v1/spaces/:id                 改名/排序/移动（admin，乐观锁）
//	DELETE /api/v1/spaces/:id                 删库（own，库内有物品则 409）
//	GET    /api/v1/spaces/:id/members         成员列表（read）
//	PUT    /api/v1/spaces/:id/members/:uid    设置成员角色（admin）
//	DELETE /api/v1/spaces/:id/members/:uid    移除成员（admin）
//	GET    /api/v1/categories                 分类树（登录即可见）
//	POST   /api/v1/categories                 建分类（管理员）
//	PATCH  /api/v1/categories/:id             改/移分类（管理员，乐观锁）
//	DELETE /api/v1/categories/:id             删分类（管理员，被引用则 409）
//	GET|POST|PATCH|DELETE /api/v1/locations…  同分类，位置带 code
package catalog

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc := newService(deps.DB)
	g := r.Group("", httpx.Auth(deps.Auth, true))

	g.GET("/spaces", handleListSpaces(svc))
	g.POST("/spaces", handleCreateSpace(svc))
	// 路径里的资源 ID 一律先过 uuid 守卫：非法 → 404，不进 SQL（22P02 → 500）。
	space := g.Group("/spaces/:id", httpx.UUIDGuard("id"))
	space.PATCH("", handlePatchSpace(svc))
	space.DELETE("", handleDeleteSpace(svc))
	space.GET("/members", handleListMembers(svc))
	space.PUT("/members/:uid", httpx.UUIDGuard("uid"), handleSetMember(svc))
	space.DELETE("/members/:uid", httpx.UUIDGuard("uid"), handleRemoveMember(svc))

	RegisterSettings(space, svc)

	g.GET("/categories", handleListCategories(svc))
	g.POST("/categories", handleCreateCategory(svc))
	cat := g.Group("/categories/:id", httpx.UUIDGuard("id"))
	cat.PATCH("", handlePatchCategory(svc))
	cat.DELETE("", handleDeleteCategory(svc))

	g.GET("/locations", handleListLocations(svc))
	g.POST("/locations", handleCreateLocation(svc))
	loc := g.Group("/locations/:id", httpx.UUIDGuard("id"))
	loc.PATCH("", handlePatchLocation(svc))
	loc.DELETE("", handleDeleteLocation(svc))
}

// patchReq 是三棵树共用的修改请求：version 必带（乐观锁）；
// parent_id 键出现才移动（null/空串表示移到根，缺省不动）。
type patchReq struct {
	Name     string  `json:"name"`
	Sort     *int    `json:"sort"`
	ParentID *string `json:"parent_id"`
	Version  int     `json:"version" binding:"required,min=1"`
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func handleListSpaces(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := httpx.CurrentActor(c)
		nodes, err := svc.listSpaces(c.Request.Context(), actor)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"items": nodes})
	}
}

func handleCreateSpace(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name      string  `json:"name" binding:"required"`
			ParentID  *string `json:"parent_id"`
			Kind      string  `json:"kind"`
			IsPrivate bool    `json:"is_private"`
			Sort      int     `json:"sort"`
		}
		if err := httpx.BindJSON(c, &req); respondBind(c, err) {
			return
		}
		actor := httpx.CurrentActor(c)
		n := &SpaceNode{Node: Node{Name: req.Name, ParentID: req.ParentID, Sort: req.Sort},
			Kind: req.Kind, IsPrivate: req.IsPrivate}
		out, err := svc.createSpace(c.Request.Context(), actor, n)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, out)
	}
}

func handlePatchSpace(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		var req patchReq
		if err := httpx.BindJSON(c, &req); respondBind(c, err) {
			return
		}
		actor := httpx.CurrentActor(c)
		n := &SpaceNode{Node: Node{Name: req.Name, ParentID: req.ParentID, Version: req.Version}}
		if err := svc.patchSpace(c.Request.Context(), actor, id, n, req.Sort, req.ParentID != nil); respondErr(c, err) {
			return
		}
		httpx.OK(c, nil)
	}
}

func handleDeleteSpace(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		actor := httpx.CurrentActor(c)
		if respondErr(c, svc.deleteSpace(c.Request.Context(), actor, id)) {
			return
		}
		httpx.OK(c, nil)
	}
}

func handleListMembers(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		actor := httpx.CurrentActor(c)
		if respondErr(c, svc.RequireSpace(c.Request.Context(), actor, id, authz.ActionRead)) {
			return
		}
		members, err := svc.listMembers(c.Request.Context(), id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"items": members})
	}
}

func handleSetMember(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		uid, herr := httpx.UUIDParam(c, "uid")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		var req struct {
			Role string `json:"role" binding:"required"`
		}
		if err := httpx.BindJSON(c, &req); respondBind(c, err) {
			return
		}
		actor := httpx.CurrentActor(c)
		if respondErr(c, svc.RequireSpace(c.Request.Context(), actor, id, authz.ActionAdmin)) {
			return
		}
		if respondErr(c, svc.setMember(c.Request.Context(), id, uid, req.Role)) {
			return
		}
		httpx.OK(c, nil)
	}
}

func handleRemoveMember(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		uid, herr := httpx.UUIDParam(c, "uid")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		actor := httpx.CurrentActor(c)
		if respondErr(c, svc.RequireSpace(c.Request.Context(), actor, id, authz.ActionAdmin)) {
			return
		}
		if respondErr(c, svc.removeMember(c.Request.Context(), id, uid)) {
			return
		}
		httpx.OK(c, nil)
	}
}

func handleListCategories(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		nodes, err := svc.listTree(c.Request.Context(), "category")
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"items": nodes})
	}
}

func handleCreateCategory(svc *catalogService) gin.HandlerFunc {
	return withDirectoryAdmin(func(c *gin.Context) error {
		var req struct {
			Name     string  `json:"name" binding:"required"`
			ParentID *string `json:"parent_id"`
		}
		if err := httpx.BindJSON(c, &req); err != nil {
			return err
		}
		out, err := svc.createDirectory(c.Request.Context(), "category", req.Name, req.ParentID)
		if err != nil {
			return err
		}
		httpx.OK(c, out)
		return nil
	})
}

func handlePatchCategory(svc *catalogService) gin.HandlerFunc {
	return withDirectoryAdmin(func(c *gin.Context) error {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			return herr
		}
		var req patchReq
		if err := httpx.BindJSON(c, &req); err != nil {
			return err
		}
		if req.ParentID != nil {
			if err := svc.moveNode(c.Request.Context(), "category", id, req.ParentID); err != nil {
				return err
			}
		}
		return svc.patchNameSort(c.Request.Context(), "category", id, req.Name, req.Sort, req.Version)
	})
}

func handleDeleteCategory(svc *catalogService) gin.HandlerFunc {
	return withDirectoryAdmin(func(c *gin.Context) error {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			return herr
		}
		return svc.deleteNode(c.Request.Context(), "category", id, categoryDeleteGuards)
	})
}

func handleListLocations(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		nodes, err := svc.listLocations(c.Request.Context())
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"items": nodes})
	}
}

func handleCreateLocation(svc *catalogService) gin.HandlerFunc {
	return withDirectoryAdmin(func(c *gin.Context) error {
		var req struct {
			Name     string  `json:"name" binding:"required"`
			ParentID *string `json:"parent_id"`
			Code     *string `json:"code"`
		}
		if err := httpx.BindJSON(c, &req); err != nil {
			return err
		}
		out, err := svc.createLocation(c.Request.Context(), req.Name, req.ParentID, req.Code)
		if err != nil {
			return err
		}
		httpx.OK(c, out)
		return nil
	})
}

func handlePatchLocation(svc *catalogService) gin.HandlerFunc {
	return withDirectoryAdmin(func(c *gin.Context) error {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			return herr
		}
		var req struct {
			patchReq
			Code *string `json:"code"`
		}
		if err := httpx.BindJSON(c, &req); err != nil {
			return err
		}
		return svc.patchLocation(c.Request.Context(), id, req.Name, req.Code, req.Sort,
			req.ParentID, req.Version, req.ParentID != nil)
	})
}

func handleDeleteLocation(svc *catalogService) gin.HandlerFunc {
	return withDirectoryAdmin(func(c *gin.Context) error {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			return herr
		}
		return svc.deleteNode(c.Request.Context(), "location", id, locationDeleteGuards)
	})
}

// withDirectoryAdmin 包一层共用目录的管理员守卫：非管理员 403，业务错误走 respondErr。
func withDirectoryAdmin(next func(c *gin.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if respondErr(c, requireDirectoryAdmin(httpx.CurrentActor(c).IsAdmin)) {
			return
		}
		if err := next(c); respondErr(c, err) {
			return
		}
	}
}

// respondBind BindJSON 失败统一 400。
func respondBind(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	// BindJSON 已经返回带状态码的 *httpx.Error（422 校验 / 413 超限），原样透传，
	// 不再包一层 Validation 把 413 改写成 422，也不回显内部错误文本。
	httpx.Abort(c, err)
	return true
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
