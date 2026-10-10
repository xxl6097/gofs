package office

import (
	"bytes"
	"encoding/xml"
	"fmt"
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
func (r *reader) xlsx() (*Doc, error) {
	p := &xlsxParser{rd: r}

	if data, err := r.readEntry("xl/sharedStrings.xml"); err == nil && len(data) > 0 {
		p.loadSharedStrings(data)
	}

	// 工作表名（拿不到就退回 sheet1、sheet2…）。
	names := p.sheetNames()

	sheetPaths := r.entriesUnder("xl/worksheets", func(base string) bool {
		return strings.HasPrefix(base, "sheet") && strings.HasSuffix(base, ".xml")
	})
	if len(sheetPaths) == 0 {
		return nil, ErrNotOffice
	}

	var parts []Part
	for i, sp := range sheetPaths {
		if i >= r.maxParts {
			r.truncated = true
			break
		}
		data, err := r.readEntry(sp)
		if err != nil || len(data) == 0 {
			continue
		}
		blocks := p.sheet(data)
		name := ""
		if i < len(names) {
			name = names[i]
		}
		if name == "" {
			name = fmt.Sprintf("工作表 %d", i+1)
		}
		parts = append(parts, Part{Name: name, Blocks: blocks})
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
	inSI, inT := false, false
	for {
		tok, err := dec.Token()
		if err != nil {
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

// sheetNames 读 xl/workbook.xml 里的工作表名。
func (p *xlsxParser) sheetNames() []string {
	data, err := p.rd.readEntry("xl/workbook.xml")
	if err != nil || len(data) == 0 {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var names []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sheet" {
			if n := attrVal(se, "name"); n != "" {
				names = append(names, n)
			}
		}
	}
	return names
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
		curCell.Text = clipRun(text)

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
		} else {
			curRow = append(curRow, *curCell)
		}
		curCell = nil
		val.Reset()
	}

loop:
	for {
		tok, err := dec.Token()
		if err != nil {
			break
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
