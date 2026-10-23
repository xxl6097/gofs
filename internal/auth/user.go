package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// parsePasswordHash 从落盘字段还原摘要，并校验参数范围。
//
// 读文件时不能直接相信这些值：盐或密钥过短会让定长比较形同虚设，
// 轮数过大则是一次启动就卡死。
func parsePasswordHash(saltB64, hashB64 string, iter int) (*passwordHash, error) {
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) < 8 {
		return nil, fmt.Errorf("盐值不合法")
	}
	key, err := base64.StdEncoding.DecodeString(hashB64)
	if err != nil || len(key) < 16 {
		return nil, fmt.Errorf("密码摘要不合法")
	}
	if iter < 1000 || iter > 10_000_000 {
		return nil, fmt.Errorf("迭代轮数 %d 超出合理范围", iter)
	}
	return &passwordHash{Salt: salt, Iter: iter, Key: key}, nil
}

// encodePasswordHash 把摘要拆成落盘用的三个字段。
func encodePasswordHash(h *passwordHash) (saltB64, hashB64 string, iter int) {
	return base64.StdEncoding.EncodeToString(h.Salt),
		base64.StdEncoding.EncodeToString(h.Key), h.Iter
}

// passwordOverride 是启动参数账号的密码覆盖值。
//
// base 记录「创建覆盖值时命令行里的那个密码」的摘要：启动时用它判断运维
// 有没有改过 -a。改过就让覆盖值失效、命令行重新成为权威，避免出现
// 「我改了命令行密码，怎么登录还是旧密码」。
type passwordOverride struct {
	hash      *passwordHash
	base      *passwordHash
	updatedAt int64
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
		// 规则字面量允许用户在首尾留空格；请求路径不 trim，所以这里
		// 必须在入口把规则的空白清掉，否则 UI 输入会静默匹配不上。
		r.Path = strings.TrimSpace(r.Path)
		// 在归一化**之前**检查 `..` 段：normalizePath 会把它静默折叠掉，
		// 之后再查就查不到了。规则里的 `..` 是歧义输入，直接拒绝比猜测意图好。
		for _, seg := range strings.Split(strings.ReplaceAll(r.Path, "\\", "/"), "/") {
			if seg == ".." {
				return nil, fmt.Errorf("路径 %q 不能包含 ..", r.Path)
			}
		}
		p := normalizePath(r.Path)
		if p == "" {
			return nil, fmt.Errorf("路径不能为空")
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

// RulePathsUnder 返回落在 prefix 之下（含自身）的 ACL 规则路径。
//
// 供「新增站点」时做冲突提示：ACL 是按 URL 前缀匹配的，一条原本针对主目录
// `/backup` 的规则，在标签同为 backup 的站点挂上之后会静默地作用于新站点。
// 这里只收集**具体落在该前缀下**的规则；`/` 这类全局规则不在此列。
func (a *Authenticator) RulePathsUnder(prefix string) []string {
	if a == nil {
		return nil
	}
	prefix = normalizePath(prefix)
	a.mu.RLock()
	defer a.mu.RUnlock()

	seen := map[string]bool{}
	var out []string
	add := func(acc *account) {
		if acc == nil {
			return
		}
		for _, r := range acc.Rules {
			if r.Path != prefix && !strings.HasPrefix(r.Path, prefix+"/") {
				continue
			}
			if !seen[r.Path] {
				seen[r.Path] = true
				out = append(out, r.Path)
			}
		}
	}
	add(a.anonAcc)
	for _, acc := range a.static {
		add(acc)
	}
	for _, acc := range a.users {
		add(acc)
	}
	sort.Strings(out)
	return out
}

// FromStartup 报告某个账号是否由启动参数（-a / GOFS_AUTH）定义。
//
// 这类账号的**规则**以命令行为准，用户表里不能改；但密码允许被页面覆盖，
// 见 ChangePassword。界面用它给出对应说明。
func (a *Authenticator) FromStartup(name string) bool {
	if a == nil || name == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.findStatic(name) != nil
}

// applyStartupPasswords 把用户表里的密码覆盖值套到启动参数账号上。
//
// 覆盖值只在「命令行密码没变过」时生效：base 摘要是创建覆盖值时命令行
// 密码的摘要，对不上说明运维后来改了 -a —— 此时丢弃覆盖值，让命令行
// 重新成为权威。这样页面改密与命令行改密都符合直觉，不会出现改了
// 命令行却登不上的情况。
//
// 返回 changed=true 表示文件里有需要清理的覆盖值（命令行密码已变，
// 或账号已从 -a 中移除）。调用方应随后 commit 一次。
//
// 调用方需持有写锁（Open 阶段还没有并发，等效于独占）。
func (a *Authenticator) applyStartupPasswords(overrides map[string]startupPasswordJSON, path string) (changed bool, err error) {
	used := make(map[string]bool, len(a.static))
	for _, acc := range a.static {
		raw, ok := overrides[acc.Username]
		if !ok {
			continue
		}
		used[acc.Username] = true
		hash, perr := parsePasswordHash(raw.Salt, raw.Hash, raw.Iter)
		if perr != nil {
			return false, fmt.Errorf("用户表 %s 中账号 %q 的密码覆盖值无效: %w", path, acc.Username, perr)
		}
		var base *passwordHash
		if raw.BaseSalt != "" {
			base, perr = parsePasswordHash(raw.BaseSalt, raw.BaseHash, raw.BaseIter)
			if perr != nil {
				return false, fmt.Errorf("用户表 %s 中账号 %q 的密码基准值无效: %w", path, acc.Username, perr)
			}
			if !base.verify(acc.password) {
				// 命令行密码已经换过：覆盖值作废。
				changed = true
				continue
			}
		}
		acc.hash = hash
		acc.updatedAt = raw.UpdatedAt
		a.staticPasswords[acc.Username] = &passwordOverride{
			hash: hash, base: base, updatedAt: raw.UpdatedAt,
		}
	}
	// 账号已从 -a 中移除时，它的覆盖值也该一起清掉。
	for name := range overrides {
		if !used[name] {
			changed = true
			break
		}
	}
	return changed, nil
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
	// 改了密码就把这个账号已发的会话全部作废。
	// 光清 pwCache 只能让旧密码不再通过校验，已经发出去的 cookie 还能读到东西 ——
	// 那正是「我都改密码了，怎么还被登着」。
	a.dropUserSessions(name)
	return nil
}

// ErrPasswordMismatch 表示账号自助改密时给出的「当前密码」不正确。
//
// 单独一个哨兵错误，好让调用方把「旧密码不对」（403，可计入失败限速）
// 与「新密码太短 / 落盘失败」（400）区分开。
var ErrPasswordMismatch = errors.New("当前密码不正确")

// ChangePassword 供账号**自助**修改自己的密码，必须同时提供当前密码。
//
// 与 UpdateUser 的区别：UpdateUser 是管理员重置，可以不提供旧密码；
// 这里是一张已登录的会话（或 Basic 凭据）在改自己的密码，要求再验一次
// 当前密码，避免会话被窃取后攻击者直接换掉密码把主人锁在门外。
//
// 启动参数里的账号也能改：新密码以覆盖值写进用户表，重启后仍然有效。
// 覆盖值同时记录「创建时的命令行密码」摘要；如果运维后来改了 -a，
// 启动时会让覆盖值失效（见 applyStartupPasswords）。
func (a *Authenticator) ChangePassword(name, currentPassword, newPassword string) error {
	if a == nil {
		return fmt.Errorf("当前没有启用鉴权")
	}
	name, err := ValidateUserName(name)
	if err != nil {
		return err
	}
	if len(newPassword) < minUserPasswordLen {
		return fmt.Errorf("新密码至少 %d 位", minUserPasswordLen)
	}

	a.mu.RLock()
	acc := a.findStatic(name)
	isStatic := acc != nil
	if acc == nil {
		acc = a.findUser(name)
	}
	if acc == nil {
		a.mu.RUnlock()
		return fmt.Errorf("用户 %q 不存在", name)
	}
	// 旧密码校验可能要跑一次 PBKDF2（约 100ms）。放在读锁里完成，
	// 不占写锁 —— 否则一个改密请求就能把所有人的权限查询挡在后面。
	currentHash := acc.hash
	// 启动参数账号的 acc.password 是命令行里的明文。覆盖生效时它仍然保留，
	// 用来给下一次启动判断「命令行有没有被改过」。
	cliPassword := ""
	if isStatic {
		cliPassword = acc.password
	}
	matched := a.passwordMatches(acc, currentPassword)
	a.mu.RUnlock()
	if !matched {
		return ErrPasswordMismatch
	}

	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	// 记录命令行密码的摘要（只用于判断它是否变过，不用于登录校验）。
	// 每次改密都重新生成随机盐，与真正的密码摘要一样安全。
	var base *passwordHash
	if isStatic {
		if base, err = hashPassword(cliPassword); err != nil {
			return err
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now().Unix()
	if isStatic {
		// 校验与落盘之间账号可能被并发改密：哈希指针变了就说明有人抢先了。
		acc = a.findStatic(name)
		if acc == nil {
			return fmt.Errorf("用户 %q 不存在", name)
		}
		if acc.hash != currentHash {
			return fmt.Errorf("用户 %q 的密码刚刚被其他人修改，请重新登录后再试", name)
		}
		oldHash, oldUpdated := acc.hash, acc.updatedAt
		oldOverride, hadOverride := a.staticPasswords[name]
		acc.hash, acc.updatedAt = hash, now
		a.staticPasswords[name] = &passwordOverride{hash: hash, base: base, updatedAt: now}
		if err := a.commit(); err != nil {
			acc.hash, acc.updatedAt = oldHash, oldUpdated
			if hadOverride {
				a.staticPasswords[name] = oldOverride
			} else {
				delete(a.staticPasswords, name)
			}
			return err
		}
	} else {
		// 校验与落盘之间账号可能被删除，或被管理员重置了密码。
		// 后者必须重新来一遍：否则会拿旧密码的校验结果覆盖掉新密码。
		acc = a.findUser(name)
		if acc == nil {
			return fmt.Errorf("用户 %q 不存在", name)
		}
		if acc.hash != currentHash {
			return fmt.Errorf("用户 %q 的密码刚刚被其他人修改，请重新登录后再试", name)
		}
		// 先在副本上改，落盘成功后再换上去 —— 失败时内存状态原样保留。
		oldHash, oldUpdated := acc.hash, acc.updatedAt
		acc.hash = hash
		acc.updatedAt = now
		if err := a.commit(); err != nil {
			acc.hash, acc.updatedAt = oldHash, oldUpdated
			return err
		}
	}
	a.pwCache.clear()
	// 与管理员重置密码同一条规则：旧密码、旧会话都当场失效。
	// 光清 pwCache 只能挡住旧密码，已经发出去的 cookie 仍能读到东西。
	a.dropUserSessions(name)
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
	// 账号没了，它的会话也必须一起断（ResolveSession 里也有一道兜底，
	// 但这里断掉更及时，省得留着一张等着被拒绝的 cookie）。
	a.dropUserSessions(removed.Username)
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
	// StartupPasswords 保存启动参数账号的密码覆盖值（用户名 -> 摘要）。
	// 老版本文件没有这一段，读出来是空 map，不影响兼容。
	StartupPasswords map[string]startupPasswordJSON `json:"startup_passwords,omitempty"`
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

// startupPasswordJSON 是启动参数账号的密码覆盖值。
//
// Base* 是创建覆盖值时命令行密码的摘要：启动时用它判断 -a 是否被改过，
// 改过就让覆盖值失效。旧文件没有这三个字段，此时直接采用覆盖值。
type startupPasswordJSON struct {
	Salt      string `json:"salt"` // base64
	Hash      string `json:"hash"` // base64
	Iter      int    `json:"iter"`
	BaseSalt  string `json:"base_salt,omitempty"`
	BaseHash  string `json:"base_hash,omitempty"`
	BaseIter  int    `json:"base_iter,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
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
		salt, hash, iter := encodePasswordHash(acc.hash)
		u := userJSON{
			Name:      acc.Username,
			Salt:      salt,
			Hash:      hash,
			Iter:      iter,
			CreatedAt: acc.createdAt,
			UpdatedAt: acc.updatedAt,
		}
		for _, r := range acc.Rules {
			u.Rules = append(u.Rules, ruleJSON{Path: r.Path, Perm: r.Perm.String()})
		}
		doc.Users = append(doc.Users, u)
	}
	// 只写当前仍然存在的启动参数账号：某个 -a 账号被摘掉后，
	// 它的覆盖值不该永远留在文件里。
	for _, acc := range a.static {
		ov := a.staticPasswords[acc.Username]
		if ov == nil || ov.hash == nil {
			continue
		}
		salt, hash, iter := encodePasswordHash(ov.hash)
		item := startupPasswordJSON{
			Salt: salt, Hash: hash, Iter: iter, UpdatedAt: ov.updatedAt,
		}
		if ov.base != nil {
			item.BaseSalt, item.BaseHash, item.BaseIter = encodePasswordHash(ov.base)
		}
		if doc.StartupPasswords == nil {
			doc.StartupPasswords = make(map[string]startupPasswordJSON)
		}
		doc.StartupPasswords[acc.Username] = item
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化用户表失败: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(a.userFile)
	if dir == "" {
		dir = "."
	}
	if dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建用户表目录失败: %w", err)
		}
	}
	// 用随机临时名，而不是固定的 users.json.tmp：
	// 固定名字可被本地攻击者预先放一条软链，让服务以自身权限覆盖任意文件。
	tmp, err := os.CreateTemp(dir, ".users-*.tmp")
	if err != nil {
		return fmt.Errorf("创建用户表临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	abort := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		abort()
		return fmt.Errorf("写入用户表失败: %w", err)
	}
	// 0600：文件里是密码摘要，虽然不能用它直接登录，也没必要让同机其他用户读到。
	if err := tmp.Chmod(0o600); err != nil {
		abort()
		return fmt.Errorf("设置用户表权限失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		abort()
		return fmt.Errorf("同步用户表失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("关闭用户表临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, a.userFile); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("替换用户表失败: %w", err)
	}
	return nil
}

// loadUserDoc 读取用户表。文件不存在时返回空表（首次运行是正常情况）。
func loadUserDoc(path string) (userFileDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return userFileDoc{}, nil
		}
		return userFileDoc{}, fmt.Errorf("读取用户表 %s 失败: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return userFileDoc{}, nil
	}
	var doc userFileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return userFileDoc{}, fmt.Errorf("解析用户表 %s 失败: %w", path, err)
	}
	if doc.Version > userFileVersion {
		return userFileDoc{}, fmt.Errorf("用户表 %s 的格式版本 %d 高于本程序支持的 %d，请升级 gofs",
			path, doc.Version, userFileVersion)
	}
	return doc, nil
}

// toAccount 把落盘的记录还原成账号。
func (u userJSON) toAccount() (*account, error) {
	name, err := ValidateUserName(u.Name)
	if err != nil {
		return nil, err
	}
	hash, err := parsePasswordHash(u.Salt, u.Hash, u.Iter)
	if err != nil {
		return nil, err
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
		hash:      hash,
		Rules:     mergeRules(nil, toPathRules(rules)),
		createdAt: u.CreatedAt,
		updatedAt: u.UpdatedAt,
	}, nil
}
