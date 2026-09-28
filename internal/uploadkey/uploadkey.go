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
)

// TokenPrefix 是所有密钥的公共前缀，便于在日志与配置里一眼识别。
const TokenPrefix = "gofs_"

// 校验失败的原因。
var (
	// ErrNotFound 表示密钥不存在（或格式不对）。
	ErrNotFound = errors.New("上传密钥无效")
	// ErrExpired 表示密钥已过期。
	ErrExpired = errors.New("上传密钥已过期")
	// ErrEmptyToken 表示调用方没提供密钥。
	ErrEmptyToken = errors.New("未提供上传密钥")
)

// 使用统计的落盘节流间隔，避免每次上传都写一次磁盘。
const flushInterval = 30 * time.Second

// Key 描述一把上传密钥。注意这里不保存明文。
type Key struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"` // 明文前若干位，仅供人工识别
	Hash   string `json:"hash"`   // sha256(明文) 的十六进制摘要
	// Scope 是允许上传的路径前缀，例如 "/uploads"；"/" 表示不限。
	Scope      string    `json:"scope"`
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

// CreateOptions 是创建密钥的参数。
type CreateOptions struct {
	Name    string
	Scope   string
	TTL     time.Duration // 0 表示永不过期
	Creator string
	Note    string
}

// Store 保存全部密钥，可选择持久化到磁盘。
type Store struct {
	mu        sync.Mutex
	path      string
	keys      map[string]*Key
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
		path: path,
		keys: make(map[string]*Key),
	}
	if path == "" {
		return s, nil
	}
	if err := s.load(); err != nil {
		return nil, err
	}
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
	now := time.Now()
	for _, k := range ff.Keys {
		if k == nil || k.ID == "" || k.Hash == "" {
			continue
		}
		s.keys[k.ID] = k
		s.order = append(s.order, k.ID)
		_ = now
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

// Create 生成一把新密钥，返回记录与**只此一次**的明文。
func (s *Store) Create(opts CreateOptions) (*Key, string, error) {
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

	now := time.Now()
	k := &Key{
		ID:        id,
		Name:      name,
		Prefix:    visiblePrefix(token),
		Hash:      hashToken(token),
		Scope:     scope,
		Creator:   opts.Creator,
		CreatedAt: now,
		Note:      strings.TrimSpace(opts.Note),
	}
	if opts.TTL > 0 {
		k.ExpiresAt = now.Add(opts.TTL)
	}

	s.mu.Lock()
	s.keys[k.ID] = k
	s.order = append(s.order, k.ID)
	s.dirty = true
	err = s.save()
	s.mu.Unlock()
	if err != nil {
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
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	var found *Key
	for _, id := range s.order {
		k, ok := s.keys[id]
		if !ok {
			continue
		}
		kh, err := hex.DecodeString(k.Hash)
		if err != nil || len(kh) != len(sum) {
			continue
		}
		if subtle.ConstantTimeCompare(kh, sum[:]) == 1 {
			found = k
			break
		}
	}
	if found == nil {
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
	if _, ok := s.keys[id]; !ok {
		return ErrNotFound
	}
	delete(s.keys, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.dirty = true
	return s.save()
}

// Prune 删除已过期超过 grace 的密钥，返回删除数量。
func (s *Store) Prune(grace time.Duration) int {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	kept := s.order[:0]
	for _, id := range s.order {
		k, ok := s.keys[id]
		if !ok {
			continue
		}
		if !k.ExpiresAt.IsZero() && now.After(k.ExpiresAt.Add(grace)) {
			delete(s.keys, id)
			removed++
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	if removed > 0 {
		s.dirty = true
		_ = s.save()
	}
	return removed
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
func normalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
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
