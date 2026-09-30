package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/uploadkey"
)

// testHost 是测试请求里固定使用的 Host。
//
// httptest.NewRequest 对「只有路径」的 target 会把 Host 填成 example.com，
// 而脚本里的地址正是从 Host 推出来的 —— 显式给一个绝对 URL，
// 断言才好写，也才不会依赖标准库的默认值。
const testHost = "http://gofs.test"

// makeKey 建一把密钥并返回明文。
func makeKey(t *testing.T, s *Server, scope string) string {
	t.Helper()
	k, token, err := s.keys.Create(uploadkey.CreateOptions{Name: "t", Scope: scope})
	if err != nil {
		t.Fatalf("建密钥: %v", err)
	}
	if k.Scope != scope {
		t.Fatalf("密钥范围是 %q，期望 %q", k.Scope, scope)
	}
	return token
}

// fetchScript 拉一次脚本端点。
func fetchScript(t *testing.T, s *Server, target string, headers map[string]string) resp {
	t.Helper()
	return do(t, s, http.MethodGet, testHost+target, nil, headers)
}

// TestUpScriptServesRunnableScript 是这条链路的核心：
// 用上传密钥换到一段**填好了密钥和地址**的脚本。
//
// 这正是「一键」的含义 —— 用户拿到就能跑，不用改任何地方。
func TestUpScriptServesRunnableScript(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")
	token := makeKey(t, s, "/")

	got := fetchScript(t, s, upScriptName+"?key="+token, nil)
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d，期望 200：%s", got.code, got.body)
	}
	if ct := got.head.Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Errorf("Content-Type = %q，期望 shell 脚本类型", ct)
	}
	// 脚本里带着密钥，绝不能被缓存下来。
	if cc := got.head.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", cc)
	}
	// 浏览器打开链接时应当下载成文件而不是显示一坨文本。
	if cd := got.head.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q，期望带着 attachment", cd)
	}

	// 占位符必须全部替换掉，否则脚本是坏的。
	for _, ph := range []string{"__KEY__", "__BASE__", "__SCOPE__"} {
		if strings.Contains(got.body, ph) {
			t.Errorf("占位符 %s 没被替换", ph)
		}
	}
	if !strings.Contains(got.body, "KEY='"+token+"'") {
		t.Errorf("脚本里没有填好的密钥")
	}
	if !strings.Contains(got.body, "#!/bin/bash") {
		t.Errorf("脚本缺少 shebang")
	}
}

// TestUpScriptBaseFollowsScope 确认脚本里的地址落在密钥范围内。
//
// 写错的话症状是「脚本拿到了，但每个文件都 403」——
// 因为密钥只能写进自己的范围。
func TestUpScriptBaseFollowsScope(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")
	token := makeKey(t, s, "/docs")

	got := fetchScript(t, s, upScriptName+"?key="+token, nil)
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d", got.code)
	}
	if !strings.Contains(got.body, "BASE='http://gofs.test/docs/'") {
		t.Errorf("脚本里的地址没有带密钥范围：\n%s", firstLines(got.body, 12))
	}
}

// TestUpScriptRejectsBadKey 确认没有有效密钥就不下发脚本。
func TestUpScriptRejectsBadKey(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")

	// 注意：「完全没带 key」不在这个列表里 —— 那种情况会回退成普通文件请求，
	// 由 TestUpScriptFallsThroughWithoutKey 覆盖。
	cases := []struct {
		name   string
		target string
	}{
		{"密钥是空串", upScriptName + "?key="},
		{"密钥是编的", upScriptName + "?key=gofs_notarealkey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fetchScript(t, s, tc.target, nil)
			if got.code != http.StatusUnauthorized {
				t.Fatalf("返回 %d，期望 401", got.code)
			}
			// 拒绝时不能把脚本模板漏出去。
			if strings.Contains(got.body, "X-Gofs-Upload-Key") {
				t.Error("被拒绝的响应里带着脚本内容")
			}
		})
	}
}

// TestUpScriptFallsThroughWithoutKey 是 /up 这个短路径带来的风险：
// 它可能和用户根目录下真实的 up 文件或 up/ 目录撞名。
//
// 处理方式是「没带 key 就交回正常流程」——
// 这样根目录里的 up 照常能访问，只有显式 ?key= 才是取脚本。
func TestUpScriptFallsThroughWithoutKey(t *testing.T) {
	// 这里刻意不开鉴权：要观察的是「回退成普通文件请求」，
	// 开着鉴权的话裸 /up 会（正确地）401，看不出回退有没有生效。

	t.Run("根目录有同名文件时照常下载", func(t *testing.T) {
		root := t.TempDir()
		content := "我是一个叫 up 的真实文件，不是脚本端点"
		if err := os.WriteFile(filepath.Join(root, "up"), []byte(content), 0o644); err != nil {
			t.Fatalf("准备文件: %v", err)
		}
		s := newTestServer(t, testConfig(t, root, "-A", "--allow-upload"))

		got := fetchScript(t, s, upScriptName, nil)
		if got.code != http.StatusOK {
			t.Fatalf("返回 %d，期望 200（应当当成普通文件）", got.code)
		}
		if got.body != content {
			t.Fatalf("拿到的不是文件内容，端点把同名文件抢走了：\n%.80s", got.body)
		}
		if strings.Contains(got.body, "#!/bin/bash") {
			t.Fatal("返回的是脚本，不是同名文件")
		}
	})

	t.Run("同名目录照常列目录", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "up"), 0o755); err != nil {
			t.Fatalf("建目录: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "up", "inside.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("准备文件: %v", err)
		}
		s := newTestServer(t, testConfig(t, root, "-A", "--allow-upload"))

		// 裸 /up：交回普通流程。目录会被当成目录处理（列目录或 301），
		// 关键是**不能**返回脚本。
		got := do(t, s, http.MethodGet, testHost+upScriptName, nil, nil)
		if strings.Contains(got.body, "#!/bin/bash") {
			t.Fatalf("同名目录被端点抢走了，返回的是脚本：%.120s", got.body)
		}
		if got.code != http.StatusOK && got.code != http.StatusMovedPermanently {
			t.Fatalf("同名目录返回 %d，期望列目录或重定向", got.code)
		}
		// 跟进去看目录列表里确实是那个目录的内容。
		got = do(t, s, http.MethodGet, testHost+upScriptName+"/?json", nil,
			map[string]string{"Accept": "application/json"})
		if got.code != http.StatusOK {
			t.Fatalf("/up/ 返回 %d，期望 200：%s", got.code, got.body)
		}
		if !strings.Contains(got.body, "inside.txt") {
			t.Fatalf("/up/ 不是那个同名目录：%.200s", got.body)
		}
	})

	t.Run("带上 key 时端点优先于同名文件", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "up"), []byte("同名文件"), 0o644); err != nil {
			t.Fatalf("准备文件: %v", err)
		}
		s := newTestServer(t, testConfig(t, root, "-A", "--allow-upload"))

		// 反过来说：带上 key 时端点一定生效，不被同名文件挡住。
		token := makeKey(t, s, "/")
		got := fetchScript(t, s, upScriptName+"?key="+token, nil)
		if got.code != http.StatusOK || !strings.Contains(got.body, "#!/bin/bash") {
			t.Fatalf("带 key 时没拿到脚本：%d\n%.120s", got.code, got.body)
		}
	})
}

// TestUpScriptRejectsRevokedKey 确认撤销之后脚本也拿不到了。
func TestUpScriptRejectsRevokedKey(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")
	k, token, err := s.keys.Create(uploadkey.CreateOptions{Name: "t", Scope: "/"})
	if err != nil {
		t.Fatalf("建密钥: %v", err)
	}
	if got := fetchScript(t, s, upScriptName+"?key="+token, nil); got.code != http.StatusOK {
		t.Fatalf("撤销前返回 %d，期望 200", got.code)
	}
	if err := s.keys.Revoke(k.ID); err != nil {
		t.Fatalf("撤销: %v", err)
	}
	if got := fetchScript(t, s, upScriptName+"?key="+token, nil); got.code != http.StatusUnauthorized {
		t.Fatalf("撤销后返回 %d，期望 401", got.code)
	}
}

// TestUpScriptRequiresUploadEnabled 确认没开上传时不下发脚本 ——
// 否则用户拿到脚本、跑了一路，最后全被 403 挡回来。
func TestUpScriptRequiresUploadEnabled(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root) // 注意：没给 --allow-upload
	token := makeKey(t, s, "/")

	got := fetchScript(t, s, upScriptName+"?key="+token, nil)
	if got.code != http.StatusForbidden {
		t.Fatalf("返回 %d，期望 403", got.code)
	}
}

// TestUpScriptHonoursForwardedProto 确认反向代理下地址是 https。
//
// 代理把 TLS 终止在上游时 r.TLS 为空，只看它会写出 http:// 的地址 ——
// 用户拿到的脚本会连不上（或者被中间层重定向掉）。这是很常见的部署形态。
func TestUpScriptHonoursForwardedProto(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")
	token := makeKey(t, s, "/")

	got := fetchScript(t, s, upScriptName+"?key="+token,
		map[string]string{"X-Forwarded-Proto": "https"})
	if !strings.Contains(got.body, "BASE='https://") {
		t.Errorf("没有采信 X-Forwarded-Proto：\n%s", firstLines(got.body, 12))
	}

	// 多层代理时可能是 "https,http"，应当取第一段。
	got = fetchScript(t, s, upScriptName+"?key="+token,
		map[string]string{"X-Forwarded-Proto": "https, http"})
	if !strings.Contains(got.body, "BASE='https://") {
		t.Errorf("多段 X-Forwarded-Proto 解析错误：\n%s", firstLines(got.body, 12))
	}

	// 胡编的值不能相信 —— 否则等于让客户端决定脚本里的协议。
	got = fetchScript(t, s, upScriptName+"?key="+token,
		map[string]string{"X-Forwarded-Proto": "gopher"})
	if !strings.Contains(got.body, "BASE='http://") {
		t.Errorf("非法协议值被采信了：\n%s", firstLines(got.body, 12))
	}
}

// TestUpScriptIncludesPathPrefix 确认前缀部署下地址完整。
func TestUpScriptIncludesPathPrefix(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload", "--path-prefix", "/files")
	token := makeKey(t, s, "/")

	got := fetchScript(t, s, "/files"+upScriptName+"?key="+token, nil)
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "BASE='http://gofs.test/files/'") {
		t.Errorf("地址没带访问前缀：\n%s", firstLines(got.body, 12))
	}
}

// TestUpScriptEscapesHostHeader 确认 Host 头里的引号不会变成命令注入。
//
// Host 是客户端可控的，而它的值会被写进脚本的单引号串里。
// 不转义的话，一个恶意的 Host 就能让拿到脚本的人执行任意命令。
func TestUpScriptEscapesHostHeader(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")
	token := makeKey(t, s, "/")

	// ⚠️ 必须设 r.Host，不能设 Header["Host"]：
	// Go 把 Host 单独存在 Request.Host 字段里，读的是它。
	// 早先这里用 Header.Set("Host", …) 根本改不动 Host，
	// 测试于是「通过」了却什么都没测到。
	evil := `evil'; touch /tmp/pwned; echo '`
	r := httptest.NewRequest(http.MethodGet, testHost+upScriptName+"?key="+token, nil)
	r.Host = evil
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("返回 %d", res.StatusCode)
	}

	// 先确认这条测试真的把恶意 Host 送进去了 —— 否则又是在测空气。
	if !strings.Contains(string(body), "evil") {
		t.Fatalf("恶意 Host 没进到脚本里，这条测试没测到东西：\n%s", firstLines(string(body), 14))
	}
	// 单引号必须被收成 '\''，否则脚本里的引号串会提前闭合，
	// touch /tmp/pwned 就成了真正的命令。
	if !strings.Contains(string(body), `'\''`) {
		t.Fatalf("Host 里的单引号没有被转义，脚本被注入了：\n%s", firstLines(string(body), 14))
	}
	if strings.Contains(string(body), "BASE='http://evil';") {
		t.Fatalf("引号串被提前闭合：\n%s", firstLines(string(body), 14))
	}
}

// TestUpScriptMethodNotAllowed 确认只接受 GET/HEAD。
func TestUpScriptMethodNotAllowed(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")
	token := makeKey(t, s, "/")

	got := do(t, s, http.MethodPost, testHost+upScriptName+"?key="+token, nil, nil)
	if got.code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 返回 %d，期望 405", got.code)
	}
}

// TestShellSingleQuote 是上面那条注入测试的单元版。
func TestShellSingleQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a'b", `a'\''b`},
		{"'", `'\''`},
		{"", ""},
	}
	for _, tc := range cases {
		if got := shellSingleQuote(tc.in); got != tc.want {
			t.Errorf("shellSingleQuote(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// firstLines 取前 n 行，用于失败时打印上下文。
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestUpScriptServedThroughRealServer 走一遍真实 HTTP 服务，
// 确认路由注册、前缀处理与中间件一起工作时也对。
func TestUpScriptServedThroughRealServer(t *testing.T) {
	root := t.TempDir()
	s := newSessionServer(t, root, "--allow-upload")
	token := makeKey(t, s, "/")

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	res, err := http.Get(ts.URL + upScriptName + "?key=" + token)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("返回 %d，期望 200", res.StatusCode)
	}
}
