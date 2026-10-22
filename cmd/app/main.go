package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/xxl6097/gofs"
)

func main() {
	// 信号处理留在命令行入口：库不该替调用方决定什么时候退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := gofs.Default()
	// 示例服务只监听回环，不暴露到所有网卡。
	cfg.Bind = "127.0.0.1"
	cfg.Port = 5000
	cfg.ServePath = "."
	cfg.AllowAll = true

	// 这是给人上手看的示例入口：口令固定为 admin / admin123，
	// 并且只监听 127.0.0.1。对外部署请用 cmd/gofs 并显式 -a 配置账号，
	// 或使用库入口 gofs.Run（每次启动随机生成口令）。
	pass := "admin123"

	// 规则里 @ 后面是 URL 路径（相对服务根目录），不是磁盘路径。
	cfg.AuthRules = []string{"admin:" + pass + "@/:rw"}

	fmt.Printf("管理员账号: admin / %s（示例口令，仅监听 127.0.0.1）\n", pass)
	if err := gofs.RunCfg(ctx, cfg, nil, nil, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, gofs.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "gofs: %v\n", err)
		os.Exit(1)
	}
}
