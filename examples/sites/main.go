// Command sites-demo 演示怎么把 gofs 配成**多站点**服务。
//
// 与 examples/upgrade、examples/restart 不同，站点不是「扩展点」——
// 它是库自带的能力，不需要实现任何接口。这个示例演示的是**怎么配**：
//
//  1. 主目录服务在 `/`，其余目录各自挂在 `/<标签>/` 下；
//  2. 按站点分权限（一个账号只读备份站，另一个只写上传站）；
//  3. 运行期能在页面上增删站点，以及怎么限定「允许挂哪些目录」。
//
// 用法：
//
//	go build -o /tmp/gofs-sites ./examples/sites
//	/tmp/gofs-sites -root ./data -site ./backup -site /srv/projects
//
//	# 打开页面用 admin / admin123 登录 →
//	# 「设置 → 常规 → 附加站点」里可以随时加/删站点
//
//	# 只想静态挂几个目录、不许页面上改：
//	/tmp/gofs-sites -no-manage ...
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/xxl6097/gofs"
)

// stringsFlag 收集可重复的选项（-site a -site b）。
type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint(*s) }
func (s *stringsFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	root := flag.String("root", ".", "主服务目录（服务在 /）")
	var sites stringsFlag
	flag.Var(&sites, "site", "附加站点目录（可重复；挂在 /<目录名>/ 下）")
	var allow stringsFlag
	flag.Var(&allow, "allow",
		"限制「页面上能新增哪些目录」的白名单前缀（可重复；不给则不限制）")
	noManage := flag.Bool("no-manage", false, "禁止在页面上增删站点")
	bind := flag.String("bind", "127.0.0.1", "监听地址")
	port := flag.Int("port", 8080, "监听端口")
	flag.Parse()

	// ★ 关键：位置参数之外的多站点入口就是 WithRoots ——
	// 第一个是主目录（服务在 /），其余自动挂在各自目录名下。
	srv, err := build(buildOptions{
		roots:   append([]string{*root}, sites...),
		allow:   allow,
		manage:  !*noManage,
		bind:    *bind,
		port:    *port,
		verbose: true,
	})
	if err != nil {
		log.Fatalf("构造服务失败: %v", err)
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Start(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	srv.Banner(os.Stdout)

	fmt.Printf("\n打开 %s ，用 admin / admin123 登录。\n", srv.URL())
	for _, m := range srv.Mounts() {
		if m.Primary {
			fmt.Printf("  主目录 %-14s →  %s\n", m.Root, m.URLPath)
			continue
		}
		fmt.Printf("  站点   %-14s →  %s/\n", m.Root, m.URLPath)
	}
	if len(srv.Mounts()) > 1 {
		fmt.Printf("  uploader / upload123 只能操作 %s/\n", srv.Mounts()[1].URLPath)
	}
	if *noManage {
		fmt.Println("\n已用 -no-manage 关闭页面上的站点增删（只剩命令行给的这几个）。")
	} else {
		fmt.Println("\n在「设置 → 常规 → 附加站点」里可以随时增删站点（立即生效并落盘）。")
	}
	fmt.Println()

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}

// buildOptions 是这个示例的配置面。抽出来是为了让测试能构造**同一套**
// 接线去验证，而不是在测试里另抄一份（抄的那份迟早和 main 走偏）。
type buildOptions struct {
	roots   []string
	allow   []string
	manage  bool
	bind    string
	port    int
	verbose bool
	// settingsFile 覆盖运行期设置的落盘位置；留空＝用库的默认路径
	// （真实部署就该用默认）。测试传临时文件，免得写进开发者自己的配置。
	settingsFile string
}

// sitePath 由站点目录推出它在权限规则里要写的路径。
//
// 与服务端分配标签的默认口径一致：取目录名。重名时服务端会加 -2/-3 后缀，
// 这里刻意不跟着算 —— 示例只覆盖常见情况，免得把标签分配那套逻辑抄第二遍
// （抄的那份迟早和 fsutil 走偏）。
func sitePath(dir string) string {
	dir = filepath.Clean(dir)
	return "/" + filepath.Base(dir)
}

// build 按示例的约定装配服务。
func build(o buildOptions) (*gofs.Server, error) {
	if len(o.roots) == 0 {
		return nil, fmt.Errorf("至少要有一个服务目录")
	}

	perms := gofs.Permissions{
		Upload:  true,
		Delete:  true,
		Search:  true,
		Archive: true,
		Extract: true,
		Edit:    true,
		Keys:    true,
		// RootSwitch 控制「能不能在页面上改主目录 / 增删站点」。
		// 关掉它就等于把站点集合钉死在启动参数上。
		RootSwitch: o.manage,
	}

	// 两个账号演示「按站点分权限」：
	//   admin    —— 整个服务读写；
	//   uploader —— 只在自己那个站点的路径下有权限。
	//
	// ⚠️ 权限规则里的路径要**带上标签**：站点挂在 /<标签>/ 下，所以
	// 「只能操作某个站点」写出来就是 user:pass@/<标签>:rw —— 这正是
	// 从单根迁到多站点时最容易漏的一处（绑在 / 上的旧规则不会覆盖站点）。
	//
	// 标签默认就是目录名，所以这里按第一个站点的目录名推导；重名时会加
	// -2/-3 后缀，那时这条规则要跟着改（示例只覆盖常见情况）。
	rules := []string{"admin:admin123@/:rw"}
	if len(o.roots) > 1 {
		rules = append(rules, "uploader:upload123@"+sitePath(o.roots[1])+":rw")
	}

	opts := []gofs.Option{
		gofs.WithRoots(o.roots...),
		gofs.WithBind(o.bind),
		gofs.WithPort(o.port),
		gofs.WithPermissions(perms),
		gofs.WithAuth(rules...),
		// 密钥与用户表放内存，别碰开发者本机的真实配置。
		gofs.WithKeyFile(""),
		gofs.WithUserFile(""),
		gofs.WithUploadDateLayout(""), // 示例里不要自动归档，免得文件跑进日期目录
	}

	if len(o.allow) > 0 {
		// 「允许在页面上新增哪些目录」挂在 Config.RootAllow 上，
		// 目前还没有专门的 Option —— 用逃生舱 WithTune 设置。
		//
		// ⚠️ 不写的话**默认不限制目录**：门槛只是「你是管理员」。
		// 想收紧就显式给白名单 —— 写了之后，页面上只能挂这些前缀下的目录。
		allow := append([]string(nil), o.allow...)
		opts = append(opts, gofs.WithTune(func(c *gofs.Config) { c.RootAllow = allow }))
	}

	if o.settingsFile != "" {
		p := o.settingsFile
		opts = append(opts, gofs.WithTune(func(c *gofs.Config) { c.SettingsFile = p }))
	}
	if !o.verbose {
		opts = append(opts, gofs.WithLog("none", ""))
	}
	return gofs.New(opts...)
}
