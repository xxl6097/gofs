// 优化 4-8: UI/UX 增强功能实现代码

// ============================================================================
// 优化 4: 实时路径验证
// ============================================================================

const ValidatePublicDir = (() => {
  const debounce = (fn, delay) => {
    let timeout;
    return function(...args) {
      clearTimeout(timeout);
      timeout = setTimeout(() => fn.apply(this, args), delay);
    };
  };

  const validatePath = async (path) => {
    if (!path) return { valid: false, error: '路径不能为空' };

    try {
      const response = await fetch('/fs/settings/public-dirs/validate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ paths: [path] })
      });
      const result = await response.json();
      return result[path] || { valid: false, error: '验证失败' };
    } catch (err) {
      return { valid: false, error: err.message };
    }
  };

  const updateValidationUI = (input, result) => {
    const wrapper = input.parentElement;
    let indicator = wrapper.querySelector('.validation-indicator');
    let message = wrapper.querySelector('.validation-message');

    if (!indicator) {
      indicator = document.createElement('span');
      indicator.className = 'validation-indicator';
      input.parentElement.appendChild(indicator);
    }

    if (!message) {
      message = document.createElement('div');
      message.className = 'validation-message';
      input.parentElement.appendChild(message);
    }

    if (result.valid) {
      indicator.className = 'validation-indicator valid';
      indicator.textContent = '✓';
      message.textContent = '路径有效';
      message.style.color = '#16a34a';
    } else {
      indicator.className = 'validation-indicator invalid';
      indicator.textContent = '✗';
      message.textContent = result.error || '路径无效';
      message.style.color = '#dc2626';
    }
  };

  const init = (input) => {
    input.addEventListener('input', debounce(async (e) => {
      if (e.target.value.length > 0) {
        const result = await validatePath(e.target.value);
        updateValidationUI(e.target, result);
      }
    }, 300));
  };

  return { init, validatePath };
})();

// ============================================================================
// 优化 5: 自动完成建议
// ============================================================================

const AutocompleteDir = (() => {
  const getSuggestions = async (prefix) => {
    try {
      const response = await fetch(
        `/fs/settings/public-dirs/suggest?q=${encodeURIComponent(prefix)}`
      );
      return response.json();
    } catch (err) {
      console.error('获取建议失败:', err);
      return [];
    }
  };

  const showSuggestions = (input, suggestions) => {
    let list = input.parentElement.querySelector('.suggestions-list');
    if (list) list.remove();

    if (suggestions.length === 0) return;

    list = document.createElement('ul');
    list.className = 'suggestions-list';

    suggestions.forEach((path, index) => {
      const li = document.createElement('li');
      li.className = 'suggestion-item';
      if (index === 0) li.classList.add('active');
      li.textContent = path;
      li.onclick = () => {
        input.value = path;
        list.remove();
        input.focus();
      };
      list.appendChild(li);
    });

    input.parentElement.appendChild(list);

    // 键盘导航
    input.addEventListener('keydown', (e) => {
      const items = list.querySelectorAll('.suggestion-item');
      const active = list.querySelector('.active');
      let index = Array.from(items).indexOf(active);

      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          if (index < items.length - 1) {
            active.classList.remove('active');
            items[index + 1].classList.add('active');
            items[index + 1].scrollIntoView(false);
          }
          break;
        case 'ArrowUp':
          e.preventDefault();
          if (index > 0) {
            active.classList.remove('active');
            items[index - 1].classList.add('active');
            items[index - 1].scrollIntoView(false);
          }
          break;
        case 'Enter':
          e.preventDefault();
          active.click();
          break;
        case 'Escape':
          list.remove();
          break;
      }
    });
  };

  const init = (input) => {
    const debounce = (fn, delay) => {
      let timeout;
      return function(...args) {
        clearTimeout(timeout);
        timeout = setTimeout(() => fn.apply(this, args), delay);
      };
    };

    input.addEventListener('input', debounce(async (e) => {
      const suggestions = await getSuggestions(e.target.value);
      showSuggestions(e.target, suggestions);
    }, 300));

    input.addEventListener('focus', async (e) => {
      if (e.target.value.length > 0) {
        const suggestions = await getSuggestions(e.target.value);
        showSuggestions(e.target, suggestions);
      }
    });
  };

  return { init };
})();

// ============================================================================
// 优化 6: 目录预览功能
// ============================================================================

const DirectoryPreview = (() => {
  const formatSize = (bytes) => {
    if (bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return Math.round(bytes / Math.pow(k, i) * 100) / 100 + ' ' + sizes[i];
  };

  const getDirectoryInfo = async (path) => {
    try {
      const response = await fetch(
        `/fs/settings/public-dirs/details?path=${encodeURIComponent(path)}`
      );
      return response.json();
    } catch (err) {
      console.error('获取目录信息失败:', err);
      return null;
    }
  };

  const showPreview = async (element, path) => {
    let preview = element.parentElement.querySelector('.dir-preview');
    if (preview) preview.remove();

    const info = await getDirectoryInfo(path);
    if (!info) return;

    preview = document.createElement('div');
    preview.className = 'dir-preview';
    preview.innerHTML = `
      <div class="preview-header">${path}</div>
      <div class="preview-item">
        <span class="preview-label">文件数</span>
        <span class="preview-value">${info.file_count || 0}</span>
      </div>
      <div class="preview-item">
        <span class="preview-label">总大小</span>
        <span class="preview-value">${formatSize(info.total_size || 0)}</span>
      </div>
      <div class="preview-item">
        <span class="preview-label">权限</span>
        <span class="preview-value">${info.permissions || '---'}</span>
      </div>
      <div class="preview-item">
        <span class="preview-label">修改时间</span>
        <span class="preview-value">${new Date(info.mtime).toLocaleString('zh-CN')}</span>
      </div>
    `;

    element.parentElement.appendChild(preview);
  };

  const init = (elements) => {
    elements.forEach(el => {
      el.addEventListener('mouseenter', async (e) => {
        await showPreview(e.target, e.target.dataset.path);
      });

      el.addEventListener('mouseleave', (e) => {
        const preview = e.target.parentElement.querySelector('.dir-preview');
        if (preview) preview.remove();
      });
    });
  };

  return { init, getDirectoryInfo };
})();

// ============================================================================
// 优化 7: 批量操作支持
// ============================================================================

const BatchOperations = (() => {
  const importPublicDirs = async (paths) => {
    try {
      const response = await fetch('/fs/settings/public-dirs/import', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ paths })
      });
      return response.json();
    } catch (err) {
      console.error('导入失败:', err);
      return { success: false, error: err.message };
    }
  };

  const parsePastedPaths = (text) => {
    return text.split('\n')
      .map(p => p.trim())
      .filter(p => p.length > 0 && p.startsWith('/'));
  };

  const parseCSV = (text) => {
    return text.split('\n')
      .map(line => line.split(',')[0].trim())
      .filter(p => p.length > 0 && p.startsWith('/'));
  };

  const parseJSON = (text) => {
    try {
      const data = JSON.parse(text);
      return Array.isArray(data) ? data : data.paths || [];
    } catch {
      return [];
    }
  };

  const init = (input) => {
    // 粘贴处理
    input.addEventListener('paste', async (e) => {
      const text = e.clipboardData.getData('text');
      const paths = parsePastedPaths(text);

      if (paths.length > 1) {
        e.preventDefault();
        const result = await importPublicDirs(paths);
        if (result.success) {
          G.toast('ok', `已导入 ${result.count} 个目录`);
        } else {
          G.toast('fail', result.error || '导入失败');
        }
      }
    });

    // 拖拽处理
    input.addEventListener('dragover', (e) => {
      e.preventDefault();
      input.classList.add('dragover');
    });

    input.addEventListener('dragleave', (e) => {
      input.classList.remove('dragover');
    });

    input.addEventListener('drop', async (e) => {
      e.preventDefault();
      input.classList.remove('dragover');

      const files = e.dataTransfer.files;
      for (const file of files) {
        if (file.type === 'text/plain' || file.name.endsWith('.txt') ||
            file.name.endsWith('.csv') || file.name.endsWith('.json')) {
          const text = await file.text();

          let paths = [];
          if (file.name.endsWith('.csv')) {
            paths = parseCSV(text);
          } else if (file.name.endsWith('.json')) {
            paths = parseJSON(text);
          } else {
            paths = parsePastedPaths(text);
          }

          if (paths.length > 0) {
            const result = await importPublicDirs(paths);
            if (result.success) {
              G.toast('ok', `已从 ${file.name} 导入 ${result.count} 个目录`);
            }
          }
        }
      }
    });
  };

  return { init, importPublicDirs };
})();

// ============================================================================
// 优化 8: 拖拽排序功能
// ============================================================================

const DragSortDirs = (() => {
  const getDragAfterElement = (container, y) => {
    const draggableElements = [...container.querySelectorAll('.dir-item:not(.dragging)')];

    return draggableElements.reduce((closest, child) => {
      const box = child.getBoundingClientRect();
      const offset = y - box.top - box.height / 2;

      if (offset < 0 && offset > closest.offset) {
        return { offset: offset, element: child };
      } else {
        return closest;
      }
    }, { offset: Number.NEGATIVE_INFINITY }).element;
  };

  const saveDirOrder = async (paths) => {
    try {
      const response = await fetch('/fs/settings/public-dirs/order', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ paths })
      });
      return response.json();
    } catch (err) {
      console.error('保存顺序失败:', err);
      return { success: false };
    }
  };

  const init = (listElement) => {
    listElement.querySelectorAll('.dir-item').forEach(item => {
      item.draggable = true;
    });

    listElement.addEventListener('dragstart', (e) => {
      if (e.target.classList.contains('dir-item')) {
        e.target.classList.add('dragging');
        e.dataTransfer.effectAllowed = 'move';
      }
    });

    listElement.addEventListener('dragover', (e) => {
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';

      const dragging = listElement.querySelector('.dir-item.dragging');
      const afterElement = getDragAfterElement(listElement, e.clientY);

      if (afterElement == null) {
        listElement.appendChild(dragging);
      } else {
        listElement.insertBefore(dragging, afterElement);
      }
    });

    listElement.addEventListener('drop', async (e) => {
      e.preventDefault();
      const dragging = listElement.querySelector('.dir-item.dragging');
      dragging.classList.remove('dragging');

      const newOrder = Array.from(listElement.children)
        .map(el => el.dataset.path);

      const result = await saveDirOrder(newOrder);
      if (!result.success) {
        G.toast('fail', '保存顺序失败');
      }
    });

    listElement.addEventListener('dragend', (e) => {
      const dragging = listElement.querySelector('.dir-item.dragging');
      if (dragging) dragging.classList.remove('dragging');
    });
  };

  return { init };
})();

// ============================================================================
// 导出所有优化模块
// ============================================================================

window.UIEnhancements = {
  ValidatePublicDir,
  AutocompleteDir,
  DirectoryPreview,
  BatchOperations,
  DragSortDirs,

  // 初始化所有功能
  initAll: (inputElement, listElement) => {
    ValidatePublicDir.init(inputElement);
    AutocompleteDir.init(inputElement);
    BatchOperations.init(inputElement);
    DirectoryPreview.init(listElement.querySelectorAll('.dir-item'));
    DragSortDirs.init(listElement);
  }
};
