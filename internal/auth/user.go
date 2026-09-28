package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// 密码摘要参数。
//
// 为什么用 PBKDF2 而不是直接 SHA-256：用户密码是**低熵**的，
// 一份没加盐的快摘要等于把明文交给离线爆破（字典几秒钟就能跑完）。
// PBKDF2 + 每用户随机盐 + 迭代轮数把单次尝试的成本抬起来，
// 同时让「同一个密码在两个人身上算出不同摘要」，彩虹表失效。
//
// 轮数取 OWASP 对 PBKDF2-HMAC-SHA256 的建议值。它只在**首次**校验某个
// 「用户名+密码」组合时消耗一次（约 100ms），之后走 pwCache 直接命中 ——
// 详见 passwordMatches 的注释。
const (
	pwIterations = 210_000
	pwKeyLen     = 32
	pwSaltLen    = 16
)

// passwordHash 是一个账号的密码摘要（盐 + 轮数 + 派生密钥）。
type passwordHash struct {
	Salt []byte
	Iter int
	Key  []byte
}

// hashPassword 生成新摘要。
func hashPassword(password string) (*passwordHash, error) {
	salt := make([]byte, pwSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("生成随机盐失败: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pwIterations, pwKeyLen)
	if err != nil {
		return nil, fmt.Errorf("计算密码摘要失败: %w", err)
	}
	return &passwordHash{Salt: salt, Iter: pwIterations, Key: key}, nil
}

// verify 定长时间比较，避免按耗时逐字节猜摘要。
func (h *passwordHash) verify(password string) bool {
	if h == nil || len(h.Key) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, h.Salt, h.Iter, len(h.Key))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, h.Key) == 1
}

// ------------------------------------------------------------------ 密码校验缓存

// pwCache 缓存「用户名+密码 → 是否匹配」的结果。
//
// 存在的理由：HTTP Basic 下**每个请求都要验一次密码**，而 PBKDF2 是按
// 「故意很慢」设计的（21 万轮 ≈ 100ms）。不缓存的话每个请求都要多花 100ms，
// 服务基本上不可用。缓存键是「用户名 + \x00 + 密码」的 SHA-256，
// 值是一个 bool，所以内存里不会留下明文密码。
//
// 任何一次用户表改动都会清空整个缓存（invalidate）—— 否则改了密码之后，
// 旧密码会因为在缓存里而被继续认作有效。
type pwCache struct {
	mu  sync.Mutex
	m   map[[32]byte]bool
	cap int
}

func newPWCache() *pwCache { return &pwCache{m: make(map[[32]byte]bool), cap: 512} }

func pwCacheKey(username, password string) [32]byte {
	h := sha256.New()
	h.Write([]byte(username))
	h.Write([]byte{0})
	h.Write([]byte(password))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (c *pwCache) get(k [32]byte) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *pwCache) put(k [32]byte, v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 满了就整体清空：缓存只是为了省计算，丢了不影响正确性。
	if len(c.m) >= c.cap {
		c.m = make(map[[32]byte]bool)
	}
	c.m[k] = v
}

func (c *pwCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = make(map[[32]byte]bool)
}

// ---------------------------------------------------------------------- 用户表

// Rule 是暴露给外部的路径规则（管理接口与界面用）。
type Rule struct {
	Path string
	Perm Permission
}

// UserInfo 是账号的对外视图。**不含任何密码材料**。
type UserInfo struct {
	Name        string
	Rules       []Rule
	FromStartup bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// 用户名限制。这些字符会破坏 -a 规则的语法或者让界面上的展示产生歧义。
const (
	maxUserNameLen     = 64
	minUserPasswordLen = 6
)

// ValidateUserName 校验用户名。返回的字符串是规范化后的名字。
func ValidateUserName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("用户名不能为空")
	}
	if len(name) > maxUserNameLen {
		return "", fmt.Errorf("用户名过长（最多 %d 字节）", maxUserNameLen)
	}
	// 空白字符一律拒绝：用户名会出现在日志、界面和 -a 规则里，
	// 混进空格既不好读，也容易被「看上去一样」的名字钓鱼。
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return "", fmt.Errorf("用户名不能包含空白字符")
	}
	if strings.ContainsAny(name, "@:,/\\\"'") {
		return "", fmt.Errorf("用户名不能包含 @ : , / \\ 或引号")
	}
	return name, nil
}

// ValidateRules 校验并归一化规则列表。
func ValidateRules(rules []Rule) ([]Rule, error) {
	if len(rules) == 0 {
		return nil, fmt.Errorf("至少要给一条路径权限")
	}
	out := make([]Rule, 0, len(rules))
	seen := map[string]bool{}
	for _, r := range rules {
		p := normalizePath(r.Path)
		if p == "" {
			return nil, fmt.Errorf("路径不能为空")
		}
		if strings.Contains(p, "..") {
			return nil, fmt.Errorf("路径 %q 不能包含 ..", r.Path)
		}
		if r.Perm != PermRead && r.Perm != PermReadWrite {
			return nil, fmt.Errorf("路径 %q 的权限只能是 rw 或 r", p)
		}
		if seen[p] {
			// 同一条路径给两个权限是歧义输入，直接拒掉，
			// 免得「最长前缀匹配」把用户意图猜错。
			return nil, fmt.Errorf("路径 %q 重复了", p)
		}
		seen[p] = true
		out = append(out, Rule{Path: p, Perm: r.Perm})
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Path) > len(out[j].Path) })
	return out, nil
}

// UserFile 返回用户表的落盘路径（空串表示仅内存）。
func (a *Authenticator) UserFile() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.userFile
}

// Users 返回全部账号（启动参数 + 用户表），按「启动参数在前、各自按名字排序」。
func (a *Authenticator) Users() []UserInfo {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	var out []UserInfo
	appendAcc := func(acc *account) {
		info := UserInfo{
			Name:        acc.Username,
			FromStartup: acc.fromStartup,
		}
		if acc.createdAt > 0 {
			info.CreatedAt = time.Unix(acc.createdAt, 0)
		}
		if acc.updatedAt > 0 {
			info.UpdatedAt = time.Unix(acc.updatedAt, 0)
		}
		for _, r := range acc.Rules {
			info.Rules = append(info.Rules, Rule{Path: r.Path, Perm: r.Perm})
		}
		// 长的路径排前面，与匹配顺序一致，界面上一眼能看出优先级。
		sort.SliceStable(info.Rules, func(i, j int) bool {
			return len(info.Rules[i].Path) > len(info.Rules[j].Path)
		})
		out = append(out, info)
	}

	for _, acc := range a.static {
		appendAcc(acc)
	}
	for _, acc := range a.users {
		appendAcc(acc)
	}
	sort.SliceStable(out[0:len(a.static)], func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.SliceStable(out[len(a.static):], func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AnonymousRules 返回匿名规则的路径列表（没有匿名规则时返回 nil）。
//
// 单独暴露而不是混进 Users()：匿名不是一个「账号」，界面上也是只读展示 ——
// 谁能匿名访问是启动参数决定的，运行期改不了。
func (a *Authenticator) AnonymousRules() []Rule {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.anonAcc == nil {
		return nil
	}
	out := make([]Rule, 0, len(a.anonAcc.Rules))
	for _, r := range a.anonAcc.Rules {
		out = append(out, Rule{Path: r.Path, Perm: r.Perm})
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Path) > len(out[j].Path) })
	return out
}

// AddUser 新建一个用户表账号。
func (a *Authenticator) AddUser(name, password string, rules []Rule) error {
	if a == nil {
		return fmt.Errorf("当前没有启用鉴权，无法创建用户")
	}
	name, err := ValidateUserName(name)
	if err != nil {
		return err
	}
	if len(password) < minUserPasswordLen {
		return fmt.Errorf("密码至少 %d 位", minUserPasswordLen)
	}
	rules, err = ValidateRules(rules)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.findStatic(name) != nil {
		return fmt.Errorf("用户 %q 已由启动参数定义，请换个名字或在命令行里改", name)
	}
	if a.findUser(name) != nil {
		return fmt.Errorf("用户 %q 已存在", name)
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	acc := &account{
		Username:  name,
		hash:      hash,
		Rules:     toPathRules(rules),
		createdAt: now,
		updatedAt: now,
	}
	a.users = append(a.users, acc)
	if err := a.commit(); err != nil {
		// 落盘失败就回滚内存，避免「界面上有、重启就没了」。
		a.users = a.users[:len(a.users)-1]
		return err
	}
	a.refreshEnabled()
	a.pwCache.clear()
	return nil
}

// UpdateUser 修改用户表账号。password 为空表示保持原密码不变。
func (a *Authenticator) UpdateUser(name, password string, rules []Rule) error {
	if a == nil {
		return fmt.Errorf("当前没有启用鉴权")
	}
	name, err := ValidateUserName(name)
	if err != nil {
		return err
	}
	if password != "" && len(password) < minUserPasswordLen {
		return fmt.Errorf("密码至少 %d 位", minUserPasswordLen)
	}
	rules, err = ValidateRules(rules)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.findStatic(name) != nil {
		return fmt.Errorf("用户 %q 来自启动参数，请改命令行参数后重启", name)
	}
	acc := a.findUser(name)
	if acc == nil {
		return fmt.Errorf("用户 %q 不存在", name)
	}

	// 先在副本上改，落盘成功后再换上去 —— 失败时内存状态原样保留。
	oldRules, oldHash := acc.Rules, acc.hash
	next := mergeRules(nil, toPathRules(rules))

	candidate := make([]*account, 0, len(a.users))
	for _, x := range a.users {
		if x == acc {
			candidate = append(candidate, &account{Username: x.Username, Rules: next})
			continue
		}
		candidate = append(candidate, x)
	}
	if err := a.guardRootAdmin(candidate); err != nil {
		return err
	}

	if password != "" {
		hash, err := hashPassword(password)
		if err != nil {
			return err
		}
		acc.hash = hash
	}
	acc.Rules = next
	acc.updatedAt = time.Now().Unix()
	if err := a.commit(); err != nil {
		acc.Rules, acc.hash = oldRules, oldHash
		return err
	}
	a.pwCache.clear()
	return nil
}

// DeleteUser 删除用户表账号。
func (a *Authenticator) DeleteUser(name string) error {
	if a == nil {
		return fmt.Errorf("当前没有启用鉴权")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.findStatic(name) != nil {
		return fmt.Errorf("用户 %q 来自启动参数，删不掉（要去掉就改命令行后重启）", name)
	}
	idx := -1
	for i, acc := range a.users {
		if acc.Username == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("用户 %q 不存在", name)
	}

	candidate := make([]*account, 0, len(a.users)-1)
	candidate = append(candidate, a.users[:idx]...)
	candidate = append(candidate, a.users[idx+1:]...)
	if err := a.guardRootAdmin(candidate); err != nil {
		return err
	}

	removed := a.users[idx]
	a.users = candidate
	if err := a.commit(); err != nil {
		a.users = append(a.users[:idx], append([]*account{removed}, a.users[idx:]...)...)
		return err
	}
	a.refreshEnabled()
	a.pwCache.clear()
	return nil
}

// guardRootAdmin 检查「按给定的候选用户表执行后，是否仍有人对根目录有读写权限」。
//
// 这是防呆而不是安全边界：管理员完全可能把自己降权或删掉，之后
// 页面上没有任何入口能救回来（尤其是所有账号都在用户表里的部署）。
// 只要有启动参数定义的账号或匿名规则兜底，这条检查自然不会触发。
//
// 调用方需持有写锁。
func (a *Authenticator) guardRootAdmin(users []*account) error {
	if canAdminRoot(a.static) || canAdminRoot(users) || canAdminRoot(
		[]*account{a.anonAcc}) {
		return nil
	}
	return fmt.Errorf("至少要保留一个对根目录（/）有读写权限的账号，否则就再没有入口能管理了")
}

// canAdminRoot 判断这组账号里有没有「对根目录可读写」的。
// 匿名账号（Username 为空）也算 —— `-a @/:rw` 同样是全权访问。
func canAdminRoot(accs []*account) bool {
	for _, acc := range accs {
		if acc != nil && acc.match("/") == PermReadWrite {
			return true
		}
	}
	return false
}

func toPathRules(rules []Rule) []pathRule {
	out := make([]pathRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, pathRule{Path: r.Path, Perm: r.Perm})
	}
	return out
}

// ------------------------------------------------------------------ 持久化

// userFileVersion 是当前落盘格式的版本号，便于以后兼容老文件。
const userFileVersion = 1

type userFileDoc struct {
	Version int        `json:"version"`
	Users   []userJSON `json:"users"`
}

type userJSON struct {
	Name      string     `json:"name"`
	Salt      string     `json:"salt"` // base64
	Hash      string     `json:"hash"` // base64
	Iter      int        `json:"iter"`
	Rules     []ruleJSON `json:"rules"`
	CreatedAt int64      `json:"created_at,omitempty"`
	UpdatedAt int64      `json:"updated_at,omitempty"`
}

type ruleJSON struct {
	Path string `json:"path"`
	Perm string `json:"perm"` // "rw" / "r"
}

// commit 把用户表写盘。调用方需持有写锁。
//
// 与上传密钥一样走「先写临时文件再 rename」：中途崩溃也不会留下半个 JSON
// 把服务卡在启动失败上。
func (a *Authenticator) commit() error {
	if a.userFile == "" {
		return nil
	}
	doc := userFileDoc{Version: userFileVersion}
	for _, acc := range a.users {
		u := userJSON{
			Name:      acc.Username,
			Salt:      base64.StdEncoding.EncodeToString(acc.hash.Salt),
			Hash:      base64.StdEncoding.EncodeToString(acc.hash.Key),
			Iter:      acc.hash.Iter,
			CreatedAt: acc.createdAt,
			UpdatedAt: acc.updatedAt,
		}
		for _, r := range acc.Rules {
			u.Rules = append(u.Rules, ruleJSON{Path: r.Path, Perm: r.Perm.String()})
		}
		doc.Users = append(doc.Users, u)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化用户表失败: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(a.userFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建用户表目录失败: %w", err)
		}
	}
	tmp := a.userFile + ".tmp"
	// 0600：文件里是密码摘要，虽然不能用它直接登录，也没必要让同机其他用户读到。
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入用户表失败: %w", err)
	}
	if err := os.Rename(tmp, a.userFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换用户表失败: %w", err)
	}
	return nil
}

// loadUsers 读取用户表。文件不存在时返回空表（首次运行是正常情况）。
func loadUsers(path string) ([]userJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取用户表 %s 失败: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var doc userFileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析用户表 %s 失败: %w", path, err)
	}
	if doc.Version > userFileVersion {
		return nil, fmt.Errorf("用户表 %s 的格式版本 %d 高于本程序支持的 %d，请升级 gofs",
			path, doc.Version, userFileVersion)
	}
	return doc.Users, nil
}

// toAccount 把落盘的记录还原成账号。
func (u userJSON) toAccount() (*account, error) {
	name, err := ValidateUserName(u.Name)
	if err != nil {
		return nil, err
	}
	salt, err := base64.StdEncoding.DecodeString(u.Salt)
	if err != nil || len(salt) < 8 {
		return nil, fmt.Errorf("盐值不合法")
	}
	key, err := base64.StdEncoding.DecodeString(u.Hash)
	if err != nil || len(key) < 16 {
		return nil, fmt.Errorf("密码摘要不合法")
	}
	if u.Iter < 1000 || u.Iter > 10_000_000 {
		return nil, fmt.Errorf("迭代轮数 %d 超出合理范围", u.Iter)
	}
	var rules []Rule
	for _, r := range u.Rules {
		perm := PermRead
		if r.Perm == "rw" {
			perm = PermReadWrite
		} else if r.Perm != "r" && r.Perm != "" {
			return nil, fmt.Errorf("路径 %q 的权限 %q 不合法", r.Path, r.Perm)
		}
		rules = append(rules, Rule{Path: r.Path, Perm: perm})
	}
	rules, err = ValidateRules(rules)
	if err != nil {
		return nil, err
	}
	return &account{
		Username:  name,
		hash:      &passwordHash{Salt: salt, Iter: u.Iter, Key: key},
		Rules:     mergeRules(nil, toPathRules(rules)),
		createdAt: u.CreatedAt,
		updatedAt: u.UpdatedAt,
	}, nil
}
