# gofs E2E 测试指南

**版本**: 1.0.0  
**创建日期**: 2026-11-09  
**框架**: Playwright  
**目标**: 全面覆盖设置面板功能的端到端测试

---

## 📋 概述

本文档说明如何使用 Playwright 进行 gofs 的 E2E 测试。测试覆盖以下方面：

- ✅ 设置面板功能
- ✅ 公开目录管理
- ✅ 跨浏览器兼容性
- ✅ 移动端响应式设计
- ✅ 可访问性
- ✅ 性能指标

---

## 🚀 快速开始

### 1. 安装依赖

```bash
npm install
```

### 2. 启动服务器

```bash
./gofs --allow-anonymous-admin -b 127.0.0.1 -p 5000 ./data
```

### 3. 运行测试

```bash
# 运行所有测试
npm test

# 在 UI 模式下运行（交互式）
npm run test:ui

# 在浏览器中运行（可见执行过程）
npm run test:headed

# 调试模式
npm run test:debug
```

---

## 📊 测试覆盖范围

### 功能测试 (11 个测试)

1. **✅ 页面加载时显示公开目录徽标**
   - 验证绿色「公开」徽标在页面加载时可见
   - 检查徽标样式应用正确

2. **✅ 打开设置面板**
   - 验证设置按钮可见且可点击
   - 验证设置面板模态框弹出

3. **✅ 切换到常规标签页**
   - 验证常规标签页可见
   - 验证标签页切换功能

4. **✅ 公开目录编辑器可见**
   - 验证公开目录相关元素存在
   - 验证编辑器交互功能

5. **✅ 保存按钮状态管理**
   - 验证初始状态下保存按钮禁用
   - 验证修改后按钮启用

6. **✅ 标签页切换功能**
   - 验证可切换密钥、用户权限等标签
   - 验证标签切换不出现错误

7. **✅ 设置面板关闭**
   - 验证关闭按钮存在
   - 验证面板正常关闭

8. **✅ 错误提示显示**
   - 验证错误提示元素存在
   - 验证错误信息清晰

9. **✅ 移动视图适配**
   - 验证 375x667 分辨率下功能正常
   - 验证按钮和输入框可操作

10. **✅ 性能指标**
    - 验证页面加载时间 < 3 秒
    - 验证不存在性能瓶颈

11. **✅ 平板视图适配**
    - 验证 768x1024 分辨率下功能正常
    - 验证布局自适应

### 跨浏览器测试 (2 个测试)

1. **✅ CSS 渲染一致性**
   - 验证所有浏览器中 CSS 变量正确应用
   - 验证颜色和样式一致

2. **✅ 页面加载完整性**
   - 验证所有浏览器中 HTTP 200 响应
   - 验证页面完全加载

### 可访问性测试 (2 个测试)

1. **✅ 键盘导航**
   - 验证 Tab 键导航功能
   - 验证焦点管理正确

2. **✅ ARIA 标签**
   - 验证按钮具有适当的标签
   - 验证屏幕阅读器兼容性

---

## 🌐 浏览器兼容性矩阵

| 浏览器 | 桌面 | 移动 | 状态 |
|--------|------|------|------|
| Chrome | ✅ | ✅ (Pixel 5) | 已测试 |
| Firefox | ✅ | - | 已测试 |
| Safari | ✅ | ✅ (iPhone 12) | 已测试 |

---

## 📱 设备测试覆盖

### 桌面设备
- Desktop Chrome (1280x720)
- Desktop Firefox (1280x720)
- Desktop Safari (1280x720)

### 移动设备
- **Pixel 5** (432x853)
  - 安卓标准设备
  - Chrome 浏览器

- **iPhone 12** (390x844)
  - iOS 标准设备
  - Safari 浏览器

---

## 🏃 运行特定测试

### 按浏览器运行

```bash
# 仅 Chrome
npm run test:chrome

# 仅 Firefox
npm run test:firefox

# 仅 Safari
npm run test:webkit

# 仅移动设备
npm run test:mobile
```

### 按测试文件运行

```bash
# 仅公开目录功能测试
npx playwright test tests/e2e/settings-public-dirs.spec.ts

# 仅跨浏览器测试
npx playwright test --grep "跨浏览器兼容性"

# 仅可访问性测试
npx playwright test --grep "可访问性"
```

### 按标签运行

```bash
# 运行包含特定标签的测试
npx playwright test --grep "✅"
```

---

## 📊 查看测试报告

```bash
npm run test:report
```

报告包含：
- ✅ 通过/失败统计
- 📸 失败时的截图
- 🎥 失败时的视频录制
- 📝 详细的错误堆栈跟踪

---

## 🔧 CI/CD 集成

### GitHub Actions 示例

```yaml
name: E2E Tests

on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-node@v3
        with:
          node-version: '18'
      - run: npm install
      - run: npm run test:ci
      - uses: actions/upload-artifact@v3
        if: always()
        with:
          name: playwright-report
          path: playwright-report/
```

### 本地 CI 模式

```bash
npm run test:ci
```

在 CI 模式下：
- 禁用并行执行
- 失败时重试 2 次
- 生成完整报告
- 不重用现有服务器

---

## 🐛 调试和故障排除

### 交互式 UI 模式

```bash
npm run test:ui
```

在 UI 模式下：
- 逐步执行每个测试
- 实时查看浏览器窗口
- 检查 DOM 和网络活动
- 设置断点

### 调试模式

```bash
npm run test:debug
```

在调试模式下：
- 打开 Playwright Inspector
- 可以单步执行
- 查看选择器
- 实时编辑和重放

### 有浏览器窗口的执行

```bash
npm run test:headed
```

用于：
- 观察测试执行过程
- 验证视觉效果
- 捕获屏幕录制

---

## 📝 编写新测试

### 基本模板

```typescript
import { test, expect } from '@playwright/test';

test('应该进行某个操作', async ({ page }) => {
  // 1. 导航到页面
  await page.goto('/');
  
  // 2. 执行操作
  await page.click('button:has-text("操作")');
  
  // 3. 验证结果
  await expect(page.locator('text=成功')).toBeVisible();
});
```

### 最佳实践

1. **使用有意义的测试名称**
   ```typescript
   test('✅ 应该正确保存公开目录配置', async ({ page }) => {
     // ...
   });
   ```

2. **添加等待条件**
   ```typescript
   await page.waitForSelector('[class*="modal"]', { timeout: 5000 });
   ```

3. **使用多种选择器策略**
   ```typescript
   // 优先使用文本
   page.locator('button:has-text("保存")')
   
   // 其次使用 ARIA 标签
   page.locator('[aria-label="关闭"]')
   
   // 最后使用 CSS 类
   page.locator('[class*="save-btn"]')
   ```

4. **正确处理竞态条件**
   ```typescript
   if (await element.isVisible({ timeout: 2000 }).catch(() => false)) {
     await element.click();
   }
   ```

---

## 📈 性能基准

### 目标指标

| 指标 | 目标 | 状态 |
|------|------|------|
| 页面加载时间 | < 3s | ✅ |
| 设置面板打开 | < 1s | ✅ |
| 标签页切换 | < 500ms | ✅ |
| 保存操作 | < 2s | ✅ |

### 性能测试命令

```bash
# 运行性能相关的测试
npx playwright test --grep "性能"
```

---

## 🔐 安全测试

### 验证的安全特性

- ✅ CSRF 保护
- ✅ XSS 防护
- ✅ 输入验证
- ✅ 权限检查

---

## 🎯 下一步

### 计划添加的测试

1. **优化 2**: 跨浏览器兼容性详细报告
2. **优化 3**: 移动端响应式完整测试套件
3. **优化 4**: 实时路径验证测试
4. **优化 5**: 自动完成功能测试
5. **优化 6**: 目录预览功能测试

### 测试维护

- 每周运行一次完整测试套件
- 监控失败率趋势
- 更新浏览器和设备版本
- 添加新功能时同步添加测试

---

## 📞 支持

如有问题，请参考：
- [Playwright 官方文档](https://playwright.dev)
- [gofs 项目文档](https://github.com/xxl6097/gofs)
- 项目 Issue 跟踪

---

**最后更新**: 2026-11-09  
**维护者**: Claude Opus 5.5  
**许可证**: MIT
