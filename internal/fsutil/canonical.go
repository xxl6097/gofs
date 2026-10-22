package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// canonicalCasePaths 控制是否按磁盘真实名字规范化 URL 路径。
//
// macOS / Windows 的默认文件系统大小写不敏感，macOS 还会做 Unicode 规范化，
// Windows 还接受 8.3 短名与段尾空格/点。ACL 是字符串最长前缀匹配；如果不把
// 请求路径映射成磁盘上的真实名字，一条更具体的只读规则就可能被大小写变体
// 绕到更宽的读写规则下。Linux 常规文件系统区分大小写，无需这段 IO 开销；
// Linux casefold 目录仍是已知限制。
var canonicalCasePaths = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// CanonicalURLPath 把 urlPath 中**已存在**的路径段替换为磁盘上的真实名字，
// 使路径级 ACL 与文件系统的查找语义一致。
//
// 不跟随软链：软链叶子仍是链接名，目标权限由 ResolveReal/真实路径复核处理。
// 不存在的尾部按原样保留（PUT 新文件时没有可映射的真实名字）。
func (r *Roots) CanonicalURLPath(urlPath string) (string, error) {
	clean := CleanURLPath(urlPath)
	if !canonicalCasePaths || clean == "/" {
		return clean, nil
	}

	trimmed := strings.TrimPrefix(clean, "/")
	first, rest := trimmed, "/"
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		first, rest = trimmed[:i], trimmed[i:]
	}

	r.mu.RLock()
	res := r.mounts[0].Res
	if idx, ok := r.byLabel[first]; ok {
		res = r.mounts[idx].Res
	} else {
		rest = clean
	}
	r.mu.RUnlock()

	if res.SingleFile() != "" {
		// 单文件模式只服务一个 URL："/"。名字不参与路由，不需要映射。
		return clean, nil
	}

	abs, err := res.Resolve(rest)
	if err != nil {
		return "", err
	}
	real, err := canonicalCaseFrom(res.Root(), abs)
	if err != nil {
		return "", err
	}
	return r.Rel(real)
}

// canonicalCaseFrom 从 root 开始逐段查找 abs 在磁盘上的真实名字。
// root 必须是已解析软链的绝对路径；abs 必须在其下。
func canonicalCaseFrom(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return root, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrEscaped, abs)
	}

	cur := root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		actual, exists := actualName(cur, part)
		cur = filepath.Join(cur, actual)
		if !exists {
			// 从这里开始都不存在：原样拼回去。
			for _, tail := range parts[i+1:] {
				if tail != "" && tail != "." {
					cur = filepath.Join(cur, tail)
				}
			}
			break
		}
	}
	return cur, nil
}

// actualName 返回 dir 下与 name 指向同一文件（按文件系统查找语义）的真实条目名。
//
// 先做精确名字快速判断；不在目录项里时，再用 Lstat 解析 name（覆盖大小写、
// Unicode 规范化、Windows 短名/尾随空格点），并用 SameFile 找回真实名字。
func actualName(dir, name string) (string, bool) {
	full := filepath.Join(dir, name)
	if _, err := os.Lstat(full); err != nil {
		return name, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return name, true // 读不了目录：保持原名，后续操作会给出真实错误
	}
	for _, e := range entries {
		if e.Name() == name {
			return name, true
		}
	}
	fi, err := os.Lstat(full)
	if err != nil {
		return name, true
	}
	for _, e := range entries {
		efi, ierr := e.Info()
		if ierr == nil && os.SameFile(efi, fi) {
			return e.Name(), true
		}
	}
	return name, true
}
