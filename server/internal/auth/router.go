// Package auth 路由：初始化、登录会话、用户管理与实例信息。
//
// 路径与两端客户端的会话调用对齐（契约「实例/初始化 + 会话/用户」
// 归核心后端，两组路径都很小，合在本模块维护）：
//
//	POST   /api/v1/setup            匿名，一次性凭据建首个管理员
//	POST   /api/v1/sessions         匿名，登录，返回令牌与账号
//	DELETE /api/v1/sessions/current 已登录（幂等），注销当前会话
//	GET    /api/v1/me               已登录，当前账号资料
//	GET    /api/v1/users            管理员，账号列表
//	POST   /api/v1/users            管理员，建号
//	PATCH  /api/v1/users/:id        管理员，改资料/启停
//	PUT    /api/v1/users/:id/password 本人或管理员，改密
//	GET    /api/v1/system/info      匿名，实例身份与初始化状态
package auth

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/audit"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc := New(deps.DB, deps.Logger, deps.Config)
	aud := audit.New(deps.DB)

	r.POST("/setup", handleSetup(svc))
	r.POST("/sessions", handleLogin(svc))
	r.GET("/system/info", handleSystemInfo(svc, deps))
	g := r.Group("", httpx.Auth(svc, true))
	g.GET("/me", handleMe(svc))
	g.DELETE("/sessions/current", handleLogout(svc))
	g.GET("/users", withAdmin(handleListUsers(svc)))
	g.POST("/users", withAdmin(handleCreateUser(svc, aud)))
	g.PATCH("/users/:id", withAdmin(handleUpdateUser(svc, aud)))
	g.PUT("/users/:id/password", handleChangePassword(svc, aud))
}

// withAdmin 管理员守卫；非管理员 403。
func withAdmin(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !httpx.CurrentActor(c).IsAdmin {
			httpx.Abort(c, httpx.Forbidden("仅管理员可执行该操作"))
			return
		}
		next(c)
	}
}

func handleListUsers(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := httpx.ParsePagination(c)
		users, total, err := svc.ListUsers(c.Request.Context(), p.Page, p.PageSize)
		if err != nil {
			respondErr(c, err)
			return
		}
		httpx.List(c, users, p.Page, p.PageSize, int(total))
	}
}

func handleCreateUser(svc *Service, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req UserInput
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.AbortBind(c, err)
			return
		}
		user, err := svc.CreateUser(c.Request.Context(), req.Username, req.Password, req.DisplayName, req.IsAdmin)
		if err != nil {
			respondErr(c, err)
			return
		}
		aud.Try(c, "user.create", "user", &user.ID, nil, gin.H{"username": req.Username, "is_admin": req.IsAdmin})
		httpx.Created(c, user)
	}
}

func handleUpdateUser(svc *Service, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		var req UserPatch
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.AbortBind(c, err)
			return
		}
		user, err := svc.UpdateUser(c.Request.Context(), id, req)
		if err != nil {
			respondErr(c, err)
			return
		}
		aud.Try(c, "user.update", "user", &id, nil, nil)
		httpx.OK(c, user)
	}
}

func handleChangePassword(svc *Service, aud *audit.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		var req PasswordInput
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.AbortBind(c, err)
			return
		}
		if err := svc.ChangePassword(c.Request.Context(), httpx.CurrentActor(c), id, req); err != nil {
			respondErr(c, err)
			return
		}
		aud.Try(c, "user.password_change", "user", &id, nil, nil)
		httpx.OK(c, nil)
	}
}

func handleSetup(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !depsAvailable(c, svc) {
			return
		}
		var req struct {
			// Secret 为一次性初始化凭据（启动日志 setup_secret）。
			// 仅 development 允许留空走网页向导；production 留空一律 403。
			Secret      string `json:"secret"`
			Username    string `json:"username" binding:"required"`
			Password    string `json:"password" binding:"required"`
			DisplayName string `json:"display_name"`
		}
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.Abort(c, err)
			return
		}
		user, err := svc.Setup(c.Request.Context(), req.Secret, req.Username, req.Password, req.DisplayName)
		if err != nil {
			respondErr(c, err)
			return
		}
		httpx.OK(c, gin.H{"user": user})
	}
}

func handleLogin(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !depsAvailable(c, svc) {
			return
		}
		var req struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
		}
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.Abort(c, err)
			return
		}
		token, user, err := svc.Login(c.Request.Context(), req.Username, req.Password, c.ClientIP())
		if err != nil {
			respondErr(c, err)
			return
		}
		svc.SetSessionCookie(c, token)
		httpx.OK(c, gin.H{"token": token, "user": user})
	}
}

func handleLogout(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			if cookie, err := c.Cookie(httpx.SessionCookieName); err == nil {
				token = cookie
			}
		}
		if err := svc.Logout(c.Request.Context(), token); err != nil {
			respondErr(c, err)
			return
		}
		svc.ClearSessionCookie(c)
		httpx.OK(c, nil)
	}
}

func handleMe(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := svc.Me(c.Request.Context(), httpx.CurrentActor(c))
		if err != nil {
			respondErr(c, err)
			return
		}
		httpx.OK(c, user)
	}
}

func handleSystemInfo(svc *Service, deps appdeps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		instanceID, err := svc.ensureInstanceID()
		if err != nil {
			respondErr(c, err)
			return
		}
		initialized, err := svc.Initialized(c.Request.Context())
		if err != nil && deps.Available() {
			respondErr(c, err)
			return
		}
		httpx.OK(c, gin.H{
			"instance_id": instanceID,
			"name":        "库了么",
			"api_version": deps.Config.APIVersion,
			"initialized": initialized,
		})
	}
}

// depsAvailable 数据库不可用时统一返回 503，不让每个 handler 各写一遍。
func depsAvailable(c *gin.Context, svc *Service) bool {
	if svc.db.Available() {
		return true
	}
	respondErr(c, store.ErrUnavailable)
	return false
}

// respondErr 把服务层错误翻译成统一响应外壳。
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
	switch {
	case errors.Is(err, ErrBadCredentials), errors.Is(err, ErrSessionExpired):
		httpx.Abort(c, httpx.Unauthenticated(err.Error()))
	case errors.Is(err, ErrRateLimited):
		httpx.Abort(c, httpx.RateLimited(err.Error()))
	case errors.Is(err, ErrWeakPassword):
		httpx.Abort(c, httpx.Validation(err.Error()))
	case errors.Is(err, ErrAlreadyInitialized), errors.Is(err, ErrNotInitialized):
		httpx.Abort(c, httpx.Conflict(httpx.CodeOpenBusiness, err.Error()))
	default:
		httpx.Abort(c, httpx.Internal(err))
	}
}

// bearerToken 读取 Authorization: Bearer 令牌，没有返回空串。
func bearerToken(c *gin.Context) string {
	const prefix = "Bearer "
	h := c.GetHeader("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}
