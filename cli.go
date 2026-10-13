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
//
// ⚠️ 这里**不能**再额外注入一个默认账号（历史上曾有一行
// `WithAuth("admin:admin123@/:rw")`）。三个后果都很难看：
//
//   - 它是**没人知道的后门**：运维配了 `-a boss:secret@/:rw`，而
//     admin/admin123 照样能进、还是全盘读写 —— 这个口令在文档里一个字都没有；
//   - 无 `-a` 时它会**悄悄把鉴权打开**，于是 `gofs -A ./data` 要求登录，
//     与 README 写的「服务指定目录，并开放全部操作」不符；
//   - 运维若恰好也用 `admin` 这个名字（密码不同），启动会直接失败在
//     「账号 admin 出现了不一致的密码」上。
//
// 要默认账号的只有示例，以及 Run 那个「起一个服务就完事」的快捷入口 ——
// 它们把口令写在明处并打印给使用者。
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := ParseArgs(args, stdout)
	if err != nil {
		return err
	}
	if cfg.ShowVersion {
		fmt.Fprintf(stdout, "gofs %s\n", cfg.Version)
		return nil
	}

	srv, err := New(WithConfig(cfg))
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
	case <-srv.Done:
		// 服务自行停止（多半是 Serve 出错），错误由下面的 Wait 返回。
	}
	return srv.Wait()
}

func Run(ctx context.Context, stdout, stderr io.Writer) error {
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
	case <-srv.Done:
		// 服务自行停止（多半是 Serve 出错），错误由下面的 Wait 返回。
	}
	return srv.Wait()
}

func RunCfg(ctx context.Context, cfg *Config, uploader Upgrader, restarter Restarter, stdout, stderr io.Writer) error {
	srv, err := New(WithConfig(cfg),
		WithUpgrader(uploader),
		WithRestarter(restarter))
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
	case <-srv.Done:
		// 服务自行停止（多半是 Serve 出错），错误由下面的 Wait 返回。
	}
	return srv.Wait()
}

func run() {

}
