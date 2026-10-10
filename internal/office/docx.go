package office

import (
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strings"
)

// docx 解析 word/document.xml。
//
// 结构大致是：
//
//	<w:body>
//	  <w:p>                     段落
//	    <w:pPr><w:pStyle w:val="Heading1"/></w:pPr>
//	    <w:r><w:rPr><w:b/></w:rPr><w:t>文字</w:t></w:r>
//	  </w:p>
//	  <w:tbl><w:tr><w:tc><w:p>…</w:p></w:tc></w:tr></w:tbl>
//	</w:body>
//
// 关于 XXE：Go 的 encoding/xml **不会**解析外部实体，也不做 DTD 实体展开。
// 所以 `<!ENTITY xxe SYSTEM "file:///etc/passwd">` 与「十亿笑声」这类攻击
// 在这里天然不成立 —— 这正是不引第三方 XML 库的一个好处。
type docxParser struct {
	rd *reader

	blocks []Block
	notes  []string

	// 表格嵌套深度：表格里的段落要并进单元格，不能漏到正文里。
	tblDepth int
	rows     [][]Cell
	curRow   []Cell
	cell     *Cell
	cellRuns []Run

	// 当前段落
	runs          []Run
	pendingStyles []func(*Run)
	inText        bool
	textBuf       strings.Builder
	pStyle        string
	inNumPr       bool
	numLevel      int
}

var (
	reHeadingStyle = regexp.MustCompile(`(?i)^(?:Heading|标题)\s*([1-6])$`)
	reQuoteStyle   = regexp.MustCompile(`(?i)^(?:Quote|IntenseQuote|引用)`)
)

// docx 提取正文。
func (r *reader) docx() (*Doc, error) {
	data, err := r.readEntry("word/document.xml")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrNotOffice
	}

	p := &docxParser{rd: r}
	dec := xml.NewDecoder(bytes.NewReader(data))
	// 文档可能声明非 UTF-8 编码。声明了却解析不了时原样透传，
	// 让 xml 包跳过非法字节 —— 总比整份解析失败强。
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	started := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// XML 坏了：返回已经解析出来的部分，用户至少能看到前面几段。
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !started && t.Name.Local == "body" {
				started = true
			}
			p.start(t)
		case xml.CharData:
			// 文字只在 <w:t> 里才算正文。漏掉这个分支的话，
			// 所有段落都会变成空的 —— 解析器看着「跑通了」，实际什么也没提取到。
			if p.inText {
				p.textBuf.Write(t)
			}
		case xml.EndElement:
			p.end(t)
		}
	}
	p.flushPara() // 末尾没有 </w:p> 时兜底

	notes := dedupe(p.notes)
	return &Doc{Parts: []Part{{Blocks: p.blocks}}, Notes: notes}, nil
}

// start 处理一个起始标签。
func (p *docxParser) start(t xml.StartElement) {
	switch t.Name.Local {
	case "p":
		p.resetPara()
	case "pStyle":
		p.pStyle = attrVal(t, "val")
	case "numPr":
		// 注意：这里**不**在 </w:numPr> 时清掉。
		// numPr 出现在段落的 pPr 里，而 pPr 排在所有 run 之前 ——
		// 等到 flushPara 时它早就闭合了。清早了，列表就会被当成普通段落。
		p.inNumPr = true
	case "ilvl":
		p.numLevel = atoiSafe(attrVal(t, "val"))
	case "tbl":
		p.tblDepth++
		p.rows = nil
	case "tr":
		p.curRow = nil
	case "tc":
		p.cell = &Cell{ColSpan: 1}
		p.cellRuns = nil
	case "gridSpan":
		if p.cell != nil {
			if n := atoiSafe(attrVal(t, "val")); n > 0 {
				p.cell.ColSpan = n
			}
		}
	case "t":
		p.inText = true
		p.textBuf.Reset()
	case "tab":
		p.textBuf.WriteByte('\t')
	case "br", "cr":
		p.textBuf.WriteByte('\n')
	case "b":
		p.pendingStyles = append(p.pendingStyles, func(r *Run) { r.Bold = true })
	case "i":
		p.pendingStyles = append(p.pendingStyles, func(r *Run) { r.Italic = true })
	case "u":
		p.pendingStyles = append(p.pendingStyles, func(r *Run) { r.Underline = true })
	case "drawing", "pict", "object":
		p.notes = append(p.notes, "跳过了文档中的图片或嵌入对象")
	}
}

// end 处理一个结束标签。
func (p *docxParser) end(t xml.EndElement) {
	switch t.Name.Local {
	case "p":
		p.flushPara()
	case "t":
		if p.inText {
			p.pushRun(clipRun(p.textBuf.String()))
			p.inText = false
		}
	case "tc":
		if p.cell != nil {
			var sb strings.Builder
			for _, run := range p.cellRuns {
				sb.WriteString(run.Text)
			}
			p.cell.Text = strings.TrimSpace(sb.String())
			p.curRow = append(p.curRow, *p.cell)
			p.cell = nil
			p.cellRuns = nil
		}
	case "tr":
		if len(p.curRow) > 0 {
			if len(p.rows) < maxTableRows {
				p.rows = append(p.rows, p.curRow)
			} else {
				p.rd.truncated = true
			}
			p.curRow = nil
		}
	case "tbl":
		if len(p.rows) > 0 {
			p.blocks = p.rd.appendBlock(p.blocks, Block{Type: "table", Rows: p.rows})
		}
		p.rows = nil
		if p.tblDepth > 0 {
			p.tblDepth--
		}
	}
}

// pushRun 收集一段文本，套上此前遇到的样式标记。
func (p *docxParser) pushRun(text string) {
	if text == "" {
		return
	}
	run := Run{Text: text}
	for _, fn := range p.pendingStyles {
		fn(&run)
	}
	p.pendingStyles = nil

	if p.tblDepth > 0 && p.cell != nil {
		p.cellRuns = append(p.cellRuns, run)
		return
	}
	p.runs = append(p.runs, run)
}

// resetPara 开始收集一个新段落。
func (p *docxParser) resetPara() {
	p.runs = nil
	p.pendingStyles = nil
	p.pStyle = ""
	p.inNumPr = false
	p.numLevel = 0
	p.textBuf.Reset()
	p.inText = false
}

// flushPara 把收集到的 runs 落成一个块。
func (p *docxParser) flushPara() {
	runs := p.runs
	style, inNum, level := p.pStyle, p.inNumPr, p.numLevel
	p.runs = nil
	p.pendingStyles = nil
	p.pStyle, p.inNumPr, p.numLevel = "", false, 0
	p.textBuf.Reset()
	p.inText = false

	if len(runs) == 0 || allBlank(runs) {
		return
	}
	// 表格里的段落已经并进单元格文本了，不再单独成块。
	if p.tblDepth > 0 {
		return
	}

	b := Block{Runs: runs, Type: "para"}
	switch {
	case reHeadingStyle.MatchString(style):
		b.Type = "heading"
		b.Level = atoiSafe(reHeadingStyle.FindStringSubmatch(style)[1])
	case reQuoteStyle.MatchString(style):
		b.Type = "quote"
	case inNum:
		b.Type = "list"
		b.Level = level + 1 // Word 的层级是 0-based，展示用 1-based
		b.Ordered = true
	}
	p.blocks = p.rd.appendBlock(p.blocks, b)
}

// attrVal 取属性值（忽略命名空间前缀）。
func attrVal(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// atoiSafe 解析十进制整数，失败返回 0。
func atoiSafe(s string) int {
	n := 0
	if s == "" {
		return 0
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1<<20 {
			return 0
		}
	}
	return n
}

// allBlank 判断一组 run 是否全是空白。
func allBlank(runs []Run) bool {
	for _, r := range runs {
		if strings.TrimSpace(r.Text) != "" {
			return false
		}
	}
	return true
}

// dedupe 去掉重复的提示，保持出现顺序。
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
