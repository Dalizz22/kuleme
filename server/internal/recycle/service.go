// Package recycle 实现 回收站：软删物品/库的列表与恢复。
//
// 回收站 30 天不清空——软删数据永不物理删除。超过 30 天保留期
// 的条目仅在查询结果里标记为已过期（expired），恢复接口拒绝过期条目；
// 系统内不存在任何回收站物理 DELETE 路径。
package recycle

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/httpx"
)

const retainFor = 30 * 24 * time.Hour

// Entry 是回收站条目。
type Entry struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	SpaceID     string `json:"space_id"`
	PublicCode  string `json:"public_code"`
	DeletedAt   string `json:"deleted_at"`
	PurgeAfter  string `json:"purge_after"`
	Expired     bool   `json:"expired"` // 已过 30 天保留期（仅标记，不做物理清理）
	DeletedByIP string `json:"-"`
}

type service struct {
	deps  appdeps.Deps
	authz *authz.Checker
}

// Register 挂载回收站路由（全部需登录）。
//
//	GET    /recycle            汇总（各类计数）
//	GET    /recycle/items      已删物品（可见库过滤）
//	POST   /recycle/items/:id/restore
//	DELETE /recycle/items/:id  物理清理（/已下线：恒 409，不做物理删除）
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	s := &service{deps: deps, authz: authz.New(deps.DB)}
	g := r.Group("", httpx.Auth(deps.Auth, true))
	g.GET("/recycle", s.handleSummary())
	g.GET("/recycle/items", s.handleItems())
	g.POST("/recycle/items/:id/restore", s.handleRestore())
	g.DELETE("/recycle/items/:id", s.handlePurge())
}

// sweep 已按 /移除：回收站不再有任何物理 DELETE，
// 保留期外条目仅在查询结果中标记 expired。

func (s *service) handleSummary() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.deps.Available() {
			httpx.Abort(c, httpx.Unavailable("数据库不可用"))
			return
		}
		actor := httpx.CurrentActor(c)
		visible, err := s.authz.VisibleSpaceIDs(c.Request.Context(), actor)
		if err != nil {
			httpx.Abort(c, err)
			return
		}
		var out struct {
			Items  int `json:"items"`
			Spaces int `json:"spaces"`
		}
		if len(visible) == 0 {
			httpx.OK(c, out)
			return
		}
		_ = s.deps.DB.Gorm.WithContext(c.Request.Context()).Raw(
			`SELECT count(*) FROM item WHERE deleted_at IS NOT NULL AND space_id IN (?)`, visible).
			Scan(&out.Items).Error
		if actor.IsAdmin {
			_ = s.deps.DB.Gorm.WithContext(c.Request.Context()).Raw(
				`SELECT count(*) FROM space WHERE deleted_at IS NOT NULL`).Scan(&out.Spaces).Error
		}
		httpx.OK(c, out)
	}
}

func (s *service) handleItems() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.deps.Available() {
			httpx.Abort(c, httpx.Unavailable("数据库不可用"))
			return
		}
		actor := httpx.CurrentActor(c)
		visible, err := s.authz.VisibleSpaceIDs(c.Request.Context(), actor)
		if err != nil {
			httpx.Abort(c, err)
			return
		}
		if len(visible) == 0 {
			httpx.OK(c, gin.H{"items": []any{}})
			return
		}
		pp := httpx.ParsePagination(c)
		var rows []struct {
			ID         string
			Name       string
			SpaceID    string `gorm:"column:space_id"`
			PublicCode string `gorm:"column:public_code"`
			DeletedAt  time.Time
		}
		if err := s.deps.DB.Gorm.WithContext(c.Request.Context()).Raw(`
SELECT id, name, space_id, COALESCE(public_code,'') AS public_code, deleted_at
FROM item WHERE deleted_at IS NOT NULL AND space_id IN (?)
ORDER BY deleted_at DESC, id DESC LIMIT ? OFFSET ?`,
			visible, pp.PageSize, pp.Offset).Scan(&rows).Error; err != nil {
			httpx.Abort(c, err)
			return
		}
		items := make([]Entry, 0, len(rows))
		for _, r := range rows {
			items = append(items, Entry{
				ID: r.ID, Kind: "item", Name: r.Name, SpaceID: r.SpaceID,
				PublicCode: r.PublicCode,
				DeletedAt:  r.DeletedAt.Format(time.RFC3339),
				PurgeAfter: r.DeletedAt.Add(retainFor).Format(time.RFC3339),
				Expired:    time.Since(r.DeletedAt) > retainFor, // 已过保留期，仅标记
			})
		}
		httpx.OK(c, gin.H{"items": items})
	}
}

func (s *service) handleRestore() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.deps.Available() {
			httpx.Abort(c, httpx.Unavailable("数据库不可用"))
			return
		}
		id, herr := httpx.UUIDParam(c, "id")
		if herr != nil {
			httpx.Abort(c, herr)
			return
		}
		actor := httpx.CurrentActor(c)
		ctx := c.Request.Context()
		var row struct {
			SpaceID   string `gorm:"column:space_id"`
			DeletedAt time.Time
		}
		if err := s.deps.DB.Gorm.WithContext(ctx).Raw(
			`SELECT space_id, deleted_at FROM item WHERE id = ? AND deleted_at IS NOT NULL`, id).
			Scan(&row).Error; err != nil {
			httpx.Abort(c, err)
			return
		}
		if row.SpaceID == "" {
			httpx.Abort(c, httpx.NotFound("回收站里没有这个物品"))
			return
		}
		// 恢复是对库的写操作；库本身也被删则不允许恢复（先恢复库）。
		if err := s.authz.Check(ctx, actor, row.SpaceID, authz.ActionWrite); err != nil {
			httpx.Abort(c, err)
			return
		}
		var spaceDeleted bool
		_ = s.deps.DB.Gorm.WithContext(ctx).Raw(
			`SELECT true FROM space WHERE id = ? AND deleted_at IS NOT NULL`, row.SpaceID).
			Scan(&spaceDeleted).Error
		if spaceDeleted {
			httpx.Abort(c, httpx.New(409, httpx.CodeOpenBusiness, "所属库在回收站中，请先恢复库"))
			return
		}
		if time.Since(row.DeletedAt) > retainFor {
			httpx.Abort(c, httpx.New(409, httpx.CodeOpenBusiness, "已超过 30 天保留期，无法恢复"))
			return
		}
		res := s.deps.DB.Gorm.WithContext(ctx).Exec(
			`UPDATE item SET deleted_at = NULL, updated_at = now(), version = version + 1
			 WHERE id = ? AND deleted_at IS NOT NULL`, id)
		if res.Error != nil {
			httpx.Abort(c, res.Error)
			return
		}
		if res.RowsAffected == 0 {
			httpx.Abort(c, httpx.NotFound("回收站里没有这个物品"))
			return
		}
		httpx.OK(c, gin.H{"restored": id})
	}
}

func (s *service) handlePurge() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 物理删除路径已整体下线——无论条目是否过保留期，
		// 回收站不提供物理 DELETE，恒 409。端点保留注册以维持 API 契约。
		if _, herr := httpx.UUIDParam(c, "id"); herr != nil {
			httpx.Abort(c, herr)
			return
		}
		httpx.Abort(c, httpx.New(409, httpx.CodeOpenBusiness,
			"回收站条目不可物理删除（30 天保留期外自动标记过期，数据不清空）"))
	}
}
