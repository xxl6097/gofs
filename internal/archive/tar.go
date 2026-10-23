package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/gofs/internal/fsutil"
)

// tempFileThreshold 超过该大小的包内文件在预览下载时改落临时文件，避免占用过多内存。
const tempFileThreshold = 32 << 20

// tarReader 实现 tar / tar.gz 归档的读取。
// tar 是流式的，因此每次操作都从头重新扫描一遍。
type tarReader struct {
	f         *os.File
	format    Format
	gzipped   bool
	mu        sync.Mutex
	limits    Limits
	truncated bool
}

// SetLimits 设置列表与单条目预览上限。
func (t *tarReader) SetLimits(l Limits) { t.limits = l }

// newTarReader 构造未压缩 tar 读取器。
func newTarReader(f *os.File) (*tarReader, error) {
	return &tarReader{f: f, format: FormatTar}, nil
}

// newTarGzReader 构造 tar.gz 读取器。
func newTarGzReader(f *os.File) (*tarReader, error) {
	return &tarReader{f: f, format: FormatTarGz, gzipped: true}, nil
}

// Format 返回格式。
func (t *tarReader) Format() Format { return t.format }

// Close 关闭文件。
func (t *tarReader) Close() error { return t.f.Close() }

// scanSlack 是扫描预算的余量。
//
// tar.Reader 在读到结尾时仍可能再请求一个块；如果预算恰好等于解压后
// 大小，这个「最后一次读」会被误判为超限。多给一个块即可避免误杀。
const scanSlack = 512

// budgetReader 把从底层读出的（已解压）字节计入预算，超限返回 ErrScanTooLarge。
type budgetReader struct {
	r      io.Reader
	remain int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.remain <= 0 {
		return 0, ErrScanTooLarge
	}
	if int64(len(p)) > b.remain {
		p = p[:b.remain]
	}
	n, err := b.r.Read(p)
	b.remain -= int64(n)
	return n, err
}

// reader 从头构造一个 tar.Reader（不限制扫描量，供 Extract 使用）。
func (t *tarReader) reader() (*tar.Reader, io.Closer, error) {
	return t.readerWithBudget(0)
}

// scanReader 与 reader 相同，但把解压后的扫描量限制在 MaxScanBytes 内
// （未设置时回落到 MaxTotalBytes）。
//
// 只用于 List / OpenEntry：这两个操作不需要写出文件，却必须顺序扫描整个
// 流，是高压缩比 tar.gz 最容易打穿 CPU/IO 的入口。Extract 不需要这层，
// 它有独立的写出量限额。
func (t *tarReader) scanReader() (*tar.Reader, io.Closer, error) {
	budget := t.limits.MaxScanBytes
	if budget <= 0 {
		budget = t.limits.MaxTotalBytes
	}
	if budget > 0 {
		budget += scanSlack
	}
	return t.readerWithBudget(budget)
}

func (t *tarReader) readerWithBudget(budget int64) (*tar.Reader, io.Closer, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	var r io.Reader = t.f
	var closer io.Closer
	if t.gzipped {
		gz, err := gzip.NewReader(t.f)
		if err != nil {
			return nil, nil, fmt.Errorf("解析 gzip 失败: %w", err)
		}
		r, closer = gz, gz
	}
	if budget > 0 {
		r = &budgetReader{r: r, remain: budget}
	}
	return tar.NewReader(r), closer, nil
}

// List 遍历 tar 内所有条目。
func (t *tarReader) List(ctx context.Context) ([]Entry, error) {
	tr, closer, err := t.scanReader()
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	out := make([]Entry, 0, 128)
	t.truncated = false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 tar 失败: %w", err)
		}
		if t.limits.MaxFiles > 0 && len(out) >= t.limits.MaxFiles {
			// 已经读到一个额外 header，说明后面还有条目。
			t.truncated = true
			break
		}
		out = append(out, tarEntry(hdr))
	}
	return out, nil
}

// Truncated 报告上一次 List 是否因为 MaxFiles 提前结束。
func (t *tarReader) Truncated() bool { return t.truncated }

// tarEntry 把 tar.Header 转换为统一的 Entry。
func tarEntry(h *tar.Header) Entry {
	e := Entry{
		Name:       normalizeEntryName(h.Name),
		Size:       h.Size,
		IsDir:      h.Typeflag == tar.TypeDir,
		ModTime:    h.ModTime.Format(time.RFC3339),
		Mode:       h.FileInfo().Mode().String(),
		LinkTarget: h.Linkname,
	}
	if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
		if !e.IsDir {
			e.Size = 0
		}
	}
	return e
}

// OpenEntry 读取包内单个文件。tar 流式读取的特性决定了这里必须把内容缓冲下来。
func (t *tarReader) OpenEntry(ctx context.Context, name string) (io.ReadCloser, Entry, error) {
	want := normalizeEntryName(name)
	tr, closer, err := t.scanReader()
	if err != nil {
		return nil, Entry{}, err
	}
	defer func() {
		if closer != nil {
			closer.Close()
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return nil, Entry{}, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, Entry{}, fmt.Errorf("读取 tar 失败: %w", err)
		}
		if normalizeEntryName(hdr.Name) != want {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, Entry{}, fmt.Errorf("%q 不是普通文件，无法直接下载", want)
		}
		if t.limits.MaxEntryBytes > 0 && hdr.Size > t.limits.MaxEntryBytes {
			return nil, Entry{}, fmt.Errorf("%w：%s 解压后 %d 字节，超过预览上限 %d",
				ErrEntryTooLarge, want, hdr.Size, t.limits.MaxEntryBytes)
		}

		if hdr.Size <= tempFileThreshold {
			buf := bytes.NewBuffer(make([]byte, 0, min64(hdr.Size, 1<<20)))
			if _, err := io.Copy(buf, tr); err != nil {
				return nil, Entry{}, err
			}
			return io.NopCloser(buf), tarEntry(hdr), nil
		}

		tmp, err := os.CreateTemp("", "gofs-archive-*")
		if err != nil {
			return nil, Entry{}, err
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, Entry{}, err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, Entry{}, err
		}
		return &tempFileReader{f: tmp}, tarEntry(hdr), nil
	}
	return nil, Entry{}, os.ErrNotExist
}

// Extract 解压 tar / tar.gz 到目标目录。
func (t *tarReader) Extract(ctx context.Context, opts Options, onProgress func(Progress)) (*Report, error) {
	limit := newLimiter(opts)
	rep := &Report{Format: t.format.String(), Dest: opts.Dest}
	prog := Progress{Total: -1}
	start := time.Now()

	if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
		return nil, err
	}

	tr, closer, err := t.reader()
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}

	for {
		if err := ctx.Err(); err != nil {
			rep.ElapsedMs = time.Since(start).Milliseconds()
			return rep, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			rep.ElapsedMs = time.Since(start).Milliseconds()
			return rep, fmt.Errorf("读取 tar 失败: %w", err)
		}

		name := normalizeEntryName(hdr.Name)
		report := func() {
			if onProgress != nil {
				prog.Current = name
				onProgress(prog)
			}
		}

		// 符号链接与硬链接一律跳过，避免软链逃逸出根目录。
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			prog.Skipped++
			rep.Skipped++
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("已跳过链接条目 %s -> %s", name, hdr.Linkname))
			report()
			continue
		}

		target, err := secureJoin(opts.Dest, hdr.Name)
		if err != nil {
			prog.Skipped++
			rep.Skipped++
			rep.Warnings = append(rep.Warnings, err.Error())
			report()
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := authorizeWrite(opts, target); err != nil {
				rep.ElapsedMs = time.Since(start).Milliseconds()
				return rep, err
			}
			// 目录同样计入条目数，理由见 zip 侧的同名分支。
			if err := limit.checkNext(hdr.Name, 0); err != nil {
				rep.ElapsedMs = time.Since(start).Milliseconds()
				return rep, err // 与文件分支一致：超限即中止
			}
			// 同 zip 侧：目录目标也不能穿过根外软链。
			if !opts.AllowSymlink {
				if err := fsutil.EnsureWithin(opts.Dest, target); err != nil {
					prog.Skipped++
					rep.Skipped++
					rep.Warnings = append(rep.Warnings, err.Error())
					report()
					continue
				}
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				rep.ElapsedMs = time.Since(start).Milliseconds()
				return rep, err
			}
			prog.Dirs++
			rep.Dirs++
			report()
			continue

		case tar.TypeReg, tar.TypeRegA:
			// 继续往下处理。

		default:
			// 设备节点、FIFO 等特殊类型直接跳过。
			prog.Skipped++
			rep.Skipped++
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("已跳过特殊条目 %s（类型 %c）", name, rune(hdr.Typeflag)))
			report()
			continue
		}

		if err := authorizeWrite(opts, target); err != nil {
			rep.ElapsedMs = time.Since(start).Milliseconds()
			return rep, err
		}
		if err := limit.checkNext(name, hdr.Size); err != nil {
			rep.ElapsedMs = time.Since(start).Milliseconds()
			return rep, err
		}

		n, werr := writeFile(target, tr, opts, limit, 0, name, hdr.ModTime)
		if werr != nil {
			if os.IsExist(werr) {
				prog.Skipped++
				rep.Skipped++
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s 已存在，已跳过", name))
				report()
				continue
			}
			rep.ElapsedMs = time.Since(start).Milliseconds()
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

// tempFileReader 在读到 EOF 时自动清理临时文件。
type tempFileReader struct {
	f *os.File
}

func (t *tempFileReader) Read(p []byte) (int, error) { return t.f.Read(p) }

func (t *tempFileReader) Close() error {
	name := t.f.Name()
	err := t.f.Close()
	os.Remove(name)
	return err
}

// min64 返回两个 int64 中的较小值。
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// stripGzExt 去掉文件名末尾的 .gz / .tgz 后缀。
func stripGzExt(name string) string {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	l := strings.ToLower(base)
	if strings.HasSuffix(l, ".gz") {
		return base[:len(base)-3]
	}
	if strings.HasSuffix(l, ".tgz") {
		return base[:len(base)-4] + ".tar"
	}
	return base + ".out"
}
