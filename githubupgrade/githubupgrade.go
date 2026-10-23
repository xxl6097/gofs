// Package githubupgrade 是 gofs.Upgrader 的一个 GitHub Releases 实现。
//
// gofs 本身不内置任何更新逻辑（更新源与校验策略该由部署方决定），
// 这个包把「对接 GitHub Releases」这件常见的事做成可复用的实现：
//
//	up, err := githubupgrade.New(githubupgrade.Options{
//		Owner: "xxl6097",
//		Repo:  "gofs",
//	})
//	if err != nil {
//		return err
//	}
//	srv, err := gofs.New(gofs.WithRoot("./data"), gofs.WithUpgrader(up))
//
// 它做的事：
//
//	检查   调 /repos/<owner>/<repo>/releases/latest，与 CurrentVersion 比较
//	升级   下载对应 GOOS/GOARCH 的资产
//	       → 用同一 release 的 checksums 校验 sha256
//	       → 原子替换当前可执行文件（失败则回滚）
//
// # 安全上的几条硬约束
//
// 这些不是可选项，写死在实现里：
//
//   - **先校验再替换**。摘要对不上就在落地前中止，一个文件都不动。
//   - **没有校验和文件就拒绝升级**。「校验不了就放行」等于没有校验。
//   - **资产地址在升级时重新拉取**，不信任任何外部传入的 URL。
//   - **临时文件建在目标二进制的同目录下**，这样 rename 是原子的；
//     放 /tmp 再跨设备搬运会在最后一步退化成「复制+删除」。
//   - **替换前把原文件改成 .old**，rename 失败还能放回去。
//
// # 已知限制
//
//   - 只处理**裸二进制**资产（如 gofs-linux-amd64），不支持 .tar.gz / .zip。
//     这个项目的 Makefile 产出的就是裸二进制；需要压缩包的话得自己解一层。
//   - 升级完成后**不会自动重启**：进程替换了自己但仍在跑旧的代码。
//     重启交给部署方式（systemd / docker / supervisor）或调用方。
package githubupgrade

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xxl6097/gofs"
)

// DefaultAPIBase 是 github.com 的 API 地址。
// GitHub Enterprise 部署需要把它换成 https://<host>/api/v3。
const DefaultAPIBase = "https://api.github.com"

// DefaultChecksumsName 是默认的校验和文件名，也认 sha256sum.txt。
const DefaultChecksumsName = "checksums.txt"

// Options 用来构造 Upgrader。Owner 与 Repo 必填，其余有默认值。
type Options struct {
	// Owner / Repo 为仓库坐标，例如 Owner="xxl6097", Repo="gofs"。
	Owner string
	Repo  string

	// CurrentVersion 为当前运行的版本号，用来判断「有没有新版」。
	//
	// 留空则用 gofs.Version（默认 0.1.0）。服务的版本由上层指定
	// （gofs.WithVersion / Config.Version），这里应传入同一个值，
	// 否则会出现「明明是最新却说有更新」或反之。
	CurrentVersion string

	// APIBase 为 API 根地址，留空用 DefaultAPIBase。
	// GitHub Enterprise 填 https://<host>/api/v3。
	APIBase string

	// Client 为 HTTP 客户端，留空用一个 10 分钟超时的。
	//
	// 超时给得宽是因为要下载几十 MB 的二进制，
	// 但**不要**设成 0（不超时）——那会让一次挂死的下载永远占着 goroutine。
	Client *http.Client

	// AssetName 按 GOOS/GOARCH 拼出资产名。
	//
	// 留空则用 "<AssetPrefix>-<goos>-<goarch>[.exe]"，
	// 也就是这个项目 Makefile 里的命名。仓库用别的约定时换掉它：
	//
	//	AssetName: func(goos, goarch string) string {
	//		return fmt.Sprintf("gofs_%s_%s", goos, goarch)
	//	},
	AssetName func(goos, goarch string) string

	// AssetPrefix 为默认命名里的前缀，留空用 "gofs"。给了 AssetName 就忽略它。
	AssetPrefix string

	// ChecksumsName 为校验和文件名，留空用 DefaultChecksumsName。
	ChecksumsName string

	// DryRun 为 true 时只下载并校验，不替换自己。
	// 第一次接入、或在生产上试水时先开它。
	DryRun bool
}

// Upgrader 实现 gofs.Upgrader。
type Upgrader struct {
	owner, repo    string
	currentVersion string
	apiBase        string
	assetName      func(goos, goarch string) string
	checksumsName  string
	client         *http.Client
	dryRun         bool
}

// defaultAssetName 返回 <prefix>-<goos>-<goarch>[.exe] 的拼装函数。
// isLoopbackHost 判断主机名是否是本机回环（允许本地 HTTP 更新镜像）。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func defaultAssetName(prefix string) func(string, string) string {
	return func(goos, goarch string) string {
		n := prefix + "-" + goos + "-" + goarch
		if goos == "windows" {
			n += ".exe"
		}
		return n
	}
}

// New 校验参数并构造 Upgrader。
func New(opts Options) (*Upgrader, error) {
	if strings.TrimSpace(opts.Owner) == "" || strings.TrimSpace(opts.Repo) == "" {
		return nil, errors.New("githubupgrade: 必须指定 Owner 与 Repo")
	}

	name := opts.AssetName
	if name == nil {
		prefix := opts.AssetPrefix
		if prefix == "" {
			prefix = "gofs"
		}
		name = defaultAssetName(prefix)
	}

	base := strings.TrimSuffix(opts.APIBase, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	// 升级会下载并替换正在运行的二进制，API 地址必须是 HTTPS（本机回环
	// 镜像除外）。否则中间人可以同时替换资产和校验和，SHA-256 校验形同虚设。
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("githubupgrade: APIBase %q 不是合法 URL", base)
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("githubupgrade: APIBase 必须使用 HTTPS（仅本机回环允许 HTTP）")
	}
	sums := opts.ChecksumsName
	if sums == "" {
		sums = DefaultChecksumsName
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	current := strings.TrimSpace(opts.CurrentVersion)
	if current == "" {
		current = gofs.Version
	}

	return &Upgrader{
		owner:          strings.TrimSpace(opts.Owner),
		repo:           strings.TrimSpace(opts.Repo),
		currentVersion: current,
		apiBase:        base,
		assetName:      name,
		checksumsName:  sums,
		client:         client,
		dryRun:         opts.DryRun,
	}, nil
}

// ---------------------------------------------------------------------------
// GitHub API 的响应
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// gofs.Upgrader
// ---------------------------------------------------------------------------

// Check 查最新 release 并与当前版本比较；没有更新时返回 (nil, nil)。
func (u *Upgrader) Check(ctx context.Context) (*gofs.Release, error) {
	rel, err := u.fetchRelease(ctx)
	if err != nil {
		return nil, err
	}

	latest := strings.TrimPrefix(rel.Tag, "v")
	if !VersionLess(u.currentVersion, latest) {
		return nil, nil // 已是最新
	}

	// 顺手确认这个版本确实有本平台的资产。没有就当场说清楚，
	// 而不是让管理员点了「立即升级」才发现下不了。
	asset, ok := u.pickAsset(*rel)
	if !ok {
		return nil, fmt.Errorf("版本 %s 里没有 %s/%s 的资产", rel.Tag, runtime.GOOS, runtime.GOARCH)
	}

	return &gofs.Release{
		Version:     latest,
		Notes:       strings.TrimSpace(rel.Body),
		PublishedAt: rel.PublishedAt.Format("2006-01-02"),
		PageURL:     rel.HTMLURL,
		Size:        asset.Size,
	}, nil
}

// Upgrade 下载并替换当前可执行文件。
func (u *Upgrader) Upgrade(ctx context.Context, rel *gofs.Release,
	onProgress func(gofs.UpgradeProgress)) error {

	report := func(p gofs.UpgradeProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	// 重新拉一次 release：资产地址以这一次为准，
	// 不信任前端传上来的任何 URL（那部分是调用方可控的）。
	ghRel, err := u.fetchRelease(ctx)
	if err != nil {
		return err
	}
	asset, ok := u.pickAsset(*ghRel)
	if !ok {
		return fmt.Errorf("版本 %s 里没有 %s/%s 的资产", ghRel.Tag, runtime.GOOS, runtime.GOARCH)
	}

	target, err := executablePath()
	if err != nil {
		return err
	}

	// 临时文件建在目标**同目录**下：同一文件系统才能用 rename 原子替换。
	tmp, err := os.CreateTemp(filepath.Dir(target), ".gofs-upgrade-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败（目录可能不可写）: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	report(gofs.UpgradeProgress{
		Stage: gofs.StageDownloading, Total: asset.Size, Message: asset.Name,
	})
	sum, err := u.download(ctx, asset, tmp, func(n int64) {
		report(gofs.UpgradeProgress{
			Stage: gofs.StageDownloading, Loaded: n, Total: asset.Size, Message: asset.Name,
		})
	})
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写出临时文件失败: %w", err)
	}

	// 先校验，再替换。
	report(gofs.UpgradeProgress{Stage: gofs.StageVerifying, Message: "校验 sha256"})
	want, err := u.fetchChecksum(ctx, *ghRel, asset.Name)
	if err != nil {
		return err
	}
	if !strings.EqualFold(want, sum) {
		return fmt.Errorf("摘要不匹配：期望 %s，实际 %s —— 已中止，未替换任何文件", want, sum)
	}

	if u.dryRun {
		report(gofs.UpgradeProgress{
			Stage:   gofs.StageDone,
			Message: "dry-run：已下载并校验通过，未替换（临时文件已清理）",
		})
		return nil
	}

	report(gofs.UpgradeProgress{Stage: gofs.StageReplacing, Message: "替换 " + filepath.Base(target)})
	if err := replaceFile(tmpName, target); err != nil {
		return err
	}
	committed = true

	report(gofs.UpgradeProgress{
		Stage:   gofs.StageDone,
		Message: "已替换为 " + rel.Version + "，重启进程后生效",
	})
	return nil
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// executablePath 返回当前可执行文件的真实路径（解析软链）。
//
// 解析软链是必要的：/usr/local/bin/gofs 常常是指向 releases/0.1.0/gofs 的软链，
// 替换软链本身只会把链接换掉，实际跑的仍是旧文件。
func executablePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位当前可执行文件失败: %w", err)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p, nil
}

// replaceFile 把 src 原子地换到 dst，失败则回滚。
//
// 先把 dst 改成 .old 而不是直接覆盖：rename 失败时还能拿回来，
// 不会留下一个不可执行的目标路径。
func replaceFile(src, dst string) error {
	if err := os.Chmod(src, 0o755); err != nil {
		return fmt.Errorf("设置可执行位失败: %w", err)
	}
	backup := dst + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(dst, backup); err != nil {
		return fmt.Errorf("备份原文件失败: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		if rbErr := os.Rename(backup, dst); rbErr != nil {
			return fmt.Errorf("替换失败（%v），且回滚也失败（%v）—— 原文件在 %s", err, rbErr, backup)
		}
		return fmt.Errorf("替换失败，已回滚: %w", err)
	}
	return nil
}

// fetchRelease 取最新 release。
func (u *Upgrader) fetchRelease(ctx context.Context) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", u.apiBase, u.owner, u.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub 失败: %w", err)
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// 没有 release 与仓库名写错都给 404，一并说清两种可能。
		return nil, fmt.Errorf("仓库 %s/%s 没有已发布的 release（或名称有误）", u.owner, u.repo)
	case http.StatusUnauthorized, http.StatusForbidden:
		// 公开仓库不带任何凭据，走到这里基本只剩「匿名请求触发速率限制」一种解释。
		// 这是开源项目的公共接口，配额按 IP 算，等一会儿再试即可。
		return nil, fmt.Errorf("GitHub 返回 %d：匿名请求可能触发了速率限制，稍后再试", res.StatusCode)
	default:
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return nil, fmt.Errorf("GitHub 返回 %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 release 失败: %w", err)
	}
	return &rel, nil
}

// pickAsset 挑出本平台的资产。
func (u *Upgrader) pickAsset(rel ghRelease) (ghAsset, bool) {
	want := u.assetName(runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		if a.Name == want {
			return a, true
		}
	}
	return ghAsset{}, false
}

// download 把资产写进 f，返回它的 sha256（十六进制）。
//
// 边写边算摘要，并通过 onBytes 上报累计字节数（用于升级进度）。
func (u *Upgrader) download(ctx context.Context, asset ghAsset, f *os.File,
	onBytes func(int64)) (string, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", err
	}
	res, err := u.client.Do(req)
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

// fetchChecksum 从 release 的校验和文件里取该资产的摘要。
//
// 找不到校验和文件时**直接报错**，而不是跳过校验 ——
// 「校验不了就放行」等于没有校验。
func (u *Upgrader) fetchChecksum(ctx context.Context, rel ghRelease, name string) (string, error) {
	var sumAsset *ghAsset
	for i := range rel.Assets {
		n := strings.ToLower(rel.Assets[i].Name)
		if n == strings.ToLower(u.checksumsName) || n == "sha256sum.txt" {
			sumAsset = &rel.Assets[i]
			break
		}
	}
	if sumAsset == nil {
		return "", fmt.Errorf("这个 release 没有 %s，拒绝在没有校验的情况下升级", u.checksumsName)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumAsset.URL, nil)
	if err != nil {
		return "", err
	}
	res, err := u.client.Do(req)
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

// VersionLess 判断 a 是否比 b 旧。
//
// 只按数字段比较（1.2.10 > 1.2.9），忽略 v 前缀与预发布后缀。
// 够这个包用；需要完整的 semver 语义请自行替换。
func VersionLess(a, b string) bool {
	as, bs := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// 预发布（-rc1）与构建元数据（+build）不参与比较。
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
