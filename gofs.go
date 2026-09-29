// Package gofs 是一个参照 sigoden/dufs 设计的文件服务器，
// 额外提供压缩包内容预览与在线解压能力。
//
// 它既可以作为独立二进制使用（见 cmd/gofs），也可以作为库嵌入其它程序：
//
//	srv, err := gofs.New(
//		gofs.WithRoot("./data"),
//		gofs.WithPort(8080),
//		gofs.WithAllowAll(),
//	)
//	if err != nil {
//		return err
//	}
//	defer srv.Close()
//	return srv.Run(ctx)
//
// 只想复用 HTTP 处理逻辑、自己管监听时，取 Handler 挂到已有的 mux 上：
//
//	mux.Handle("/files/", srv.Handler())
//
// 本包不安装信号处理器 —— 退出时机由调用方通过 ctx 决定。
package gofs

import (
	"io"
	"time"

	"github.com/xxl6097/gofs/internal/config"
)

// Config 为 gofs 的运行时配置。
//
// 它是 internal 配置类型的别名：外部调用方无法 import internal 包，
// 但可以通过这个别名正常读写字段、调用其方法。
// 字段含义见各字段的注释；通常不需要直接构造，用 Default 加 Option 即可。
type Config = config.Config

// Version 为当前版本号。
const Version = config.Version

// ErrHelp 表示调用方在参数里请求了帮助信息（-h / --help）。
// 仅 ParseArgs 与 RunCLI 会返回它，不是错误。
var ErrHelp = config.ErrHelp

// Default 返回一份只含内置默认值的配置，不读取环境变量。
//
// 默认是**只读**服务：上传、删除、编辑、打包、解压全部关闭，
// 监听 0.0.0.0:5000，服务当前目录。
func Default() *Config { return config.Defaults() }

func DefaultKeyDir() string { return config.DefaultKeyDir() }

// ParseArgs 按命令行语义解析参数与 GOFS_ 前缀的环境变量。
// args 不含程序名本身（即传入 os.Args[1:]）。
// 用户请求帮助时返回 ErrHelp，帮助文本已写入 stdout。
func ParseArgs(args []string, stdout io.Writer) (*Config, error) {
	return config.Parse(args, stdout)
}

// Option 在配置上做一次修改，由 New 依次应用。
type Option func(*Config)

// Permissions 为各项操作的开关。
//
// 与命令行不同，这里**没有**「未指定 Edit/Keys 就跟随 Upload」的隐式推断
// —— 那依赖「flag 是否被显式设置」，库里没有这个概念。
// 字段值即最终值，需要哪项就显式打开哪项。
type Permissions struct {
	// Upload 允许上传文件与目录。
	Upload bool
	// Edit 允许在线编辑文本文件。
	Edit bool
	// Delete 允许删除。
	Delete bool
	// Search 允许搜索。
	Search bool
	// Archive 允许把目录打包成 zip 下载。
	Archive bool
	// Extract 允许在线解压压缩包。
	Extract bool
	// Keys 允许在页面上管理上传密钥。
	Keys bool
	// Symlink 允许符号链接指向根目录之外。
	Symlink bool
	// UserManage 允许在页面上增删改用户（需已配置鉴权规则才生效）。
	UserManage bool
	// RootSwitch 允许登录后在页面上切换服务根目录。
	RootSwitch bool
}

// WithConfig 直接采用一份现成的配置，覆盖 Default 的全部字段。
// 常用于把 ParseArgs 的结果交给 New。
//
// 它会替换整份配置，所以要放在其它 Option 之前。
func WithConfig(cfg *Config) Option {
	return func(c *Config) {
		if cfg != nil {
			*c = *cfg
		}
	}
}

// WithRoot 设置被服务的根目录或单个文件。
func WithRoot(path string) Option {
	return func(c *Config) { c.ServePath = path }
}

// WithBind 设置监听地址，可为 IP 或 host:port。
// 传 host:port 时端口以此为准，WithPort 不再生效。
func WithBind(bind string) Option {
	return func(c *Config) { c.Bind = bind }
}

// WithPort 设置监听端口。传 0 表示由系统分配，
// 实际端口在 Start 之后通过 Addr 或 URL 获取。
func WithPort(port int) Option {
	return func(c *Config) { c.Port = port }
}

// WithUnixSocket 改为监听 unix socket。
// socket 文件会在 Start 时创建（残留文件自动清理），权限为 0660。
func WithUnixSocket(path string) Option {
	return func(c *Config) { c.Bind = path }
}

// WithPathPrefix 设置访问路径前缀，例如 "/files"。
func WithPathPrefix(prefix string) Option {
	return func(c *Config) { c.PathPrefix = prefix }
}

// WithAuth 追加鉴权规则，格式为 "user:pass@/dir1:rw,/dir2"。
// 不调用则任何人都可访问。
func WithAuth(rules ...string) Option {
	return func(c *Config) { c.AuthRules = append(c.AuthRules, rules...) }
}

// WithAllowAll 打开全部操作权限，等价于命令行的 -A。
func WithAllowAll() Option {
	return func(c *Config) { c.AllowAll = true }
}

// WithPermissions 按字段逐项设置操作权限。
func WithPermissions(p Permissions) Option {
	return func(c *Config) {
		c.AllowUpload = p.Upload
		c.AllowEdit = p.Edit
		c.AllowDelete = p.Delete
		c.AllowSearch = p.Search
		c.AllowArchive = p.Archive
		c.AllowExtract = p.Extract
		c.AllowKeys = p.Keys
		c.AllowSymlink = p.Symlink
		c.AllowUserManage = p.UserManage
		c.AllowRootSwitch = p.RootSwitch
	}
}

// WithTLS 启用 HTTPS，两个参数必须同时给出。
func WithTLS(certFile, keyFile string) Option {
	return func(c *Config) {
		c.TLSCert = certFile
		c.TLSKey = keyFile
	}
}

// WithAssetsDir 用磁盘上的目录替换内嵌前端资源，便于改前端时热更新。
func WithAssetsDir(dir string) Option {
	return func(c *Config) { c.AssetsDir = dir }
}

// WithLog 设置访问日志的格式与输出文件。file 为空表示写到标准错误。
func WithLog(format, file string) Option {
	return func(c *Config) {
		c.LogFormat = format
		c.LogFile = file
	}
}

// WithHidden 追加目录列表中要隐藏的文件名 glob。
func WithHidden(globs ...string) Option {
	return func(c *Config) { c.Hidden = append(c.Hidden, globs...) }
}

// WithKeyFile 设置上传密钥的持久化路径，空串表示只存在内存中。
func WithKeyFile(path string) Option {
	return func(c *Config) { c.KeyFile = path }
}

// WithUserFile 设置用户表的持久化路径，空串表示只存在内存中。
func WithUserFile(path string) Option {
	return func(c *Config) { c.UserFile = path }
}

// WithUploadDateLayout 设置上传自动归档的日期目录布局（Go 时间布局字面量）。
// 传空串关闭归档，文件直接落在目标目录。
func WithUploadDateLayout(layout string) Option {
	return func(c *Config) { c.UploadDateLayout = layout }
}

// WithUploadLimit 设置单次上传的字节上限与「无进展」超时。
// size 传 0 表示不限制大小。
func WithUploadLimit(size int64, idleTimeout time.Duration) Option {
	return func(c *Config) {
		c.UploadMaxSize = size
		c.UploadReadTimeout = idleTimeout
	}
}

// WithTune 是逃生舱：直接改配置里任意字段。
//
// Config 有五十多个字段，为每个都写一个 Option 只会让 API 变臃肿。
// 上面没覆盖到的项都从这里改：
//
//	gofs.WithTune(func(c *gofs.Config) { c.MaxConcurrent = 128 })
func WithTune(fn func(*Config)) Option {
	return func(c *Config) {
		if fn != nil {
			fn(c)
		}
	}
}

// buildConfig 从默认值出发依次应用 Option，并做一次校验。
func buildConfig(opts []Option) (*Config, error) {
	cfg := Default()
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}
