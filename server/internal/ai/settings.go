package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 本文件负责「AI 提供者配置的持久化与在请求内的传递」。
//
// 三条约定：
//
//  1. 落盘的是密文（enc:v1:...），明文密钥只在进程内存在一次请求的时间；
//  2. 读出来给接口看的一律走 Settings.Masked；
//  3. 主密钥不在这个文件里——它由 config.SecretPath 指向的配置卷保管，
//     本文件只持有 *Cipher 的引用。
//
// 存储选择：把配置存在数据目录的一个 JSON 文件里，而不是新建一张表。
// 理由：这是「整个实例一份」的配置，不是按库/按用户的数据；用文件省掉一张
// 表和一次迁移，也避免配置读写落在业务事务里。需要多实例共享配置时，
// 实现一个基于数据库的 SettingsStore 替换即可，本模块调用方不用改。

// 配置文件名。放数据目录下，与 master.key 同级。
const settingsFileName = "ai-settings.json"

// 密钥在 JSON 里的字段名带 stored_ 前缀。
//
// 好处是它不可能与 Settings 结构体的 json tag 撞名：直接复用
// Settings 序列化会把明文密钥按 "api_key" 写进文件，而解出来时
// 又因为字段名相同被当成明文继续用——这条路径必须一眼看得见。
type settingsFile struct {
	Provider       string    `json:"provider"`
	BaseURL        string    `json:"base_url"`
	StoredAPIKey   string    `json:"stored_api_key,omitempty"`
	Model          string    `json:"model"`
	TimeoutSeconds int       `json:"timeout_seconds,omitempty"`
	MaxImages      int       `json:"max_images,omitempty"`
	Enabled        bool      `json:"enabled"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

// 默认值。
const (
	DefaultProviderName   = ProviderName
	DefaultTimeoutSeconds = 60
	DefaultMaxImages      = 8
	// MaxImagesLimit 是一次任务能发出去的图片上限，防止一次「识别整个库」。
	MaxImagesLimit = 32
	// TimeoutSecondsRange 是允许配置的超时区间。
	MinTimeoutSeconds = 5
	MaxTimeoutSeconds = 300
)

// FileSettingsStore 把 AI 配置存在数据目录的 JSON 文件里。
type FileSettingsStore struct {
	path string
	ciph *Cipher
	mu   sync.Mutex
}

// NewFileSettingsStore 创建文件型配置存储。
//
// cipher 为 nil 时：读得到已有配置，但保存密钥会失败。这种情况出现在
// 「主密钥文件缺失且不允许生成」的只读部署上——此时应当明确报错，
// 而不是把密钥明文写进文件。
func NewFileSettingsStore(dataDir string, cipher *Cipher) *FileSettingsStore {
	if dataDir == "" {
		dataDir = "."
	}
	return &FileSettingsStore{
		path: filepath.Join(dataDir, settingsFileName),
		ciph: cipher,
	}
}

// Path 返回配置文件路径（用于日志与诊断，不含内容）。
func (s *FileSettingsStore) Path() string { return s.path }

// Load 读取配置。文件不存在时返回默认配置（可用但没有提供者）。
func (s *FileSettingsStore) Load(ctx context.Context) (Settings, error) {
	if err := ctx.Err(); err != nil {
		return Settings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultSettings(), nil
		}
		return Settings{}, err
	}
	var f settingsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return Settings{}, fmt.Errorf("AI 配置无法解析：%v", err)
	}
	out := Settings{
		Provider:       f.Provider,
		BaseURL:        f.BaseURL,
		Model:          f.Model,
		TimeoutSeconds: f.TimeoutSeconds,
		MaxImages:      f.MaxImages,
		Enabled:        f.Enabled,
		UpdatedAt:      f.UpdatedAt,
	}
	if f.StoredAPIKey != "" {
		key, err := s.ciph.Decrypt(f.StoredAPIKey)
		if err != nil {
			// 密钥解不开不该让整个设置页打不开：其余字段仍然可用，
			// 但必须让使用者知道「密钥需要重新填一次」。
			out.APIKey = ""
			out.needsRekey = true
		} else {
			out.APIKey = key
		}
	}
	return out.normalized(), nil
}

// Save 写入配置，密钥加密后落盘。
func (s *FileSettingsStore) Save(ctx context.Context, in Settings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in = in.normalized()
	if err := in.Validate(); err != nil {
		return err
	}
	if in.APIKey != "" && s.ciph == nil {
		return fmt.Errorf("%w: 无法加密保存密钥，请检查主密钥文件", ErrNoMasterKey)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := s.ciph.Encrypt(in.APIKey)
	if err != nil {
		return err
	}
	f := settingsFile{
		Provider:       in.Provider,
		BaseURL:        strings.TrimRight(in.BaseURL, "/"),
		StoredAPIKey:   stored,
		Model:          in.Model,
		TimeoutSeconds: in.TimeoutSeconds,
		MaxImages:      in.MaxImages,
		Enabled:        in.Enabled,
		UpdatedAt:      time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	// 先写临时文件再改名：半个配置文件会让服务起不来，
	// 而这份文件恰好保存着唯一一份可用的密钥。
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// DefaultSettings 是「还没配置过」的默认值。
//
// Enabled 默认 false：没配提供者就发起识别，只会得到一堆网络错误，
// 不如让接口明确返回「未启用」。
func DefaultSettings() Settings {
	return Settings{
		Provider:       ProviderName,
		TimeoutSeconds: DefaultTimeoutSeconds,
		MaxImages:      DefaultMaxImages,
		Enabled:        false,
	}
}

// normalized 补默认值并夹住范围。
func (s Settings) normalized() Settings {
	if s.Provider == "" {
		s.Provider = ProviderName
	}
	s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	s.Model = strings.TrimSpace(s.Model)
	s.APIKey = strings.TrimSpace(s.APIKey)
	if s.TimeoutSeconds <= 0 {
		s.TimeoutSeconds = DefaultTimeoutSeconds
	}
	s.TimeoutSeconds = clampInt(s.TimeoutSeconds, MinTimeoutSeconds, MaxTimeoutSeconds)
	if s.MaxImages <= 0 {
		s.MaxImages = DefaultMaxImages
	}
	s.MaxImages = clampInt(s.MaxImages, 1, MaxImagesLimit)
	return s
}

// Validate 检查配置是否自洽。
//
// 只有「启用」时才要求 base_url 与 model：允许先保存一半再填完，
// 否则设置页会被自己的校验卡住，用户没法分段保存。
func (s Settings) Validate() error {
	if !s.Enabled {
		return nil
	}
	if s.Provider == "" {
		return fmt.Errorf("请选择 AI 提供者")
	}
	if s.BaseURL == "" {
		return fmt.Errorf("请填写接口地址")
	}
	if !strings.HasPrefix(s.BaseURL, "http://") && !strings.HasPrefix(s.BaseURL, "https://") {
		return fmt.Errorf("接口地址需要以 http:// 或 https:// 开头")
	}
	if s.Model == "" {
		return fmt.Errorf("请填写模型标识")
	}
	return nil
}

// Configured 表示「已经具备发起识别的条件」。
func (s Settings) Configured() bool {
	return s.Enabled && s.BaseURL != "" && s.Model != ""
}

// --- 请求内的配置传递 ---
//
// 提供者是窄接口，签名里没有 Settings 参数（那会让每个实现都要接收密钥）。
// 配置由调用方放进 context，客户端从中取出。这样：
//   - 提供者实现拿不到存储，也拿不到别人的配置；
//   - 任务处理器不必把密钥当参数一路传递（也就不容易进日志）。

type settingsCtxKey struct{}

// WithSettings 把配置放进 context。
func WithSettings(ctx context.Context, s Settings) context.Context {
	return context.WithValue(ctx, settingsCtxKey{}, s)
}

// SettingsFromContext 取出配置。
func SettingsFromContext(ctx context.Context) (Settings, bool) {
	s, ok := ctx.Value(settingsCtxKey{}).(Settings)
	return s, ok
}

// maskedWithRekey 与 Masked 相同，但把「密钥需要重填」显式带出去。
//
// 单独一个方法而不是塞进 Masked：needsRekey 只在「本实例读过一次
// 解不开的密文」时为真，是存储层的事实，不是配置本身的性质。
func (s Settings) maskedWithRekey() MaskedSettings {
	m := s.Masked()
	m.RekeyNeeded = s.needsRekey
	return m
}
