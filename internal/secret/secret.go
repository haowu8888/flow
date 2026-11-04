// Package secret 提供密钥值的加密存储（AES-256-GCM）与日志脱敏。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const version byte = 1

var ErrDecrypt = errors.New("secret: decryption failed (wrong FLOW_SECRET_KEY?)")

// Cipher 用主密钥加解密。密文格式：版本(1B) | nonce(12B) | 密文+tag。
// AAD 绑定密钥的归属（租户 / 流水线 / 名称），防止数据库中的密文被挪用到别处。
type Cipher struct {
	aead   cipher.AEAD
	macKey []byte // 由主密钥派生，用于签名一次性链接（如邮件审批）
}

func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	mk := sha256.Sum256(append([]byte("flow-mac:"), key...))
	return &Cipher{aead: aead, macKey: mk[:]}, nil
}

// MAC 返回 msg 的 HMAC-SHA256 签名（十六进制），用于校验由 Server 签发的链接。
func (c *Cipher) MAC(msg string) string {
	h := hmac.New(sha256.New, c.macKey)
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyMAC 以常量时间比较签名。
func (c *Cipher) VerifyMAC(msg, sig string) bool {
	return hmac.Equal([]byte(c.MAC(msg)), []byte(sig))
}

// NewRandomCipher 生成临时主密钥，仅用于内存存储与测试。
func NewRandomCipher() *Cipher {
	c, err := NewCipher(RandomKey())
	if err != nil {
		panic(err)
	}
	return c
}

func RandomKey() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}

func (c *Cipher) Seal(plain []byte, aad string) []byte {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	out := append([]byte{version}, nonce...)
	return c.aead.Seal(out, nonce, plain, []byte(aad))
}

func (c *Cipher) Open(data []byte, aad string) ([]byte, error) {
	n := c.aead.NonceSize()
	if len(data) < 1+n || data[0] != version {
		return nil, ErrDecrypt
	}
	plain, err := c.aead.Open(nil, data[1:1+n], data[1+n:], []byte(aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

// ParseKey 接受 32 字节密钥的 base64 或 hex 编码。
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if k, err := hex.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	return nil, errors.New("secret key must be 32 bytes encoded as base64 or hex (generate with: openssl rand -base64 32)")
}

// LoadOrCreateKeyFile 读取密钥文件，不存在时生成（权限 0600）。
func LoadOrCreateKeyFile(path string) (key []byte, created bool, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		k, err := ParseKey(string(data))
		return k, false, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	k := RandomKey()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n"); err != nil {
		return nil, false, err
	}
	return k, true, nil
}
