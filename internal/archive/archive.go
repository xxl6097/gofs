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
	"runtime"
	"strings"
	"time"

	"github.com/xxl6097/gofs/internal/fsutil"
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
	// AllowSymlink 对应服务的 --allow-symlink：为 true 时允许经由目标
	// 路径上已有的软链写出（保留旧的显式放开语义）。
	AllowSymlink bool
	// Authorize 在**每个将要写出的**目录/文件落盘前调用，rel 是相对
	// Dest 的路径（已用 / 分隔）。返回错误则中止解压。
	//
	// 放在写入点而不是只依赖调用方事前的 List 预检：List 可能因
	// MaxFiles 截断，而填充条目（符号链接、特殊类型、非法名字）在
	// Extract 中会被跳过、不计数。只检查截断后的前缀会让后面的真实
	// 文件完全绕过 ACL。
	Authorize func(rel string) error
	// OpenWrite 可选：替换默认的 os.OpenFile，供调用方用 os.Root 或
	// 其它安全打开方式落盘。target 是绝对路径，flags/perm 与 os.OpenFile
	// 相同。为 nil 时退回 os.OpenFile。
	OpenWrite func(target string, flags int, perm fs.FileMode) (*os.File, error)
	// MkdirAll 可选：替换默认的 os.MkdirAll，供调用方用 os.Root 创建目录。
	MkdirAll func(target string, perm fs.FileMode) error
}

// mkdirAll 在有安全实现时使用它，否则退回 os.MkdirAll。
func mkdirAll(opts Options, target string, perm fs.FileMode) error {
	if opts.MkdirAll != nil {
		return opts.MkdirAll(target, perm)
	}
	return os.MkdirAll(target, perm)
}

// authorizeWrite 在落盘前执行调用方提供的逐条目授权。
func authorizeWrite(opts Options, target string) error {
	if opts.Authorize == nil {
		return nil
	}
	rel, err := filepath.Rel(opts.Dest, target)
	if err != nil {
		return fmt.Errorf("条目路径非法: %w", err)
	}
	if rel == "." {
		return nil
	}
	return opts.Authorize(filepath.ToSlash(rel))
}

// authorizeParents 在创建目录之前，逐级校验目标路径上的每一级父目录。
//
// 只授权叶子文件是不够的：`/docs:r,/docs/sub/leaf.txt:rw` 的账号
// 可以写 leaf，但不应因此获得在 /docs 下创建目录的权力。MkdirAll 会
// 顺带创建缺失的祖先目录，所以必须在调用它之前逐级过一遍 Authorize。
func authorizeParents(opts Options, target string) error {
	if opts.Authorize == nil {
		return nil
	}
	rel, err := filepath.Rel(opts.Dest, filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("条目路径非法: %w", err)
	}
	if rel == "." || rel == "" {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("条目路径试图逃逸出目标目录")
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 1; i <= len(parts); i++ {
		if err := opts.Authorize(strings.Join(parts[:i], "/")); err != nil {
			return err
		}
	}
	return nil
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

// ErrEntryTooLarge 表示单个条目的解压后体积超过预览上限。
var ErrEntryTooLarge = errors.New("压缩包条目过大")

// ErrScanTooLarge 表示为了列出条目或查找某个条目而需要扫描的解压数据
// 超过预算。
//
// tar 是流式的：枚举/定位条目必须从头顺序读，`Next()` 会跳过（也就是
// 解压）前面条目的全部内容。只限制「写出的字节数」挡不住一个高压缩比
// 的 tar.gz —— 它可以在不写任何文件的情况下吃掉大量 CPU/IO。
var ErrScanTooLarge = errors.New("压缩包扫描量超过上限")

// Limits 限制「列表 / 单条目预览」的规模。
//
// 这些操作与 Extract 共用同一份配置，但语义不同：列表只是给界面看，
// 不需要为了展示几十万个条目把内存吃光；预览一个条目也不该允许
// 一个几十 GB 的稀疏文件把临时目录写满。
type Limits struct {
	MaxFiles      int
	MaxTotalBytes int64
	MaxEntryBytes int64
	// MaxScanBytes 是「为了列表/定位条目而允许解压扫描」的字节上限。
	//
	// 只对 tar 这类流式格式有意义：列出一个条目要先把前面所有内容读完。
	// 它通常应小于 MaxTotalBytes —— 列表/预览不需要把整份 10 GiB 的归档
	// 解压一遍。为 0 时回落到 MaxTotalBytes。
	MaxScanBytes int64
}

// LimitSetter 是可选能力：支持限额的 Reader 实现它。
type LimitSetter interface {
	SetLimits(Limits)
}

// Open 打开一个压缩包并返回对应的 Reader。
func Open(filename string) (Reader, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	return OpenFile(f, filename)
}

// OpenFile 与 Open 相同，但使用调用方已经打开（并已完成安全校验）的文件。
//
// 所有权转移给 Reader：成功时由 Reader.Close 关闭，失败时由本函数关闭。
func OpenFile(f *os.File, filename string) (Reader, error) {
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		f.Close()
		return nil, err
	}
	head = head[:n]
	// 后续 reader 会自行 Seek(0) 重新扫描；这里只读文件头做格式嗅探。
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}

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
		if unsafeWindowsSegment(seg) {
			return "", fmt.Errorf("条目 %q 含有 Windows 不安全的路径段 %q", name, seg)
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

// unsafeWindowsSegment 识别 Win32 会“改写语义”的路径段。
//
// Windows 会去掉段尾的空格/点（`".. "` 等价于 `".."`），并把 `:` 解释为
// NTFS 备用数据流；这些字符在 Linux/macOS 上合法，词法检查也看不出来，
// 但落盘时由 Win32 解析，可能越出目标目录或写到只读 ACL 覆盖不到的 ADS。
func unsafeWindowsSegment(seg string) bool {
	if runtime.GOOS != "windows" || seg == "" || seg == "." {
		return false
	}
	if strings.ContainsRune(seg, ':') {
		return true
	}
	trimmed := strings.TrimRight(seg, " .")
	if trimmed == "" || trimmed != seg {
		return true
	}
	if trimmed == ".." {
		return true
	}
	base := strings.ToUpper(strings.SplitN(trimmed, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 {
		switch base[:3] {
		case "COM", "LPT":
			if base[3] >= '1' && base[3] <= '9' {
				return true
			}
		}
	}
	return false
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
	// limited 与 max 分开，是因为「预算刚好用尽」与「没有上限」都表现为 max == 0：
	// 只看 max > 0 的话，前一个条目恰好把总预算写满时，下一个条目会变成**无上限**，
	// 整份内容先落到磁盘上，等写完才被 checkWritten 发现超限再删掉 ——
	// 那正是解压炸弹要的效果（磁盘被瞬时写满），而这个上限的存在意义就是提前拦住。
	limited bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.limited && lw.written+int64(len(p)) > lw.max {
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
	// 包内条目名本身是干净的（secureJoin 做过词法检查），但目标路径上可能
	// 有一条**本来就存在**的软链指向根外 —— MkdirAll/OpenFile 会跟过去，
	// 于是解压把文件写到服务根之外。这条检查与 HTTP 层用的是同一个实现
	//（fsutil.EnsureWithin），只堵一边等于没堵。
	if !opts.AllowSymlink {
		if err := fsutil.EnsureWithin(opts.Dest, target); err != nil {
			return 0, err
		}
	}
	if !opts.Overwrite {
		if _, err := os.Stat(target); err == nil {
			return 0, fs.ErrExist
		}
	}
	if err := authorizeParents(opts, target); err != nil {
		return 0, err
	}
	if dir := filepath.Dir(target); dir != "" {
		if err := mkdirAll(opts, dir, 0o755); err != nil {
			return 0, err
		}
	}
	// 恒用 0644，忽略包内权限位。
	var f *os.File
	var err error
	if opts.OpenWrite != nil {
		f, err = opts.OpenWrite(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	} else {
		f, err = os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	}
	if err != nil {
		return 0, err
	}

	lw := &limitedWriter{w: f}
	if limit.maxTotal > 0 {
		// 剩余预算可能是 0（上一个条目刚好写满），那时应当**立刻**拒绝，
		// 而不是当成「无上限」—— 见 limitedWriter.limited 的说明。
		lw.max = limit.maxTotal - limit.total
		lw.limited = true
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
