package webdav

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Storage 处理账号数据的持久化。
type Storage struct {
	mu       sync.RWMutex
	filePath string
}

// accountData 用于序列化的账号数据结构。
type accountData struct {
	Username     string   `json:"username"`
	PasswordHash string   `json:"password_hash"`
	AllowedDirs  []string `json:"allowed_dirs"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
}

type storageData struct {
	Version  int            `json:"version"`
	Accounts []*accountData `json:"accounts"`
}

// NewStorage 创建一个新的存储实例。
func NewStorage(filePath string) *Storage {
	return &Storage{
		filePath: filePath,
	}
}

// Save 保存账号列表到文件。
func (s *Storage) Save(accounts map[string]*Account) error {
	if s.filePath == "" {
		return nil // 未配置文件路径，跳过保存
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 准备数据
	accountList := make([]*accountData, 0, len(accounts))
	for _, acc := range accounts {
		accountList = append(accountList, &accountData{
			Username:     acc.Username,
			PasswordHash: acc.PasswordHash,
			AllowedDirs:  acc.AllowedDirs,
			CreatedAt:    acc.CreatedAt,
			UpdatedAt:    acc.UpdatedAt,
		})
	}

	data := &storageData{
		Version:  1,
		Accounts: accountList,
	}

	// 序列化为 JSON
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}

	// 确保目录存在
	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	// 写入临时文件
	tempFile := s.filePath + ".tmp"
	if err := os.WriteFile(tempFile, jsonData, 0o600); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}

	// 原子性重命名
	if err := os.Rename(tempFile, s.filePath); err != nil {
		os.Remove(tempFile) // 清理临时文件
		return fmt.Errorf("重命名失败: %w", err)
	}

	return nil
}

// Load 从文件加载账号列表。
func (s *Storage) Load() (map[string]*Account, error) {
	if s.filePath == "" {
		return make(map[string]*Account), nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// 检查文件是否存在
	if _, err := os.Stat(s.filePath); os.IsNotExist(err) {
		return make(map[string]*Account), nil
	}

	// 读取文件
	jsonData, err := os.ReadFile(s.filePath)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}

	// 解析 JSON
	var data storageData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}

	// 转换为 map
	accounts := make(map[string]*Account)
	for _, acc := range data.Accounts {
		accounts[acc.Username] = &Account{
			Username:     acc.Username,
			PasswordHash: acc.PasswordHash,
			AllowedDirs:  acc.AllowedDirs,
			CreatedAt:    acc.CreatedAt,
			UpdatedAt:    acc.UpdatedAt,
		}
	}

	return accounts, nil
}
