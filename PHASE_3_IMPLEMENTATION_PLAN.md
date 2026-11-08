# 第三阶段实现计划 - 后端 API 优化和缓存 (v0.1.25)

**启动日期**: 2026-11-09  
**预计完成**: 1-2 周  
**优化范围**: 优化 9-12 (4 个功能)

---

## 🎯 第三阶段概览

### 优化 9: 新增详细信息 API (已在第二阶段实现 ✅)

**状态**: 已完成  
**文件**: `internal/server/handler_public_dirs.go`  
**端点**: `GET /fs/settings/public-dirs/details?path=...`

功能已在第二阶段的 `handleGetDirDetails()` 中实现。

---

### 优化 10: 路径验证 API (已在第二阶段实现 ✅)

**状态**: 已完成  
**文件**: `internal/server/handler_public_dirs.go`  
**端点**: `POST /fs/settings/public-dirs/validate`

功能已在第二阶段的 `handleValidatePublicDirs()` 中实现。

---

### 优化 11: 批量导入 API (已在第二阶段实现 ✅)

**状态**: 已完成  
**文件**: `internal/server/handler_public_dirs.go`  
**端点**: `POST /fs/settings/public-dirs/import`

功能已在第二阶段的 `handleImportPublicDirs()` 中实现。

---

### 优化 12: 缓存优化 (新增 🆕)

**功能描述**: 添加缓存层以减少文件系统操作，提升性能。

**实现重点**:
1. 内存缓存目录统计信息
2. TTL (生存时间) 机制
3. 缓存失效策略
4. LRU (最近最少使用) 淘汰

**关键代码**:

```go
package server

import (
	"sync"
	"time"
)

// CacheEntry 缓存条目
type CacheEntry struct {
	Value      interface{}
	ExpireTime time.Time
}

// DirStatsCache 目录统计缓存
type DirStatsCache struct {
	mu    sync.RWMutex
	cache map[string]CacheEntry
	ttl   time.Duration
	maxSize int
}

// NewDirStatsCache 创建缓存实例
func NewDirStatsCache(ttl time.Duration, maxSize int) *DirStatsCache {
	cache := &DirStatsCache{
		cache:   make(map[string]CacheEntry),
		ttl:     ttl,
		maxSize: maxSize,
	}
	
	// 启动定期清理
	go cache.cleanupExpired()
	
	return cache
}

// Get 获取缓存值
func (c *DirStatsCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	entry, exists := c.cache[key]
	if !exists {
		return nil, false
	}
	
	// 检查是否过期
	if time.Now().After(entry.ExpireTime) {
		return nil, false
	}
	
	return entry.Value, true
}

// Set 设置缓存值
func (c *DirStatsCache) Set(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// 检查大小限制
	if len(c.cache) >= c.maxSize {
		c.evictOldest()
	}
	
	c.cache[key] = CacheEntry{
		Value:      value,
		ExpireTime: time.Now().Add(c.ttl),
	}
}

// Invalidate 使缓存失效
func (c *DirStatsCache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, key)
}

// InvalidatePrefix 使前缀匹配的缓存失效
func (c *DirStatsCache) InvalidatePrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	for k := range c.cache {
		if strings.HasPrefix(k, prefix) {
			delete(c.cache, k)
		}
	}
}

// Clear 清空所有缓存
func (c *DirStatsCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make(map[string]CacheEntry)
}

// evictOldest LRU 淘汰最旧的条目
func (c *DirStatsCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	
	first := true
	for k, v := range c.cache {
		if first || v.ExpireTime.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.ExpireTime
			first = false
		}
	}
	
	if oldestKey != "" {
		delete(c.cache, oldestKey)
	}
}

// cleanupExpired 定期清理过期条目
func (c *DirStatsCache) cleanupExpired() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		c.mu.Lock()
		now := time.Now()
		for k, v := range c.cache {
			if now.After(v.ExpireTime) {
				delete(c.cache, k)
			}
		}
		c.mu.Unlock()
	}
}

// Stats 获取缓存统计
func (c *DirStatsCache) Stats() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	return map[string]interface{}{
		"size":     len(c.cache),
		"max_size": c.maxSize,
		"ttl":      c.ttl.String(),
	}
}
```

**使用示例**:

```go
// 在 Server 结构体中添加
type Server struct {
	// ... 现有字段
	dirStatsCache *DirStatsCache
}

// 初始化
func NewServer(...) *Server {
	s := &Server{
		// ...
		dirStatsCache: NewDirStatsCache(5*time.Minute, 1000),
	}
	return s
}

// 在 countDirStats 中使用缓存
func (s *Server) countDirStats(dir string) (fileCount int64, totalSize int64) {
	// 尝试从缓存获取
	if cached, ok := s.dirStatsCache.Get(dir); ok {
		stats := cached.(map[string]int64)
		return stats["file_count"], stats["total_size"]
	}
	
	// 计算统计
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
	
	// 存入缓存
	s.dirStatsCache.Set(dir, map[string]int64{
		"file_count": fileCount,
		"total_size": totalSize,
	})
	
	return
}

// 在文件修改时使缓存失效
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	// ... 上传处理 ...
	
	// 使缓存失效
	uploadDir := filepath.Dir(uploadPath)
	s.dirStatsCache.Invalidate(uploadDir)
}
```

---

## 📊 第三阶段完成情况

### 实现状态

| 优化 | 功能 | 状态 | 说明 |
|------|------|------|------|
| 优化 9 | 详细信息 API | ✅ 已完成 | 第二阶段实现 |
| 优化 10 | 路径验证 API | ✅ 已完成 | 第二阶段实现 |
| 优化 11 | 批量导入 API | ✅ 已完成 | 第二阶段实现 |
| 优化 12 | 缓存优化 | 🆕 本阶段新增 | 需集成到现有代码 |

---

## 🚀 优化 12 的集成步骤

### 步骤 1: 创建缓存文件

创建 `internal/server/cache.go`:

```go
// 完整的缓存实现代码
// (见上文)
```

### 步骤 2: 更新 Server 结构

在 `internal/server/server.go` 中:

```go
type Server struct {
	// ... 现有字段
	dirStatsCache *DirStatsCache
	suggestCache  *DirStatsCache // 建议缓存
}
```

### 步骤 3: 初始化缓存

```go
func NewServer(...) *Server {
	s := &Server{
		// ...
		dirStatsCache: NewDirStatsCache(5*time.Minute, 1000),
		suggestCache:  NewDirStatsCache(10*time.Minute, 500),
	}
	return s
}
```

### 步骤 4: 在 API 中使用缓存

更新 `handler_public_dirs.go` 中的函数以使用缓存。

### 步骤 5: 缓存失效处理

在文件操作 (上传、删除、编辑) 后使缓存失效:

```go
func (s *Server) handleFileOperation(...) {
	// ... 操作代码 ...
	
	// 使相关缓存失效
	dir := filepath.Dir(path)
	s.dirStatsCache.Invalidate(dir)
	s.dirStatsCache.InvalidatePrefix(dir + "/")
}
```

---

## 📈 性能优化效果

### 预期性能提升

| 操作 | 无缓存 | 有缓存 | 提升 |
|------|--------|--------|------|
| 目录统计 | 500ms | 5ms | 100x |
| 建议查询 | 200ms | 10ms | 20x |
| 详情获取 | 300ms | 5ms | 60x |

### 内存使用

```
缓存配置:
- 目录统计缓存: 1000 条 × 100 字节 = 100 KB
- 建议缓存: 500 条 × 50 字节 = 25 KB
总计: ~125 KB (可忽略不计)
```

---

## 🔍 缓存监控

### 添加监控端点

```go
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	stats := map[string]interface{}{
		"dir_stats_cache": s.dirStatsCache.Stats(),
		"suggest_cache":   s.suggestCache.Stats(),
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// 注册路由
s.mux.HandleFunc("/fs/cache/stats", s.handleCacheStats)
```

### 查看缓存统计

```bash
curl http://127.0.0.1:5000/fs/cache/stats
```

响应:
```json
{
  "dir_stats_cache": {
    "size": 42,
    "max_size": 1000,
    "ttl": "5m0s"
  },
  "suggest_cache": {
    "size": 15,
    "max_size": 500,
    "ttl": "10m0s"
  }
}
```

---

## 📋 测试清单

- [ ] 缓存正确存储和获取值
- [ ] 过期条目自动清理
- [ ] LRU 淘汰机制工作
- [ ] 缓存失效正确触发
- [ ] 前缀失效正确工作
- [ ] 并发访问线程安全
- [ ] 性能提升符合预期
- [ ] 内存使用在预期范围内

---

## 📊 第三阶段总结

### 成果

- ✅ 优化 9-11: 已在第二阶段完成
- ✅ 优化 12: 完整的缓存实现和集成指南

### 代码行数

- 缓存实现: 200+ 行
- 集成代码: 50+ 行
- 文档: 本文件

### 下一步

- 第四阶段 (优化 13-16): 功能增强
- 第五阶段 (优化 17-19): 文档和国际化
- 第六阶段 (优化 20-21): 性能和安全

---

**版本**: v0.1.25  
**状态**: 设计完成，待集成

