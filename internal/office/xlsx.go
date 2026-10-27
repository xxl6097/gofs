package office

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// xlsx 解析工作簿。
//
// 需要三个地方：
//   - xl/workbook.xml        —— 工作表名与顺序（sheetN.xml 的编号不一定连续）
//   - xl/sharedStrings.xml   —— 字符串池；单元格里 t="s" 时值是这个池的下标
//   - xl/worksheets/*.xml    —— 实际单元格
//
// 单元格 <c r="B3" t="s"><v>12</v></c>：
// r 是 A1 记法位置，t 是类型（s=共享字符串, str=公式串, inlineStr=内联,
// 缺省=数字），v 是值。
type xlsxParser struct {
	rd      *reader
	strings []string // 共享字符串池
}

// xlsx 提取全部工作表。
//
// 工作表名与 XML 文件的对应关系**不能按文件名猜**：workbook.xml 里的
// <sheet r:id="rId3"> 指向 workbook.xml.rels 中的 Target，两者编号常常
// 与 sheet1.xml / sheet2.xml 的顺序对不上（重命名、删表、复制表之后
// Excel 不会重排文件名）。按名字配对时猜错，标签就会指着另一张表。
// 只有 rels 缺失（手写 / 残缺的包）才退回「按编号排序后按下标配对」。
func (r *reader) xlsx() (*Doc, error) {
	p := &xlsxParser{rd: r}

	if data, err := r.readEntry("xl/sharedStrings.xml"); err == nil && len(data) > 0 {
		p.loadSharedStrings(data)
	}

	refs := p.sheetRefs()
	targets := p.workbookTargets()

	// rels 不可用时的退路：xl/worksheets/ 下按编号排序的 sheet*.xml。
	sheetPaths := r.entriesUnder("xl/worksheets", func(base string) bool {
		return strings.HasPrefix(base, "sheet") && strings.HasSuffix(base, ".xml")
	})
	if len(refs) == 0 && len(sheetPaths) == 0 {
		return nil, ErrNotOffice
	}

	var parts []Part
	// addPart 读取一张表；文件不存在或是空条目时返回 false。
	addPart := func(name, entry string) bool {
		data, err := r.readEntry(entry)
		if err != nil || len(data) == 0 {
			return false
		}
		parts = append(parts, Part{Name: name, Blocks: p.sheet(data)})
		return true
	}

	if len(refs) > 0 {
		for i, ref := range refs {
			if len(parts) >= r.maxParts {
				r.truncated = true
				break
			}
			name := ref.name
			if name == "" {
				name = fmt.Sprintf("工作表 %d", i+1)
			}
			entry := resolvePartTarget(targets[ref.rid])
			if entry == "" {
				entry = fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
			}
			if addPart(name, entry) {
				continue
			}
			// rels 指向的条目读不到（残缺包）：按排序后的第 i 个兜底。
			if i < len(sheetPaths) {
				addPart(name, sheetPaths[i])
			}
		}
	} else {
		for i, sp := range sheetPaths {
			if len(parts) >= r.maxParts {
				r.truncated = true
				break
			}
			addPart(fmt.Sprintf("工作表 %d", i+1), sp)
		}
	}
	if len(parts) == 0 {
		return nil, ErrNotOffice
	}
	return &Doc{Parts: parts}, nil
}

// loadSharedStrings 载入字符串池。
func (p *xlsxParser) loadSharedStrings(data []byte) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	p.strings = make([]string, 0, 256)
	var sb strings.Builder
	guard := &xmlGuard{}
	inSI, inT := false, false
	for {
		tok, err := guard.token(dec)
		if err != nil {
			if errors.Is(err, ErrNotOffice) {
				p.rd.truncated = true
			}
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				sb.Reset()
			case "t":
				inT = true
			}
		case xml.CharData:
			if inSI && inT {
				sb.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "si":
				inSI = false
				if len(p.strings) < 1_000_000 {
					p.strings = append(p.strings, sb.String())
				}
			}
		}
	}
}

// sheetRef 是 workbook.xml 里的一条工作表声明。
type sheetRef struct {
	name string
	// rid 是 r:id，指向 workbook.xml.rels 里的关系条目。
	rid string
}

// sheetRefs 按 workbook.xml 中的显示顺序读出工作表名与关系 ID。
//
// 没写 r:id 的条目也保留：rels 缺失时还要靠顺序号码兜底。
func (p *xlsxParser) sheetRefs() []sheetRef {
	data, err := p.rd.readEntry("xl/workbook.xml")
	if err != nil || len(data) == 0 {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	guard := &xmlGuard{}
	var refs []sheetRef
	for {
		tok, err := guard.token(dec)
		if err != nil {
			if errors.Is(err, ErrNotOffice) {
				p.rd.truncated = true
			}
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sheet" {
			continue
		}
		// 带 r: 前缀的属性经 encoding/xml 解析后 Local 是 "id"，
		// 与普通属性名不会混淆（工作表 id 是 "sheetId"）。
		refs = append(refs, sheetRef{name: attrVal(se, "name"), rid: attrVal(se, "id")})
	}
	return refs
}

// workbookTargets 读 xl/_rels/workbook.xml.rels，把关系 ID 映射到包内路径。
//
// rels 是 XML 里的 URI 引用：Target 可能是相对 xl/ 的路径、以 / 开头的
// 绝对路径，也可能带百分号编码 —— 三种都要落到 zip 的真实条目名上。
func (p *xlsxParser) workbookTargets() map[string]string {
	data, err := p.rd.readEntry("xl/_rels/workbook.xml.rels")
	if err != nil || len(data) == 0 {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	guard := &xmlGuard{}
	targets := make(map[string]string)
	for {
		tok, err := guard.token(dec)
		if err != nil {
			if errors.Is(err, ErrNotOffice) {
				p.rd.truncated = true
			}
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Relationship" {
			continue
		}
		id, target := attrVal(se, "Id"), attrVal(se, "Target")
		if id == "" || target == "" {
			continue
		}
		targets[id] = target
	}
	return targets
}

// resolvePartTarget 把 rels 的 Target 换成 zip 条目路径。
func resolvePartTarget(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if u, err := url.PathUnescape(target); err == nil {
		target = u
	}
	if strings.HasPrefix(target, "/") {
		return strings.TrimPrefix(path.Clean(target), "/")
	}
	return path.Join("xl", target)
}

// sheet 解析一张工作表，产出一个 table 块。
func (p *xlsxParser) sheet(data []byte) []Block {
	dec := xml.NewDecoder(bytes.NewReader(data))

	var (
		rows     [][]Cell
		curRow   []Cell
		maxCol   int
		curCell  *Cell
		cellRef  string
		cellType string
		inV      bool
		inT      bool
		val      strings.Builder
		truncRow bool
		// 下一个应当出现的行号（1-based）。
		// 表格里的行号不是连续的：数据从第 2 行开始时，<row> 直接写 r="2"，
		// 中间那行根本不出现。不按 r 补空行的话，整张表会往上挪一格。
		nextRow     = 1
		pendingRows int
	)
	flushCell := func() {
		if curCell == nil {
			return
		}
		text := strings.TrimSpace(val.String())
		switch cellType {
		case "s": // 共享字符串，值是下标
			if n, err := strconv.Atoi(text); err == nil && n >= 0 && n < len(p.strings) {
				text = p.strings[n]
			}
		case "b": // 布尔
			if text == "1" {
				text = "TRUE"
			} else {
				text = "FALSE"
			}
		}
		if !p.rd.chargeCell() {
			curCell = nil
			val.Reset()
			return
		}
		curCell.Text = p.rd.chargeText(text)

		// 按 A1 记法把单元格放到正确的列上：中间的空单元格要补位，
		// 否则 B 列的值会挤到 A 列去。
		if col := colIndex(cellRef); col >= 0 {
			for len(curRow) < col && len(curRow) < maxTableCols {
				curRow = append(curRow, Cell{Text: "", ColSpan: 1})
			}
			if len(curRow) < maxTableCols {
				curRow = append(curRow, *curCell)
			} else {
				truncRow = true
			}
			if len(curRow) > maxCol {
				maxCol = len(curRow)
			}
		} else if len(curRow) < maxTableCols {
			curRow = append(curRow, *curCell)
		} else {
			truncRow = true
		}
		curCell = nil
		val.Reset()
	}

	guard := &xmlGuard{}
loop:
	for {
		tok, err := guard.token(dec)
		if err != nil {
			if errors.Is(err, ErrNotOffice) {
				p.rd.truncated = true
			}
			break loop
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				curRow = nil
				if rn := atoiSafe(attrVal(t, "r")); rn > nextRow {
					pendingRows = rn - nextRow
				}
			case "c":
				if curCell == nil {
					curCell = &Cell{ColSpan: 1}
				}
				cellRef = attrVal(t, "r")
				cellType = attrVal(t, "t")
			case "v":
				inV = true
			case "t": // 内联字符串
				inT = true
			}
		case xml.CharData:
			if inV || inT {
				val.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "t":
				inT = false
			case "c":
				flushCell()
			case "row":
				flushCell()
				// 先补出中间被跳过的空行（宽度留空，最后统一补齐）。
				for ; pendingRows > 0; pendingRows-- {
					if len(rows) >= maxTableRows {
						truncRow = true
						break loop
					}
					rows = append(rows, nil)
					nextRow++
				}
				pendingRows = 0
				if len(curRow) > 0 {
					if len(rows) >= maxTableRows {
						truncRow = true
						break loop
					}
					rows = append(rows, curRow)
				}
				nextRow++
				curRow = nil
			case "sheetData":
				break loop
			}
		}
	}
	flushCell()

	if len(rows) == 0 {
		return nil
	}
	// 补齐每行的列数：表格渲染时列数不一致会错位。
	for i := range rows {
		for len(rows[i]) < maxCol {
			rows[i] = append(rows[i], Cell{Text: "", ColSpan: 1})
		}
	}
	if truncRow {
		p.rd.truncated = true
	}
	// 首行当表头：表格内容里第一行通常是字段名。
	for i := range rows[0] {
		rows[0][i].Header = true
	}
	return []Block{{Type: "table", Rows: rows}}
}

// colIndex 把 A1 记法里的列字母转成 0-based 列号（A→0, B→1, AA→26）。
// 解析不出来时返回 -1。
func colIndex(ref string) int {
	if ref == "" {
		return -1
	}
	n := 0
	seen := 0
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		switch {
		case c >= 'A' && c <= 'Z':
			n = n*26 + int(c-'A'+1)
			seen++
		case c >= 'a' && c <= 'z':
			n = n*26 + int(c-'a'+1)
			seen++
		default:
			if seen == 0 {
				return -1
			}
			return n - 1
		}
	}
	if seen == 0 {
		return -1
	}
	return n - 1
}
