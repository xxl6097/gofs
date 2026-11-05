package webdav

import (
	"testing"
)

func TestHashPassword(t *testing.T) {
	password := "test123"
	hash := hashPassword(password)

	if hash == "" {
		t.Fatal("哈希不应为空")
	}

	// 验证哈希格式
	if len(hash) < 50 {
		t.Errorf("哈希长度不正确: %d", len(hash))
	}
}

func TestVerifyPassword(t *testing.T) {
	password := "test123"
	hash := hashPassword(password)

	tests := []struct {
		name     string
		password string
		want     bool
	}{
		{
			name:     "正确密码",
			password: "test123",
			want:     true,
		},
		{
			name:     "错误密码",
			password: "wrong",
			want:     false,
		},
		{
			name:     "空密码",
			password: "",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verifyPassword(tt.password, hash)
			if got != tt.want {
				t.Errorf("verifyPassword() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVerifyPasswordInvalidFormat(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{
			name: "空哈希",
			hash: "",
		},
		{
			name: "无效格式",
			hash: "invalid",
		},
		{
			name: "错误的算法",
			hash: "bcrypt$123$abc$def",
		},
		{
			name: "缺少部分",
			hash: "pbkdf2$123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if verifyPassword("test", tt.hash) {
				t.Error("应该验证失败")
			}
		})
	}
}
