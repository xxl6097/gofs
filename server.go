package gofs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

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

	mu       sync.Mutex
	ln       net.Listener
	started  bool
	stopping bool
	closed   bool

	// done 在 Serve 返回后关闭，serveErr 存放其返回值。
	done     chan struct{}
	serveErr error
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

	assets, err := Assets()
	if err != nil {
		return nil, err
	}

	inner, err := server.New(cfg, assets)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:   cfg,
		inner: inner,
		done:  make(chan struct{}),
	}, nil
}

// Config 返回生效中的配置。
// 返回的是内部指针，启动之后不要再改动它。
func (s *Server) Config() *Config { return s.cfg }

// Root 返回被服务的根目录绝对路径。
func (s *Server) Root() string { return s.inner.Root() }

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

	ln, err := s.listen()
	if err != nil {
		return err
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
	}
	s.ln = ln
	s.started = true

	go s.serve(ln)
	return nil
}

// serve 在后台跑 Serve，结束后记录错误并关闭 done。
func (s *Server) serve(ln net.Listener) {
	var err error
	if s.cfg.TLSCert != "" {
		err = s.httpSrv.ServeTLS(ln, s.cfg.TLSCert, s.cfg.TLSKey)
	} else {
		err = s.httpSrv.Serve(ln)
	}
	// 主动关闭不是错误。
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	s.mu.Lock()
	s.serveErr = err
	s.mu.Unlock()
	close(s.done)
}

// listen 按配置建立监听。调用时须持有 s.mu。
func (s *Server) listen() (net.Listener, error) {
	addr := s.cfg.Addr()
	if s.cfg.IsUnixSocket() {
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
	if s.cfg.IsUnixSocket() {
		return "http+unix://" + addr.String()
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return s.Scheme() + "://" + addr.String() + s.cfg.PathPrefix
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return s.Scheme() + "://" + net.JoinHostPort(host, port) + s.cfg.PathPrefix
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
	<-s.done
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
	case <-s.done:
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
		<-s.done
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
