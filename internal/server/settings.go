package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/xxl6097/gofs/internal/config"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// runtimeSettings 保存「运行期可以修改、且会被并发读取」的设置。
//
// 为什么不能直接改 cfg 里的字段：cfg 是启动时构造好后被所有请求共享的，
// 在请求处理中途写它会造成数据竞争（Go 的 race detector 会直接报错）。
// 所以这些可变的取值单独抽出来，用锁保护。
//
// 目前有四处：
//   - 站点标题（浏览器标签页与顶栏品牌名）
//   - 单文件上传上限
//   - 在线编辑的大小上限
//   - 允许切换服务根目录的范围（由启动参数推导，运行期不变）
//
// 前三项都会落盘（见 saveSettings）。**但不是无条件写**：值与启动参数
// 相同时不写磁盘（见 saveSettings 的说明）—— 那样运维之后改启动参数
// 仍然生效，不会被一个「其实等于默认值」的旧记录压住。
//
// 服务根目录的切换仍然**不落盘**：那是「这个部署该服务哪个目录」的声明，
// 属于启动参数，不该被一次页面操作固化。
type runtimeSettings struct {
	cfg *config.Config

	mu            sync.RWMutex
	uploadMaxSize int64
	editMaxSize   int64
	title         string
	// sites 是运行期新增的附加站点（持久化的那份）。
	sites []siteRecord

	// settingsFile 是标题的持久化路径；空表示只存内存。
	settingsFile string

	// allowRoots 是「允许把服务根目录切换到哪些地方」的前缀列表。
	//
	// 由启动时的根目录推导而来，**之后不再变化**：如果跟着当前根一起变，
	// 那切到子目录之后就再也切不回来了（范围会跟着收窄）。
	allowRoots []string

	// siteRoots 是**显式**配置的路径前缀（--root-allow / RootAllow），
	// 不包含上面那个推导出来的父目录。
	//
	// 单独留一份是因为「加站点」与「换根」的默认值不同：换根默认限在启动
	// 主目录的兄弟目录内；加站点默认不限（见 AllowsSite）。要区分「用户显式
	// 写了白名单」与「我们替他推出来的默认范围」，就必须把它单独存着。
	siteRoots []string
}

// Title 返回当前站点标题。
func (s *runtimeSettings) Title() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.title
}

// SetTitle 修改站点标题并落盘。
//
// 落盘失败不影响本次生效（内存里已经是新值），但要把错误回给调用方 ——
// 否则管理员会以为「设好了」，重启后才发现又变回去了。
func (s *runtimeSettings) SetTitle(v string) error {
	s.mu.Lock()
	s.title = v
	s.mu.Unlock()
	return s.saveSettings()
}

// settingsDoc 是设置文件的磁盘格式。
//
// 带 version 是为了以后加字段时能识别旧文件：
//
//	1 只有 title
//	2 起多了 sites（运行期新增的附加站点）
//	3 起多了两个上限（upload_max_size / edit_max_size）
//
// 两个上限用**指针**：上传上限的 0 是有意义的取值（不限制），
// 用零值当「没存过」会把「不限制」读成「没设置」。
type settingsDoc struct {
	Version int          `json:"version"`
	Title   string       `json:"title,omitempty"`
	Sites   []siteRecord `json:"sites,omitempty"`
	// 只在「与启动参数不同」时才写（见 saveSettings）。
	UploadMaxSize *int64 `json:"upload_max_size,omitempty"`
	EditMaxSize   *int64 `json:"edit_max_size,omitempty"`
}

// siteRecord 是一个持久化的附加站点。
//
// 只存**运行期新增**的站点：启动时用命令行给的附加目录仍由命令行说了算
// （去掉那个参数就等于去掉站点）。记 label 是为了让书签跨重启仍然有效。
type siteRecord struct {
	Path  string `json:"path"`
	Label string `json:"label"`
}

// loadSettings 读取设置文件（标题 + 附加站点）。
//
// 读不到（文件不存在 / 内容损坏 / 路径为空）一律**保留启动参数给的默认值**，
// 不报错、也不让服务起不来：一个坏掉的设置文件不该影响整个服务可用。
func (s *runtimeSettings) loadSettings() {
	path := s.settingsFile
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc settingsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	if t := strings.TrimSpace(doc.Title); t != "" {
		s.title = t
	}
	s.sites = append([]siteRecord(nil), doc.Sites...)
	// 文件里的值优先于启动参数 —— 与标题、站点同一个口径。
	// 上传上限 0 是合法值（不限制），所以只判「有没有存过」。
	if doc.UploadMaxSize != nil {
		s.uploadMaxSize = *doc.UploadMaxSize
	}
	// 编辑上限没有「不限制」这一档：文件里被手工写成 0/负数时忽略它，
	// 让它回落到启动参数 —— 否则每个文件都会被判成超限（全都 413）。
	if doc.EditMaxSize != nil && *doc.EditMaxSize > 0 {
		s.editMaxSize = *doc.EditMaxSize
	}
}

// Sites 返回持久化的附加站点。
func (s *runtimeSettings) Sites() []siteRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]siteRecord, len(s.sites))
	copy(out, s.sites)
	return out
}

// SetSites 记下当前的附加站点并落盘。
func (s *runtimeSettings) SetSites(v []siteRecord) error {
	s.mu.Lock()
	s.sites = append([]siteRecord(nil), v...)
	s.mu.Unlock()
	return s.saveSettings()
}

// saveSettings 原子写回设置文件（路径为空时只留在内存里）。
//
// ⚠️ 必须把**当前全部**字段一起写：只写 title 会把 sites 清空 ——
// 改一次标题就把站点列表弄丢，是这套「单文件多字段」最容易踩的坑。
func (s *runtimeSettings) saveSettings() error {
	path := s.settingsFile
	if path == "" {
		return nil
	}
	doc := settingsDoc{Version: 3, Title: s.Title(), Sites: s.Sites()}
	// 只在**与启动参数不同**时才写这两项。
	//
	// 为什么不无脑写：那样「恢复启动时的默认值」之后，文件里会留下一份
	// 等于旧默认值的记录；运维以后把 --upload-max-size 改大，重启却仍被
	// 那份旧记录压住 —— 参数明明改了却不生效，很难查。
	if v := s.UploadMaxSize(); v != s.cfg.UploadMaxSize {
		doc.UploadMaxSize = &v
	}
	if v := s.EditMaxSize(); v != s.cfg.EditMaxSize {
		doc.EditMaxSize = &v
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建设置目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	abort := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(raw); err != nil {
		abort()
		return err
	}
	if err := tmp.Sync(); err != nil {
		abort()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// newRuntimeSettings 依据启动配置与启动根目录构造运行时设置。
func newRuntimeSettings(cfg *config.Config, startRoot string) *runtimeSettings {
	rs := &runtimeSettings{
		cfg:           cfg,
		uploadMaxSize: cfg.UploadMaxSize,
		editMaxSize:   cfg.EditMaxSize,
		title:         cfg.Title,
		settingsFile:  cfg.SettingsFile,
	}
	rs.loadSettings()

	// 默认可切换范围是启动根的**父目录**：
	// 启动根是 /srv/data 时能切到 /srv/* 下的任何地方（含兄弟目录），
	// 但够不到系统别处 —— 既能满足「换个数据目录」的常见需求，
	// 又不会因为一次误操作把整个文件系统暴露出去。
	base := filepath.Dir(startRoot)
	if base == "" || base == "." {
		base = startRoot
	}
	rs.allowRoots = append(rs.allowRoots, base)

	for _, p := range cfg.RootAllow {
		abs, err := fsutil.NormalizePath(p)
		if err != nil {
			continue
		}
		rs.allowRoots = append(rs.allowRoots, abs)
		rs.siteRoots = append(rs.siteRoots, abs)
	}
	return rs
}

// AllowsSite 判断某个目录能否被新增为站点。
//
// 与 AllowsRoot（运行期换根）**默认值刻意不同**：
//
//   - 换根默认限在启动主目录的兄弟目录内 —— 那是个「在目录树里跳来跳去」
//     的动作，收窄一点更安全；
//   - 加站点**默认不限制**（门槛是「你是管理员」）。运维明确想再加一个
//     目录时，「先改启动参数重启」正是这个功能要免掉的事。
//
// 两者都认可**显式**配置的白名单：一旦写了 --root-allow / RootAllow，
// 加站点也按它收窄 —— 想收紧的部署有这个开关，不想管的人不必配。
func (s *runtimeSettings) AllowsSite(abs string) bool {
	if len(s.siteRoots) == 0 {
		return true
	}
	for _, base := range s.siteRoots {
		if fsutil.IsWithin(base, abs) {
			return true
		}
	}
	return false
}

// SiteRoots 返回显式配置的白名单（供界面提示与错误信息用；空表示不限制）。
func (s *runtimeSettings) SiteRoots() []string {
	out := make([]string, len(s.siteRoots))
	copy(out, s.siteRoots)
	return out
}

// UploadMaxSize 返回当前生效的单文件上传上限，0 表示不限制。
func (s *runtimeSettings) UploadMaxSize() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.uploadMaxSize
}

// SetUploadMaxSize 修改单文件上传上限并落盘。负值会被归一化为 0（不限制）。
//
// 落盘失败不影响本次生效（内存里已经是新值），但要把错误回给调用方 ——
// 否则管理员会以为「设好了」，重启后才发现又变回去了。
func (s *runtimeSettings) SetUploadMaxSize(n int64) error {
	if n < 0 {
		n = 0
	}
	s.mu.Lock()
	s.uploadMaxSize = n
	s.mu.Unlock()
	return s.saveSettings()
}

// EditMaxSize 返回当前生效的在线编辑大小上限（恒为正数）。
//
// 注意：这个上限**服务端也要用**（见 textRead/textSave），不能只靠前端
// 判断是否显示「编辑」按钮 —— 那样绕过去就能改一个几百 MB 的文件。
func (s *runtimeSettings) EditMaxSize() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.editMaxSize
}

// SetEditMaxSize 修改在线编辑大小上限并落盘。
//
// 非正值会被归一化为默认值 —— 调用方（handler_settings）已经先校验过，
// 这里只是兜底，避免出现「上限为 0」把每个文件都判成超限。
func (s *runtimeSettings) SetEditMaxSize(n int64) error {
	if n <= 0 {
		n = config.DefaultEditMaxSize
	}
	s.mu.Lock()
	s.editMaxSize = n
	s.mu.Unlock()
	return s.saveSettings()
}

// AllowRoots 返回允许切换到的路径前缀（供界面展示）。
func (s *runtimeSettings) AllowRoots() []string {
	out := make([]string, len(s.allowRoots))
	copy(out, s.allowRoots)
	return out
}

// AllowsRoot 判断 abs 是否在允许切换的范围内。
//
// 只做字符串前缀比较，不访问文件系统 —— 白名单里的路径可能还不存在。
// 调用方必须先把 abs 规范化（Abs + 软链解析），否则可以拿 ".." 或软链绕过。
func (s *runtimeSettings) AllowsRoot(abs string) bool {
	for _, base := range s.allowRoots {
		if fsutil.IsWithin(base, abs) {
			return true
		}
	}
	return false
}
