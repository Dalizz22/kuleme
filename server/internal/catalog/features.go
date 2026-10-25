// 功能开关的服务端强制。
//
// 开关存在 space.settings（jsonb，缺项 = 开，见 settings.go）。这里只做两件事：
//  1. canOperate 是纯决策函数，Item 详情与各命令入口共用同一份判定；
//  2. CheckFeature 是命令入口的门：关闭 → 403 FEATURE_DISABLED + 中文原因。
//
// 历史记录不在这里拦：关闭只拒绝新命令，列表/详情/历史仍按原权限可读。
package catalog

import (
	"context"

	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/store"
)

// 功能键，与 SpaceSettings 四个字段一一对应。
const (
	FeaturePrice       = "price"
	FeatureSale        = "sale"
	FeatureMaintenance = "maintenance"
	FeatureStocktake   = "stocktake"
)

// CodeFeatureDisabled 是功能关闭时命令入口返回的错误码（HTTP 403）。
// 放在本包而不是 httpx：这是库配置域的语义，其它模块只经 CheckFeature 触达。
const CodeFeatureDisabled = "FEATURE_DISABLED"

// featureBlockedReason 是关闭原因文案，前端直接展示，不经二次加工。
var featureBlockedReason = map[string]string{
	FeaturePrice:       "该库已关闭价格功能",
	FeatureSale:        "该库已关闭出售功能",
	FeatureMaintenance: "该库已关闭维修功能",
	FeatureStocktake:   "该库已关闭盘点功能",
}

// canOperate 判定某功能当前是否允许执行命令，返回 (允许, 中文原因)。
// 未知功能键不拦（向前兼容：新增开关不会误锁旧入口）。
func canOperate(settings SpaceSettings, feature string) (bool, string) {
	var on bool
	switch feature {
	case FeaturePrice:
		on = settings.Price
	case FeatureSale:
		on = settings.Sale
	case FeatureMaintenance:
		on = settings.Maintenance
	case FeatureStocktake:
		on = settings.Stocktake
	default:
		return true, ""
	}
	if on {
		return true, ""
	}
	return false, featureBlockedReason[feature]
}

// CanOperate 是 canOperate 的导出形态，供 item 的 allowed_actions 复用同一判定。
func CanOperate(settings SpaceSettings, feature string) (bool, string) {
	return canOperate(settings, feature)
}

// LoadSettings 只读库开关，不做权限判断：调用方必须先过库权限，
// 否则「403 功能关闭」会变成探测他人库配置的预言机。
// 库不存在时返回全开（调用方已判定对象可见）。
func LoadSettings(ctx context.Context, db *store.DB, spaceID string) (SpaceSettings, error) {
	var row spaceSettingsRow
	err := db.Gorm.WithContext(ctx).Raw(
		`SELECT settings FROM space WHERE id = ? AND deleted_at IS NULL`, spaceID).
		Scan(&row).Error
	if err != nil {
		return effectiveSettings(nil), err
	}
	return effectiveSettings(row.Settings), nil
}

// CheckFeature 是命令入口的开关门。spaceID 必须是调用方已完成权限校验的库；
// 关闭时返回 403 FEATURE_DISABLED + 中文原因，开启/未配置时返回 nil。
func CheckFeature(ctx context.Context, db *store.DB, spaceID, feature string) error {
	settings, err := LoadSettings(ctx, db, spaceID)
	if err != nil {
		return err
	}
	if ok, reason := canOperate(settings, feature); !ok {
		return httpx.New(403, CodeFeatureDisabled, reason)
	}
	return nil
}
