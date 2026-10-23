package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/uploadkey"
)

// TestKeyCreateRejectsControlChars 确认含换行的 scope 在创建入口就被拒绝。
func TestKeyCreateRejectsControlChars(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "alice:pw@/docs:rw"))

	payload, _ := json.Marshal(map[string]any{
		"name": "x", "scope": "/docs/\ntouch /tmp/pwn #",
	})
	got := do(t, s, http.MethodPost, "/fs/keys", bytes.NewReader(payload),
		map[string]string{
			"Content-Type":  "application/json",
			"Authorization": basicAuth("alice", "pw"),
		})
	if got.code != http.StatusBadRequest {
		t.Fatalf("含换行 scope 返回 %d，期望 400：%s", got.code, got.body)
	}
}

// TestUpScriptSanitizesLegacyScope 兜住历史 keys.json 里已存在的换行 scope。
//
// 新数据在 Store.Create 就被拒绝；这里直接改内存中的 Key 模拟旧数据，
// 确认渲染 /up 时不会把 scope 中的换行带进注释、变成可执行命令。
func TestUpScriptSanitizesLegacyScope(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "alice:pw@/docs:rw"))

	k, token, err := s.keys.Create(uploadkey.CreateOptions{
		Name: "legacy", Scope: "/docs", Creator: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧版本落盘的恶意 scope。
	k.Scope = "/docs/\ntouch /tmp/pwn #"

	got := do(t, s, http.MethodGet, "/up?key="+token, nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("取脚本返回 %d：%s", got.code, got.body)
	}
	// 注释必须保持在同一行：换行被替换成空格，后面的 # 也仍在注释里。
	if !strings.Contains(got.body, "# 不带目标子目录时直接传到 /docs/ touch /tmp/pwn #。") {
		t.Fatalf("scope 没有被替换成注释内的安全文本：\n%s", got.body)
	}
	// BASE 里的换行位于单引号串内，不是命令；上面的断言已经确认注释
	// 没有被拆成独立命令行。
}

// TestKeyCreateIgnoresShareToken 确认分享令牌不能授权签发新密钥。
//
// 低权限账号 u 只在 /write 可写、对 /secret 只读。持有一条 /secret 的
// 可读写分享令牌时，旧实现会让 checkPerm 看到分享的 rw，从而签出永久
// 密钥；现在范围授权只按账号凭据计算，必须 403。
func TestKeyCreateIgnoresShareToken(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "secret", "s.txt"), []byte("s"))
	writeFile(t, filepath.Join(root, "write", "w.txt"), []byte("w"))
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "u:pw@/write:rw,/secret:r",
		"-a", "boss:pw@/:rw"))

	// 模拟管理员签发的、当前仍然有效的可读写分享。
	_, shareToken, err := s.keys.Create(uploadkey.CreateOptions{
		Name: "share", Scope: "/secret", Kind: uploadkey.KindReadWrite, Creator: "boss",
	})
	if err != nil {
		t.Fatal(err)
	}

	post := func(scope, query string) resp {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"name": "x", "scope": scope})
		return do(t, s, http.MethodPost, "/fs/keys"+query, bytes.NewReader(body),
			map[string]string{
				"Content-Type":  "application/json",
				"Authorization": basicAuth("u", "pw"),
			})
	}

	if got := post("/secret", "?share="+shareToken); got.code != http.StatusForbidden {
		t.Fatalf("带分享令牌为只读范围签密钥返回 %d，期望 403：%s", got.code, got.body)
	}
	// 分享令牌存在时，账号本来有权限的范围仍应正常创建。
	if got := post("/write", "?share="+shareToken); got.code != http.StatusCreated {
		t.Fatalf("账号可写范围返回 %d，期望 201：%s", got.code, got.body)
	}
}

// TestZipErrorDoesNotLeakAbsolutePath 确认打包不可读文件时不会把主机绝对路径写进 zip。
func TestZipErrorDoesNotLeakAbsolutePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不适用 0000 权限位")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 可以读取 0000 文件")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "locked.txt"), []byte("secret"))
	if err := os.Chmod(filepath.Join(root, "docs", "locked.txt"), 0); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, root, "-A"))

	got := do(t, s, http.MethodGet, "/docs?zip", nil, nil)
	if got.code != http.StatusOK {
		t.Fatalf("打包返回 %d：%s", got.code, got.body)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(got.body)), int64(len(got.body)))
	if err != nil {
		t.Fatalf("解析 zip: %v", err)
	}
	var body string
	for _, f := range zr.File {
		if f.Name != "locked.txt" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		body = string(b)
	}
	if body == "" {
		t.Fatal("zip 中没有 locked.txt 条目")
	}
	if strings.Contains(body, root) {
		t.Fatalf("zip 内容泄露了绝对路径：%q", body)
	}
	if !strings.Contains(body, "无法读取该文件") {
		t.Fatalf("缺少说明文本：%q", body)
	}
}
