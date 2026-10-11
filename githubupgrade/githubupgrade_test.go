package githubupgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xxl6097/gofs"
)

// TestVersionLess 覆盖版本比较。
//
// 这里最容易出错的是**字符串比较**：按字典序 "1.10.0" < "1.9.0"，
// 于是新版本反而被判成旧的，用户永远等不到升级提示。
func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.2.0", "0.1.0", false},
		{"0.1.0", "0.1.0", false},
		{"1.9.0", "1.10.0", true},  // 字典序会判错的那条
		{"1.10.0", "1.9.0", false}, // 反过来
		{"v0.1.0", "0.2.0", true},  // 带 v 前缀
		{"0.1.0", "v0.2.0", true},
		{"0.1.0", "0.2.0-rc1", true}, // 预发布后缀不参与比较
		{"0.1.0", "0.1.1", true},
		{"1.0.0", "2.0.0", true},
	}
	for _, tc := range cases {
		if got := VersionLess(tc.a, tc.b); got != tc.want {
			t.Errorf("VersionLess(%q, %q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestPickAsset 确认按 GOOS/GOARCH 挑资产，而不是拿到第一个就下。
func TestPickAsset(t *testing.T) {
	g := &Upgrader{assetName: defaultAssetName("gofs")}
	rel := ghRelease{Assets: []ghAsset{
		{Name: "gofs-linux-amd64"},
		{Name: "gofs-darwin-arm64"},
		{Name: "checksums.txt"},
	}}
	got, ok := g.pickAsset(rel)
	if !ok {
		t.Fatal("没挑出资产")
	}
	want := fmt.Sprintf("gofs-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got.Name != want {
		t.Errorf("挑到了 %q，期望 %q", got.Name, want)
	}

	// 没有匹配的平台时不能瞎挑一个。
	if _, ok := g.pickAsset(ghRelease{Assets: []ghAsset{{Name: "gofs-plan9-mips"}}}); ok {
		t.Error("没有匹配平台时不该挑出资产")
	}
}

// fakeGitHub 起一个假的 GitHub API + 下载服务，返回 (upgrader, 关掉它的函数)。
//
// binary 是「新版本二进制」的内容；checksum 是写进 checksums.txt 的摘要，
// 测试可以故意写错来验证「摘要不匹配就中止」。
func fakeGitHub(t *testing.T, version string, binary []byte, checksum string) (*Upgrader, func()) {
	t.Helper()

	assetName := fmt.Sprintf("gofs-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		assetName += ".exe"
	}

	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		rel := map[string]any{
			"tag_name":     "v" + version,
			"body":         "修了几个 bug",
			"html_url":     "https://example.com/rel",
			"published_at": time.Now().Format(time.RFC3339),
			"assets": []map[string]any{
				{"name": assetName, "browser_download_url": srv.URL + "/dl/bin", "size": len(binary)},
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/dl/sums", "size": len(checksum)},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/dl/bin", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binary)
	})
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", checksum, assetName)
	})

	srv = httptest.NewServer(mux)
	// 让被测代码走假服务。
	g := &Upgrader{owner: "o", repo: "r", client: srv.Client(), apiBase: srv.URL,
		assetName: defaultAssetName("gofs"), checksumsName: DefaultChecksumsName}
	return g, func() {
		srv.Close()
	}
}

// TestCheckFindsNewVersion 覆盖「检查到新版本」。
func TestCheckFindsNewVersion(t *testing.T) {
	g, done := fakeGitHub(t, "99.0.0", []byte("bin"), "deadbeef")
	defer done()

	rel, err := g.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rel == nil {
		t.Fatal("应当报出新版本")
	}
	if rel.Version != "99.0.0" {
		t.Errorf("版本 = %q", rel.Version)
	}
	if rel.Notes != "修了几个 bug" || rel.PageURL == "" {
		t.Errorf("发布说明或发布页丢了：%+v", rel)
	}
	if rel.Size != 3 {
		t.Errorf("体积 = %d，期望 3（界面上要显示）", rel.Size)
	}
}

// TestCheckSaysUpToDate 确认远端版本不比当前新时返回 nil，
// 而不是把同一个版本当成「有更新」。
func TestCheckSaysUpToDate(t *testing.T) {
	g, done := fakeGitHub(t, gofs.Version, []byte("bin"), "x")
	defer done()

	rel, err := g.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rel != nil {
		t.Fatalf("同版本不该报更新：%+v", rel)
	}
}

// TestUpgradeVerifiesChecksum 是这份示例最该被钉住的一条：
// 摘要对不上就必须中止，且**一个文件都不能被改**。
func TestUpgradeVerifiesChecksum(t *testing.T) {
	binary := []byte("我是新版本")
	g, done := fakeGitHub(t, "99.0.0", binary, "0000000000000000000000000000000000000000000000000000000000000000")
	defer done()

	err := g.Upgrade(context.Background(), &gofs.Release{Version: "99.0.0"}, nil)
	if err == nil {
		t.Fatal("摘要不匹配却升级成功了")
	}
	if !strings.Contains(err.Error(), "摘要不匹配") {
		t.Errorf("错误信息没说清原因：%v", err)
	}
	// 原文件必须原封不动 —— 这是「先校验再替换」的意义。
	if _, statErr := os.Stat(execSelf(t) + ".old"); statErr == nil {
		t.Error("校验失败却留下了备份文件，说明已经动过原文件")
	}
}

// TestUpgradeDryRunStopsBeforeReplacing 确认 -dry-run 下载并校验通过之后
// 就停下，不碰自己 —— 让人能安全地试一遍。
func TestUpgradeDryRunStopsBeforeReplacing(t *testing.T) {
	binary := []byte("我是新版本")
	sum := sha256.Sum256(binary)
	g, done := fakeGitHub(t, "99.0.0", binary, hex.EncodeToString(sum[:]))
	defer done()
	g.dryRun = true

	var stages []string
	err := g.Upgrade(context.Background(), &gofs.Release{Version: "99.0.0"},
		func(p gofs.UpgradeProgress) { stages = append(stages, p.Stage) })
	if err != nil {
		t.Fatalf("dry-run 不该失败: %v", err)
	}
	if _, statErr := os.Stat(execSelf(t) + ".old"); statErr == nil {
		t.Fatal("dry-run 却动了原文件")
	}
	// 应当走完下载与校验，然后在替换前停下。
	joined := strings.Join(stages, ",")
	for _, want := range []string{gofs.StageDownloading, gofs.StageVerifying, gofs.StageDone} {
		if !strings.Contains(joined, want) {
			t.Errorf("阶段里缺少 %q：%v", want, stages)
		}
	}
	if strings.Contains(joined, gofs.StageReplacing) {
		t.Errorf("dry-run 不该走到替换阶段：%v", stages)
	}
}

// TestUpgradeRefusesWithoutChecksums 确认 release 里没有校验和文件时**拒绝升级**。
//
// 「校验不了就放行」等于没有校验 —— 这条路径必须走不通。
func TestUpgradeRefusesWithoutChecksums(t *testing.T) {
	g := &Upgrader{client: http.DefaultClient, assetName: defaultAssetName("gofs"),
		checksumsName: DefaultChecksumsName}
	_, err := g.fetchChecksum(context.Background(), ghRelease{
		Assets: []ghAsset{{Name: "gofs-linux-amd64"}},
	}, "gofs-linux-amd64")
	if err == nil {
		t.Fatal("没有 checksums.txt 却拿到了摘要")
	}
	if !strings.Contains(err.Error(), "拒绝在没有校验的情况下升级") {
		t.Errorf("错误信息没说清原因：%v", err)
	}
}

// TestFetchChecksumParsesSha256SumOutput 覆盖 sha256sum 的两种输出格式。
func TestFetchChecksumParsesSha256SumOutput(t *testing.T) {
	const want = "abc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  other-file\n", "ffff")
		fmt.Fprintf(w, "%s *gofs-linux-amd64\n", want) // 二进制模式的 *
		fmt.Fprint(w, "\n")                            // 空行
	}))
	defer srv.Close()

	g := &Upgrader{client: srv.Client(), assetName: defaultAssetName("gofs"),
		checksumsName: DefaultChecksumsName}
	got, err := g.fetchChecksum(context.Background(), ghRelease{
		Assets: []ghAsset{{Name: "checksums.txt", URL: srv.URL}},
	}, "gofs-linux-amd64")
	if err != nil {
		t.Fatalf("fetchChecksum: %v", err)
	}
	if got != want {
		t.Errorf("摘要 = %q，期望 %q", got, want)
	}
}

// TestCheckReportsMissingRelease 确认仓库没有 release 时给的是人话。
func TestCheckReportsMissingRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	g := &Upgrader{owner: "o", repo: "r", client: srv.Client(), apiBase: srv.URL,
		assetName: defaultAssetName("gofs"), checksumsName: DefaultChecksumsName}
	_, err := g.Check(context.Background())
	if err == nil {
		t.Fatal("404 却当成成功")
	}
	if !strings.Contains(err.Error(), "没有已发布的 release") {
		t.Errorf("错误信息不明确：%v", err)
	}
}

// execSelf 返回当前测试二进制的路径。
func execSelf(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
