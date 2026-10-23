package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/uploadkey"
)

// newShare 建一条指向 scope 的分享链接，返回明文令牌。
func newShare(t *testing.T, s *Server, scope string) string {
	t.Helper()
	k, token, err := s.keys.Create(uploadkey.CreateOptions{
		Name: scope, Scope: scope, Kind: uploadkey.KindRead, Creator: "boss",
	})
	if err != nil {
		t.Fatalf("建分享链接: %v", err)
	}
	if k.KindOf() != uploadkey.KindRead {
		t.Fatalf("类型 = %q，期望 read", k.KindOf())
	}
	return token
}

// shareServer 造一个「开了鉴权、内容默认要登录」的实例。
func shareServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"docs/readme.txt", "docs/sub/deep.txt", "other/secret.txt"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("内容:"+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 只有 boss 能读整个根目录 —— 所以「没登录也能读」只可能来自分享链接。
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw"))
	return s, root
}

// TestShareLinkReadsWithoutLogin 是这次的核心：
// 拿到分享链接的人**不需要登录**就能读范围内的内容。
func TestShareLinkReadsWithoutLogin(t *testing.T) {
	s, _ := shareServer(t)
	tok := newShare(t, s, "/docs")

	t.Run("文件内容", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/docs/readme.txt?share="+tok, nil, nil)
		if got.code != http.StatusOK {
			t.Fatalf("返回 %d，期望 200：%s", got.code, got.body)
		}
		if !strings.Contains(got.body, "内容:docs/readme.txt") {
			t.Fatalf("内容不对：%q", got.body)
		}
	})

	t.Run("目录列表", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/docs/?json&share="+tok, nil, nil)
		if got.code != http.StatusOK {
			t.Fatalf("返回 %d，期望 200：%s", got.code, got.body)
		}
		if !strings.Contains(got.body, "readme.txt") {
			t.Fatalf("列表里没有内容：%s", got.body)
		}
	})

	t.Run("子目录也在范围内", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/docs/sub/deep.txt?share="+tok, nil, nil)
		if got.code != http.StatusOK {
			t.Fatalf("返回 %d，期望 200：%s", got.code, got.body)
		}
	})

	t.Run("目录页面外壳", func(t *testing.T) {
		// 分享目录时打开的是页面，必须把这个页面本身也给出去，
		// 否则用户拿到的是登录框。
		got := do(t, s, http.MethodGet, "/docs/?share="+tok, nil,
			map[string]string{"Accept": "text/html"})
		if got.code != http.StatusOK {
			t.Fatalf("返回 %d，期望 200", got.code)
		}
		if !strings.Contains(got.body, "<html") {
			t.Fatalf("不是页面外壳：%.120s", got.body)
		}
	})
}

// TestShareLinkOutOfScope 确认分享链接**只**能读它自己范围内的东西。
//
// 这是整件事的安全底线：链接公开的是那一个路径，不是整个服务。
func TestShareLinkOutOfScope(t *testing.T) {
	s, _ := shareServer(t)
	tok := newShare(t, s, "/docs")

	for _, p := range []string{
		"/other/secret.txt?share=",
		"/other/?json&share=",
		"/docs/../other/secret.txt?share=", // 归一化之后同样越界
	} {
		got := do(t, s, http.MethodGet, p+tok, nil, nil)
		if got.code == http.StatusOK {
			t.Fatalf("%s 被放行了：%s", p, got.body)
		}
		if strings.Contains(got.body, "内容:other/secret.txt") {
			t.Fatalf("%s 泄漏了范围外的内容：%s", p, got.body)
		}
	}
}

// TestShareLinkIsReadOnly 确认分享链接**不能写**。
//
// 它换来的只是「不用登录」，不是「什么都能干」。
func TestShareLinkIsReadOnly(t *testing.T) {
	s, root := shareServer(t)
	tok := newShare(t, s, "/docs")

	put := do(t, s, http.MethodPut, "/docs/new.txt?share="+tok,
		strings.NewReader("x"), nil)
	if put.code == http.StatusOK || put.code == http.StatusCreated {
		t.Fatalf("分享链接竟然能写：%d", put.code)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "new.txt")); err == nil {
		t.Fatal("文件被写进去了")
	}

	if del := do(t, s, http.MethodDelete, "/docs/readme.txt?share="+tok, nil, nil); del.code < 400 {
		t.Fatalf("分享链接竟然能删：%d", del.code)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "readme.txt")); err != nil {
		t.Fatal("文件被删了")
	}
}

// TestUploadKeyIsNotAShareToken 确认两种凭证不能互相冒充。
//
// 上传密钥要是能当分享链接用，就等于把「只能传」的凭证升级成了「能读」——
// 那是权限提升。
func TestUploadKeyIsNotAShareToken(t *testing.T) {
	s, _ := shareServer(t)

	up, tok, err := s.keys.Create(uploadkey.CreateOptions{Name: "up", Scope: "/", Kind: uploadkey.KindUpload})
	if err != nil {
		t.Fatal(err)
	}
	if up.KindOf() != uploadkey.KindUpload {
		t.Fatalf("类型 = %q，期望 upload", up.KindOf())
	}

	got := do(t, s, http.MethodGet, "/docs/readme.txt?share="+tok, nil, nil)
	if got.code == http.StatusOK {
		t.Fatalf("上传密钥被当成分享链接放行了：%s", got.body)
	}
	if strings.Contains(got.body, "内容:") {
		t.Fatal("内容泄漏")
	}
}

// TestShareLinkInvalidOrRevoked 确认无效与已撤销的令牌都进不来。
func TestShareLinkInvalidOrRevoked(t *testing.T) {
	s, _ := shareServer(t)

	t.Run("伪造的", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/docs/readme.txt?share=gofs_nope", nil, nil)
		if got.code == http.StatusOK {
			t.Fatal("伪造令牌被放行")
		}
	})

	t.Run("撤销后失效", func(t *testing.T) {
		k, tok, err := s.keys.Create(uploadkey.CreateOptions{
			Name: "临时", Scope: "/docs", Kind: uploadkey.KindRead, Creator: "boss",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := do(t, s, http.MethodGet, "/docs/readme.txt?share="+tok, nil, nil); got.code != http.StatusOK {
			t.Fatalf("撤销前返回 %d", got.code)
		}
		if err := s.keys.Revoke(k.ID); err != nil {
			t.Fatal(err)
		}
		if got := do(t, s, http.MethodGet, "/docs/readme.txt?share="+tok, nil, nil); got.code == http.StatusOK {
			t.Fatal("撤销之后仍然能读")
		}
	})
}

// TestShareCookieForSPA 确认分享 cookie 下发，且后续请求靠它就能继续读。
//
// 这一条是**目录分享能不能用**的关键：页面里的目录列表、预览都是前端
// 自己再发 XHR 拉的，那些请求不带 `?share=`。没有 cookie 的话，
// 用户会看到一个加载不出内容的空壳。
func TestShareCookieForSPA(t *testing.T) {
	s, _ := shareServer(t)
	tok := newShare(t, s, "/docs")

	// 第一次：带 ?share=，应当下发 cookie。
	r := httptest.NewRequest(http.MethodGet, "/docs/?json&share="+tok, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("首次返回 %d", res.StatusCode)
	}
	var ck *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == shareCookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("没有下发分享 cookie，SPA 的后续请求会全部 401")
	}
	if !ck.HttpOnly {
		t.Error("分享 cookie 应当是 HttpOnly")
	}
	// Path 必须限定在分享范围内，不能是全站。
	if !strings.HasPrefix(ck.Path, "/docs") {
		t.Errorf("cookie Path = %q，应当限定在 /docs 内", ck.Path)
	}

	// 第二次：只带 cookie，不带 ?share=。
	got := do(t, s, http.MethodGet, "/docs/?json", nil,
		map[string]string{"Cookie": shareCookieName + "=" + tok})
	if got.code != http.StatusOK {
		t.Fatalf("只带 cookie 返回 %d，期望 200：%s", got.code, got.body)
	}

	// 拿同一张 cookie 去读范围外，仍然要被挡住。
	out := do(t, s, http.MethodGet, "/other/secret.txt", nil,
		map[string]string{"Cookie": shareCookieName + "=" + tok})
	if out.code == http.StatusOK {
		t.Fatal("分享 cookie 被拿去读了范围外的内容")
	}
}

// TestKeyViewReportsKind 确认列表里能区分两种凭证（界面上要标出来）。
func TestKeyViewReportsKind(t *testing.T) {
	s, _ := shareServer(t)
	newShare(t, s, "/docs")
	if _, _, err := s.keys.Create(uploadkey.CreateOptions{Name: "up", Scope: "/"}); err != nil {
		t.Fatal(err)
	}

	got := do(t, s, http.MethodGet, "/fs/keys", nil,
		map[string]string{"Authorization": basicAuth("boss", "pw")})
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, `"kind":"read"`) {
		t.Errorf("列表里没有 read 类型：%s", got.body)
	}
	if !strings.Contains(got.body, `"kind":"upload"`) {
		t.Errorf("列表里没有 upload 类型：%s", got.body)
	}
}

// TestReadOnlyPermsDoNotIncludeEdit 钉住一个既有 bug。
//
// buildPerms 里 Edit 原本写的是 `canRead && AllowEdit`，而 Write/Delete/
// Extract 用的都是 `rw`。于是**任何只读账号**（不只是分享链接）都会拿到
// edit=true，界面上出现「编辑」按钮，点下去必然被 textSave 的 403 挡回来。
//
// 分享链接是只读的，所以最容易把它照出来。这里直接看页面数据 ——
// 那才是界面实际拿到的东西。
func TestReadOnlyPermsDoNotIncludeEdit(t *testing.T) {
	s, _ := shareServer(t)
	tok := newShare(t, s, "/docs")

	got := do(t, s, http.MethodGet, "/docs/?share="+tok, nil,
		map[string]string{"Accept": "text/html"})
	if got.code != http.StatusOK {
		t.Fatalf("返回 %d", got.code)
	}
	perms := pagePerms(t, got.body)
	if v, ok := perms["edit"].(bool); !ok || v {
		t.Errorf("只读权限下 edit = %v —— 界面会显示一个必然失败的「编辑」按钮", perms["edit"])
	}
	for _, k := range []string{"write", "delete", "extract"} {
		if v, _ := perms[k].(bool); v {
			t.Errorf("只读权限下 %s 不该为 true", k)
		}
	}
	if v, _ := perms["read"].(bool); !v {
		t.Error("读权限应当保留")
	}
}

// pagePerms 从页面外壳里抠出注入的初始状态，返回 perms。
func pagePerms(t *testing.T, html string) map[string]any {
	t.Helper()
	const open = `<script id="gofs-data" type="application/json">`
	i := strings.Index(html, open)
	if i < 0 {
		t.Fatalf("页面里没有 gofs-data：%.200s", html)
	}
	rest := html[i+len(open):]
	j := strings.Index(rest, "</script>")
	if j < 0 {
		t.Fatal("gofs-data 没有闭合")
	}
	var d struct {
		Perms map[string]any `json:"perms"`
	}
	if err := json.Unmarshal([]byte(rest[:j]), &d); err != nil {
		t.Fatalf("解析页面数据失败：%v", err)
	}
	return d.Perms
}

// TestSharePathFormForWget 覆盖路径形式的分享地址。
//
// 为什么需要第二种形式：wget 会把查询串拼进文件名，
// `?share=` 这条路上递归下载下来的是 `readme.txt?share=gofs_xxx`，
// 基本没法用。路径形式下所有链接都是普通相对路径。
func TestSharePathFormForWget(t *testing.T) {
	s, _ := shareServer(t)
	tok := newShare(t, s, "/docs")
	base := "/s/" + tok

	t.Run("单文件按路径取", func(t *testing.T) {
		got := do(t, s, http.MethodGet, base+"/docs/readme.txt", nil, nil)
		if got.code != http.StatusOK {
			t.Fatalf("返回 %d，期望 200：%s", got.code, got.body)
		}
		if !strings.Contains(got.body, "内容:docs/readme.txt") {
			t.Fatalf("内容不对：%q", got.body)
		}
	})

	t.Run("目录列表是带链接的 HTML，且链接不带查询串", func(t *testing.T) {
		// wget / curl 发的是 Accept: */*，应当拿到服务端渲染的链接列表，
		// 而不是那个一个 <a> 都没有的 SPA 外壳。
		got := do(t, s, http.MethodGet, base+"/docs/", nil,
			map[string]string{"Accept": "*/*"})
		if got.code != http.StatusOK {
			t.Fatalf("返回 %d", got.code)
		}
		if !strings.Contains(got.body, `href="readme.txt"`) {
			t.Fatalf("没有可跟随的链接：%.300s", got.body)
		}
		if strings.Contains(got.body, "?share=") {
			t.Fatalf("链接里还挂着查询串 —— wget 会把文件名存成 readme.txt?share=…：%.300s", got.body)
		}
		if !strings.Contains(got.body, `href="sub/"`) {
			t.Errorf("子目录链接缺失（夹具里那个目录叫 sub）：%.300s", got.body)
		}
	})

	t.Run("浏览器仍然拿到 SPA 外壳", func(t *testing.T) {
		// 判据是 Accept —— 浏览器一定带 text/html，不能把它也换成链接列表。
		got := do(t, s, http.MethodGet, base+"/docs/", nil,
			map[string]string{"Accept": "text/html,application/xhtml+xml"})
		if got.code != http.StatusOK {
			t.Fatalf("返回 %d", got.code)
		}
		if !strings.Contains(got.body, "gofs-data") {
			t.Fatalf("浏览器拿到的不是 SPA 外壳：%.200s", got.body)
		}
	})

	t.Run("路径形式同样不能越界", func(t *testing.T) {
		for _, p := range []string{"/other/secret.txt", "/other/"} {
			got := do(t, s, http.MethodGet, base+p, nil, nil)
			if got.code == http.StatusOK {
				t.Fatalf("%s 被放行了：%s", p, got.body)
			}
			if strings.Contains(got.body, "内容:other") {
				t.Fatalf("%s 泄漏了范围外内容", p)
			}
		}
	})

	t.Run("伪造的令牌不行", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/s/gofs_fake/docs/readme.txt", nil, nil)
		if got.code == http.StatusOK {
			t.Fatal("伪造令牌被放行")
		}
	})
}

// TestShareQueryFormStillWorks 确认查询串形式没被路径形式挤掉。
func TestShareQueryFormStillWorks(t *testing.T) {
	s, _ := shareServer(t)
	tok := newShare(t, s, "/docs")
	if got := do(t, s, http.MethodGet, "/docs/readme.txt?share="+tok, nil, nil); got.code != http.StatusOK {
		t.Fatalf("查询串形式返回 %d", got.code)
	}
}

// TestShareTokenCannotUpload 是权限模型的另一个方向。
//
// 已有的 TestUploadKeyIsNotAShareToken 只堵了「上传密钥当分享令牌用」；
// 反方向没堵：handleWithUploadKey 校验了令牌有效，却没校验它是**哪一种**
// —— 于是发给第三方的只读分享链接可以直接拿来 PUT/POST 上传，
// 而且落在该链接自己的 scope 内，看上去完全「合规」。
func TestShareTokenCannotUpload(t *testing.T) {
	s, root := shareServer(t)
	tok := newShare(t, s, "/docs") // kind = read（只读分享）

	put := do(t, s, http.MethodPut, "/docs/uploaded.txt?key="+tok,
		strings.NewReader("恶意内容"), nil)
	if put.code == http.StatusCreated || put.code == http.StatusOK || put.code == http.StatusNoContent {
		t.Fatalf("只读分享令牌上传成功了：%d %s", put.code, put.body)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "uploaded.txt")); err == nil {
		t.Error("文件竟然真的写进去了")
	}
}

// newShareRW 建一条**可读写**的分享链接，返回明文令牌。
func newShareRW(t *testing.T, s *Server, scope string) string {
	t.Helper()
	k, token, err := s.keys.Create(uploadkey.CreateOptions{
		Name: scope, Scope: scope, Kind: uploadkey.KindReadWrite, Creator: "boss",
	})
	if err != nil {
		t.Fatalf("建可读写分享: %v", err)
	}
	if k.KindOf() != uploadkey.KindReadWrite {
		t.Fatalf("类型 = %q，期望 readwrite", k.KindOf())
	}
	if !k.ReadWrite() || !k.IsShare() {
		t.Fatalf("ReadWrite=%v IsShare=%v，都该是 true", k.ReadWrite(), k.IsShare())
	}
	return token
}

// TestShareLinkReadWrite 确认可读写分享能在范围内写，且**仅限**范围内。
func TestShareLinkReadWrite(t *testing.T) {
	s, root := shareServer(t)
	tok := newShareRW(t, s, "/docs")

	t.Run("范围内可上传", func(t *testing.T) {
		put := do(t, s, http.MethodPut, "/docs/new.txt?share="+tok,
			strings.NewReader("x"), nil)
		if put.code != http.StatusCreated && put.code != http.StatusOK {
			t.Fatalf("可读写分享上传返回 %d：%s", put.code, put.body)
		}
		if _, err := os.Stat(filepath.Join(root, "docs", "new.txt")); err != nil {
			t.Fatal("文件没写进去")
		}
	})

	t.Run("范围内可删除", func(t *testing.T) {
		del := do(t, s, http.MethodDelete, "/docs/readme.txt?share="+tok, nil, nil)
		if del.code >= 400 {
			t.Fatalf("可读写分享删除返回 %d：%s", del.code, del.body)
		}
		if _, err := os.Stat(filepath.Join(root, "docs", "readme.txt")); err == nil {
			t.Fatal("文件没被删掉")
		}
	})

	// 给了写权限不等于给了整棵树：范围判定与只读分享走的是同一条路径。
	t.Run("范围外仍不能写", func(t *testing.T) {
		put := do(t, s, http.MethodPut, "/other/evil.txt?share="+tok,
			strings.NewReader("x"), nil)
		if put.code == http.StatusOK || put.code == http.StatusCreated {
			t.Fatalf("越界写入被放行：%d", put.code)
		}
		if _, err := os.Stat(filepath.Join(root, "other", "evil.txt")); err == nil {
			t.Fatal("文件被写到范围外了")
		}
	})
}

// TestReadWriteShareIsNotAdmin 是这次改动最要紧的一条回归。
//
// 可读写分享在范围内的权限就是 PermReadWrite，而 isAdminRequest 是按
// **请求路径**算「对根有没有读写权限」的 —— 不特意排除分享令牌的话，
// 一条 scope 为 / 的可读写分享会直接解锁服务设置（换服务根目录、重启、
// 卸载）与用户管理。那不是「能改几个文件」，是把整个服务交出去。
func TestReadWriteShareIsNotAdmin(t *testing.T) {
	s, _ := shareServer(t)
	tok := newShareRW(t, s, "/") // 范围是整个服务：最坏的情况

	t.Run("页面不给设置入口", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/?share="+tok, nil,
			map[string]string{"Accept": "text/html"})
		if got.code != http.StatusOK {
			t.Fatalf("打开页面返回 %d", got.code)
		}
		for _, bad := range []string{
			`"allow_settings":true`, `"allow_users":true`, `"allow_keys":true`,
		} {
			if strings.Contains(got.body, bad) {
				t.Errorf("分享访客的页面里出现了 %s", bad)
			}
		}
		// 反面也要钉住：读写权限本身是给了的，别把整个分享功能一起改坏。
		if !strings.Contains(got.body, `"write":true`) {
			t.Errorf("可读写分享的页面里没有写权限：%.400s", got.body)
		}
	})

	t.Run("改服务设置被拒", func(t *testing.T) {
		got := do(t, s, http.MethodPost, "/fs/settings?share="+tok,
			strings.NewReader(`{"upload_max_size":1024}`),
			map[string]string{"Content-Type": "application/json"})
		if got.code < 400 {
			t.Fatalf("分享访客改动了服务设置：%d %s", got.code, got.body)
		}
	})

	t.Run("重启被拒", func(t *testing.T) {
		got := do(t, s, http.MethodPost, "/fs/restart?share="+tok, nil, nil)
		if got.code < 400 {
			t.Fatalf("分享访客重启了服务：%d", got.code)
		}
	})

	t.Run("用户管理被拒", func(t *testing.T) {
		got := do(t, s, http.MethodGet, "/fs/users?share="+tok, nil, nil)
		if got.code < 400 {
			t.Fatalf("分享访客读到了用户列表：%d", got.code)
		}
	})
}

// TestCannotMintShareBeyondOwnPerm 钉住「只能授出自己有的」。
//
// 一个只在 /read 上有读权限的账号不能签发 /read 的可读写分享 ——
// 那等于把自己没有的写权限借出去。它在 /write 上确实能写，所以那里可以。
func TestCannotMintShareBeyondOwnPerm(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "read", "a.txt"), []byte("x"))
	writeFile(t, filepath.Join(root, "write", "b.txt"), []byte("x"))
	s := newTestServer(t, testConfig(t, root, "-A",
		"-a", "u:pw@/write:rw,/read:r"))

	post := func(kind, scope string) resp {
		t.Helper()
		return do(t, s, http.MethodPost, "/fs/keys",
			strings.NewReader(`{"kind":"`+kind+`","name":"t","scope":"`+scope+`"}`),
			map[string]string{
				"Content-Type":  "application/json",
				"Authorization": basicAuth("u", "pw"),
			})
	}

	if got := post(uploadkey.KindReadWrite, "/read"); got.code != http.StatusForbidden {
		t.Fatalf("只读范围签发可读写分享返回 %d，期望 403：%s", got.code, got.body)
	}
	if got := post(uploadkey.KindRead, "/read"); got.code != http.StatusCreated {
		t.Fatalf("只读范围签发只读分享返回 %d，期望 201：%s", got.code, got.body)
	}
	if got := post(uploadkey.KindReadWrite, "/write"); got.code != http.StatusCreated {
		t.Fatalf("可写范围签发可读写分享返回 %d，期望 201：%s", got.code, got.body)
	}
}

// TestShareCookieForSPAOnPathForm 钉住一个很容易漏掉的方向。
//
// 分享地址有两种形式，而**「分享」按钮生成的是路径形式**（/s/<token>/…，
// 为了 wget 递归下来文件名干净）。SPA 打开后会把地址栏里的 /s/<token>/ 前缀
// 丢掉（它按 state.path 拼 URL），后续的 XHR 全是 /docs/… 这种普通路径 ——
// 那些请求只能靠这条 cookie 认人。
//
// 早先只在 `?share=` 形式下发 cookie，于是路径形式打开的分享页表现为
// 「页面能看，但点什么都跳登录页」：列子目录、预览、上传、删除全部 401，
// 前端收到 401 就弹登录框。可读写分享把这个 bug 暴露得最明显
// （点删除/上传立刻就撞上），但只读分享同样中招，只是不容易发现。
func TestShareCookieForSPAOnPathForm(t *testing.T) {
	s, _ := shareServer(t)
	tok := newShareRW(t, s, "/docs")

	// 第一步：路径形式的页面请求要下发 cookie。
	r := httptest.NewRequest(http.MethodGet, "/s/"+tok+"/docs/?json", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("返回 %d，期望 200", res.StatusCode)
	}
	var ck *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == shareCookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("路径形式的分享页没有下发 cookie —— 页面里的 XHR 会全部 401")
	}
	if !strings.HasPrefix(ck.Path, "/docs") {
		t.Errorf("cookie Path = %q，应当限定在 /docs 内", ck.Path)
	}

	// 第二步（真正的回归点）：**不带令牌、只靠 cookie** 去写，必须成功。
	// 这就是 SPA 的处境 —— 它发的是 /docs/… 普通路径，URL 里没有令牌。
	cookie := map[string]string{"Cookie": shareCookieName + "=" + tok}
	put := do(t, s, http.MethodPut, "/docs/via-cookie.txt", strings.NewReader("x"), cookie)
	if put.code != http.StatusCreated && put.code != http.StatusOK {
		t.Fatalf("只靠 cookie 的上传返回 %d，期望 2xx：%s", put.code, put.body)
	}
	del := do(t, s, http.MethodDelete, "/docs/via-cookie.txt", nil, cookie)
	if del.code >= 400 {
		t.Fatalf("只靠 cookie 的删除返回 %d：%s", del.code, del.body)
	}
	// 子目录列举（只读分享也走的同一条路）同样不能 401。
	if got := do(t, s, http.MethodGet, "/docs/sub/?json", nil, cookie); got.code != http.StatusOK {
		t.Fatalf("只靠 cookie 列子目录返回 %d", got.code)
	}
}
