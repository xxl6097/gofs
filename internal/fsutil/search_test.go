package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// searchFixture 造一棵固定的目录树，供各模式的匹配用例复用。
func searchFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"docs/readme.md",
		"docs/guide.md",
		"notes.log",
		"media/A.TXT",
		"media/照片.jpg",
	}
	for _, rel := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runSearch 执行一次搜索，返回命中条目的名字（已排序，便于比较）。
func runSearch(t *testing.T, mode SearchMode, pattern string) []string {
	t.Helper()
	root := searchFixture(t)
	res, err := NewResolver(root, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Search(res, root, SearchOptions{Pattern: pattern, Mode: mode})
	if err != nil {
		t.Fatalf("Search(%v, %q) 报错: %v", mode, pattern, err)
	}
	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSearchModes 覆盖四种模式在「名字命中 / 路径命中 / 不命中」上的语义。
func TestSearchModes(t *testing.T) {
	cases := []struct {
		name    string
		mode    SearchMode
		pattern string
		want    []string
	}{
		// 模糊：包含即命中，忽略大小写；名字或路径任一命中。
		{"模糊-名字子串", SearchFuzzy, "readme", []string{"readme.md"}},
		{"模糊-忽略大小写", SearchFuzzy, "READ", []string{"readme.md"}},
		{"模糊-路径命中", SearchFuzzy, "docs/readme", []string{"readme.md"}},
		{"模糊-扩展名", SearchFuzzy, "md", []string{"guide.md", "readme.md"}},

		// 精确：名称或相对路径完全相等，忽略大小写；子串不算命中。
		{"精确-全名", SearchExact, "readme.md", []string{"readme.md"}},
		{"精确-子串不算", SearchExact, "readme", nil},
		{"精确-路径全等", SearchExact, "docs/readme.md", []string{"readme.md"}},
		{"精确-忽略大小写", SearchExact, "NOTES.LOG", []string{"notes.log"}},
		{"精确-大小写不同的扩展名", SearchExact, "a.txt", []string{"A.TXT"}},

		// 通配符：* 不跨 /，故 *.log 靠名字命中、docs/*.md 靠路径命中。
		{"通配-按名字", SearchWildcard, "*.log", []string{"notes.log"}},
		{"通配-任意目录下的 md", SearchWildcard, "*.md", []string{"guide.md", "readme.md"}},
		{"通配-按路径", SearchWildcard, "docs/*.md", []string{"guide.md", "readme.md"}},
		{"通配-问号", SearchWildcard, "note?.log", []string{"notes.log"}},
		{"通配-大小写不敏感", SearchWildcard, "*.TXT", []string{"A.TXT"}},
		{"通配-不跨目录", SearchWildcard, "docs*", []string{"docs"}},

		// 正则：Go RE2，带 (?i) 故忽略大小写；同样名字或路径任一命中。
		{"正则-锚定名字", SearchRegex, `^readme`, []string{"readme.md"}},
		{"正则-忽略大小写", SearchRegex, `\.txt$`, []string{"A.TXT"}},
		{"正则-路径匹配", SearchRegex, `docs/.*\.md$`, []string{"guide.md", "readme.md"}},
		{"正则-或", SearchRegex, `^(notes|guide)`, []string{"guide.md", "notes.log"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runSearch(t, c.mode, c.pattern)
			if !eq(got, c.want) {
				t.Errorf("模式=%v 关键字=%q 命中=%v，期望 %v", c.mode, c.pattern, got, c.want)
			}
		})
	}
}

// TestSearchBadPattern 非法表达式要返回 ErrBadSearchPattern（而非 500）。
func TestSearchBadPattern(t *testing.T) {
	cases := []struct {
		name    string
		mode    SearchMode
		pattern string
	}{
		{"坏通配符", SearchWildcard, "["},
		{"坏正则-括号不闭合", SearchRegex, "("},
		{"坏正则-非法组", SearchRegex, "(?<bad"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := searchFixture(t)
			res, err := NewResolver(root, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Search(res, root, SearchOptions{Pattern: c.pattern, Mode: c.mode})
			if !errors.Is(err, ErrBadSearchPattern) {
				t.Fatalf("Search(%v, %q) 错误 = %v，期望 ErrBadSearchPattern", c.mode, c.pattern, err)
			}
		})
	}
}

// TestParseSearchMode 别名与未知值回落。
func TestParseSearchMode(t *testing.T) {
	cases := map[string]SearchMode{
		"":         SearchFuzzy,
		"bogus":    SearchFuzzy,
		"fuzzy":    SearchFuzzy,
		"exact":    SearchExact,
		"equal":    SearchExact,
		"EXACT":    SearchExact,
		"wildcard": SearchWildcard,
		"glob":     SearchWildcard,
		"regex":    SearchRegex,
		"regexp":   SearchRegex,
		" 正则 ":     SearchFuzzy, // 中文不是合法取值，回落
	}
	for in, want := range cases {
		if got := ParseSearchMode(in); got != want {
			t.Errorf("ParseSearchMode(%q) = %v, 期望 %v", in, got, want)
		}
	}
}

// TestSearchHiddenRootIsStillWalked 守住一条不变量：隐藏规则只作用于
// 搜索根**以下**的条目。搜索根自身的名字即使命中隐藏 glob，整棵树也要遍历。
// （这不是在修一个已存在的 bug —— 是防止将来有人把根判断挪到隐藏判断之后。）
func TestSearchHiddenRootIsStillWalked(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "node_modules")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "target.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := NewResolver(base, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Search(res, root, SearchOptions{
		Pattern: "target",
		Mode:    SearchFuzzy,
		Hidden:  []string{"node_modules"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if len(names) != 1 || names[0] != "target.txt" {
		t.Fatalf("命中 = %v，期望 [target.txt]（搜索根被隐藏规则误伤）", names)
	}
}

// TestSearchLimit 上限截断仍然生效。
func TestSearchLimit(t *testing.T) {
	root := searchFixture(t)
	res, err := NewResolver(root, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Search(res, root, SearchOptions{Pattern: "", Mode: SearchFuzzy, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Limit=2 时返回 %d 条，期望 2", len(got))
	}
}

// TestSearchTrimPattern 前后空格被忽略。
func TestSearchTrimPattern(t *testing.T) {
	got := runSearch(t, SearchFuzzy, "  readme  ")
	if !strings.EqualFold(strings.Join(got, ","), "readme.md") {
		t.Errorf("带空格的关键字命中 = %v，期望 [readme.md]", got)
	}
}
