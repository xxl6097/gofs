package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/xxl6097/gofs/internal/fsutil"
)

// zip 中央目录预检的硬上限。
//
// zip.NewReader 会先读出并解析整个中央目录，条目数与目录体积都在压缩包
// 里声明、完全由攻击者控制。一个几十 MB 的文件可以声明上百万条目，
// 在服务器拿到 ExtractMaxFiles 之前就把内存吃光。打开前先看 EOCD，
// 超过上限直接拒绝。
const (
	hardMaxZipEntries  = 200_000
	hardMaxZipDirBytes = 64 << 20
)

// CheckZipDirectory 在 zip.NewReader 之前做一个有界的 EOCD 预检。
//
// 不是完整的 zip 校验，只回答「中央目录的声明值是否足以造成资源问题」。
// Zip64（0xFFFF / 0xFFFFFFFF）一律拒绝：本服务的解压上限远低于该量级，
// 没必要为了支持它承担额外解析风险。
func CheckZipDirectory(f *os.File, size int64, maxEntries int, maxDirBytes int64) error {
	if size < 22 {
		return nil
	}
	tail := int64(1<<16 + 22)
	if tail > size {
		tail = size
	}
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, size-tail); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	idx := bytes.LastIndex(buf, []byte{'P', 'K', 5, 6})
	if idx < 0 || len(buf)-idx < 22 {
		return nil // 不是 zip（或 EOCD 损坏）：交给 zip.NewReader 报错
	}
	e := buf[idx:]
	entries := binary.LittleEndian.Uint16(e[10:12])
	dirSize := binary.LittleEndian.Uint32(e[12:16])
	dirOffset := binary.LittleEndian.Uint32(e[16:20])
	if entries == 0xFFFF || (maxEntries > 0 && int(entries) > maxEntries) {
		return fmt.Errorf("zip 条目数 %d 超过上限 %d", entries, maxEntries)
	}
	if dirSize == 0xFFFFFFFF || dirOffset == 0xFFFFFFFF ||
		(maxDirBytes > 0 && int64(dirSize) > maxDirBytes) {
		return fmt.Errorf("zip 中央目录 %d 字节，超过上限 %d", dirSize, maxDirBytes)
	}
	// EOCD 必须紧跟在中央目录之后。Go 的 archive/zip 在 baseOffset > 0
	// 且 directoryOffset 处能读到合法头时，会**忽略 EOCD 声明的 dirSize**、
	// 一路读到坏记录为止；条数又只比较 uint16，于是“声明 1 条、实际
	// 65537 条”的包能通过上面的计数检查并在 zip.NewReader 里分配巨量
	// *zip.File。这里直接拒绝需要重定位基址的包（带前缀的自解压 zip）。
	eocdOff := int64(size-tail) + int64(idx)
	if int64(dirOffset)+int64(dirSize) > eocdOff {
		return errors.New("zip 中央目录位置超出文件范围")
	}
	if base := eocdOff - int64(dirSize) - int64(dirOffset); base != 0 {
		return fmt.Errorf("zip 存在 %d 字节前置数据，目录偏移异常", base)
	}
	if entries > 0 && int64(entries)*46 > int64(dirSize) {
		return fmt.Errorf("zip 条目数 %d 与目录大小 %d 不匹配", entries, dirSize)
	}
	return nil
}

// zipReader 实现 zip 归档的读取。
type zipReader struct {
	f         *os.File
	zr        *zip.Reader
	limits    Limits
	truncated bool
}

// SetLimits 设置列表与单条目预览上限。
func (z *zipReader) SetLimits(l Limits) { z.limits = l }

// newZipReader 构造 zip 读取器。
func newZipReader(f *os.File) (*zipReader, error) {
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := CheckZipDirectory(f, info.Size(), hardMaxZipEntries, hardMaxZipDirBytes); err != nil {
		f.Close()
		return nil, err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("解析 zip 失败: %w", err)
	}
	// 预检被绕过时的兜底：zip.NewReader 已经分配完，但至少不要让后续
	// List/Extract 继续处理一个超大条目表。
	if len(zr.File) > hardMaxZipEntries {
		f.Close()
		return nil, fmt.Errorf("zip 条目数 %d 超过上限 %d", len(zr.File), hardMaxZipEntries)
	}
	return &zipReader{f: f, zr: zr}, nil
}

// Format 返回格式。
func (z *zipReader) Format() Format { return FormatZip }

// Close 关闭文件。
func (z *zipReader) Close() error { return z.f.Close() }

// isEncrypted 判断 zip 条目是否加密（general purpose bit 0）。
func isEncrypted(f *zip.File) bool { return f.Flags&0x1 != 0 }

// List 遍历 zip 内所有条目。
func (z *zipReader) List(ctx context.Context) ([]Entry, error) {
	capHint := len(z.zr.File)
	if z.limits.MaxFiles > 0 && capHint > z.limits.MaxFiles {
		capHint = z.limits.MaxFiles
	}
	out := make([]Entry, 0, capHint)
	z.truncated = false
	for _, f := range z.zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if z.limits.MaxFiles > 0 && len(out) >= z.limits.MaxFiles {
			// 还有条目没返回：调用方不能把这份列表当成全集做授权。
			z.truncated = true
			break
		}
		out = append(out, zipEntry(f))
	}
	return out, nil
}

// Truncated 报告上一次 List 是否因为 MaxFiles 提前结束。
func (z *zipReader) Truncated() bool { return z.truncated }

// zipDataSpans 用中央目录里的真实数据偏移计算每个条目的物理跨度。
//
// 压缩比防护不能相信条目里声明的 CompressedSize64（攻击者可以填大来稀释
// 比例）。数据偏移来自中央目录，跨度到下一个条目的起点（最后一个到文件尾）
// 是一个**已知、无法通过声明值伪造**的上界；中间若插入填充数据，攻击者
// 也必须真的上传这些字节。
func zipDataSpans(zr *zip.Reader, archiveSize int64) map[*zip.File]int64 {
	type item struct {
		f   *zip.File
		off int64
	}
	items := make([]item, 0, len(zr.File))
	for _, f := range zr.File {
		off, err := f.DataOffset()
		if err != nil || off < 0 || off > archiveSize {
			continue
		}
		items = append(items, item{f: f, off: off})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].off < items[j].off })
	out := make(map[*zip.File]int64, len(items))
	for i, it := range items {
		end := archiveSize
		if i+1 < len(items) && items[i+1].off > it.off {
			end = items[i+1].off
		}
		span := end - it.off
		if span < 0 {
			span = 0
		}
		out[it.f] = span
	}
	return out
}

// zipSize 把 zip 的 uint64 声明值安全转成 int64。
// 超过 MaxInt64 时返回 -1（未知），调用方不应据此设置 Content-Length。
func zipSize(v uint64) int64 {
	if v > math.MaxInt64 {
		return -1
	}
	return int64(v)
}

// zipEntry 把 zip.File 转换为统一的 Entry。
func zipEntry(f *zip.File) Entry {
	e := Entry{
		Name:           normalizeEntryName(f.Name),
		Size:           zipSize(f.UncompressedSize64),
		CompressedSize: zipSize(f.CompressedSize64),
		IsDir:          f.FileInfo().IsDir(),
		ModTime:        f.Modified.Format(time.RFC3339),
		Mode:           f.Mode().String(),
		Encrypted:      isEncrypted(f),
	}
	return e
}

// OpenEntry 打开 zip 内的单个文件。
func (z *zipReader) OpenEntry(_ context.Context, name string) (io.ReadCloser, Entry, error) {
	want := normalizeEntryName(name)
	for _, f := range z.zr.File {
		if normalizeEntryName(f.Name) != want {
			continue
		}
		if isEncrypted(f) {
			return nil, Entry{}, ErrEncrypted
		}
		// 声明值可能设置最高位（zip64），直接用 int64 比较会变成负数并
		// 绕过上限；实际输出也不能只信声明值，必须再套一道字节闸门。
		if f.UncompressedSize64 > math.MaxInt64 ||
			(z.limits.MaxEntryBytes > 0 && f.UncompressedSize64 > uint64(z.limits.MaxEntryBytes)) {
			return nil, Entry{}, fmt.Errorf("%w：%s 解压后 %d 字节，超过预览上限 %d",
				ErrEntryTooLarge, f.Name, f.UncompressedSize64, z.limits.MaxEntryBytes)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, Entry{}, err
		}
		if z.limits.MaxEntryBytes > 0 {
			rc = &limitedReadCloser{rc: rc, remain: z.limits.MaxEntryBytes, max: z.limits.MaxEntryBytes}
		}
		return rc, zipEntry(f), nil
	}
	return nil, Entry{}, os.ErrNotExist
}

// Extract 解压 zip 到目标目录。
func (z *zipReader) Extract(ctx context.Context, opts Options, onProgress func(Progress)) (*Report, error) {
	limit := newLimiter(opts)
	rep := &Report{Format: FormatZip.String(), Dest: opts.Dest}
	prog := Progress{Total: len(z.zr.File)}
	start := time.Now()
	var archiveSize int64
	if fi, err := z.f.Stat(); err == nil {
		archiveSize = fi.Size()
	}
	spans := zipDataSpans(z.zr, archiveSize)

	if err := mkdirAll(opts, opts.Dest, 0o755); err != nil {
		return nil, err
	}

	for _, f := range z.zr.File {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		report := func() {
			if onProgress != nil {
				prog.Current = normalizeEntryName(f.Name)
				onProgress(prog)
			}
		}

		if isEncrypted(f) {
			return rep, fmt.Errorf("条目 %q：%w", f.Name, ErrEncrypted)
		}

		target, err := secureJoin(opts.Dest, f.Name)
		if err != nil {
			rep.Warnings = append(rep.Warnings, err.Error())
			prog.Skipped++
			rep.Skipped++
			report()
			continue
		}

		if f.FileInfo().IsDir() {
			// 目录也要计入条目数：解压炸弹不一定要有文件内容 ——
			// 一个几百 KB 的包可以声明上百万个空目录，把 inode 吃光。
			// 早先只在写文件的路径上 checkNext，目录分支整个绕过了上限。
			if err := authorizeWrite(opts, target); err != nil {
				return rep, err
			}
			if err := limit.checkNext(f.Name, 0); err != nil {
				return rep, err // 与文件分支一致：超限即中止，不静默跳过
			}
			// 目录条目同样要防软链：目标位置可能已有一条指向根外的软链，
			// os.MkdirAll 会顺着它在根外建目录。
			if !opts.AllowSymlink {
				if err := fsutil.EnsureWithin(opts.Dest, target); err != nil {
					rep.Warnings = append(rep.Warnings, err.Error())
					prog.Skipped++
					rep.Skipped++
					report()
					continue
				}
			}
			if err := authorizeParents(opts, target); err != nil {
				return rep, err
			}
			if err := mkdirAll(opts, target, 0o755); err != nil {
				return rep, err
			}
			prog.Dirs++
			rep.Dirs++
			report()
			continue
		}

		// zip 中可能出现 0 字节的目录占位项。
		if target == opts.Dest && f.UncompressedSize64 == 0 {
			prog.Skipped++
			rep.Skipped++
			report()
			continue
		}

		if err := authorizeWrite(opts, target); err != nil {
			return rep, err
		}
		// 声明值不可信：最高位可让 int64 变负数，从而绕过累计上限；
		// 压缩大小也可能被伪造得很大，用来稀释压缩比检查。坏值一律按 0
		// 处理，由写出的 limitedWriter 按实际字节兜底。
		declared := int64(0)
		if f.UncompressedSize64 <= math.MaxInt64 {
			declared = int64(f.UncompressedSize64)
		}
		if err := limit.checkNext(f.Name, declared); err != nil {
			return rep, err
		}

		rc, err := f.Open()
		if err != nil {
			return rep, err
		}
		// 用物理跨度而不是可伪造的 CompressedSize64 计算压缩比。
		compressed := spans[f]
		n, werr := writeFile(target, rc, opts, limit, compressed, f.Name, f.Modified)
		rc.Close()
		if werr != nil {
			if os.IsExist(werr) {
				prog.Skipped++
				rep.Skipped++
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s 已存在，已跳过", normalizeEntryName(f.Name)))
				report()
				continue
			}
			return rep, werr
		}
		prog.Files++
		prog.Bytes += n
		rep.Files++
		rep.Bytes += n
		report()
	}

	rep.ElapsedMs = time.Since(start).Milliseconds()
	return rep, nil
}
