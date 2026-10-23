package server

import (
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestZipRespectsHidden 确认打包与目录列举/搜索使用同一套隐藏规则。
func TestZipRespectsHidden(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".secret"), []byte("TOPSECRET"))
	writeFile(t, filepath.Join(root, ".git", "config"), []byte("[core]"))
	writeFile(t, filepath.Join(root, "visible.txt"), []byte("ok"))

	s := newTestServer(t, testConfig(t, root, "-A", "--hidden", ".*"))
	got := do(t, s, http.MethodGet, "/?zip", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("打包返回 %d：%s", got.code, got.body)
	}

	names := zipEntries(t, got.body)
	joined := strings.Join(names, "\n")
	if strings.Contains(joined, ".secret") || strings.Contains(joined, ".git") {
		t.Fatalf("隐藏条目被打进了 zip：%v", names)
	}
	if !strings.Contains(joined, "visible.txt") {
		t.Fatalf("普通条目缺失：%v", names)
	}
}

// TestZipEntryNamesAreWindowsSafe 确认服务端文件名里的反斜杠/冒号不会原样
// 进入 zip：Windows 解压器可能把 `..\x` 当成穿越路径、把 `C:` 当成盘符。
func TestZipEntryNamesAreWindowsSafe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无法创建含 \\ 或 : 的文件名")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, `..\evil.txt`), []byte("x"))
	writeFile(t, filepath.Join(root, `C:evil.txt`), []byte("x"))

	s := newTestServer(t, testConfig(t, root, "-A"))
	got := do(t, s, http.MethodGet, "/?zip", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("打包返回 %d：%s", got.code, got.body)
	}

	names := zipEntries(t, got.body)
	if len(names) != 2 {
		t.Fatalf("zip 条目数 = %d，期望 2：%v", len(names), names)
	}
	for _, name := range names {
		if strings.ContainsAny(name, `\:`) {
			t.Errorf("条目名仍含 Windows 危险字符：%q", name)
		}
		for _, seg := range strings.Split(name, "/") {
			if seg == ".." {
				t.Errorf("条目名含 .. 段：%q", name)
			}
		}
	}
}
