// rules 路由：规则 CRUD、运行详情/撤回、站内收件箱。
package rules

import (
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
)

type ruleRequest struct {
	Name    string         `json:"name"`
	Enabled *bool          `json:"enabled"`
	Trigger string         `json:"trigger_type"`
	Config  map[string]any `json:"config"`
	Actions []Action       `json:"actions"`
	Version int            `json:"version,omitempty"`
}

func (r ruleRequest) toRule() *Rule {
	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	return &Rule{Name: r.Name, Enabled: enabled, Trigger: r.Trigger,
		Config: r.Config, Actions: r.Actions}
}

// Register 挂载本模块路由（全部需登录）。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc := NewService(deps.DB)
	g := r.Group("", httpx.Auth(deps.Auth, true))

	g.POST("/spaces/:id/rules", handleCreate(svc))
	g.GET("/spaces/:id/rules", handleList(svc))
	g.PATCH("/rules/:rid", handlePatch(svc))
	g.DELETE("/rules/:rid", handleDelete(svc))
	g.GET("/rule-runs/:id", handleGetRun(svc))
	g.POST("/rule-runs/:id/undo", handleUndo(svc))
	g.GET("/inbox", handleInbox(svc))
	g.POST("/inbox/:mid/read", handleRead(svc))
}

// paramOr404 取路径参数并校验为 uuid：资源地址不成立是 404，不是格式错误；
// 也保证垃圾参数不会把 uuid 列比较冒成 500。
func paramOr404(c *gin.Context, name string) (string, bool) {
	id, err := httpx.UUIDParam(c, name)
	if err != nil {
		httpx.Abort(c, err)
		return "", false
	}
	return id, true
}

func handleCreate(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID, ok := paramOr404(c, "id")
		if !ok {
			return
		}
		var req ruleRequest
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.Abort(c, err)
			return
		}
		out, err := svc.Create(c.Request.Context(), httpx.CurrentActor(c), spaceID, req.toRule())
		if respondErr(c, err) {
			return
		}
		httpx.Created(c, out)
	}
}

func handleList(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID, ok := paramOr404(c, "id")
		if !ok {
			return
		}
		list, err := svc.ListSpace(c.Request.Context(), httpx.CurrentActor(c), spaceID)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"items": list})
	}
}

func handlePatch(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := paramOr404(c, "rid")
		if !ok {
			return
		}
		var req ruleRequest
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.Abort(c, err)
			return
		}
		// Enabled 为 *bool，PATCH 不带 enabled 时 nil → 不更新（不静默重启用）。
		out, err := svc.Patch(c.Request.Context(), httpx.CurrentActor(c), id, req.toRule(), req.Version, req.Enabled)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, out)
	}
}

func handleDelete(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := paramOr404(c, "rid")
		if !ok {
			return
		}
		if err := svc.Delete(c.Request.Context(), httpx.CurrentActor(c), id); respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"deleted": id})
	}
}

func handleGetRun(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := paramOr404(c, "id")
		if !ok {
			return
		}
		out, err := svc.GetRun(c.Request.Context(), httpx.CurrentActor(c), id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, out)
	}
}

func handleUndo(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := paramOr404(c, "id")
		if !ok {
			return
		}
		out, err := svc.Undo(c.Request.Context(), httpx.CurrentActor(c), id)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, out)
	}
}

func handleInbox(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := httpx.CurrentActor(c)
		var rows []struct {
			ID        string
			Title     string
			Body      string
			RunID     *string    `gorm:"column:run_id"`
			ReadAt    *time.Time `gorm:"column:read_at"`
			CreatedAt time.Time
		}
		err := svc.db.Gorm.WithContext(c.Request.Context()).Raw(
			`SELECT id, title, body, run_id, read_at, created_at
			 FROM message WHERE recipient_id = ?
			 ORDER BY created_at DESC, id DESC LIMIT 100`, actor.UserID).Scan(&rows).Error
		if respondErr(c, err) {
			return
		}
		type mv struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Body      string `json:"body"`
			RunID     string `json:"run_id,omitempty"`
			Read      bool   `json:"read"`
			CreatedAt string `json:"created_at"`
		}
		out := make([]mv, 0, len(rows))
		unread := 0
		for _, r := range rows {
			if r.ReadAt == nil {
				unread++
			}
			runID := ""
			if r.RunID != nil {
				runID = *r.RunID
			}
			out = append(out, mv{ID: r.ID, Title: r.Title, Body: r.Body, RunID: runID,
				Read: r.ReadAt != nil, CreatedAt: r.CreatedAt.Format(time.RFC3339)})
		}
		httpx.OK(c, gin.H{"items": out, "unread": unread})
	}
}

func handleRead(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := paramOr404(c, "mid")
		if !ok {
			return
		}
		actor := httpx.CurrentActor(c)
		res := svc.db.Gorm.WithContext(c.Request.Context()).Exec(
			`UPDATE message SET read_at = now() WHERE id = ? AND recipient_id = ? AND read_at IS NULL`,
			id, actor.UserID)
		if res.Error != nil {
			httpx.Abort(c, res.Error)
			return
		}
		if res.RowsAffected == 0 {
			// 已读重放幂等成功；不存在/非本人按隐藏 404。
			var n int
			svc.db.Gorm.WithContext(c.Request.Context()).Raw(
				`SELECT count(*) FROM message WHERE id = ? AND recipient_id = ?`, id, actor.UserID).Scan(&n)
			if n == 0 {
				httpx.Abort(c, httpx.NotFound("消息不存在"))
				return
			}
		}
		httpx.OK(c, gin.H{"read": id})
	}
}

func respondErr(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	// authz 的「库不在可见范围」在 rules 里等价于资源不存在：报 404，不冒 500。
	if errors.Is(err, authz.ErrNoSpace) {
		httpx.Abort(c, httpx.NotFound("规则不存在"))
		return true
	}
	httpx.Abort(c, err)
	return true
}
