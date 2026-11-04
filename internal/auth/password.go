// Package auth 实现登录、会话 / 个人访问令牌、租户成员与角色。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// pbkdf2Iterations 参考 OWASP 对 PBKDF2-HMAC-SHA256 的建议值。
const pbkdf2Iterations = 600_000

// HashPassword 返回 "pbkdf2-sha256$迭代次数$salt$hash"（base64）。
func HashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	return encodeHash(pbkdf2Iterations, salt, pbkdf2([]byte(password), salt, pbkdf2Iterations, 32))
}

func encodeHash(iter int, salt, key []byte) string {
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter, enc.EncodeToString(salt), enc.EncodeToString(key))
}

func CheckPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2 实现 RFC 8018 PBKDF2，PRF 为 HMAC-SHA256。
func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	var idx [4]byte
	u := make([]byte, hLen)
	t := make([]byte, hLen)
	for b := 1; b <= blocks; b++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(idx[:], uint32(b))
		prf.Write(idx[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// NewToken 生成带前缀的随机令牌，返回明文、存储用的哈希与展示用的前缀。
func NewToken(prefix string) (plain, hash, display string) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	plain = prefix + base64.RawURLEncoding.EncodeToString(b)
	return plain, HashToken(plain), plain[:len(prefix)+6]
}

func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
