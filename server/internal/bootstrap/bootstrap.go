// Package bootstrap 是唯一的接线点：构造依赖、挂中间件、注册各模块路由。
//
// 业务模块只暴露自己的 Register(r *gin.RouterGroup, deps Deps) 入口，
// 不允许互相 import 对方的 handler；跨模块调用走服务函数。
// 本包由集成负责人维护，并发开发期间其他人只读。
package bootstrap

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/auth"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
	"github.com/klm/server/internal/web"
)

// Deps 是共享依赖的别名，避免各模块与入口各写一份类型。
type Deps = appdeps.Deps

// NewRouter 组装完整路由树。
//
// 路径约定：全部业务接口在 /api/v1 下，见 docs/接口契约约定.md 第 6 节。
// 只有 /api/v1/system/info、/api/v1/setup 与健康检查允许匿名访问。
func NewRouter(deps Deps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(httpx.Middleware(deps.Logger)...)
	// 跨域在最外层（logger 之后、路由之前）：预检才能 204 而不是落到 NoRoute 的 404。
	r.Use(httpx.Cors(deps.Config))
	// 未命中路由：/api/* 返回 JSON，其余回落内嵌前端（根路径开箱即用）。
	// gin 没有 MethodNotAllowed 钩子，方法不匹配由各路由组自行在需要处返回。
	webFS := web.Dist()
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			httpx.NoRoute(c)
			return
		}
		p := strings.TrimPrefix(c.Request.URL.Path, "/")
		if _, err := fs.Stat(webFS, p); err != nil || p == "" {
			p = "index.html" // 哈希路由只有一个真实入口；未知路径也回首页
		}
		f, err := webFS.Open(p)
		if err != nil {
			httpx.NoRoute(c)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			httpx.NoRoute(c)
			return
		}
		if p == "index.html" {
			c.Header("Cache-Control", "no-cache") // 升级后不留旧壳
		}
		// 不用 gin FileFromFS：它会把 /index.html 规范化成 ./ 造成 301 循环
		if rs, ok := f.(io.ReadSeeker); ok {
			http.ServeContent(c.Writer, c.Request, p, st.ModTime(), rs)
			return
		}
		c.DataFromReader(http.StatusOK, st.Size(), mime.TypeByExtension(path.Ext(p)), f, nil)
	})

	r.GET("/healthz", func(c *gin.Context) {
		if err := deps.DB.Ping(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "degraded", "database": "unavailable"})
			return
		}
		c.JSON(200, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	v1.Use(httpx.CSRFProtection())
	// 会话服务只构造一次：auth 模块自己注册路由，其余模块经 deps.Auth 挂登录中间件。
	deps.Auth = auth.New(deps.DB, deps.Logger, deps.Config)
	registerModules(v1, deps)
	return r
}

// Migrate 应用数据库迁移。数据库不可达时不视为致命错误，由健康检查暴露。
func Migrate(ctx context.Context, db *store.DB, logger *slog.Logger) error {
	if db == nil || !db.Available() {
		logger.Warn("跳过迁移：数据库不可用")
		return nil
	}
	if err := store.Migrate(ctx, db, store.MigrationFS); err != nil {
		return err
	}
	logger.Info("数据库迁移完成")
	return nil
}
