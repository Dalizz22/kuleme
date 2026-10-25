// 用户管理：/users 路由的服务层。
//
// 权限约定：列表/建号/改资料仅管理员；改密允许管理员代改或本人凭旧口令改。
// 禁用账号与改密都会撤销该账号全部会话，逼重新登录。
package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/klm/server/internal/httpx"
)

// UserInput 是建号请求体。
type UserInput struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	IsAdmin     bool   `json:"is_admin"`
}

// UserPatch 是改资料请求体；指针为 nil 表示不修改。
type UserPatch struct {
	DisplayName *string `json:"display_name"`
	IsAdmin     *bool   `json:"is_admin"`
	Disabled    *bool   `json:"disabled"`
}

// PasswordInput 是改密请求体。非管理员本人操作必须提供 old_password。
type PasswordInput struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// ListUsers 分页返回账号（不含口令摘要）。
func (s *Service) ListUsers(ctx context.Context, page, pageSize int) ([]User, int64, error) {
	page, pageSize = clampPage(page, pageSize)
	var total int64
	if err := s.db.Gorm.WithContext(ctx).Raw(`SELECT count(*) FROM app_user`).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计账号: %w", err)
	}
	// 空表必须序列化成 []，不是 null（§2 契约；null 会让客户端 d.items.length 抛错）。
	rows := []User{}
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT id, username, display_name, is_admin, disabled_at IS NOT NULL AS disabled,
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at
		  FROM app_user ORDER BY created_at DESC, id DESC
		 LIMIT ? OFFSET ?`, pageSize, (page-1)*pageSize).Scan(&rows).Error
	if err != nil {
		return nil, 0, fmt.Errorf("查询账号列表: %w", err)
	}
	return rows, total, nil
}

// CreateUser 管理员建号。用户名大小写不敏感唯一。
func (s *Service) CreateUser(ctx context.Context, username, password, displayName string, isAdmin bool) (User, error) {
	if !validUsername(username) {
		return User{}, httpx.Validation("用户名需 2-32 位字母、数字或下划线")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, httpx.Validation(fmt.Sprintf("密码至少 %d 位", MinPasswordLength))
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = username
	}
	id := uuid.NewString()
	err = s.db.Gorm.WithContext(ctx).Exec(`
		INSERT INTO app_user (id, username, display_name, password_hash, is_admin)
		VALUES (?, ?, ?, ?, ?)`, id, username, displayName, hash, isAdmin).Error
	if err != nil && isUniqueViolation(err) {
		return User{}, httpx.Validation("用户名已存在")
	}
	if err != nil {
		return User{}, fmt.Errorf("创建账号: %w", err)
	}
	return s.userByID(ctx, id)
}

// UpdateUser 管理员改资料。禁用会撤销全部会话；系统必须至少保留一个启用的管理员。
func (s *Service) UpdateUser(ctx context.Context, userID string, p UserPatch) (User, error) {
	cur, err := s.userByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	disabling := p.Disabled != nil && *p.Disabled && !cur.Disabled
	demoting := p.IsAdmin != nil && !*p.IsAdmin && cur.IsAdmin
	if disabling || demoting {
		ok, err := s.hasOtherActiveAdmin(ctx, userID)
		if err != nil {
			return User{}, err
		}
		if !ok {
			return User{}, httpx.New(409, httpx.CodeOpenBusiness, "系统至少要保留一个启用的管理员")
		}
	}
	if p.DisplayName != nil {
		if err := s.db.Gorm.WithContext(ctx).Exec(
			`UPDATE app_user SET display_name = ?, updated_at = now() WHERE id = ?`,
			strings.TrimSpace(*p.DisplayName), userID).Error; err != nil {
			return User{}, fmt.Errorf("更新显示名: %w", err)
		}
	}
	if p.IsAdmin != nil {
		if err := s.db.Gorm.WithContext(ctx).Exec(
			`UPDATE app_user SET is_admin = ?, version = version + 1, updated_at = now() WHERE id = ?`,
			*p.IsAdmin, userID).Error; err != nil {
			return User{}, fmt.Errorf("更新管理员标记: %w", err)
		}
	}
	if disabling {
		if err := s.db.Gorm.WithContext(ctx).Exec(
			`UPDATE app_user SET disabled_at = now(), version = version + 1, updated_at = now() WHERE id = ?`,
			userID).Error; err != nil {
			return User{}, fmt.Errorf("禁用账号: %w", err)
		}
		if err := s.RevokeUserSessions(ctx, userID); err != nil {
			return User{}, err
		}
	}
	if p.Disabled != nil && !*p.Disabled && cur.Disabled {
		if err := s.db.Gorm.WithContext(ctx).Exec(
			`UPDATE app_user SET disabled_at = NULL, version = version + 1, updated_at = now() WHERE id = ?`,
			userID).Error; err != nil {
			return User{}, fmt.Errorf("启用账号: %w", err)
		}
	}
	return s.userByID(ctx, userID)
}

// ChangePassword 改密：管理员可代改任意账号；本人必须验旧口令。改完撤销全部会话。
func (s *Service) ChangePassword(ctx context.Context, actor httpx.Actor, userID string, in PasswordInput) error {
	isSelf := actor.UserID == userID
	if !actor.IsAdmin && !isSelf {
		return httpx.Forbidden("只能修改自己的密码")
	}
	if !actor.IsAdmin {
		// 存在性校验：目标账号必须真实存在才继续。
		if _, err := s.userByID(ctx, userID); err != nil {
			return err
		}
		var hash string
		if err := s.db.Gorm.WithContext(ctx).Raw(
			`SELECT password_hash FROM app_user WHERE id = ?`, userID).Scan(&hash).Error; err != nil {
			return fmt.Errorf("读取口令摘要: %w", err)
		}
		if !VerifyPassword(hash, in.OldPassword) {
			return httpx.Validation("旧密码不正确")
		}
	}
	newHash, err := HashPassword(in.NewPassword)
	if err != nil {
		return httpx.Validation(fmt.Sprintf("密码至少 %d 位", MinPasswordLength))
	}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE app_user SET password_hash = ?, version = version + 1, updated_at = now() WHERE id = ?`,
		newHash, userID).Error; err != nil {
		return fmt.Errorf("更新口令: %w", err)
	}
	return s.RevokeUserSessions(ctx, userID)
}

// userByID 按 ID 读账号，不存在返回 404。
func (s *Service) userByID(ctx context.Context, id string) (User, error) {
	var row struct {
		ID          string
		Username    string
		DisplayName string
		IsAdmin     bool
		DisabledAt  *time.Time
		CreatedAt   time.Time
	}
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT id, username, display_name, is_admin, disabled_at, created_at
		  FROM app_user WHERE id = ?`, id).Scan(&row).Error
	if err != nil {
		return User{}, fmt.Errorf("查询账号: %w", err)
	}
	if row.ID == "" {
		return User{}, httpx.New(404, httpx.CodeNotFound, "账号不存在")
	}
	return User{
		ID:          row.ID,
		Username:    row.Username,
		DisplayName: row.DisplayName,
		IsAdmin:     row.IsAdmin,
		Disabled:    row.DisabledAt != nil,
		CreatedAt:   row.CreatedAt.Format(time.RFC3339),
	}, nil
}

// hasOtherActiveAdmin 判断除 excludeID 外是否还有启用的管理员。
func (s *Service) hasOtherActiveAdmin(ctx context.Context, excludeID string) (bool, error) {
	var n int64
	err := s.db.Gorm.WithContext(ctx).Raw(`
		SELECT count(*) FROM app_user
		 WHERE is_admin AND disabled_at IS NULL AND id <> ?`, excludeID).Scan(&n).Error
	if err != nil {
		return false, fmt.Errorf("统计管理员: %w", err)
	}
	return n > 0, nil
}

// clampPage 分页参数兜底：页码最小 1，页大小 1-100。
func clampPage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

// isUniqueViolation 粗判唯一约束冲突。
// ponytail: 字符串匹配够用；要细分错误码时换 pgconn.PgError
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}
