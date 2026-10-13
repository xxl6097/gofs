package config

import "context"

// Relistener 由**调用方**实现「把服务换到另一个监听地址上」。
//
// 为什么做成接口而不是让库自己重绑：监听套接字属于「谁 Start 谁拥有」——
// gofs 命令行入口自己 Start，所以能自己重绑；但以库方式嵌进别人的 HTTP 服务
// （Handler + 自己的 mux，或干脆挂在别人的 http.Server 上）时，那个进程里
// 根本没有「gofs 的监听」这回事，重绑该由宿主决定（换自己的监听、改
// service 配置、通知编排器都行）。
//
// 未通过 WithRelistener 注入、且**不是** gofs 自己 Start 的实例时，相关接口
// 返回「未启用」，页面上那一块也不显示 —— 免得给一个点了必然失败的按钮。
//
// 与 Upgrader / Restarter 是同一类扩展点，三者相互独立。
type Relistener interface {
	// CanRelisten 表示**当下**是否真的能重绑。
	//
	// 单独一个方法，是为了避免界面上出现「点了必然失败」的按钮：注入了实现
	// 不等于有监听 —— 以 Handler 方式嵌在别人的 http.Server 上、自己从不
	// Start 的实例也持有实现，但它没有自己的监听可换。此时返回 false，
	// 页面就不显示监听地址那一栏。
	CanRelisten() bool

	// Relisten 在 bind/port 上建立监听并开始服务，然后关掉旧监听。
	//
	// 两条硬性要求：
	//
	//   - **失败时旧监听必须仍然可用。** 实现方要先建新监听，成功了再关旧的；
	//     反过来一旦新地址建不起来（端口被占、socket 路径不可写），调用方就
	//     已经失去了旧地址，而页面往往是唯一能改回来的入口。
	//   - 返回**实际**监听地址（如 "127.0.0.1:8080"），端口传 0 时由系统分配，
	//     只有实现方拿得到真值。
	//
	// bind/port 的语义与 Config 里的同名字段一致，组合规则见 ListenAddr。
	// 已经建立的连接不受重绑影响：关掉监听只是不再接受**新**连接，
	// 正在进行的上传/下载会照常跑完。
	Relisten(ctx context.Context, bind string, port int) (string, error)
}
