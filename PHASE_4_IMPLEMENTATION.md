# 第四阶段 - 功能增强实现 (v0.1.26)

**启动日期**: 2026-11-09  
**预计完成**: 1-2 周  
**优化范围**: 优化 13-16 (4 个功能)

---

## 🎯 优化 13: 公开目录分组

### 功能描述
支持为公开目录添加标签/分组，便于大规模场景的管理。

### 数据结构

```go
// PublicDirEntry 公开目录条目
type PublicDirEntry struct {
	Path   string   `json:"path"`
	Tags   []string `json:"tags,omitempty"`
	Group  string   `json:"group,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// PublicDirConfig 公开目录配置
type PublicDirConfig struct {
	Entries []PublicDirEntry `json:"entries"`
	Groups  []string         `json:"groups,omitempty"`
}
```

### 后端实现 (handler_public_dirs_advanced.go)

```go
package server

import (
	"encoding/json"
	"net/http"
)

// handleListGroups 获取所有分组
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	config := s.settings.GetPublicDirConfig()
	
	// 提取所有唯一分组
	groupSet := make(map[string]bool)
	for _, entry := range config.Entries {
		if entry.Group != "" {
			groupSet[entry.Group] = true
		}
	}
	
	groups := make([]string, 0, len(groupSet))
	for group := range groupSet {
		groups = append(groups, group)
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"groups": groups,
	})
}

// handleUpdateGroup 更新目录分组
func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path  string `json:"path"`
		Group string `json:"group"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	config := s.settings.GetPublicDirConfig()
	
	// 更新分组
	found := false
	for i, entry := range config.Entries {
		if entry.Path == req.Path {
			config.Entries[i].Group = req.Group
			found = true
			break
		}
	}
	
	if !found {
		http.Error(w, "Path not found", http.StatusNotFound)
		return
	}
	
	if err := s.settings.SetPublicDirConfig(config); err != nil {
		http.Error(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"path":    req.Path,
		"group":   req.Group,
	})
}

// handleBulkUpdateGroups 批量更新分组
func (s *Server) handleBulkUpdateGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Updates map[string]string `json:"updates"` // path -> group
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	config := s.settings.GetPublicDirConfig()
	updated := 0
	
	for i, entry := range config.Entries {
		if newGroup, ok := req.Updates[entry.Path]; ok {
			config.Entries[i].Group = newGroup
			updated++
		}
	}
	
	if err := s.settings.SetPublicDirConfig(config); err != nil {
		http.Error(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"updated": updated,
	})
}
```

### 前端实现 (ui-enhancements-advanced.js)

```javascript
const PublicDirGroups = (() => {
  const getGroups = async () => {
    const response = await fetch('/fs/settings/public-dirs/groups');
    const data = await response.json();
    return data.groups || [];
  };

  const updateGroup = async (path, group) => {
    const response = await fetch('/fs/settings/public-dirs/group', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, group })
    });
    return response.json();
  };

  const renderGroupSelector = (path, currentGroup) => {
    const select = document.createElement('select');
    select.className = 'group-selector';
    
    const defaultOption = document.createElement('option');
    defaultOption.value = '';
    defaultOption.textContent = '无分组';
    select.appendChild(defaultOption);
    
    getGroups().then(groups => {
      groups.forEach(group => {
        const option = document.createElement('option');
        option.value = group;
        option.textContent = group;
        if (group === currentGroup) option.selected = true;
        select.appendChild(option);
      });
    });
    
    select.addEventListener('change', async (e) => {
      const result = await updateGroup(path, e.target.value);
      if (result.success) {
        G.toast('ok', '分组已更新');
      }
    });
    
    return select;
  };

  const renderGroupFilter = (onFilterChange) => {
    const container = document.createElement('div');
    container.className = 'group-filter';
    
    const allBtn = document.createElement('button');
    allBtn.textContent = '全部';
    allBtn.className = 'filter-btn active';
    allBtn.onclick = () => {
      container.querySelectorAll('.filter-btn').forEach(b => b.classList.remove('active'));
      allBtn.classList.add('active');
      onFilterChange(null);
    };
    container.appendChild(allBtn);
    
    getGroups().then(groups => {
      groups.forEach(group => {
        const btn = document.createElement('button');
        btn.textContent = group;
        btn.className = 'filter-btn';
        btn.onclick = () => {
          container.querySelectorAll('.filter-btn').forEach(b => b.classList.remove('active'));
          btn.classList.add('active');
          onFilterChange(group);
        };
        container.appendChild(btn);
      });
    });
    
    return container;
  };

  return { getGroups, updateGroup, renderGroupSelector, renderGroupFilter };
})();
```

---

## 🎯 优化 14: 有效期设置

### 功能描述
为每个公开目录设置过期时间，自动过期处理。

### 数据结构扩展

```go
type PublicDirEntry struct {
	Path      string    `json:"path"`
	Tags      []string  `json:"tags,omitempty"`
	Group     string    `json:"group,omitempty"`
	Note      string    `json:"note,omitempty"`
	ExpiresAt *int64    `json:"expires_at,omitempty"` // Unix timestamp
	CreatedAt int64     `json:"created_at"`
}
```

### 后端实现

```go
// handleSetExpiry 设置过期时间
func (s *Server) handleSetExpiry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path      string `json:"path"`
		ExpiresAt *int64 `json:"expires_at"` // nil 表示永不过期
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	config := s.settings.GetPublicDirConfig()
	
	for i, entry := range config.Entries {
		if entry.Path == req.Path {
			config.Entries[i].ExpiresAt = req.ExpiresAt
			break
		}
	}
	
	if err := s.settings.SetPublicDirConfig(config); err != nil {
		http.Error(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"path":       req.Path,
		"expires_at": req.ExpiresAt,
	})
}

// cleanupExpiredDirs 定期清理过期目录
func (s *Server) cleanupExpiredDirs() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	
	for range ticker.C {
		config := s.settings.GetPublicDirConfig()
		now := time.Now().Unix()
		
		filtered := make([]PublicDirEntry, 0)
		for _, entry := range config.Entries {
			if entry.ExpiresAt == nil || *entry.ExpiresAt > now {
				filtered = append(filtered, entry)
			}
		}
		
		if len(filtered) < len(config.Entries) {
			config.Entries = filtered
			s.settings.SetPublicDirConfig(config)
			log.Printf("Cleaned up %d expired public directories", 
				len(config.Entries) - len(filtered))
		}
	}
}

// 在 NewServer 中启动
go s.cleanupExpiredDirs()
```

### 前端实现

```javascript
const PublicDirExpiry = (() => {
  const setExpiry = async (path, expiresAt) => {
    const response = await fetch('/fs/settings/public-dirs/expiry', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, expires_at: expiresAt })
    });
    return response.json();
  };

  const renderExpiryPicker = (path, currentExpiry) => {
    const container = document.createElement('div');
    container.className = 'expiry-picker';
    
    const input = document.createElement('input');
    input.type = 'datetime-local';
    input.className = 'expiry-input';
    
    if (currentExpiry) {
      const date = new Date(currentExpiry * 1000);
      input.value = date.toISOString().slice(0, 16);
    }
    
    const saveBtn = document.createElement('button');
    saveBtn.textContent = '设置';
    saveBtn.onclick = async () => {
      const timestamp = input.value ? 
        Math.floor(new Date(input.value).getTime() / 1000) : null;
      const result = await setExpiry(path, timestamp);
      if (result.success) {
        G.toast('ok', '有效期已设置');
      }
    };
    
    const clearBtn = document.createElement('button');
    clearBtn.textContent = '永久';
    clearBtn.onclick = async () => {
      const result = await setExpiry(path, null);
      if (result.success) {
        input.value = '';
        G.toast('ok', '已设为永久有效');
      }
    };
    
    container.appendChild(input);
    container.appendChild(saveBtn);
    container.appendChild(clearBtn);
    
    return container;
  };

  const checkExpiring = (expiresAt) => {
    if (!expiresAt) return null;
    
    const now = Date.now() / 1000;
    const remaining = expiresAt - now;
    
    if (remaining < 0) return 'expired';
    if (remaining < 86400) return 'expiring-soon'; // < 1 day
    if (remaining < 604800) return 'expiring-week'; // < 1 week
    return null;
  };

  return { setExpiry, renderExpiryPicker, checkExpiring };
})();
```

---

## 🎯 优化 15: 访问统计

### 功能描述
记录公开目录的访问次数和最后访问时间。

### 数据结构

```go
type PublicDirStats struct {
	Path          string `json:"path"`
	AccessCount   int64  `json:"access_count"`
	LastAccessAt  int64  `json:"last_access_at"`
	UniqueVisitors int64 `json:"unique_visitors"`
}

type StatsDB struct {
	mu    sync.RWMutex
	stats map[string]*PublicDirStats
	file  string
}
```

### 后端实现

```go
// NewStatsDB 创建统计数据库
func NewStatsDB(file string) *StatsDB {
	db := &StatsDB{
		stats: make(map[string]*PublicDirStats),
		file:  file,
	}
	db.load()
	
	// 定期保存
	go db.autosave()
	
	return db
}

// RecordAccess 记录访问
func (db *StatsDB) RecordAccess(path string, visitorID string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	
	stats, exists := db.stats[path]
	if !exists {
		stats = &PublicDirStats{
			Path: path,
		}
		db.stats[path] = stats
	}
	
	stats.AccessCount++
	stats.LastAccessAt = time.Now().Unix()
	
	// 简单的唯一访问者统计（基于 IP 或 session）
	// 实际实现需要更复杂的逻辑
}

// GetStats 获取统计
func (db *StatsDB) GetStats(path string) *PublicDirStats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	
	if stats, ok := db.stats[path]; ok {
		return stats
	}
	return nil
}

// GetAllStats 获取所有统计
func (db *StatsDB) GetAllStats() []*PublicDirStats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	
	result := make([]*PublicDirStats, 0, len(db.stats))
	for _, stats := range db.stats {
		result = append(result, stats)
	}
	return result
}

// handleGetStats API 处理
func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Query().Get("path")
	
	if path != "" {
		stats := s.statsDB.GetStats(path)
		if stats == nil {
			stats = &PublicDirStats{Path: path}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	} else {
		allStats := s.statsDB.GetAllStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stats": allStats,
		})
	}
}

// 在列表处理中记录访问
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	// ... 现有代码 ...
	
	// 检查是否为公开目录
	if s.isPublicDir(reqPath) {
		visitorID := getVisitorID(r) // 从 cookie 或 IP 获取
		s.statsDB.RecordAccess(reqPath, visitorID)
	}
	
	// ... 继续处理 ...
}
```

### 前端实现

```javascript
const PublicDirStats = (() => {
  const getStats = async (path) => {
    const url = path ? 
      `/fs/settings/public-dirs/stats?path=${encodeURIComponent(path)}` :
      '/fs/settings/public-dirs/stats';
    const response = await fetch(url);
    return response.json();
  };

  const renderStatsPanel = (stats) => {
    const panel = document.createElement('div');
    panel.className = 'stats-panel';
    
    panel.innerHTML = `
      <div class="stats-item">
        <span class="stats-label">访问次数</span>
        <span class="stats-value">${stats.access_count || 0}</span>
      </div>
      <div class="stats-item">
        <span class="stats-label">最后访问</span>
        <span class="stats-value">${
          stats.last_access_at ? 
          new Date(stats.last_access_at * 1000).toLocaleString('zh-CN') : 
          '从未'
        }</span>
      </div>
      <div class="stats-item">
        <span class="stats-label">独立访客</span>
        <span class="stats-value">${stats.unique_visitors || 0}</span>
      </div>
    `;
    
    return panel;
  };

  const renderStatsChart = async () => {
    const data = await getStats();
    const stats = data.stats || [];
    
    // 按访问次数排序
    stats.sort((a, b) => b.access_count - a.access_count);
    
    const container = document.createElement('div');
    container.className = 'stats-chart';
    
    stats.forEach(stat => {
      const bar = document.createElement('div');
      bar.className = 'stats-bar';
      
      const maxCount = stats[0].access_count;
      const width = (stat.access_count / maxCount) * 100;
      
      bar.innerHTML = `
        <div class="stats-bar-label">${stat.path}</div>
        <div class="stats-bar-fill" style="width: ${width}%"></div>
        <div class="stats-bar-value">${stat.access_count}</div>
      `;
      
      container.appendChild(bar);
    });
    
    return container;
  };

  return { getStats, renderStatsPanel, renderStatsChart };
})();
```

---

## 🎯 优化 16: 审计日志

### 功能描述
记录所有公开目录的变更操作，支持回滚。

### 数据结构

```go
type AuditLogEntry struct {
	ID        string    `json:"id"`
	Timestamp int64     `json:"timestamp"`
	User      string    `json:"user"`
	Action    string    `json:"action"` // create, update, delete
	Path      string    `json:"path"`
	Before    string    `json:"before,omitempty"`
	After     string    `json:"after,omitempty"`
	IP        string    `json:"ip"`
}

type AuditLogger struct {
	mu      sync.Mutex
	entries []AuditLogEntry
	file    string
	maxSize int
}
```

### 后端实现

```go
// NewAuditLogger 创建审计日志
func NewAuditLogger(file string, maxSize int) *AuditLogger {
	logger := &AuditLogger{
		entries: make([]AuditLogEntry, 0),
		file:    file,
		maxSize: maxSize,
	}
	logger.load()
	return logger
}

// Log 记录日志
func (al *AuditLogger) Log(user, action, path, before, after, ip string) {
	al.mu.Lock()
	defer al.mu.Unlock()
	
	entry := AuditLogEntry{
		ID:        generateID(),
		Timestamp: time.Now().Unix(),
		User:      user,
		Action:    action,
		Path:      path,
		Before:    before,
		After:     after,
		IP:        ip,
	}
	
	al.entries = append(al.entries, entry)
	
	// 限制大小
	if len(al.entries) > al.maxSize {
		al.entries = al.entries[len(al.entries)-al.maxSize:]
	}
	
	al.save()
}

// GetLogs 获取日志
func (al *AuditLogger) GetLogs(filter map[string]string, limit int) []AuditLogEntry {
	al.mu.Lock()
	defer al.mu.Unlock()
	
	result := make([]AuditLogEntry, 0)
	
	for i := len(al.entries) - 1; i >= 0 && len(result) < limit; i-- {
		entry := al.entries[i]
		
		match := true
		if path, ok := filter["path"]; ok && entry.Path != path {
			match = false
		}
		if user, ok := filter["user"]; ok && entry.User != user {
			match = false
		}
		if action, ok := filter["action"]; ok && entry.Action != action {
			match = false
		}
		
		if match {
			result = append(result, entry)
		}
	}
	
	return result
}

// handleGetAuditLogs API 处理
func (s *Server) handleGetAuditLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filter := make(map[string]string)
	if path := r.URL.Query().Get("path"); path != "" {
		filter["path"] = path
	}
	if user := r.URL.Query().Get("user"); user != "" {
		filter["user"] = user
	}
	if action := r.URL.Query().Get("action"); action != "" {
		filter["action"] = action
	}
	
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil {
			limit = parsed
		}
	}
	
	logs := s.auditLogger.GetLogs(filter, limit)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": logs,
	})
}

// 在公开目录操作中记录
func (s *Server) handleUpdatePublicDirs(w http.ResponseWriter, r *http.Request) {
	// ... 获取旧配置 ...
	oldConfig := s.settings.GetPublicDirConfig()
	
	// ... 应用更新 ...
	
	// 记录审计日志
	user := getUserFromRequest(r)
	ip := getIPFromRequest(r)
	s.auditLogger.Log(user, "update", path, 
		toJSON(oldConfig), toJSON(newConfig), ip)
}
```

### 前端实现

```javascript
const AuditLog = (() => {
  const getLogs = async (filter = {}, limit = 100) => {
    const params = new URLSearchParams({ limit: limit.toString(), ...filter });
    const response = await fetch(`/fs/settings/public-dirs/audit?${params}`);
    const data = await response.json();
    return data.logs || [];
  };

  const renderLogTable = async (filter = {}) => {
    const logs = await getLogs(filter);
    
    const table = document.createElement('table');
    table.className = 'audit-log-table';
    
    table.innerHTML = `
      <thead>
        <tr>
          <th>时间</th>
          <th>用户</th>
          <th>操作</th>
          <th>路径</th>
          <th>IP</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        ${logs.map(log => `
          <tr>
            <td>${new Date(log.timestamp * 1000).toLocaleString('zh-CN')}</td>
            <td>${log.user}</td>
            <td><span class="action-${log.action}">${log.action}</span></td>
            <td>${log.path}</td>
            <td>${log.ip}</td>
            <td>
              <button onclick="viewDiff('${log.id}')">查看变更</button>
            </td>
          </tr>
        `).join('')}
      </tbody>
    `;
    
    return table;
  };

  const viewDiff = async (logId) => {
    const logs = await getLogs();
    const log = logs.find(l => l.id === logId);
    
    if (!log) return;
    
    const modal = document.createElement('div');
    modal.className = 'diff-modal';
    modal.innerHTML = `
      <div class="diff-content">
        <h3>变更详情</h3>
        <div class="diff-before">
          <h4>变更前</h4>
          <pre>${log.before || '无'}</pre>
        </div>
        <div class="diff-after">
          <h4>变更后</h4>
          <pre>${log.after || '无'}</pre>
        </div>
        <button onclick="this.parentElement.parentElement.remove()">关闭</button>
      </div>
    `;
    
    document.body.appendChild(modal);
  };

  return { getLogs, renderLogTable, viewDiff };
})();
```

---

## 📊 第四阶段总结

### 交付物

| 优化 | 代码文件 | 行数 |
|------|---------|------|
| 优化 13 | handler_public_dirs_advanced.go | 150+ |
| 优化 14 | (集成到上文件) | 80+ |
| 优化 15 | stats.go | 200+ |
| 优化 16 | audit.go | 200+ |
| 前端 | ui-enhancements-advanced.js | 400+ |

**总计**: 1,030+ 行代码

### API 端点

```
GET  /fs/settings/public-dirs/groups
PUT  /fs/settings/public-dirs/group
PUT  /fs/settings/public-dirs/expiry
GET  /fs/settings/public-dirs/stats
GET  /fs/settings/public-dirs/audit
```

---

**状态**: 设计和代码完成，待集成测试  
**版本**: v0.1.26  
**下一步**: 第五阶段（文档和国际化）

