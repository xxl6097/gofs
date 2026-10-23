package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReplaceHTTPHeadersTerminates 钉住一个能打死服务的循环。
//
// 早先的展开是「替换一处、再从行首重扫」，而替换进去的值由客户端控制：
// 只要它含有正在展开的变量名（User-Agent: $http_user_agent 配
// --log-format '… $http_user_agent'），整行替换前后一模一样，循环永不退出。
// 日志中间件在并发闸门里面，所以这一个请求会一直占着名额 ——
// 几个匿名请求就能把服务打到拒绝服务。
//
// 用超时守一下：真回归了要**失败**，而不是把整个测试进程挂死。
func TestReplaceHTTPHeadersTerminates(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", "$http_user_agent") // 值里带正在展开的变量名
	r.Header.Set("X-Simple", "hello")

	done := make(chan string, 1)
	go func() {
		done <- replaceHTTPHeaders(
			"$remote_addr $http_user_agent $http_x_simple", r)
	}()

	select {
	case got := <-done:
		// 替换进去的 $http_user_agent 必须原样留在输出里，不再被二次展开。
		if !strings.Contains(got, "$http_user_agent") {
			t.Errorf("展开结果不对：%q", got)
		}
		if !strings.Contains(got, "hello") {
			t.Errorf("普通变量没展开：%q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replaceHTTPHeaders 没有返回 —— 又出现了展开死循环")
	}

	// 行里根本没有 $http_ 时应当原样返回（快路径）。
	if got := replaceHTTPHeaders("plain line", r); got != "plain line" {
		t.Errorf("无变量时被改写了：%q", got)
	}
}

// TestClientIPTrust 确认只有可信代理提供的 X-Forwarded-For 才会被采信，
// 且从右往左跳过可信代理，防止客户端自带的左侧地址被误当真实来源。
func TestClientIPTrust(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.9:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(req, nil); got != "192.0.2.9" {
		t.Fatalf("未配置可信代理时 clientIP = %q，期望 RemoteAddr", got)
	}

	trusted := parseTrustedProxies([]string{"192.0.2.0/24"})
	if got := clientIP(req, trusted); got != "1.2.3.4" {
		t.Fatalf("可信代理下 clientIP = %q，期望 1.2.3.4", got)
	}
	// 右侧是另一层可信代理时，应返回最右侧的**非可信**地址。
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 192.0.2.10")
	if got := clientIP(req, trusted); got != "1.2.3.4" {
		t.Fatalf("多层代理下 clientIP = %q，期望 1.2.3.4", got)
	}
}

// TestUnixSocketPeerTrust 确认 unix socket 的对端没有 IP 时，
// 只有显式配置了可信代理才会采信转发头 —— 否则 nginx 走 socket 时
// X-Real-IP / X-Forwarded-Proto 会静默失效。
func TestUnixSocketPeerTrust(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "" // unix socket 的 RemoteAddr 为空
	req.Header.Set("X-Real-Ip", "203.0.113.7")
	if got := clientIP(req, nil); got != "" {
		t.Fatalf("未配置可信代理时不应采信转发头，得到 %q", got)
	}
	trusted := parseTrustedProxies([]string{"127.0.0.1"})
	if got := clientIP(req, trusted); got != "203.0.113.7" {
		t.Fatalf("unix socket 对端 + 可信代理：clientIP = %q，期望 203.0.113.7", got)
	}
}

// TestRedactURI 确认访问日志里的凭据参数不会被原样落盘。
func TestRedactURI(t *testing.T) {
	got := redactURI("/fs/auth?user=boss&pass=secret&key=gofs_x&share=tok&x=1")
	for _, leak := range []string{"secret", "gofs_x", "tok"} {
		if strings.Contains(got, leak) {
			t.Fatalf("redactURI 泄漏了 %q：%s", leak, got)
		}
	}
	if !strings.Contains(got, "user=boss") || !strings.Contains(got, "x=1") {
		t.Fatalf("非敏感参数被误删：%s", got)
	}
}

// TestUploadKeyHeaderIsRedacted 确认自定义日志格式里的上传密钥头也会被脱敏。
func TestUploadKeyHeaderIsRedacted(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Gofs-Upload-Key", "gofs_super_secret")
	got := replaceHTTPHeaders("key=$http_x_gofs_upload_key", r)
	if strings.Contains(got, "gofs_super_secret") {
		t.Fatalf("上传密钥被写进日志：%s", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("敏感头没有被替换：%s", got)
	}
}

// TestAccessLogIgnoresUnverifiedQueryUser 确认未认证请求的 ?user= 不能伪造日志身份。
func TestAccessLogIgnoresUnverifiedQueryUser(t *testing.T) {
	root := makeFixture(t)
	s := newTestServer(t, testConfig(t, root, "-A", "--log-format", "$remote_user"))

	var buf bytes.Buffer
	s.logger.mu.Lock()
	s.logger.out = &buf
	s.logger.mu.Unlock()

	do(t, s, http.MethodGet, "/?user=admin", nil, nil)
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Fatalf("未认证请求的查询串用户名被当成了身份：%q", got)
	}
}
