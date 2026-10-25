// Package config 读取运行期配置。所有值来自环境变量，未设置时使用可本地启动的默认值。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是进程的完整运行期配置。
type Config struct {
	// Addr 是 HTTP 监听地址，形如 :8080。
	Addr string
	// DataDir 存放运行期数据：主密钥、instance_id、照片文件。
	DataDir string
	// DatabaseURL 是 PostgreSQL 连接串。为空时不连接，服务以受限模式启动。
	DatabaseURL string
	// PublicBaseURL 用于生成图片与文件链接。地址变化后链接失效，导出需重新生成。
	PublicBaseURL string
	// MaxUploadBytes 是单张照片上传上限。
	MaxUploadBytes int64
	// LoginRatePerMinute 是同一来源每分钟允许的登录尝试次数。
	LoginRatePerMinute int
	// Env 是运行环境：production / development（默认 development）。
	// production 模式收紧初始化等安全边界。
	Env string
	// CORSOrigins 是允许跨域访问的来源（KLM_CORS_ORIGINS，逗号分隔）。
	// 只做精确匹配（含 scheme 与端口），不接受 * ；development 另放行局域网来源。
	CORSOrigins []string

	// API 版本与兼容下限由服务端声明，客户端据此判断是否可用。
	APIVersion    string
	MinAPIVersion string
}

// Load 返回配置。只读取环境变量，不做校验兜底之外的推断。
func Load() Config {
	dataDir := env("KLM_DATA_DIR", "./data")
	return Config{
		Addr:               env("KLM_ADDR", ":8080"),
		DataDir:            dataDir,
		DatabaseURL:        env("KLM_DATABASE_URL", defaultDatabaseURL()),
		PublicBaseURL:      strings.TrimRight(env("KLM_PUBLIC_BASE_URL", ""), "/"),
		MaxUploadBytes:     envInt64("KLM_MAX_UPLOAD_BYTES", 20<<20),
		LoginRatePerMinute: int(envInt64("KLM_LOGIN_RATE_PER_MINUTE", 10)),
		Env:                env("KLM_ENV", "development"),
		CORSOrigins:        envList("KLM_CORS_ORIGINS", defaultCORSOrigins),
		APIVersion:         env("KLM_API_VERSION", "1.0"),
		MinAPIVersion:      env("KLM_MIN_API_VERSION", "1.0"),
	}
}

// IsProduction 报告是否以生产模式运行（KLM_ENV=production）。
func (c Config) IsProduction() bool { return c.Env == "production" }

// MediaDir 返回照片与导出文件的存放目录。
func (c Config) MediaDir() string { return filepath.Join(c.DataDir, "media") }

// SecretPath 返回主密钥文件路径。主密钥不与密文放在同一处暴露给客户端。
func (c Config) SecretPath() string { return filepath.Join(c.DataDir, "master.key") }

// InstanceIDPath 返回实例身份文件路径。恢复备份时必须保留该文件才能识别为同一实例。
func (c Config) InstanceIDPath() string { return filepath.Join(c.DataDir, "instance_id") }

// EnsureDirs 建立运行期目录。
func (c Config) EnsureDirs() error {
	for _, dir := range []string{c.DataDir, c.MediaDir()} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("创建目录 %s: %w", dir, err)
		}
	}
	return nil
}

// defaultCORSOrigins 是 Web 与 Capacitor 客户端的默认来源。
// 局域网地址不在此列：需要时显式写进 KLM_CORS_ORIGINS。
var defaultCORSOrigins = []string{"capacitor://localhost", "http://localhost", "https://localhost"}

func defaultDatabaseURL() string {
	return "postgres://klm:klm@localhost:5432/klm?sslmode=disable"
}

// envList 读取逗号分隔的列表；未设置时用默认值。
func envList(key string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return append([]string(nil), fallback...)
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), fallback...)
	}
	return out
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}
