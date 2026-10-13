package gofs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/gofs/internal/config"
	"github.com/xxl6097/gofs/internal/server"
)

// DefaultShutdownTimeout 为优雅关闭时等待在途请求的默认时长。
const DefaultShutdownTimeout = 10 * time.Second

// ErrStarted 表示服务器已经启动过，不能重复启动。
var ErrStarted = errors.New("gofs: 服务器已启动")

// Server 是一个可启停的 gofs 文件服务器实例。
//
// 零值不可用，须由 New 构造。构造之后：
//   - 只想复用 HTTP 处理逻辑 —— 取 Handler 挂到自己的 mux 上，不必调 Start；
//   - 想让它自己听端口 —— 调 Run（阻塞）或 Start（非阻塞）。
//
// 无论走哪条路，用完都要调 Close 释放日志文件与上传密钥等资源。
// 除 Handler 与 Config 外，各方法都可并发调用。
type Server struct {
	// ShutdownTimeout 为 Run 收到 ctx 取消后等待在途请求的时长。
	// 置 0 表示用 DefaultShutdownTimeout；须在 Run 之前设置。
	ShutdownTimeout time.Duration

	cfg     *Config
	inner   *server.Server
	httpSrv *http.Server

	// tlsConfig 在 Start 时把证书读一次装好，serve 之后用 ServeTLS(ln, "", "")。
	//
	// 为什么不直接把文件名传给 ServeTLS：那样**每次** ServeTLS 都会重新读盘，
	// 于是热重绑时多出一条「读到一半证书没了」的失败路径 —— 而它发生在旧监听
	// 已经关掉之后，等于把服务改没了。证书在启动时读成功过，就应当一直可用。
	tlsConfig *tls.Config

	mu       sync.Mutex
	ln       net.Listener
	started  bool
	stopping bool
	closed   bool

	// Done 在 Serve 返回后关闭，serveErr 存放其返回值。
	//
	// 只有**当前**监听（s.ln）的退出才会关它：热重绑会主动关掉旧监听，
	// 那不是「服务结束」，不能据此让 RunCLI 以为该退出了。
	Done     chan struct{}
	serveErr error
	// doneOnce 兜底，保证 Done 只关一次。
	doneOnce sync.Once
}

// New 构造一个服务器实例。
//
// 配置从 Default 出发，依次应用 opts，最后做一次校验 ——
// 校验不通过（例如非法的压缩级别）会返回错误。
// 不传任何 Option 时服务当前目录，只读。
func New(opts ...Option) (*Server, error) {
	cfg, err := buildConfig(opts)
	if err != nil {
		return nil, err
	}

	// 把早期版本遗留在上一层目录（少一个 gofs/）里的文件搬到新的默认位置。
	// 只动**默认路径**、不覆盖已有文件、可重复调用（见该函数的注释）。
	// 放在这里而不是 New 之外，是为了让命令行与库调用者都能自动受益。
	if err := config.MigrateLegacyDefaults(cfg); err != nil {
		// 搬不动就报错：那种情况下使用者会看到「密钥全没了」，
		// 明确告诉他手动挪比让他自己猜好。
		return nil, err
	}

	assets, err := Assets()
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, Done: make(chan struct{})}

	inner, err := server.New(cfg, assets)
	if err != nil {
		return nil, err
	}
	s.inner = inner

	// 自己 Start 的实例自己会重绑 —— 把内置实现注入进去。
	//
	// 注入时机看着别扭（server.New 已经跑完了），但没问题：设置处理器是
	// **每请求**读 cfg.Relistener，而不是构造时缓存。宿主自己注入了就不覆盖，
	// 那种部署里监听归宿主，不归我们。
	if cfg.Relistener == nil {
		cfg.Relistener = s
	}
	return s, nil
}

// Config 返回生效中的配置。
// 返回的是内部指针，启动之后不要再改动它。
func (s *Server) Config() *Config { return s.cfg }

// Root 返回被服务的根目录绝对路径。
//
// 多根时返回第一个根 —— 多根没有「唯一根」这回事。
func (s *Server) Root() string { return s.inner.Root() }

// MountInfo 描述一个挂载点，供 banner 之类的展示用途。
type MountInfo struct {
	// Label 是展示名（主目录为目录名，附加站点即 URL 首段）。
	Label string
	// Root 是磁盘上的绝对路径。
	Root string
	// URLPath 是访问地址：主目录 "/"，附加站点 "/<标签>"。
	URLPath string
	// Primary 标记主服务目录。
	Primary bool
}

// Mounts 返回全部挂载点（[0] 是主目录）。
//
// 站点是运行期可增删的，所以每次调用都取当下的快照 —— 不要在启动时缓存它。
func (s *Server) Mounts() []MountInfo {
	ms := s.inner.Mounts()
	out := make([]MountInfo, 0, len(ms))
	for i, m := range ms {
		url := "/" + m.Label
		if i == 0 {
			url = "/"
		}
		out = append(out, MountInfo{Label: m.Label, Root: m.Res.Root(), URLPath: url, Primary: i == 0})
	}
	return out
}

// MountLabels 返回附加站点的 URL 标签（主目录服务在 /，没有前缀）。
func (s *Server) MountLabels() []string { return s.inner.MountLabels() }

// MultiRoot 表示当前是否挂了多个服务目录（此时 URL 带标签前缀）。
func (s *Server) MultiRoot() bool { return s.inner.MultiRoot() }

// Handler 返回配置好全部中间件的 http.Handler。
//
// 想把 gofs 挂在自己已有的服务器上时用它，此时不要调 Start / Run：
//
//	mux.Handle("/files/", srv.Handler())
//
// 注意挂载路径要与 WithPathPrefix 一致，否则链接会指错地方。
func (s *Server) Handler() http.Handler { return s.inner.Handler() }

// Start 建立监听并在后台开始服务，不阻塞。
// 重复调用返回 ErrStarted。启动后可用 Addr / URL 取实际监听地址。
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return ErrStarted
	}
	if s.closed {
		return errors.New("gofs: 服务器已关闭")
	}

	// 监听地址取自 inner 的运行期设置（它可能被 settings.json 覆盖），
	// 而不是 cfg.Addr() —— 否则页面上改过的地址重启后不生效。
	ln, err := s.listen(s.inner.ListenBind(), s.inner.ListenPort())
	if err != nil {
		return err
	}

	// 证书读一次装好：serve 之后不再碰文件（见 tlsConfig 的说明）。
	if s.cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("读取 TLS 证书失败: %w", err)
		}
		s.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	// 服务器层面的防护：
	//   - ReadHeaderTimeout 掐断「连上后慢慢发请求头」的慢速攻击（Slowloris）。
	//     默认 15s 偏宽松，10s 足够正常客户端发完头部；
	//   - MaxHeaderBytes 收紧到 64KiB（标准库默认 1MiB），
	//     请求头是每个连接都要缓冲的，上限越小被滥用的空间越小；
	//   - 不设 ReadTimeout/WriteTimeout：上传大文件与 SSE 进度推送都需要
	//     长时间连接，全局超时会误杀它们。上传的时限改由 handler 按
	//     --upload-read-timeout 单独设置；资源占用则由并发闸门兜住。
	s.httpSrv = &http.Server{
		Handler:           s.inner.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    s.cfg.MaxHeaderBytes,
		TLSConfig:         s.tlsConfig,
	}
	s.ln = ln
	s.started = true

	go s.serve(ln)
	return nil
}

// serve 在后台跑 Serve，结束后记录错误并关闭 Done。
//
// ⚠️ 必须区分「这个监听被热重绑换掉了」与「服务真的结束了」：
// 热重绑会主动关掉旧监听，那时 Serve 返回的是 net.ErrClosed（**不是**
// http.ErrServerClosed）。若照旧在这里关 Done，RunCLI 的
// `select { <-ctx.Done / <-srv.Done }` 会立刻走 Wait() 返回 —— 一次改端口
// 就等于让进程自己退出。判据是「它还是不是当前监听」。
func (s *Server) serve(ln net.Listener) {
	var err error
	if s.tlsConfig != nil {
		err = s.httpSrv.ServeTLS(ln, "", "") // 证书已在 Start 时装进 TLSConfig
	} else {
		err = s.httpSrv.Serve(ln)
	}
	// 主动关闭不是错误。
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	s.mu.Lock()
	if s.ln != ln {
		// 已被热重绑换下：这是预期内的退出，不当成服务结束。
		s.mu.Unlock()
		return
	}
	s.serveErr = err
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.Done) })
}

// listen 在指定的 bind/port 上建立监听。调用时须持有 s.mu。
//
// 显式收参数而不是读 cfg：热重绑用的是页面上刚改的值，而 cfg 启动后不可写。
func (s *Server) listen(bind string, port int) (net.Listener, error) {
	addr := config.ListenAddr(bind, port)
	if strings.HasPrefix(bind, "/") {
		// unix socket 需要先清理残留文件。
		if _, err := os.Stat(addr); err == nil {
			if rmErr := os.Remove(addr); rmErr != nil {
				return nil, fmt.Errorf("清理残留 socket %s 失败: %w", addr, rmErr)
			}
		}
		ln, err := net.Listen("unix", addr)
		if err != nil {
			return nil, fmt.Errorf("监听 unix socket 失败: %w", err)
		}
		_ = os.Chmod(addr, 0o660)
		return ln, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	return ln, nil
}

// CanRelisten 实现 config.Relistener：只有真的在监听（Start 过且没关）才为 true。
func (s *Server) CanRelisten() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ln != nil && !s.closed && !s.stopping
}

// Relisten 实现 config.Relistener：把服务热重绑到新的监听地址上。
//
// 顺序是刻意的：**先建新监听，成功了再关旧的**。反过来一旦新地址建不起来
// （端口被占、socket 路径不可写），调用方就已经失去旧地址了 —— 而页面
// （或那条 curl）往往是唯一能改回来的入口。
//
// 只关监听、不断连接：已经 accept 的连接由各自的 goroutine 继续处理，
// 正在跑的上传/下载会照常完成，只有**新**连接会走新端口。
func (s *Server) Relisten(ctx context.Context, bind string, port int) (string, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return "", errors.New("本服务没有自己监听端口（未调用 Start，或以库方式嵌在别的 HTTP 服务里）")
	}
	if s.closed || s.stopping {
		return "", errors.New("服务正在关闭，无法切换监听地址")
	}

	old := s.ln

	// 地址没变就直接返回 —— 对 unix socket 来说这不是「省点事」，而是**必须**：
	//
	// Go 的 UnixListener.Close() 会把 socket 路径 unlink 掉，而此刻那个路径
	// 已经属于**新**监听 —— 于是旧监听顺手把新监听的 socket 文件删了，
	// 之后所有新连接都是 ENOENT，只有重启能救。
	//
	// 这条路径并不罕见：页面上「只改端口」就会走到这里。unix socket 下端口是
	// 无效字段（界面提示里也这么写了），但界面允许改，而 ListenAddr 对 unix
	// 只认路径 —— 于是 bind 没变、port 变了，handler 那边的短路判断不成立。
	if config.ListenAddr(bind, port) == old.Addr().String() {
		return old.Addr().String(), nil
	}

	newLn, err := s.listen(bind, port)
	if err != nil {
		return "", err // 旧监听一个字节没动
	}
	s.ln = newLn
	go s.serve(newLn)

	_ = old.Close()
	// 旧监听是 unix socket 时它的文件已经随 Close 被 unlink 了（见上）；
	// 这里再删一次是给「Close 没删成」的平台兜底，而且是**另一个**路径
	//（同路径的分支上面已经短路返回了），不会误删新监听的 socket。
	if ua, ok := old.Addr().(*net.UnixAddr); ok && ua != nil {
		_ = os.Remove(ua.Name)
	}
	return newLn.Addr().String(), nil
}

// Addr 返回实际监听地址，未启动时返回 nil。
// 配置 Port 为 0 时用它拿系统分配的端口。
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Scheme 返回 "http" 或 "https"。
func (s *Server) Scheme() string {
	if s.cfg.TLSCert != "" {
		return "https"
	}
	return "http"
}

// URL 返回可直接访问的地址，未启动时返回空串。
//
// 监听 unix socket 时返回 "http+unix://<socket 路径>" —— 它不能直接用来发请求，
// 只是给日志和提示文案用。监听在通配地址（0.0.0.0 / ::）时会换成回环地址，
// 这样拿到的 URL 本机就能直接访问。
func (s *Server) URL() string {
	addr := s.Addr()
	if addr == nil {
		return ""
	}
	// 判 unix 看**实时监听**而不是 cfg.IsUnixSocket()：热重绑可以在 tcp 与
	// unix socket 之间来回切，cfg 里那个是启动时的值，切过之后就不准了。
	if addr.Network() == "unix" {
		return "http+unix://" + addr.String()
	}
	// 前缀经 inner 的访问器取：它可能在页面上被改掉，读 cfg 只会拿到启动值。
	prefix := s.inner.PathPrefix()
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return s.Scheme() + "://" + addr.String() + prefix
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return s.Scheme() + "://" + net.JoinHostPort(host, port) + prefix
}

// Wait 阻塞直到服务停止，返回 Serve 的错误（主动关闭时为 nil）。
// 未启动时立即返回 nil。
func (s *Server) Wait() error {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil
	}
	<-s.Done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serveErr
}

// Run 启动服务并阻塞，直到 ctx 被取消或服务自行停止。
//
// ctx 取消后会做优雅关闭：等待在途请求至多 ShutdownTimeout，
// 超时则强制断开。Run 不会释放日志、密钥等资源，调用方仍需负责 Close。
//
// 本方法不安装信号处理器 —— 想按 Ctrl-C 退出就自己包一层：
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	err := srv.Run(ctx)
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		if err := s.Start(); err != nil {
			return err
		}
	}

	select {
	case <-ctx.Done():
		timeout := s.ShutdownTimeout
		if timeout <= 0 {
			timeout = DefaultShutdownTimeout
		}
		// 用 context.WithoutCancel 起新 ctx：外层 ctx 已经取消了，
		// 直接拿它做超时控制会让 Shutdown 立刻放弃在途请求。
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			return err
		}
	case <-s.Done:
	}
	return s.Wait()
}

// Shutdown 优雅关闭：停止接受新连接，等待在途请求结束。
// ctx 到期仍未结束则强制断开所有连接。未启动时为空操作。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || s.stopping {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	srv := s.httpSrv
	s.mu.Unlock()

	if err := srv.Shutdown(ctx); err != nil {
		// 超时或 ctx 取消：放弃等待，直接断开。
		_ = srv.Close()
		return fmt.Errorf("优雅关闭未完成: %w", err)
	}
	return nil
}

// Close 停止服务并释放资源（写出上传密钥、关闭日志文件、关闭监听）。
//
// 它不等待在途请求 —— 想优雅退出请先调 Shutdown 或 Run。
// 重复调用是安全的。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	started, stopping := s.started, s.stopping
	srv, ln := s.httpSrv, s.ln
	s.mu.Unlock()

	if started && !stopping && srv != nil {
		_ = srv.Close()
	}
	if started {
		<-s.Done
	} else if ln != nil {
		_ = ln.Close()
	}
	return s.inner.Close()
}

// Serve 以给定的选项服务 root 目录，阻塞直到 ctx 被取消。
//
// 它是 New + Run + Close 的快捷方式，适合「起一个服务就完事」的场景：
//
//	err := gofs.Serve(ctx, "./data", gofs.WithPort(8080), gofs.WithAllowAll())
func Serve(ctx context.Context, root string, opts ...Option) error {
	srv, err := New(append([]Option{WithRoot(root)}, opts...)...)
	if err != nil {
		return err
	}
	defer srv.Close()
	return srv.Run(ctx)
}
