package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// defaultLogFormat 为未指定 --log-format 时使用的格式。
const defaultLogFormat = `$time_iso8601 $log_level - $remote_addr "$request" $status`

// Logger 输出访问日志与内部错误日志。
type Logger struct {
	mu      sync.Mutex
	out     io.Writer
	file    *os.File
	format  string
	enabled bool
}

// NewLogger 依据配置构造日志器。
// format 为空串表示关闭访问日志；file 为空串表示输出到 stdout。
func NewLogger(format, file string) (*Logger, error) {
	l := &Logger{out: os.Stdout, format: format, enabled: true}
	if format == "" {
		// 未显式指定时使用默认格式；显式传空串（GOFS_LOG_FORMAT=""）仍按默认处理，
		// 需要关闭访问日志请传 --log-format=none。
		l.format = defaultLogFormat
	}
	if strings.EqualFold(strings.TrimSpace(format), "none") {
		l.enabled = false
	}
	if file != "" {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("打开日志文件失败: %w", err)
		}
		l.file = f
		l.out = f
	}
	return l, nil
}

// Close 关闭日志文件。
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Log 记录一次请求访问。
func (l *Logger) Log(r *http.Request, status int, user string, start time.Time) {
	if l == nil || !l.enabled {
		return
	}
	line := l.render(r, status, user, start)
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(l.out, line)
}

// render 做日志格式的变量替换。
func (l *Logger) render(r *http.Request, status int, user string, start time.Time) string {
	repl := strings.NewReplacer(
		"$time_iso8601", start.Format(time.RFC3339),
		"$time_local", start.Format("2006-01-02T15:04:05-07:00"),
		"$log_level", levelFor(status),
		"$remote_addr", clientIP(r),
		"$remote_user", user,
		"$request", fmt.Sprintf("%s %s %s", r.Method, r.URL.RequestURI(), r.Proto),
		"$request_method", r.Method,
		"$request_uri", r.URL.RequestURI(),
		"$status", fmt.Sprintf("%d", status),
	)
	out := repl.Replace(l.format)
	out = replaceHTTPHeaders(out, r)
	return out
}

// replaceHTTPHeaders 展开 $http_xxx 变量。
func replaceHTTPHeaders(line string, r *http.Request) string {
	for {
		idx := strings.Index(line, "$http_")
		if idx < 0 {
			return line
		}
		end := idx + len("$http_")
		for end < len(line) && isHeaderChar(line[end]) {
			end++
		}
		key := line[idx+len("$http_") : end]
		key = strings.ReplaceAll(key, "_", "-")
		val := r.Header.Get(key)
		line = line[:idx] + val + line[end:]
	}
}

// isHeaderChar 判断字符是否可作为 header 变量名的一部分。
func isHeaderChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// levelFor 依据状态码给出日志级别。
func levelFor(status int) string {
	switch {
	case status >= 500:
		return "ERROR"
	case status >= 400:
		return "WARN"
	default:
		return "INFO"
	}
}

// clientIP 提取客户端地址（不含端口）。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-Ip"); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Infof 输出内部信息日志。
func (l *Logger) Infof(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "%s INFO  - %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// Errorf 输出内部错误日志。
func (l *Logger) Errorf(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "%s ERROR - %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
