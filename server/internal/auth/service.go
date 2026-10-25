// Package auth 实现账号、初始化、登录限速与可撤销会话。
//
// 会话两种传输方式共用同一套服务端记录：
//   - Web：同源 HttpOnly Cookie（klm_session），登录成功时下发；
//   - App：Authorization: Bearer <token>，令牌在服务端只存 SHA-256 摘要。
//
// 数据库访问用 SQL 而不是 ORM 模型：账号域只有少量固定查询，
// 引入模型结构体不会让任何一行变短。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/klm/server/internal/config"
	"github.com/klm/server/internal/httpx"
	"github.com/klm/server/internal/labels/kod"
	"github.com/klm/server/internal/store"
)

// 编译期确认本包实现了 httpx 需要的鉴权入口。
var _ httpx.AuthCarrier = (*Service)(nil)

// 会话与限速的固定参数。改这里同时改交接文档，不散落在调用处。
const (
	SessionTTLHours   = 24 * 14
	InstanceHeader    = "X-KLM-Instance"
	MinPasswordLength = 10
)

var (
	// ErrNotInitialized 表示实例尚未完成首次初始化，只允许 /setup。
	ErrNotInitialized = errors.New("实例尚未初始化")
	// ErrAlreadyInitialized 表示初始化入口已关闭。
	ErrAlreadyInitialized = errors.New("实例已初始化")
	// ErrBadCredentials 表示账号或密码错误。对外文案统一，不区分是哪一项错。
	ErrBadCredentials = errors.New("账号或密码错误")
	// ErrSessionExpired 表示会话已失效、被撤销或账号被禁用。
	ErrSessionExpired = errors.New("登录已失效")
	// ErrWeakPassword 表示密码不满足长度要求。
	ErrWeakPassword = errors.New("密码不符合要求")
	// ErrRateLimited 表示同一来源登录尝试过于频繁。
	ErrRateLimited = errors.New("操作过于频繁")
)

// User 是账号的对外形态，不含任何口令字段。
type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsAdmin     bool   `json:"is_admin"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   string `json:"created_at"`
}

// SetupResult 是一次性初始化凭据的明文返回。只在首次启动打印一次，不落库明文。
type SetupResult struct {
	Secret string
}

// Service 持有数据库与配置依赖。
type Service struct {
	db     *store.DB
	logger *slog.Logger
	cfg    config.Config
}

// New 构造鉴权服务。进程内可以存在多个实例：限速状态在包级共享。
func New(db *store.DB, logger *slog.Logger, cfg config.Config) *Service {
	return &Service{db: db, logger: logger, cfg: cfg}
}

// ---------------------------------------------------------------------------
// 登录限速：进程内固定窗口。「同一来源」按调用方给的 source（客户端 IP）计。
//
// ponytail: 单实例部署用进程内固定窗口足够；多副本部署时换成
// 数据库或共享存储计数，接口 allowLogin 不变。
// ---------------------------------------------------------------------------

var (
	limiterMu   sync.Mutex
	limiterHits = map[string]int{}
	limiterSlot time.Time
)

// allowLogin 报告该来源本次登录是否放行。
func allowLogin(source string, perMinute int) bool {
	if perMinute <= 0 {
		return true
	}
	limiterMu.Lock()
	defer limiterMu.Unlock()
	now := time.Now()
	if limiterSlot.IsZero() || now.Sub(limiterSlot) >= time.Minute {
		limiterSlot = now
		limiterHits = map[string]int{}
	}
	limiterHits[source]++
	return limiterHits[source] <= perMinute
}

// ---------------------------------------------------------------------------
// 初始化：一次性凭据 + 首个管理员
// ---------------------------------------------------------------------------

// EnsureSetupCredential 在首次启动时生成一次性初始化凭据并返回明文，
// 已初始化时返回空字符串。凭据哈希写入 setup_credential，每次未初始化
// 启动都轮换一次，避免日志里的旧凭据长期有效。
func (s *Service) EnsureSetupCredential(ctx context.Context) (SetupResult, error) {
	if !s.db.Available() {
		return SetupResult{}, nil
	}
	instanceID, err := s.ensureInstanceID()
	if err != nil {
		return SetupResult{}, err
	}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO instance_meta (id, instance_id) VALUES (1, ?)
		 ON CONFLICT (id) DO NOTHING`, instanceID).Error; err != nil {
		return SetupResult{}, fmt.Errorf("写入实例记录: %w", err)
	}
	if init, err := s.Initialized(ctx); err != nil || init {
		return SetupResult{}, err
	}
	// 一次性凭据复用 12 位 Crockford 随机串（约 60 位熵）：
	// 一次性使用且初始化后入口永久关闭，长度不再加码。
	secret, err := kod.NewInstanceID()
	if err != nil {
		return SetupResult{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return SetupResult{}, err
	}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO setup_credential (id, secret_hash) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET secret_hash = EXCLUDED.secret_hash, consumed_at = NULL`,
		string(hash)).Error; err != nil {
		return SetupResult{}, fmt.Errorf("写入初始化凭据: %w", err)
	}
	return SetupResult{Secret: secret}, nil
}

// Initialized 报告实例是否已经完成初始化。
func (s *Service) Initialized(ctx context.Context) (bool, error) {
	if !s.db.Available() {
		return false, nil
	}
	var init bool
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT initialized FROM instance_meta WHERE id = 1`).Scan(&init).Error
	if err != nil {
		return false, fmt.Errorf("查询实例状态: %w", err)
	}
	return init, nil
}

// Setup 创建首个管理员。secret 为空 = 网页向导模式（未初始化时首访问者可抢先，
// 初始化后入口永久关闭）；携带一次性凭据则按凭据校验。已初始化时返回
// ErrAlreadyInitialized 且不修改任何数据。
// 生产模式（KLM_ENV=production）必须携带一次性凭据，空 secret 不允许初始化。
func (s *Service) Setup(ctx context.Context, secret, username, password, displayName string) (User, error) {
	if s.cfg.IsProduction() && secret == "" {
		return User{}, httpx.Forbidden("生产环境必须使用一次性初始化凭据，不能通过空凭据初始化")
	}
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if !validUsername(username) {
		return User{}, httpx.Validation("用户名只能包含字母、数字和下划线，长度 2-32")
	}
	if err := s.db.Gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var init bool
		if err := tx.Raw(`SELECT initialized FROM instance_meta WHERE id = 1`).Scan(&init).Error; err != nil {
			return err
		}
		if init {
			return ErrAlreadyInitialized
		}
		if secret != "" {
			var hash string
			var consumed *time.Time
			err := tx.Raw(`SELECT secret_hash, consumed_at FROM setup_credential WHERE id = 1`).
				Row().Scan(&hash, &consumed)
			if err != nil {
				return ErrBadCredentials
			}
			if consumed != nil || !VerifyPassword(hash, secret) {
				return ErrBadCredentials
			}
		}
		// secret 为空 = 网页向导模式：实例未初始化时首访问者可抢先建管理员，
		// 初始化完成后入口永久关闭；不可信网络部署应携带一次性凭据初始化。
		var taken bool
		if err := tx.Raw(`SELECT true FROM app_user WHERE lower(username) = lower(?)`, username).
			Scan(&taken).Error; err != nil {
			return err
		}
		if taken {
			return httpx.Validation("用户名已存在")
		}
		passwordHash, err := HashPassword(password)
		if err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO app_user (id, username, display_name, password_hash, is_admin)
			 VALUES (?, ?, ?, ?, true)`,
			uuid.NewString(), username, displayName, passwordHash).Error; err != nil {
			return err
		}
		if err := tx.Exec(`UPDATE instance_meta SET initialized = true WHERE id = 1`).Error; err != nil {
			return err
		}
		return tx.Exec(`UPDATE setup_credential SET consumed_at = now() WHERE id = 1`).Error
	}); err != nil {
		if errors.Is(err, ErrAlreadyInitialized) || errors.Is(err, ErrBadCredentials) {
			return User{}, err
		}
		var httpErr *httpx.Error
		if errors.As(err, &httpErr) {
			return User{}, httpErr
		}
		return User{}, fmt.Errorf("完成初始化: %w", err)
	}
	return s.userByUsername(ctx, username)
}

// ensureInstanceID 读取或创建实例身份文件。文件是实例身份的唯一事实来源，
// 数据库记录只是缓存，恢复备份后仍以文件为准。
func (s *Service) ensureInstanceID() (string, error) {
	path := s.cfg.InstanceIDPath()
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); kod.ValidInstanceID(id) {
			return id, nil
		}
	}
	id, err := kod.NewInstanceID()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("写入实例身份文件: %w", err)
	}
	s.logger.Info("生成新的实例身份", "instance_id", id)
	return id, nil
}

// ---------------------------------------------------------------------------
// 登录、注销、会话
// ---------------------------------------------------------------------------

// Login 校验口令、建立会话并返回令牌明文。令牌只在此处出现一次。
// 来源限速超过 config.LoginRatePerMinute 时返回 ErrRateLimited。
func (s *Service) Login(ctx context.Context, username, password, source string) (string, User, error) {
	if !allowLogin(source, s.cfg.LoginRatePerMinute) {
		return "", User{}, ErrRateLimited
	}
	if !s.db.Available() {
		return "", User{}, store.ErrUnavailable
	}
	var row struct {
		ID           string
		Username     string
		DisplayName  string
		IsAdmin      bool
		DisabledAt   *time.Time
		CreatedAt    time.Time
		PasswordHash string
	}
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, username, display_name, is_admin, disabled_at, created_at, password_hash
		 FROM app_user WHERE lower(username) = lower(?)`,
		strings.TrimSpace(username)).Scan(&row).Error
	if err != nil {
		return "", User{}, fmt.Errorf("查询账号: %w", err)
	}
	// 账号不存在与密码错误同一文案；不存在的账号也消耗一次限速额度。
	if row.ID == "" || row.DisabledAt != nil || !VerifyPassword(row.PasswordHash, password) {
		return "", User{}, ErrBadCredentials
	}
	token, digest, err := NewToken()
	if err != nil {
		return "", User{}, err
	}
	if err := s.db.Gorm.WithContext(ctx).Exec(
		`INSERT INTO app_session (id, user_id, token_digest, expires_at)
		 VALUES (?, ?, ?, now() + ? * interval '1 hour')`,
		uuid.NewString(), row.ID, digest, SessionTTLHours).Error; err != nil {
		return "", User{}, fmt.Errorf("建立会话: %w", err)
	}
	return token, User{
		ID:          row.ID,
		Username:    row.Username,
		DisplayName: row.DisplayName,
		IsAdmin:     row.IsAdmin,
		CreatedAt:   row.CreatedAt.Format(time.RFC3339),
	}, nil
}

// Logout 撤销当前会话。令牌无效或已注销同样返回成功（幂等）。
func (s *Service) Logout(ctx context.Context, token string) error {
	if !s.db.Available() || token == "" {
		return nil
	}
	return s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE app_session SET revoked_at = now()
		 WHERE token_digest = ? AND revoked_at IS NULL`,
		tokenDigest(token)).Error
}

// Authenticate 实现 httpx.AuthCarrier：从 Bearer 令牌或会话 Cookie 解析操作者。
// ok=false 表示确实没有携带有效凭据（含令牌过期/被撤销/账号被禁用）；
// err 非空表示存储不可用。
func (s *Service) Authenticate(c *gin.Context) (httpx.Actor, bool, error) {
	token := bearerToken(c)
	if token == "" {
		if cookie, err := c.Cookie(httpx.SessionCookieName); err == nil {
			token = cookie
		}
	}
	if token == "" || !s.db.Available() {
		return httpx.Actor{}, false, nil
	}
	var actor httpx.Actor
	err := s.db.Gorm.WithContext(c.Request.Context()).Raw(
		`SELECT s.id, u.id, u.username, u.display_name, u.is_admin
		 FROM app_session s JOIN app_user u ON u.id = s.user_id
		 WHERE s.token_digest = ? AND s.revoked_at IS NULL
		   AND s.expires_at > now() AND u.disabled_at IS NULL`,
		tokenDigest(token)).Row().
		Scan(&actor.SessionID, &actor.UserID, &actor.Username, &actor.DisplayName, &actor.IsAdmin)
	if err != nil {
		return httpx.Actor{}, false, nil
	}
	return actor, true, nil
}

// RevokeUserSessions 撤销某账号的全部会话，用于禁用账号与改密。
func (s *Service) RevokeUserSessions(ctx context.Context, userID string) error {
	if !s.db.Available() {
		return nil
	}
	return s.db.Gorm.WithContext(ctx).Exec(
		`UPDATE app_session SET revoked_at = now()
		 WHERE user_id = ? AND revoked_at IS NULL`, userID).Error
}

// Me 返回当前会话对应账号的最新资料（含禁用状态），会话失效返回 ErrSessionExpired。
func (s *Service) Me(ctx context.Context, actor httpx.Actor) (User, error) {
	u, err := s.userByUsername(ctx, actor.Username)
	if err != nil {
		return User{}, ErrSessionExpired
	}
	if u.Disabled {
		return User{}, ErrSessionExpired
	}
	return u, nil
}

func (s *Service) userByUsername(ctx context.Context, username string) (User, error) {
	var row struct {
		ID          string
		Username    string
		DisplayName string
		IsAdmin     bool
		DisabledAt  *time.Time
		CreatedAt   time.Time
	}
	err := s.db.Gorm.WithContext(ctx).Raw(
		`SELECT id, username, display_name, is_admin, disabled_at, created_at
		 FROM app_user WHERE lower(username) = lower(?)`, username).Scan(&row).Error
	if err != nil || row.ID == "" {
		return User{}, fmt.Errorf("查询账号: %w", err)
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

// ---------------------------------------------------------------------------
// 口令与令牌
// ---------------------------------------------------------------------------

// HashPassword 生成口令摘要（bcrypt），供初始化与管理员建账号共用。
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("计算口令摘要: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword 校验口令摘要。
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// NewToken 生成会话令牌明文与其存储摘要。
func NewToken() (string, []byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("生成会话令牌: %w", err)
	}
	plain := base64.RawURLEncoding.EncodeToString(buf)
	return plain, tokenDigest(plain), nil
}

// tokenDigest 计算令牌的存储摘要。摘要不可逆，泄露数据库也拿不到令牌。
func tokenDigest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// SetSessionCookie 按 config 决定是否下发 Cookie；App 走 Bearer 时不调用。
func (s *Service) SetSessionCookie(c *gin.Context, token string) {
	secure := strings.HasPrefix(s.cfg.PublicBaseURL, "https")
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(httpx.SessionCookieName, token, SessionTTLHours*3600, "/", "", secure, true)
}

// ClearSessionCookie 注销与切换账号时清除 Cookie。
func (s *Service) ClearSessionCookie(c *gin.Context) {
	c.SetCookie(httpx.SessionCookieName, "", -1, "/", "", false, true)
}

// validUsername 校验管理员自选用户名：2-32 位字母数字下划线。
func validUsername(s string) bool {
	if len(s) < 2 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// HTTPStatusFor 说明错误到 HTTP 的映射，避免各调用处自行翻译。
func HTTPStatusFor(err error) int {
	switch {
	case errors.Is(err, ErrBadCredentials), errors.Is(err, ErrSessionExpired):
		return http.StatusUnauthorized
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrAlreadyInitialized):
		return http.StatusConflict
	case errors.Is(err, ErrWeakPassword):
		// 业务校验与格式错误统一 422：错误码白名单里没有 400。
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
