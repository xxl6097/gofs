package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSized 造一个指定大小的文本文件。
func writeSized(t *testing.T, root, rel string, n int) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// getSettings 读一次服务设置。
func getSettings(t *testing.T, s *Server, hdr map[string]string) map[string]any {
	t.Helper()
	got := do(t, s, http.MethodGet, "/__gofs__/settings", nil, hdr)
	if got.code != http.StatusOK {
		t.Fatalf("读设置返回 %d：%s", got.code, got.body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got.body), &m); err != nil {
		t.Fatalf("解析设置失败：%v\n%s", err, got.body)
	}
	return m
}

// TestEditMaxSizeIsReported 确认设置里带上了编辑上限（含启动默认值与当前值）。
func TestEditMaxSizeIsReported(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A", "--edit-max-size", "1048576"))
	auth := map[string]string{"Authorization": basicAuth("x", "y")}

	got := do(t, s, http.MethodGet, "/__gofs__/settings", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d", got.code)
	}
	m := getSettings(t, s, auth)

	if _, ok := m["edit_max_size"]; !ok {
		t.Fatalf("设置里没有 edit_max_size：%v", m)
	}
	if v, _ := m["edit_max_size"].(float64); int64(v) != 1<<20 {
		t.Errorf("edit_max_size = %v，期望 %d", m["edit_max_size"], 1<<20)
	}
	if _, ok := m["edit_max_size_default"]; !ok {
		t.Error("设置里没有 edit_max_size_default（界面要用它做「恢复默认」）")
	}
}

// TestEditMaxSizeRuntimeChange 是这次的核心：改完之后**立刻**生效。
//
// 不只是「设置里存下来了」—— 真正要保证的是文本读写接口按新值办事。
// 只改配置不生效的话，界面上按钮没了，接口却还放行，等于白做。
func TestEditMaxSizeRuntimeChange(t *testing.T) {
	root := t.TempDir()
	writeSized(t, root, "small.txt", 100)
	writeSized(t, root, "big.txt", 5000)

	// 启动时给一个很大的上限：两个文件都可编辑。
	s := newTestServer(t, testConfig(t, root, "-A", "--allow-edit", "--edit-max-size", "10485760"))
	auth := map[string]string{"Authorization": basicAuth("x", "y")}

	readOK := func(name string) bool {
		got := do(t, s, http.MethodGet, "/__gofs__/text?path=/"+name, nil, auth)
		return got.code == http.StatusOK
	}
	if !readOK("big.txt") {
		t.Fatalf("前置条件不成立：上限 10MiB 时 big.txt 应当可读")
	}

	// 调到 1 KiB —— big.txt（5000 字节）就该被拒了。
	patch, _ := json.Marshal(map[string]any{"edit_max_size": 1024})
	got := do(t, s, http.MethodPut, "/__gofs__/settings", bytes.NewReader(patch),
		map[string]string{
			"Authorization": basicAuth("x", "y"),
			"Content-Type":  "application/json",
		})
	if got.code != http.StatusOK {
		t.Fatalf("改设置返回 %d：%s", got.code, got.body)
	}

	if !readOK("small.txt") {
		t.Error("小于新上限的文件应当仍可读")
	}
	if readOK("big.txt") {
		t.Fatal("改小上限之后，超限文件仍然可读 —— 设置没生效")
	}

	// 写也要拒：不能只挡读，改文件同样是「编辑」。
	put := do(t, s, http.MethodPut, "/__gofs__/text?path=/big.txt",
		strings.NewReader(`{"content":"x","hash":""}`),
		map[string]string{"Authorization": basicAuth("x", "y")})
	if put.code == http.StatusOK {
		t.Fatal("超限文件仍然可写 —— 只挡了读没挡写")
	}
	// 原文件内容不能被改动。
	b, err := os.ReadFile(filepath.Join(root, "big.txt"))
	if err != nil || len(b) != 5000 {
		t.Fatalf("超限写被挡住之后源文件被改了：len=%d err=%v", len(b), err)
	}
}

// TestEditMaxSizeRejectsNegative 确认负数被拒。
func TestEditMaxSizeRejectsNegative(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A"))

	body, _ := json.Marshal(map[string]any{"edit_max_size": -1})
	got := do(t, s, http.MethodPut, "/__gofs__/settings", bytes.NewReader(body),
		map[string]string{
			"Authorization": basicAuth("x", "y"),
			"Content-Type":  "application/json",
		})
	if got.code != http.StatusBadRequest {
		t.Fatalf("返回 %d，期望 400：%s", got.code, got.body)
	}
}

// TestEditMaxSizeZeroRejected 确认 0 被拒，而不是被当成「不限制」。
//
// 这一条是测试逼出来的：我一开始按「0 = 不限制」写（和 upload_max_size 一致），
// 但 handler_text 的判断是 `size > 上限 就拒`，上限为 0 意味着**每个**
// 非空文件都超限（全是 413）。而启动参数 --edit-max-size 本来也是
// 「必须为正整数」。两边语义必须一致，所以 0 直接拒掉。
func TestEditMaxSizeZeroRejected(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A"))

	body, _ := json.Marshal(map[string]any{"edit_max_size": 0})
	got := do(t, s, http.MethodPut, "/__gofs__/settings", bytes.NewReader(body),
		map[string]string{
			"Authorization": basicAuth("x", "y"),
			"Content-Type":  "application/json",
		})
	if got.code != http.StatusBadRequest {
		t.Fatalf("返回 %d，期望 400：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "正整数") {
		t.Errorf("错误信息没说清原因：%s", got.body)
	}
}

// TestSettingsRejectsEmptyPatch 确认三项都没给时报错（而不是静默成功）。
func TestSettingsRejectsEmptyPatch(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := do(t, s, http.MethodPut, "/__gofs__/settings", bytes.NewReader([]byte("{}")),
		map[string]string{
			"Authorization": basicAuth("x", "y"),
			"Content-Type":  "application/json",
		})
	if got.code != http.StatusBadRequest {
		t.Fatalf("返回 %d，期望 400：%s", got.code, got.body)
	}
}
