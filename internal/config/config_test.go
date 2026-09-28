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
