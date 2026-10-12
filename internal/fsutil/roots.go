package fsutil

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// 多站点服务：一个**主目录**始终服务在 `/`，另外若干个**附加站点**挂在
// `/<标签>/` 下。
//
//	/foo.txt     → 主目录的 foo.txt
//	/backup/x    → 标签为 backup 的附加站点下的 x
//
// 为什么主目录不带前缀：站点是**运行期**可增删的，若「有第二个站点时大家
// 都带前缀」，那加上第二个站点的瞬间主目录的地址就变了 —— 所有书签与正在
// 浏览的页面都会失效。主目录钉在 `/`，增删站点就只影响新站点自己。
//
// 代价是 `/标签` 会遮蔽主目录下的同名子目录（主目录里有个 backup/，同时又
// 有标签 backup 的站点时，/backup/x 走站点）。这是这套 URL 形状的固有取舍。
//
// 路由按 URL 首段认标签，而 Rel 按磁盘路径包含关系反查 —— 两者只有在
// 各根**互不嵌套**时才一致，所以 BuildRoots 与 AddSite 都会拒绝嵌套。

var (
	// ErrNoRoot 已不再由 Resolve 产生（混合形状下首段不匹配就是主目录下的
	// 普通路径），保留它是为了兼容其它包里的引用与错误判断。
	ErrNoRoot = errors.New("未知的挂载点")

	// ErrOutsideRoots 表示磁盘路径不在任何挂载根之下（Rel 用）。
	ErrOutsideRoots = errors.New("路径不在任何挂载根下")

	// ErrUnknownSite 表示没有这个标签的附加站点（移除时用）。
	ErrUnknownSite = errors.New("没有这个标签的站点")
	// ErrPrimarySite 表示试图移除主服务目录。
	ErrPrimarySite = errors.New("不能移除主服务目录")
	// ErrDuplicateSite 表示该目录已经在服务中。
	ErrDuplicateSite = errors.New("该目录已经在服务中")
	// ErrNestedRoot 表示两个目录互相嵌套。
	ErrNestedRoot = errors.New("服务目录不能互相嵌套")
)

// Mount 是一个挂载点。mounts[0] 是主目录，其余是附加站点。
type Mount struct {
	// Label 是展示用标签。对附加站点而言它就是 URL 首段；主目录的标签
	// **不参与路由**（主目录服务在 `/`），只用于展示。
	Label string
	// Res 承载这个根的解析器（含它自己的单文件模式）。
	Res *Resolver
}

// reservedLabels 是已被别处占用的 URL 首段，附加站点的标签必须避开。
//
//   - fs：mux 上的内部接口（/fs/health、/fs/settings…）
//   - s：路径形式的分享地址（/s/<token>/…）
var reservedLabels = map[string]bool{"fs": true, "s": true}

// Roots 是多站点模式的路径解析器，方法集与 *Resolver 对齐，
// 因此 server 侧把它当同一个东西用。
//
// 它**在运行期可增删站点**，所有状态都在 mu 之下；调用方持有的是同一个
// *Roots 指针，永远不需要替换它（那才是并发下最容易出错的地方）。
type Roots struct {
	mu           sync.RWMutex
	allowSymlink bool
	// mounts[0] 是主目录，[1:] 是附加站点（按加入顺序）。
	mounts []Mount
	// byLabel 只索引**附加站点**：标签 -> mounts 下标。
	//
	// 存下标而不是 &mounts[i]：append 扩容会让旧指针指向被丢弃的旧数组，
	// 之后 Resolve 读到的就是悬空数据。下标在重建后始终正确。
	byLabel map[string]int
	// usedLabels 是保留字 + 已分配过的标签。**只增不减**：移除站点后标签
	// 不再复用，这样一张旧书签永远不会被悄悄指到另一个目录上。
	usedLabels map[string]bool
}

// BuildRoots 由一组根路径构造多站点解析器：第一个是主目录，其余是附加站点。
//
// 会逐个建解析器、按解析后的路径去重、拒绝互相嵌套，并为附加站点分配标签。
func BuildRoots(paths []string, allowSymlink bool) (*Roots, error) {
	if len(paths) == 0 {
		return nil, errors.New("至少要有一个服务目录")
	}
	r := &Roots{
		allowSymlink: allowSymlink,
		byLabel:      map[string]int{},
		usedLabels:   map[string]bool{},
	}
	for l := range reservedLabels {
		r.usedLabels[l] = true
	}

	for _, p := range paths {
		res, err := NewResolver(p, allowSymlink)
		if err != nil {
			return nil, fmt.Errorf("服务目录 %q 不可用: %w", p, err)
		}
		if err := r.appendRoot(res); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// appendRoot 把一个新的根挂上（内部用，调用方需自己持有/不需锁 —— 构造期）。
func (r *Roots) appendRoot(res *Resolver) error {
	root := res.Root()
	for _, m := range r.mounts {
		if m.Res.Root() == root {
			// 同一个路径传两次只留一个，否则会出现两个标签指向同一份文件。
			return nil
		}
	}
	for _, m := range r.mounts {
		if err := checkNested(root, m.Res.Root()); err != nil {
			return err
		}
	}

	m := Mount{Res: res}
	if len(r.mounts) == 0 {
		// 主目录：标签只用于展示，但也要占住，免得附加站点抢走它。
		m.Label = sanitizeLabel(labelFor(m))
		if m.Label == "" {
			m.Label = "root"
		}
		r.usedLabels[m.Label] = true
		r.mounts = append(r.mounts, m)
		return nil
	}

	m.Label = r.allocLabel(labelFor(m))
	r.mounts = append(r.mounts, m)
	r.byLabel[m.Label] = len(r.mounts) - 1
	return nil
}

// checkNested 双向检查两个根是否互相嵌套。
//
// 嵌套会让「按首段路由」与「按包含关系 Rel」打架：/a 与 /a/b 同时挂载时，
// /a/b/x 路由到 a 根，但 Rel(/a/b/x) 会认成更长匹配的 a/b 根 —— 同一个文件
// 两个地址。直接拒绝，别让用户猜。
func checkNested(a, b string) error {
	if a == b {
		return nil
	}
	if IsWithin(a, b) || IsWithin(b, a) {
		return fmt.Errorf("%w：%s 与 %s", ErrNestedRoot, a, b)
	}
	return nil
}

// allocLabel 分配一个未被占用、也不在保留字里的标签。调用方需持有写锁。
func (r *Roots) allocLabel(base string) string {
	if base == "" {
		base = "root"
	}
	label := base
	for n := 2; r.usedLabels[label]; n++ {
		label = fmt.Sprintf("%s-%d", base, n)
	}
	r.usedLabels[label] = true
	return label
}

// labelFor 由根路径推一个默认标签：目录名（单文件模式取文件名）。
func labelFor(m Mount) string {
	src := m.Res.Root()
	if name := m.Res.SingleFile(); name != "" {
		src = name
	}
	label := sanitizeLabel(filepath.Base(src))
	if label == "" || label == "." || label == string(filepath.Separator) {
		label = "root"
	}
	return label
}

// sanitizeLabel 清洗标签：它会被直接拼进 URL 与面包屑，不能带分隔符或
// 会被转义/截断的字符。
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			// 控制字符直接丢掉：URL、标签页与日志里都麻烦。
		case r == '/' || r == '\\' || r == '#' || r == '?' || r == '%':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}

// rebuildByLabel 按当前 mounts 重建标签索引。调用方需持有写锁。
//
// 删除中间元素会让后面的下标整体前移，所以每次都重建而不是增量改。
func (r *Roots) rebuildByLabel() {
	r.byLabel = make(map[string]int, len(r.mounts))
	for i := 1; i < len(r.mounts); i++ {
		r.byLabel[r.mounts[i].Label] = i
	}
}

// Mounts 返回全部挂载点（[0] 是主目录），供界面展示与 banner 使用。
func (r *Roots) Mounts() []Mount {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Mount, len(r.mounts))
	copy(out, r.mounts)
	return out
}

// Primary 返回主目录挂载点。
func (r *Roots) Primary() Mount {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mounts[0]
}

// HasExtras 表示除主目录外还有附加站点。
func (r *Roots) HasExtras() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.mounts) > 1
}

// Labels 返回附加站点的标签（按加入顺序）。
func (r *Roots) Labels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.mounts)-1)
	for i := 1; i < len(r.mounts); i++ {
		out = append(out, r.mounts[i].Label)
	}
	return out
}

// IsRoot 判断 abs 是否是某个挂载根（主目录或附加站点）本身。
//
// 删除/移动的根保护要用它：`DELETE /<标签>?recursive` 解析出来正是某个
// 站点目录本身，只看 URL 是不是 "/" 是拦不住的 —— 那会把整个站点目录删掉。
func (r *Roots) IsRoot(abs string) bool {
	clean := filepath.Clean(abs)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.mounts {
		if r.mounts[i].Res.Root() == clean {
			return true
		}
	}
	return false
}

// Resolve 把 URL 路径映射为磁盘绝对路径。
//
// 首段命中某个附加站点的标签 → 交给该站点；否则整条路径交给主目录。
// 注意 `/nope/x` 在混合形状下**不是**「未知前缀」，它就是主目录下的
// nope/x（GET 不存在才 404，PUT 会把它创建出来）。
func (r *Roots) Resolve(urlPath string) (string, error) {
	clean := CleanURLPath(urlPath)
	trimmed := strings.TrimPrefix(clean, "/")
	first, rest := trimmed, "/"
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		first, rest = trimmed[:i], trimmed[i:]
	}

	// 只在锁内取出需要的那一个 *Resolver，随后放锁再做文件系统操作
	// （EvalSymlinks 之类可能很慢，没必要阻塞增删站点）。
	r.mu.RLock()
	res := r.mounts[0].Res
	if idx, ok := r.byLabel[first]; ok {
		res = r.mounts[idx].Res
	} else {
		rest = clean // 走主目录：用完整路径（含前导 /）
	}
	r.mu.RUnlock()

	return res.Resolve(rest)
}

// Rel 把磁盘路径转回 URL 路径。
//
// 落在主目录下就是普通相对路径（无前缀）；落在某个附加站点下才带标签。
func (r *Roots) Rel(abs string) (string, error) {
	clean := filepath.Clean(abs)

	r.mu.RLock()
	primary := r.mounts[0].Res.Root()
	if within(primary, clean) {
		r.mu.RUnlock()
		return relFrom(primary, clean, "")
	}
	// 附加站点互不嵌套（构建与新增时都拒绝过），所以包含它的至多一个；
	// 仍然取最长匹配作为兜底。
	best, bestLen, bestLabel := "", -1, ""
	for i := 1; i < len(r.mounts); i++ {
		root := r.mounts[i].Res.Root()
		if within(root, clean) && len(root) > bestLen {
			best, bestLen, bestLabel = root, len(root), r.mounts[i].Label
		}
	}
	r.mu.RUnlock()

	if best == "" {
		return "", ErrOutsideRoots
	}
	return relFrom(best, clean, bestLabel)
}

// relFrom 把 root 下的 abs 拼成带前缀的 URL 路径。
func relFrom(root, abs, label string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	prefix := ""
	if label != "" {
		prefix = "/" + label
	}
	if rel == "." {
		if prefix == "" {
			return "/", nil
		}
		return prefix, nil
	}
	return prefix + "/" + filepath.ToSlash(rel), nil
}

// Root 返回**主目录**的绝对路径。
func (r *Roots) Root() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mounts[0].Res.Root()
}

// SingleFile 委托给主目录（单文件模式只可能是主目录）。
func (r *Roots) SingleFile() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mounts[0].Res.SingleFile()
}

// SetRoot 切换**主目录**，即单根时的运行期换根。
//
// 附加站点各自的根不受影响。有附加站点时调用方（server）会先拒绝 ——
// 那时「换根」的语义不清（换哪一个？其余的呢？）。
func (r *Roots) SetRoot(root string) error {
	r.mu.RLock()
	res := r.mounts[0].Res
	r.mu.RUnlock()
	return res.SetRoot(root)
}

// AddSite 新增一个附加站点，返回挂载点（含分配到的标签）。
//
// wantLabel 为空或已被占用时按目录名自动分配。新站点不能与现有任何根
// 相同（去重）或互相嵌套。
func (r *Roots) AddSite(path, wantLabel string) (Mount, error) {
	res, err := NewResolver(path, r.allowSymlink)
	if err != nil {
		return Mount{}, err
	}
	root := res.Root()

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, m := range r.mounts {
		if m.Res.Root() == root {
			return Mount{}, ErrDuplicateSite
		}
	}
	for _, m := range r.mounts {
		if err := checkNested(root, m.Res.Root()); err != nil {
			return Mount{}, err
		}
	}

	// 指定了标签就尽量尊重它：被占了加后缀（自定义-2），而不是悄悄换成
	// 目录名 —— 用户填了名字却拿到另一个，比报错还难理解。
	base := sanitizeLabel(wantLabel)
	if base == "" {
		base = labelFor(Mount{Res: res})
	}
	label := r.allocLabel(base)

	r.mounts = append(r.mounts, Mount{Label: label, Res: res})
	r.rebuildByLabel()
	return r.mounts[len(r.mounts)-1], nil
}

// RemoveSite 按标签移除一个附加站点。
//
// 主目录不能移除；被移除的标签**不会**被后续分配复用（见 usedLabels 的说明）。
func (r *Roots) RemoveSite(label string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	idx, ok := r.byLabel[label]
	if !ok {
		if len(r.mounts) > 0 && r.mounts[0].Label == label {
			return ErrPrimarySite
		}
		return ErrUnknownSite
	}
	r.mounts = append(r.mounts[:idx], r.mounts[idx+1:]...)
	r.rebuildByLabel()
	return nil
}
