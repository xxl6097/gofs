// Command upgrade-demo 演示怎么把 gofs 的在线升级接起来。
//
// 实现本身在 githubupgrade 包里（可直接 import 复用）；这个示例只做三件事：
// 解析参数 → 构造 Upgrader → 注入给 gofs。
//
// 用法：
//
//	go build -o /tmp/gofs-demo ./examples/upgrade
//	/tmp/gofs-demo -owner xxl6097 -repo gofs -root ./data -port 8080
//
//	# 只检查、只下载校验，不替换自己（推荐先这样跑一遍）
//	/tmp/gofs-demo ... -dry-run
//
//	# GitHub Enterprise
//	/tmp/gofs-demo ... -api-base https://git.corp.example/api/v3
//
// 仓库是公开的，不需要 token：检查更新走匿名的 GitHub API。
//
// 注意：**不要用 go run 跑它**。go run 会把程序编译到临时目录，
// os.Executable() 拿到的是那个临时文件，替换它没有任何意义。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/xxl6097/gofs"
	"github.com/xxl6097/gofs/githubupgrade"
)

func main() {
	owner := flag.String("owner", "xxl6097", "GitHub 账号")
	repo := flag.String("repo", "gofs", "仓库名")
	apiBase := flag.String("api-base", "", "API 根地址（GitHub Enterprise 用），默认 api.github.com")
	assetPrefix := flag.String("asset-prefix", "gofs", "资产名前缀，默认 gofs（即 gofs-<goos>-<goarch>）")
	root := flag.String("root", ".", "要服务的目录")
	port := flag.Int("port", 8000, "监听端口")
	version := flag.String("version", gofs.Version, "本程序的版本号（用于与 release 比较，须与实际构建一致）")
	dryRun := flag.Bool("dry-run", false, "只检查并下载校验，不替换自己")
	flag.Parse()

	// 版本号要传给两处：升级器据此判断有没有新版，服务用它做展示。
	// 两者不一致会出现「明明最新却说有更新」。
	up, err := githubupgrade.New(githubupgrade.Options{
		Owner:          *owner,
		Repo:           *repo,
		APIBase:        *apiBase,
		AssetPrefix:    *assetPrefix,
		CurrentVersion: *version,
		DryRun:         *dryRun,
	})
	if err != nil {
		log.Fatalf("构造升级器失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := gofs.New(
		gofs.WithRoot(*root),
		gofs.WithBind("127.0.0.1"),
		gofs.WithPort(*port),
		gofs.WithVersion(*version),
		gofs.WithAllowAll(),
		gofs.WithAuth("admin:admin123@/:rw"),
		// 密钥与用户表放内存，别碰开发者本机的真实配置。
		gofs.WithKeyFile(""),
		gofs.WithUserFile(""),
		// ★ 关键的一行：注入之后页面上才会出现「版本与升级」。
		gofs.WithUpgrader(up),
	)
	if err != nil {
		log.Fatalf("构造服务失败: %v", err)
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	srv.Banner(os.Stdout)
	fmt.Printf("\n打开 %s ，用 admin / admin123 登录，\n", srv.URL())
	fmt.Printf("在「设置 → 版本与升级」里点「检查更新」。%s\n\n", dryRunNote(*dryRun))

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}

func dryRunNote(dry bool) string {
	if dry {
		return "（当前是 -dry-run：会下载并校验，但不会替换自己）"
	}
	return "（升级会替换本程序文件，完成后需要重启进程生效）"
}
