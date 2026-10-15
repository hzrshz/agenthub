package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// NewID 生成带前缀的随机 ID（16 字节 hex，与 nexusdock 同构）。
func NewID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	if prefix == "" {
		return hex.EncodeToString(raw), nil
	}
	return prefix + "_" + hex.EncodeToString(raw), nil
}

// NewSecret 生成 URL 安全的高熵随机秘密（token/配对码本体）。
// 只返回明文一次；落库前由调用方自行哈希。
func NewSecret(nBytes int) (string, error) {
	raw := make([]byte, nBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashToken 计算 token/配对码/会话的存储哈希。SHA-256 足够：
// 被哈希的对象都是高熵随机串，不需要口令慢哈希。
// 配对码除外——它是人类可读短串，口令属性更强，由 auth 层用 bcrypt 处理。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
