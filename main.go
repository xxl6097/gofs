// Command gofs 是一个参照 sigoden/dufs 设计的文件服务器，
// 额外提供压缩包内容预览与在线解压能力。
//
// 用法示例：
//
//	gofs                         以只读模式服务当前目录
//	gofs -A ./data               服务 ./data 并允许所有操作
//	gofs -A --allow-extract .    开放 zip / tar / tar.gz / gz 的在线解压
//	gofs -a admin:123@/:rw -A .  启用账号密码
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/uuxia/gofs/internal/config"
	"github.com/uuxia/gofs/internal/server"
)

// assetsFS 内嵌前端资源，保证单二进制分发。
//
//go:embed all:assets
var assetsFS embed.FS

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, config.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "gofs: %v\n", err)
		os.Exit(1)
	}
}

// run 是程序主体，返回错误便于统一处理退出码。
func run(args []string) error {
	cfg, err := config.Parse(args, os.Stdout)
	if err != nil {
		return err
	}
	if cfg.ShowVersion {
		fmt.Printf("gofs %s\n", config.Version)
		return nil
	}

	// 取出 assets 子目录作为资源根。
	assets, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return fmt.Errorf("加载内嵌资源失败: %w", err)
	}

	srv, err := server.New(cfg, assets)
	if err != nil {
		return err
	}
	defer srv.Close()

	addr := cfg.Addr()
	var ln net.Listener
	if cfg.IsUnixSocket() {
		// unix socket 需要先清理残留文件。
		if _, err := os.Stat(addr); err == nil {
			if rmErr := os.Remove(addr); rmErr != nil {
				return fmt.Errorf("清理残留 socket %s 失败: %w", addr, rmErr)
			}
		}
		ln, err = net.Listen("unix", addr)
		if err != nil {
			return fmt.Errorf("监听 unix socket 失败: %w", err)
		}
		_ = os.Chmod(addr, 0o660)
	} else {
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("监听 %s 失败: %w", addr, err)
		}
	}
	defer ln.Close()

	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	printBanner(cfg, scheme, ln)

	// 服务器层面的防护：
	//   - ReadHeaderTimeout 掐断「连上后慢慢发请求头」的慢速攻击（Slowloris）。
	//     默认 15s 偏宽松，10s 足够正常客户端发完头部；
	//   - MaxHeaderBytes 收紧到 64KiB（标准库默认 1MiB），
	//     请求头是每个连接都要缓冲的，上限越小被滥用的空间越小；
	//   - 不设 ReadTimeout/WriteTimeout：上传大文件与 SSE 进度推送都需要
	//     长时间连接，全局超时会误杀它们。上传的时限改由 handler 按
	//     --upload-read-timeout 单独设置；资源占用则由并发闸门兜住。
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}

	// 捕获退出信号，做优雅关闭。
	idleClosed := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Fprintln(os.Stderr, "\ngofs: 正在关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "gofs: 关闭超时: %v\n", err)
			_ = httpSrv.Close()
		}
		close(idleClosed)
	}()

	if scheme == "https" {
		err = httpSrv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
	} else {
		err = httpSrv.Serve(ln)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleClosed
	return nil
}

// printBanner 打印启动信息，让使用者一眼看清关键配置。
func printBanner(cfg *config.Config, scheme string, ln net.Listener) {
	addr := ln.Addr().String()
	if cfg.IsUnixSocket() {
		addr = ln.Addr().String() + " (unix socket)"
	}
	root := cfg.ServePath
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}

	perm := func(name string, on bool) string {
		if on {
			return name
		}
		return "-"
	}
	fmt.Printf("gofs %s\n", config.Version)
	fmt.Printf("  监听      : %s://%s\n", scheme, addr)
	fmt.Printf("  服务目录  : %s\n", root)
	if cfg.PathPrefix != "" {
		fmt.Printf("  路径前缀  : %s\n", cfg.PathPrefix)
	}
	fmt.Printf("  权限      : upload=%s edit=%s delete=%s search=%s archive=%s extract=%s keys=%s\n",
		perm("on", cfg.AllowUpload),
		perm("on", cfg.AllowEdit),
		perm("on", cfg.AllowDelete),
		perm("on", cfg.AllowSearch),
		perm("on", cfg.AllowArchive),
		perm("on", cfg.AllowExtract),
		perm("on", cfg.AllowKeys),
	)
	if len(cfg.AuthRules) > 0 {
		fmt.Printf("  鉴权      : 已启用（%d 条规则），页面使用 Basic 认证\n", len(cfg.AuthRules))
	} else {
		fmt.Printf("  鉴权      : 未启用（任何人都可访问）\n")
	}
	if cfg.AllowKeys {
		keyLoc := cfg.KeyFile
		if keyLoc == "" {
			keyLoc = "仅内存（重启后失效）"
		}
		fmt.Printf("  上传密钥  : %s\n", keyLoc)
	}
	// 用户管理只有在「已启用鉴权」时才可用，所以这里也按同一条件打印，
	// 免得看到一个用不了的路径。
	if cfg.AllowUserManage && len(cfg.AuthRules) > 0 {
		userLoc := cfg.UserFile
		if userLoc == "" {
			userLoc = "仅内存（重启后失效）"
		}
		fmt.Printf("  用户表    : %s\n", userLoc)
	}
	// 把生效中的防护值打出来：这些默认值直接决定了服务被滥用时的表现，
	// 显式可见比藏在 --help 里更让人放心。
	fmt.Printf("  防护      : 上传≤%s  打包≤%s/%s  目录≤%s  并发%d  无进展超时%s  跨站校验%s\n",
		limitSize(cfg.UploadMaxSize),
		limitCount(cfg.ArchiveMaxItems), limitSize(cfg.ArchiveMaxBytes),
		limitCount(cfg.ListMaxEntries),
		cfg.MaxConcurrent,
		cfg.DownloadTimeout,
		perm("on", !cfg.DisableCSRFProtect),
	)
}

// limitSize 把字节上限格式化成可读文案，0 表示不限制。
func limitSize(n int64) string {
	if n <= 0 {
		return "不限"
	}
	const unit = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n)
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f%s", v, units[i])
	}
	return fmt.Sprintf("%.0f%s", v, units[i])
}

// limitCount 把条目数上限格式化成可读文案，0 表示不限制。
func limitCount(n int) string {
	if n <= 0 {
		return "不限"
	}
	return fmt.Sprintf("%d条", n)
}
