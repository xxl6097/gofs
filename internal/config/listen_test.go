package config

import (
	"strings"
	"testing"
)

// TestListenAddr 钉住 bind+port → 监听地址的四种组合。
//
// 它是启动、热重绑、页面展示三处共用的唯一一份实现，规则一旦走岔，
// 「页面上显示的当前地址」与「实际监听的地址」就会对不上 —— 那种不一致
// 只能靠抓包发现。
func TestListenAddr(t *testing.T) {
	for _, tc := range []struct {
		bind string
		port int
		want string
	}{
		{"", 5000, "0.0.0.0:5000"},                 // 空 → 所有网卡
		{"127.0.0.1", 5000, "127.0.0.1:5000"},      // 纯主机 → 拼端口
		{"127.0.0.1:9000", 5000, "127.0.0.1:9000"}, // 已含端口 → 原样，port 忽略
		{"/run/gofs.sock", 5000, "/run/gofs.sock"}, // unix socket → 原样
	} {
		if got := ListenAddr(tc.bind, tc.port); got != tc.want {
			t.Errorf("ListenAddr(%q,%d) = %q，期望 %q", tc.bind, tc.port, got, tc.want)
		}
	}

	// Config.Addr 必须与它完全一致（就是同一个函数）。
	c := &Config{Bind: "127.0.0.1", Port: 8080}
	if c.Addr() != ListenAddr(c.Bind, c.Port) {
		t.Errorf("Addr() = %q 与 ListenAddr 不一致", c.Addr())
	}
}

// TestNormalizeListen 覆盖页面上那两个字段的校验。
func TestNormalizeListen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bind     string
		port     int
		wantBind string
		wantPort int
		wantErr  bool
	}{
		{name: "空地址", bind: "", port: 5000, wantBind: "", wantPort: 5000},
		{name: "去空白", bind: "  127.0.0.1  ", port: 5000, wantBind: "127.0.0.1", wantPort: 5000},
		{name: "unix socket", bind: "/run/gofs.sock", port: 0, wantBind: "/run/gofs.sock", wantPort: 0},
		{name: "端口 0 由系统分配", bind: "", port: 0, wantBind: "", wantPort: 0},
		{name: "端口上界", bind: "", port: 65535, wantBind: "", wantPort: 65535},
		{name: "端口越界", bind: "", port: 65536, wantErr: true},
		{name: "端口为负", bind: "", port: -1, wantErr: true},
		// 地址里带端口是**故意**拒绝的：页面上端口有单独的框，
		// 两处都能表达它只会让人不知道以哪个为准。
		{name: "地址里带端口", bind: "127.0.0.1:9000", port: 5000, wantErr: true},
		{name: "地址里有空白", bind: "127.0.0.1 9000", port: 5000, wantErr: true},
		{name: "地址里有斜杠", bind: "127.0.0.1/x", port: 5000, wantErr: true},
	} {
		gotBind, gotPort, err := NormalizeListen(tc.bind, tc.port)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s：期望报错，却得到 (%q,%d)", tc.name, gotBind, gotPort)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：报错 %v", tc.name, err)
			continue
		}
		if gotBind != tc.wantBind || gotPort != tc.wantPort {
			t.Errorf("%s：得到 (%q,%d)，期望 (%q,%d)",
				tc.name, gotBind, gotPort, tc.wantBind, tc.wantPort)
		}
	}
}

// TestBindSetPortSetRecorded 确认「用户到底写没写」被记下来了。
//
// 这是热重绑能救回来的前提：Port 有默认值 5000，光看值分不出「显式写了
// 5000」与「默认就是 5000」，而运行期设置要靠这个标记决定「重启后听谁的」。
func TestBindSetPortSetRecorded(t *testing.T) {
	// 都没写。
	cfg, err := parse(t, "-log-format", "none", ".")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BindSet || cfg.PortSet {
		t.Errorf("没写监听参数却标了显式：BindSet=%v PortSet=%v", cfg.BindSet, cfg.PortSet)
	}

	// 写了默认值本身（5000）也算显式 —— 这正是需要标记的原因。
	cfg, err = parse(t, "-log-format", "none", "--port", "5000", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PortSet {
		t.Error("显式写了 --port 5000 却没标 PortSet")
	}
	if cfg.BindSet {
		t.Error("只写了 --port，不该标 BindSet")
	}

	// 别名与 --bind 都要认。
	cfg, err = parse(t, "-log-format", "none", "-p", "6000", "--bind", "127.0.0.1", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PortSet || !cfg.BindSet {
		t.Errorf("短选项没被认出来：BindSet=%v PortSet=%v", cfg.BindSet, cfg.PortSet)
	}
}

// TestBindSetFromEnv 确认环境变量也算显式指定。
func TestBindSetFromEnv(t *testing.T) {
	t.Setenv("GOFS_BIND", "127.0.0.1")
	t.Setenv("GOFS_PORT", "5000") // 与默认值相同也要算
	cfg, err := parse(t, "-log-format", "none", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BindSet || !cfg.PortSet {
		t.Errorf("环境变量没被认成显式：BindSet=%v PortSet=%v", cfg.BindSet, cfg.PortSet)
	}
	if cfg.Bind != "127.0.0.1" {
		t.Errorf("GOFS_BIND 没生效：%q", cfg.Bind)
	}
}

// TestNormalizeListenErrorMentionsPortField 确认错误文案把用户指回正确的框。
func TestNormalizeListenErrorMentionsPortField(t *testing.T) {
	_, _, err := NormalizeListen("127.0.0.1:9000", 5000)
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "监听端口") {
		t.Errorf("错误文案没有指向「监听端口」字段：%v", err)
	}
}
