// Command restart-demo 演示怎么把 gofs 的「重启」接起来。
//
// gofs 自己不实现「怎么重启自己」—— 它只定义接口 gofs.Restarter，
// 实现由部署方给。为什么：怎么重启完全取决于部署方式。这个示例给出
// 两种最常见的写法，都用几行就能跑通。
//
// 用法：
//
//	go build -o /tmp/gofs-restart-demo ./examples/restart
//	/tmp/gofs-restart-demo -root ./data -port 8080
//
//	# 打开 http://127.0.0.1:8080 ，用 admin / admin123 登录，
//	# 进「设置 → 升级与重启」，点「重启」。
//
//	# 另一种重启方式：退出后交给 supervisor 拉起（见下面 -mode signal 的说明）
//	/tmp/gofs-restart-demo -mode signal ...
//
// 注意：**不要用 go run 跑它**。go run 会把程序编译到临时目录，
// os.Executable() 拿到的是那个临时文件，把它重新执行一遍没有意义
// （而且那个临时文件随时会被清掉）。
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

func main() {
	root := flag.String("root", ".", "要服务的目录")
	port := flag.Int("port", 8080, "监听端口")
	mode := flag.String("mode", "exec",
		"重启方式：exec＝原地重新执行自己；signal＝给自己发 SIGTERM，交给 supervisor 拉起")
	flag.Parse()

	// ★ 关键的一行：注入之后页面上才会出现「升级与重启 → 重启」。
	// 不注入的话接口返回 404，按钮也不显示 —— 免得给一个点了必然失败的按钮。
	restarter, err := restarterFor(*mode)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := gofs.New(
		gofs.WithRoot(*root),
		gofs.WithBind("127.0.0.1"),
		gofs.WithPort(*port),
		gofs.WithAllowAll(),
		gofs.WithAuth("admin:admin123@/:rw"),
		// 密钥 / 用户表 / 运行期设置都放内存，别碰开发者本机的真实配置。
		// 少了 settings 那一项的话，示例会读到（并写坏）你真实环境里存的
		// 站点标题、访问前缀、监听地址 —— 表现为「示例一启动就在 /files 下」。
		gofs.WithKeyFile(""),
		gofs.WithUserFile(""),
		gofs.WithSettingsFile(""),
		gofs.WithRestarter(restarter),
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
	fmt.Printf("在「设置 → 重启」里点「重启」。%s\n\n", modeNote(*mode))

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}

// restarterFor 按 -mode 选出一种重启策略。
//
// 抽成函数是为了能测：main 里的分支没法单独调用，而选错策略
// （比如把 signal 写成 exec）会以「点了没反应」的形式才暴露。
func restarterFor(mode string) (gofs.Restarter, error) {
	switch mode {
	case "exec":
		return gofs.RestarterFunc(reexecSelf), nil
	case "signal":
		return gofs.RestarterFunc(signalSelf), nil
	default:
		return nil, fmt.Errorf("未知的 -mode %q，可选：exec / signal", mode)
	}
}

// execTarget 给出「原地重执行自己」要用的可执行文件与参数。
//
// 单独抽出来是为了能测：syscall.Exec 一调用，测试进程本身就被换掉了，
// 没有机会断言任何东西。
func execTarget() (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("定位当前可执行文件失败: %w", err)
	}
	// 解析软链只是为了报错信息好看：/usr/local/bin/gofs 常是指向
	// releases/0.1.0/gofs 的软链，出问题时给出真实路径更好排查。
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe, os.Args, nil
}

// reexecSelf 把当前进程**原地**换成一份新的自己。
//
// 这是最省事的一种重启：不需要 supervisor、也不需要外部脚本，
// 启动参数与环境变量原样传给新进程，因此监听端口、服务目录都不变。
//
// 三点要知道：
//
//   - **HTTP 响应多半发不出去**。execve 会把进程镜像整个换掉，
//     原来的 goroutine 不复存在，请求上下文也就没了。客户端看到的是
//     连接中断 —— gofs 前端把「连接断开」当成重启的正常表现，会转而
//     轮询 /fs/health 等服务回来，所以不必为它做什么。
//   - **监听端口能被立刻重新绑定**。Go 建的 socket 带 close-on-exec，
//     execve 时由内核关闭，不会卡在「address already in use」。
//   - **PID 不变**。execve 换的是进程镜像，不是进程本身，所以 `ps` 上看
//     还是同一个 pid —— 别拿它判断「重启没生效」。该看的是启动横幅有没有
//     再打印一次（示例实测：点一次多一行「监听」）。
//   - **会话表在内存里，重启即清空**，所以刷新后需要重新登录。
func reexecSelf(context.Context) error {
	exe, argv, err := execTarget()
	if err != nil {
		return err
	}
	// syscall.Exec 成功时不返回；返回即失败。
	return syscall.Exec(exe, argv, os.Environ())
}

// signalSelf 给自己发 SIGTERM，退出后交给 systemd / supervisor / 容器编排拉起。
//
// ⚠️ 这个示例进程没有 supervisor，所以在没有外部托管的情况下点「重启」
// 只会让服务停下来 —— 想看到重启效果就用默认的 -mode exec，
// 或者把本程序真的放进 systemd / docker 里跑。
//
// 好处是它**走的是 gofs 的优雅关闭流程**（Run 已经在监听 SIGTERM）：
// 会先停止接收新请求、关掉监听、关闭日志，然后才退出。原地重执行没有
// 这一步，等价于硬重启。
func signalSelf(context.Context) error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

func modeNote(mode string) string {
	if mode == "signal" {
		return "（当前 -mode signal：会优雅退出，需要 supervisor 才能被拉起来）"
	}
	return "（当前 -mode exec：会原地换一份新的自己，页面会自动重连）"
}
