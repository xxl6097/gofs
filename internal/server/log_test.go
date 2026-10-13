package server

import (
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
