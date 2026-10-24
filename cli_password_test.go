package gofs

import (
	"strings"
	"testing"
)

// TestRandomAdminPassword 确认便捷入口不再使用固定口令，且生成结果
// 可以安全地放进 user:pass@path 规则（不含 @ : ,）。
func TestRandomAdminPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		p, err := randomAdminPassword()
		if err != nil {
			t.Fatal(err)
		}
		if p == "" || len(p) < 16 {
			t.Fatalf("口令过短：%q", p)
		}
		if strings.ContainsAny(p, "@:,\n\r") {
			t.Fatalf("口令包含会破坏 -a 规则的字符：%q", p)
		}
		if p == "admin123" {
			t.Fatalf("仍在生成默认口令")
		}
		if seen[p] {
			t.Fatalf("随机口令重复：%q", p)
		}
		seen[p] = true
	}
}

// TestRandomAdminName 确认便捷入口的账号名也不再是固定的 admin：
// 常见默认名会让定向猜口令/锁定的成本低得多。生成结果还必须能安全地
// 放进 user:pass@path 规则（不含 @ : ,）。
func TestRandomAdminName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		name, err := randomAdminName()
		if err != nil {
			t.Fatal(err)
		}
		if name == "" || name == "admin" {
			t.Fatalf("仍在生成默认账号名：%q", name)
		}
		if strings.ContainsAny(name, "@:,\n\r") {
			t.Fatalf("账号名包含会破坏 -a 规则的字符：%q", name)
		}
		if seen[name] {
			t.Fatalf("随机账号名重复：%q", name)
		}
		seen[name] = true
	}
}
