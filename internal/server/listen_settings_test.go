package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxl6097/gofs/internal/config"
)

// fakeRelistener 记录被要求绑到哪里，并按预设结果作答。
//
// 真的去 Listen 一个端口不是这里要测的（那是根包 relisten_test.go 的事）；
// 这里测的是**设置接口这一层**：校验、调用、失败处理、落盘顺序、跳转地址。
type fakeRelistener struct {
	can     bool
	err     error
	gotBind string
	gotPort int
	calls   int
}

func (f *fakeRelistener) CanRelisten() bool { return f.can }

func (f *fakeRelistener) Relisten(_ context.Context, bind string, port int) (string, error) {
	f.calls++
	f.gotBind, f.gotPort = bind, port
	if f.err != nil {
		return "", f.err
	}
	return config.ListenAddr(bind, port), nil
}

// listenServer 造一个「可以改监听」的实例。
//
// 刻意把 BindSet/PortSet 清掉：testConfig 总是带 -b/-p，那会把两项标成
// 「显式指定」，于是文件里的值一律不采纳 —— 那是另一条测试要覆盖的分支。
// 这里要的是「启动参数没给过监听」的常见部署。
func listenServer(t *testing.T, sf string, rl config.Relistener, extra ...string) *Server {
	t.Helper()
	cfg := testConfig(t, t.TempDir(), append([]string{"-A", "-a", "boss:pw@/:rw"}, extra...)...)
	cfg.BindSet = false
	cfg.PortSet = false
	cfg.SettingsFile = sf
	cfg.Relistener = rl
	return newTestServer(t, cfg)
}

// TestSettingsReplyCarriesListen 确认常规页需要的字段都下发了。
func TestSettingsReplyCarriesListen(t *testing.T) {
	rl := &fakeRelistener{can: true}
	// 这里**不能**用 listenServer：它会把 BindSet/PortSet 清掉，而本用例正是
	// 要验证「启动参数显式给过监听 → listen_locked 为 true」。
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw", "--bind", "127.0.0.1", "--port", "8080")
	cfg.Relistener = rl
	s := newTestServer(t, cfg)

	body := do(t, s, http.MethodGet, "/fs/settings", nil, adminSettingsHdr()).body
	var rep map[string]any
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"listen_addr":         "127.0.0.1",
		"listen_port":         float64(8080),
		"listen_addr_default": "127.0.0.1",
		"listen_port_default": float64(8080),
		"listen_locked":       true, // --bind/--port 显式给过
		"listen_editable":     true,
	} {
		if got := rep[k]; got != want {
			t.Errorf("%s = %v，期望 %v", k, got, want)
		}
	}

	// 启动参数没给过时不该显示「已锁」。
	unlocked := listenServer(t, "", rl, "--bind", "127.0.0.1", "--port", "8080")
	if b := do(t, unlocked, http.MethodGet, "/fs/settings", nil, adminSettingsHdr()).body; !strings.Contains(b, `"listen_locked":false`) {
		t.Errorf("启动参数没给过监听时 listen_locked 应为 false：%s", b)
	}

	// 没注入 Relistener（或它说自己不能）时不该显示那一栏。
	s2 := listenServer(t, "", nil)
	if got := do(t, s2, http.MethodGet, "/fs/settings", nil, adminSettingsHdr()).body; !strings.Contains(got, `"listen_editable":false`) {
		t.Errorf("没有 Relistener 时 listen_editable 应为 false：%s", got)
	}
}

// TestChangeListenHappyPath 确认改端口会去调 Relistener 并落盘。
func TestChangeListenHappyPath(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	rl := &fakeRelistener{can: true}
	s := listenServer(t, sf, rl, "--bind", "127.0.0.1", "--port", "8080")

	got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"listen_port":9000}`), adminSettingsHdr())
	if got.code != http.StatusOK {
		t.Fatalf("改端口返回 %d：%s", got.code, got.body)
	}
	if rl.calls != 1 {
		t.Fatalf("Relistener 被调了 %d 次，期望 1", rl.calls)
	}
	// 只传端口时地址沿用当前值。
	if rl.gotBind != "127.0.0.1" || rl.gotPort != 9000 {
		t.Errorf("Relistener 收到 (%q,%d)，期望 (127.0.0.1,9000)", rl.gotBind, rl.gotPort)
	}
	if s.ListenPort() != 9000 {
		t.Errorf("内存里的端口 = %d，期望 9000", s.ListenPort())
	}
	raw, err := os.ReadFile(sf)
	if err != nil {
		t.Fatalf("设置文件没写出来: %v", err)
	}
	if !strings.Contains(string(raw), `"listen_port": 9000`) {
		t.Errorf("端口没落盘：%s", raw)
	}
}

// TestChangeListenFailureKeepsEverything 确认重绑失败时**什么都不变**。
//
// 两个「不变」都要钉住：内存状态不变（页面继续按旧地址显示），文件也不变 ——
// 把建不起来的地址写进文件，下次重启就起不来了。
func TestChangeListenFailureKeepsEverything(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	rl := &fakeRelistener{can: true, err: errors.New("监听 127.0.0.1:9000 失败: bind: address already in use")}
	s := listenServer(t, sf, rl, "--bind", "127.0.0.1", "--port", "8080")

	// 先写一个正常值，好确认它没被弄坏。
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"标题"}`), adminSettingsHdr()); got.code != http.StatusOK {
		t.Fatalf("改标题返回 %d", got.code)
	}

	got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"listen_port":9000}`), adminSettingsHdr())
	if got.code != http.StatusBadRequest {
		t.Fatalf("失败的改端口返回 %d，期望 400：%s", got.code, got.body)
	}
	// 底层错误要原样透出来 —— address already in use 比任何我们编的话都有用。
	if !strings.Contains(got.body, "address already in use") {
		t.Errorf("没有透出底层错误：%s", got.body)
	}
	if s.ListenPort() != 8080 {
		t.Errorf("失败后内存里的端口 = %d，期望仍是 8080", s.ListenPort())
	}
	raw, _ := os.ReadFile(sf)
	if strings.Contains(string(raw), "listen_port") {
		t.Errorf("失败的值不该落盘：%s", raw)
	}
}

// TestChangeListenValidation 覆盖校验与「没启用」两条拒绝路径。
func TestChangeListenValidation(t *testing.T) {
	rl := &fakeRelistener{can: true}
	s := listenServer(t, "", rl, "--bind", "127.0.0.1", "--port", "8080")
	hdr := adminSettingsHdr()
	put := func(body string) resp {
		return do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(body), hdr)
	}

	for _, tc := range []struct{ name, body string }{
		{"端口越界", `{"listen_port":70000}`},
		{"端口为负", `{"listen_port":-1}`},
		{"地址里带端口", `{"listen_addr":"127.0.0.1:9000"}`},
		{"地址里有空白", `{"listen_addr":"127.0.0.1 9000"}`},
	} {
		if got := put(tc.body); got.code != http.StatusBadRequest {
			t.Errorf("%s 返回 %d，期望 400：%s", tc.name, got.code, got.body)
		}
	}
	if rl.calls != 0 {
		t.Errorf("校验失败的请求不该碰到 Relistener（被调 %d 次）", rl.calls)
	}

	// 值没变就不必重绑。
	if got := put(`{"listen_port":8080}`); got.code != http.StatusOK {
		t.Fatalf("设成同值返回 %d", got.code)
	}
	if rl.calls != 0 {
		t.Errorf("值没变却重绑了 %d 次", rl.calls)
	}

	// 没启用（以库方式嵌入）→ 409，而不是 500/400 这种让人摸不着头绪的码。
	s2 := listenServer(t, "", nil, "--bind", "127.0.0.1", "--port", "8080")
	if got := do(t, s2, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"listen_port":9000}`), hdr); got.code != http.StatusConflict {
		t.Errorf("未启用时返回 %d，期望 409：%s", got.code, got.body)
	}

	// 只有管理员能改。
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"listen_port":9000}`), nil); got.code != http.StatusUnauthorized {
		t.Errorf("匿名返回 %d，期望 401", got.code)
	}
}

// TestListenJumpURLOnlyWhenDirect 确认跳转地址只在「浏览器直达监听端口」时给出。
//
// 前面挡着反向代理时（请求 Host 的端口与监听端口对不上），改监听端口只改了
// 上游那一侧，把页面地址也改过去只会让人更连不上 —— 必须不给跳转地址。
func TestListenJumpURLOnlyWhenDirect(t *testing.T) {
	rl := &fakeRelistener{can: true}
	s := listenServer(t, "", rl, "--bind", "127.0.0.1", "--port", "8080")

	put := func(host string) resp {
		r := httptest.NewRequest(http.MethodPut, "/fs/settings",
			strings.NewReader(`{"listen_port":9000}`))
		r.Host = host
		r.Header.Set("Authorization", basicAuth("boss", "pw"))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		res := w.Result()
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return resp{code: res.StatusCode, body: string(b), head: res.Header}
	}

	// 直达：请求 Host 的端口就是当前监听端口 → 给出跳转地址。
	direct := put("192.168.1.9:8080")
	if direct.code != http.StatusOK {
		t.Fatalf("直达场景返回 %d：%s", direct.code, direct.body)
	}
	if !strings.Contains(direct.body, `"listen_jump_url":"http://192.168.1.9:9000/"`) {
		t.Errorf("直达场景没给出跳转地址：%s", direct.body)
	}

	// 经过代理：端口对不上 → 不跳。此时端口已经是 9000 了，先改回 8080。
	rl2 := &fakeRelistener{can: true}
	s2 := listenServer(t, "", rl2, "--bind", "127.0.0.1", "--port", "8080")
	r := httptest.NewRequest(http.MethodPut, "/fs/settings",
		strings.NewReader(`{"listen_port":9000}`))
	r.Host = "files.example.com" // 没有端口（80/443 被代理收着）
	r.Header.Set("Authorization", basicAuth("boss", "pw"))
	w := httptest.NewRecorder()
	s2.Handler().ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("代理场景返回 %d：%s", res.StatusCode, b)
	}
	if strings.Contains(string(b), "listen_jump_url") {
		t.Errorf("代理场景不该给跳转地址：%s", b)
	}

	// unix socket 同理：浏览器访问不到。
	rl3 := &fakeRelistener{can: true}
	s3 := listenServer(t, "", rl3, "--bind", "127.0.0.1", "--port", "8080")
	body := do(t, s3, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"listen_addr":"/tmp/gofs-test.sock"}`), adminSettingsHdr()).body
	if strings.Contains(body, "listen_jump_url") {
		t.Errorf("unix socket 场景不该给跳转地址：%s", body)
	}
}

// TestListenPersistRules 覆盖落盘与「启动参数优先」这条恢复入口。
func TestListenPersistRules(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")

	// ① 启动参数没给过监听 → 文件里的值胜出（页面上改过的设置要能跨重启）。
	rl := &fakeRelistener{can: true}
	s := listenServer(t, sf, rl, "--bind", "127.0.0.1", "--port", "8080")
	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"listen_addr":"0.0.0.0","listen_port":9000}`),
		adminSettingsHdr()); got.code != http.StatusOK {
		t.Fatalf("改监听返回 %d：%s", got.code, got.body)
	}
	s2 := listenServer(t, sf, &fakeRelistener{can: true}, "--bind", "127.0.0.1", "--port", "8080")
	if s2.ListenPort() != 9000 || s2.ListenBind() != "0.0.0.0" {
		t.Errorf("重启后 = %q:%d，期望 0.0.0.0:9000（文件优先）", s2.ListenBind(), s2.ListenPort())
	}

	// ② 启动参数显式给过 → 参数胜出，文件里的记录被忽略（改坏了的退路）。
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw", "--bind", "127.0.0.1", "--port", "5000")
	cfg.SettingsFile = sf
	cfg.Relistener = &fakeRelistener{can: true}
	s3 := newTestServer(t, cfg) // testConfig 带 -b/-p，所以 BindSet/PortSet 为 true
	if s3.ListenPort() != 5000 || s3.ListenBind() != "127.0.0.1" {
		t.Errorf("重启后 = %q:%d，期望 127.0.0.1:5000（启动参数优先）", s3.ListenBind(), s3.ListenPort())
	}
}

// TestListenNotPersistedWhenUnchanged 确认「没改过就不写盘」。
func TestListenNotPersistedWhenUnchanged(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "settings.json")
	s := listenServer(t, sf, &fakeRelistener{can: true}, "--bind", "127.0.0.1", "--port", "8080")

	if got := do(t, s, http.MethodPut, "/fs/settings",
		strings.NewReader(`{"title":"只改标题"}`), adminSettingsHdr()); got.code != http.StatusOK {
		t.Fatalf("改标题返回 %d：%s", got.code, got.body)
	}
	raw, err := os.ReadFile(sf)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"listen_addr", "listen_port"} {
		if strings.Contains(string(raw), unwanted) {
			t.Errorf("没改过的监听不该写进文件（%s）：%s", unwanted, raw)
		}
	}
}
