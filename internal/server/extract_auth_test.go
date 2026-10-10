package server

import (
	"archive/zip"
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeZip 在 root 下造一个压缩包，内含一个 secret.txt。
func makeZip(t *testing.T, root, zipRel, secret string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(zipRel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(secret)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExtractListRequiresAuth 钉住一个曾经真实存在的鉴权绕过。
//
// /__gofs__/extract 是独立路由，不经过 handleRoot 的授权。
// 它既能列出包内清单、又能用 &file= 取出包内单个文件 ——
// 而它原先**一次权限校验都没有**，只受全局的 --allow-extract 约束。
//
// 后果：只要开了在线解压（-A 就会开），任何能连上服务的人都能读到
// 服务目录里任意压缩包的内容，绕过全部路径级权限。
// 实测过一次：未登录请求 /secret.zip 是 401，但同一个包走 extract 却 200，
// 文件内容原样返回。
func TestExtractListRequiresAuth(t *testing.T) {
	root := t.TempDir()
	const secret = "绝密：工资表"
	makeZip(t, root, "private/secret.zip", secret)

	// 只有登录用户能读整个根目录。
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))

	t.Run("未登录列清单被拒", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/extract?path=/private/secret.zip", nil, nil)
		if got.code == http.StatusOK {
			t.Fatalf("未登录竟然列出了包内清单：%s", got.body)
		}
		if got.code != http.StatusUnauthorized {
			t.Fatalf("返回 %d，期望 401", got.code)
		}
	})

	t.Run("未登录取包内文件被拒", func(t *testing.T) {
		got := do(t, s, http.MethodGet,
			"/__gofs__/extract?path=/private/secret.zip&file=secret.txt", nil, nil)
		if strings.Contains(got.body, secret) {
			t.Fatalf("未登录竟然读到了包内内容：%s", got.body)
		}
		if got.code != http.StatusUnauthorized {
			t.Fatalf("返回 %d，期望 401", got.code)
		}
	})

	auth := map[string]string{"Authorization": basicAuth("boss", "pw")}

	t.Run("登录后可以正常用", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/__gofs__/extract?path=/private/secret.zip", nil, auth)
		if got.code != http.StatusOK {
			t.Fatalf("带凭据列清单返回 %d，期望 200：%s", got.code, got.body)
		}

		got = do(t, s, http.MethodGet,
			"/__gofs__/extract?path=/private/secret.zip&file=secret.txt", nil, auth)
		if got.code != http.StatusOK {
			t.Fatalf("带凭据取包内文件返回 %d，期望 200：%s", got.code, got.body)
		}
		if !strings.Contains(got.body, secret) {
			t.Fatalf("内容不对：%q", got.body)
		}
	})
}

// TestExtractListChecksReadPermNotJustAnyPerm 确认校验的是**对那个路径**的读权限，
// 而不是「有没有凭据」—— 有凭据但对该路径无权限，同样要拒。
func TestExtractListChecksReadPermNotJustAnyPerm(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeZip(t, root, "private/deep/secret.zip", "深层机密")

	s := newTestServer(t, testConfig(t, root, "-A", "-a", "guest:g@/docs:r"))

	got := do(t, s, http.MethodGet,
		"/__gofs__/extract?path=/private/deep/secret.zip", nil, nil)
	if got.code == http.StatusOK {
		t.Fatalf("越权读到了：%s", got.body)
	}
	if !strings.Contains(got.body, "secret") && got.code != http.StatusUnauthorized &&
		got.code != http.StatusForbidden {
		t.Fatalf("返回 %d，期望 401 或 403：%s", got.code, got.body)
	}
}
