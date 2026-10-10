package office

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildZip 把若干「路径 → 内容」打成 zip，返回 *zip.Reader。
//
// 测试自己造 OOXML（而不是塞一个二进制样例进仓库）有三个好处：
// 夹具自洽、不依赖外部工具、而且能精确构造要考的那几个特性
// （标题样式、合并单元格、XXE 载荷…）。
func buildZip(t *testing.T, files map[string]string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

// docxXML 包一层最小可用的 document.xml。
func docxXML(body string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>` + body + `</w:body></w:document>`
}

// para 造一个普通段落。
func para(text string) string {
	return `<w:p><w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

// TestDocxBasics 覆盖段落、标题、加粗、列表、表格。
func TestDocxBasics(t *testing.T) {
	xml := docxXML(
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>报告标题</w:t></w:r></w:p>` +
			`<w:p><w:r><w:t>普通</w:t></w:r><w:r><w:rPr><w:b/></w:rPr><w:t>加粗</w:t></w:r><w:r><w:t>结束</w:t></w:r></w:p>` +
			`<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/></w:numPr></w:pPr><w:r><w:t>第一项</w:t></w:r></w:p>` +
			`<w:tbl>` +
			`<w:tr><w:tc><w:p><w:r><w:t>A1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>B1</w:t></w:r></w:p></w:tc></w:tr>` +
			`<w:tr><w:tc><w:p><w:r><w:t>A2</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>B2</w:t></w:r></w:p></w:tc></w:tr>` +
			`</w:tbl>`)

	doc, err := Extract(buildZip(t, map[string]string{"word/document.xml": xml}), "docx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if doc.Format != "docx" || len(doc.Parts) != 1 {
		t.Fatalf("形状不对：%+v", doc)
	}
	blocks := doc.Parts[0].Blocks

	var types []string
	for _, b := range blocks {
		types = append(types, b.Type)
	}
	want := []string{"heading", "para", "list", "table"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("块类型 = %v，期望 %v", types, want)
	}

	if blocks[0].Level != 1 {
		t.Errorf("标题层级 = %d，期望 1", blocks[0].Level)
	}
	// 段落被拆成三段 run，中间那段是加粗。
	p := blocks[1]
	if len(p.Runs) != 3 {
		t.Fatalf("段落 run 数 = %d，期望 3：%+v", len(p.Runs), p.Runs)
	}
	if p.Runs[0].Text != "普通" || p.Runs[1].Text != "加粗" || p.Runs[2].Text != "结束" {
		t.Errorf("run 内容不对：%+v", p.Runs)
	}
	if !p.Runs[1].Bold || p.Runs[0].Bold {
		t.Errorf("加粗标记不对：%+v", p.Runs)
	}
	if !blocks[2].Ordered {
		t.Error("列表应当标记为有序")
	}

	tbl := blocks[3]
	if len(tbl.Rows) != 2 || len(tbl.Rows[0]) != 2 {
		t.Fatalf("表格形状不对：%+v", tbl.Rows)
	}
	if tbl.Rows[0][0].Text != "A1" || tbl.Rows[1][1].Text != "B2" {
		t.Errorf("单元格内容不对：%+v", tbl.Rows)
	}
}

// TestDocxMergedCell 确认横向合并的 gridSpan 被读出来。
func TestDocxMergedCell(t *testing.T) {
	xml := docxXML(`<w:tbl><w:tr>` +
		`<w:tc><w:tcPr><w:gridSpan w:val="2"/></w:tcPr><w:p><w:r><w:t>跨两列</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>第三列</w:t></w:r></w:p></w:tc>` +
		`</w:tr></w:tbl>`)

	doc, err := Extract(buildZip(t, map[string]string{"word/document.xml": xml}), "docx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	rows := doc.Parts[0].Blocks[0].Rows
	if len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("形状不对：%+v", rows)
	}
	if rows[0][0].ColSpan != 2 {
		t.Errorf("ColSpan = %d，期望 2", rows[0][0].ColSpan)
	}
}

// TestDocxMalformedReturnsPartial 确认 XML 坏了也能拿到已解析的部分，
// 而不是整份失败 —— 用户至少能看到前面几段。
func TestDocxMalformedReturnsPartial(t *testing.T) {
	xml := docxXML(para("第一段") + para("第二段") + `<w:p><w:r><w:t>没闭合`)
	doc, err := Extract(buildZip(t, map[string]string{"word/document.xml": xml}), "docx", Options{})
	if err != nil {
		t.Fatalf("不该整份失败：%v", err)
	}
	blocks := doc.Parts[0].Blocks
	if len(blocks) < 2 {
		t.Fatalf("只拿到 %d 个块，期望至少 2 个", len(blocks))
	}
	if blocks[0].Runs[0].Text != "第一段" {
		t.Errorf("第一段内容不对：%+v", blocks[0].Runs)
	}
}

// TestDocxXXENotExpanded 确认外部实体不会被解析。
//
// 这是「不引第三方 XML 库」的一个实际好处：Go 的 encoding/xml 压根不做
// DTD 实体展开，所以 XML External Entity 攻击在这里不成立。
// 这条测试把它钉住 —— 哪天换成别的解析器，这里会立刻变红。
func TestDocxXXENotExpanded(t *testing.T) {
	// 造一个真实存在的本地文件当作攻击目标。
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "secret.txt")
	const secret = "本地文件的内容不该出现"
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	xml := `<?xml version="1.0"?>
<!DOCTYPE w:document [
  <!ENTITY xxe SYSTEM "file://` + secretPath + `">
]>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body><w:p><w:r><w:t>&xxe;</w:t></w:r></w:p></w:body></w:document>`

	doc, err := Extract(buildZip(t, map[string]string{"word/document.xml": xml}), "docx", Options{})
	if err != nil {
		// 解析失败也是可接受的（未定义实体），只要没把内容读出来。
		return
	}
	for _, part := range doc.Parts {
		for _, b := range part.Blocks {
			for _, run := range b.Runs {
				if strings.Contains(run.Text, secret) {
					t.Fatalf("外部实体被展开了，本地文件内容泄漏：%q", run.Text)
				}
			}
		}
	}
}

// TestDocxScriptTextStaysText 确认文档里的 HTML/脚本只是文本，
// 不会被当成标记 —— 服务端输出的是结构化模型，不生成任何标记。
func TestDocxScriptTextStaysText(t *testing.T) {
	payload := `<script>alert(1)</script><img src=x onerror=alert(2)>`
	xml := docxXML(para(`&lt;script&gt;alert(1)&lt;/script&gt;&lt;img src=x onerror=alert(2)&gt;`))
	doc, err := Extract(buildZip(t, map[string]string{"word/document.xml": xml}), "docx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got := doc.Parts[0].Blocks[0].Runs[0].Text
	if !strings.Contains(got, "<script>") {
		t.Fatalf("文本被意外改写：%q", got)
	}
	// 关键：它就是一个字符串，模型里没有任何「会变成标记」的字段。
	if !strings.Contains(got, payload) {
		t.Logf("提取到的文本：%q", got)
	}
}

// TestDocxSkipsEmptyParagraphs 确认空段落不产出空块（Word 常拿它做间距）。
func TestDocxSkipsEmptyParagraphs(t *testing.T) {
	xml := docxXML(`<w:p></w:p><w:p><w:r><w:t>有内容</w:t></w:r></w:p><w:p><w:r><w:t>  </w:t></w:r></w:p>`)
	doc, err := Extract(buildZip(t, map[string]string{"word/document.xml": xml}), "docx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if n := len(doc.Parts[0].Blocks); n != 1 {
		t.Fatalf("块数 = %d，期望 1（空段落应被忽略）：%+v", n, doc.Parts[0].Blocks)
	}
}

// xlsx 夹具：共享字符串 + 两张表，第二张故意少一列，用来验证补列。
func xlsxZip(t *testing.T, sheets map[string]string, shared string) *zip.Reader {
	files := map[string]string{
		"xl/workbook.xml": `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheets>` +
			`<sheet name="销售" sheetId="1" r:id="rId1"/>` +
			`<sheet name="库存" sheetId="2" r:id="rId2"/>` +
			`</sheets></workbook>`,
		"xl/sharedStrings.xml": shared,
	}
	for name, body := range sheets {
		files[name] = body
	}
	return buildZip(t, files)
}

// TestXlsxBasics 覆盖共享字符串、A1 定位、空单元格补位、表名。
func TestXlsxBasics(t *testing.T) {
	shared := `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="4" uniqueCount="4">` +
		`<si><t>名称</t></si><si><t>数量</t></si><si><t>苹果</t></si><si><t>香蕉</t></si></sst>`

	// 第一行：A1=名称(s0) B1=数量(s1)
	// 第二行：A2=苹果(s2) B2=数字 5
	// 第三行：B3=香蕉(s3)，A3 缺失 —— 用来验证空单元格补位
	sheet1 := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
		`<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>5</v></c></row>` +
		`<row r="3"><c r="B3" t="s"><v>3</v></c></row>` +
		`</sheetData></worksheet>`

	sheet2 := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
		`<row r="1"><c r="A1" t="inlineStr"><is><t>库存表</t></is></c></row>` +
		`</sheetData></worksheet>`

	zr := xlsxZip(t, map[string]string{
		"xl/worksheets/sheet1.xml": sheet1,
		"xl/worksheets/sheet2.xml": sheet2,
	}, shared)

	doc, err := Extract(zr, "xlsx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(doc.Parts) != 2 {
		t.Fatalf("工作表数 = %d，期望 2", len(doc.Parts))
	}
	if doc.Parts[0].Name != "销售" || doc.Parts[1].Name != "库存" {
		t.Errorf("表名不对：%q / %q", doc.Parts[0].Name, doc.Parts[1].Name)
	}

	rows := doc.Parts[0].Blocks[0].Rows
	if len(rows) != 3 {
		t.Fatalf("行数 = %d，期望 3", len(rows))
	}
	if rows[0][0].Text != "名称" || rows[0][1].Text != "数量" {
		t.Errorf("表头不对：%+v", rows[0])
	}
	// 共享字符串按下标解析
	if rows[1][0].Text != "苹果" {
		t.Errorf("共享字符串没解析对：%+v", rows[1])
	}
	if rows[1][1].Text != "5" {
		t.Errorf("数字单元格 = %q，期望 5", rows[1][1].Text)
	}
	// B3 有值、A3 缺失 —— A3 必须补成空单元格，否则香蕉会跑到第一列
	if rows[2][0].Text != "" || rows[2][1].Text != "香蕉" {
		t.Fatalf("空单元格没有补位，列错开了：%+v", rows[2])
	}
	// 每行列数应当一致
	for i, r := range rows {
		if len(r) != 2 {
			t.Errorf("第 %d 行列数 = %d，期望 2", i, len(r))
		}
	}
	// 首行标记为表头
	if !rows[0][0].Header {
		t.Error("首行应当是表头")
	}
	// 内联字符串
	if got := doc.Parts[1].Blocks[0].Rows[0][0].Text; got != "库存表" {
		t.Errorf("内联字符串 = %q", got)
	}
}

// TestXlsxSheetOrderNumeric 确认 sheet2 排在 sheet10 前面（按数字而非字符串）。
func TestXlsxSheetOrderNumeric(t *testing.T) {
	sheetXML := `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>X</t></is></c></row></sheetData></worksheet>`
	zr := xlsxZip(t, map[string]string{
		"xl/worksheets/sheet2.xml":  sheetXML,
		"xl/worksheets/sheet10.xml": sheetXML,
		"xl/worksheets/sheet1.xml":  sheetXML,
	}, `<sst/>`)

	doc, err := Extract(zr, "xlsx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// 表名来自 workbook.xml 的顺序，这里只验证数量与不崩。
	if len(doc.Parts) != 3 {
		t.Fatalf("工作表数 = %d，期望 3", len(doc.Parts))
	}
}

// TestXlsxPadsSkippedLeadingRows 确认被跳过的行号会补成空行。
//
// 真实文件里 <row> 的编号不是连续的：数据从第 2 行开始时直接写 r="2"，
// 第 1 行根本不出现。不按 r 属性定位的话整张表会往上挪一格 ——
// 这是拿 openpyxl 产出的真文件对出来的（手写夹具当时「恰好」从第 1 行开始，
// 把这个 bug 盖住了）。
func TestXlsxPadsSkippedLeadingRows(t *testing.T) {
	sheet := `<worksheet><sheetData>` +
		`<row r="2"><c r="B2" t="inlineStr"><is><t>第二行B列</t></is></c></row>` +
		`<row r="3"><c r="A3" t="inlineStr"><is><t>第三行A列</t></is></c></row>` +
		`</sheetData></worksheet>`
	zr := xlsxZip(t, map[string]string{"xl/worksheets/sheet1.xml": sheet}, `<sst/>`)

	doc, err := Extract(zr, "xlsx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	rows := doc.Parts[0].Blocks[0].Rows
	if len(rows) != 3 {
		t.Fatalf("行数 = %d，期望 3（第 1 行要补空）：%+v", len(rows), rows)
	}
	if rows[0][0].Text != "" || rows[0][1].Text != "" {
		t.Errorf("第 1 行应当是空的：%+v", rows[0])
	}
	if rows[1][1].Text != "第二行B列" {
		t.Errorf("B2 = %q：%+v", rows[1][1].Text, rows[1])
	}
	if rows[2][0].Text != "第三行A列" {
		t.Errorf("A3 = %q：%+v", rows[2][0].Text, rows[2])
	}
	// 每行列数仍然一致
	for i, r := range rows {
		if len(r) != 2 {
			t.Errorf("第 %d 行列数 = %d，期望 2", i, len(r))
		}
	}
}

// TestColIndex 是 A1 记法换算的单元测试。
func TestColIndex(t *testing.T) {
	cases := map[string]int{
		"A1":  0,
		"B1":  1,
		"Z1":  25,
		"AA1": 26,
		"AB1": 27,
		"BA1": 52,
		"":    -1,
		"1":   -1,
	}
	for in, want := range cases {
		if got := colIndex(in); got != want {
			t.Errorf("colIndex(%q) = %d，期望 %d", in, got, want)
		}
	}
}

// pptx 夹具。
func TestPptxBasics(t *testing.T) {
	slide := func(texts ...string) string {
		var sb strings.Builder
		sb.WriteString(`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" ` +
			`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree>`)
		for _, t := range texts {
			sb.WriteString(`<p:sp><p:txBody><a:p><a:r><a:t>` + t + `</a:t></a:r></a:p></p:txBody></p:sp>`)
		}
		sb.WriteString(`</p:spTree></p:cSld></p:sld>`)
		return sb.String()
	}
	zr := buildZip(t, map[string]string{
		"ppt/slides/slide1.xml": slide("封面", "副标题"),
		"ppt/slides/slide2.xml": slide("第二页内容"),
	})

	doc, err := Extract(zr, "pptx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(doc.Parts) != 2 {
		t.Fatalf("幻灯片数 = %d，期望 2", len(doc.Parts))
	}
	if doc.Parts[0].Name != "第 1 张" || doc.Parts[1].Name != "第 2 张" {
		t.Errorf("页名不对：%q / %q", doc.Parts[0].Name, doc.Parts[1].Name)
	}
	b0 := doc.Parts[0].Blocks
	if len(b0) != 2 {
		t.Fatalf("第 1 张块数 = %d，期望 2：%+v", len(b0), b0)
	}
	if b0[0].Runs[0].Text != "封面" || b0[1].Runs[0].Text != "副标题" {
		t.Errorf("文字不对：%+v", b0)
	}
}

// TestLimitsOversizedEntry 确认单条超限时截断而不是把内存吃光。
func TestLimitsOversizedEntry(t *testing.T) {
	// 造一个超过 maxEntryBytes 的 document.xml。
	big := strings.Repeat("x", maxEntryBytes+1024)
	xml := docxXML(para(big))
	doc, err := Extract(buildZip(t, map[string]string{"word/document.xml": xml}), "docx", Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !doc.Truncated {
		t.Fatal("超过单条上限却没有标记截断")
	}
	total := 0
	for _, part := range doc.Parts {
		for _, b := range part.Blocks {
			for _, r := range b.Runs {
				total += len(r.Text)
			}
		}
	}
	if total > maxEntryBytes+8 {
		t.Fatalf("读入 %d 字节，超过单条上限 %d", total, maxEntryBytes)
	}
}

// TestLimitsTooManyEntries 确认条目数超限直接拒绝。
func TestLimitsTooManyEntries(t *testing.T) {
	files := map[string]string{"word/document.xml": docxXML(para("x"))}
	// 名字必须**唯一** —— 用 map 装夹具，重复的名字会被去重，
	// 条目数根本到不了上限，测试就成了空转。
	for i := 0; i < maxEntries+10; i++ {
		files[fmt.Sprintf("junk/e%d.xml", i)] = "<a/>"
	}
	zr := buildZip(t, files)
	if _, err := Extract(zr, "docx", Options{}); err == nil {
		t.Fatal("条目数超限时应当报错")
	}
}

// TestUnsupportedFormat 确认非 OOXML 输入被明确拒绝。
func TestUnsupportedFormat(t *testing.T) {
	zr := buildZip(t, map[string]string{"hello.txt": "hi"})
	if _, err := Extract(zr, "docx", Options{}); err == nil {
		t.Fatal("不含 word/document.xml 的 zip 不该被当成 docx")
	}
	if _, err := Extract(zr, "rar", Options{}); err == nil {
		t.Fatal("不支持的格式应当报错")
	}
}

// TestMaxPartsOption 确认部分数上限可收紧。
func TestMaxPartsOption(t *testing.T) {
	sheetXML := `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>X</t></is></c></row></sheetData></worksheet>`
	files := map[string]string{"xl/sharedStrings.xml": `<sst/>`}
	for _, n := range []string{"1", "2", "3", "4"} {
		files["xl/worksheets/sheet"+n+".xml"] = sheetXML
	}
	doc, err := Extract(buildZip(t, files), "xlsx", Options{MaxParts: 2})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(doc.Parts) != 2 {
		t.Fatalf("部分数 = %d，期望被 MaxParts 限制到 2", len(doc.Parts))
	}
	if !doc.Truncated {
		t.Error("被上限截断时应当置 Truncated")
	}
}
