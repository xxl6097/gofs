// Package fsutil 提供受限目录下的路径解析与目录列举能力。
package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/gofs/internal/textfile"
)

// ErrEscaped 表示解析出的路径逃逸出了服务根目录。
var ErrEscaped = errors.New("路径超出了服务根目录")

// ErrBadSearchPattern 表示搜索表达式本身非法（正则/通配符语法错误）。
//
// 单独抽出来是因为它与「文件系统出错」性质不同：前者是调用方输入错误，
// 应当回 400；后者才走 404/403/500。调用方用 errors.Is 区分。
var ErrBadSearchPattern = errors.New("搜索表达式无效")

// relator 是 ReadDir / Search 真正需要的能力：把磁盘路径转回 URL 路径。
//
// 单独抽出来是为了让多根模式（*Roots）也能用这两个函数 —— 它们不关心
// 路径是哪个根解析出来的，只关心能不能算出一个可点的 URL。
type relator interface {
	Rel(abs string) (string, error)
}

// Resolver 在根目录内做安全的路径解析。
//
// 根目录可以在运行期切换（登录后的「服务设置」），因此所有读写都要过
// mu：解析路径时持读锁，切换根时持写锁。锁的粒度是「一次 Resolve」而不是
// 「整个请求」——不然一次大文件传输就会把切换操作挡住。
type Resolver struct {
	mu            sync.RWMutex
	root          string // 已求绝对路径的根目录
	allowSymlink  bool   // 是否允许符号链接逃逸
	singleFile    bool   // 根节点是单个文件而非目录
	serveRootName string // 单文件模式下的文件名
}

// NewResolver 创建解析器。root 会被展开为绝对路径。
func NewResolver(root string, allowSymlink bool) (*Resolver, error) {
	abs, single, name, err := normalizeRoot(root)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		root:          abs,
		allowSymlink:  allowSymlink,
		singleFile:    single,
		serveRootName: name,
	}, nil
}

// normalizeRoot 把用户给的路径规范化成「已解析软链的绝对目录」。
//
// 根目录自身可能位于符号链接之下（macOS 上 /tmp 指向 /private/tmp），
// 必须先解析为真实路径，否则每次 EvalSymlinks 后的前缀比对都会失败，
// 导致所有请求被误判为「逃逸出根目录」。
//
// 传入单个文件时返回它所在的目录，并由 single 标记。
func normalizeRoot(root string) (abs string, single bool, name string, err error) {
	abs, err = filepath.Abs(root)
	if err != nil {
		return "", false, "", err
	}
	abs = filepath.Clean(abs)
	if real, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = real
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return "", false, "", statErr
	}
	if info.IsDir() {
		return abs, false, "", nil
	}
	return filepath.Dir(abs), true, filepath.Base(abs), nil
}

// Root 返回服务根目录的绝对路径。
func (r *Resolver) Root() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.root
}

// SetRoot 在运行期切换服务根目录。
//
// 新根必须是存在的目录（或单个文件，此时进入单文件模式）。
// 路径校验（是否在允许范围内）由调用方负责 —— 这里只保证路径本身可用。
func (r *Resolver) SetRoot(root string) error {
	abs, single, name, err := normalizeRoot(root)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.root, r.singleFile, r.serveRootName = abs, single, name
	return nil
}

// SingleFile 返回单文件模式下被服务的文件名，非单文件模式返回空串。
func (r *Resolver) SingleFile() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.singleFile {
		return r.serveRootName
	}
	return ""
}

// Resolve 把 URL 路径映射为磁盘绝对路径。
// 返回的路径一定位于根目录之内（除非开启 allowSymlink）。
func (r *Resolver) Resolve(urlPath string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveLocked(urlPath)
}

func (r *Resolver) resolveLocked(urlPath string) (string, error) {
	clean := CleanURLPath(urlPath)
	if r.singleFile {
		// 单文件模式只暴露一个文件。
		if clean == "/" {
			return filepath.Join(r.root, r.serveRootName), nil
		}
		return filepath.Join(r.root, r.serveRootName), ErrEscaped
	}
	rel := strings.TrimPrefix(clean, "/")
	full := filepath.Join(r.root, filepath.FromSlash(rel))

	if !within(r.root, full) {
		return "", ErrEscaped
	}
	// 处理符号链接逃逸。
	if !r.allowSymlink {
		if err := EnsureWithin(r.root, full); err != nil {
			return "", err
		}
	}
	return full, nil
}

// EnsureWithin 确认 target 落在 root 之内 —— **沿已存在的那一段解析软链之后**。
//
// 与 IsWithin 的分工：IsWithin 是纯字符串前缀比较，看不懂软链；这里会解析
// target 中最长的那段**已存在**路径，因此能挡住「先往根里放一条指向根外的
// 软链，再借一次新建写入跳出去」这条路。
//
// ⚠️ 为什么不能只在 EvalSymlinks 成功时判断：叶子往往还不存在 ——
// PUT 一个新文件、MKCOL 一个新目录、MOVE 的目标、解压写出一个新条目都是
// 这种情况。EvalSymlinks 对不存在的路径直接返回 ENOENT，于是整段检查被跳过，
// 而随后的 os.OpenFile / os.MkdirAll 会老老实实跟着父目录里的软链写出去。
//
// root 必须已经是解析过软链的绝对路径（fsutil 的根目录构造已保证这一点）。
// 任何要把客户端给的路径落盘的地方都该过一下它：HTTP 层与解压层都用它，
// 只堵一边等于没堵。
func EnsureWithin(root, target string) error {
	// root 也解析一次：它本身可能就是一条软链（例如解压目标是个指向根内
	// 别处的软链目录）。只解析 target 的话，两边一个是解析后的真实路径、
	// 一个还是软链形式，前缀比对必然失败 —— 于是合法写入被误判成逃逸。
	// 正常情况下 root 已经被规范化过，这里是幂等的。
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	real, err := evalExistingAncestor(target)
	if err != nil {
		// 连一段存在的祖先都找不到：正常情况下根目录必然存在，
		// 所以按逃逸处理（宁可拒绝，不可放行）。
		return ErrEscaped
	}
	if !within(root, real) {
		return ErrEscaped
	}
	return nil
}

// evalExistingAncestor 解析 p 中最长的那段**已存在**的路径，
// 再把剩下（尚不存在）的部分原样接回去。
//
// 为什么要这么绕：EvalSymlinks 要求整条路径都存在，而这里恰恰要判断
// 「即将被创建」的路径 —— 它的父目录可能就是一条通往根目录之外的软链。
// 只解析存在的部分就够了：还不存在的那一段不可能是软链。
//
// ⚠️ 这不构成完整的 TOCTOU 防护：检查与随后的 OpenFile 之间，攻击者仍可能
// 把某个已经检查过的目录换成软链。要彻底堵住得用 openat/O_NOFOLLOW 逐段打开，
// 而本服务的威胁模型里「能在服务根内建软链的人」已经具备写权限，
// 这里挡住的是「预置一条软链，事后借一次写入跳出根目录」这条路径。
func evalExistingAncestor(p string) (string, error) {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real, nil
	}
	cur := filepath.Clean(p)
	rest := ""
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", ErrEscaped // 走到根了都没有存在的部分
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest), nil
		}
	}
}

// within 判断 p 是否位于 root 之内。
func within(root, p string) bool {
	if p == root {
		return true
	}
	// root 无尾分隔符时补一个，避免把 "/srv" 误判成 "/srvx" 的前缀。
	// 但 root 本身已是分隔符结尾（如文件系统根的 "/"，或 Windows 的 "C:\"）时
	// 不能再补 —— 补出来是 "//"，HasPrefix 恒为 false，
	// 结果就是「白名单写了 / 反而谁都不放行」。
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(p, root)
}

// IsWithin 判断 p 是否位于 base 之内（含 base 本身）。
//
// 只做纯字符串比较，不访问文件系统：白名单里的路径可能尚不存在，
// 调用方需要自己先把两边都规范化（Abs + EvalSymlinks）再传进来。
func IsWithin(base, p string) bool {
	if base == "" || p == "" {
		return false
	}
	return within(filepath.Clean(base), filepath.Clean(p))
}

// NormalizePath 把用户输入的路径规范化成绝对路径，供白名单校验使用。
//
// 与 normalizeRoot 的区别是**不要求路径存在**：管理员可能想先把根切到
// 一个尚未创建的目录（会失败并给出明确提示），也可能只是校验一下范围。
// evalSymlinks 为 true 时会对已存在的部分解析软链，避免用软链绕过白名单。
func NormalizePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if real, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return real, nil
	}
	// 路径不存在时，只解析它的父目录，其余部分拼回去。
	// 这样 /tmp/link-not-yet 里的 link 仍会被展开，绕不过白名单。
	if dir, base := filepath.Dir(abs), filepath.Base(abs); dir != abs {
		if realDir, rerr := filepath.EvalSymlinks(dir); rerr == nil {
			return filepath.Join(realDir, base), nil
		}
	}
	return abs, nil
}

// Rel 把磁盘路径转回相对根目录的 URL 路径（以 / 开头）。
func (r *Resolver) Rel(abs string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rel, err := filepath.Rel(r.root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "/", nil
	}
	return "/" + filepath.ToSlash(rel), nil
}

// CleanURLPath 归一化 URL 路径：确保以 / 开头、去除 . 与 ..、保留结尾斜杠语义。
func CleanURLPath(p string) string {
	if p == "" {
		return "/"
	}
	hadTrailing := strings.HasSuffix(p, "/")
	c := path.Clean("/" + p)
	if c != "/" && hadTrailing {
		c += "/"
	}
	return c
}

// HasParentSegment 判断路径中是否含有 ".." 段。
// CleanURLPath 会把 ".." 静默消解掉，这会掩盖调用方的真实意图，
// 因此涉及写盘的接口需要先用本函数显式拒绝，再走归一化。
func HasParentSegment(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// Entry 表示目录中的一个条目。
type Entry struct {
	Path    string `json:"path"`    // 相对根目录的 URL 路径
	Name    string `json:"name"`    // 文件名
	Mtime   string `json:"mtime"`   // 修改时间 RFC3339
	Size    int64  `json:"size"`    // 字节数，目录为 0
	IsDir   bool   `json:"is_dir"`  // 是否为目录
	Symlink bool   `json:"symlink"` // 是否为符号链接
	Ext     string `json:"ext"`     // 小写扩展名，不含点
	Archive bool   `json:"archive"` // 是否为可在线解压的压缩包
	// Editable 表示该文件属于可在线编辑的文本类型。
	// 只按文件名判定（不读内容），体积上限由前端结合 max_size 再判断。
	Editable bool `json:"editable"`
	// Lang 为语言标识（markdown / json / python …），前端据此选择预览方式。
	Lang string `json:"lang,omitempty"`
	// Site 表示这不是本目录里的真实条目，而是挂在根上的另一个站点。
	//
	// 前端据此打个标记并**隐藏会破坏它的操作**（移动 / 重命名 / 删除）——
	// 那种请求服务端也会以 403 拒绝（见 server 的根保护），但让按钮先别出现
	// 比点下去报错友好得多。
	Site bool `json:"site,omitempty"`
}

// Listing 表示一次目录列举的结果。
type Listing struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	IsDir    bool    `json:"is_dir"`
	Entries  []Entry `json:"entries"`
	Total    int     `json:"total"`
	DirCount int     `json:"dir_count"`
	FileSize int64   `json:"file_size"`
	// TotalAll 是目录里的实际条目总数（未截断时等于 Total）。
	TotalAll int `json:"total_all,omitempty"`
	// Truncated 表示因为超过条目上限而被截断。
	Truncated bool `json:"truncated,omitempty"`
}

// ReadDir 列举目录内容。hidden 为文件名 glob 列表，匹配到的条目会被隐藏。
// 目录在前、同类按名称排序（与常见文件管理器一致）。
//
// maxEntries 限制返回的条目数（<=0 表示不限制）。十万级文件的目录如果
// 一次性全部返回，光是序列化就会吃掉大量内存，响应体也会大到无法渲染；
// 截断后由 Truncated 标记告知调用方。
func ReadDir(res relator, dir string, hidden []string, maxEntries int) (*Listing, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &os.PathError{Op: "readdir", Path: dir, Err: errors.New("不是目录")}
	}

	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	// os.ReadDir 已经把整个目录读进内存了，这一步省不掉；
	// 但可以避免再把它放大成一份同样大的 Entry 切片 + JSON。
	total := len(items)
	if maxEntries > 0 && total > maxEntries {
		items = items[:maxEntries]
	}

	list := &Listing{
		Name:      info.Name(),
		IsDir:     true,
		Entries:   make([]Entry, 0, len(items)),
		TotalAll:  total,
		Truncated: total > len(items),
	}
	if rel, err := res.Rel(dir); err == nil {
		list.Path = rel
		list.Name = baseName(rel, info.Name())
	}

	for _, it := range items {
		name := it.Name()
		if isHidden(name, hidden) {
			continue
		}
		fi, err := it.Info()
		if err != nil {
			continue
		}
		abs := filepath.Join(dir, name)
		rel, err := res.Rel(abs)
		if err != nil {
			continue
		}
		e := Entry{
			Path:    rel,
			Name:    name,
			Mtime:   fi.ModTime().Format(time.RFC3339),
			IsDir:   it.IsDir(),
			Symlink: it.Type()&fs.ModeSymlink != 0,
		}
		if !e.IsDir {
			e.Size = fi.Size()
			e.Ext = strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
			e.Archive = IsArchiveName(name)
			if lang := textfile.Language(name); lang != "" {
				e.Editable = true
				e.Lang = lang
			}
		}
		list.Entries = append(list.Entries, e)
		if e.IsDir {
			list.DirCount++
		} else {
			list.FileSize += e.Size
		}
	}

	SortEntries(list.Entries)
	list.Total = len(list.Entries)
	return list, nil
}

// SortEntries 按 Listing 的约定给条目排序：目录在前，再按名字（忽略大小写）。
//
// 单独导出是因为「把挂载站点并进根目录列表」也要用同一套顺序 ——
// 各写一份迟早会出现「服务端 ?json 与页面看到的顺序不一样」。
func SortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

// baseName 在根目录时返回合适的展示名。
func baseName(rel, fallback string) string {
	c := strings.Trim(rel, "/")
	if c == "" {
		return "/"
	}
	i := strings.LastIndex(c, "/")
	if i >= 0 {
		return c[i+1:]
	}
	return c
}

// isHidden 判断文件名是否命中隐藏 glob。
func isHidden(name string, globs []string) bool {
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if g == "*/" {
			// 特殊写法：隐藏所有目录由调用方无法感知，此处按字面匹配处理。
			continue
		}
		if ok, err := path.Match(g, name); err == nil && ok {
			return true
		}
	}
	return false
}

// SearchMode 为搜索的匹配方式。
type SearchMode int

const (
	// SearchFuzzy 忽略大小写的「包含」匹配（默认，也是历史上唯一的行为）。
	SearchFuzzy SearchMode = iota
	// SearchExact 名称或相对路径与关键字完全相等（忽略大小写）。
	SearchExact
	// SearchWildcard 用 path.Match 的 glob 语法匹配名称或相对路径。
	SearchWildcard
	// SearchRegex 用 Go regexp（RE2，线性时间）匹配名称或相对路径，忽略大小写。
	SearchRegex
)

// ParseSearchMode 把请求里的字符串解析成 SearchMode。
//
// 未知值与空串一律回落到 SearchFuzzy —— 这样旧客户端不带 mode 时行为不变，
// 拼错的值也不会变成「一个都不命中」这种更难排查的结果。
func ParseSearchMode(s string) SearchMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "exact", "equal", "equals":
		return SearchExact
	case "wildcard", "glob", "match":
		return SearchWildcard
	case "regex", "regexp", "re":
		return SearchRegex
	default:
		return SearchFuzzy
	}
}

// SearchOptions 是 Search 的参数。
//
// 用结构体而不是一串位置参数：模式是后加的，若继续按位置传，
// 调用点会变成 Search(res, root, p, mode, hidden, limit) 这种一眼看不懂顺序的调用。
type SearchOptions struct {
	// Pattern 为搜索关键字（表达式），必填。
	Pattern string
	// Mode 为匹配方式，零值为 SearchFuzzy。
	Mode SearchMode
	// Hidden 为文件名 glob 列表，匹配到的条目及其子树会被跳过。
	Hidden []string
	// Limit 为返回上限，0 表示不限制。
	Limit int
}

// searchMatcher 把一次搜索的表达式预先编译好。
//
// 之所以要单独一个类型：正则/通配符的编译是有成本的，遍历前编译一次、
// 之后每个条目只做匹配，而不是在 WalkDir 里对每个文件重复 Compile。
type searchMatcher struct {
	mode   SearchMode
	needle string         // Fuzzy/Exact 用：小写关键字
	glob   string         // Wildcard 用：小写 glob
	re     *regexp.Regexp // Regex 用：已编译（带 (?i)）
}

// newSearchMatcher 编译表达式；表达式非法时返回 ErrBadSearchPattern。
func newSearchMatcher(mode SearchMode, pattern string) (*searchMatcher, error) {
	m := &searchMatcher{mode: mode}
	switch mode {
	case SearchExact:
		m.needle = strings.ToLower(strings.TrimSpace(pattern))
	case SearchWildcard:
		m.glob = strings.ToLower(strings.TrimSpace(pattern))
		// 先拿一个空串试匹配，语法错误会在这里暴露 —— 否则错误要等到
		// 遍历到第一个条目才出现，白白走一遍文件系统。
		if _, err := path.Match(m.glob, ""); err != nil {
			return nil, fmt.Errorf("%w：通配符写法有误（%v）", ErrBadSearchPattern, err)
		}
	case SearchRegex:
		// (?i) 让正则忽略大小写，与其它模式口径一致（本服务的路径匹配
		// 一律不区分大小写）。Go 的 regexp 是 RE2，不存在灾难性回溯。
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			return nil, fmt.Errorf("%w：正则表达式有误（%v）", ErrBadSearchPattern, err)
		}
		m.re = re
	default:
		m.mode = SearchFuzzy
		m.needle = strings.ToLower(strings.TrimSpace(pattern))
	}
	return m, nil
}

// match 判断某个条目是否命中。name 为条目名，relPath 为相对搜索根的斜杠路径。
//
// 每个模式都是「名称 或 相对路径」取或：
//   - 名称一侧让 `*.log`、`readme` 这类只关心文件名的写法能命中任意目录下的文件；
//   - 路径一侧让 `docs/*.md`、`docs/readme.md` 这类带目录的写法能用。
//
// path.Match 的 `*` 不跨 `/`，所以 `*.log` 天然只会命中名字（路径里带 `/`），
// `docs/*.md` 天然只会命中路径 —— 两个条件既互补又不会互相误伤。
func (m *searchMatcher) match(name, relPath string) bool {
	switch m.mode {
	case SearchExact:
		// 只对表达式做 trim（用户手输难免多空格），文件名与路径保持原样 ——
		// 否则一个名字里真带前导空格的条目会被误判为「完全相等」。
		return strings.EqualFold(name, m.needle) ||
			strings.EqualFold(relPath, m.needle)
	case SearchWildcard:
		return globMatch(m.glob, name) || globMatch(m.glob, relPath)
	case SearchRegex:
		return m.re.MatchString(name) || m.re.MatchString(relPath)
	default:
		return strings.Contains(strings.ToLower(name), m.needle) ||
			strings.Contains(strings.ToLower(relPath), m.needle)
	}
}

// globMatch 为已预校验过语法的 glob 做匹配（此处可安全忽略错误）。
func globMatch(glob, s string) bool {
	ok, err := path.Match(glob, strings.ToLower(s))
	return err == nil && ok
}

// Search 在指定目录下递归查找匹配的条目（不区分大小写）。
//
// 匹配目标同时包含文件名与相对路径，具体语义见 SearchMode 与 searchMatcher.match。
//
// 返回 entries 与**命中总数** total。达到 Limit 之后仍会把剩下的目录走完，
// 但只计数、不再构造 Entry —— 否则调用方没法如实告诉用户「共 N 项命中，
// 只列出前 M 项」。早先是到了上限直接 SkipAll 停掉，于是 5000 个命中只回
// 3000，而界面上一点被截断的迹象都没有：用户以为那就是全部。
// 多走一遍不额外分配内存，代价只是遍历本身（实测 5000 文件 20ms）。
func Search(res relator, root string, opt SearchOptions) (entries []Entry, total int, err error) {
	matcher, err := newSearchMatcher(opt.Mode, opt.Pattern)
	if err != nil {
		// 表达式非法：直接返回，连目录都不遍历。
		return nil, 0, err
	}
	out := make([]Entry, 0, 32)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// 单个条目权限不足等情况直接跳过。
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// 根目录自身不进结果，但必须继续往下走。先判根再判隐藏：隐藏规则
		// 只作用于根以下的条目 —— 搜索根的名字恰好命中隐藏 glob 时
		// （例如 --hidden tmp 时在 tmp 目录里搜），整棵树仍应被遍历。
		if p == root {
			return nil
		}
		if isHidden(d.Name(), opt.Hidden) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relPath := ""
		if r, rerr := filepath.Rel(root, p); rerr == nil {
			relPath = filepath.ToSlash(r)
		}
		if !matcher.match(d.Name(), relPath) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := res.Rel(p)
		if err != nil {
			return nil
		}
		e := Entry{
			Path:    rel,
			Name:    d.Name(),
			Mtime:   fi.ModTime().Format(time.RFC3339),
			IsDir:   d.IsDir(),
			Symlink: d.Type()&fs.ModeSymlink != 0,
		}
		if !e.IsDir {
			e.Size = fi.Size()
			e.Ext = strings.ToLower(strings.TrimPrefix(filepath.Ext(d.Name()), "."))
			e.Archive = IsArchiveName(d.Name())
			if lang := textfile.Language(d.Name()); lang != "" {
				e.Editable = true
				e.Lang = lang
			}
		}
		total++
		if opt.Limit > 0 && len(out) >= opt.Limit {
			// 到上限了：继续走完，但只计数（见上面的说明）。
			return nil
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, total, nil
}

// archiveSuffixes 为支持在线解压的扩展名（长后缀优先）。
var archiveSuffixes = []string{".tar.gz", ".tar.bz2", ".tgz", ".tar", ".zip", ".gz"}

// IsArchiveName 判断文件名是否为可在线解压的压缩包。
func IsArchiveName(name string) bool {
	l := strings.ToLower(name)
	for _, s := range archiveSuffixes {
		if strings.HasSuffix(l, s) {
			return true
		}
	}
	return false
}
