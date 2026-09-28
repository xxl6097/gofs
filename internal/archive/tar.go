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
)

// tempFileThreshold 超过该大小的包内文件在预览下载时改落临时文件，避免占用过多内存。
const tempFileThreshold = 32 << 20

// tarReader 实现 tar / tar.gz 归档的读取。
// tar 是流式的，因此每次操作都从头重新扫描一遍。
type tarReader struct {
	f       *os.File
	format  Format
	gzipped bool
	mu      sync.Mutex
}

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

// reader 从头构造一个 tar.Reader。
func (t *tarReader) reader() (*tar.Reader, io.Closer, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	if !t.gzipped {
		return tar.NewReader(t.f), nil, nil
	}
	gz, err := gzip.NewReader(t.f)
	if err != nil {
		return nil, nil, fmt.Errorf("解析 gzip 失败: %w", err)
	}
	return tar.NewReader(gz), gz, nil
}

// List 遍历 tar 内所有条目。
func (t *tarReader) List(ctx context.Context) ([]Entry, error) {
	tr, closer, err := t.reader()
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	out := make([]Entry, 0, 128)
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
		out = append(out, tarEntry(hdr))
	}
	return out, nil
}

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
	tr, closer, err := t.reader()
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
