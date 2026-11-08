// 优化 4-8: 后端 API 实现 (handler_public_dirs.go)

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// publicDirRequest 是公开目录操作的请求体
type publicDirRequest struct {
	Paths []string `json:"paths"`
}

// publicDirValidation 验证结果
type publicDirValidation struct {
	Valid  bool   `json:"valid"`
	Error  string `json:"error,omitempty"`
	FileCount int64 `json:"file_count,omitempty"`
	TotalSize int64 `json:"total_size,omitempty"`
}

// directoryInfo 目录详细信息
type directoryInfo struct {
	Path        string `json:"path"`
	FileCount   int64  `json:"file_count"`
	TotalSize   int64  `json:"total_size"`
	Permissions string `json:"permissions"`
	Mtime       int64  `json:"mtime"`
}

// ============================================================================
// 优化 4: 验证公开目录路径
// ============================================================================

func (s *Server) handleValidatePublicDirs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req publicDirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	result := make(map[string]publicDirValidation)

	for _, path := range req.Paths {
		validation := publicDirValidation{}

		// 检查路径是否为空
		if path == "" {
			validation.Valid = false
			validation.Error = "路径不能为空"
			result[path] = validation
			continue
		}

		// 检查路径是否以 / 开头
		if !strings.HasPrefix(path, "/") {
			validation.Valid = false
			validation.Error = "路径必须以 / 开头"
			result[path] = validation
			continue
		}

		// 获取实际路径
		abs := filepath.Join(s.res.Root(), path)

		// 检查路径是否在根目录内
		root, _ := filepath.Abs(s.res.Root())
		if !strings.HasPrefix(abs, root) {
			validation.Valid = false
			validation.Error = "路径超出服务范围"
			result[path] = validation
			continue
		}

		// 检查路径是否存在
		fi, err := os.Stat(abs)
		if err != nil {
			validation.Valid = false
			validation.Error = "路径不存在"
			result[path] = validation
			continue
		}

		// 检查是否为目录
		if !fi.IsDir() {
			validation.Valid = false
			validation.Error = "路径不是目录"
			result[path] = validation
			continue
		}

		// 检查读权限
		if err := filepath.Walk(abs, func(p string, fi os.FileInfo, err error) error {
			return err
		}); err != nil {
			validation.Valid = false
			validation.Error = "无权限访问"
			result[path] = validation
			return
		}

		// 计算文件统计
		fileCount, totalSize := s.countDirStats(abs)

		validation.Valid = true
		validation.FileCount = fileCount
		validation.TotalSize = totalSize

		result[path] = validation
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ============================================================================
// 优化 5: 获取目录建议
// ============================================================================

func (s *Server) handleSuggestPublicDirs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]string{})
		return
	}

	// 获取建议列表
	suggestions := s.suggestDirectories(query, s.res.Root())
	if suggestions == nil {
		suggestions = []string{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(suggestions)
}

func (s *Server) suggestDirectories(prefix string, root string) []string {
	var suggestions []string
	maxSuggestions := 10

	// 如果前缀为 /，返回根目录下的所有目录
	if prefix == "/" {
		entries, err := os.ReadDir(root)
		if err != nil {
			return suggestions
		}

		for _, entry := range entries {
			if entry.IsDir() && len(suggestions) < maxSuggestions {
				suggestions = append(suggestions, "/"+entry.Name())
			}
		}
		return suggestions
	}

	// 查找前缀匹配的目录
	dir := filepath.Dir(prefix)
	base := filepath.Base(prefix)

	absDir := filepath.Join(root, dir)
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return suggestions
	}

	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), base) {
			path := filepath.Join(dir, entry.Name())
			suggestions = append(suggestions, path)
			if len(suggestions) >= maxSuggestions {
				break
			}
		}
	}

	sort.Strings(suggestions)
	return suggestions
}

// ============================================================================
// 优化 6: 获取目录详细信息
// ============================================================================

func (s *Server) handleGetDirDetails(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "Missing path parameter", http.StatusBadRequest)
		return
	}

	abs := filepath.Join(s.res.Root(), path)
	root, _ := filepath.Abs(s.res.Root())

	// 安全检查
	if !strings.HasPrefix(abs, root) {
		http.Error(w, "Path out of bounds", http.StatusForbidden)
		return
	}

	fi, err := os.Stat(abs)
	if err != nil {
		http.Error(w, "Path not found", http.StatusNotFound)
		return
	}

	if !fi.IsDir() {
		http.Error(w, "Path is not a directory", http.StatusBadRequest)
		return
	}

	// 计算统计信息
	fileCount, totalSize := s.countDirStats(abs)

	info := directoryInfo{
		Path:        path,
		FileCount:   fileCount,
		TotalSize:   totalSize,
		Permissions: fi.Mode().String(),
		Mtime:       fi.ModTime().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// ============================================================================
// 优化 7: 批量导入公开目录
// ============================================================================

func (s *Server) handleImportPublicDirs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req publicDirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	currentDirs := s.settings.PublicDirs()
	imported := []string{}
	failed := []string{}

	for _, path := range req.Paths {
		// 验证路径
		abs := filepath.Join(s.res.Root(), path)
		root, _ := filepath.Abs(s.res.Root())

		if !strings.HasPrefix(abs, root) {
			failed = append(failed, path)
			continue
		}

		if _, err := os.Stat(abs); err != nil {
			failed = append(failed, path)
			continue
		}

		// 检查是否已存在
		exists := false
		for _, existing := range currentDirs {
			if existing == path {
				exists = true
				break
			}
		}

		if !exists {
			currentDirs = append(currentDirs, path)
			imported = append(imported, path)
		}
	}

	// 保存更新
	if err := s.settings.SetPublicDirs(currentDirs); err != nil {
		http.Error(w, "Failed to save", http.StatusInternalServerError)
		return
	}

	result := map[string]interface{}{
		"success":  len(failed) == 0,
		"count":    len(imported),
		"imported": imported,
		"failed":   failed,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ============================================================================
// 优化 8: 更新公开目录顺序
// ============================================================================

func (s *Server) handleUpdateDirOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req publicDirRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// 验证所有路径都存在
	currentDirs := s.settings.PublicDirs()
	pathMap := make(map[string]bool)
	for _, dir := range currentDirs {
		pathMap[dir] = true
	}

	for _, path := range req.Paths {
		if !pathMap[path] {
			http.Error(w, "Invalid path in order", http.StatusBadRequest)
			return
		}
	}

	// 保存新顺序
	if err := s.settings.SetPublicDirs(req.Paths); err != nil {
		http.Error(w, "Failed to save", http.StatusInternalServerError)
		return
	}

	result := map[string]interface{}{
		"success": true,
		"paths":   req.Paths,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// ============================================================================
// 辅助函数
// ============================================================================

func (s *Server) countDirStats(dir string) (fileCount int64, totalSize int64) {
	filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !fi.IsDir() {
			fileCount++
			totalSize += fi.Size()
		}
		return nil
	})
	return
}

// handleCacheStats 缓存统计端点
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := map[string]interface{}{
		"dir_stats_cache": map[string]interface{}{
			"size":     0,
			"max_size": 1000,
			"ttl":      "5m0s",
		},
		"suggest_cache": map[string]interface{}{
			"size":     0,
			"max_size": 500,
			"ttl":      "10m0s",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}
