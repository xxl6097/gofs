// Package uploadkey 实现「上传密钥」。
//
// 场景：让脚本、第三方或临时协作者在**不上交账号密码**的前提下上传文件。
// 密钥只授予上传能力，可限定目标目录，可设置有效期，可随时撤销。
//
// 安全设计：
//   - 明文只在创建时返回一次，服务端只保存 SHA-256 摘要；
//     即使 keys.json 泄露也无法直接拿去使用。
//   - 校验使用定长时间比较，避免通过响应耗时逐字节猜测。
//   - 每次使用记录时间与次数，便于发现异常调用。
package uploadkey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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
	"unicode/utf8"
)

// TokenPrefix 是所有密钥的公共前缀，便于在日志与配置里一眼识别。
const TokenPrefix = "gofs_"

// 密钥数量与字段长度上限。
//
// 没有配额时，任何有写权限的账号都能无限签发密钥：每次创建都要把整份
// keys.json 序列化并 fsync，而每次校验都要线性扫描全部密钥 —— 密钥数
// 一多，服务就会被自己的管理数据拖死。
const (
	// MaxKeys 是全局密钥数上限。
	MaxKeys = 10000
	// MaxKeysPerCreator 是单个账号可创建的密钥数上限。
	MaxKeysPerCreator = 1000
	// MaxKeyNameRunes 是密钥名称的最大字符数。
	MaxKeyNameRunes = 200
	// MaxKeyNoteRunes 是备注的最大字符数。
	MaxKeyNoteRunes = 2000
	// MaxKeyScopeRunes 是 scope 的最大字符数。
	MaxKeyScopeRunes = 1024
)

// 校验失败的原因。
var (
	// ErrNotFound 表示密钥不存在（或格式不对）。
	ErrNotFound = errors.New("上传密钥无效")
	// ErrExpired 表示密钥已过期。
	ErrExpired = errors.New("上传密钥已过期")
	// ErrEmptyToken 表示调用方没提供密钥。
	ErrEmptyToken = errors.New("未提供上传密钥")
	// ErrTooManyKeys 表示密钥数量已达到全局或单账号上限。
	ErrTooManyKeys = errors.New("密钥数量已达上限")
	// ErrInvalidOption 表示创建密钥时的字段不合法（长度等）。
	ErrInvalidOption = errors.New("密钥参数不合法")
)

// 使用统计的落盘节流间隔，避免每次上传都写一次磁盘。
const flushInterval = 30 * time.Second

// Key 描述一把上传密钥。注意这里不保存明文。
type Key struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"` // 明文前若干位，仅供人工识别
	Hash   string `json:"hash"`   // sha256(明文) 的十六进制摘要
	// Scope 是允许访问的路径前缀，例如 "/uploads"；"/" 表示不限。
	//
	// 对上传密钥是「能传到哪」，对分享链接是「能读到哪」。
	Scope string `json:"scope"`
	// Kind 区分这把凭证能做什么：
	//   ""/"upload" 上传密钥（历史数据没有这个字段，按上传处理）
	//   "read"      分享链接：只读，不需要登录
	Kind       string    `json:"kind,omitempty"`
	Creator    string    `json:"creator"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"` // 零值表示永不过期
	LastUsedAt time.Time `json:"last_used_at"`
	UseCount   int64     `json:"use_count"`
	Note       string    `json:"note,omitempty"`
}

// Expired 判断密钥在给定时刻是否已过期。
func (k *Key) Expired(now time.Time) bool {
	return !k.ExpiresAt.IsZero() && now.After(k.ExpiresAt)
}

// Permanent 表示密钥永不过期。
func (k *Key) Permanent() bool { return k.ExpiresAt.IsZero() }

// Status 返回 active / expired。
func (k *Key) Status(now time.Time) string {
	if k.Expired(now) {
		return "expired"
	}
	return "active"
}

// Remaining 返回距离过期还有多久；永不过期时返回 0。
func (k *Key) Remaining(now time.Time) time.Duration {
	if k.ExpiresAt.IsZero() {
		return 0
	}
	d := k.ExpiresAt.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// 凭证类型。
const (
	// KindUpload 是上传密钥：只能往 Scope 里传文件（默认）。
	KindUpload = "upload"
	// KindRead 是分享链接：只能读 Scope 下的内容，**不需要登录**。
	KindRead = "read"
	// KindReadWrite 是分享链接，但把写入也一起给出去：拿到链接的人
	// 不需要登录就能在 Scope 内上传、新建、重命名、删除。
	//
	// 它与 KindUpload 是两回事，别混：上传密钥**只能写**、且走另一条
	// 认证分支（X-Gofs-Upload-Key）；这一种是**能读能写**的公开地址。
	KindReadWrite = "readwrite"
)

// IsShareKind 判断某个 kind 取值是不是分享链接（只读或可读写）。
//
// 归一化之前先判，是因为 keyCreate 拿到的还是请求里的原始字符串。
func IsShareKind(kind string) bool {
	return kind == KindRead || kind == KindReadWrite
}

// KindOf 返回归一化后的类型。
//
// 历史数据里没有 kind 字段，一律当作上传密钥 —— 它们本来就是上传用的，
// 不能因为加了这个字段就把老密钥的语义变掉。
func (k *Key) KindOf() string {
	if IsShareKind(k.Kind) {
		return k.Kind
	}
	return KindUpload
}

// IsShare 报告这把凭证是不是分享链接（只读或可读写）。
//
// 判断「是不是分享」必须走这里，不要写 KindOf() == KindRead —— 加了
// 可读写分享之后，那种写法会把 KindReadWrite 归到「不是分享」里去。
func (k *Key) IsShare() bool { return IsShareKind(k.KindOf()) }

// ReadWrite 报告分享链接是否把写入也一起授出。
func (k *Key) ReadWrite() bool { return k.KindOf() == KindReadWrite }

// CreateOptions 是创建密钥的参数。
type CreateOptions struct {
	Name    string
	Scope   string
	TTL     time.Duration // 0 表示永不过期
	Creator string
	Note    string
	// Kind 为凭证类型，留空按 KindUpload 处理。
	Kind string
}

// Store 保存全部密钥，可选择持久化到磁盘。
type Store struct {
	mu   sync.Mutex
	path string
	keys map[string]*Key
	// byHash 是 SHA-256 摘要 -> 密钥 ID 的索引。
	//
	// 校验不能每次都线性扫描全部密钥：密钥数一多，每个请求都变成 O(N)。
	// token 本身有 192 位随机熵，按摘要查找不构成可用的时序侧信道；
	// Verify 里仍保留一次定长比较作为防御。
	byHash    map[string]string
	order     []string // 维持创建顺序
	lastFlush time.Time
	dirty     bool
}

// fileFormat 是磁盘上的存储格式。
type fileFormat struct {
	Version int    `json:"version"`
	Keys    []*Key `json:"keys"`
}

// New 创建（或加载）密钥存储。path 为空表示仅保存在内存中，重启即失效。
func New(path string) (*Store, error) {
	s := &Store{
		path:   path,
		keys:   make(map[string]*Key),
		byHash: make(map[string]string),
	}
	if path == "" {
		return s, nil
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	// 启动时清掉已过期的密钥：它们校验时会被拒绝，留着只会让
	// keys.json 无限增长（Prune 之前从没被调用过）。
	s.Prune(0)
	return s, nil
}

// Path 返回持久化文件路径，可能为空。
func (s *Store) Path() string { return s.path }

// Persistent 表示密钥会写入磁盘。
func (s *Store) Persistent() bool { return s.path != "" }

// load 从磁盘读取密钥。
func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取密钥文件失败: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var ff fileFormat
	if err := json.Unmarshal(raw, &ff); err != nil {
		return fmt.Errorf("密钥文件格式错误（%s）: %w", s.path, err)
	}
	for _, k := range ff.Keys {
		if k == nil || k.ID == "" || k.Hash == "" {
			continue
		}
		// 摘要统一小写，兼容手工编辑过（或旧版本）的文件。
		k.Hash = strings.ToLower(strings.TrimSpace(k.Hash))
		s.keys[k.ID] = k
		s.byHash[k.Hash] = k.ID
		s.order = append(s.order, k.ID)
	}
	return nil
}

// save 原子写回磁盘。
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	ff := fileFormat{Version: 1, Keys: make([]*Key, 0, len(s.order))}
	for _, id := range s.order {
		if k, ok := s.keys[id]; ok {
			ff.Keys = append(ff.Keys, k)
		}
	}
	raw, err := json.MarshalIndent(ff, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建密钥目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".keys-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	abort := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(raw); err != nil {
		abort()
		return err
	}
	// 密钥摘要等同敏感信息，文件权限收紧到 0600。
	if err := tmp.Chmod(0o600); err != nil {
		abort()
		return err
	}
	if err := tmp.Sync(); err != nil {
		abort()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	s.lastFlush = time.Now()
	s.dirty = false
	return nil
}

// ValidateScope 校验 scope 中不含控制字符。
//
// scope 会进入生成的 shell 脚本（/up）与 usage 示例。即使渲染侧已经做了
// 转义，控制字符（尤其是换行）也可能让注释提前结束，因此创建时就必须
// 拒绝，而不是只在某一个输出路径上补。
func ValidateScope(scope string) error {
	if strings.ContainsFunc(scope, unicode.IsControl) {
		return fmt.Errorf("%w：scope 不能包含控制字符（换行、制表符等）", ErrInvalidOption)
	}
	return nil
}

// Create 生成一把新密钥，返回记录与**只此一次**的明文。
func (s *Store) Create(opts CreateOptions) (*Key, string, error) {
	if err := ValidateScope(opts.Scope); err != nil {
		return nil, "", err
	}
	token, err := generateToken()
	if err != nil {
		return nil, "", err
	}
	id, err := randomID()
	if err != nil {
		return nil, "", err
	}

	scope := strings.TrimSpace(opts.Scope)
	if scope == "" {
		scope = "/"
	}
	if !strings.HasPrefix(scope, "/") {
		scope = "/" + scope
	}
	if len(scope) > 1 {
		scope = strings.TrimSuffix(scope, "/")
	}

	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = "未命名密钥"
	}

	kind := opts.Kind
	if !IsShareKind(kind) {
		kind = KindUpload
	}
	// 两种分享都是「分享链接」，默认名相同 —— 权限级别由 KindOf 体现，
	// 不必在名字上再分一遍（用户自己填的备注优先）。
	if name == "未命名密钥" && IsShareKind(kind) {
		name = "分享链接"
	}
	if utf8.RuneCountInString(name) > MaxKeyNameRunes {
		return nil, "", fmt.Errorf("%w：名称最多 %d 个字符", ErrInvalidOption, MaxKeyNameRunes)
	}
	note := strings.TrimSpace(opts.Note)
	if utf8.RuneCountInString(note) > MaxKeyNoteRunes {
		return nil, "", fmt.Errorf("%w：备注最多 %d 个字符", ErrInvalidOption, MaxKeyNoteRunes)
	}
	if utf8.RuneCountInString(scope) > MaxKeyScopeRunes {
		return nil, "", fmt.Errorf("%w：scope 最多 %d 个字符", ErrInvalidOption, MaxKeyScopeRunes)
	}

	now := time.Now()
	k := &Key{
		ID:        id,
		Name:      name,
		Prefix:    visiblePrefix(token),
		Hash:      hashToken(token),
		Scope:     scope,
		Kind:      kind,
		Creator:   opts.Creator,
		CreatedAt: now,
		Note:      note,
	}
	if opts.TTL > 0 {
		k.ExpiresAt = now.Add(opts.TTL)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 先清掉过期密钥，让配额统计的是「仍可能被使用的」数量。
	s.pruneLocked(now, 0)
	if len(s.keys) >= MaxKeys {
		return nil, "", fmt.Errorf("%w（全局最多 %d 个）", ErrTooManyKeys, MaxKeys)
	}
	if k.Creator != "" && s.countByCreatorLocked(k.Creator) >= MaxKeysPerCreator {
		return nil, "", fmt.Errorf("%w（每个账号最多 %d 个）", ErrTooManyKeys, MaxKeysPerCreator)
	}

	s.keys[k.ID] = k
	s.byHash[k.Hash] = k.ID
	s.order = append(s.order, k.ID)
	s.dirty = true
	if err := s.save(); err != nil {
		// 落盘失败就回滚内存，避免「界面上有、重启就没了」。
		delete(s.keys, k.ID)
		delete(s.byHash, k.Hash)
		s.order = s.order[:len(s.order)-1]
		return nil, "", err
	}
	return k, token, nil
}

// Verify 校验明文密钥，成功时返回对应记录。
// 同时累加使用次数与最后使用时间，并按节流策略落盘。
func (s *Store) Verify(token string) (*Key, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrEmptyToken
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	// O(1) 摘要索引。token 有 192 位随机熵，按摘要查找不构成可用的
	// 时序侧信道；下面仍保留一次定长比较，避免索引被意外写脏时放行。
	id, ok := s.byHash[hash]
	if !ok {
		return nil, ErrNotFound
	}
	found, ok := s.keys[id]
	if !ok {
		delete(s.byHash, hash)
		return nil, ErrNotFound
	}
	if subtle.ConstantTimeCompare([]byte(found.Hash), []byte(hash)) != 1 {
		return nil, ErrNotFound
	}
	if found.Expired(now) {
		return nil, ErrExpired
	}

	found.LastUsedAt = now
	found.UseCount++
	s.dirty = true
	// 使用统计不必每次落盘，按间隔合并写入。
	if now.Sub(s.lastFlush) > flushInterval {
		_ = s.save()
	}
	return found, nil
}

// List 返回全部密钥的副本，按创建时间倒序。
func (s *Store) List() []*Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Key, 0, len(s.keys))
	for _, k := range s.keys {
		cp := *k
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Get 按 ID 取密钥。
func (s *Store) Get(id string) (*Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, false
	}
	cp := *k
	return &cp, true
}

// Revoke 删除密钥。
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.keys, id)
	delete(s.byHash, k.Hash)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.dirty = true
	return s.save()
}

// RevokeIDs 批量撤销一组密钥，只落盘一次。
//
// 规则变更后可能需要撤销多把凭据；逐把调用 Revoke 会每把都 fsync 一次，
// 账号下的密钥一多，用户管理接口就会明显变慢。
func (s *Store) RevokeIDs(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		k, ok := s.keys[id]
		if !ok {
			continue
		}
		drop[id] = true
		delete(s.keys, id)
		delete(s.byHash, k.Hash)
	}
	if len(drop) == 0 {
		return 0, nil
	}
	kept := s.order[:0]
	for _, id := range s.order {
		if !drop[id] {
			kept = append(kept, id)
		}
	}
	s.order = kept
	s.dirty = true
	if err := s.save(); err != nil {
		return len(drop), err
	}
	return len(drop), nil
}

// RevokeByCreator 撤销某个账号创建的全部密钥（含分享链接）。
//
// 账号被删除或密码被重置时调用：会话会被 dropUserSessions 断掉，但这些
// bearer 凭据不会自动失效。不撤销的话，删除一个被入侵的账号之后，攻击者
// 仍能用它此前签发的密钥/分享继续访问。
//
// creator 为空串时不做任何事（避免误删匿名创建的密钥）。
func (s *Store) RevokeByCreator(creator string) (int, error) {
	creator = strings.TrimSpace(creator)
	if creator == "" {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	kept := s.order[:0]
	for _, id := range s.order {
		k, ok := s.keys[id]
		if !ok {
			continue
		}
		if k.Creator == creator {
			delete(s.keys, id)
			delete(s.byHash, k.Hash)
			removed++
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	if removed == 0 {
		return 0, nil
	}
	s.dirty = true
	if err := s.save(); err != nil {
		return removed, err
	}
	return removed, nil
}

// Prune 删除已过期超过 grace 的密钥，返回删除数量。
func (s *Store) Prune(grace time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := s.pruneLocked(time.Now(), grace)
	if removed > 0 {
		_ = s.save()
	}
	return removed
}

// pruneLocked 是 Prune 的无锁版本（调用方需持有写锁）。返回删除数量。
func (s *Store) pruneLocked(now time.Time, grace time.Duration) int {
	removed := 0
	kept := s.order[:0]
	for _, id := range s.order {
		k, ok := s.keys[id]
		if !ok {
			continue
		}
		if !k.ExpiresAt.IsZero() && now.After(k.ExpiresAt.Add(grace)) {
			delete(s.keys, id)
			delete(s.byHash, k.Hash)
			removed++
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	if removed > 0 {
		s.dirty = true
	}
	return removed
}

// countByCreatorLocked 统计某个创建者名下的密钥数。调用方需持有写锁。
func (s *Store) countByCreatorLocked(creator string) int {
	n := 0
	for _, k := range s.keys {
		if k.Creator == creator {
			n++
		}
	}
	return n
}

// Count 返回密钥总数。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// Flush 把未落盘的改动写出。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.save()
}

// AllowsPath 判断密钥是否覆盖目标路径。
//
// 比较在归一化后的路径上进行，避免 //foo、/foo/ 这类写法绕过前缀判断。
func (k *Key) AllowsPath(urlPath string) bool {
	scope := k.Scope
	if scope == "" || scope == "/" {
		return true
	}
	clean := normalizePath(urlPath)
	return clean == scope || strings.HasPrefix(clean, scope+"/")
}

// normalizePath 归一化 URL 路径。
//
// ⚠️ 不把 `\` 当分隔符：Unix 上它是合法的文件名字符，而 fsutil.Resolve
// 也不会转换它。scope 检查若把 `/uploads\evil` 当成 `/uploads/evil`，
// 就会放行一次实际写到 `/uploads\evil`（scope 之外）的请求。
// Windows 上 Resolve 会把它当分隔符，那时这里只会更严格（误拒），不会放行。
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// 折叠重复斜杠与 . / .. 段。
	segs := make([]string, 0, 8)
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, seg)
		}
	}
	return "/" + strings.Join(segs, "/")
}

// generateToken 生成明文密钥。
func generateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机密钥失败: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// randomID 生成密钥的内部标识。
func randomID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成密钥 ID 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// hashToken 计算密钥摘要。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// visiblePrefix 取明文的一段用于人工识别。
// 前缀取的是固定长度，不足以反推密钥本体。
func visiblePrefix(token string) string {
	s := token
	if strings.HasPrefix(s, TokenPrefix) {
		s = s[len(TokenPrefix):]
	}
	if len(s) > 8 {
		s = s[:8]
	}
	return TokenPrefix + s
}
