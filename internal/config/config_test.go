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
