package config

import "context"

// Release 描述一个可用的新版本。
//
// 字段刻意保持最小：gofs 不假定更新源长什么样（GitHub Releases、
// 自建接口、内网制品库都行），所以这里只承载界面需要展示与决策的东西。
type Release struct {
	// Version 为新版本号，例如 "0.2.0"。用于与当前版本比较。
	Version string `json:"version"`
	// Notes 为发布说明，纯文本或 Markdown，界面直接展示。
	Notes string `json:"notes,omitempty"`
	// PublishedAt 为发布时间，RFC3339，可空。
	PublishedAt string `json:"published_at,omitempty"`
	// PageURL 为发布页地址，供用户去看完整说明。
	PageURL string `json:"page_url,omitempty"`
	// Size 为待下载体积（字节），0 表示未知。
	Size int64 `json:"size,omitempty"`
}

// UpgradeProgress 是一次升级过程中的进度。
type UpgradeProgress struct {
	// Stage 为当前阶段：checking / downloading / verifying / replacing / done。
	// 界面按它显示不同的文案，所以取值是约定好的几个。
	Stage string `json:"stage"`
	// Loaded / Total 为已下载与总字节数，Total 为 0 表示未知。
	Loaded int64 `json:"loaded"`
	Total  int64 `json:"total"`
	// Message 为一行可读的状态或错误说明。
	Message string `json:"message,omitempty"`
}

// 升级阶段常量。界面据此选文案，所以不要随意改名。
const (
	StageChecking    = "checking"
	StageDownloading = "downloading"
	StageVerifying   = "verifying"
	StageReplacing   = "replacing"
	StageDone        = "done"
)

// Upgrader 由**调用方**实现：gofs 自己不关心更新从哪来、怎么装。
//
// 为什么做成接口而不是内置实现：
//   - 更新源五花八门（GitHub Releases、自建接口、内网制品库），
//     内置一种必然有人要改；
//   - 「下载一个二进制并替换自己」是权限极高、且不可逆的动作，
//     校验策略（摘要、签名、来源白名单）应该由部署方决定，
//     不该由库替他们拍板。
//
// 未通过 WithUpgrader 注入时，相关接口返回「未启用」，界面也不显示这一块。
type Upgrader interface {
	// Check 查询是否有可用更新。没有更新时返回 (nil, nil)；
	// 出错时返回 (nil, err)，错误信息会原样展示给管理员。
	//
	// 调用方需要自行处理超时与重试 —— ctx 由服务端带上。
	Check(ctx context.Context) (*Release, error)

	// Upgrade 执行升级。rel 是 Check 返回的那个版本。
	//
	// onProgress 可能为 nil，实现里要么判空、要么用一个空函数兜底；
	// 它会被并发调用，实现方需要保证线程安全。
	//
	// 实现方负责一切：下载、校验、替换、以及决定要不要重启进程。
	// 返回 nil 表示升级成功（服务端会提示重启）。
	Upgrade(ctx context.Context, rel *Release, onProgress func(UpgradeProgress)) error
}
