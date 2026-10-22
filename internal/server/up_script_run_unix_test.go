//go:build unix

package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试直接执行下发给用户的脚本，用 PATH 里的假 curl 观察它到底发了什么。
// 只断言模板字符串的话，`curl -f` 把 3xx 当成功这种问题照样能漏过去 ——
// 真实症状正是「脚本报成功，服务器上没有文件」。

// runUpScript 在临时目录里执行生成的脚本，并返回假 curl 记下的请求日志。
func runUpScript(t *testing.T, body string, args []string, env map[string]string) (stdout, stderr, curlLog string, err error) {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if mkErr := os.MkdirAll(binDir, 0o755); mkErr != nil {
		t.Fatalf("建假 curl 目录: %v", mkErr)
	}

	// 假 curl：把收到的参数记进日志，按 URL 协议返回预设状态码。
	// 输出格式模仿 -w '%{http_code} %{redirect_url}'。
	fakeCurl := `#!/bin/sh
url=""
probe=0
for a in "$@"; do
  [ "$a" = "-I" ] && probe=1
  url="$a"
done
printf '%s\n' "$*" >> "$CURL_LOG"
http="${FAKE_HTTP_CODE:-307}"
[ "$probe" = "1" ] && http="${FAKE_PROBE_CODE:-$http}"
case "$url" in
  https://*) printf '%s %s\n' "${FAKE_HTTPS_CODE:-201}" "https://gofs.test/" ;;
  http://*)  printf '%s %s\n' "$http" "https://gofs.test/" ;;
  *)         printf '000 \n' ;;
esac
`
	curlPath := filepath.Join(binDir, "curl")
	if writeErr := os.WriteFile(curlPath, []byte(fakeCurl), 0o755); writeErr != nil {
		t.Fatalf("写假 curl: %v", writeErr)
	}

	scriptPath := filepath.Join(dir, "up.sh")
	if writeErr := os.WriteFile(scriptPath, []byte(body), 0o755); writeErr != nil {
		t.Fatalf("写脚本: %v", writeErr)
	}

	logPath := filepath.Join(dir, "curl.log")
	if writeErr := os.WriteFile(logPath, nil, 0o644); writeErr != nil {
		t.Fatalf("建日志: %v", writeErr)
	}

	cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CURL_LOG="+logPath,
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()

	logged, _ := os.ReadFile(logPath)
	return out.String(), errBuf.String(), string(logged), err
}

// upScriptForTest 用与线上一致的替换逻辑生成脚本。
func upScriptForTest(base string) string {
	return strings.NewReplacer(
		"__KEY__", shellSingleQuote("testkey"),
		"__BASE__", shellSingleQuote(base),
		"__SCOPE__", shellSingleQuote("/docs"),
	).Replace(upScriptTpl)
}

func TestUpScriptUpgradesToHTTPSOnRedirect(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "d")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	local := filepath.Join(sub, "f.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	for _, code := range []string{"301", "302", "307", "308"} {
		t.Run("redirect "+code, func(t *testing.T) {
			stdout, stderr, log, err := runUpScript(t, upScriptForTest("http://gofs.test/docs/"),
				[]string{sub}, map[string]string{"FAKE_HTTP_CODE": code})
			if err != nil {
				t.Fatalf("脚本失败: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
			}
			if !strings.Contains(stdout, "✓ d/f.txt") {
				t.Fatalf("没有报告成功：\n%s\n%s", stdout, stderr)
			}

			lines := strings.Split(strings.TrimSpace(log), "\n")
			if len(lines) != 2 {
				t.Fatalf("期望一次探测 + 一次上传，实际 %d 次：\n%s", len(lines), log)
			}
			// 探测走明文，但**不能**带密钥。
			if !strings.Contains(lines[0], "-I") ||
				!strings.Contains(lines[0], "http://gofs.test/docs/") ||
				strings.Contains(lines[0], "X-Gofs-Upload-Key") {
				t.Errorf("探测请求不对：\n%s", lines[0])
			}
			// 上传应当直接落在 https 的**同一个路径**上，
			// 而不是跟去 Location 里的地址。
			if !strings.Contains(lines[1], "https://gofs.test/docs/") ||
				!strings.Contains(lines[1], "path=d") ||
				!strings.Contains(lines[1], "X-Gofs-Upload-Key") {
				t.Errorf("https 上传没有保留路径或密钥：\n%s", lines[1])
			}
		})
	}
}

// TestUpScriptNoUpgradeForPlainHTTP 确认纯 HTTP 部署不会被误升级。
func TestUpScriptNoUpgradeForPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	stdout, stderr, log, err := runUpScript(t, upScriptForTest("http://gofs.test/docs/"),
		[]string{local}, map[string]string{"FAKE_HTTP_CODE": "201"})
	if err != nil {
		t.Fatalf("脚本失败: %v\n%s\n%s", err, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望一次探测 + 一次上传，实际请求 %d 次", len(lines))
	}
	if !strings.Contains(lines[1], "http://gofs.test/docs/") {
		t.Errorf("明文可用时不该升级到 https：\n%s", lines[1])
	}
}

// TestUpScriptReportsRedirectFailure 确认 3xx 不再被当成成功。
func TestUpScriptReportsRedirectFailure(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	stdout, stderr, _, err := runUpScript(t, upScriptForTest("http://gofs.test/docs/"),
		[]string{local},
		map[string]string{"FAKE_HTTP_CODE": "307", "FAKE_HTTPS_CODE": "307"})
	if err == nil {
		t.Fatal("3xx 被当成了成功")
	}
	if strings.Contains(stdout, "✓") {
		t.Errorf("失败时仍然打了成功标记：\n%s", stdout)
	}
	if !strings.Contains(stderr, "跳转未完成") {
		t.Errorf("没有给出跳转失败提示：\n%s", stderr)
	}
}

// TestUpScriptReportsHTTPError 确认 4xx 会带上状态码、且不触发 https 重试。
func TestUpScriptReportsHTTPError(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	_, stderr, log, err := runUpScript(t, upScriptForTest("http://gofs.test/docs/"),
		[]string{local}, map[string]string{"FAKE_HTTP_CODE": "403"})
	if err == nil {
		t.Fatal("403 被当成了成功")
	}
	if !strings.Contains(stderr, "HTTP 403") {
		t.Errorf("失败信息里没有状态码：\n%s", stderr)
	}
	if strings.Contains(log, "https://") {
		t.Errorf("4xx 不该换 https 重试：\n%s", log)
	}
}

// TestUpScriptRetriesWhenProbeBlocked 确认 HEAD 探针失效时，
// 上传请求本身的 3xx 仍然会触发 https 兜底重试。
func TestUpScriptRetriesWhenProbeBlocked(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "d")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	local := filepath.Join(sub, "f.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	stdout, stderr, log, err := runUpScript(t, upScriptForTest("http://gofs.test/docs/"),
		[]string{sub}, map[string]string{
			"FAKE_PROBE_CODE": "405", // 反代不支持 HEAD，探测不出跳转
			"FAKE_HTTP_CODE":  "307",
			"FAKE_HTTPS_CODE": "201",
		})
	if err != nil {
		t.Fatalf("脚本失败: %v\n%s\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "✓ d/f.txt") {
		t.Fatalf("没有报告成功：\n%s\n%s", stdout, stderr)
	}

	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 3 {
		t.Fatalf("期望探测 + http + https 共三次，实际 %d 次：\n%s", len(lines), log)
	}
	if !strings.Contains(lines[2], "https://gofs.test/docs/") ||
		!strings.Contains(lines[2], "path=d") {
		t.Errorf("https 兜底重试没有保留路径：\n%s", lines[2])
	}
}
