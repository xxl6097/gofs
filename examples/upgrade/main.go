// Command upgrade-demo 演示怎么给 gofs 实现「在线检测升级」。
//
// gofs 只定义 Upgrader 接口，不关心更新从哪来、怎么装。这个示例对接
// **GitHub Releases**，把一条完整的链路走通：
//
//	检查   调 /repos/<owner>/<repo>/releases/latest，拿最新 tag 与当前版本比
//	升级   下载对应 GOOS/GOARCH 的资产
//	       → 用同一 release 里的 checksums.txt 校验 sha256
//	       → 原子替换当前可执行文件（失败则回滚）
//
// 用法：
//
//	go build -o /tmp/gofs-demo ./examples/upgrade
//	/tmp/gofs-demo -owner xxl6097 -repo gofs -root ./data -port 8080
//
//	# 只检查、不替换（推荐先这样跑一遍）
//	/tmp/gofs-demo ... -dry-run
//
//	# 私有仓库需要 token
//	GITHUB_TOKEN=ghp_xxx /tmp/gofs-demo ...
//
// 注意：**不要用 go run 跑它**。go run 会把程序编译到临时目录，
// os.Executable() 拿到的是那个临时文件，替换它没有任何意义。
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/xxl6097/gofs"
)

func main() {
	owner := flag.String("owner", "xxl6097", "GitHub 账号")
	repo := flag.String("repo", "gofs", "仓库名")
	root := flag.String("root", ".", "要服务的目录")
	port := flag.Int("port", 8000, "监听端口")
	token := flag.String("token", os.Getenv("GITHUB_TOKEN"),
		"GitHub token（私有仓库需要，也可用 GITHUB_TOKEN 环境变量）")
	dryRun := flag.Bool("dry-run", false, "只检查并下载校验，不替换自己")
	flag.Parse()

	up := &githubUpgrader{
		owner:  *owner,
		repo:   *repo,
		token:  *token,
		dryRun: *dryRun,
		client: &http.Client{Timeout: 10 * time.Minute},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := gofs.New(
		gofs.WithRoot(*root),
		gofs.WithBind("127.0.0.1"),
		gofs.WithPort(*port),
		gofs.WithAllowAll(),
		gofs.WithAuth("admin:admin123@/:rw"),
		// 密钥与用户表放临时目录，别碰开发者本机的真实配置。
		gofs.WithKeyFile(""),
		gofs.WithUserFile(""),
		// ★ 关键的一行：注入之后页面上才会出现「版本与升级」。
		gofs.WithUpgrader(up),
	)
	if err != nil {
		log.Fatalf("构造服务失败: %v", err)
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	srv.Banner(os.Stdout)
	fmt.Printf("\n打开 %s ，用 admin / admin123 登录，\n", srv.URL())
	fmt.Printf("在「设置 → 版本与升级」里点「检查更新」。%s\n\n", dryRunNote(*dryRun))

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}

func dryRunNote(dry bool) string {
	if dry {
		return "（当前是 -dry-run：会下载并校验，但不会替换自己）"
	}
	return "（升级会替换本程序文件，完成后需要重启进程生效）"
}

// ---------------------------------------------------------------------------
// Upgrader 实现
// ---------------------------------------------------------------------------

// githubUpgrader 是 gofs.Upgrader 的一个 GitHub Releases 实现。
type githubUpgrader struct {
	owner, repo, token string
	dryRun             bool
	client             *http.Client
}

// apiBase 抽出来是为了能在测试里指向本地服务。
var apiBase = "https://api.github.com"

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type ghRelease struct {
	Tag         string    `json:"tag_name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

// Check 查最新 release，与当前版本比较。
//
// 没有更新时返回 (nil, nil) —— gofs 会把「已是最新」显示给管理员。
func (g *githubUpgrader) Check(ctx context.Context) (*gofs.Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", apiBase, g.owner, g.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}

	res, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub 失败: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		// 仓库没有 release，或者名字写错了。两种情况都值得说清楚。
		return nil, fmt.Errorf("仓库 %s/%s 没有已发布的 release（或名称有误）", g.owner, g.repo)
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return nil, fmt.Errorf("GitHub 返回 %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 release 失败: %w", err)
	}

	latest := strings.TrimPrefix(rel.Tag, "v")
	if !versionLess(gofs.Version, latest) {
		return nil, nil // 已是最新
	}

	// 挑出与本平台匹配的资产，把体积一并报给界面。
	if a, ok := g.pickAsset(rel); ok {
		return &gofs.Release{
			Version:     latest,
			Notes:       strings.TrimSpace(rel.Body),
			PublishedAt: rel.PublishedAt.Format("2006-01-02"),
			PageURL:     rel.HTMLURL,
			Size:        a.Size,
		}, nil
	}
	return nil, fmt.Errorf("版本 %s 里没有 %s/%s 的资产", rel.Tag, runtime.GOOS, runtime.GOARCH)
}

// Upgrade 下载并替换当前可执行文件。
//
// 顺序是刻意的：**先校验，再替换**。哪怕下载来源被投毒，
// 摘要对不上就在落地前被拦下，不会有一次「先装上再发现」。
func (g *githubUpgrader) Upgrade(ctx context.Context, rel *gofs.Release,
	onProgress func(gofs.UpgradeProgress)) error {

	report := func(p gofs.UpgradeProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	// 重新拉一次 release，拿资产地址与校验和 —— 不信任前端传来的任何 URL。
	ghRel, err := g.fetchRelease(ctx)
	if err != nil {
		return err
	}
	asset, ok := g.pickAsset(*ghRel)
	if !ok {
		return fmt.Errorf("版本 %s 里没有 %s/%s 的资产", ghRel.Tag, runtime.GOOS, runtime.GOARCH)
	}

	// 1) 下载到**目标二进制同目录**下的临时文件。
	//    同一文件系统才能用 rename 原子替换；放到 /tmp 再跨设备搬运
	//    会在最后一步退化成「复制+删除」，中间崩掉就只剩半个文件。
	target, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位当前可执行文件失败: %w", err)
	}
	target, _ = filepath.EvalSymlinks(target)
	tmp, err := os.CreateTemp(filepath.Dir(target), ".gofs-upgrade-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败（目录可能不可写）: %w", err)
	}
	tmpName := tmp.Name()
	// 任何提前返回都要把这个半成品清掉。
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	report(gofs.UpgradeProgress{
		Stage: gofs.StageDownloading, Total: asset.Size,
		Message: asset.Name,
	})
	sum, err := g.download(ctx, asset, tmp, func(n int64) {
		report(gofs.UpgradeProgress{
			Stage: gofs.StageDownloading, Loaded: n, Total: asset.Size, Message: asset.Name,
		})
	})
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// 2) 校验 sha256。
	//
	//    这一步不能省：下载地址来自网络，中间任何一环被替换，
	//    装上去的就是别人的代码。校验和取自同一 release 的
	//    checksums.txt —— 它跑在 HTTPS 后面，攻击者要同时改两个地方。
	report(gofs.UpgradeProgress{Stage: gofs.StageVerifying, Message: "校验 sha256"})
	want, err := g.fetchChecksum(ctx, *ghRel, asset.Name)
	if err != nil {
		return err
	}
	if !strings.EqualFold(want, sum) {
		return fmt.Errorf("摘要不匹配：期望 %s，实际 %s —— 已中止，未替换任何文件", want, sum)
	}

	if g.dryRun {
		report(gofs.UpgradeProgress{
			Stage:   gofs.StageDone,
			Message: "dry-run：已下载并校验通过，未替换（临时文件已清理）",
		})
		return nil
	}

	// 3) 原子替换。
	report(gofs.UpgradeProgress{Stage: gofs.StageReplacing, Message: "替换 " + filepath.Base(target)})
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("设置可执行位失败: %w", err)
	}
	// 先把旧文件挪成 .old（而不是直接覆盖）：rename 失败时还能拿回来。
	backup := target + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("备份原文件失败: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		// 回滚：把备份放回去，别把一个不可执行的路径留在那儿。
		if rbErr := os.Rename(backup, target); rbErr != nil {
			return fmt.Errorf("替换失败（%v），且回滚也失败（%v）—— 原文件在 %s", err, rbErr, backup)
		}
		return fmt.Errorf("替换失败，已回滚: %w", err)
	}
	committed = true

	report(gofs.UpgradeProgress{
		Stage:   gofs.StageDone,
		Message: "已替换为 " + rel.Version + "，重启进程后生效",
	})
	return nil
}

// fetchRelease 取最新 release 的完整信息。
func (g *githubUpgrader) fetchRelease(ctx context.Context) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", apiBase, g.owner, g.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	res, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub 失败: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 返回 %d", res.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 release 失败: %w", err)
	}
	return &rel, nil
}

// pickAsset 挑出与本平台匹配的资产。
//
// 命名约定是 `<name>-<goos>-<goarch>[.exe]`，与 Makefile 里的 cross 目标一致。
func (g *githubUpgrader) pickAsset(rel ghRelease) (ghAsset, bool) {
	suffix := fmt.Sprintf("-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		suffix += ".exe"
	}
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, suffix) {
			return a, true
		}
	}
	return ghAsset{}, false
}

// download 把资产写进 f，返回它的 sha256（十六进制）。
func (g *githubUpgrader) download(ctx context.Context, asset ghAsset, f *os.File,
	onBytes func(int64)) (string, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", err
	}
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	res, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 %s 返回 %d", asset.Name, res.StatusCode)
	}

	h := sha256.New()
	var n int64
	buf := make([]byte, 64<<10)
	for {
		read, rerr := res.Body.Read(buf)
		if read > 0 {
			if _, werr := f.Write(buf[:read]); werr != nil {
				return "", fmt.Errorf("写入临时文件失败: %w", werr)
			}
			h.Write(buf[:read])
			n += int64(read)
			if onBytes != nil {
				onBytes(n)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("读取响应失败: %w", rerr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchChecksum 从同一 release 的 checksums.txt 里取该文件的摘要。
//
// 找不到校验和文件时**直接报错**，而不是跳过校验 ——
// 「校验不了就放行」等于没有校验。
func (g *githubUpgrader) fetchChecksum(ctx context.Context, rel ghRelease, name string) (string, error) {
	var sumAsset *ghAsset
	for i := range rel.Assets {
		n := strings.ToLower(rel.Assets[i].Name)
		if n == "checksums.txt" || n == "sha256sum.txt" {
			sumAsset = &rel.Assets[i]
			break
		}
	}
	if sumAsset == nil {
		return "", errors.New("这个 release 没有 checksums.txt，拒绝在没有校验的情况下升级")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumAsset.URL, nil)
	if err != nil {
		return "", err
	}
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	res, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载校验和失败: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载校验和返回 %d", res.StatusCode)
	}

	// 格式是 `sha256sum` 的输出：<摘要>  <文件名>，每行一条。
	sc := bufio.NewScanner(io.LimitReader(res.Body, 1<<20))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		// sha256sum 会给二进制模式加 `*` 前缀。
		if strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("校验和文件里没有 %s", name)
}

// versionLess 判断 a 是否比 b 旧。
//
// 只按数字段比较（1.2.10 > 1.2.9），忽略 v 前缀与预发布后缀 ——
// 对一个演示足够了。真要用请换成完整的 semver 比较。
func versionLess(a, b string) bool {
	as := versionParts(a)
	bs := versionParts(b)
	for i := 0; i < 3; i++ {
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// 预发布后缀（-rc1）与元数据（+build）不参与比较。
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, seg := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n := 0
		for _, c := range seg {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}
