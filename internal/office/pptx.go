package office

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
)

// pptx 解析 ppt/slides/slideN.xml。
//
// 幻灯片里的文字都在 <a:p>（段落）里的 <a:r>/<a:t>。标题约占位符
// <p:ph type="title"> 里，这里不特别区分 —— 幻灯片本来就短，
// 全部按段落列出来已经够读。文字之外的内容（图形、图表、图片、
// 演讲者备注）都不呈现。
type pptxParser struct {
	rd *reader
}

// numName 从 slide12.xml 里取出 12，用于「第 N 张」的显示名。
var numName = regexp.MustCompile(`(\d+)`)

// pptx 提取全部幻灯片。
func (r *reader) pptx() (*Doc, error) {
	slides := r.entriesUnder("ppt/slides", func(base string) bool {
		return strings.HasPrefix(base, "slide") && strings.HasSuffix(base, ".xml")
	})
	if len(slides) == 0 {
		return nil, ErrNotOffice
	}

	p := &pptxParser{rd: r}
	var parts []Part
	for i, sp := range slides {
		if i >= r.maxParts {
			r.truncated = true
			break
		}
		data, err := r.readEntry(sp)
		if err != nil || len(data) == 0 {
			continue
		}
		name := fmt.Sprintf("第 %d 张", i+1)
		if m := numName.FindString(sp); m != "" {
			if n := atoiSafe(m); n > 0 {
				name = fmt.Sprintf("第 %d 张", n)
			}
		}
		parts = append(parts, Part{Name: name, Blocks: p.slide(data)})
	}
	if len(parts) == 0 {
		return nil, ErrNotOffice
	}
	return &Doc{Parts: parts}, nil
}

// slide 解析一张幻灯片。
func (p *pptxParser) slide(data []byte) []Block {
	dec := xml.NewDecoder(bytes.NewReader(data))

	var (
		blocks []Block
		runs   []Run
		sb     strings.Builder
		inT    bool
	)
	flush := func() {
		if len(runs) == 0 {
			return
		}
		if allBlank(runs) {
			runs = nil
			return
		}
		blocks = p.rd.appendBlock(blocks, Block{Type: "para", Runs: runs})
		runs = nil
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				runs = nil
			case "t":
				inT = true
				sb.Reset()
			case "br":
				sb.WriteByte('\n')
			}
		case xml.CharData:
			if inT {
				sb.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				if inT {
					if txt := clipRun(sb.String()); txt != "" {
						runs = append(runs, Run{Text: txt})
					}
					inT = false
				}
			case "p":
				flush()
			}
		}
	}
	flush()
	return blocks
}
