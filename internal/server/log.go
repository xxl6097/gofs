package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
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
	// trusted 是可信反向代理网段；只有直连对端落在其中时才采信转发头。
	trusted []*net.IPNet
}

// NewLogger 依据配置构造日志器。
// format 为空串表示关闭访问日志；file 为空串表示输出到 stdout。
// trusted 为空表示不信任任何转发头。
func NewLogger(format, file string, trusted []*net.IPNet) (*Logger, error) {
	l := &Logger{out: os.Stdout, format: format, enabled: true, trusted: trusted}
	if format == "" {
		// 未显式指定时使用默认格式；显式传空串（GOFS_LOG_FORMAT=""）仍按默认处理，
		// 需要关闭访问日志请传 --log-format=none。
		l.format = defaultLogFormat
	}
	if strings.EqualFold(strings.TrimSpace(format), "none") {
		l.enabled = false
	}
	if file != "" {
		// 日志里可能出现 URL、用户名等敏感信息；默认只给当前用户读写。
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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
//
// ⚠️ 变量顺序有讲究：strings.Replacer 在同一位置**按参数顺序**取第一个命中的，
// 不是最长匹配。所以所有 $request_* 都必须排在 $request 之前 —— 否则
// $request_method 会先被 $request 吃掉，渲染成 "GET /a HTTP/1.1_method"。
func (l *Logger) render(r *http.Request, status int, user string, start time.Time) string {
	// 查询串里可能带着密码、上传密钥或分享令牌；它们都是 bearer 凭据，
	// 不能原样落进访问日志。
	uri := redactURI(r.URL.RequestURI())
	repl := strings.NewReplacer(
		"$time_iso8601", start.Format(time.RFC3339),
		"$time_local", start.Format("2006-01-02T15:04:05-07:00"),
		"$log_level", levelFor(status),
		"$remote_addr", clientIP(r, l.trusted),
		"$remote_user", user,
		"$request_method", r.Method,
		"$request_uri", uri,
		// $request_time 与 nginx 同名同义：从开始处理到响应完成的秒数，保留毫秒。
		// 上传排查时它就派上用场 —— 服务端真正花了多久，和客户端观察到的时间
		// 一比，就能立刻分清「慢在服务端」还是「慢在链路/客户端」。
		"$request_time", fmt.Sprintf("%.3f", time.Since(start).Seconds()),
		// $request 必须排在最后：它是上面几个变量的前缀。
		"$request", fmt.Sprintf("%s %s %s", r.Method, uri, r.Proto),
		"$status", fmt.Sprintf("%d", status),
	)
	out := repl.Replace(l.format)
	out = replaceHTTPHeaders(out, r)
	return sanitizeLogLine(out)
}

// sensitiveQueryRe 匹配查询串里的敏感参数值。
//
// 覆盖 gofs 自己支持的所有凭据入口：?pass= 登录、?key= 上传密钥、
// ?share= 分享令牌，以及第三方客户端常用的 token/api_key 等写法。
var sensitiveQueryRe = regexp.MustCompile(
	`(?i)([?&](?:pass|password|pwd|key|token|access_token|share|secret|api_?key)=)[^&#]*`,
)

// redactURI 把 URI 查询串中的敏感值替换为 ***。
func redactURI(uri string) string {
	return sensitiveQueryRe.ReplaceAllString(uri, "${1}***")
}

// sanitizeLogLine 清理日志行里的控制字符。
//
// 日志内容有一部分来自客户端：URL、User-Agent，以及**文件名**。
// 文件名这一点容易被忽略 —— 在 Linux 上文件名可以包含换行，
// 于是 `rm -rf "a\n127.0.0.1 - - \"GET /admin\" 200"` 这样一个名字
// 就能在日志里伪造出额外的一行，把排查方向带偏。
// ANSI 转义序列（ESC）也该去掉，否则日志在终端里显示会串色。
func sanitizeLogLine(s string) string {
	// 绝大多数情况都是干净的，先做一次快速判断避免额外分配。
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\r' || c == '\n' || c == '\t':
			b.WriteByte(' ')
		case c < 0x20 || c == 0x7f:
			// 其余控制字符（含 ESC）直接丢弃。
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// replaceHTTPHeaders 展开 $http_xxx 变量。
//
// ⚠️ 必须**单趟从左到右**扫：替换进去的内容不再参与查找。
//
// 早先的写法是「替换一处、再从行首重扫」，而替换进去的值是**客户端可控**的
// —— 只要它里面含有正在展开的那个变量名，替换前后整行一个字节都不变，
// 循环就永远出不来（`User-Agent: $http_user_agent` +
// `--log-format '… $http_user_agent'` 即触发）。后果不只是占着 CPU：
// 日志中间件在并发闸门**里面**，那一个请求会一直握着名额不放，
// 几个请求就能把服务打到拒绝服务，而且不需要任何凭据。
//
// 单趟扫描天然终止：每轮至少消费掉原始行里的 `$http_` 这 6 个字节。
func replaceHTTPHeaders(line string, r *http.Request) string {
	if !strings.Contains(line, "$http_") {
		return line
	}
	var b strings.Builder
	for {
		idx := strings.Index(line, "$http_")
		if idx < 0 {
			b.WriteString(line) // 剩余部分原样落地
			break
		}
		b.WriteString(line[:idx])
		end := idx + len("$http_")
		for end < len(line) && isHeaderChar(line[end]) {
			end++
		}
		key := strings.ReplaceAll(line[idx+len("$http_"):end], "_", "-")
		value := r.Header.Get(key)
		// Authorization / Cookie 里是可直接复用的凭据，自定义日志格式
		// 即使显式写了它们，也不该把值原样落盘。
		switch strings.ToLower(key) {
		case "authorization", "proxy-authorization", "cookie":
			value = "***"
		}
		b.WriteString(value)
		// 只继续扫**原始行**剩下的部分，替换进来的值不再回头处理。
		line = line[end:]
	}
	return b.String()
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
//
// ⚠️ 转发头只在**直连对端是可信代理**时才采信。否则任何客户端都能用
// 一个伪造的 X-Forwarded-For 绕过认证限速、伪造日志里的来源地址，
// 甚至故意把某个 IP 打进封禁名单。
//
// X-Forwarded-For 从右往左找第一个非可信地址：右侧是代理追加的，
// 最左侧那一段可能是客户端自己塞进来的，不能直接信。
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	host = strings.TrimSpace(host)

	if !trustedPeer(r, trusted) {
		return host
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			s := strings.TrimSpace(parts[i])
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			if !ipInAny(ip, trusted) {
				return ip.String()
			}
		}
		// 全是可信地址：退回最左侧的合法地址（多层可信代理的场景）。
		for _, p := range parts {
			if ip := net.ParseIP(strings.TrimSpace(p)); ip != nil {
				return ip.String()
			}
		}
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-Ip")); v != "" {
		if ip := net.ParseIP(v); ip != nil {
			return ip.String()
		}
	}
	return host
}

// trustedPeer 判断直连对端是否是可信代理。
//
// 对端地址不是 IP 时（unix socket 的 RemoteAddr 为空），只有在显式配置了
// 可信代理的前提下才接受：能连上 socket 的本就只有有权访问该文件的进程，
// 转发头正是由它填进来的。
func trustedPeer(r *http.Request, trusted []*net.IPNet) bool {
	if len(trusted) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return true
	}
	return ipInAny(ip, trusted)
}

// ipInAny 判断 ip 是否落在任一网段内。
func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseTrustedProxies 把配置里的 IP / CIDR 字符串解析成网段。
// 非法项在 config.Normalize 已经拒绝，这里再静默跳过一道。
func parseTrustedProxies(vals []string) []*net.IPNet {
	var out []*net.IPNet
	for _, raw := range vals {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if ip := net.ParseIP(raw); ip != nil {
			bits := 128
			if ip = ip.To4(); ip != nil {
				bits = 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		if _, n, err := net.ParseCIDR(raw); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Infof 输出内部信息日志。
// 同样要清理控制字符：格式化参数里经常带文件路径，而路径来自客户端。
func (l *Logger) Infof(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "%s INFO  - %s\n", time.Now().Format(time.RFC3339),
		sanitizeLogLine(fmt.Sprintf(format, args...)))
}

// Errorf 输出内部错误日志。
func (l *Logger) Errorf(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "%s ERROR - %s\n", time.Now().Format(time.RFC3339),
		sanitizeLogLine(fmt.Sprintf(format, args...)))
}
