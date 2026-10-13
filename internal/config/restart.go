package config

import "context"

// Restarter 由**调用方**实现「怎么重启自己」。
//
// 为什么做成接口而不是内置实现：怎么重启完全取决于部署方式 ——
// systemd 下常见的是 syscall.Exec 原地把进程重新执行一遍；容器里可能要
// 通知编排器（删 Pod / docker restart）；也有人只是让进程退出，交给
// supervisor 拉起来。库替调用方拍板只会错。
//
// 未通过 WithRestarter 注入时，相关接口返回「未启用」，界面上也不显示这一块。
type Restarter interface {
	// Restart 执行重启。返回 nil 表示「已经安排好了」—— 进程可能马上就退。
	//
	// ctx 由服务端带上，实现方需要自行处理超时与重试。
	//
	// 实现方负责一切。两种常见写法：
	//
	//   - 原地重执行：syscall.Exec(os.Executable(), os.Args, os.Environ())
	//     —— 注意这时 HTTP 响应多半发不出去（进程镜像已经被换掉），
	//     客户端看到的是连接中断，所以前端必须把「连接断开」也当成
	//     重启的正常表现，而不是失败。
	//   - 优雅退出后交给 supervisor：给自己发 SIGTERM。gofs 的
	//     RunCLI / Run 已经处理了它，会走完关闭流程再退出。
	Restart(ctx context.Context) error
}

// Uninstaller 是 Restarter 的**可选扩展**：实现了它，设置页就多一个「卸载」按钮。
//
// 为什么是独立接口，而不是给 Restarter 加一个方法：
//
//   - 加方法会让**所有既有实现编译不过** —— 包括本库的 gofs.RestarterFunc
//     （一个函数包不住两个动作），而 README 里推广的正是
//     gofs.WithRestarter(gofs.RestarterFunc(...)) 这个写法；
//   - 「只让页面能重启、不暴露卸载」是很常见的部署，卸载不该成为重启的代价。
//
// 类型断言天然就是这个能力的探测：`cfg.Restarter.(Uninstaller)` 成功才下发标志、
// 界面才显示按钮 —— 也就不会出现本库一直在避免的「点了必然失败」的按钮。
//
// 库自己**不删任何东西**。卸载是比升级（换掉自己的二进制）更不可逆的操作：
// 删二进制、注销 systemd 单元、清配置目录、通知编排器 …… 各家不同，
// 与 Upgrader / Restarter 同一个理由：怎么做由部署方按自己的威胁模型决定。
type Uninstaller interface {
	// Uninstall 执行卸载。返回 nil 表示「已经安排好了」—— 进程可能马上就退。
	//
	// 与 Restart 同样的契约：这个 HTTP 响应是尽力而为。实现方若删掉自己的
	// 二进制再退出，客户端多半只看到连接中断 —— 前端必须把「连接断开」当成
	// 卸载的正常表现，随后靠轮询 /fs/health **没**回来确认它确实停了。
	//
	// ctx 由服务端带上，已经脱开请求上下文（浏览器一关连接就会取消
	// r.Context()，而卸载可能要好一会儿），并带 5 分钟超时。
	Uninstall(ctx context.Context) error
}
