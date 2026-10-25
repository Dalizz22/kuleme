// transfer 路由：CSV 导入（预览/确认）与 CSV 导出。
//
//	POST /api/v1/imports            multipart 上传 CSV（file, space_id, template_key）→ 201 draft+预览
//	GET  /api/v1/imports/:id        批次详情+行结果
//	POST /api/v1/imports/:id/confirm 逐行建物品（失败行可重试）
//	POST /api/v1/exports            按过滤条件导出 CSV 附件
package transfer

import (
	"bytes"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/idem"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc, err := NewService(deps.DB)
	if err != nil {
		panic("transfer: " + err.Error())
	}
	x := newExportService(deps.DB, deps.Config.DataDir)
	g := r.Group("", httpx.Auth(deps.Auth, true))

	g.POST("/imports", idem.Middleware(deps.DB), handleImport(svc))
	g.GET("/imports/:id", handleGetImport(svc))
	g.POST("/imports/:id/confirm", idem.Middleware(deps.DB), handleConfirm(svc))
	g.POST("/exports", handleExportRequest(x))
	g.GET("/exports", handleExportList(x))
	g.GET("/exports/:id/download", handleExportDownload(x))
	g.DELETE("/exports/:id", handleExportDelete(x))
}

func handleImport(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 413 判定前移：全量解析前就封顶请求体（D 约定表格 10MiB + multipart 余量）。
		const maxUpload = 10<<20 + 64<<10
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUpload)
		fh, err := c.FormFile("file")
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				httpx.Abort(c, httpx.New(413, httpx.CodeFileTooLarge, "文件不能超过 10MB"))
				return
			}
			httpx.Abort(c, httpx.Validation("缺少 file 文件字段"))
			return
		}
		if fh.Size > 10*1024*1024 {
			// 413：文件本体超限，与请求格式错误（400）区分开。
			httpx.Abort(c, httpx.New(413, httpx.CodeFileTooLarge, "文件不能超过 10MB"))
			return
		}
		f, err := fh.Open()
		if err != nil {
			httpx.Abort(c, httpx.Internal(err))
			return
		}
		defer f.Close()
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(f); err != nil {
			httpx.Abort(c, httpx.Internal(err))
			return
		}
		content := buf.String()
		// XLSX：zip 魔数即转 CSV 走同一条解析/确认链路（只读第一张表）。
		if bytes.HasPrefix(buf.Bytes(), []byte("PK\x03\x04")) {
			content, err = xlsxToCSV(buf.Bytes())
			if err != nil {
				httpx.Abort(c, err)
				return
			}
		}
		var loc *string
		if v := strings.TrimSpace(c.PostForm("location_id")); v != "" {
			loc = &v
		}
		batch, preview, err := svc.CreateImport(c.Request.Context(), httpx.CurrentActor(c), ImportInput{
			SpaceID:     c.PostForm("space_id"),
			TemplateKey: c.PostForm("template_key"),
			Content:     content,
			Mode:        strings.TrimSpace(c.PostForm("mode")),
			LocationID:  loc,
		})
		if respondErr(c, err) {
			return
		}
		httpx.Created(c, gin.H{"batch": batch, "preview": preview})
	}
}

func handleGetImport(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		batch, rows, err := svc.GetImport(c.Request.Context(), httpx.CurrentActor(c), id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"batch": batch, "rows": rows})
	}
}

func handleConfirm(svc *service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		var req ConfirmInput
		_ = httpx.BindJSON(c, &req) // 空 body 合法：沿用创建时映射
		req.OperationID = idem.KeyFrom(c)
		batch, rows, err := svc.ConfirmImport(c.Request.Context(), httpx.CurrentActor(c), id, &req)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"batch": batch, "rows": rows})
	}
}

func handleExportRequest(x *exportService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			TemplateKey string `json:"template_key"`
			Keyword     string `json:"keyword"`
		}
		_ = httpx.BindJSON(c, &req)
		view, err := x.Request(c.Request.Context(), httpx.CurrentActor(c), req.TemplateKey, req.Keyword)
		if respondErr(c, err) {
			return
		}
		httpx.Created(c, view)
	}
}

func handleExportList(x *exportService) gin.HandlerFunc {
	return func(c *gin.Context) {
		list, err := x.List(c.Request.Context(), httpx.CurrentActor(c))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"items": list})
	}
}

func handleExportDownload(x *exportService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		data, name, err := x.Download(c.Request.Context(), httpx.CurrentActor(c), id)
		if respondErr(c, err) {
			return
		}
		c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
		c.Data(http.StatusOK, "text/csv; charset=utf-8", data)
	}
}

func handleExportDelete(x *exportService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		if err := x.Delete(c.Request.Context(), httpx.CurrentActor(c), id); respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"deleted": id})
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
