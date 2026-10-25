// Package media 路由：文件上传与授权下载。
//
// 路径与 docs/接口契约约定.md 第 6 节「文件」分组对齐：
//
//	POST /api/v1/files                 已登录，multipart 上传，落盘 + 写 file_object + 生成变体
//	GET  /api/v1/files                 已登录，分页列表，按创建时间倒序
//	DELETE /api/v1/files/{id}          已登录，仅上传者本人且未关联，成功 204
//	GET  /api/v1/files/{id}/content    已登录 + 下载授权，授权原图
//	GET  /api/v1/files/{id}/thumbnail  已登录 + 下载授权，320px 变体（无则降级原图，X-Thumbnail 标注）
//	GET  /api/v1/files/{id}/preview    已登录 + 下载授权，1600px 变体（无则降级原图）
//
// 变体存储约定见 service.go：media/<id>_<最长边>.jpg，接口按约定取文件，不查列。
//
// 登录要求：鉴权中间件由 bootstrap 统一挂载；集成完成前各 handler 用
// httpx.RequireActor 自保，未登录直接返回 401，不依赖中间件是否已挂。
package media

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc := New(deps.DB, deps.Logger, deps.Config)

	g := r.Group("/files", httpx.Auth(deps.Auth, true))
	g.POST("", handleUpload(svc))
	g.GET("", handleList(svc))
	// 文件 ID 先过 uuid 守卫：非法 → 404，不进 SQL。
	file := g.Group("/:id", httpx.UUIDGuard("id"))
	file.DELETE("", handleDelete(svc))
	file.GET("/content", handleFile(svc, 0))
	file.GET("/thumbnail", handleFile(svc, 320))
	file.GET("/preview", handleFile(svc, 1600))
}

func handleUpload(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, err := httpx.RequireActor(c)
		if err != nil {
			httpx.Abort(c, err)
			return
		}
		fh, err := c.FormFile("file")
		if err != nil {
			httpx.Abort(c, httpx.Validation("请选择要上传的文件"))
			return
		}
		// 业务层只收 multipart.FileHeader，不把 gin.Context 带进去。
		f, err := svc.Upload(c.Request.Context(), actor, fh)
		if err != nil {
			respondErr(c, err)
			return
		}
		httpx.Created(c, f)
	}
}

// handleFile 服务原图与变体：maxEdge 为 0 返回原图；否则按存储约定取
// <id>_<maxEdge>.jpg 变体，不存在降级原图。/thumbnail 恒带 X-Thumbnail
// 头，如实标注返回的是变体（variant）还是原图（original）。
func handleFile(svc *Service, maxEdge int) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, err := httpx.RequireActor(c)
		if err != nil {
			httpx.Abort(c, err)
			return
		}
		row, err := svc.Get(c.Request.Context(), c.Param("id"))
		if err != nil {
			respondErr(c, err)
			return
		}
		// 下载前授权——引用物品库读权；上传者例外只对未关联私有文件。
		if err := svc.AuthorizeDownload(c.Request.Context(), actor, row); err != nil {
			respondErr(c, err)
			return
		}
		if maxEdge > 0 {
			if vpath, ok := svc.variantFile(row.ID, maxEdge); ok {
				c.Header("X-Thumbnail", "variant")
				serveFile(c, vpath, "image/jpeg", variantName(row.ID, maxEdge))
				return
			}
			c.Header("X-Thumbnail", "original")
		}
		path, err := svc.diskPath(row.StoragePath)
		if err != nil {
			respondErr(c, err)
			return
		}
		serveFile(c, path, row.ContentType, row.StoragePath)
	}
}

// serveFile 统一输出：显式 Content-Type + attachment 文件名，与既有语义一致。
func serveFile(c *gin.Context, path, contentType, downloadName string) {
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", `attachment; filename="`+downloadName+`"`)
	c.File(path)
}

// handleDelete 删除本人上传且未关联的私有文件：成功 204；
// 已关联 409 FILE_IN_USE；他人或不存在的文件 404。
func handleDelete(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, err := httpx.RequireActor(c)
		if err != nil {
			httpx.Abort(c, err)
			return
		}
		if err := svc.Delete(c.Request.Context(), actor, c.Param("id")); err != nil {
			respondErr(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func handleList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, err := httpx.RequireActor(c)
		if err != nil {
			httpx.Abort(c, err)
			return
		}
		p := httpx.ParsePagination(c)
		items, total, err := svc.List(c.Request.Context(), actor, p.Limit(), p.Offset)
		if err != nil {
			respondErr(c, err)
			return
		}
		httpx.List(c, items, p.Page, p.PageSize, total)
	}
}

// respondErr 把服务层错误翻译成统一响应外壳，与 auth 模块同一套映射。
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
