// Package archive 提供压缩包的格式嗅探、内容列举与安全解压能力。
//
// 支持的格式：
//
//	.zip      通过 archive/zip 读取
//	.tar      通过 archive/tar 读取
//	.tar.gz   先 gunzip 再按 tar 读取
//	.tgz      同 .tar.gz
//	.gz       单文件 gzip，解压后得到去掉 .gz 后缀的原文件
//
// 安全约束（在线解压是不可信输入，必须防守）：
//  1. Zip Slip —— 任何条目路径只允许落在目标目录之内，拒绝绝对路径与 ..
//  2. 软链接逃逸 —— tar 中的 symlink/hardlink 默认跳过，不落盘
//  3. 解压炸弹 —— 限制条目数、写出总字节数、单文件压缩比
//  4. 权限位 —— 文件恒为 0644、目录恒为 0755，忽略包内 mode，防止写 setuid
package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Format 表示压缩包格式。
type Format int

const (
	// FormatUnknown 无法识别的格式。
	FormatUnknown Format = iota
	// FormatZip zip 归档。
	FormatZip
	// FormatTar 未压缩的 tar 归档。
	FormatTar
	// FormatTarGz tar + gzip。
	FormatTarGz
	// FormatGz 单个 gzip 压缩文件。
	FormatGz
)

// String 返回格式的可读名称。
func (f Format) String() string {
	switch f {
	case FormatZip:
		return "zip"
	case FormatTar:
		return "tar"
	case FormatTarGz:
		return "tar.gz"
	case FormatGz:
		return "gz"
	}
	return "unknown"
}

// Entry 描述压缩包内的一个条目。
type Entry struct {
	Name           string `json:"name"`            // 包内路径，统一使用 /
	Size           int64  `json:"size"`            // 解压后大小，未知为 -1
	CompressedSize int64  `json:"compressed_size"` // 压缩后大小，未知为 -1
	IsDir          bool   `json:"is_dir"`
	ModTime        string `json:"modtime"`
	Mode           string `json:"mode"`
	LinkTarget     string `json:"link_target"` // 非空表示符号链接
	Encrypted      bool   `json:"encrypted"`   // zip 加密标记
}

// Options 控制一次解压行为。
type Options struct {
	// Dest 为目标目录的绝对路径。
	Dest string
	// Overwrite 为 true 时覆盖已存在的文件，否则跳过。
	Overwrite bool
	// MaxTotalBytes 写出字节总量上限。
	MaxTotalBytes int64
	// MaxFiles 条目数上限。
	MaxFiles int
	// MaxRatio 单文件压缩比上限。
	MaxRatio int
}

// Progress 描述解压进度。
type Progress struct {
	Current string `json:"current"` // 当前处理的条目
	Files   int    `json:"files"`   // 已写出文件数
	Dirs    int    `json:"dirs"`    // 已创建目录数
	Skipped int    `json:"skipped"` // 跳过条目数
	Bytes   int64  `json:"bytes"`   // 已写出字节数
	Total   int    `json:"total"`   // 已知条目总数，-1 表示未知
}

// Report 为解压结束后的汇总。
type Report struct {
	Format    string   `json:"format"`
	Dest      string   `json:"dest"`
	Files     int      `json:"files"`
	Dirs      int      `json:"dirs"`
	Bytes     int64    `json:"bytes"`
	Skipped   int      `json:"skipped"`
	Warnings  []string `json:"warnings"`
	ElapsedMs int64    `json:"elapsed_ms"`
}

// Reader 为各类压缩包的统一抽象。
type Reader interface {
	// Format 返回格式。
	Format() Format
	// List 遍历全部条目元数据。
	List(ctx context.Context) ([]Entry, error)
	// OpenEntry 打开包内某个文件用于读取（不解压落盘）。
	OpenEntry(ctx context.Context, name string) (io.ReadCloser, Entry, error)
	// Extract 解压到 opts.Dest。
	Extract(ctx context.Context, opts Options, onProgress func(Progress)) (*Report, error)
	// Close 释放资源。
	Close() error
}

// ErrEncrypted 表示遇到了暂不支持的加密压缩包。
var ErrEncrypted = errors.New("该压缩包已加密，暂不支持在线解压")

// ErrUnsupported 表示格式不受支持。
var ErrUnsupported = errors.New("不支持的压缩包格式")

// Open 打开一个压缩包并返回对应的 Reader。
func Open(filename string) (Reader, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		f.Close()
		return nil, err
	}
	head = head[:n]

	format := Detect(filename, head)
	switch format {
	case FormatZip:
		return newZipReader(f)
	case FormatTarGz:
		return newTarGzReader(f)
	case FormatTar:
		return newTarReader(f)
	case FormatGz:
		return newGzReader(f)
	}
	f.Close()
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, filename)
}

// Detect 依据文件名与文件头判断格式。
func Detect(filename string, head []byte) Format {
	// 优先用文件头判断，避免扩展名造假。
	switch {
	case len(head) >= 4 && head[0] == 'P' && head[1] == 'K' &&
		(head[2] == 0x03 || head[2] == 0x05 || head[2] == 0x07):
		return FormatZip
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		// gzip：需进一步区分「tar.gz」与「单个 .gz 文件」。
		if looksLikeTarGz(filename, head) {
			return FormatTarGz
		}
		return FormatGz
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return FormatTar
	}

	// 退回到扩展名。
	l := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(l, ".zip"):
		return FormatZip
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"):
		return FormatTarGz
	case strings.HasSuffix(l, ".tar"):
		return FormatTar
	case strings.HasSuffix(l, ".gz"):
		return FormatGz
	}
	return FormatUnknown
}

// looksLikeTarGz 通过「文件名后缀」与「gzip 头里的原始文件名」双重线索判断是否为 tar.gz。
func looksLikeTarGz(filename string, head []byte) bool {
	l := strings.ToLower(filename)
	if strings.HasSuffix(l, ".tar.gz") || strings.HasSuffix(l, ".tgz") {
		return true
	}
	// gzip 头部可能带有 FNAME 字段（FLG bit3），其中常写明原始文件名。
	if len(head) > 10 && head[3]&0x08 != 0 {
		name := readCString(head[10:])
		if strings.HasSuffix(strings.ToLower(name), ".tar") {
			return true
		}
		if name != "" {
			// 头部明确写了非 tar 的文件名，认为是单文件 gzip。
			return false
		}
	}
	// 无更多线索时，按「扩展名是 .gz 且不是 .tar.gz」处理为单文件 gzip。
	return false
}

// readCString 读取以 NUL 结尾的字符串。
func readCString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 公共辅助
// ---------------------------------------------------------------------------

// secureJoin 把压缩包内的路径安全地拼接到 dest 之下。
// 任何试图逃逸出 dest 的路径都会返回错误。
//
// 注意：这里刻意不依赖 path.Clean 来消除 ..，因为 Clean 会把
// "../../x" 静默改写成 "x" 并当作合法路径落盘——结果虽然没逃逸，
// 却掩盖了压缩包的逃逸意图，也让使用者在目录里看到一个来历不明的文件。
// 因此含 .. 的条目一律显式拒绝。
func secureJoin(dest, name string) (string, error) {
	if name == "" {
		return "", errors.New("条目路径为空")
	}
	// 统一分隔符，兼容 Windows 打包的包。
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("条目 %q 使用了绝对路径", name)
	}
	if v := filepath.VolumeName(name); v != "" {
		return "", fmt.Errorf("条目 %q 使用了盘符路径", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", fmt.Errorf("条目 %q 使用了 .. 路径段，已拒绝", name)
		}
	}
	cleaned := strings.TrimPrefix(path.Clean("/"+name), "/")
	if cleaned == "" || cleaned == "." {
		return dest, nil
	}
	target := filepath.Join(dest, filepath.FromSlash(cleaned))
	rel, err := filepath.Rel(dest, target)
	if err != nil {
		return "", fmt.Errorf("条目 %q 路径非法: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("条目 %q 试图逃逸出目标目录", name)
	}
	return target, nil
}

// limiter 累计并校验解压规模，防止解压炸弹。
type limiter struct {
	maxTotal int64
	maxFiles int
	maxRatio int

	total int64
	files int
}

// newLimiter 依据配置构造限制器。
func newLimiter(opts Options) *limiter {
	return &limiter{
		maxTotal: opts.MaxTotalBytes,
		maxFiles: opts.MaxFiles,
		maxRatio: opts.MaxRatio,
	}
}

// checkNext 在写出前调用，校验条目数与总字节数。
func (l *limiter) checkNext(name string, size int64) error {
	l.files++
	if l.maxFiles > 0 && l.files > l.maxFiles {
		return fmt.Errorf("条目数超过上限 %d，已中止解压", l.maxFiles)
	}
	if l.maxTotal > 0 && l.total+size > l.maxTotal {
		return fmt.Errorf("解压后总大小将超过上限 %s，已中止解压", humanBytes(l.maxTotal))
	}
	return nil
}

// checkWritten 在写出后累计实际字节，并校验压缩比。
func (l *limiter) checkWritten(name string, written, compressed int64) error {
	l.total += written
	if l.maxTotal > 0 && l.total > l.maxTotal {
		return fmt.Errorf("解压后总大小超过上限 %s，已中止解压", humanBytes(l.maxTotal))
	}
	if l.maxRatio > 0 && compressed > 0 {
		if written/compressed > int64(l.maxRatio) && written > 1<<20 {
			return fmt.Errorf("条目 %q 压缩比过高（%.1fx），疑似解压炸弹，已中止", name, float64(written)/float64(compressed))
		}
	}
	return nil
}

// limitedWriter 统计写入量并实时拦截超限。
type limitedWriter struct {
	w       io.Writer
	written int64
	max     int64
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.max > 0 && lw.written+int64(len(p)) > lw.max {
		return 0, fmt.Errorf("解压后总大小超过上限 %s，已中止解压", humanBytes(lw.max))
	}
	n, err := lw.w.Write(p)
	lw.written += int64(n)
	return n, err
}

// humanBytes 把字节数格式化为可读字符串。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// normalizeEntryName 归一化包内条目名称。
func normalizeEntryName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	return name
}

// writeFile 安全地把 src 写入 target，并返回写入字节数。
// 一旦中途失败（超出配额、磁盘错误等），已写入的残缺文件会被删除，
// 避免在目标目录里留下半截数据。
func writeFile(target string, src io.Reader, opts Options, limit *limiter, compressed int64, name string, modTime time.Time) (int64, error) {
	if !opts.Overwrite {
		if _, err := os.Stat(target); err == nil {
			return 0, fs.ErrExist
		}
	}
	if dir := filepath.Dir(target); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
	}
	// 恒用 0644，忽略包内权限位。
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}

	lw := &limitedWriter{w: f, max: 0}
	if limit.maxTotal > 0 {
		lw.max = limit.maxTotal - limit.total
	}

	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(target)
	}

	n, err := io.Copy(lw, src)
	if err != nil {
		cleanup()
		return n, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(target)
		return n, err
	}
	if err := limit.checkWritten(name, n, compressed); err != nil {
		_ = os.Remove(target)
		return n, err
	}
	if !modTime.IsZero() {
		_ = os.Chtimes(target, modTime, modTime)
	}
	return n, nil
}
