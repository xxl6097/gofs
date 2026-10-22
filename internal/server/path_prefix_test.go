package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// prefixServer 起一个「admin 可写全盘」的实例，设置文件指向 sf。
func prefixServer(t *testing.T, sf string, extra ...string) *Server {
	t.Helper()
	args := append([]string{"-A", "-a", "boss:pw@/:rw", "-a", "guest:g@/:r"}, extra...)
	cfg := testConfig(t, t.TempDir(), args...)
	cfg.SettingsFile = sf
	return newTestServer(t, cfg)
}

// TestPathPrefixRuntimeChange 是这次的核心：页面上改前缀 → 立刻按新前缀路由。
//
// 只测「接口返回了 new_prefix」是不够的 —— 那只证明字段存下来了。
// 真正要钉的是 withPathPrefix 会**每次请求现取**这个值：早先它在构造中间件时
// 把前缀捕获进闭包，改完仍是旧前缀在路由（而且空前缀还会直通短路，永远装不上
// 剥离逻辑），接口返回 200 但页面照样 404。
func TestPathPrefixRuntimeChange(t *testing.T) {
	s := prefixServer(t, "")
	hdr := adminSettingsHdr()
	// 接口地址本身也要跟着当前前缀走 —— 改完之后旧的 /fs/settings 就已经
	// 不再被路由了（这正是本功能生效的证明，见下面的 404 断言）。
	put := func(body string) resp {
		return do(t, s, http.MethodPut, s.PathPrefix()+"/fs/settings", strings.NewReader(body), hdr)
	}
	get := func(p string) resp { return do(t, s, http.MethodGet, s.PathPrefix()+p, nil, hdr) }

	// 起手没有前缀：根路径可用。
	if got := get("/?json"); got.code != http.StatusOK {
		t.Fatalf("无前缀时 / 返回 %d", got.code)
	}

	got := put(`{"path_prefix":"/data"}`)
	if got.code != http.StatusOK {
		t.Fatalf("改前缀返回 %d：%s", got.code, got.body)
	}
	if !strings.Contains(got.body, `"path_prefix":"/data"`) {
		t.Errorf("响应里没有新前缀：%s", got.body)
	}

	// 服务端路由立刻跟着变。
	if r := do(t, s, http.MethodGet, "/data/?json", nil, hdr); r.code != http.StatusOK {
		t.Errorf("新前缀下访问返回 %d，期望 200", r.code)
	}
	if r := do(t, s, http.MethodGet, "/?json", nil, hdr); r.code != http.StatusNotFound {
		t.Errorf("旧路径（根）返回 %d，期望 404", r.code)
	}
	if r := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"x"}`), hdr); r.code != http.StatusNotFound {
		t.Errorf("旧前缀下的接口返回 %d，期望 404", r.code)
	}

	// 页面外壳里的资源地址与品牌链接也要跟着走，否则页面能打开但一片空白。
	page := get("/")
	if !strings.Contains(page.body, "/data/fs/assets/") {
		t.Errorf("资源前缀没跟随运行期改动：%.300s", page.body)
	}
	if !strings.Contains(page.body, `"path_prefix":"/data"`) {
		t.Errorf("注入页面的 path_prefix 没更新：%.300s", page.body)
	}

	// 改回空前缀也要生效（空前缀不是「没设置」，是「服务在根」）。
	if r := put(`{"path_prefix":""}`); r.code != http.StatusOK {
		t.Fatalf("清空前缀返回 %d：%s", r.code, r.body)
	}
	if r := do(t, s, http.MethodGet, "/?json", nil, hdr); r.code != http.StatusOK {
		t.Errorf("清空前缀后 / 返回 %d，期望 200", r.code)
	}
}

// TestPathPrefixRoundTripAndPersist 覆盖落盘：改前缀 → 写文件 → 重启读回来。
func TestPathPrefixRoundTripAndPersist(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	s := prefixServer(t, sf)

	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"path_prefix":"/files"}`), adminSettingsHdr()); got.code != http.StatusOK {
		t.Fatalf("改前缀返回 %d：%s", got.code, got.body)
	}
	raw, err := os.ReadFile(sf)
	if err != nil {
		t.Fatalf("设置文件没写出来: %v", err)
	}
	if !strings.Contains(string(raw), `"path_prefix": "/files"`) {
		t.Errorf("设置文件里没有前缀：%s", raw)
	}

	// 新实例（＝重启）用同一份文件，启动参数里**没有**前缀 → 文件胜出。
	if got := prefixServer(t, sf).PathPrefix(); got != "/files" {
		t.Errorf("重启后前缀 = %q，期望 /files", got)
	}
}

// TestPathPrefixFlagBeatsFile 钉住那个刻意与其它设置项相反的口径。
//
// 前缀改成一个反向代理没路由的值之后，页面在新旧两个前缀下都打不开 ——
// 而页面正是改回它的唯一入口。所以「用参数重启一次」必须能救回来，
// 也就是启动参数要压过文件里的记录。其它设置项（标题、上限）是文件优先，
// 这条测试同时也是在提醒：别为了「统一」把这个例外改掉。
func TestPathPrefixFlagBeatsFile(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(sf, []byte(`{"version":4,"path_prefix":"/data"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := prefixServer(t, sf, "--path-prefix", "/files")
	if got := s.PathPrefix(); got != "/files" {
		t.Errorf("前缀 = %q，期望启动参数胜出（/files）而不是文件里的 /data", got)
	}
	// 实例开了鉴权，所以带上凭据 —— 这里要验的是路径被路由到，不是鉴权。
	if r := do(t, s, http.MethodGet, "/files/?json", nil, adminSettingsHdr()); r.code != http.StatusOK {
		t.Errorf("参数指定的前缀不可用，返回 %d", r.code)
	}
	// 页面上也应该能看出这个前缀是「参数锁住」的。
	body := do(t, s, http.MethodGet, "/files/fs/settings", nil, adminSettingsHdr()).body
	if !strings.Contains(body, `"path_prefix_locked":true`) {
		t.Errorf("没有下发 path_prefix_locked：%s", body)
	}
}

// TestPathPrefixNormalize 覆盖归一化与非法值。
func TestPathPrefixNormalize(t *testing.T) {
	s := prefixServer(t, "")
	hdr := adminSettingsHdr()
	// 每次都要打在当前前缀下：上一轮可能刚把前缀改掉。
	put := func(body string) resp {
		return do(t, s, http.MethodPut, s.PathPrefix()+"/fs/settings", strings.NewReader(body), hdr)
	}

	// 归一化：补前导斜杠、去尾斜杠、"//" 与 "/" 都当空。
	for _, tc := range []struct{ in, want string }{
		{"data", "/data"},
		{"/data/", "/data"},
		{"  /data  ", "/data"},
		{"/", ""},
		{"", ""},
		{"/a/b", "/a/b"},
	} {
		if got := put(`{"path_prefix":"` + tc.in + `"}`); got.code != http.StatusOK {
			t.Errorf("设 %q 返回 %d：%s", tc.in, got.code, got.body)
			continue
		}
		if got := s.PathPrefix(); got != tc.want {
			t.Errorf("设 %q 归一化成 %q，期望 %q", tc.in, got, tc.want)
		}
	}

	// 会破坏 URL / cookie Path / 页面标记的取值必须当场拒掉，
	// 而不是收下再出怪问题。
	for _, bad := range []string{
		"/a b", "/a\tb", "/files?x", "/files#y", "/a/../b", "/a/./b",
		// HTML 注入面：这些字符会进入 index.html 的 href/src。
		`/x"><img/src=x/onerror=alert(1)>`,
		`/x'onload='alert(1)`,
		`/a<b`, `/a>b`, `/a\b`,
		// 浏览器会把 //host 当成协议相对地址（另一个主机）。
		`//evil.example.com`,
		// 控制字符。
		"/a\x01b",
	} {
		body, _ := json.Marshal(map[string]string{"path_prefix": bad})
		got := do(t, s, http.MethodPut, s.PathPrefix()+"/fs/settings", bytes.NewReader(body), hdr)
		if got.code != http.StatusBadRequest {
			t.Errorf("非法前缀 %q 返回 %d，期望 400：%s", bad, got.code, got.body)
		}
	}
}

// TestWriteHTMLEscapesPathPrefix 是输出层的最后一道防线。
//
// 输入校验已经拒绝引号/尖括号，但旧版本写下的 settings.json、手工改过的
// 文件、或将来放宽校验，都不该让前缀直接变成标记。这里直接改运行时设置
// （绕过 API），验证 writeHTML 仍会转义。
func TestWriteHTMLEscapesPathPrefix(t *testing.T) {
	s := newTestServer(t, testConfig(t, t.TempDir()))
	payload := `/x"><img/src=x/onerror=alert(1)>`
	if err := s.settings.SetPathPrefix(payload); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.writeHTML(w, pageData{})
	body := w.Body.String()
	if strings.Contains(body, `<img/src=x`) {
		t.Fatalf("path_prefix 未经转义进入页面：%.400s", body)
	}
	if !strings.Contains(body, `&lt;img/src=x`) {
		t.Fatalf("path_prefix 没有被 HTML 转义：%.400s", body)
	}
}

// TestPathPrefixOnlyAdmin 确认改前缀要管理员 —— 它影响所有人能不能打开页面。
func TestPathPrefixOnlyAdmin(t *testing.T) {
	s := prefixServer(t, "")

	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"path_prefix":"/x"}`), nil); got.code != http.StatusUnauthorized {
		t.Errorf("匿名返回 %d，期望 401", got.code)
	}
	guest := map[string]string{"Authorization": basicAuth("guest", "g")}
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"path_prefix":"/x"}`), guest); got.code == http.StatusOK {
		t.Errorf("只读账号竟然能改前缀：%s", got.body)
	}
}

// TestPathPrefixNotPersistedWhenUnchanged 确认「没改过就不写盘」。
//
// 与两个上限同一个理由：无条件写会把「其实等于启动值」的记录留在文件里，
// 运维之后改 --path-prefix 就被它压住。
func TestPathPrefixNotPersistedWhenUnchanged(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	s := prefixServer(t, sf, "--path-prefix", "/files")

	// 只改标题（接口地址带上当前前缀 —— 它就是启动参数给的那个）。
	if got := do(t, s, http.MethodPut, "/files/fs/settings",
		strings.NewReader(`{"title":"只改标题"}`), adminSettingsHdr()); got.code != http.StatusOK {
		t.Fatalf("改标题返回 %d：%s", got.code, got.body)
	}
	raw, err := os.ReadFile(sf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "path_prefix") {
		t.Errorf("没改过的前缀不该写进文件：%s", raw)
	}
}

// TestPathPrefixSessionCookieMigrated 确认改前缀不会把在线的人踢下线。
//
// 浏览器的 cookie 按 (name, domain, path) 三元组存：Path=/ 的那条在
// /data/… 的请求上不会被带上，不重发就等于「莫名其妙被登出」。
func TestPathPrefixSessionCookieMigrated(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root, "-a", "admin:secret@/:rw")
	cfg.SettingsFile = filepath.Join(t.TempDir(), "settings.json")
	s := newTestServer(t, cfg)

	ck := login(t, s, "admin", "secret")

	r := httptest.NewRequest(http.MethodPut, "/fs/settings",
		strings.NewReader(`{"path_prefix":"/data"}`))
	r.AddCookie(ck)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("改前缀返回 %d", res.StatusCode)
	}

	raw := strings.Join(res.Header.Values("Set-Cookie"), "\n")
	// 新 Path 那条要下发，旧 Path 那条要清掉（值留空 —— 否则它会在
	// cookie 罐里一直躺到过期，prefix 改回去时又冒出来）。
	if !strings.Contains(raw, "Path=/data/") {
		t.Errorf("没有按新前缀重发会话 cookie：%q", raw)
	}
	if !strings.Contains(raw, sessionCookieName+"=; Path=/;") {
		t.Errorf("没有清掉旧 Path 上的会话 cookie：%q", raw)
	}

	// 新前缀下带着这张 cookie 仍然能读。
	got := do(t, s, http.MethodGet, "/data/?json", nil,
		map[string]string{"Cookie": ck.Name + "=" + ck.Value})
	if got.code != http.StatusOK {
		t.Errorf("新前缀下带会话 cookie 访问返回 %d，期望 200", got.code)
	}
}

// TestSharePathFormUnderPrefix 是回归测试：路径形式的分享地址在前缀部署下曾一律 404。
//
// 原因是中间件顺序 —— withSharePath 排在前缀剥离之前，拿着 `/files/s/<token>/…`
// 去比 `/s/` 这个字面前缀，永远匹配不上，于是被当成普通文件路径。
// 分享链接是**发给别人**的，装到前缀部署上就静默失效，很难被发现。
func TestSharePathFormUnderPrefix(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"docs/readme.txt", "docs/sub/deep.txt"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("内容:"+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 只有 boss 能读根目录 —— 所以「没登录也能读」只可能来自分享链接。
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw",
		"--path-prefix", "/files"))
	tok := newShare(t, s, "/docs")

	for _, p := range []string{"/docs/readme.txt", "/docs/sub/deep.txt", "/docs/?json"} {
		got := do(t, s, http.MethodGet, "/files/s/"+tok+p, nil, nil)
		if got.code != http.StatusOK {
			t.Errorf("带前缀的分享地址 %s 返回 %d，期望 200：%.200s", p, got.code, got.body)
		}
	}
	// 范围之外仍然要挡住，别为了修匹配把权限一起放开。
	if got := do(t, s, http.MethodGet, "/files/s/"+tok+"/other/secret.txt", nil, nil); got.code == http.StatusOK {
		t.Errorf("超出范围的路径竟然能读：%s", got.body)
	}
}

// TestShareCookiePathIncludesPrefix 钉住分享 cookie 的 Path 要带上访问前缀。
//
// scope 是**文件路径**，而 cookie 的 Path 比的是**请求 URL**。少了前缀这一层，
// 分享页面能打开、列表却刷不出来：页面里的 XHR 不会带上这条 cookie，全 401。
func TestShareCookiePathIncludesPrefix(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, testConfig(t, root, "-A", "-a", "boss:pw@/:rw",
		"--path-prefix", "/files"))
	tok := newShare(t, s, "/docs")

	// 首次带 ?share= 打开，应当下发 cookie。
	r := httptest.NewRequest(http.MethodGet, "/files/docs/?json&share="+tok, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("带 ?share= 访问返回 %d", res.StatusCode)
	}

	var found bool
	for _, c := range res.Cookies() {
		if c.Name != shareCookieName {
			continue
		}
		found = true
		if c.Path != "/files/docs/" {
			t.Errorf("分享 cookie 的 Path = %q，期望 /files/docs/", c.Path)
		}
	}
	if !found {
		t.Fatalf("没有下发分享 cookie：%q", res.Header.Values("Set-Cookie"))
	}
}

// TestPathPrefixNormalizeHelper 直接钉住 config 里的归一化规则。
//
// 启动参数与页面设置走的是同一个函数，规则分叉就会出现「参数写 /files、
// 页面写 files」得到两个不同前缀这种没法排查的问题。
func TestPathPrefixNormalizeHelper(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{"", "", false},
		{"/", "", false},
		{"//", "", false},
		{"files", "/files", false},
		{"/files/", "/files", false},
		{"  /files  ", "/files", false},
		{"/a/b/", "/a/b", false},
		{"/a//", "/a", false},
		{"/a&b", "/a&b", false},
		{"/文件", "/文件", false},
		{"/a b", "", true},
		{"/a?b", "", true},
		{"/a#b", "", true},
		{"/a/../b", "", true},
		{"/a/./b", "", true},
		{"/a//b", "", true},
		{"//evil.example.com", "", true},
		{`/x"><img/src=x/onerror=alert(1)>`, "", true},
		{`/a'b`, "", true},
		{`/a\b`, "", true},
		{"/a\x01b", "", true},
	} {
		got, err := config.NormalizePathPrefix(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizePathPrefix(%q) 期望报错，却得到 %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizePathPrefix(%q) 报错: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizePathPrefix(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestPathPrefixConfigNormalizeUsesSameRules 确认启动参数那条路也走了同一套规则。
func TestPathPrefixConfigNormalizeUsesSameRules(t *testing.T) {
	cfg := config.Defaults()
	cfg.PathPrefix = "files/"
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.PathPrefix != "/files" {
		t.Errorf("Normalize 后前缀 = %q，期望 /files", cfg.PathPrefix)
	}

	bad := config.Defaults()
	bad.PathPrefix = "/a b"
	if err := bad.Normalize(); err == nil {
		t.Errorf("非法前缀没被 Normalize 拒掉")
	}
}
