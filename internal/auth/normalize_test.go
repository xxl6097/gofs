package auth

import "testing"

// TestLookupFoldsDotSegments 确认 ACL 匹配前会折叠 . 与 ..。
//
// 这是纵深防御：HTTP 层已经在 entry 处把路径规范化了，但 auth 自己也不能
// 把 `/docs/../secret` 当成 `/docs` 的子路径 —— 否则任何一处漏掉规范化
// 的调用点都会重新引入越权。
func TestLookupFoldsDotSegments(t *testing.T) {
	a, err := Parse([]string{"alice:pw@/docs:rw"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want Permission
	}{
		{"/docs/a.txt", PermReadWrite},
		{"/docs/./a.txt", PermReadWrite},
		{"/docs//a.txt", PermReadWrite},
		{"/docs/../secret.txt", PermNone},
		{"/docs/../../etc/passwd", PermNone},
		{"/docs2/a.txt", PermNone},
		// 反斜杠在 Unix 上是文件名字符，不能当分隔符：否则鉴权会
		// 把 /docs\secret 当成 /docs 的子路径放行，而 Resolve 打开的是
		// 根目录下另一个名字带反斜杠的文件。
		{`/docs\secret.txt`, PermNone},
	}
	for _, c := range cases {
		if got := a.Lookup(c.path, "alice", "pw", true); got != c.want {
			t.Errorf("Lookup(%q) = %v，期望 %v", c.path, got, c.want)
		}
	}
}
