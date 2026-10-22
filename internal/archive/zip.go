package archive

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/xxl6097/gofs/internal/fsutil"
)

// zipReader 实现 zip 归档的读取。
type zipReader struct {
	f      *os.File
	zr     *zip.Reader
	limits Limits
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
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("解析 zip 失败: %w", err)
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
	for _, f := range z.zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if z.limits.MaxFiles > 0 && len(out) >= z.limits.MaxFiles {
			break
		}
		out = append(out, zipEntry(f))
	}
	return out, nil
}

// zipEntry 把 zip.File 转换为统一的 Entry。
func zipEntry(f *zip.File) Entry {
	e := Entry{
		Name:           normalizeEntryName(f.Name),
		Size:           int64(f.UncompressedSize64),
		CompressedSize: int64(f.CompressedSize64),
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
		if z.limits.MaxEntryBytes > 0 && int64(f.UncompressedSize64) > z.limits.MaxEntryBytes {
			return nil, Entry{}, fmt.Errorf("%w：%s 解压后 %d 字节，超过预览上限 %d",
				ErrEntryTooLarge, f.Name, f.UncompressedSize64, z.limits.MaxEntryBytes)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, Entry{}, err
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

	if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
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
			if err := os.MkdirAll(target, 0o755); err != nil {
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

		if err := limit.checkNext(f.Name, int64(f.UncompressedSize64)); err != nil {
			return rep, err
		}

		rc, err := f.Open()
		if err != nil {
			return rep, err
		}
		n, werr := writeFile(target, rc, opts, limit, int64(f.CompressedSize64), f.Name, f.Modified)
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
