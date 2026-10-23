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
	publicKey := flag.String("public-key", "",
		"Ed25519 公钥（hex/base64），强制校验 checksums.txt.sig")
	allowUnsigned := flag.Bool("allow-unsigned", false,
		"显式接受无签名更新（只校验 SHA-256，不推荐；与 -public-key 二选一）")
	assetPrefix := flag.String("asset-prefix", "gofs", "资产名前缀，默认 gofs（即 gofs-<goos>-<goarch>）")
	root := flag.String("root", ".", "要服务的目录")
	port := flag.Int("port", 8000, "监听端口")
	version := flag.String("version", gofs.Version, "本程序的版本号（用于与 release 比较，须与实际构建一致）")
	dryRun := flag.Bool("dry-run", false, "只检查并下载校验，不替换自己")
	enableRestart := flag.Bool("enable-restart", false,
		"注入一个「重启」实现（给自己发 SIGTERM），页面上会出现「重启」按钮")
	flag.Parse()

	// 版本号要传给两处：升级器据此判断有没有新版，服务用它做展示。
	// 两者不一致会出现「明明最新却说有更新」。
	up, err := githubupgrade.New(githubupgrade.Options{
		Owner:          *owner,
		Repo:           *repo,
		APIBase:        *apiBase,
		AssetPrefix:    *assetPrefix,
		CurrentVersion: *version,
		PublicKeyText:  *publicKey,
		AllowUnsigned:  *allowUnsigned,
		DryRun:         *dryRun,
	})
	if err != nil {
		log.Fatalf("构造升级器失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := []gofs.Option{
		gofs.WithRoot(*root),
		gofs.WithBind("127.0.0.1"),
		gofs.WithPort(*port),
		gofs.WithVersion(*version),
		gofs.WithAllowAll(),
		gofs.WithAuth("admin:admin123@/:rw"),
		// 密钥 / 用户表 / 运行期设置都放内存，别碰开发者本机的真实配置。
		// 少了 settings 那一项的话，示例会读到（并写坏）你真实环境里存的
		// 站点标题、访问前缀、监听地址 —— 表现为「示例一启动就在 /files 下」。
		gofs.WithKeyFile(""),
		gofs.WithUserFile(""),
		gofs.WithSettingsFile(""),
		// ★ 关键的一行：注入之后页面上才会出现「检查更新」。
		gofs.WithUpgrader(up),
	}
	// 重启实现同样由上层给，而且**不注入就不显示按钮** —— 这里用条件注入
	// 演示这一点（而不是注入一个总是报错的实现，那样按钮会一直在）。
	//
	// 演示的是最常见的一种：给自己发 SIGTERM，走完优雅关闭后交给
	// systemd / supervisor / 容器编排拉起来。⚠️ 这个示例进程没有
	// supervisor，点了之后它就是真的退出了，所以默认不开。
	if *enableRestart {
		opts = append(opts, gofs.WithRestarter(gofs.RestarterFunc(
			func(context.Context) error {
				return syscall.Kill(os.Getpid(), syscall.SIGTERM)
			})))
	}

	srv, err := gofs.New(opts...)
	if err != nil {
		log.Fatalf("构造服务失败: %v", err)
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	srv.Banner(os.Stdout)
	fmt.Printf("\n打开 %s ，用 admin / admin123 登录，\n", srv.URL())
	fmt.Printf("在「设置 → 升级与重启」里点「检查更新」。%s\n\n", dryRunNote(*dryRun))

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
