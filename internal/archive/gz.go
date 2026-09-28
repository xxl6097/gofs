package archive

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// gzReader 实现单文件 gzip 的读取。
type gzReader struct {
	f   *os.File
	mu  sync.Mutex
	out string // 解压后的文件名
}

// newGzReader 构造单文件 gzip 读取器。
func newGzReader(f *os.File) (*gzReader, error) {
	g := &gzReader{f: f, out: stripGzExt(filepath.Base(f.Name()))}
	// gzip 头里若写有原始文件名则优先采用。
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		if zr, err := gzip.NewReader(f); err == nil {
			if zr.Name != "" && !strings.ContainsRune(zr.Name, 0) {
				g.out = filepath.Base(strings.ReplaceAll(zr.Name, "\\", "/"))
			}
			zr.Close()
		}
		_, _ = f.Seek(0, io.SeekStart)
	}
	return g, nil
}

// Format 返回格式。
func (g *gzReader) Format() Format { return FormatGz }

// Close 关闭文件。
func (g *gzReader) Close() error { return g.f.Close() }

// List 返回唯一的逻辑条目。
func (g *gzReader) List(ctx context.Context) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := g.f.Stat()
	if err != nil {
		return nil, err
	}
	return []Entry{{
		Name:           g.out,
		Size:           -1, // 解压后大小无法在不完整扫描的情况下得知
		CompressedSize: info.Size(),
		IsDir:          false,
		ModTime:        info.ModTime().Format(time.RFC3339),
		Mode:           "-rw-r--r--",
	}}, nil
}

// open 从头构造 gzip 读取器。
func (g *gzReader) open() (*gzip.Reader, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return gzip.NewReader(g.f)
}

// OpenEntry 打开解压后的内容流。
func (g *gzReader) OpenEntry(ctx context.Context, name string) (io.ReadCloser, Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, Entry{}, err
	}
	if filepath.Base(name) != g.out && name != "" {
		return nil, Entry{}, os.ErrNotExist
	}
	zr, err := g.open()
	if err != nil {
		return nil, Entry{}, fmt.Errorf("解析 gzip 失败: %w", err)
	}
	entries, err := g.List(ctx)
	if err != nil {
		zr.Close()
		return nil, Entry{}, err
	}
	return zr, entries[0], nil
}

// Extract 把 gzip 解压为单个文件。
func (g *gzReader) Extract(ctx context.Context, opts Options, onProgress func(Progress)) (*Report, error) {
	limit := newLimiter(opts)
	rep := &Report{Format: FormatGz.String(), Dest: opts.Dest}
	prog := Progress{Total: 1}
	start := time.Now()

	if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
		return nil, err
	}
	target, err := secureJoin(opts.Dest, g.out)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(target) == filepath.Dir(opts.Dest) {
		// 目标名落在 dest 下，正常。
		_ = target
	}

	zr, err := g.open()
	if err != nil {
		return nil, fmt.Errorf("解析 gzip 失败: %w", err)
	}
	defer zr.Close()

	if err := limit.checkNext(g.out, 0); err != nil {
		return nil, err
	}

	info, _ := g.f.Stat()
	var compressed int64
	if info != nil {
		compressed = info.Size()
	}

	n, werr := writeFile(target, zr, opts, limit, compressed, g.out, time.Time{})
	if werr != nil {
		if os.IsExist(werr) {
			prog.Skipped = 1
			rep.Skipped = 1
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s 已存在，已跳过", g.out))
			if onProgress != nil {
				prog.Current = g.out
				onProgress(prog)
			}
			rep.ElapsedMs = time.Since(start).Milliseconds()
			return rep, nil
		}
		return rep, werr
	}

	prog.Current = g.out
	prog.Files = 1
	prog.Bytes = n
	rep.Files = 1
	rep.Bytes = n
	if onProgress != nil {
		onProgress(prog)
	}
	rep.ElapsedMs = time.Since(start).Milliseconds()
	return rep, nil
}
