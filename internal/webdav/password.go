package webdav

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const (
	// PBKDF2 参数
	pbkdf2Iterations = 210000
	pbkdf2KeyLen     = 32
	saltLen          = 16
)

// timeNow 返回当前时间，方便测试时替换。
var timeNow = time.Now

// hashPassword 使用 PBKDF2 哈希密码。
// 返回格式: pbkdf2$iterations$salt$hash
func hashPassword(password string) string {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(fmt.Sprintf("生成盐失败: %v", err))
	}

	hash, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		panic(fmt.Sprintf("PBKDF2 失败: %v", err))
	}

	return fmt.Sprintf("pbkdf2$%d$%s$%s",
		pbkdf2Iterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(hash),
	)
}

// verifyPassword 验证密码是否匹配哈希值。
func verifyPassword(password, hashedPassword string) bool {
	parts := strings.Split(hashedPassword, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}

	// 解析迭代次数
	var iterations int
	if _, err := fmt.Sscanf(parts[1], "%d", &iterations); err != nil {
		return false
	}

	// 解析盐
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}

	// 解析存储的哈希
	storedHash, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}

	// 计算输入密码的哈希
	computedHash, err := pbkdf2.Key(sha256.New, password, salt, iterations, pbkdf2KeyLen)
	if err != nil {
		return false
	}

	// 使用常量时间比较防止时序攻击
	return subtle.ConstantTimeCompare(computedHash, storedHash) == 1
}
