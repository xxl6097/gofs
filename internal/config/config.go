// Package config 定义 gofs 的运行时配置，并负责从命令行参数与环境变量加载。
//
// 参数风格参考 sigoden/dufs：位置参数为被服务的目录（可给多个），其余为选项。
// 所有选项均可通过 GOFS_ 前缀的环境变量设置，命令行显式传入时以命令行为准。
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// dateLayoutProbe 是校验日期布局字面量时使用的参照时刻。
var dateLayoutProbe = time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)

// DefaultKeyDir 返回默认的配置目录：<用户配置目录>/gofs。
//
// 多一层 gofs 是必要的：下面那三个文件（keys / users / settings.json）
// 名字都很通用，直接躺在 <用户配置目录> 里既不符合两个平台的惯例
// （macOS 是 Application Support/<app>/，Linux 是 ~/.config/<app>/），
// 也容易和别的软件的 settings.json 撞名。
//
// 历史：早期版本直接用了 os.UserConfigDir() 的返回值（少了 gofs 这一层）。
// 旧位置的文件由 MigrateLegacyDefaults 在启动时搬过来。
func DefaultKeyDir() string {
	base := configBaseDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "gofs")
}

// configBaseDir 返回「用户配置目录」这一层（还没有拼 gofs）。
//
// 平台惯例由 os.UserConfigDir() 决定；它在**环境变量拿不到**时会失败，
// 这时退回账号的真实家目录 + .config —— 见 homeDir 的注释，那是 systemd
// 系统级服务的常见情形。
func configBaseDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return dir
	}
	if h := homeDir(); h != "" {
		return filepath.Join(h, ".config")
	}
	return ""
}

// homeDir 返回当前账号的家目录。
//
// 先看 $HOME；拿不到就查账号数据库（/etc/passwd、Directory Services），
// 那条路不依赖环境变量。为什么需要它：**systemd 的系统级 service 默认不设
// HOME**，os.UserHomeDir() 会直接失败。放任下去 DefaultKeyDir() 返回空串，
// filepath.Join("", "keys.json") 就是**相对路径** —— 文件会静默写进服务的
// WorkingDirectory（常见是 /），而 banner 上只显示一个 `keys.json`，
// 使用者根本看不出它去了哪。
//
// ⚠️ 这条退路给的是 <家目录>/.config，在 macOS 上不是惯例位置
// （那边是 Library/Application Support）。但那只会发生在 HOME 也丢了的
// 情况下 —— 那时能有个绝对路径、且每次一致，比一个会到处飘的相对路径好。
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return ""
}

// LegacyKeyDir 返回加上 gofs 这一层之前用的目录，仅供迁移使用。
//
// 它要**精确复刻旧版的计算方式**，否则迁移会找错地方：
//   - UserConfigDir() 可用 → 就是它（少一层 gofs，可搬）；
//   - 不可用但能拿到家目录 → 旧版当年拼的是 <家>/.config/gofs（已经带
//     gofs），与新默认位置相同，于是没有东西可搬；
//   - 都不行 → 空串（旧版那时给的是相对路径，无处可查，不猜）。
func LegacyKeyDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return dir
	}
	if h := homeDir(); h != "" {
		return filepath.Join(h, ".config", "gofs")
	}
	return ""
}

// MigrateLegacyDefaults 把上一版留在旧目录里的文件搬到新的默认位置。
//
// 三条规矩：
//
//   - **只动默认路径**。三者中任何一项被显式指定过（--key-file /
//     GOFS_KEY_FILE / WithKeyFile…）就跳过该项 —— 那种情况下旧位置的文件
//     与本次启动无关，去动它属于越权。
//   - **不覆盖**。新位置已经有文件就跳过，绝不拿旧的盖掉新的。
//   - **幂等**。只在「新位置没有、旧位置有」时才搬，重复调用无副作用。
//
// 新旧目录是同一个父目录下的两层（<cfgdir> 与 <cfgdir>/gofs），所以在同一
// 文件系统上，rename 是原子的。搬不动时报错而不是默默吞掉 —— 那种情况下
// 使用者会看到「密钥全没了」，明确告诉他手动挪比让他猜好。
func MigrateLegacyDefaults(cfg *Config) error {
	legacy := LegacyKeyDir()
	newDir := DefaultKeyDir()
	if legacy == "" || newDir == "" || legacy == newDir {
		return nil
	}
	items := []struct {
		cur  string // 本次实际使用的路径
		def  string // 默认路径（只有等于它才认为「没被显式指定」）
		name string
	}{
		{cfg.KeyFile, DefaultKeyFile(), "keys.json"},
		{cfg.UserFile, DefaultUserFile(), "users.json"},
		{cfg.SettingsFile, DefaultSettingsFile(), "settings.json"},
	}
	for _, it := range items {
		if it.cur == "" || it.cur != it.def {
			continue
		}
		if _, err := os.Stat(it.cur); err == nil {
			continue // 新位置已有，不覆盖
		}
		old := filepath.Join(legacy, it.name)
		if _, err := os.Stat(old); err != nil {
			continue // 旧位置也没有，没什么可搬
		}
		if err := os.MkdirAll(filepath.Dir(it.cur), 0o700); err != nil {
			return fmt.Errorf("迁移 %s 失败（建目录）: %w", it.name, err)
		}
		if err := os.Rename(old, it.cur); err != nil {
			return fmt.Errorf("迁移 %s 失败（%s → %s）: %w", it.name, old, it.cur, err)
		}
	}
	return nil
}

// DefaultKeyFile 的返回值是上传密钥的默认持久化路径。
//
// 放在用户配置目录而不是服务目录，理由有两个：
// 一是不会污染被服务的文件树（否则密钥文件会出现在目录列表里），
// 二是不会因为换一个服务目录就丢掉已有密钥。
func DefaultKeyFile() string {
	return filepath.Join(DefaultKeyDir(), "keys.json")
}

// DefaultUserFile 的返回值是用户表的默认持久化路径。
// 与密钥文件同理放在用户配置目录：不污染被服务的文件树，也不会因为换服务目录而丢。
func DefaultUserFile() string {
	return filepath.Join(DefaultKeyDir(), "users.json")
}

// DefaultSettingsFile 的返回值是「运行期设置」的默认持久化路径。
//
// 目前只存一项：站点标题。它属于标识/品牌，重启后应该还在 ——
// 不像上传上限那种「临时放宽」，重启回到启动参数声明的值才是对的。
func DefaultSettingsFile() string {
	return filepath.Join(DefaultKeyDir(), "settings.json")
}

// DefaultTitle 是未指定时的站点标题。
const DefaultTitle = "gofs"

// 解压相关的硬性安全上限，可由命令行覆盖。
const (
	// DefaultExtractMaxTotal 单次解压允许写出的字节总量上限，默认 10 GiB。
	DefaultExtractMaxTotal int64 = 10 << 30
	// DefaultExtractMaxFiles 单次解压允许的文件条目数上限。
	DefaultExtractMaxFiles = 100000
	// DefaultExtractMaxRatio 单文件压缩比上限（防解压炸弹，100 倍）。
	DefaultExtractMaxRatio = 200
	// DefaultUploadDateLayout 为上传时默认使用的年/月/日三级目录布局。
	DefaultUploadDateLayout = "2006/01/02"
	// DefaultEditMaxSize 为在线编辑允许打开的最大文件尺寸（2 MiB）。
	DefaultEditMaxSize int64 = 2 << 20

	// ---- 服务端防护默认值 ----
	//
	// 下面这些上限的存在意义是「让服务在被恶意使用时退化而不是崩溃」：
	// 单个请求耗尽磁盘、内存或文件描述符，是文件服务器最容易被击穿的地方。

	// DefaultUploadMaxSize 单次上传允许的最大字节数（10 GiB），0 表示不限制。
	//
	// 选 10 GiB 而不是「不限制」：它足够覆盖常见的大文件场景
	// （镜像、数据库备份、视频素材），同时仍能兜住「一个请求写爆磁盘」。
	// 需要传更大的东西时显式设成 0 即可 —— 但那等于把磁盘交给调用方处置。
	DefaultUploadMaxSize int64 = 10 << 30
	// DefaultUploadReadTimeout 上传连续多久没有进展就断开（默认 2 分钟）。
	//
	// 注意是「无进展」而不是「总时长」：只要数据还在传，连接就会被不断续期，
	// 所以 10 GB 的文件在慢速链路上传三个小时也不会被打断；
	// 真正会被断开的是「连上之后几乎不发数据」的连接 ——
	// 那种连接会长期占着并发名额，几十个就能让服务拒绝正常用户。
	DefaultUploadReadTimeout = 2 * time.Minute
	// DefaultDownloadTimeout 下载连续多久没有进展就断开（默认 2 分钟）。
	//
	// 同样按进展计算，防的是「连上之后不读数据」的客户端：
	// 服务端会一直阻塞在写 socket 上，却始终占着并发名额。
	DefaultDownloadTimeout = 2 * time.Minute
	// DefaultListMaxEntries 单次目录列举最多返回的条目数。
	// 十万级文件的目录一次性序列化会给内存和响应体都带来很大压力。
	DefaultListMaxEntries = 20000
	// DefaultArchiveMaxItems 单次打包允许的最大条目数。
	DefaultArchiveMaxItems = 200000
	// DefaultArchiveMaxBytes 单次打包允许的最大原始字节数（50 GiB）。
	DefaultArchiveMaxBytes int64 = 50 << 30
	// DefaultMaxConcurrent 同时在处理的请求数上限。
	// 目的是兜住文件描述符与内存，而不是限流业务。
	DefaultMaxConcurrent = 512
	// DefaultMaxConcurrentJobs 同时进行的重任务（上传 / 解压）数上限。
	DefaultMaxConcurrentJobs = 64
	// DefaultAuthFailLimit 同一来源在窗口内允许的认证失败次数，0 表示不限制。
	DefaultAuthFailLimit = 10
	// DefaultAuthFailWindow 认证失败的统计窗口，同时也是封禁时长。
	DefaultAuthFailWindow = 5 * time.Minute
	// DefaultMaxHeaderBytes 请求行 + 请求头允许的最大字节数（默认 64 KiB）。
	DefaultMaxHeaderBytes = 64 << 10
	// DefaultMaxNameBytes 允许的单个文件名最大字节数。
	// 多数文件系统的上限是 255 字节，提前拒绝比让底层报错更友好。
	DefaultMaxNameBytes = 255
	// DefaultHashMaxSize 允许计算摘要（?hash）的最大文件尺寸。
	// 算摘要要读完整个文件，对超大文件开放等于送出一个 CPU/IO 耗尽入口。
	DefaultHashMaxSize int64 = 512 << 20
)

// Config 保存完整的服务配置。
type Config struct {
	// ServePath 为被服务的根目录或单个文件。**等于 ServePaths[0]**，
	// 保留它是为了兼容只认「一个根」的老代码与老测试。
	ServePath string

	// ServePaths 为全部被服务的根目录（含 ServePath）。
	//
	// 长度 1 时行为与只有 ServePath 时完全一样：服务在 `/`，URL 里没有前缀。
	// 长度 >= 2 时每个根挂在各自的标签下（`/data/…`、`/backup/…`）。
	// Normalize 之后长度恒 >= 1，且 ServePaths[0] == ServePath。
	ServePaths []string
	// Bind 为监听地址，可为 IP、host:port，或以 / 开头的 unix socket 路径。
	Bind string
	// Port 为监听端口。
	Port int
	// BindSet / PortSet 表示监听地址/端口是**显式指定**的（命令行、环境变量
	// 或 WithBind/WithPort/WithUnixSocket），而不是取的内置默认值。
	//
	// 为什么要单独记：监听地址是运行期可在页面上改的，而「改错了怎么回来」
	// 只有一条路 —— 用启动参数重启一次。所以启动参数必须能压过 settings.json
	// 里的记录（与 PathPrefix 同一个道理）。而 Port 有默认值 5000，
	// 光看「值是不是非零」分不出「显式给了 5000」与「默认就是 5000」，
	// 只能由解析阶段把「用户到底写没写」这件事记下来。
	BindSet bool
	PortSet bool
	// PathPrefix 为访问路径前缀，例如 /dufs。
	PathPrefix string
	// Hidden 为目录列表中隐藏的文件名 glob（只匹配文件名，不匹配路径）。
	Hidden []string

	// AuthRules 为原始鉴权规则串，交由 auth 包解析。
	AuthRules []string

	// 权限开关。
	// AllowAll 打开全部开关。
	AllowAll bool

	// AllowEdit 表示允许在线编辑文本文件。
	// 未显式指定时跟随 AllowUpload（能上传即能改文件），可由 --allow-edit 单独控制。
	AllowEdit bool

	// EditMaxSize 为在线编辑允许打开的最大文件尺寸，超过则只能下载。
	EditMaxSize int64

	// AllowKeys 表示允许在页面上管理上传密钥。
	AllowKeys bool
	// KeyFile 为上传密钥的持久化路径；为空表示只保存在内存中，重启即失效。
	KeyFile string
	// AllowUserManage 表示允许在页面上增删改用户（默认开启，需已有鉴权规则）。
	AllowUserManage bool
	// UserFile 为用户表的持久化路径；为空表示只保存在内存中，重启即失效。
	UserFile     string
	AllowUpload  bool
	AllowDelete  bool
	AllowSearch  bool
	AllowArchive bool // 允许把目录打包成 zip 下载
	AllowExtract bool // 允许在线解压压缩包
	AllowSymlink bool // 允许符号链接指向根目录之外

	// 渲染模式。
	EnableCORS     bool
	RenderIndex    bool
	RenderTryIndex bool
	RenderSPA      bool

	// UnsafeIndexHTML 允许在**可写**部署里以同源方式执行服务目录中的
	// index.html（默认关闭）。
	//
	// 上传/在线编辑/解压任一开启时，拥有写权限的账号可以放置或覆盖
	// index.html；若它被当作同源页面执行，脚本就能读取登录凭据、调用所有
	// 接口。默认对这些 index.html 加 CSP sandbox（脚本可运行，但不透明源），
	// 需要真正的同源应用时显式打开本开关并自行承担风险。
	UnsafeIndexHTML bool

	// AssetsDir 为自定义前端资源目录，为空时使用内嵌资源。
	AssetsDir string

	// 日志。
	LogFormat string
	LogFile   string

	// Compress 为目录打包时的压缩级别：none / low / medium / high。
	Compress string

	// UploadDateLayout 为上传时自动归档的日期目录布局，写法是 Go 的时间布局字面量。
	// 默认 "2006/01/02"，即在上传目标目录下按 年/月/日 建三级目录存放文件；
	// 置为空串表示关闭归档，文件直接落在目标目录。
	UploadDateLayout string

	// TLS。
	TLSCert string
	TLSKey  string

	// 在线解压的安全上限。
	ExtractMaxTotal int64
	ExtractMaxFiles int
	ExtractMaxRatio int

	// ---- 服务端防护 ----

	// UploadMaxSize 为单次上传允许的最大字节数，0 表示不限制。
	UploadMaxSize int64
	// UploadReadTimeout 为单次上传「无进展」多久后断开。
	UploadReadTimeout time.Duration
	// DownloadTimeout 为单次下载「无进展」多久后断开；0 表示不限制。
	DownloadTimeout time.Duration
	// ListMaxEntries 为单次目录列举返回的最大条目数。
	ListMaxEntries int
	// ArchiveMaxItems / ArchiveMaxBytes 为单次打包的条目数与字节数上限。
	ArchiveMaxItems int
	ArchiveMaxBytes int64
	// MaxConcurrent 为同时处理的请求数上限（0 表示不限制）。
	MaxConcurrent int
	// MaxConcurrentJobs 为同时进行的重任务（上传 / 解压）数上限。
	MaxConcurrentJobs int
	// AuthFailLimit 为同一来源在窗口内允许的认证失败次数（0 表示不限制）。
	AuthFailLimit int
	// AuthFailWindow 为认证失败的统计窗口与封禁时长。
	AuthFailWindow time.Duration
	// TrustedProxies 是允许提供 X-Forwarded-For / X-Real-IP /
	// X-Forwarded-Proto 的反向代理地址（IP 或 CIDR）。
	//
	// 默认为空：此时只认 r.RemoteAddr，客户端伪造转发头没有意义。
	// 部署在反向代理之后时必须显式配置，否则限速与访问日志看到的
	// 都会是代理的地址。
	TrustedProxies []string
	// MaxHeaderBytes 为请求行 + 请求头允许的最大字节数。
	MaxHeaderBytes int
	// MaxNameBytes 为允许的单个文件名最大字节数。
	MaxNameBytes int
	// DisableCSRFProtect 关闭写操作的来源校验（默认开启）。
	//
	// 开着它时，带 Origin/Referer 且与请求 Host 不同源的写请求会被拒绝。
	// 非浏览器客户端（curl、脚本）通常不带这两个头，不受影响。
	DisableCSRFProtect bool

	// HashMaxSize 为允许计算 ?hash 摘要的最大文件尺寸（0 表示不限制）。
	HashMaxSize int64

	// DisableHTMLSandbox 关闭对 HTML/SVG/XML 的 CSP sandbox。
	//
	// 默认开启：上传的网页会在不透明源里渲染，脚本读不到本站数据，
	// 从而堵死「上传恶意页面 → 诱导管理员打开 → 借其登录态操作」这条路径。
	DisableHTMLSandbox bool

	// ---- 运行期可修改的设置 ----
	//
	// AllowRootSwitch 允许登录后在页面上切换服务根目录（默认开启）。
	//
	// 只对服务根拥有读写权限的账号可以操作；可切换的范围默认限制在
	// 「启动根目录的父目录」之内（能换到兄弟目录，但换不到系统的别处），
	// 需要放开时用 --root-allow 追加允许的前缀。
	AllowRootSwitch bool
	// RootAllow 为允许切换到的额外路径前缀。
	RootAllow []string

	// AllowAnonymousAdmin 允许在**未启用鉴权**时把 HTTP 管理接口的调用者
	// 当作管理员。
	//
	// 默认关闭。未启用鉴权意味着所有能连上端口的人都能访问，而管理接口
	// （/fs/settings、/fs/keys、/fs/upgrade、/fs/restart、/fs/uninstall）
	// 能挂载任意目录、切换服务根、改监听地址 —— 这些已经超出了「被服务的
	// 那棵目录」，不能因为没配账号就默认交给所有人。需要这种部署时要显式
	// 打开此开关（--allow-anonymous-admin / GOFS_ALLOW_ANONYMOUS_ADMIN）。
	AllowAnonymousAdmin bool

	// Version 为本服务对外报告的版本号，用于页面标题、健康检查与升级比较。
	//
	// 默认取 DefaultVersion（0.1.0）。上层嵌入时可以设成自己的版本，
	// 例如 gofs.WithVersion("1.4.2") 或直接写 cfg.Version = "1.4.2"。
	Version string

	// Title 为站点标题，显示在浏览器标签页与顶栏品牌名上。默认 "gofs"。
	//
	// 这里给的是**初始默认值**：管理员在页面上改过之后会落到 SettingsFile，
	// 之后以文件为准（改一次就生效、且重启还在）。想改回默认就把文件删掉，
	// 或者把 --title 换成新值再用一次「恢复默认」。
	Title string

	// SettingsFile 为运行期设置的持久化路径；为空表示只保存在内存中，
	// 重启即回到 Title 的启动值。
	SettingsFile string

	// ShowVersion 为 true 时打印版本并退出。
	ShowVersion bool

	// Upgrader 为「检查更新 / 执行升级」的实现，由调用方注入。
	//
	// 为 nil 表示这个部署不提供在线升级：相关接口返回「未启用」，
	// 页面上也不显示那一块。库自己不内置任何实现 —— 更新源与校验策略
	// 应该由部署方决定（见 Upgrader 的注释）。
	Upgrader Upgrader

	// Restarter 为「重启服务」的实现，由调用方注入。
	//
	// 为 nil 表示这个部署不提供重启：相关接口返回「未启用」，界面上也不
	// 显示「重启」按钮。与 Upgrader 相互独立 —— 只升级不重启、或只重启
	// 不升级的部署都很常见（见 Restarter 的注释）。
	Restarter Restarter

	// Uninstaller 为「卸载服务」的实现，由调用方注入。
	//
	// 与 Restarter **相互独立**：只重启不卸载、只卸载不重启都常见。
	// 为 nil 表示这个部署不提供卸载：/fs/uninstall 返回「未启用」，
	// 界面上也不显示「卸载」按钮。
	Uninstaller Uninstaller

	// Relistener 为「把服务换到另一个监听地址上」的实现（热重绑）。
	//
	// gofs 自己 Start 时会自动注入内置实现（见根包 New）；以库方式嵌进
	// 别人的 HTTP 服务时由宿主注入。为 nil 表示这个部署不提供运行期改
	// 监听地址：相关字段不下发，页面上那一栏也不显示。
	Relistener Relistener
}

// DefaultVersion 是未显式配置时的版本号。
//
// 是 var 而不是 const，便于构建时注入：
//
//	go build -ldflags "-X github.com/xxl6097/gofs/internal/config.DefaultVersion=1.2.3"
var DefaultVersion = "0.1.0"

// ErrHelp 表示用户主动请求了帮助信息。
var ErrHelp = errors.New("help requested")

// envBool 读取布尔型环境变量。
func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	case "0", "false", "no", "off", "n":
		return false
	}
	return def
}

// envStr 读取字符串型环境变量。
func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envInt 读取整型环境变量，非法值回落到默认值。
func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// envInt64 读取 64 位整型环境变量，非法值回落到默认值。
func envInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return def
}

// envDuration 读取时长型环境变量（如 30m、1h），非法值回落到默认值。
func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

// splitList 按逗号切分环境变量列表，忽略空项。
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitAuth 按 | 切分环境变量里的多条鉴权规则。
// 之所以不用逗号，是因为规则本身就以逗号分隔路径。
func splitAuth(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envList 读取逗号分隔的列表型环境变量；变量未设置时保留默认值。
func envList(key string, def []string) []string {
	if v, ok := os.LookupEnv(key); ok {
		return splitList(v)
	}
	return def
}

// envAuthList 读取 | 分隔的鉴权规则环境变量；变量未设置时保留默认值。
func envAuthList(key string, def []string) []string {
	if v, ok := os.LookupEnv(key); ok {
		return splitAuth(v)
	}
	return def
}

// DefaultPort 为未指定 --port 时的监听端口。
const DefaultPort = 5000

// Defaults 返回一份只含内置默认值的配置，不读取任何环境变量。
//
// 供以库方式嵌入时作为起点：调用方在其上改字段，再调 Normalize 校验。
// 命令行入口走 Parse —— 它在本函数之上叠加环境变量与命令行参数。
func Defaults() *Config {
	return &Config{
		ServePath:        ".",
		Bind:             "",
		Port:             DefaultPort,
		PathPrefix:       "",
		Hidden:           nil,
		AuthRules:        nil,
		AllowAll:         false,
		AllowEdit:        false,
		EditMaxSize:      DefaultEditMaxSize,
		AllowKeys:        false,
		KeyFile:          DefaultKeyFile(),
		AllowUserManage:  true,
		UserFile:         DefaultUserFile(),
		AllowUpload:      false,
		AllowDelete:      false,
		AllowSearch:      false,
		AllowArchive:     false,
		AllowExtract:     false,
		AllowSymlink:     false,
		EnableCORS:       false,
		RenderIndex:      false,
		RenderTryIndex:   false,
		RenderSPA:        false,
		UnsafeIndexHTML:  false,
		AssetsDir:        "",
		LogFormat:        "",
		LogFile:          "",
		Compress:         "low",
		UploadDateLayout: DefaultUploadDateLayout,
		TLSCert:          "",
		TLSKey:           "",
		ExtractMaxTotal:  DefaultExtractMaxTotal,
		ExtractMaxFiles:  DefaultExtractMaxFiles,
		ExtractMaxRatio:  DefaultExtractMaxRatio,

		UploadMaxSize:      DefaultUploadMaxSize,
		UploadReadTimeout:  DefaultUploadReadTimeout,
		DownloadTimeout:    DefaultDownloadTimeout,
		ListMaxEntries:     DefaultListMaxEntries,
		ArchiveMaxItems:    DefaultArchiveMaxItems,
		ArchiveMaxBytes:    DefaultArchiveMaxBytes,
		MaxConcurrent:      DefaultMaxConcurrent,
		MaxConcurrentJobs:  DefaultMaxConcurrentJobs,
		AuthFailLimit:      DefaultAuthFailLimit,
		AuthFailWindow:     DefaultAuthFailWindow,
		MaxHeaderBytes:     DefaultMaxHeaderBytes,
		MaxNameBytes:       DefaultMaxNameBytes,
		DisableCSRFProtect: false,
		HashMaxSize:        DefaultHashMaxSize,
		DisableHTMLSandbox: false,
		AllowRootSwitch:    true,
		RootAllow:          nil,
		// 未启用鉴权时管理接口默认关闭：无鉴权只读/上传仍是常见用法，
		// 但不该顺带把「挂载任意目录、改监听、换根」交给所有人。
		AllowAnonymousAdmin: false,

		Version:      DefaultVersion,
		Title:        DefaultTitle,
		SettingsFile: DefaultSettingsFile(),
	}
}

// ApplyEnv 用 GOFS_ 前缀的环境变量覆盖配置。
// 只有真正设置了的变量才会生效，未设置的字段保持原值。
func (c *Config) ApplyEnv() {
	c.Version = envStr("GOFS_VERSION", c.Version)
	c.Title = envStr("GOFS_TITLE", c.Title)
	c.SettingsFile = envStr("GOFS_SETTINGS_FILE", c.SettingsFile)
	c.ServePath = envStr("GOFS_SERVE_PATH", c.ServePath)
	c.Bind = envStr("GOFS_BIND", c.Bind)
	c.Port = envInt("GOFS_PORT", c.Port)
	// 环境变量也算「显式指定」—— 但只有真的设过才算，否则 GOFS_PORT 一被
	// 默认值填上就会把页面上的改动静默压回去。
	if _, ok := os.LookupEnv("GOFS_BIND"); ok {
		c.BindSet = true
	}
	if _, ok := os.LookupEnv("GOFS_PORT"); ok {
		c.PortSet = true
	}
	c.PathPrefix = envStr("GOFS_PATH_PREFIX", c.PathPrefix)
	c.Hidden = envList("GOFS_HIDDEN", c.Hidden)
	c.AuthRules = envAuthList("GOFS_AUTH", c.AuthRules)
	c.AllowAll = envBool("GOFS_ALLOW_ALL", c.AllowAll)
	c.AllowEdit = envBool("GOFS_ALLOW_EDIT", c.AllowEdit)
	c.EditMaxSize = envInt64("GOFS_EDIT_MAX_SIZE", c.EditMaxSize)
	c.AllowKeys = envBool("GOFS_ALLOW_KEYS", c.AllowKeys)
	c.KeyFile = envStr("GOFS_KEY_FILE", c.KeyFile)
	c.AllowUserManage = envBool("GOFS_ALLOW_USER_MANAGE", c.AllowUserManage)
	c.UserFile = envStr("GOFS_USER_FILE", c.UserFile)
	c.AllowUpload = envBool("GOFS_ALLOW_UPLOAD", c.AllowUpload)
	c.AllowDelete = envBool("GOFS_ALLOW_DELETE", c.AllowDelete)
	c.AllowSearch = envBool("GOFS_ALLOW_SEARCH", c.AllowSearch)
	c.AllowArchive = envBool("GOFS_ALLOW_ARCHIVE", c.AllowArchive)
	c.AllowExtract = envBool("GOFS_ALLOW_EXTRACT", c.AllowExtract)
	c.AllowSymlink = envBool("GOFS_ALLOW_SYMLINK", c.AllowSymlink)
	c.EnableCORS = envBool("GOFS_ENABLE_CORS", c.EnableCORS)
	c.RenderIndex = envBool("GOFS_RENDER_INDEX", c.RenderIndex)
	c.RenderTryIndex = envBool("GOFS_RENDER_TRY_INDEX", c.RenderTryIndex)
	c.RenderSPA = envBool("GOFS_RENDER_SPA", c.RenderSPA)
	c.UnsafeIndexHTML = envBool("GOFS_UNSAFE_INDEX_HTML", c.UnsafeIndexHTML)
	c.AssetsDir = envStr("GOFS_ASSETS", c.AssetsDir)
	c.LogFormat = envStr("GOFS_LOG_FORMAT", c.LogFormat)
	c.LogFile = envStr("GOFS_LOG_FILE", c.LogFile)
	c.Compress = envStr("GOFS_COMPRESS", c.Compress)
	c.UploadDateLayout = envStr("GOFS_UPLOAD_DATE_LAYOUT", c.UploadDateLayout)
	c.TLSCert = envStr("GOFS_TLS_CERT", c.TLSCert)
	c.TLSKey = envStr("GOFS_TLS_KEY", c.TLSKey)
	c.ExtractMaxTotal = envInt64("GOFS_EXTRACT_MAX_TOTAL", c.ExtractMaxTotal)
	c.ExtractMaxFiles = envInt("GOFS_EXTRACT_MAX_FILES", c.ExtractMaxFiles)
	c.ExtractMaxRatio = envInt("GOFS_EXTRACT_MAX_RATIO", c.ExtractMaxRatio)

	c.UploadMaxSize = envInt64("GOFS_UPLOAD_MAX_SIZE", c.UploadMaxSize)
	c.UploadReadTimeout = envDuration("GOFS_UPLOAD_READ_TIMEOUT", c.UploadReadTimeout)
	c.DownloadTimeout = envDuration("GOFS_DOWNLOAD_TIMEOUT", c.DownloadTimeout)
	c.ListMaxEntries = envInt("GOFS_LIST_MAX_ENTRIES", c.ListMaxEntries)
	c.ArchiveMaxItems = envInt("GOFS_ARCHIVE_MAX_ITEMS", c.ArchiveMaxItems)
	c.ArchiveMaxBytes = envInt64("GOFS_ARCHIVE_MAX_BYTES", c.ArchiveMaxBytes)
	c.MaxConcurrent = envInt("GOFS_MAX_CONCURRENT", c.MaxConcurrent)
	c.MaxConcurrentJobs = envInt("GOFS_MAX_CONCURRENT_JOBS", c.MaxConcurrentJobs)
	c.AuthFailLimit = envInt("GOFS_AUTH_FAIL_LIMIT", c.AuthFailLimit)
	c.AuthFailWindow = envDuration("GOFS_AUTH_FAIL_WINDOW", c.AuthFailWindow)
	c.TrustedProxies = envList("GOFS_TRUSTED_PROXIES", c.TrustedProxies)
	c.MaxHeaderBytes = envInt("GOFS_MAX_HEADER_BYTES", c.MaxHeaderBytes)
	c.MaxNameBytes = envInt("GOFS_MAX_NAME_BYTES", c.MaxNameBytes)
	c.DisableCSRFProtect = envBool("GOFS_DISABLE_CSRF_PROTECT", c.DisableCSRFProtect)
	c.HashMaxSize = envInt64("GOFS_HASH_MAX_SIZE", c.HashMaxSize)
	c.DisableHTMLSandbox = envBool("GOFS_DISABLE_HTML_SANDBOX", c.DisableHTMLSandbox)
	c.AllowRootSwitch = envBool("GOFS_ALLOW_ROOT_SWITCH", c.AllowRootSwitch)
	c.RootAllow = envList("GOFS_ROOT_ALLOW", c.RootAllow)
	c.AllowAnonymousAdmin = envBool("GOFS_ALLOW_ANONYMOUS_ADMIN", c.AllowAnonymousAdmin)
}

// Parse 解析命令行参数与环境变量，返回最终配置。
// args 不含程序名本身（即传入 os.Args[1:]）。
func Parse(args []string, stdout io.Writer) (*Config, error) {
	cfg := Defaults()
	cfg.ApplyEnv()

	fs := flag.NewFlagSet("gofs", flag.ContinueOnError)
	fs.SetOutput(stdout)
	fs.Usage = func() { fmt.Fprint(stdout, usageText()) }

	// 该开关以「否定形式」存在，便于在不改动 layout 默认值的前提下关闭归档。
	var noUploadDated bool
	// 同理：换根默认开启，用否定形式关闭。
	var noRootSwitch bool
	// 用户管理默认开启，用否定形式关闭。
	var noUserManage bool

	fs.StringVar(&cfg.Bind, "b", cfg.Bind, "指定监听地址或 unix socket")
	fs.StringVar(&cfg.Bind, "bind", cfg.Bind, "指定监听地址或 unix socket")
	fs.IntVar(&cfg.Port, "p", cfg.Port, "指定监听端口")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "指定监听端口")
	fs.StringVar(&cfg.PathPrefix, "path-prefix", cfg.PathPrefix, "指定访问路径前缀")
	fs.Func("hidden", "目录列表中隐藏的路径 glob，逗号分隔", func(v string) error {
		cfg.Hidden = append(cfg.Hidden, splitList(v)...)
		return nil
	})
	fs.Func("a", "添加鉴权规则，例如 user:pass@/dir1:rw,/dir2", func(v string) error {
		cfg.AuthRules = append(cfg.AuthRules, v)
		return nil
	})
	fs.Func("auth", "添加鉴权规则，例如 user:pass@/dir1:rw,/dir2", func(v string) error {
		cfg.AuthRules = append(cfg.AuthRules, v)
		return nil
	})
	fs.BoolVar(&cfg.AllowAll, "A", cfg.AllowAll, "允许所有操作")
	fs.BoolVar(&cfg.AllowAll, "allow-all", cfg.AllowAll, "允许所有操作")
	fs.BoolVar(&cfg.AllowUpload, "allow-upload", cfg.AllowUpload, "允许上传文件/目录")
	fs.BoolVar(&cfg.AllowEdit, "allow-edit", cfg.AllowEdit, "允许在线编辑文本文件（未指定时跟随 --allow-upload）")
	fs.Int64Var(&cfg.EditMaxSize, "edit-max-size", cfg.EditMaxSize,
		"在线编辑允许打开的最大文件尺寸（字节），必须为正整数")
	fs.BoolVar(&cfg.AllowKeys, "allow-keys", cfg.AllowKeys, "允许管理上传密钥（未指定时跟随 --allow-upload）")
	fs.StringVar(&cfg.KeyFile, "key-file", cfg.KeyFile, "上传密钥存储路径，置空则只保存在内存中")
	fs.BoolVar(&noUserManage, "no-user-manage", false, "禁止在页面上增删改用户")
	fs.StringVar(&cfg.UserFile, "user-file", cfg.UserFile, "用户表存储路径，置空则只保存在内存中")
	fs.StringVar(&cfg.Title, "title", cfg.Title, "站点标题（浏览器标签页与顶栏品牌名）")
	fs.StringVar(&cfg.SettingsFile, "settings-file", cfg.SettingsFile,
		"运行期设置（目前只有标题）的存储路径，置空则只保存在内存中")
	fs.BoolVar(&cfg.AllowDelete, "allow-delete", cfg.AllowDelete, "允许删除文件/目录")
	fs.BoolVar(&cfg.AllowSearch, "allow-search", cfg.AllowSearch, "允许搜索文件/目录")
	fs.BoolVar(&cfg.AllowArchive, "allow-archive", cfg.AllowArchive, "允许把目录打包成压缩包下载")
	fs.BoolVar(&cfg.AllowExtract, "allow-extract", cfg.AllowExtract, "允许在线解压压缩包")
	fs.BoolVar(&cfg.AllowSymlink, "allow-symlink", cfg.AllowSymlink, "允许符号链接指向根目录之外")
	fs.BoolVar(&cfg.EnableCORS, "enable-cors", cfg.EnableCORS, "开启 CORS，返回 Access-Control-Allow-Origin: *")
	fs.BoolVar(&cfg.RenderIndex, "render-index", cfg.RenderIndex, "请求目录时返回 index.html，不存在则 404")
	fs.BoolVar(&cfg.RenderTryIndex, "render-try-index", cfg.RenderTryIndex, "请求目录时优先返回 index.html，无则目录列表")
	fs.BoolVar(&cfg.RenderSPA, "render-spa", cfg.RenderSPA, "以 SPA 模式渲染，未命中路径回退到 index.html")
	fs.BoolVar(&cfg.UnsafeIndexHTML, "unsafe-index-html", cfg.UnsafeIndexHTML,
		"允许可写部署中的 index.html 以同源方式执行（默认加 CSP sandbox）")
	fs.StringVar(&cfg.AssetsDir, "assets", cfg.AssetsDir, "自定义前端资源目录")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "自定义 HTTP 日志格式，置空可关闭")
	fs.StringVar(&cfg.LogFile, "log-file", cfg.LogFile, "日志输出文件，默认 stdout")
	fs.StringVar(&cfg.Compress, "compress", cfg.Compress, "打包压缩级别：none/low/medium/high")
	fs.StringVar(&cfg.UploadDateLayout, "upload-date-layout", cfg.UploadDateLayout,
		"上传时自动归档的日期目录布局（Go 时间布局），如 2006/01/02；只对传到服务根目录的上传生效")
	fs.BoolVar(&noUploadDated, "no-upload-dated", false, "关闭上传按日期自动归档（等同于 --upload-date-layout=''）")
	fs.StringVar(&cfg.TLSCert, "tls-cert", cfg.TLSCert, "HTTPS 证书路径")
	fs.StringVar(&cfg.TLSKey, "tls-key", cfg.TLSKey, "HTTPS 私钥路径")
	fs.Int64Var(&cfg.ExtractMaxTotal, "extract-max-total", cfg.ExtractMaxTotal, "单次解压写出字节上限")
	fs.IntVar(&cfg.ExtractMaxFiles, "extract-max-files", cfg.ExtractMaxFiles, "单次解压文件数上限")
	fs.IntVar(&cfg.ExtractMaxRatio, "extract-max-ratio", cfg.ExtractMaxRatio, "单文件压缩比上限（防解压炸弹）")
	fs.Int64Var(&cfg.UploadMaxSize, "upload-max-size", cfg.UploadMaxSize,
		"单次上传的最大字节数，0 表示不限制")
	fs.BoolVar(&noRootSwitch, "no-root-switch", false,
		"禁止登录后在页面上切换服务根目录")
	fs.BoolVar(&cfg.AllowAnonymousAdmin, "allow-anonymous-admin", cfg.AllowAnonymousAdmin,
		"未启用鉴权时仍允许通过 HTTP 管理接口修改服务设置（默认关闭，风险自负）")
	fs.Func("root-allow", "允许把服务根目录切换到的路径前缀（可重复指定）", func(v string) error {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		cfg.RootAllow = append(cfg.RootAllow, v)
		return nil
	})
	fs.Func("upload-read-timeout", "单次上传的最长读取时间，如 30m、1h", func(v string) error {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("无法解析时长 %q: %w", v, err)
		}
		cfg.UploadReadTimeout = d
		return nil
	})
	fs.Func("download-timeout", "单次下载的最长写出时间，如 30m、2h；0 表示不限制", func(v string) error {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("无法解析时长 %q: %w", v, err)
		}
		cfg.DownloadTimeout = d
		return nil
	})
	fs.IntVar(&cfg.ListMaxEntries, "list-max-entries", cfg.ListMaxEntries,
		"单次目录列举最多返回多少条，0 表示不限制")
	fs.IntVar(&cfg.ArchiveMaxItems, "archive-max-items", cfg.ArchiveMaxItems,
		"单次打包的条目数上限，0 表示不限制")
	fs.Int64Var(&cfg.ArchiveMaxBytes, "archive-max-bytes", cfg.ArchiveMaxBytes,
		"单次打包的原始字节上限，0 表示不限制")
	fs.IntVar(&cfg.MaxConcurrent, "max-concurrent", cfg.MaxConcurrent,
		"同时处理的请求数上限，0 表示不限制")
	fs.IntVar(&cfg.MaxConcurrentJobs, "max-concurrent-jobs", cfg.MaxConcurrentJobs,
		"同时进行的上传/解压任务数上限，0 表示不限制")
	fs.IntVar(&cfg.AuthFailLimit, "auth-fail-limit", cfg.AuthFailLimit,
		"同一来源认证失败多少次后临时封禁，0 表示不限制")
	fs.Func("auth-fail-window", "认证失败的统计窗口与封禁时长，如 5m、1h", func(v string) error {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("无法解析时长 %q: %w", v, err)
		}
		cfg.AuthFailWindow = d
		return nil
	})
	fs.Func("trusted-proxies", "可信反向代理地址（IP 或 CIDR，逗号分隔）；只有它们发来的转发头才会被采信", func(v string) error {
		cfg.TrustedProxies = append(cfg.TrustedProxies, splitList(v)...)
		return nil
	})
	fs.IntVar(&cfg.MaxHeaderBytes, "max-header-bytes", cfg.MaxHeaderBytes,
		"请求行+请求头允许的最大字节数")
	fs.IntVar(&cfg.MaxNameBytes, "max-name-bytes", cfg.MaxNameBytes,
		"允许的单个文件名最大字节数")
	fs.BoolVar(&cfg.DisableCSRFProtect, "no-csrf-protect", cfg.DisableCSRFProtect,
		"关闭写操作的跨站来源校验（默认开启，仅影响浏览器发起的请求）")
	fs.Int64Var(&cfg.HashMaxSize, "hash-max-size", cfg.HashMaxSize,
		"允许计算 ?hash 摘要的最大文件字节数，0 表示不限制")
	fs.BoolVar(&cfg.DisableHTMLSandbox, "no-html-sandbox", cfg.DisableHTMLSandbox,
		"关闭 HTML/SVG/XML 的 CSP sandbox（关闭后这些文件内的脚本可读取本站数据）")
	fs.BoolVar(&cfg.ShowVersion, "V", false, "打印版本")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "打印版本")

	// flag 包在遇到 -h/--help 时会返回 flag.ErrHelp。
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, ErrHelp
		}
		return nil, err
	}

	// 位置参数：**全部**被服务目录（多根时每个挂一个标签前缀）。
	// 以前只取最后一个，多填的会被静默丢掉 —— 那种「写了不生效」最难查。
	if rest := fs.Args(); len(rest) > 0 {
		cfg.ServePaths = append([]string(nil), rest...)
	}

	// --no-upload-dated 与显式的 --upload-date-layout 互斥，避免意图含糊。
	specified := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { specified[f.Name] = true })
	if noUploadDated {
		if specified["upload-date-layout"] {
			return nil, errors.New("--no-upload-dated 与 --upload-date-layout 不能同时使用")
		}
		cfg.UploadDateLayout = ""
	}

	// --no-root-switch 关掉运行期换根能力（服务方不希望用户改根目录时使用）。
	if noRootSwitch {
		cfg.AllowRootSwitch = false
	}
	if noUserManage {
		cfg.AllowUserManage = false
	}

	// 记下监听地址/端口是不是用户显式给的：运行期在页面上改过之后，
	// 只有显式给过的启动参数才压得过 settings.json 里的记录（恢复入口）。
	cfg.BindSet = cfg.BindSet || specified["b"] || specified["bind"]
	cfg.PortSet = cfg.PortSet || specified["p"] || specified["port"]

	// -A 是各项权限的并集，真正的展开放在 Normalize 里，
	// 这样以库方式直接设 AllowAll 的调用方也能享受同样的语义。
	if !cfg.AllowAll {
		// 未显式指定 --allow-edit / --allow-keys 时跟随上传权限：
		// 能上传文件却没能力改文件、发密钥会很别扭。
		//
		// 这一段依赖「flag 是否被显式设置」，是命令行独有的推断，
		// 所以留在 Parse 里而不是下沉到 Normalize。
		if !specified["allow-edit"] {
			cfg.AllowEdit = cfg.AllowUpload
		}
		if !specified["allow-keys"] {
			cfg.AllowKeys = cfg.AllowUpload
		}
	}

	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// NormalizePathPrefix 归一化访问路径前缀，返回 "" 或 "/xxx"（无尾斜杠）。
//
// 归一化规则同时用于启动参数与页面上的运行期修改 —— 两处必须一致，
// 否则「参数里写 /files、页面上写 files」会得到两个不同的前缀。
//
// 拒绝的取值：
//   - 空白、控制字符、`?`、`#`：会破坏 URL 与 cookie 的 Path；
//   - `"`、`'`、`<`、`>`、`\`：前缀会被拼进页面（href/src），这些字符
//     既是 HTML 注入面，也会让 URL 语义产生歧义；
//   - `..` / `.` 段、以及包含 `//` 的写法：前者可构造越界路径，后者在
//     URL 里是 scheme-relative（`//evil.com` 会被浏览器当成另一个主机）。
//
// 这些与其静默接受再出怪问题，不如在设置的那一刻就报错。输出侧仍会做
// HTML 转义（见 writeHTMLStatus），这里只是把明显不安全的输入挡在门外。
func NormalizePathPrefix(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" || p == "/" {
		return "", nil
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// 去掉全部尾斜杠：`//` 归一化为空，`/a//` 归一化为 `/a`。
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", nil
	}
	for _, r := range p {
		switch {
		case r < 0x20 || r == 0x7f, unicode.IsSpace(r):
			return "", fmt.Errorf("路径前缀不能包含空白或控制字符: %q", raw)
		case r == '?' || r == '#' || r == '"' || r == '\'' || r == '<' || r == '>' || r == '\\':
			return "", fmt.Errorf("路径前缀不能包含字符 %q: %q", r, raw)
		}
	}
	if strings.Contains(p, "//") {
		return "", fmt.Errorf("路径前缀不能包含连续斜杠 //（会被浏览器当成协议相对地址）: %q", raw)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("路径前缀不能包含 %q 段: %q", seg, raw)
		}
	}
	return p, nil
}

// Normalize 做参数合法化、归一化与校验。
// 以库方式手工构造 Config 后必须调用一次，否则 Compress 等字段可能非法。
func (c *Config) Normalize() error {
	// 版本号兜底：手工构造 Config（不经 Defaults）时也要有个可报告的版本。
	if strings.TrimSpace(c.Version) == "" {
		c.Version = DefaultVersion
	}
	// 服务根：把「一个」和「多个」统一到 ServePaths 上，并维持
	// ServePaths[0] == ServePath 这条不变量（单一事实来源）。
	// 上层可能只设了 ServePath（老代码 / WithRoot），也可能只设了 ServePaths。
	if len(c.ServePaths) == 0 {
		p := strings.TrimSpace(c.ServePath)
		if p == "" {
			p = "."
		}
		c.ServePaths = []string{p}
	}
	cleaned := make([]string, 0, len(c.ServePaths))
	for _, p := range c.ServePaths {
		p = strings.TrimSpace(p)
		if p == "" {
			return errors.New("服务根路径不能为空")
		}
		if strings.ContainsRune(p, 0) {
			return fmt.Errorf("服务根路径 %q 含有非法字符", p)
		}
		cleaned = append(cleaned, p)
	}
	c.ServePaths = cleaned
	c.ServePath = cleaned[0]

	// 标题同理：空标题会让浏览器标签页只剩一个「—」。
	if strings.TrimSpace(c.Title) == "" {
		c.Title = DefaultTitle
	}

	// -A 展开成各项权限的并集。
	if c.AllowAll {
		c.AllowUpload = true
		c.AllowDelete = true
		c.AllowSearch = true
		c.AllowArchive = true
		c.AllowExtract = true
		c.AllowEdit = true
		c.AllowKeys = true
	}

	// 路径前缀统一为 "/xxx" 或空。
	p, err := NormalizePathPrefix(c.PathPrefix)
	if err != nil {
		return err
	}
	c.PathPrefix = p

	// 压缩级别校验。
	switch strings.ToLower(c.Compress) {
	case "none", "low", "medium", "high":
		c.Compress = strings.ToLower(c.Compress)
	default:
		return fmt.Errorf("非法的压缩级别 %q，可选：none / low / medium / high", c.Compress)
	}

	// 上传日期布局校验：用一个参照时刻试格式化，确认不会生成越界路径段。
	c.UploadDateLayout = strings.TrimSpace(c.UploadDateLayout)
	if c.UploadDateLayout != "" {
		probe := dateLayoutProbe.Format(c.UploadDateLayout)
		switch {
		case strings.Trim(probe, "/") == "":
			return fmt.Errorf("非法的上传日期布局 %q：格式化结果为空", c.UploadDateLayout)
		case strings.Contains(probe, ".."):
			return fmt.Errorf("非法的上传日期布局 %q：不允许出现 ..", c.UploadDateLayout)
		case strings.HasPrefix(probe, "/"), strings.HasPrefix(probe, "\\"):
			return fmt.Errorf("非法的上传日期布局 %q：不允许是绝对路径", c.UploadDateLayout)
		case strings.ContainsAny(probe, "\\:"):
			return fmt.Errorf("非法的上传日期布局 %q：不允许出现 \\ 或 :", c.UploadDateLayout)
		}
	}

	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("非法端口 %d", c.Port)
	}
	if c.TLSCert != "" || c.TLSKey != "" {
		if c.TLSCert == "" || c.TLSKey == "" {
			return errors.New("启用 HTTPS 需要同时指定 --tls-cert 与 --tls-key")
		}
	}
	if c.ExtractMaxFiles <= 0 {
		return errors.New("--extract-max-files 必须为正整数")
	}
	if c.ExtractMaxTotal <= 0 {
		return errors.New("--extract-max-total 必须为正整数")
	}
	if c.ExtractMaxRatio <= 0 {
		return errors.New("--extract-max-ratio 必须为正整数")
	}
	if c.EditMaxSize <= 0 {
		return errors.New("--edit-max-size 必须为正整数")
	}

	// ---- 服务端防护 ----
	// 这几个允许为 0（表示不限制），负数才是配置错误。
	if c.UploadMaxSize < 0 {
		return errors.New("--upload-max-size 不能为负数（0 表示不限制）")
	}
	if c.ListMaxEntries < 0 {
		return errors.New("--list-max-entries 不能为负数（0 表示不限制）")
	}
	if c.ArchiveMaxItems < 0 {
		return errors.New("--archive-max-items 不能为负数（0 表示不限制）")
	}
	if c.ArchiveMaxBytes < 0 {
		return errors.New("--archive-max-bytes 不能为负数（0 表示不限制）")
	}
	if c.MaxConcurrent < 0 {
		return errors.New("--max-concurrent 不能为负数（0 表示不限制）")
	}
	if c.MaxConcurrentJobs <= 0 {
		return errors.New("--max-concurrent-jobs 必须为正整数（0 表示不限制，请用负数以外的值）")
	}
	if c.AuthFailLimit < 0 {
		return errors.New("--auth-fail-limit 不能为负数（0 表示不限制）")
	}
	if c.AuthFailLimit > 0 && c.AuthFailWindow <= 0 {
		return errors.New("--auth-fail-window 必须为正时长")
	}
	// 转发头只在直连对端是可信代理时才采信；地址写错会让限速与日志
	// 悄悄退回代理 IP，所以这里直接拒绝非法值。
	{
		var normalized []string
		for _, raw := range c.TrustedProxies {
			for _, item := range splitList(raw) {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				if net.ParseIP(item) == nil {
					if _, _, err := net.ParseCIDR(item); err != nil {
						return fmt.Errorf("--trusted-proxies 里的 %q 不是合法 IP 或 CIDR", item)
					}
				}
				normalized = append(normalized, item)
			}
		}
		c.TrustedProxies = normalized
	}
	if c.MaxHeaderBytes < 4096 {
		// 4 KiB 已经放不下正常的 Cookie 与长 URL，再小只会伤到自己。
		return errors.New("--max-header-bytes 至少为 4096")
	}
	if c.MaxNameBytes < 16 {
		return errors.New("--max-name-bytes 至少为 16")
	}
	if c.UploadReadTimeout <= 0 {
		return errors.New("--upload-read-timeout 必须为正时长")
	}
	if c.DownloadTimeout < 0 {
		return errors.New("--download-timeout 不能为负（0 表示不限制）")
	}
	return nil
}

// UploadDated 表示是否开启了上传按日期归档。
func (c *Config) UploadDated() bool { return c.UploadDateLayout != "" }

// UploadDateDir 返回 t 时刻按配置布局生成的归档目录段（如 "2026/09/28"）。
// 未开启归档时返回空串。
func (c *Config) UploadDateDir(t time.Time) string {
	if c.UploadDateLayout == "" {
		return ""
	}
	return t.Format(c.UploadDateLayout)
}

// ListenAddr 把 bind 与 port 组合成最终监听地址。
//
// 独立成函数是因为它有三处调用者：启动时建监听、页面上热重绑、以及展示当前
// 地址。三处必须用同一套规则，否则「页面上改完显示的地址」与「实际监听的地址」
// 会对不上 —— 那种不一致只能靠抓包发现。
//
// 规则：
//   - bind 以 "/" 开头 → 就是 unix socket 路径，port 忽略；
//   - bind 里已含冒号（如 "127.0.0.1:8080"）→ 整个当作地址，port 忽略；
//   - bind 是纯主机/IP → 拼上 port；
//   - bind 为空 → 监听所有网卡。
func ListenAddr(bind string, port int) string {
	if strings.HasPrefix(bind, "/") {
		return bind // unix socket
	}
	if bind != "" {
		if strings.Contains(bind, ":") {
			return bind
		}
		return fmt.Sprintf("%s:%d", bind, port)
	}
	return fmt.Sprintf("0.0.0.0:%d", port)
}

// NormalizeListen 归一化并校验页面上的「监听地址 + 监听端口」两个字段。
//
// 与 ListenAddr 的分工：ListenAddr 是**拼地址**（接受 CLI 那种把端口写进
// --bind 的写法），这里服务的是页面上分成两个框的取值 —— 所以地址里**不接受**
// host:port：两个框都能表达端口时，用户不知道该以哪个为准，我们也不该猜。
//
// 返回归一化后的 bind 与 port。以 "/" 开头视为 unix socket，此时端口无意义
// （仍原样返回，便于用户切回 TCP 时不用重填）。
func NormalizeListen(bind string, port int) (string, int, error) {
	bind = strings.TrimSpace(bind)
	if port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("非法端口 %d：取值 0-65535（0 表示由系统分配）", port)
	}
	if bind == "" {
		return bind, port, nil
	}
	if strings.HasPrefix(bind, "/") {
		// unix socket 路径会直接交给 net.Listen，控制字符会让错误信息
		// 与日志都变得不可读；NUL 更是直接非法。
		for _, r := range bind {
			if r == 0 || r < 0x20 || r == 0x7f {
				return "", 0, fmt.Errorf("非法的 unix socket 路径 %q：包含控制字符", bind)
			}
		}
		return bind, port, nil
	}
	if strings.Contains(bind, ":") {
		return "", 0, fmt.Errorf("监听地址里不要写端口（%q），端口请填在下面的「监听端口」里", bind)
	}
	if strings.ContainsAny(bind, " \t/?#@") {
		return "", 0, fmt.Errorf("非法的监听地址 %q", bind)
	}
	return bind, port, nil
}

// Addr 返回最终的监听地址。
func (c *Config) Addr() string { return ListenAddr(c.Bind, c.Port) }

// IsUnixSocket 判断是否监听 unix socket。
//
// ⚠️ 读的是**启动时**的 Bind。运行期热重绑之后它就不再准确了 ——
// 需要判断当下实际监听类型的地方，应当看实时监听的 Addr().Network()。
func (c *Config) IsUnixSocket() bool { return strings.HasPrefix(c.Bind, "/") }

// usageText 返回帮助文本。
func usageText() string {
	return `gofs - 一个参照 dufs 设计的 Go 文件服务器，支持在线解压压缩包

用法: gofs [选项] [serve-path ...]

参数:
  [serve-path ...]           要服务的路径，默认 "."。给多个时每个目录挂在
                             自己的名字下（/data/…、/backup/…）；重名自动
                             加后缀，互相嵌套的目录会被拒绝（启动即报错）

选项:
  -b, --bind <addrs>         监听地址或 unix socket
  -p, --port <port>          监听端口，默认 5000
                             （这两项也能在页面的「设置 → 常规」里改，立即重绑；
                               此处显式给过的值优先，重启后回到参数值）
      --path-prefix <path>   访问路径前缀
                             （同样可在页面上改；显式给过时优先）
      --hidden <value>       目录列表中隐藏的路径 glob，例如 tmp,*.log
  -a, --auth <rules>         鉴权规则，例如 user:pass@/dir1:rw,/dir2
  -A, --allow-all            允许所有操作
      --allow-upload         允许上传
      --allow-edit           允许在线编辑文本文件
                             （未显式指定时跟随 --allow-upload 与 -A）
      --edit-max-size <n>    在线编辑允许打开的最大文件尺寸，默认 2MiB，必须为正整数
                             （登录后也可以在页面的「服务设置」里改，改完立刻生效）
      --allow-keys           允许管理上传密钥（未指定时跟随 --allow-upload）
      --key-file <path>      上传密钥存储路径，默认 <用户配置目录>/gofs/keys.json；
                             置空则只保存在内存中，重启即失效
      --user-file <path>     用户表存储路径，默认 <用户配置目录>/gofs/users.json；
                             置空则只保存在内存中，重启即失效
      --title <text>         站点标题（浏览器标签页与顶栏品牌名），默认 gofs。
                             管理员也能在页面的「设置 → 常规」里改，改完会落盘
      --settings-file <path> 运行期设置的存储路径，默认 <用户配置目录>/gofs/settings.json；
                             置空则只保存在内存中，重启即失效
      --no-user-manage       禁止在页面上增删改用户（账号只能由 -a 定义）
      --allow-delete         允许删除
      --allow-search         允许搜索
      --allow-archive        允许目录打包为 zip 下载
      --allow-extract        允许在线解压压缩包（zip/tar/tar.gz/tgz/gz）
      --allow-symlink        允许符号链接指向根目录之外
      --enable-cors          开启 CORS
      --render-index         目录下 index.html 作为首页，不存在返回 404
      --render-try-index     优先 index.html，无则目录列表
      --render-spa           SPA 模式，未命中回退 index.html
      --unsafe-index-html    可写部署（上传/编辑/解压）中的 index.html
                             默认加 CSP sandbox；加此项才允许它以同源方式执行
      --assets <path>        自定义前端资源目录
      --log-format <format>  自定义 HTTP 日志格式
      --log-file <file>      日志文件
      --compress <level>     打包压缩级别：none/low/medium/high，默认 low
      --upload-date-layout <layout>
                             上传时自动归档的日期目录布局（Go 时间布局字面量），
                             默认 "2006/01/02"，即按 年/月/日 建三级目录；
                             置空（--upload-date-layout=""）则关闭
                             ★ 只对「直接传到服务根目录」的上传生效：
                               /pic.jpg      → /2026/09/28/pic.jpg
                               /sub/pic.jpg  → /sub/pic.jpg（子目录原地放）
                             单次可覆盖：?dated=0 跳过、?dated=1 强制归档
      --no-upload-dated      关闭上传按日期自动归档
      --tls-cert <path>      HTTPS 证书
      --tls-key <path>       HTTPS 私钥
      --extract-max-total <n> 解压写出字节上限，默认 10GiB
      --extract-max-files <n> 解压文件数上限，默认 100000
      --extract-max-ratio <n> 单文件压缩比上限，默认 200
  -V, --version              打印版本
  -h, --help                 打印帮助

防护选项（默认即为较安全的取值，一般无需调整）:
      --upload-max-size <n>  单次上传的最大字节数，默认 10GiB；0 表示不限制
                             （登录后也可以在页面的「服务设置」里改）
      --upload-read-timeout <d>
                            上传「连续多久没有进展」就断开，默认 2m
                            （只要还在往里传就不会被打断，防的是占着名额不发的连接）
      --download-timeout <d> 下载「连续多久没有进展」就断开，默认 2m；0 表示不限制
                            （防的是「连上后不读数据」的客户端长期占住名额）
      --list-max-entries <n> 单次目录列举最多返回多少条，默认 20000；0 表示不限制
      --archive-max-items <n> 单次打包的条目数上限，默认 200000
      --archive-max-bytes <n> 单次打包的原始字节上限，默认 50GiB
      --max-concurrent <n>   同时处理的请求数上限，默认 512；0 表示不限制
      --max-concurrent-jobs <n>
                             同时进行的上传/解压任务数上限，默认 64
      --auth-fail-limit <n>  同一来源认证失败多少次后临时封禁，默认 10；0 表示关闭
      --auth-fail-window <d> 认证失败的统计窗口与封禁时长，默认 5m
      --trusted-proxies <ips>
                            可信反向代理地址（IP/CIDR，逗号分隔）。
                            只有直连对端在列表内时才采信 X-Forwarded-For /
                            X-Real-IP；默认不信任任何转发头
      --max-header-bytes <n> 请求行+请求头允许的最大字节数，默认 64KiB
      --max-name-bytes <n>   允许的单个文件名最大字节数，默认 255
      --hash-max-size <n>    允许计算 ?hash 摘要的最大文件字节数，默认 512MiB
      --no-csrf-protect      关闭写操作的跨站来源校验
                             （默认开启；只影响浏览器发起的跨站请求，
                              curl 等不带 Origin 的客户端不受影响）
      --no-html-sandbox      关闭 HTML/SVG/XML 的 CSP sandbox
                             （默认开启：上传的网页在不透明源里渲染，
                              脚本读不到本站数据，堵住存储型 XSS）
      --no-root-switch       禁止登录后在页面上切换服务根目录
      --root-allow <prefix>  允许把服务根目录切换到的路径前缀，可重复指定
                             （默认可切换范围 = 启动根目录的父目录，
                               比如启动根是 /srv/data 时可切到 /srv/*）
      --allow-anonymous-admin
                             未启用鉴权时仍允许通过 HTTP 管理接口修改服务设置。
                             默认关闭：无鉴权时任何能连上端口的人都不应能
                             挂载任意目录、切换服务根或修改监听地址

示例:
  gofs                                 以只读模式服务当前目录
  gofs -A ./data                       服务 ./data 并允许所有操作
  gofs -A --allow-extract ./data       在上一行基础上开放在线解压
  gofs -a admin:123@/:rw -A ./data     需要账号密码
  gofs -b 127.0.0.1 -p 8080 ./public   指定监听地址与端口

在线解压:
  PUT    /fs/extract       请求体 {"path":"a.zip","dest":"a","overwrite":false}
                                 返回 text/event-stream，逐条推送解压进度
  GET    /fs/extract?path=a.zip          列出压缩包内容（JSON）
  GET    /fs/extract?path=a.zip&file=x   从压缩包内流式下载单个文件
在线编辑:
  GET    /fs/text?path=a.md         读取文本内容（含语言、换行、BOM、内容摘要）
  PUT    /fs/text                   保存，请求体 {"path","content","base_hash","force"}
                                          摘要不匹配时返回 409，避免静默覆盖他人改动

上传密钥:
  GET    /fs/keys                   列出全部密钥（不含明文）
  POST   /fs/keys                   创建，请求体 {"name","scope","ttl_seconds"}
                                          响应中的 token 只返回这一次
  DELETE /fs/keys?id=<id>           撤销
  使用   curl -T f.txt -H 'X-Gofs-Upload-Key: <token>' http://host/path
         或 curl -T f.txt 'http://host/path?key=<token>'
         密钥只允许上传，且只能落在 scope 限定的目录内

用户管理:
  GET    /fs/users                  列出账号（启动参数定义的会标 from_startup）
  POST   /fs/users                  请求体 {"name","password","rules":[{"path","perm"}]}
  PUT    /fs/users                  同上；省略 password 表示不改密码
  DELETE /fs/users?name=<name>      删除
  perm   "rw" 可读写 / "r" 只读，按最长前缀匹配，未命中的路径一律拒绝
  权限   只有「对服务根拥有读写权限」的账号能用；且服务必须已启用鉴权
         （先用 -a 配一个账号，再来页面创建其他用户）
  密码   只保存 PBKDF2-HMAC-SHA256 摘要，每用户独立随机盐，服务端拿不回明文

服务设置（运行期，免重启）:
  GET    /fs/settings               读取当前设置
  PUT    /fs/settings               请求体 {"root":"/srv/data","upload_max_size":10737418240}
                                          upload_max_size 为 0 表示不限制
                                          也可改 path_prefix / listen_addr / listen_port
                                          （改监听是热重绑：先建新监听，成功才关旧的）
  权限   需要对服务根拥有读写权限（否则 403）；受 --no-root-switch /
         --root-allow 约束，可切换范围之外的路径会被拒绝（400）

认证:
  页面使用 HTTP Basic 认证。浏览器导航未登录时返回应用外壳（200），
  由前端弹出登录框；应用内部请求会带 X-Gofs-Ajax 标记，
  此时 401 不附带 WWW-Authenticate 挑战，避免弹出浏览器原生登录框。
  命令行仍然照常使用 curl -u user:pass 或 --anyauth。
`
}
