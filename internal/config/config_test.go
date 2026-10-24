package config

import (
	"io"
	"strings"
	"testing"
	"time"
)

// probeTime 是断言用的固定时刻。
var probeTime = time.Date(2026, 9, 28, 15, 4, 5, 0, time.UTC)

func parse(t *testing.T, args ...string) (*Config, error) {
	t.Helper()
	return Parse(args, io.Discard)
}

func TestUploadDateLayoutDefaultOn(t *testing.T) {
	cfg, err := parse(t, "-log-format", "none", ".")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !cfg.UploadDated() {
		t.Fatalf("上传日期归档默认应开启")
	}
	if cfg.UploadDateLayout != DefaultUploadDateLayout {
		t.Errorf("布局 = %q, 期望 %q", cfg.UploadDateLayout, DefaultUploadDateLayout)
	}
	if got := cfg.UploadDateDir(probeTime); got != "2026/09/28" {
		t.Errorf("日期目录 = %q, 期望 2026/09/28", got)
	}
}

func TestUploadDateLayoutCustom(t *testing.T) {
	cases := map[string]string{
		"2006-01-02": "2026-09-28",
		"2006/01":    "2026/09",
		"200601":     "202609",
		"2006/2006":  "2026/2026",
	}
	for layout, want := range cases {
		cfg, err := parse(t, "--upload-date-layout", layout, ".")
		if err != nil {
			t.Fatalf("布局 %q 解析失败: %v", layout, err)
		}
		if got := cfg.UploadDateDir(probeTime); got != want {
			t.Errorf("布局 %q → %q, 期望 %q", layout, got, want)
		}
	}
}

func TestUploadDateLayoutDisabled(t *testing.T) {
	// 方式一：--no-upload-dated
	cfg, err := parse(t, "--no-upload-dated", ".")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.UploadDated() {
		t.Errorf("--no-upload-dated 未关闭归档")
	}
	if got := cfg.UploadDateDir(probeTime); got != "" {
		t.Errorf("关闭后日期目录应为空串，实际 = %q", got)
	}

	// 方式二：显式置空布局
	cfg2, err := parse(t, "--upload-date-layout", "", ".")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg2.UploadDated() {
		t.Errorf("--upload-date-layout='' 未关闭归档")
	}
}

func TestUploadDateLayoutConflictsWithNoDated(t *testing.T) {
	_, err := parse(t, "--no-upload-dated", "--upload-date-layout", "2006/01/02", ".")
	if err == nil {
		t.Fatalf("两个互斥参数同时使用时应报错")
	}
	if !strings.Contains(err.Error(), "不能同时使用") {
		t.Errorf("错误信息不明确: %v", err)
	}
}

func TestUploadDateLayoutRejectsUnsafe(t *testing.T) {
	bad := []string{
		"../../etc",   // .. 穿越
		"/2006/01/02", // 绝对路径
		`2006\01\02`,  // 反斜杠
	}
	for _, layout := range bad {
		if _, err := parse(t, "--upload-date-layout", layout, "."); err == nil {
			t.Errorf("布局 %q 应被拒绝", layout)
		}
	}
}

func TestUploadDateLayoutFromEnv(t *testing.T) {
	t.Setenv("GOFS_UPLOAD_DATE_LAYOUT", "2006-01")
	cfg, err := parse(t, ".")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := cfg.UploadDateDir(probeTime); got != "2026-09" {
		t.Errorf("环境变量布局未生效，得到 %q", got)
	}
}

// TestEnvExplicitDisableNotOverriddenByUpload 钉住环境变量的显式优先级：
// GOFS_ALLOW_EDIT/KEYS=false 不能被 GOFS_ALLOW_UPLOAD=true 的推断覆盖。
func TestEnvExplicitDisableNotOverriddenByUpload(t *testing.T) {
	t.Setenv("GOFS_ALLOW_UPLOAD", "true")
	t.Setenv("GOFS_ALLOW_EDIT", "false")
	t.Setenv("GOFS_ALLOW_KEYS", "false")
	cfg, err := parse(t, "-log-format", "none", ".")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowEdit {
		t.Error("GOFS_ALLOW_EDIT=false 被上传推断覆盖了")
	}
	if cfg.AllowKeys {
		t.Error("GOFS_ALLOW_KEYS=false 被上传推断覆盖了")
	}
}

// TestSafeDefaults 钉住默认值：监听所有网卡（开箱即用）、禁止任意站点、
// assets 不得位于服务根内。
func TestSafeDefaults(t *testing.T) {
	cfg, err := parse(t, "-log-format", "none", ".")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bind != "0.0.0.0" {
		t.Errorf("默认 bind = %q，期望 0.0.0.0", cfg.Bind)
	}
	if cfg.AllowAnySite {
		t.Error("默认不应允许挂载任意站点")
	}
	if cfg.UnsafeAssetsInRoot {
		t.Error("默认不应允许 assets 位于服务根内")
	}
}

// TestAuthSprayLimitDefaults 钉住新增的「按 IP / 全局」限速默认值。
func TestAuthSprayLimitDefaults(t *testing.T) {
	cfg, err := parse(t, "-log-format", "none", ".")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.AuthFailIPLimit != 10 || cfg.AuthFailIPWindow != time.Minute {
		t.Errorf("单 IP 默认值 = %d/%s，期望 10/1m",
			cfg.AuthFailIPLimit, cfg.AuthFailIPWindow)
	}
	if cfg.AuthFailGlobalLimit != 200 || cfg.AuthFailGlobalWindow != time.Minute {
		t.Errorf("全局默认值 = %d/%s，期望 200/1m",
			cfg.AuthFailGlobalLimit, cfg.AuthFailGlobalWindow)
	}
	// 用户名维度的默认值不能被本次改动带偏。
	if cfg.AuthFailLimit != DefaultAuthFailLimit || cfg.AuthFailWindow != DefaultAuthFailWindow {
		t.Errorf("用户名维度默认值 = %d/%s，期望 %d/%s",
			cfg.AuthFailLimit, cfg.AuthFailWindow,
			DefaultAuthFailLimit, DefaultAuthFailWindow)
	}
	if cfg.AuthLockMaxDelay != DefaultAuthLockMaxDelay {
		t.Errorf("锁定封顶默认值 = %s，期望 %s", cfg.AuthLockMaxDelay, DefaultAuthLockMaxDelay)
	}
	if !contains(cfg.AuthLockExempt, "127.0.0.1/32") ||
		!contains(cfg.AuthLockExempt, "::1/128") {
		t.Errorf("默认恢复网段 = %v，期望包含回环", cfg.AuthLockExempt)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestAuthLockConfigOverrides(t *testing.T) {
	cfg, err := parse(t,
		"-log-format", "none",
		"--auth-lock-base-delay", "30s",
		"--auth-lock-max-delay", "10m",
		"--auth-lock-exempt", "10.0.0.0/8,192.168.0.0/16",
		".")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.AuthFailWindow != 30*time.Second {
		t.Errorf("基础退避 = %s，期望 30s", cfg.AuthFailWindow)
	}
	if cfg.AuthLockMaxDelay != 10*time.Minute {
		t.Errorf("封顶 = %s，期望 10m", cfg.AuthLockMaxDelay)
	}
	if !contains(cfg.AuthLockExempt, "10.0.0.0/8") ||
		!contains(cfg.AuthLockExempt, "192.168.0.0/16") {
		t.Errorf("恢复网段 = %v", cfg.AuthLockExempt)
	}
}

func TestAuthLockConfigValidation(t *testing.T) {
	if _, err := parse(t, "--auth-lock-base-delay", "0s", "."); err == nil {
		t.Error("基础退避为 0 应当被拒绝")
	}
	if _, err := parse(t, "--auth-lock-max-delay", "0s", "."); err == nil {
		t.Error("封顶为 0 应当被拒绝")
	}
	if _, err := parse(t,
		"--auth-lock-base-delay", "10m",
		"--auth-lock-max-delay", "1m", "."); err == nil {
		t.Error("封顶小于基础退避应当被拒绝")
	}
	if _, err := parse(t, "--auth-lock-exempt", "not-an-ip", "."); err == nil {
		t.Error("非法恢复网段应当被拒绝")
	}
}

func TestAuthSprayLimitOverrides(t *testing.T) {
	cfg, err := parse(t,
		"-log-format", "none",
		"--auth-fail-ip-limit", "5",
		"--auth-fail-ip-window", "30s",
		"--auth-fail-global-limit", "50",
		"--auth-fail-global-window", "2m",
		".")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.AuthFailIPLimit != 5 || cfg.AuthFailIPWindow != 30*time.Second {
		t.Errorf("单 IP 配置 = %d/%s，期望 5/30s", cfg.AuthFailIPLimit, cfg.AuthFailIPWindow)
	}
	if cfg.AuthFailGlobalLimit != 50 || cfg.AuthFailGlobalWindow != 2*time.Minute {
		t.Errorf("全局配置 = %d/%s，期望 50/2m", cfg.AuthFailGlobalLimit, cfg.AuthFailGlobalWindow)
	}
}

func TestAuthSprayLimitValidation(t *testing.T) {
	if _, err := parse(t, "--auth-fail-ip-limit", "-1", "."); err == nil {
		t.Error("负的单 IP 阈值应当被拒绝")
	}
	if _, err := parse(t, "--auth-fail-global-limit", "-1", "."); err == nil {
		t.Error("负的全局阈值应当被拒绝")
	}
	if _, err := parse(t, "--auth-fail-ip-limit", "5", "--auth-fail-ip-window", "0s", "."); err == nil {
		t.Error("阈值为正但窗口为 0 应当被拒绝")
	}
}
