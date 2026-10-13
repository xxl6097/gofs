package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// settingsSnapshot 读回落盘文件，看每一项在不在。
func settingsSnapshot(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("设置文件坏了: %v\n%s", err, raw)
	}
	return doc
}

// TestSettingsConcurrentSaveKeepsAllFields 探「改内存 + 落盘」这一步的原子性。
//
// saveSettings 的写法是「逐个读字段（各一次短 RLock）→ 拼文档 → 原子写」。
// 读与写之间没有覆盖整段的锁，于是两个并发的设置调用可能这样交错：
// A 改完标题、B 改完上传上限，而 A 拼文档时读到的是**旧的**上限、
// B 拼文档时读到的是**旧的**标题 —— 后写的那份把先写的那份盖掉。
// 内存里两个改动都在，磁盘上少一个：重启后静默回退，谁也不会发现。
//
// 这里直接用设置对象本身压（绕开 HTTP，把窗口相对放大），
// 每个 goroutine 改一个**不同**的字段，最后文件里必须八个都在。
func TestSettingsConcurrentSaveKeepsAllFields(t *testing.T) {
	sf := t.TempDir() + "/settings.json"
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw")
	cfg.SettingsFile = sf
	s := newTestServer(t, cfg)
	rs := s.settings

	const rounds = 400
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		setters := []func(){
			func() { _ = rs.SetTitle(fmt.Sprintf("T%d", i)) },
			func() { _ = rs.SetUploadMaxSize(int64(1000 + i)) },
			func() { _ = rs.SetEditMaxSize(int64(2000 + i)) },
			func() { _ = rs.SetPathPrefix(fmt.Sprintf("/p%d", i)) },
			func() { _ = rs.SetListen("127.0.0.1", 3000+i) },
			func() { _ = rs.SetSites([]siteRecord{{Path: "/tmp", Label: fmt.Sprintf("s%d", i)}}) },
		}
		for _, fn := range setters {
			wg.Add(1)
			go func(f func()) { defer wg.Done(); f() }(fn)
		}
		wg.Wait()
	}

	doc := settingsSnapshot(t, sf)
	// 内存里的权威值。
	if got, _ := doc["title"].(string); got != rs.Title() {
		t.Errorf("文件里的标题 %q 与内存 %q 不一致 —— 一次保存被另一次盖掉了", got, rs.Title())
	}
	if got, ok := doc["upload_max_size"].(float64); !ok || int64(got) != rs.UploadMaxSize() {
		t.Errorf("文件里的上传上限 %v 与内存 %d 不一致（有没有=%v）", doc["upload_max_size"], rs.UploadMaxSize(), ok)
	}
	if got, ok := doc["edit_max_size"].(float64); !ok || int64(got) != rs.EditMaxSize() {
		t.Errorf("文件里的编辑上限 %v 与内存 %d 不一致", doc["edit_max_size"], rs.EditMaxSize())
	}
	if got, _ := doc["path_prefix"].(string); got != rs.PathPrefix() {
		t.Errorf("文件里的前缀 %q 与内存 %q 不一致", got, rs.PathPrefix())
	}
	if got, _ := doc["listen_port"].(float64); int(got) != rs.ListenPort() {
		t.Errorf("文件里的端口 %v 与内存 %d 不一致", doc["listen_port"], rs.ListenPort())
	}
	if sites, _ := doc["sites"].([]any); len(sites) == 0 {
		t.Errorf("文件里没有站点 —— 被并发的另一次保存盖掉了")
	}
}

// TestSettingsConcurrentSaveKeepsAllFieldsHTTP 是从 HTTP 层压同一个不变量：
// 两个管理员同时改不同字段，最后一轮之后落盘文件必须与内存一致。
func TestSettingsConcurrentSaveKeepsAllFieldsHTTP(t *testing.T) {
	sf := t.TempDir() + "/settings.json"
	cfg := testConfig(t, t.TempDir(), "-A", "-a", "boss:pw@/:rw")
	cfg.SettingsFile = sf
	s := newTestServer(t, cfg)
	hdr := adminSettingsHdr()

	const rounds = 150
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		bodies := []string{
			`{"title":"` + strings.Repeat("t", i%7+1) + `"}`,
			`{"upload_max_size":` + fmt.Sprint(1000+i) + `}`,
			`{"edit_max_size":` + fmt.Sprint(2000+i) + `}`,
		}
		for _, b := range bodies {
			wg.Add(1)
			go func(b string) {
				defer wg.Done()
				do(t, s, http.MethodPut, "/fs/settings", strings.NewReader(b), hdr)
			}(b)
		}
		wg.Wait()
	}

	doc := settingsSnapshot(t, sf)
	if got, _ := doc["title"].(string); got != s.settings.Title() {
		t.Errorf("文件标题 %q ≠ 内存 %q", got, s.settings.Title())
	}
	if got, ok := doc["upload_max_size"].(float64); !ok || int64(got) != s.settings.UploadMaxSize() {
		t.Errorf("文件上传上限 %v ≠ 内存 %d", doc["upload_max_size"], s.settings.UploadMaxSize())
	}
	if got, ok := doc["edit_max_size"].(float64); !ok || int64(got) != s.settings.EditMaxSize() {
		t.Errorf("文件编辑上限 %v ≠ 内存 %d", doc["edit_max_size"], s.settings.EditMaxSize())
	}
}
