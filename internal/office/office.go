// Package office 从 OOXML 文档（docx / xlsx / pptx）里提取内容，
// 供网页预览。
//
// 设计取舍：
//
//   - **只做内容提取，不做版面还原**。docx/xlsx/pptx 本质是 zip + XML，
//     用标准库就能解出段落、表格、单元格与文字。而分页、字体、颜色、
//     图表、嵌入图片这些要靠一个完整的排版引擎 —— 那要么引入第三方库，
//     要么内嵌 LibreOffice，两者都与本项目的「零第三方依赖」相悖。
//     所以这里的产出是「内容读得到」，不是「长得和 Word 一样」。
//
//   - **输出中性模型，不输出 HTML**。解析结果是一棵 Block 树，
//     交给前端用 DOM API 渲染。这样服务端不生成任何标记，
//     文档里的文本再刁钻也注入不进页面（前端一律 textContent）。
//     返回 HTML 字符串的写法要处处记得转义，漏一处就是一个 XSS。
//
//   - **老的 .doc / .xls / .ppt 不支持**。那是 OLE2 复合文档，二进制格式，
//     要从头写解析器且极易出错。这类文件仍然走下载。
package office

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// 提取上限。
//
// OOXML 是用户上传的内容，里面的数字（条目大小、单元格数量）全都不可信，
// 所以每一层都要有硬上限，而不是「相信 zip 目录里写的 size」。
const (
	// maxEntryBytes 单个 XML 条目解压后的读取上限。
	// word/document.xml 这类主文档通常远小于此，超了就说明不对劲。
	maxEntryBytes = 32 << 20
	// maxTotalBytes 一次预览读入的全部条目总量上限。
	maxTotalBytes = 64 << 20
	// maxParts 最多解析多少个部分（工作表 / 幻灯片）。
	maxParts = 200
	// maxBlocks 单个部分最多保留多少个块。
	maxBlocks = 20000
	// maxTableRows / maxTableCols 表格规模上限。
	maxTableRows = 2000
	maxTableCols = 200
	// maxRunLen 单个文本片段的长度上限，防止一个超长 run 撑爆响应。
	maxRunLen = 100_000
	// maxEntries 扫描 zip 目录时最多看多少个条目。
	maxEntries = 20000
)

// ErrNotOffice 表示这个文件不是可解析的 OOXML 文档。
var ErrNotOffice = errors.New("不是可解析的 Office 文档")

// Doc 是从一份文档里提取出来的全部内容。
type Doc struct {
	// Format 为 docx / xlsx / pptx。
	Format string `json:"format"`
	// Title 取自文档属性，取不到时为空。
	Title string `json:"title,omitempty"`
	// Parts 是文档的组成部分：docx 恒为 1 个；xlsx 一个工作表一个；
	// pptx 一张幻灯片一个。
	Parts []Part `json:"parts"`
	// Truncated 表示内容因超限被截断，界面上要提示。
	Truncated bool `json:"truncated"`
	// Notes 是给用户看的说明（例如「跳过了 N 张图片」）。
	Notes []string `json:"notes,omitempty"`
}

// Part 是文档的一个组成部分。
type Part struct {
	// Name 是显示名：工作表名 / 「第 N 张幻灯片」。
	Name string `json:"name,omitempty"`
	// Blocks 是这个部分的内容块。
	Blocks []Block `json:"blocks"`
}

// Block 是一个块级元素。
type Block struct {
	// Type 为 heading / para / list / table。
	Type string `json:"type"`
	// Level 是标题层级（1-6），仅 heading 有意义。
	Level int `json:"level,omitempty"`
	// Ordered 表示有序列表，仅 list 有意义。
	Ordered bool `json:"ordered,omitempty"`
	// Runs 是带样式的文本片段。
	Runs []Run `json:"runs,omitempty"`
	// Rows 是表格内容，仅 table 有意义。
	Rows [][]Cell `json:"rows,omitempty"`
	// Header 表示这一行是表头，仅表格内使用。
}

// Run 是一段带样式的文本。
type Run struct {
	Text      string `json:"t"`
	Bold      bool   `json:"b,omitempty"`
	Italic    bool   `json:"i,omitempty"`
	Underline bool   `json:"u,omitempty"`
}

// Cell 是表格里的一个单元格。
type Cell struct {
	Text string `json:"t"`
	// ColSpan 为横向合并数，1 表示不合并。
	ColSpan int `json:"cs,omitempty"`
	// Header 表示这是表头单元格。
	Header bool `json:"h,omitempty"`
}

// Options 控制一次提取。
type Options struct {
	// MaxParts 覆盖默认的部分数上限（0 表示用默认值）。
	MaxParts int
}

// Extract 从 zip 里解析出文档内容。
//
// 调用方负责先把文件打开成 zip.Reader（并已经确认它不是加密包）。
func Extract(zr *zip.Reader, format string, opts Options) (*Doc, error) {
	if len(zr.File) > maxEntries {
		return nil, fmt.Errorf("%w：条目数 %d 超过上限 %d", ErrNotOffice, len(zr.File), maxEntries)
	}
	rd := &reader{zr: zr, budget: maxTotalBytes}
	if opts.MaxParts > 0 {
		rd.maxParts = opts.MaxParts
	} else {
		rd.maxParts = maxParts
	}

	var (
		doc *Doc
		err error
	)
	switch format {
	case "docx":
		doc, err = rd.docx()
	case "xlsx":
		doc, err = rd.xlsx()
	case "pptx":
		doc, err = rd.pptx()
	default:
		return nil, ErrNotOffice
	}
	if err != nil {
		return nil, err
	}
	doc.Format = format
	if rd.truncated {
		doc.Truncated = true
	}
	return doc, nil
}

// reader 负责按条目读取 XML，并守住总量上限。
type reader struct {
	zr        *zip.Reader
	budget    int64 // 剩余可读字节数
	truncated bool
	maxParts  int
}

// readEntry 读出包内某个条目的全部内容。
//
// 两道上限：
//   - 每个条目不超过 maxEntryBytes —— **不能信 f.UncompressedSize64**，
//     那是 zip 目录里写的声明值，攻击者可以随便填；
//   - 累计不超过 maxTotalBytes —— 一个包里塞满大 XML 同样能把内存吃光。
//
// 用 io.LimitedReader 而不是先把数据读出来再判断长度：后者意味着
// 攻击者已经赢了 —— 内存已经分配出去了。
func (r *reader) readEntry(name string) ([]byte, error) {
	f := r.find(name)
	if f == nil {
		return nil, nil
	}
	limit := int64(maxEntryBytes)
	if r.budget < limit {
		limit = r.budget
	}
	if limit <= 0 {
		r.truncated = true
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	lr := &io.LimitedReader{R: rc, N: limit + 1}
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		// 读到上限还没结束 —— 说明这个条目超限，截断处理。
		r.truncated = true
		data = data[:limit]
	}
	r.budget -= int64(len(data))
	return data, nil
}

// find 按包内路径找一个条目（路径大小写敏感，统一用 /）。
func (r *reader) find(name string) *zip.File {
	want := strings.TrimPrefix(path.Clean("/"+name), "/")
	for _, f := range r.zr.File {
		if strings.TrimPrefix(path.Clean("/"+f.Name), "/") == want {
			return f
		}
	}
	return nil
}

// entriesUnder 列出某个目录下、文件名匹配 fn 的条目，按名字排序。
//
// 用它来找 xl/worksheets/*.xml、ppt/slides/*.xml 这类编号条目。
func (r *reader) entriesUnder(dir string, fn func(base string) bool) []string {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	for _, f := range r.zr.File {
		name := f.Name
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[len(prefix):]
		// 只要直接子级，且不含更深目录。
		if strings.Contains(rest, "/") || rest == "" {
			continue
		}
		if fn(rest) {
			out = append(out, name)
		}
	}
	sortByNumericSuffix(out)
	return out
}

// appendBlock 追加一个块，超出上限时打截断标记。
func (r *reader) appendBlock(dst []Block, b Block) []Block {
	if len(dst) >= maxBlocks {
		r.truncated = true
		return dst
	}
	return append(dst, b)
}

// clipRun 截断过长的文本片段。
func clipRun(s string) string {
	if len(s) <= maxRunLen {
		return s
	}
	return s[:maxRunLen] + "…"
}
