package office

import (
	"sort"
	"strconv"
	"strings"
)

// sortByNumericSuffix 按路径尾部的数字排序。
//
// xl/worksheets/sheet2.xml 必须排在 sheet10.xml 前面 —— 直接按字符串比
// 会得到 sheet1、sheet10、sheet2，表格顺序就乱了。
func sortByNumericSuffix(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		ni, si := splitNumericSuffix(names[i])
		nj, sj := splitNumericSuffix(names[j])
		if si != "" && sj != "" && ni != nj {
			return ni < nj
		}
		return names[i] < names[j]
	})
}

// splitNumericSuffix 把 ".../slide12.xml" 拆成 (12, ".xml")。
// 取不到数字时返回 (0, "")。
func splitNumericSuffix(name string) (int, string) {
	base := name
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	// 去掉扩展名
	ext := ""
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		ext, base = base[i:], base[:i]
	}
	// 从尾部取连续数字
	i := len(base)
	for i > 0 && base[i-1] >= '0' && base[i-1] <= '9' {
		i--
	}
	if i == len(base) {
		return 0, ""
	}
	n, err := strconv.Atoi(base[i:])
	if err != nil {
		return 0, ""
	}
	return n, ext
}
