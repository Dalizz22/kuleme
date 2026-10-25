// item 包命令入口的功能开关门。
//
// 口径：关闭只拒「新操作」，历史记录仍可读；开关判定必须在权限校验之后，
// 否则「403 功能关闭」会变成探测他人库配置的预言机。
//
// 明确不拦的入口：转库、撤销出售、导入确认、AI 确认、照片四接口——
// 这些要么是纠正性操作，要么本身不属于被开关管辖的功能。
package item

import (
	"context"

	"github.com/klm/server/internal/catalog"
	"github.com/klm/server/internal/templates"
)

// featureGate 返回 403 FEATURE_DISABLED + 中文原因（开关关闭时）。
func (s *itemService) featureGate(ctx context.Context, spaceID, feature string) error {
	return catalog.CheckFeature(ctx, s.db, spaceID, feature)
}

// hasPriceField 判断本次提交的附加字段里是否含价格/金额键。
// 键集取自模板 manifest（unit=currency 或 type=money），不写死 "price"：
// 内置模板里价格键既有 price（library）也有 purchase_price（general/gear/...）。
func (s *itemService) hasPriceField(templateKey string, fields map[string]any) bool {
	if len(fields) == 0 {
		return false
	}
	m, err := s.tcat.ValidateTemplateKey(templateKey)
	if err != nil {
		// 未知模板的报错交给 mergeFields（它负责 400/422 与逐字段原因）。
		return false
	}
	for _, f := range m.Fields {
		if f.Unit != templates.UnitCurrency && f.Type != templates.TypeMoney {
			continue
		}
		if _, hit := fields[f.FieldID]; hit {
			return true
		}
	}
	return false
}
