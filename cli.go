package gofs

import (
	"context"
	"fmt"
	"io"
)

// RunCLI 按命令行语义运行 gofs：解析参数与环境变量、启动服务、
// 打印启动信息，并阻塞到 ctx 被取消。
//
// args 不含程序名本身（即传入 os.Args[1:]）。
// 用户请求帮助（-h / --help）时帮助文本已写入 stdout，返回 ErrHelp ——
// 调用方应当据此以 0 退出，而不是当成错误。
//
// 本函数不安装信号处理器，退出时机由 ctx 决定。
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := ParseArgs(args, stdout)
	if err != nil {
		return err
	}
	if cfg.ShowVersion {
		fmt.Fprintf(stdout, "gofs %s\n", Version)
		return nil
	}

	srv, err := New(WithConfig(cfg), WithAuth("admin:admin123@/:rw"))
	if err != nil {
		return err
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		return err
	}
	srv.Banner(stdout)

	select {
	case <-ctx.Done():
		fmt.Fprintln(stderr, "\ngofs: 正在关闭…")
		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), DefaultShutdownTimeout)
		defer cancel()
		// 关闭超时不算运行失败：连接已被强制断开，提示一句就够了。
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "gofs: %v\n", err)
		}
	case <-srv.done:
		// 服务自行停止（多半是 Serve 出错），错误由下面的 Wait 返回。
	}
	return srv.Wait()
}

func Run(ctx context.Context, stdout, stderr io.Writer) error {
	//cfg := Default()
	//cfg.Port = 5000
	//user := "admin"
	//pass := "admin123"
	//path := "."
	//cfg.AuthRules = []string{fmt.Sprintf("%s:%s@%s:rw", user, pass, path)}
	//cfg.AllowAll = true
	//cfg.Compress = "low"

	srv, err := New(WithRoot("."),
		WithPort(5000),
		WithAuth("admin:admin123@/:rw"),
		WithAllowAll(),
		WithTune(func(c *Config) { // 逃生舱：改任意字段
			c.MaxConcurrent = 128
		}),
	)
	//srv, err := New(WithConfig(cfg))
	if err != nil {
		return err
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		return err
	}
	srv.Banner(stdout)

	select {
	case <-ctx.Done():
		fmt.Fprintln(stderr, "\ngofs: 正在关闭…")
		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), DefaultShutdownTimeout)
		defer cancel()
		// 关闭超时不算运行失败：连接已被强制断开，提示一句就够了。
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "gofs: %v\n", err)
		}
	case <-srv.done:
		// 服务自行停止（多半是 Serve 出错），错误由下面的 Wait 返回。
	}
	return srv.Wait()
}

func RunCfg(ctx context.Context, cfg *Config, stdout, stderr io.Writer) error {
	srv, err := New(WithRoot(cfg.ServePath),
		WithPort(cfg.Port),
		WithAuth(cfg.AuthRules...),
		WithAllowAll(),
		WithTune(func(c *Config) { // 逃生舱：改任意字段
			c.MaxConcurrent = 128
		}))
	if err != nil {
		return err
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		return err
	}
	srv.Banner(stdout)

	select {
	case <-ctx.Done():
		fmt.Fprintln(stderr, "\ngofs: 正在关闭…")
		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), DefaultShutdownTimeout)
		defer cancel()
		// 关闭超时不算运行失败：连接已被强制断开，提示一句就够了。
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "gofs: %v\n", err)
		}
	case <-srv.done:
		// 服务自行停止（多半是 Serve 出错），错误由下面的 Wait 返回。
	}
	return srv.Wait()
}

func run() {

}
