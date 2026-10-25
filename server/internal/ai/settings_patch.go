package ai

import (
	"context"

	"github.com/klm/server/internal/httpx"
)

// SettingsPatch 是 PUT /ai/settings 的请求体。
//
// 全部指针：nil 表示「本次不改这个字段」。用值类型时，客户端只改
// timeout_seconds 会把 max_images 静默重置成默认值（服务端陷阱，任何
// 新客户端都会踩）；显式传 0/负数仍按非法处理，不静默取默认。
type SettingsPatch struct {
	Provider       *string `json:"provider"`
	BaseURL        *string `json:"base_url"`
	APIKey         *string `json:"api_key"`
	Model          *string `json:"model"`
	TimeoutSeconds *int    `json:"timeout_seconds"`
	MaxImages      *int    `json:"max_images"`
	Enabled        *bool   `json:"enabled"`
}

// PatchSettings 合并式保存配置：只改 patch 里出现的字段，其余保持原值。
//
// api_key 传空串仍等于「保留旧密钥」（既有语义）；要清空密钥请显式传一个
// 空格以外的值 —— 本期没有「删除密钥」的需求，不额外造标记。
// 全部字段都不传是无操作，直接返回当前配置（不重写文件、不动 updated_at）。
func (s *Service) PatchSettings(ctx context.Context, actor httpx.Actor, patch SettingsPatch) (MaskedSettings, error) {
	if !actor.IsAdmin {
		return MaskedSettings{}, ErrForbidden
	}
	next, err := s.settings.Load(ctx)
	if err != nil {
		return MaskedSettings{}, err
	}
	if patch.TimeoutSeconds != nil && *patch.TimeoutSeconds <= 0 {
		return MaskedSettings{}, invalid("timeout_seconds 必须大于 0")
	}
	if patch.MaxImages != nil && *patch.MaxImages <= 0 {
		return MaskedSettings{}, invalid("max_images 必须大于 0")
	}
	if patch.Provider != nil {
		next.Provider = *patch.Provider
	}
	if patch.BaseURL != nil {
		next.BaseURL = *patch.BaseURL
	}
	if patch.APIKey != nil && *patch.APIKey != "" {
		next.APIKey = *patch.APIKey
	}
	if patch.Model != nil {
		next.Model = *patch.Model
	}
	if patch.TimeoutSeconds != nil {
		next.TimeoutSeconds = *patch.TimeoutSeconds
	}
	if patch.MaxImages != nil {
		next.MaxImages = *patch.MaxImages
	}
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if !patch.any() {
		return next.maskedWithRekey(), nil
	}
	next = next.normalized()
	if err := next.Validate(); err != nil {
		return MaskedSettings{}, invalid(err.Error())
	}
	if err := s.settings.Save(ctx, next); err != nil {
		return MaskedSettings{}, err
	}
	return next.maskedWithRekey(), nil
}

func (p SettingsPatch) any() bool {
	return p.Provider != nil || p.BaseURL != nil || p.APIKey != nil || p.Model != nil ||
		p.TimeoutSeconds != nil || p.MaxImages != nil || p.Enabled != nil
}
