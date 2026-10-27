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
	// 示例服务只监听回环，不暴露到所有网卡。BindSet/PortSet 必须置位：
	// 否则真实的 ~/.config/gofs/settings.json 里的 listen_bind 会覆盖这里，
	// 一条曾经存在过的 0.0.0.0 记录就能把固定口令的示例服务推到公网。
	cfg.Bind = "127.0.0.1"
	cfg.BindSet = true
	cfg.Port = 5000
	cfg.PortSet = true
	cfg.ServePath = "."
	cfg.AllowAll = true
	// 示例不读取也不写入真实部署的密钥/用户/设置，避免状态混用。
	cfg.KeyFile = ""
	cfg.UserFile = ""
	cfg.SettingsFile = ""

	// 这是给人上手看的示例入口：口令固定为 gofs / gofs123，
	// 并且只监听 127.0.0.1。账号名刻意不用 admin —— 常见默认名会让
	// 定向猜口令/锁定的成本低得多。对外部署请用 cmd/gofs 并显式 -a
	// 配置账号，或使用库入口 gofs.Run（每次启动随机生成账号名与口令）。
	const user = "admin"
	pass := "admin123"

	// 规则里 @ 后面是 URL 路径（相对服务根目录），不是磁盘路径。
	cfg.AuthRules = []string{user + ":" + pass + "@/:rw"}

	fmt.Printf("管理员账号: %s / %s（示例口令，仅监听 127.0.0.1）\n", user, pass)
	if err := gofs.RunCfg(ctx, cfg, nil, nil, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, gofs.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "gofs: %v\n", err)
		os.Exit(1)
	}
}
