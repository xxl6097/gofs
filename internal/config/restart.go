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
