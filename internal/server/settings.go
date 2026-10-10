package server

import (
	"path/filepath"
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
// 目前有三处：
//   - 单文件上传上限（登录取到管理员后可在页面上调整）
//   - 在线编辑的大小上限（同上）
//   - 允许切换服务根目录的范围（由启动参数推导，运行期不变）
type runtimeSettings struct {
	cfg *config.Config

	mu            sync.RWMutex
	uploadMaxSize int64
	editMaxSize   int64

	// allowRoots 是「允许把服务根目录切换到哪些地方」的前缀列表。
	//
	// 由启动时的根目录推导而来，**之后不再变化**：如果跟着当前根一起变，
	// 那切到子目录之后就再也切不回来了（范围会跟着收窄）。
	allowRoots []string
}

// newRuntimeSettings 依据启动配置与启动根目录构造运行时设置。
func newRuntimeSettings(cfg *config.Config, startRoot string) *runtimeSettings {
	rs := &runtimeSettings{
		cfg:           cfg,
		uploadMaxSize: cfg.UploadMaxSize,
		editMaxSize:   cfg.EditMaxSize,
	}

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
