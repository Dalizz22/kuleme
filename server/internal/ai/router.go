// Package ai 路由：AI 设置、识别任务与候选确认。
//
//	GET  /api/v1/ai/settings                 读脱敏配置
//	PUT  /api/v1/ai/settings                 保存配置（api_key 留空保留旧值；仅管理员）
//	POST /api/v1/ai/settings/test            连通性测试（仅管理员）
//	POST /api/v1/ai/tasks                    建任务并同步识别（202 + 终态）
//	GET  /api/v1/ai/tasks/:id                任务 + 候选
//	POST /api/v1/ai/candidates/:id/confirm   确认候选建物品（Idempotency-Key 可选）
//	POST /api/v1/ai/candidates/:id/discarded 丢弃候选
package ai

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/appdeps"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/idem"
	"github.com/klm/server/internal/store"
)

// Register 挂载本模块路由。服务构造失败（模板资产损坏等启动期问题）
// 记日志不挂路由，与 item 模块同一处理。
func Register(r *gin.RouterGroup, deps appdeps.Deps) {
	svc, err := NewService(deps)
	if err != nil {
		deps.Logger.Error("ai 模块初始化失败，路由未挂载", "error", err)
		return
	}
	g := r.Group("", httpx.Auth(deps.Auth, true))

	g.GET("/ai/settings", handleGetSettings(svc))
	g.PUT("/ai/settings", handlePutSettings(svc))
	g.POST("/ai/settings/test", handleTestSettings(svc))
	g.POST("/ai/tasks", idem.Middleware(deps.DB), handleCreateTask(svc))
	// 资源 ID 先过 uuid 守卫：非法路径参数 404，不进 SQL。
	task := g.Group("/ai/tasks/:id", httpx.UUIDGuard("id"))
	task.GET("", handleGetTask(svc))
	cand := g.Group("/ai/candidates/:id", httpx.UUIDGuard("id"))
	cand.POST("/confirm", idem.Middleware(deps.DB), handleConfirm(svc))
	cand.POST("/discarded", handleDiscard(svc))
}

func handleGetSettings(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		masked, err := svc.GetSettings(c.Request.Context())
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, masked)
	}
}

func handlePutSettings(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 指针化入参：没传的字段保持原值，避免「只改超时 → 图片上限被重置」。
		var in SettingsPatch
		if err := httpx.BindJSON(c, &in); err != nil {
			httpx.Abort(c, err)
			return
		}
		masked, err := svc.PatchSettings(c.Request.Context(), httpx.CurrentActor(c), in)
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, masked)
	}
}

func handleTestSettings(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := svc.TestConnection(c.Request.Context(), httpx.CurrentActor(c))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, result)
	}
}

func handleCreateTask(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createTaskInput
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.Abort(c, err)
			return
		}
		task, cands, err := svc.CreateTask(c.Request.Context(), httpx.CurrentActor(c), req)
		if respondErr(c, err) {
			return
		}
		// 202：任务已受理并同步执行完毕，返回终态（docs/接口契约约定.md 第 6 节）。
		httpx.Respond(c, http.StatusAccepted, gin.H{"task": task, "candidates": cands})
	}
}

func handleGetTask(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		task, cands, err := svc.GetTask(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, gin.H{"task": task, "candidates": cands})
	}
}

// confirmReq 是确认请求体。ConfirmInput 无 json tag（进程内类型），
// HTTP 形态在这里单独声明。
type confirmReq struct {
	SpaceID     string         `json:"space_id"`
	TemplateKey string         `json:"template_key"`
	Name        string         `json:"name"`
	CategoryID  string         `json:"category_id"`
	LocationID  string         `json:"location_id"`
	Fields      map[string]any `json:"fields"`
	Quantity    string         `json:"quantity"`
	// PhotoIDs 用指针区分「没传/null」与「明确给了数组」：前者走来源图共享，
	// 后者（含空数组）以数组为准。契约见接口契约约定.md 的照片共享语义。
	PhotoIDs    *[]string `json:"photo_ids"`
	SharePhotos *bool     `json:"share_photos"`
}

func handleConfirm(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req confirmReq
		if err := httpx.BindJSON(c, &req); err != nil {
			httpx.Abort(c, err)
			return
		}
		in := ConfirmInput{
			OperationID: c.GetHeader("Idempotency-Key"),
			CandidateID: c.Param("id"),
			SpaceID:     req.SpaceID,
			TemplateKey: req.TemplateKey,
			Name:        req.Name,
			CategoryID:  req.CategoryID,
			LocationID:  req.LocationID,
			Fields:      req.Fields,
			Quantity:    req.Quantity,
			SharePhotos: req.SharePhotos,
		}
		if req.PhotoIDs != nil {
			in.PhotoIDs = *req.PhotoIDs
			in.PhotosSet = true
		}
		result, err := svc.Confirm(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"), in)
		if respondErr(c, err) {
			return
		}
		// 契约：data 固定三键 {item_id, public_code, movement_id}；无初始库存
		// （待入库）时 movement_id 为 null。
		var movementID any
		if result.MovementID != "" {
			movementID = result.MovementID
		}
		httpx.OK(c, gin.H{
			"item_id":     result.ItemID,
			"public_code": result.PublicCode,
			"movement_id": movementID,
		})
	}
}

func handleDiscard(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		cand, err := svc.Discard(c.Request.Context(), httpx.CurrentActor(c), c.Param("id"))
		if respondErr(c, err) {
			return
		}
		httpx.OK(c, cand)
	}
}

// respondErr 服务层错误 → 统一响应外壳。业务可见失败（未配置、重复确认、
// 限流）用明确的业务码；其余按 loan 模块同一套映射。
func respondErr(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	var httpErr *httpx.Error
	switch {
	case errors.Is(err, store.ErrUnavailable):
		httpx.Abort(c, httpx.Unavailable("服务暂时不可用，请稍后重试"))
	case errors.As(err, &httpErr):
		httpx.Abort(c, httpErr)
	case isValidation(err):
		httpx.Abort(c, httpx.Validation(err.Error()))
	case errors.Is(err, ErrNotConfigured):
		httpx.Abort(c, httpx.New(http.StatusConflict, httpx.CodeOpenBusiness, "AI 未配置或未启用"))
	case errors.Is(err, errCandidateHandled):
		httpx.Abort(c, httpx.Conflict(httpx.CodeIdempotencyConflict, "候选已被处理"))
	case errors.Is(err, ErrRateLimited):
		httpx.Abort(c, httpx.New(http.StatusTooManyRequests, httpx.CodeOpenBusiness, "AI 提供者限流，请稍后重试"))
	case errors.Is(err, ErrForbidden):
		httpx.Abort(c, httpx.Forbidden("没有执行该操作的权限"))
	case errors.Is(err, ErrNotFound):
		httpx.Abort(c, httpx.NotFound("资源不存在"))
	case errors.Is(err, ErrBadImage):
		httpx.Abort(c, httpx.Validation("图片不可用："+err.Error()))
	default:
		httpx.Abort(c, httpx.Internal(err))
	}
	return true
}

// isValidation 判定 service 层的入参校验错误（400 语义）。
func isValidation(err error) bool {
	var ve *validationError
	return errors.As(err, &ve)
}
