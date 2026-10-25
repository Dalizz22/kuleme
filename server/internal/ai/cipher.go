package ai

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// 本文件负责「AI 密钥加密保存」与实例标识。
//
// 三条不变式：
//
//  1. 密文与主密钥分开存放：密文进数据目录的配置文件，主密钥只在配置卷里；
//  2. 主密钥从不出现在接口响应、日志、错误信息里；
//  3. 主密钥不是明文可替换的常量——首次运行生成一次，之后只读。
//
// 用的算法是 AES-256-GCM：认证加密，密文被改动会解密失败而不是解出垃圾。
// 每次加密生成新的随机 nonce，前缀带版本号，将来换算法时能识别旧密文。

const (
	// masterKeyLen 是主密钥长度（AES-256）。
	masterKeyLen = 32
	// keyVersionPrefix 是密文前缀，用于将来的算法迁移。
	keyVersionPrefix = "enc:v1:"
	// instanceIDLen 是实例码长度，与 labels/kod.go 的 InstanceIDLen 一致。
	instanceIDLen = 12
)

// ErrNoMasterKey 表示主密钥缺失或格式不对。
var ErrNoMasterKey = errors.New("主密钥不可用")

// Cipher 用主密钥加解密短文本（AI 密钥、将来的第三方令牌）。
type Cipher struct {
	aead cipher.AEAD
}

// LoadCipher 从主密钥文件装载。文件不存在时按需生成。
//
// create 为 false 时文件缺失返回 ErrNoMasterKey：只读部署（例如多实例
// 共用一份配置卷）不该悄悄生成一个只在本机有效的新密钥，那会让另一台
// 机器解不开已有密文。
func LoadCipher(path string, create bool) (*Cipher, error) {
	key, err := loadMasterKey(path, create)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoMasterKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoMasterKey, err)
	}
	return &Cipher{aead: aead}, nil
}

// loadMasterKey 读取或生成主密钥。
func loadMasterKey(path string, create bool) ([]byte, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return nil, fmt.Errorf("%w: %s 是目录", ErrNoMasterKey, path)
		}
		// 权限过宽就报警而不是拒绝：Windows 上 0600 未必可表达，
		// 直接拒绝会让服务在那些平台上起不来。
		if info.Mode().Perm()&0o077 != 0 {
			// 不自动 chmod：那是在动别人的配置卷。提示由调用方决定。
			return os.ReadFile(path)
		}
		return os.ReadFile(path)
	case !create:
		return nil, fmt.Errorf("%w: %s 不存在", ErrNoMasterKey, path)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %v", ErrNoMasterKey, err)
	}

	key := make([]byte, masterKeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoMasterKey, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoMasterKey, err)
	}
	// 先写临时文件再改名：避免写入过程中崩溃留下半个密钥，
	// 那会让已有密文永久解不开。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, key, 0o600); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoMasterKey, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("%w: %v", ErrNoMasterKey, err)
	}
	return key, nil
}

// Encrypt 加密明文，返回 enc:v1:<base64> 形态的字符串。
//
// 空字符串返回空字符串：没配置密钥就是没配置，不该产生一段密文
// 让「是否设置过」的判断变复杂。
func (c *Cipher) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if c == nil {
		return "", ErrNoMasterKey
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return keyVersionPrefix + base64Std(sealed), nil
}

// Decrypt 解密 Encrypt 的输出。非本方案产生的字符串按「明文」处理。
//
// 为什么容忍明文：配置可能来自环境变量或更早的版本。把明文当密文解会
// 直接报错，把一个能用的部署弄坏；这里选择兼容，并在设置页提示建议保存
// 一次以完成加密迁移。
func (c *Cipher) Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, keyVersionPrefix) {
		return stored, nil
	}
	if c == nil {
		return "", ErrNoMasterKey
	}
	raw, err := base64StdDecode(strings.TrimPrefix(stored, keyVersionPrefix))
	if err != nil {
		return "", fmt.Errorf("%w: 密文格式不正确", ErrNoMasterKey)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("%w: 密文长度不足", ErrNoMasterKey)
	}
	out, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		// 不把底层错误包出来：它可能带密文片段。
		return "", fmt.Errorf("%w: 解密失败（主密钥可能已更换）", ErrNoMasterKey)
	}
	return string(out), nil
}

// LoadOrCreateInstanceID 读取实例码，不存在时生成一次。
//
// 实例码进二维码载荷（KLM1:<instance>:...），因此必须稳定：
// 换一次实例码，之前印出去的标签就都指向别的实例了。
func LoadOrCreateInstanceID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(raw))
		if validInstanceID(id) {
			return id, nil
		}
		return "", fmt.Errorf("实例码格式不正确：%s", path)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	id, err := newInstanceID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return id, nil
}
