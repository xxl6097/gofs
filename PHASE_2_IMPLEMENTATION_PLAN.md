# 第二阶段实现计划 - UI/UX 增强 (v0.1.24)

**启动日期**: 2026-11-09  
**预计完成**: 1-2 周  
**优化范围**: 优化 4-8 (5 个功能)

---

## 🎯 第二阶段概览

### 优化 4: 实时路径验证 (0.5-1 天)

**功能描述**: 用户输入公开目录路径时，实时验证路径是否存在、是否有权限访问。

**实现文件**: `assets/settings.js` + `internal/server/handler_list.go` (新增 API)

**关键代码位置**:
```javascript
// 在 buildGeneralTab 中添加输入验证
const validatePath = async (path) => {
  try {
    const response = await fetch('/fs/settings/public-dirs/validate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ paths: [path] })
    });
    const result = await response.json();
    return result[path]; // { valid: true/false, error?: string }
  } catch (err) {
    return { valid: false, error: err.message };
  }
};

// 输入框添加 input 事件监听
dirInput.addEventListener('input', debounce(async (e) => {
  const result = await validatePath(e.target.value);
  updateValidationUI(result);
}, 300));
```

**后端 API**: `POST /fs/settings/public-dirs/validate`
```go
// 返回结构
{
  "/path1": { "valid": true },
  "/path2": { "valid": false, "error": "路径不存在" }
}
```

**UI 效果**:
- ✅ 绿色勾号 (路径有效)
- ❌ 红色 X (路径无效)
- ⚠️ 黄色提示 (权限不足)

---

### 优化 5: 自动完成建议 (1 天)

**功能描述**: 用户输入时，基于文件系统显示匹配的目录建议列表。

**实现文件**: `assets/settings.js` + `internal/server/handler_list.go`

**关键代码位置**:
```javascript
// 获取目录建议
const getSuggestions = async (prefix) => {
  const response = await fetch(
    `/fs/settings/public-dirs/suggest?q=${encodeURIComponent(prefix)}`
  );
  return response.json(); // ['path/a', 'path/b', ...]
};

// 显示建议列表
const showSuggestions = async (input) => {
  if (input.length < 1) return;
  
  const suggestions = await getSuggestions(input);
  const suggestionList = document.createElement('ul');
  
  suggestions.forEach(path => {
    const li = document.createElement('li');
    li.textContent = path;
    li.onclick = () => {
      dirInput.value = path;
      suggestionList.remove();
    };
    suggestionList.appendChild(li);
  });
  
  dirInput.parentNode.appendChild(suggestionList);
};
```

**后端 API**: `GET /fs/settings/public-dirs/suggest?q=prefix`
```go
// 返回建议列表
["path/dir1", "path/dir2", "path/subdir/dir3"]
```

**UI 效果**:
- 下拉列表显示建议
- 键盘上下键导航
- 回车键选择
- 模糊匹配搜索

---

### 优化 6: 目录预览功能 (1-2 天)

**功能描述**: 鼠标悬停或点击公开目录时，显示目录信息面板（文件数、大小、权限、最后修改时间）。

**实现文件**: `assets/settings.js` + `internal/server/handler_list.go`

**关键代码位置**:
```javascript
// 获取目录详细信息
const getDirectoryInfo = async (path) => {
  const response = await fetch(
    `/fs/settings/public-dirs/details?path=${encodeURIComponent(path)}`
  );
  return response.json();
};

// 显示预览面板
const showPreview = async (element, path) => {
  const info = await getDirectoryInfo(path);
  
  const preview = document.createElement('div');
  preview.className = 'dir-preview';
  preview.innerHTML = `
    <div class="preview-header">${path}</div>
    <div class="preview-item">
      <span>文件数</span>
      <span>${info.file_count}</span>
    </div>
    <div class="preview-item">
      <span>总大小</span>
      <span>${formatSize(info.total_size)}</span>
    </div>
    <div class="preview-item">
      <span>权限</span>
      <span>${info.permissions}</span>
    </div>
    <div class="preview-item">
      <span>修改时间</span>
      <span>${new Date(info.mtime).toLocaleString()}</span>
    </div>
  `;
  
  element.appendChild(preview);
};
```

**后端 API**: `GET /fs/settings/public-dirs/details?path=...`
```go
// 返回目录信息
{
  "path": "/public",
  "file_count": 42,
  "total_size": 1024000,
  "permissions": "rw-r--r--",
  "mtime": "2026-11-09T10:00:00Z"
}
```

**UI 效果**:
- 悬停显示预览框
- 显示文件数量、大小、权限
- 自动计算总大小

---

### 优化 7: 批量操作支持 (1-2 天)

**功能描述**: 支持一次性添加多个公开目录（通过粘贴、拖拽或导入文件）。

**实现文件**: `assets/settings.js` + `internal/server/handler_list.go`

**关键代码位置**:
```javascript
// 批量导入 API 调用
const importPublicDirs = async (paths) => {
  const response = await fetch('/fs/settings/public-dirs/import', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ paths })
  });
  return response.json();
};

// 粘贴处理
dirInput.addEventListener('paste', async (e) => {
  const text = e.clipboardData.getData('text');
  const paths = text.split('\n')
    .map(p => p.trim())
    .filter(p => p.length > 0);
  
  if (paths.length > 1) {
    const result = await importPublicDirs(paths);
    if (result.success) {
      G.toast('ok', `已导入 ${result.count} 个目录`);
    }
  }
});

// 拖拽处理
dirInput.addEventListener('dragover', (e) => {
  e.preventDefault();
  dirInput.classList.add('dragover');
});

dirInput.addEventListener('drop', async (e) => {
  e.preventDefault();
  dirInput.classList.remove('dragover');
  
  const files = e.dataTransfer.files;
  for (const file of files) {
    if (file.name.endsWith('.csv') || file.name.endsWith('.json')) {
      const text = await file.text();
      // 解析并导入
    }
  }
});
```

**后端 API**: `POST /fs/settings/public-dirs/import`
```go
// 请求体
{
  "paths": ["/dir1", "/dir2", "/dir3"]
}

// 响应
{
  "success": true,
  "count": 3,
  "imported": ["/dir1", "/dir2", "/dir3"],
  "failed": []
}
```

**UI 效果**:
- 支持粘贴多行路径
- 支持拖拽 CSV/JSON 文件
- 显示导入进度和结果
- 冲突处理（覆盖/跳过）

---

### 优化 8: 拖拽排序功能 (1 天)

**功能描述**: 用户可以拖拽公开目录列表中的项目来改变优先级/顺序，自动保存。

**实现文件**: `assets/settings.js` + CSS

**关键代码位置**:
```javascript
// 初始化拖拽
const initDragSort = () => {
  const list = document.querySelector('.public-dirs-list');
  
  list.addEventListener('dragstart', (e) => {
    if (e.target.classList.contains('dir-item')) {
      e.target.classList.add('dragging');
      e.dataTransfer.effectAllowed = 'move';
    }
  });
  
  list.addEventListener('dragover', (e) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    
    const dragging = document.querySelector('.dir-item.dragging');
    const afterElement = getDragAfterElement(list, e.clientY);
    
    if (afterElement == null) {
      list.appendChild(dragging);
    } else {
      list.insertBefore(dragging, afterElement);
    }
  });
  
  list.addEventListener('drop', async (e) => {
    e.preventDefault();
    const dragging = document.querySelector('.dir-item.dragging');
    dragging.classList.remove('dragging');
    
    // 保存新顺序
    const newOrder = Array.from(list.children)
      .map(el => el.dataset.path);
    
    await saveDirOrder(newOrder);
  });
};
```

**CSS 样式**:
```css
.public-dirs-list {
  list-style: none;
  padding: 0;
}

.dir-item {
  padding: 12px;
  margin: 8px 0;
  background: #f5f5f5;
  border-radius: 8px;
  cursor: move;
  user-select: none;
  transition: all 0.2s;
}

.dir-item:hover {
  background: #e8e8e8;
  transform: translateY(-2px);
}

.dir-item.dragging {
  opacity: 0.5;
  transform: rotate(2deg);
}

.dir-item::before {
  content: '⋮⋮';
  margin-right: 8px;
  color: #999;
}
```

**UI 效果**:
- 拖拽项目改变顺序
- 视觉反馈（变透、旋转）
- 自动保存顺序
- 撤销/重做支持

---

## 📋 实现优先级

```
高优先级:
  1. 优化 4 (实时验证) - 防止错误输入
  2. 优化 7 (批量操作) - 提升效率

中优先级:
  3. 优化 5 (自动完成) - 改善体验
  4. 优化 6 (目录预览) - 提供上下文

低优先级:
  5. 优化 8 (拖拽排序) - UI 细节
```

---

## 🚀 实现步骤

### 第 1 周
- Day 1: 优化 4 (实时验证)
- Day 2-3: 优化 5 (自动完成) + 优化 6 (目录预览)
- Day 4: 优化 7 (批量操作)
- Day 5: 优化 8 (拖拽排序) + 测试

### 第 2 周
- 完整功能测试
- 性能优化
- 文档更新
- 版本发布 v0.1.24

---

## 📝 后端 API 设计

### 新增 API 端点

```
GET  /fs/settings/public-dirs/details?path=...
     返回目录信息（文件数、大小、权限、时间）

POST /fs/settings/public-dirs/validate
     验证多个路径的有效性

GET  /fs/settings/public-dirs/suggest?q=prefix
     获取匹配的目录建议

POST /fs/settings/public-dirs/import
     批量导入公开目录

PUT  /fs/settings/public-dirs/order
     更新目录顺序
```

---

## 🧪 测试清单

- [ ] 实时验证在输入时工作
- [ ] 自动完成显示正确的建议
- [ ] 目录预览显示完整信息
- [ ] 批量导入成功处理多个路径
- [ ] 拖拽排序保存正确的顺序
- [ ] 所有操作都有用户反馈
- [ ] 移动端上述功能都可用
- [ ] 性能指标达标（< 100ms 响应）

---

## 📊 预计工作量

```
优化 4: 8 小时
优化 5: 12 小时
优化 6: 16 小时
优化 7: 16 小时
优化 8: 8 小时
━━━━━━━━━━━━
总计: 60 小时 (1-2 周，取决于工作强度)
```

---

## 🎯 成功标准

- ✅ 所有 5 个优化功能都实现
- ✅ 代码质量达到产品级别
- ✅ 性能指标达标
- ✅ 移动端完全支持
- ✅ 完整的测试覆盖
- ✅ 详细的文档和示例

---

## 📞 关键技术点

1. **防抖和节流** - 优化实时验证性能
2. **虚拟滚动** - 处理大量建议列表
3. **拖拽 API** - 现代浏览器 Drag Drop
4. **数据同步** - 确保前后端一致
5. **错误处理** - 用户友好的错误信息

---

**下一步**: 根据此计划逐个实现 5 个优化功能

**版本**: v0.1.24 (第二阶段)  
**预计发布**: 2 周内

