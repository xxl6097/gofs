package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/xxl6097/gofs"
)

// randomPassword 生成一个可安全放进 -a 规则（不含 @ : ,）的随机口令。
func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func main() {
	// 信号处理留在命令行入口：库不该替调用方决定什么时候退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := gofs.Default()
	cfg.Bind = "127.0.0.1" // 示例服务默认不暴露到所有网卡
	cfg.Port = 5000
	cfg.ServePath = "."
	cfg.AllowAll = true

	// 口令从环境变量读取；没有就每次启动随机生成并打印一次。
	// 绝不再硬编码 admin123 —— 那是一个人人皆知的默认后门。
	pass := strings.TrimSpace(os.Getenv("GOFS_ADMIN_PASSWORD"))
	if pass == "" {
		var err error
		pass, err = randomPassword()
		if err != nil {
			fmt.Fprintf(os.Stderr, "gofs: 生成管理员口令失败: %v\n", err)
			os.Exit(1)
		}
	}
	if strings.ContainsAny(pass, "@:,\n\r") {
		fmt.Fprintln(os.Stderr, "gofs: GOFS_ADMIN_PASSWORD 不能包含 @ : , 或换行")
		os.Exit(1)
	}

	// 规则里 @ 后面是 URL 路径（相对服务根目录），不是磁盘路径。
	cfg.AuthRules = []string{"admin:" + pass + "@/:rw"}

	fmt.Printf("管理员账号: admin / %s\n", pass)
	if err := gofs.RunCfg(ctx, cfg, nil, nil, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, gofs.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "gofs: %v\n", err)
		os.Exit(1)
	}
}
