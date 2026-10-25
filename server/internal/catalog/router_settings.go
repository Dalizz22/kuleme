// 空间配置与字段定义路由。
package catalog

import (
	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/httpx"
)

// RegisterSettings 挂载 /spaces/:id/settings 与 /spaces/:id/fields
// （调用方传入已带 uuid 守卫的 /spaces/:id 分组）。
func RegisterSettings(g *gin.RouterGroup, svc *catalogService) {
	g.GET("/settings", handleGetSettings(svc))
	g.PUT("/settings", handlePutSettings(svc))
	g.GET("/fields", handleListFields(svc))
	g.POST("/fields", handleCreateField(svc))
	// 字段行 id 也会作为 text 参数与 space_field.id（uuid 列）比较：非 uuid → 404，不进 SQL。
	fld := g.Group("/fields/:fid", httpx.UUIDGuard("fid"))
	fld.PATCH("", handlePatchField(svc))
	fld.DELETE("", handleDeleteField(svc))
}

func handleGetSettings(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		set, ver, err := svc.GetSettings(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"settings": set, "config_version": ver})
	}
}

type putSettingsReq struct {
	Settings      SpaceSettings `json:"settings"`
	ConfigVersion int           `json:"config_version" binding:"required,min=1"`
}

func handlePutSettings(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req putSettingsReq
		if err := c.ShouldBindJSON(&req); err != nil {
			httpx.Abort(c, httpx.Validation("请求体不合法：需要 settings 四项开关与 config_version"))
			return
		}
		set, ver, err := svc.UpdateSettings(c.Request.Context(), httpx.CurrentActor(c),
			c.Param("id"), req.Settings, req.ConfigVersion)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"settings": set, "config_version": ver})
	}
}

func handleListFields(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		fields, err := svc.ListFields(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"items": fields})
	}
}

func handleCreateField(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var f FieldDef
		if err := c.ShouldBindJSON(&f); err != nil {
			httpx.Abort(c, httpx.Validation("请求体不合法"))
			return
		}
		def, err := svc.CreateField(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"), &f)
		if respondErr(c, err) {
			return
		}
		httpx.Created(c, def)
	}
}

type patchFieldReq struct {
	FieldDef
	Version int `json:"version" binding:"required,min=1"`
}

func handlePatchField(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req patchFieldReq
		if err := c.ShouldBindJSON(&req); err != nil {
			httpx.Abort(c, httpx.Validation("请求体不合法：需要 version"))
			return
		}
		def, err := svc.PatchField(c.Request.Context(), httpx.CurrentActor(c),
			c.Param("id"), c.Param("fid"), &req.FieldDef, req.Version)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, def)
	}
}

func handleDeleteField(svc *catalogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := svc.DeleteField(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"), c.Param("fid"))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, nil)
	}
}
