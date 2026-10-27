package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xxl6097/gofs/internal/archive"
	"github.com/xxl6097/gofs/internal/auth"
	"github.com/xxl6097/gofs/internal/fsutil"
)

// conflictMode 描述上传/解压遇到同名条目时的处理方式。
//
// 默认（conflictNone）保持各接口原有的语义：PUT 覆盖写、表单上传覆盖写、
// 解压跳过已存在文件。显式带上 conflict=replace|backup 才会启用新行为。
type conflictMode int

const (
	conflictNone conflictMode = iota
	// conflictReplace：先删除原条目，再写入新内容。
	conflictReplace
	// conflictBackup：先把原条目重命名为「名称-时间戳.扩展名」，再写入新内容。
	conflictBackup
)

// parseConflictMode 解析 conflict 参数。
//
// overwrite 作为 replace 的别名，方便老的调用方渐进迁移。
func parseConflictMode(v string) conflictMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "replace", "overwrite", "truncate":
		return conflictReplace
	case "backup", "rename":
		return conflictBackup
	}
	return conflictNone
}

// conflictModeOf 从查询参数读取冲突处理方式（PUT 使用）。
//
// 不要在这里调 FormValue：PUT 的请求体就是文件内容，一旦被当作表单解析，
// 文件内容会被消费掉。
func conflictModeOf(r *http.Request) conflictMode {
	return parseConflictMode(r.URL.Query().Get("conflict"))
}

// backupStamp 是备份文件名里使用的时间戳格式。
const backupStamp = "20060102-150405"

// backupFileName 生成带时间戳后缀的备份名。
//
// 文件的扩展名保留在末尾：report.pdf -> report-20261027-153045.pdf。
// 点开头的隐藏文件（.gitignore）没有可保留的扩展名，直接追加后缀。
// n > 1 时再追加序号，避免同一秒内多次备份互相覆盖。
// max > 0 时按字节截断主名，保证结果不超过服务端的文件名长度上限。
func backupFileName(name string, isDir bool, ts string, n int, max int) string {
	suffix := "-" + ts
	if n > 1 {
		suffix += fmt.Sprintf("-%d", n)
	}
	stem, ext := name, ""
	if !isDir {
		if e := filepath.Ext(name); e != "" && e != name {
			stem, ext = strings.TrimSuffix(name, e), e
		}
	}
	if max <= 0 || len(stem)+len(suffix)+len(ext) <= max {
		return stem + suffix + ext
	}
	keep := max - len(suffix) - len(ext)
	if keep < 1 {
		return suffix + ext
	}
	// 按字节截断时不能把一个 UTF-8 字符切成两半。
	for keep > 0 && !utf8.RuneStart(stem[keep]) {
		keep--
	}
	return stem[:keep] + suffix + ext
}

// backupPath 为 abs 选择一个新的备份路径（同目录、带时间戳、绝不覆盖已有条目）。
func (s *Server) backupPath(abs string, info os.FileInfo) (string, error) {
	dir, name := filepath.Split(abs)
	ts := time.Now().Format(backupStamp)
	for i := 1; i <= 100; i++ {
		cand := filepath.Join(dir, backupFileName(name, info.IsDir(), ts, i, s.cfg.MaxNameBytes))
		if _, err := os.Lstat(cand); errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("无法为 %s 生成可用的备份文件名", name)
}

// refreshAuthorizedPath 在删除/重命名目标之后刷新请求内的 inode 快照。
//
// 上传链路在授权时记录的是“旧文件”的快照；如果随后替换/备份把它移走，
// secureOpenWrite 仍会拿旧 inode 复核并报 errPathChanged。重新取一次快照后，
// 目标对后续写入而言就是一个干净的新文件。
func (s *Server) refreshAuthorizedPath(r *http.Request, urlPath, abs string) {
	real, err := fsutil.ResolveReal(abs)
	if err != nil {
		return
	}
	s.noteAuthorizedPath(r, urlPath, real)
	if realURL, _, ok := s.realPathOf(abs); ok && realURL != urlPath {
		s.noteAuthorizedPath(r, realURL, real)
	}
}

// uploadBackupAllowed 复核备份目标路径的写权限。
//
// 备份名是从原文件名派生的新条目，仍然要按 ACL / 上传密钥范围检查，
// 不能因为“只是重命名”就绕过权限模型。
func (s *Server) uploadBackupAllowed(w http.ResponseWriter, r *http.Request, p permResult, backupURL, backupAbs string) bool {
	if p.UploadKey != nil {
		if !s.uploadKeyAllowed(r, p.UploadKey, backupURL) {
			http.Error(w, "403 Forbidden: 备份路径超出该密钥允许的范围，或创建者已无对应权限",
				http.StatusForbidden)
			return false
		}
	} else if s.checkPerm(r, backupURL) != auth.PermReadWrite {
		http.Error(w, "403 Forbidden: 对备份路径没有写权限", http.StatusForbidden)
		return false
	}
	if realURL, realAbs, perm, within := s.realPermOf(r, backupAbs); within {
		s.noteAuthorizedPath(r, backupURL, realAbs)
		if realURL != backupURL && perm < auth.PermReadWrite {
			http.Error(w, "403 Forbidden: 对备份路径的真实路径没有写权限", http.StatusForbidden)
			return false
		}
	} else if p.UploadKey != nil || s.shareTokenOf(r) != "" || !s.cfg.AllowSymlink {
		http.Error(w, "403 Forbidden: 备份路径超出服务范围", http.StatusForbidden)
		return false
	}
	return true
}

// resolveUploadConflict 在上传写入之前处理同名目标。
//
// 只对非追加模式调用：续传（append）必须保留磁盘上的半截文件，不能因为
// 带了 conflict 参数就把续传点删掉。
func (s *Server) resolveUploadConflict(w http.ResponseWriter, r *http.Request, p permResult, urlPath, abs string, mode conflictMode) bool {
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		s.writeErr(w, err)
		return false
	}
	if s.res.IsRoot(abs) {
		http.Error(w, "403 Forbidden: 不允许替换或备份服务根目录", http.StatusForbidden)
		return false
	}

	switch mode {
	case conflictReplace:
		// 目录要递归删除，必须逐项复核权限（目录可写、子项只读的 ACL
		// 不能被一次替换绕过）。
		if info.IsDir() {
			if bad, ok := s.requireDeleteTree(r, abs); !ok {
				http.Error(w, "403 Forbidden: 目录内的 "+bad+" 没有写权限", http.StatusForbidden)
				return false
			}
		}
		if err := s.secureRemoveAll(abs); err != nil {
			s.writeErr(w, err)
			return false
		}
	case conflictBackup:
		backupAbs, err := s.backupPath(abs, info)
		if err != nil {
			s.writeErr(w, err)
			return false
		}
		backupURL := path.Join(path.Dir(urlPath), filepath.Base(backupAbs))
		if !s.uploadBackupAllowed(w, r, p, backupURL, backupAbs) {
			return false
		}
		if info.IsDir() {
			if bad, ok := s.requireMoveTree(r, abs, urlPath, backupURL); !ok {
				http.Error(w, "403 Forbidden: "+bad, http.StatusForbidden)
				return false
			}
		}
		if err := s.secureRename(abs, backupAbs); err != nil {
			s.writeErr(w, err)
			return false
		}
	default:
		return true
	}

	s.refreshAuthorizedPath(r, urlPath, abs)
	return true
}

// extractTargetPath 把包内条目名安全地映射为 destAbs 下的绝对路径。
func extractTargetPath(destAbs, name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" {
		return "", errors.New("条目路径为空")
	}
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	if clean == "" || clean == "." {
		return destAbs, nil
	}
	target := filepath.Join(destAbs, filepath.FromSlash(clean))
	if err := fsutil.EnsureWithin(destAbs, target); err != nil {
		return "", err
	}
	return target, nil
}

// existingConflict 返回会阻碍 target 落盘的现有条目。
//
// 返回值可能是 target 自身，也可能是它上面某一级“本应是目录、实际是文件”
// 的祖先。wantDir 为 true 时，已存在的同名**目录**不算冲突（解压目录条目
// 本来就应该与已有目录合并）。
//
// 找不到冲突返回 ("", nil)。向上查找是因为包里可能是 a/b.txt，而目标目录里
// 已有一个名为 a 的普通文件 —— 此时 Lstat(a/b.txt) 会直接报 ENOTDIR。
func existingConflict(destAbs, target string, wantDir bool) (string, os.FileInfo) {
	for p := target; ; {
		if p == destAbs || !fsutil.IsWithin(destAbs, p) {
			return "", nil
		}
		fi, err := os.Lstat(p)
		if err == nil {
			if p == target {
				if wantDir && fi.IsDir() {
					return "", nil
				}
				return p, fi
			}
			if fi.IsDir() {
				// 目标本身不存在，最近存在的祖先是目录：没有冲突。
				return "", nil
			}
			return p, fi
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", nil
		}
		p = parent
	}
}

// extractConflictList 列出压缩包条目会碰到的同名目标，供界面弹窗询问。
func (s *Server) extractConflictList(destAbs string, entries []archive.Entry) []extractConflict {
	out := make([]extractConflict, 0)
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		// 软链/硬链条目在 Extract 中一律跳过，不会写出，自然也不会覆盖
		// 同名文件；把它们排除掉可以少一次无意义的弹窗。
		if e.Name == "" || e.LinkTarget != "" || seen[e.Name] {
			continue
		}
		target, err := extractTargetPath(destAbs, e.Name)
		if err != nil || target == destAbs {
			continue
		}
		conflictPath, info := existingConflict(destAbs, target, e.IsDir)
		if conflictPath == "" {
			continue
		}
		seen[e.Name] = true
		out = append(out, extractConflict{
			Name:          e.Name,
			IsDir:         e.IsDir,
			ExistingIsDir: info != nil && info.IsDir(),
		})
	}
	return out
}

// extractDestState 返回目标目录是否存在、是否非空、是否其实是一个文件。
func extractDestState(destAbs string) (exists, nonEmpty, isFile bool) {
	fi, err := os.Stat(destAbs)
	if err != nil {
		return false, false, false
	}
	if !fi.IsDir() {
		return true, false, true
	}
	f, err := os.Open(destAbs)
	if err != nil {
		return true, false, false
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return true, err == nil && len(names) > 0, false
}

// resolveExtractConflict 是解压写入点的同名处理回调。
//
// target 是本次要写出的路径，conflict 是挡路的现有条目（通常与 target
// 相同，也可能是它的祖先）。archive.Extract 只在**确实要写出**某个条目、
// 且目标已存在时调用它；包里被跳过的软链/特殊类型/非法路径不会触发，
// 因此不会出现「包里的条目最终没解压出来，但用户的同名文件已经被删掉」。
func (s *Server) resolveExtractConflict(r *http.Request, destClean, destAbs string, mode conflictMode, target, conflict string, info fs.FileInfo) error {
	// 与 archive.writeFile 保持同一口径：只有显式 --allow-symlink
	// 才允许经由目标目录里的软链写出去。
	if !s.cfg.AllowSymlink {
		if err := fsutil.EnsureWithin(destAbs, conflict); err != nil {
			return err
		}
	}
	rel, err := filepath.Rel(destAbs, conflict)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("条目路径非法: %w", fs.ErrPermission)
	}
	urlPath := path.Join(destClean, filepath.ToSlash(rel))
	if s.checkPerm(r, urlPath) != auth.PermReadWrite ||
		!s.realPermAllowed(r, conflict, auth.PermReadWrite) {
		return fmt.Errorf("条目 %s 没有写权限: %w", urlPath, fs.ErrPermission)
	}

	switch mode {
	case conflictReplace:
		if info.IsDir() {
			if bad, ok := s.requireDeleteTree(r, conflict); !ok {
				return fmt.Errorf("目录内的 %s 没有写权限: %w", bad, fs.ErrPermission)
			}
		}
		if err := s.secureRemoveAll(conflict); err != nil {
			return err
		}
	case conflictBackup:
		backupAbs, err := s.backupPath(conflict, info)
		if err != nil {
			return err
		}
		backupRel, err := filepath.Rel(destAbs, backupAbs)
		if err != nil {
			return err
		}
		backupURL := path.Join(destClean, filepath.ToSlash(backupRel))
		if s.checkPerm(r, backupURL) != auth.PermReadWrite ||
			!s.realPermAllowed(r, backupAbs, auth.PermReadWrite) {
			return fmt.Errorf("备份路径 %s 没有写权限: %w", backupURL, fs.ErrPermission)
		}
		if info.IsDir() {
			if bad, ok := s.requireMoveTree(r, conflict, urlPath, backupURL); !ok {
				return fmt.Errorf("目录内的 %s 移动到备份路径会提升权限: %w", bad, fs.ErrPermission)
			}
		}
		if err := s.secureRename(conflict, backupAbs); err != nil {
			return err
		}
	default:
		return nil
	}

	// 授权快照记录的是原目标（可能已被删除/改名）。conflict 和 target 都要
	// 重新取一次：祖先被改名时，target 的父目录 inode 也变了，不刷新会让
	// secureOpenWrite 在 --allow-symlink 等回退路径上误判成路径被替换。
	for _, p := range []string{conflict, target} {
		rel, err := filepath.Rel(destAbs, p)
		if err != nil || rel == "." || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if real, err := fsutil.ResolveReal(p); err == nil {
			s.noteAuthorizedPath(r, "extract:"+filepath.ToSlash(rel), real)
		}
	}
	return nil
}
