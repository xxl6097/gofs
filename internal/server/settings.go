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
//   - 站点标题（浏览器标签页与顶栏品牌名，**会落盘**）
//   - 单文件上传上限（登录取到管理员后可在页面上调整）
//   - 在线编辑的大小上限（同上）
//   - 允许切换服务根目录的范围（由启动参数推导，运行期不变）
//
// 为什么只有标题落盘：上限是「临时放宽」性质的运行时覆盖，重启回到启动
// 参数声明的值才是对的（否则运维临时调大一次就再也回不去了）；标题是标识，
// 重启后还在才符合预期。
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
// 带 version 是为了以后加字段时能识别旧文件。version 1 只有 title；
// version 2 起多了 sites（运行期新增的附加站点）。
type settingsDoc struct {
	Version int          `json:"version"`
	Title   string       `json:"title,omitempty"`
	Sites   []siteRecord `json:"sites,omitempty"`
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
	doc := settingsDoc{Version: 2, Title: s.Title(), Sites: s.Sites()}
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
	}
	return rs
}

// UploadMaxSize 返回当前生效的单文件上传上限，0 表示不限制。
func (s *runtimeSettings) UploadMaxSize() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.uploadMaxSize
}

// SetUploadMaxSize 修改单文件上传上限。负值会被归一化为 0（不限制）。
func (s *runtimeSettings) SetUploadMaxSize(n int64) {
	if n < 0 {
		n = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadMaxSize = n
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

// SetEditMaxSize 修改在线编辑大小上限。
//
// 非正值会被归一化为默认值 —— 调用方（handler_settings）已经先校验过，
// 这里只是兜底，避免出现「上限为 0」把每个文件都判成超限。
func (s *runtimeSettings) SetEditMaxSize(n int64) {
	if n <= 0 {
		n = config.DefaultEditMaxSize
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.editMaxSize = n
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
