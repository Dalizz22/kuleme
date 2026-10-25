// Item 详情的动作可用性由服务端计算，客户端只按结果显示/禁用按钮。
//
// 判据 = 该库写权限 AND 对应功能开关（缺项 = 开）。export 只需要读权限
// （详情能打开就说明有读权限），因此恒为 true；transfer 的目标库权限
// 要等选完目标库才知道，这里只表达「可以发起转库」。
package item

import (
	"context"

	"github.com/klm/server/internal/authz"
	"github.com/klm/server/internal/catalog"
	"github.com/klm/server/internal/httpx"
)

// 动作键，顺序即接口契约顺序（openapi /items/{id}）。
const (
	ActionTransfer    = "transfer"
	ActionSale        = "sale"
	ActionScrap       = "scrap"
	ActionMaintenance = "maintenance"
	ActionStocktake   = "stocktake"
	ActionPhotos      = "photos"
	ActionExport      = "export"
)

// actionKeys 固定键集：缺键会让客户端无法判断，宁可全给。
var actionKeys = []string{
	ActionTransfer, ActionSale, ActionScrap, ActionMaintenance,
	ActionStocktake, ActionPhotos, ActionExport,
}

// actionFeatures 是「动作 → 功能开关」映射；不在表里的动作只看权限。
var actionFeatures = map[string]string{
	ActionSale:        catalog.FeatureSale,
	ActionMaintenance: catalog.FeatureMaintenance,
	ActionStocktake:   catalog.FeatureStocktake,
}

// Reasons 是关闭原因的固定文案（前端直接展示）：
// 权限不足与开关关闭是两类原因，权限优先——没有写权限时开关状态无意义。
const (
	ReasonNoWritePermission = "没有该库的操作权限"
)

// actionAvailability 是 Item 详情的两个附加结构。
type actionAvailability struct {
	AllowedActions       map[string]bool   `json:"allowed_actions"`
	ActionBlockedReasons map[string]string `json:"action_blocked_reasons"`
}

// availabilityOf 是纯决策：writable 为该库的写权限判定，settings 为已合并缺项的开关。
// 只把 false 的键写进 reasons，键集与 allowed_actions 严格一一对应。
func availabilityOf(writable bool, settings catalog.SpaceSettings) actionAvailability {
	out := actionAvailability{
		AllowedActions:       make(map[string]bool, len(actionKeys)),
		ActionBlockedReasons: make(map[string]string),
	}
	for _, key := range actionKeys {
		ok, reason := true, ""
		if key != ActionExport && !writable {
			ok, reason = false, ReasonNoWritePermission
		} else if feature, has := actionFeatures[key]; has {
			ok, reason = catalog.CanOperate(settings, feature)
		}
		out.AllowedActions[key] = ok
		if !ok {
			out.ActionBlockedReasons[key] = reason
		}
	}
	return out
}

// availability 读取该物品所属库的写权限与开关，交给纯决策。
// 读开关失败按「开」处理：不能因为一次读失败把整库功能锁死。
func (s *itemService) availability(ctx context.Context, actor httpx.Actor, itemID string) actionAvailability {
	settings := catalog.SpaceSettings{Price: true, Sale: true, Maintenance: true, Stocktake: true}
	writable := false
	var spaceID string
	if err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT space_id FROM item WHERE id = ?`, itemID).Scan(&spaceID).Error; err == nil && spaceID != "" {
		writable = s.authz.Check(ctx, actor, spaceID, authz.ActionWrite) == nil
		if got, err := catalog.LoadSettings(ctx, s.db, spaceID); err == nil {
			settings = got
		}
	}
	return availabilityOf(writable, settings)
}
