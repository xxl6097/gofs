// Package fsutil 提供受限目录下的路径解析与目录列举能力。
package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/uuxia/gofs/internal/textfile"
)

// ErrEscaped 表示解析出的路径逃逸出了服务根目录。
var ErrEscaped = errors.New("路径超出了服务根目录")

// Resolver 在固定根目录内做安全的路径解析。
type Resolver struct {
	root          string // 已求绝对路径的根目录
	allowSymlink  bool   // 是否允许符号链接逃逸
	singleFile    bool   // 根节点是单个文件而非目录
	serveRootName string // 单文件模式下的文件名
}

// NewResolver 创建解析器。root 会被展开为绝对路径。
func NewResolver(root string, allowSymlink bool) (*Resolver, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	// 根目录自身可能位于符号链接之下（macOS 上 /tmp 指向 /private/tmp），
	// 必须先解析为真实路径，否则每次 EvalSymlinks 后的前缀比对都会失败，
	// 导致所有请求被误判为「逃逸出根目录」。
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	r := &Resolver{root: abs, allowSymlink: allowSymlink}
	if !info.IsDir() {
		r.singleFile = true
		r.serveRootName = filepath.Base(abs)
		r.root = filepath.Dir(abs)
	}
	return r, nil
}

// Root 返回服务根目录的绝对路径。
func (r *Resolver) Root() string { return r.root }

// SingleFile 返回单文件模式下被服务的文件名，非单文件模式返回空串。
func (r *Resolver) SingleFile() string {
	if r.singleFile {
		return r.serveRootName
	}
	return ""
}

// Resolve 把 URL 路径映射为磁盘绝对路径。
// 返回的路径一定位于根目录之内（除非开启 allowSymlink）。
func (r *Resolver) Resolve(urlPath string) (string, error) {
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

	if !r.within(full) {
		return "", ErrEscaped
	}
	// 处理符号链接逃逸：仅当目标存在且开启软链逃逸时放行。
	if !r.allowSymlink {
		real, err := filepath.EvalSymlinks(full)
		if err == nil {
			if !r.within(real) {
				return "", ErrEscaped
			}
		}
	}
	return full, nil
}

// within 判断 p 是否位于根目录之内。
func (r *Resolver) within(p string) bool {
	if p == r.root {
		return true
	}
	return strings.HasPrefix(p, r.root+string(filepath.Separator))
}

// Rel 把磁盘路径转回相对根目录的 URL 路径（以 / 开头）。
func (r *Resolver) Rel(abs string) (string, error) {
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
}

// ReadDir 列举目录内容。hidden 为文件名 glob 列表，匹配到的条目会被隐藏。
// 目录在前、同类按名称排序（与常见文件管理器一致）。
func ReadDir(res *Resolver, dir string, hidden []string) (*Listing, error) {
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

	list := &Listing{
		Name:    info.Name(),
		IsDir:   true,
		Entries: make([]Entry, 0, len(items)),
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

	sort.SliceStable(list.Entries, func(i, j int) bool {
		a, b := list.Entries[i], list.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	list.Total = len(list.Entries)
	return list, nil
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

// Search 在指定目录下递归查找名称匹配 pattern 的条目（不区分大小写）。
// limit 为返回上限，0 表示不限制。
func Search(res *Resolver, root string, pattern string, hidden []string, limit int) ([]Entry, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	out := make([]Entry, 0, 32)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// 单个条目权限不足等情况直接跳过。
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != root && isHidden(d.Name(), hidden) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		if !strings.Contains(strings.ToLower(d.Name()), pattern) {
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
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
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
