// Package bootstrap：各模块路由注册点。
//
// 每个模块提供 func Register(r *gin.RouterGroup, deps Deps)。
// 未实现完的模块保持调用存在即可，函数体内部自行决定挂哪些路由。
package bootstrap

import (
	"github.com/gin-gonic/gin"

	"github.com/klm/server/internal/ai"
	"github.com/klm/server/internal/audit"
	"github.com/klm/server/internal/auth"
	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/catalog"
	"github.com/klm/server/internal/idem"
	"github.com/klm/server/internal/inventory"
	"github.com/klm/server/internal/item"
	"github.com/klm/server/internal/jobs"
	"github.com/klm/server/internal/labels"
	"github.com/klm/server/internal/media"
	"github.com/klm/server/internal/recycle"
	"github.com/klm/server/internal/rules"
	"github.com/klm/server/internal/stocktake"
	"github.com/klm/server/internal/templates"
	"github.com/klm/server/internal/transfer"
)

// registerModules 是全部模块的唯一注册顺序。
//
// 顺序只影响路由匹配优先级，不表达依赖：跨模块调用走服务函数，不靠中间件顺序。
func registerModules(v1 *gin.RouterGroup, deps Deps) {
	auth.Register(v1, deps)
	// 写操作结果恢复（GET /operation-results/:operation_id）读的是幂等表，
	// 与业务模块无依赖，独立挂载。
	idem.Register(v1, deps)
	catalog.Register(v1, deps)
	item.Register(v1, deps)
	media.Register(v1, deps)
	inventory.Register(v1, deps)
	stocktake.Register(v1, deps)
	//  借还/租赁暂未开放；出售改走 /items/:id/sale 生命周期，
	// 旧 loan/sale 路由不再注册（包保留作通用插件底座）。
	jobs.Register(v1, deps)
	audit.Register(v1, deps)
	// 业务集成模块（D）复用 B 的鉴权、文件、事务与任务基础。
	templates.Register(v1, deps)
	transfer.Register(v1, deps)
	ai.Register(v1, deps) // AI 子代理已交付：build/vet/test 全绿（5/5 测试过）
	recycle.Register(v1, deps)
	rules.Register(v1, deps)
	// 规则触发接线：物品更新 → field_changed 规则。
	// 标签与扫码解析：渲染依赖在模块内可空——预览/版面/解析可用，
	// 落盘任务在文件服务接线前返回 SERVICE_UNAVAILABLE。
	// 标签打印桥接核心服务——真实落盘/权限复检，不再 503 降级。
	rec, loc, az := labels.NewBridges(deps.DB, deps.Config.DataDir)
	// 扫码解析的对象查询由核心服务实现：labels 只做格式解析，不查库。
	// 不注入 resolver 时 resolved 恒为 null（Android 扫码进详情不可用）。
	labels.Register(v1, deps, labels.Config{FontSearchPaths: labels.DefaultFontSearchPaths()}, rec, loc, az).
		WithResolver(item.NewCodeResolver(deps.DB, authz.New(deps.DB), deps.Config.InstanceIDPath()))
}
