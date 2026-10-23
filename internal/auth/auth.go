// Package auth 实现基于路径的账号权限控制。
//
// 规则语法参考 dufs：
//
//	user:pass@/dir1:rw,/dir2      user 在 /dir1 下可读写，在 /dir2 下只读
//	guest:guest@/                 匿名外的 guest 账号对整个根目录只读
//	@/                            整个根目录匿名可见（只读）
//
// 说明：
//   - 以 @ 分隔「账号」与「路径列表」；账号缺省表示匿名用户。
//   - 账号内以 : 分隔用户名与密码；密码缺省表示空密码。
//   - 路径后缀 :rw 表示可读写，:ro 或省略表示只读。
//   - 多条规则按「最长前缀优先」匹配，未命中任何规则即拒绝。
//
// 鉴权只是权限上限，最终仍受全局 --allow-* 开关约束。
//
// 账号有两个来源，合并后一起参与匹配：
//   - **启动参数**（-a / GOFS_AUTH）：启动后不可改，界面上标记为「来自启动参数」；
//   - **用户表**（users.json，见 user.go）：页面上可增删改，密码只存 PBKDF2 摘要。
//
// 所有查询方法都是并发安全的（改动只发生在管理接口里，用写锁串行化）。
package auth

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Permission 表示对某个路径的授权级别。
type Permission int

const (
	// PermNone 未授权。
	PermNone Permission = iota
	// PermRead 只读。
	PermRead
	// PermReadWrite 可读写。
	PermReadWrite
)

// String 返回权限的可读形式（也是 API / 界面里用的取值）。
func (p Permission) String() string {
	switch p {
	case PermReadWrite:
		return "rw"
	case PermRead:
		return "r"
	default:
		return "none"
	}
}

// pathRule 为单条路径规则。
type pathRule struct {
	Path string
	Perm Permission
}

// account 为单个账号（或匿名）的规则集合。
//
// 密码有两种形态，二选一：
//   - password：明文，只可能来自启动参数（用户自己写在命令行里，加密存储没有意义）；
//   - hash：PBKDF2 摘要，来自页面上创建的用户。
type account struct {
	Username string
	password string
	hash     *passwordHash
	Rules    []pathRule
	anon     bool

	// fromStartup 标记账号来自 -a / GOFS_AUTH，界面上只读。
	fromStartup bool
	createdAt   int64 // Unix 秒；启动参数来的账号为 0
	updatedAt   int64
}

// Authenticator 保存解析后的全部鉴权规则。
type Authenticator struct {
	// mu 保护下面所有字段。管理接口会改写它们，而每个请求都要读。
	mu      sync.RWMutex
	users   []*account // 来自用户表的账号（可改）
	anonAcc *account   // 匿名规则（只有启动参数能定义）
	static  []*account // 来自启动参数的账号（规则只读，密码可被页面覆盖）
	enabled bool

	userFile string   // 用户表落盘路径，空串表示仅内存
	pwCache  *pwCache // 「账号+密码」校验结果的缓存，见 pwCache 的注释

	// staticPasswords 保存启动参数账号的密码覆盖值（用户名 -> 摘要）。
	//
	// 启动参数的规则仍然只读，但密码允许账号在页面上自助修改：否则用 -a
	// 配了唯一管理员之后，这个口令就再也改不了。覆盖值写进用户表的
	// startup_passwords 段；若运维后来改了命令行里的密码，覆盖值会自动失效
	// （见 applyStartupPasswords），让命令行重新成为权威。
	staticPasswords map[string]*passwordOverride

	// verifySem 限制同时进行的 PBKDF2 计算数。
	//
	// PBKDF2 是按「故意很慢」设计的；没有这道闸门时，攻击者可以用大量
	// 并发请求（尤其是能绕过失败限速的入口）把 CPU 打满。超过等待窗口
	// 的校验直接失败（fail closed），而不是无限排队。
	verifySem chan struct{}

	// sessions 是服务端会话表（token -> 会话），供 cookie 鉴权使用。
	// 见 session.go 里为什么不用自包含 token。
	sessions map[string]*Session
}

// Parse 解析全部鉴权规则串。
func Parse(rules []string) (*Authenticator, error) {
	return Open(rules, "")
}

// Open 解析启动参数里的规则，并加载用户表。
//
// userFile 为空时用户表只存在于内存中（重启即失效），
// 与上传密钥的 --key-file 语义保持一致。
func Open(rules []string, userFile string) (*Authenticator, error) {
	a := &Authenticator{
		userFile:        userFile,
		pwCache:         newPWCache(),
		sessions:        make(map[string]*Session),
		verifySem:       make(chan struct{}, verifyConcurrency()),
		staticPasswords: make(map[string]*passwordOverride),
	}
	for _, raw := range rules {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		acc, err := parseRule(raw)
		if err != nil {
			return nil, err
		}
		acc.fromStartup = true
		if acc.anon {
			if a.anonAcc != nil {
				a.anonAcc.Rules = mergeRules(a.anonAcc.Rules, acc.Rules)
				continue
			}
			a.anonAcc = acc
			continue
		}
		if err := a.mergeStatic(acc); err != nil {
			return nil, err
		}
	}

	if userFile != "" {
		doc, err := loadUserDoc(userFile)
		if err != nil {
			return nil, err
		}
		for _, u := range doc.Users {
			acc, err := u.toAccount()
			if err != nil {
				return nil, fmt.Errorf("用户表 %s 中的账号 %q 无效: %w", userFile, u.Name, err)
			}
			// 与启动参数重名时**以启动参数为准**并跳过，而不是启动失败：
			// 用户表的文件可能比命令行先存在，谁对谁错很难说，但
			// 「命令行是权威」这条规则简单且不会把服务卡死。
			if a.findStatic(acc.Username) != nil {
				continue
			}
			a.users = append(a.users, acc)
		}
		changed, err := a.applyStartupPasswords(doc.StartupPasswords, userFile)
		if err != nil {
			return nil, err
		}
		if changed {
			// 清理失效的覆盖值（改过命令行密码 / 账号已移除）。
			// 写盘失败不影响本次启动：内存里已经按命令行密码处理了，
			// 下一次启动会再尝试清理。
			_ = a.commit()
		}
	}

	a.refreshEnabled()
	if !a.enabled {
		return nil, nil
	}
	return a, nil
}

// refreshEnabled 重新计算「是否有任何鉴权规则」。调用方需持有写锁。
func (a *Authenticator) refreshEnabled() {
	a.enabled = a.anonAcc != nil || len(a.static) > 0 || len(a.users) > 0
}

// findStatic 按用户名查启动参数里的账号。调用方需持有锁。
func (a *Authenticator) findStatic(name string) *account {
	for _, acc := range a.static {
		if acc.Username == name {
			return acc
		}
	}
	return nil
}

// findUser 按用户名查用户表里的账号。调用方需持有锁。
func (a *Authenticator) findUser(name string) *account {
	for _, acc := range a.users {
		if acc.Username == name {
			return acc
		}
	}
	return nil
}

// mergeRules 合并两组路径规则，保持「长的在前」，便于最长前缀匹配。
func mergeRules(dst, src []pathRule) []pathRule {
	out := append(append([]pathRule{}, dst...), src...)
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Path) > len(out[j].Path) })
	return out
}

// parseRule 解析单条规则串。
func parseRule(raw string) (*account, error) {
	at := strings.Index(raw, "@")
	if at < 0 {
		return nil, fmt.Errorf("鉴权规则 %q 缺少 @ 分隔符，正确形式如 user:pass@/dir:rw", raw)
	}
	cred := raw[:at]
	pathSpec := raw[at+1:]

	acc := &account{}
	if cred == "" {
		acc.anon = true
	} else {
		if i := strings.Index(cred, ":"); i >= 0 {
			acc.Username = cred[:i]
			acc.password = cred[i+1:]
		} else {
			acc.Username = cred
		}
		if acc.Username == "" {
			return nil, fmt.Errorf("鉴权规则 %q 的用户名为空", raw)
		}
	}

	if strings.TrimSpace(pathSpec) == "" {
		pathSpec = "/"
	}
	for _, item := range strings.Split(pathSpec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		perm := PermRead
		switch {
		case strings.HasSuffix(item, ":rw"):
			perm = PermReadWrite
			item = strings.TrimSuffix(item, ":rw")
		case strings.HasSuffix(item, ":ro"), strings.HasSuffix(item, ":r"):
			// :ro 是 dufs 的写法；:r 是页面上「只读」的取值，一并接受 ——
			// 否则 `bob:pw@/docs:r` 会被当成一个叫「/docs:r」的目录，
			// bob 就莫名其妙什么权限都没有了（这种静默失败很难排查）。
			perm = PermRead
			if strings.HasSuffix(item, ":ro") {
				item = strings.TrimSuffix(item, ":ro")
			} else {
				item = strings.TrimSuffix(item, ":r")
			}
		}
		p := normalizePath(item)
		acc.Rules = append(acc.Rules, pathRule{Path: p, Perm: perm})
	}
	if len(acc.Rules) == 0 {
		return nil, fmt.Errorf("鉴权规则 %q 未包含任何有效路径", raw)
	}
	// 路径长的排前面，便于按最长前缀匹配。
	sort.SliceStable(acc.Rules, func(i, j int) bool {
		return len(acc.Rules[i].Path) > len(acc.Rules[j].Path)
	})
	return acc, nil
}

// mergeStatic 把新账号并入启动参数集合；同名账号的规则会累加。
func (a *Authenticator) mergeStatic(in *account) error {
	if exist := a.findStatic(in.Username); exist != nil {
		if exist.password != in.password {
			return fmt.Errorf("账号 %q 出现了不一致的密码", in.Username)
		}
		exist.Rules = mergeRules(exist.Rules, in.Rules)
		return nil
	}
	a.static = append(a.static, in)
	return nil
}

// normalizePath 归一化规则与请求路径。
//
// 这里必须折叠 `.`/`..` 与重复斜杠：鉴权与落盘解析（fsutil.CleanURLPath）
// 必须对同一个请求得出一致的路径。早先只做了去尾斜杠，于是
// `/docs/../secret` 在 ACL 眼里仍属于 `/docs`，落盘时却被折叠成 `/secret` ——
// 受限账号因此可以越出自己的目录。规则本身由 ValidateRules 拒绝 `..`，
// 所以对合法配置这是纯防御。
//
// ⚠️ **不能 trim 请求路径**：`/docs/uploads `（尾随空格）在 Linux/macOS 上
// 是合法且与 `/docs/uploads` 不同的文件。若这里把请求路径 trim 掉，ACL
// 会按 `/docs/uploads` 匹配，落盘却打开另一个条目，形成越权。
// 规则字面量在 parseRule / ValidateRules 的边界处单独 trim。
//
// ⚠️ 刻意**不**把 `\` 当成分隔符：Unix 上它是合法的文件名字符，而
// Resolve 也不会转换它。在鉴权层转换会造成「鉴权看 /docs/secret、
// 实际打开 /docs\secret」的错位。Windows 上 Resolve 会把它当分隔符，
// 那时鉴权只会更严格（误拒），不会放行。
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	// path.Clean 会补上前导 /、折叠 . 与 ..、去掉重复斜杠与尾斜杠。
	return path.Clean("/" + p)
}

// Enabled 表示是否存在任何鉴权规则。
func (a *Authenticator) Enabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.enabled
}

// Verify 判断一组凭据是否对应某个已知的具名账号。
//
// 与 Lookup 的区别：Verify 只回答「这对账号密码是否有效」，
// 不关心该账号对哪个路径有权限。登录框需要的是前者——
// 若用 Lookup("/") 校验，一个只被授予 /docs 权限的账号会因为对根目录无权限而登录失败。
func (a *Authenticator) Verify(username, password string) bool {
	if !a.Enabled() {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if acc := a.lookupAccount(username); acc != nil {
		return a.passwordMatches(acc, password)
	}
	// 未知用户名也跑一次同等成本的 PBKDF2，抹平「用户是否存在」的时序差异。
	a.verifyDummy(password)
	return false
}

// lookupAccount 按用户名找账号（先用户表再启动参数，重名以启动参数为准）。
// 调用方需持有锁。
func (a *Authenticator) lookupAccount(username string) *account {
	if username == "" {
		return nil
	}
	if acc := a.findUser(username); acc != nil {
		if a.findStatic(username) == nil {
			return acc
		}
	}
	return a.findStatic(username)
}

// subtleEqual 做定长时间比较，避免通过响应耗时逐字节猜测密码。
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// PBKDF2 并发闸门与未知用户的等时校验。
//
// PBKDF2 单次约 100ms，必须限制同时计算的数量：否则攻击者用大量并发请求
// 就能把 CPU 打满。超过等待窗口的校验直接失败（fail closed），而不是无限
// 排队把请求堆在内存里。
const authVerifyTimeout = 2 * time.Second

// dummySalt 只用于「未知用户名」的等时计算，不需要保密。
var dummySalt = []byte("gofs-dummy-salt!")

func verifyConcurrency() int {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

// withVerifySlot 在并发配额内执行一次慢速密码计算。
// ran=false 表示等待超时、没有执行。
func (a *Authenticator) withVerifySlot(fn func() bool) (ok, ran bool) {
	if a.verifySem == nil {
		return fn(), true
	}
	timer := time.NewTimer(authVerifyTimeout)
	defer timer.Stop()
	select {
	case a.verifySem <- struct{}{}:
		defer func() { <-a.verifySem }()
		return fn(), true
	case <-timer.C:
		return false, false
	}
}

// verifyDummy 对未知用户名执行一次与真实校验等成本的 PBKDF2。
func (a *Authenticator) verifyDummy(password string) {
	_, _ = a.withVerifySlot(func() bool {
		_, _ = pbkdf2.Key(sha256.New, password, dummySalt, pwIterations, pwKeyLen)
		return false
	})
}

// passwordMatches 校验密码。明文账号直接定长比较；用户表的账号走 PBKDF2。
//
// 这里带一层结果缓存：PBKDF2 是按「故意很慢」设计的，而 HTTP Basic 意味着
// **每个请求都要验一次密码**，真按 21 万轮算的话每个请求要多花约 100ms。
// 缓存键是「用户名 + 密码」的摘要，命中就完全跳过计算；
// 任何一次用户表改动都会清空缓存（见 invalidate()）。
func (a *Authenticator) passwordMatches(acc *account, password string) bool {
	key := pwCacheKey(acc.Username, password)
	if ok, hit := a.pwCache.get(key); hit {
		return ok
	}

	if acc.hash == nil {
		ok := subtleEqual(acc.password, password)
		// 启动参数里的明文账号如果只做定长比较，耗时会比未知用户名/用户表
		// 账号的 PBKDF2 低几个数量级，攻击者可以据此枚举启动账号名。
		// 这里补一次等成本的 PBKDF2，并和用户表账号共用结果缓存。
		a.withVerifySlot(func() bool {
			_, _ = pbkdf2.Key(sha256.New, password, dummySalt, pwIterations, pwKeyLen)
			return false
		})
		a.pwCache.put(key, ok)
		return ok
	}

	ok, ran := a.withVerifySlot(func() bool { return acc.hash.verify(password) })
	if ran {
		a.pwCache.put(key, ok)
	}
	return ok
}

// HasAnyWrite 判断该账号是否在**任意**一条规则的路径上具备写权限。
//
// 用于与具体路径无关的资源（例如上传密钥管理）：这类接口不该要求
// 调用者对根目录有权限——一个只被授予 /docs 的账号理应能签发
// 局限在 /docs 内的密钥。具体能签发多大范围的密钥，再由调用者
// 对该范围的 checkPerm 结果决定。
func (a *Authenticator) HasAnyWrite(username, password string, authenticated bool) bool {
	if !a.Enabled() {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	var acc *account
	if authenticated {
		acc = a.lookupAccount(username)
		if acc != nil && !a.passwordMatches(acc, password) {
			return false
		}
	} else {
		acc = a.anonAcc
	}
	if acc == nil {
		return false
	}
	for _, r := range acc.Rules {
		if r.Perm == PermReadWrite {
			return true
		}
	}
	return false
}

// accountFor 取出这组凭据对应的账号：具名账号（密码不对即视为不存在）
// 或匿名账号（未携带凭据时）。调用方需持有锁。
func (a *Authenticator) accountFor(username, password string, authenticated bool) *account {
	if !authenticated {
		return a.anonAcc
	}
	acc := a.lookupAccount(username)
	if acc == nil || !a.passwordMatches(acc, password) {
		return nil
	}
	return acc
}

// Lookup 在提供的凭据下计算 urlPath 的权限。
// 若未配置鉴权，返回 PermReadWrite（由全局开关再收敛）。
func (a *Authenticator) Lookup(urlPath, username, password string, authenticated bool) Permission {
	perm, _ := a.LookupChecked(urlPath, username, password, authenticated)
	return perm
}

// LookupChecked 与 Lookup 相同，但额外报告**具名凭据是否真的匹配**。
//
// 调用方需要区分两种「权限为 None」：
//   - 密码/账号错误 —— 应当计入认证失败限速；
//   - 密码正确、但该账号对这条路径没有权限 —— 不是认证失败，不能锁账号。
//
// 返回的 valid 对匿名查询表示“是否存在匿名规则”，对具名查询表示密码是否匹配。
func (a *Authenticator) LookupChecked(urlPath, username, password string, authenticated bool) (Permission, bool) {
	if !a.Enabled() {
		return PermReadWrite, true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	if !authenticated {
		if a.anonAcc == nil {
			return PermNone, false
		}
		return a.anonAcc.match(urlPath), true
	}

	acc := a.lookupAccount(username)
	if acc == nil {
		// 未知用户名也执行一次等成本的 PBKDF2，避免通过响应时间枚举账号。
		// 空用户名（Basic base64(":pass")）不值得跑慢哈希：它不可能是
		// 合法账号，也没有枚举价值。
		if username != "" {
			a.verifyDummy(password)
		}
		return PermNone, false
	}
	if !a.passwordMatches(acc, password) {
		return PermNone, false
	}
	return acc.match(urlPath), true
}

// HomePath 返回账号登录后应当落地的目录：它有权限的、最靠上的那一层。
//
// 为什么需要它：只被授予 /docs 的账号站在 / 上什么都读不到，而在前端看来
// 那是一次 401 —— 被当成「登录已失效」，于是刚登录就被弹回登录框。换乘到
// 自己的目录才能让登录真正落得下去。前端用它替换地址栏里那个读不到的路径。
//
// 选择顺序：可读写的优先（那才是这个账号真正能干活的地方），
// 同为读写或同为只读时取路径最短的（最靠上的一层）。
// 没有任何可读路径时返回空串。
func (a *Authenticator) HomePath(username, password string, authenticated bool) string {
	if a == nil || !a.Enabled() {
		// 未启用鉴权：到处都能读，落在哪儿都一样。
		return "/"
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	acc := a.accountFor(username, password, authenticated)
	if acc == nil {
		return ""
	}
	best := ""
	bestPerm := PermNone
	for _, r := range acc.Rules {
		if r.Perm == PermNone {
			continue
		}
		if best == "" || r.Perm > bestPerm || (r.Perm == bestPerm && len(r.Path) < len(best)) {
			best, bestPerm = r.Path, r.Perm
		}
	}
	return best
}

// ReadPaths 返回账号可读的规则路径（长的在前），供前端判断某个地址是否
// 可达 —— 面包屑与「上一级」不该把一个必被拒绝的地址画成能点的链接。
//
// 未启用鉴权或账号不可用时返回 nil，调用方应把 nil 当作「不做限制」。
func (a *Authenticator) ReadPaths(username, password string, authenticated bool) []string {
	if a == nil || !a.Enabled() {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	acc := a.accountFor(username, password, authenticated)
	if acc == nil {
		return nil
	}
	out := make([]string, 0, len(acc.Rules))
	for _, r := range acc.Rules {
		if r.Perm != PermNone {
			out = append(out, r.Path)
		}
	}
	return out
}

// HomePathFor 与 HomePath 相同，但只按**已确认的账号名**计算，供会话
// Cookie 使用（会话里没有密码，也没有必要在每次请求时重验）。
func (a *Authenticator) HomePathFor(username string) string {
	if a == nil || !a.Enabled() {
		return "/"
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	acc := a.lookupAccount(username)
	if acc == nil {
		return ""
	}
	best := ""
	bestPerm := PermNone
	for _, r := range acc.Rules {
		if r.Perm == PermNone {
			continue
		}
		if best == "" || r.Perm > bestPerm || (r.Perm == bestPerm && len(r.Path) < len(best)) {
			best, bestPerm = r.Path, r.Perm
		}
	}
	return best
}

// ReadPathsFor 与 ReadPaths 相同，但只按已确认的账号名计算。
func (a *Authenticator) ReadPathsFor(username string) []string {
	if a == nil || !a.Enabled() {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	acc := a.lookupAccount(username)
	if acc == nil {
		return nil
	}
	out := make([]string, 0, len(acc.Rules))
	for _, r := range acc.Rules {
		if r.Perm != PermNone {
			out = append(out, r.Path)
		}
	}
	return out
}

// HasAnyWriteUser 与 HasAnyWrite 相同，但只按已确认的账号名计算。
func (a *Authenticator) HasAnyWriteUser(username string) bool {
	if a == nil || !a.Enabled() {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	acc := a.lookupAccount(username)
	if acc == nil {
		return false
	}
	for _, r := range acc.Rules {
		if r.Perm == PermReadWrite {
			return true
		}
	}
	return false
}

// NeedsAuth 判断该路径是否要求客户端提供凭据（用于决定是否回 401 挑战）。
func (a *Authenticator) NeedsAuth(urlPath string) bool {
	if !a.Enabled() {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.anonAcc != nil && a.anonAcc.match(urlPath) != PermNone {
		return false
	}
	return true
}

// match 按最长前缀匹配路径规则。
func (acc *account) match(urlPath string) Permission {
	urlPath = normalizePath(urlPath)
	for _, r := range acc.Rules {
		if r.Path == "/" {
			return r.Perm
		}
		if urlPath == r.Path || strings.HasPrefix(urlPath, r.Path+"/") {
			return r.Perm
		}
	}
	return PermNone
}
