package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/xxl6097/gofs/internal/fsutil"
)

// errPathChanged 表示“授权时看到的文件”和“真正打开的文件”不是同一个。
//
// 单靠路径字符串无法防住软链/目录替换：ACL 检查完到 os.Open 之间，
// 攻击者可以把叶子换成指向敏感文件的软链，或把父目录换掉。这里在授权时
// 记录 inode 快照，打开后再用 SameFile 复核；对新建文件则复核父目录。
var errPathChanged = errors.New("路径在授权后发生变化")

// authorizedPath 是一次授权时看到的真实路径快照。
type authorizedPath struct {
	real       string
	info       os.FileInfo // nil 表示授权时目标尚不存在（新建）
	parent     string
	parentInfo os.FileInfo
}

func snapshotAuthorized(real string) *authorizedPath {
	ap := &authorizedPath{real: real}
	if fi, err := os.Lstat(real); err == nil {
		ap.info = fi
	}
	// 目标可能位于还不存在的目录里（新建上传）。记录**最近的已存在祖先**，
	// MkdirAll 之后再复核它没有被换掉。更严格的 openat 逐段校验属于后续工作。
	parent := filepath.Dir(real)
	for {
		if pfi, err := os.Lstat(parent); err == nil {
			ap.parent = parent
			ap.parentInfo = pfi
			break
		}
		up := filepath.Dir(parent)
		if up == parent || up == "." || up == string(filepath.Separator) {
			break
		}
		parent = up
	}
	return ap
}

func (ap *authorizedPath) parentUnchanged() bool {
	if ap == nil || ap.parent == "" || ap.parentInfo == nil {
		return false
	}
	pfi, err := os.Lstat(ap.parent)
	return err == nil && os.SameFile(pfi, ap.parentInfo)
}

// openRead 打开授权时的真实文件，并确认打开的 inode 与授权快照一致。
func (ap *authorizedPath) openRead() (*os.File, error) {
	if ap == nil || ap.real == "" || ap.info == nil {
		return nil, errPathChanged
	}
	f, err := os.Open(ap.real)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(fi, ap.info) {
		_ = f.Close()
		return nil, errPathChanged
	}
	return f, nil
}

// openWrite 打开授权时的真实目标；对覆盖写先验证 inode 再 truncate，
// 避免“先截断，再发现打错了文件”。
func (ap *authorizedPath) openWrite(flags int, perm os.FileMode) (*os.File, error) {
	if ap == nil || ap.real == "" {
		return nil, errPathChanged
	}
	wantTrunc := flags&os.O_TRUNC != 0
	flags &^= os.O_TRUNC

	if ap.info == nil {
		// 新建：先确认父目录没有在授权后被换掉。
		if !ap.parentUnchanged() {
			return nil, errPathChanged
		}
		f, err := os.OpenFile(ap.real, flags|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return nil, errPathChanged
			}
			return nil, err
		}
		// 再复核一次父目录；失败时把刚创建的空文件清掉。
		if !ap.parentUnchanged() {
			_ = f.Close()
			_ = os.Remove(ap.real)
			return nil, errPathChanged
		}
		return f, nil
	}

	f, err := os.OpenFile(ap.real, flags, perm)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(fi, ap.info) {
		_ = f.Close()
		return nil, errPathChanged
	}
	if wantTrunc {
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// mountRootFor 返回包含 real 的最长服务根（用于 os.Root 逐段安全打开）。
func (s *Server) mountRootFor(real string) string {
	if s == nil || s.res == nil {
		return ""
	}
	best := ""
	for _, m := range s.res.Mounts() {
		root := m.Res.Root()
		if fsutil.IsWithin(root, real) && len(root) > len(best) {
			best = root
		}
	}
	return best
}

// openUnderMount 用 os.Root 在服务根内打开 real。
//
// os.Root 从根目录 fd 出发逐段解析，即使中途有目录被换成指向根外的
// 软链也会被拒绝，不会像普通 os.Open 那样跟出去。
// ok=false 表示 real 不在任何挂载根内（例如 --allow-symlink 指向根外），
// 调用方退回带 inode 快照的普通打开。
func (s *Server) openUnderMount(real string, flags int, perm os.FileMode) (f *os.File, ok bool, err error) {
	root := s.mountRootFor(real)
	if root == "" {
		return nil, false, nil
	}
	rel, rerr := filepath.Rel(root, real)
	if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false, nil
	}
	rootFS, rerr := os.OpenRoot(root)
	if rerr != nil {
		return nil, true, rerr
	}
	defer rootFS.Close()
	if flags == os.O_RDONLY {
		// 读取（包括目录列举）用 Root.Open：Windows 上 OpenFile 对目录的
		// 行为与 Unix 不一致，Open 的语义更稳定。
		f, err = rootFS.Open(rel)
	} else {
		f, err = rootFS.OpenFile(rel, flags, perm)
	}
	if err != nil {
		return nil, true, err
	}
	return f, true, nil
}

// secureOpenRead 打开一个已经授权过的路径。
//
// 如果本次请求没有记录快照（单文件模式、某些内部调用），从当前路径
// 现取一个快照再打开：至少能挡住“快照之后”的替换。
func (s *Server) secureOpenRead(r *http.Request, urlPath, abs string) (*os.File, error) {
	ap := s.authorizedPathFor(r, urlPath)
	if ap == nil {
		real, err := fsutil.ResolveReal(abs)
		if err != nil {
			return nil, err
		}
		ap = snapshotAuthorized(real)
	}
	if f, ok, err := s.openUnderMount(ap.real, os.O_RDONLY, 0); ok {
		if err != nil {
			return nil, err
		}
		if ap.info == nil {
			_ = f.Close()
			return nil, errPathChanged
		}
		fi, serr := f.Stat()
		if serr != nil || !os.SameFile(fi, ap.info) {
			_ = f.Close()
			if serr != nil {
				return nil, serr
			}
			return nil, errPathChanged
		}
		return f, nil
	}
	return ap.openRead()
}

// readFileAuthorized 通过授权快照读取整个文件。
func (s *Server) readFileAuthorized(r *http.Request, urlPath, abs string) ([]byte, error) {
	f, err := s.secureOpenRead(r, urlPath, abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// secureOpenWrite 与 secureOpenRead 对称，用于写入/创建。
func (s *Server) secureOpenWrite(r *http.Request, urlPath, abs string, flags int, perm os.FileMode) (*os.File, error) {
	ap := s.authorizedPathFor(r, urlPath)
	if ap == nil {
		real, err := fsutil.ResolveReal(abs)
		if err != nil {
			return nil, err
		}
		ap = snapshotAuthorized(real)
	}
	wantTrunc := flags&os.O_TRUNC != 0
	openFlags := flags &^ os.O_TRUNC
	if ap.info == nil {
		// 新建时用 O_EXCL：授权后若有人抢先把路径变成软链/文件，必须失败。
		openFlags |= os.O_CREATE | os.O_EXCL
	} else {
		// 授权时目标已存在：不要因为它在检查后被删掉就顺手新建一个。
		openFlags &^= os.O_CREATE
	}
	if f, ok, err := s.openUnderMount(ap.real, openFlags, perm); ok {
		if err != nil {
			if ap.info == nil && errors.Is(err, os.ErrExist) {
				return nil, errPathChanged
			}
			return nil, err
		}
		if ap.info != nil {
			fi, serr := f.Stat()
			if serr != nil || !os.SameFile(fi, ap.info) {
				_ = f.Close()
				if serr != nil {
					return nil, serr
				}
				return nil, errPathChanged
			}
		}
		if wantTrunc {
			if err := f.Truncate(0); err != nil {
				_ = f.Close()
				return nil, err
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				_ = f.Close()
				return nil, err
			}
		}
		return f, nil
	}
	return ap.openWrite(flags, perm)
}

// secureMkdirAll 在服务根内逐级创建目录。
//
// 每一级都用 os.Root 相对根目录 fd 操作，即使中间目录被换成根外软链，
// 也不会在根外建目录；根外目标（--allow-symlink）退回 os.MkdirAll。
func (s *Server) secureMkdirAll(target string, perm os.FileMode) error {
	real, err := fsutil.ResolveReal(target)
	if err != nil {
		return os.MkdirAll(target, perm)
	}
	root := s.mountRootFor(real)
	if root == "" {
		return os.MkdirAll(target, perm)
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	rel, err := filepath.Rel(root, real)
	if err != nil {
		return os.MkdirAll(target, perm)
	}
	if rel == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		if cur == "" {
			cur = part
		} else {
			cur += "/" + part
		}
		name := filepath.FromSlash(cur)
		fi, serr := rootFS.Stat(name)
		if serr == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s 不是目录", target)
			}
			continue
		}
		if !errors.Is(serr, os.ErrNotExist) {
			return serr
		}
		if merr := rootFS.Mkdir(name, perm); merr != nil && !errors.Is(merr, os.ErrExist) {
			return merr
		}
	}
	return nil
}

// searchSecure 在授权快照对应的目录内做递归搜索。
//
// 服务根内的搜索走 os.Root.FS()，遍历过程中即使目录被换成根外软链也不会
// 逃逸；根外（--allow-symlink）目标退回原有路径遍历。
func (s *Server) searchSecure(r *http.Request, urlPath, abs string, opt fsutil.SearchOptions) ([]fsutil.Entry, int, error) {
	real, err := fsutil.ResolveReal(abs)
	if err != nil {
		return nil, 0, err
	}
	mountRoot := s.mountRootFor(real)
	if mountRoot == "" {
		return fsutil.Search(s.res, abs, opt)
	}
	rootFS, err := os.OpenRoot(mountRoot)
	if err != nil {
		return nil, 0, err
	}
	defer rootFS.Close()
	startRel, err := filepath.Rel(mountRoot, real)
	if err != nil {
		return fsutil.Search(s.res, abs, opt)
	}
	if ap := s.authorizedPathFor(r, urlPath); ap != nil && ap.info != nil {
		fi, serr := rootFS.Stat(startRel)
		if serr != nil {
			return nil, 0, serr
		}
		if !os.SameFile(fi, ap.info) {
			return nil, 0, errPathChanged
		}
	}
	return fsutil.SearchFS(rootFS.FS(), filepath.ToSlash(startRel), real, mountRoot, s.res, opt)
}

// mountRel 把绝对路径映射为「挂载根 + 根内相对路径 + 打开的 os.Root」。
// ok=false 表示路径不在任何挂载根内（--allow-symlink 的根外目标）。
func (s *Server) mountRel(path string) (rootFS *os.Root, root, rel string, ok bool, err error) {
	real, err := fsutil.ResolveReal(path)
	if err != nil {
		return nil, "", "", false, err
	}
	root = s.mountRootFor(real)
	if root == "" {
		return nil, "", "", false, nil
	}
	rel, err = filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", "", false, nil
	}
	rootFS, err = os.OpenRoot(root)
	if err != nil {
		return nil, "", "", true, err
	}
	return rootFS, root, rel, true, nil
}

// secureRemoveAll 在挂载根内安全地递归删除；叶子是软链时只删链接本身。
func (s *Server) secureRemoveAll(target string) error {
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(target)
	}
	rootFS, _, rel, ok, err := s.mountRel(target)
	if err != nil {
		if ok {
			return err
		}
		return os.RemoveAll(target)
	}
	if !ok {
		return os.RemoveAll(target)
	}
	defer rootFS.Close()
	return rootFS.RemoveAll(rel)
}

// secureRemove 与 secureRemoveAll 相同，但只删除单个条目。
func (s *Server) secureRemove(target string) error {
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(target)
	}
	rootFS, _, rel, ok, err := s.mountRel(target)
	if err != nil {
		if ok {
			return err
		}
		return os.Remove(target)
	}
	if !ok {
		return os.Remove(target)
	}
	defer rootFS.Close()
	return rootFS.Remove(rel)
}

// secureRename 在同一挂载根内用 os.Root 重命名；叶子是软链时按链接本身
// 处理，跨根/根外退回 os.Rename。
func (s *Server) secureRename(src, dst string) error {
	if fi, err := os.Lstat(src); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return os.Rename(src, dst)
	}
	srcRoot, srcRootPath, srcRel, srcOK, srcErr := s.mountRel(src)
	if srcErr != nil {
		if srcOK {
			return srcErr
		}
		return os.Rename(src, dst)
	}
	dstRoot, dstRootPath, dstRel, dstOK, dstErr := s.mountRel(dst)
	if dstErr != nil {
		if srcOK {
			srcRoot.Close()
		}
		return os.Rename(src, dst)
	}
	if !srcOK || !dstOK || srcRootPath != dstRootPath {
		if srcOK {
			srcRoot.Close()
		}
		if dstOK {
			dstRoot.Close()
		}
		return os.Rename(src, dst)
	}
	defer srcRoot.Close()
	if dstOK {
		dstRoot.Close() // 只用于比较根路径；重命名在 srcRoot 上完成
	}
	return srcRoot.Rename(srcRel, dstRel)
}

// verifyAuthorizedParent 在 rename/删除等路径级操作前复核父目录快照。
func (s *Server) verifyAuthorizedParent(r *http.Request, urlPath string) error {
	ap := s.authorizedPathFor(r, urlPath)
	if ap == nil {
		return nil
	}
	if ap.parent == "" || ap.parentInfo == nil {
		return errPathChanged
	}
	if !ap.parentUnchanged() {
		return errPathChanged
	}
	return nil
}
