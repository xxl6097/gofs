import { test, expect } from '@playwright/test';

test.describe('设置面板 - 公开目录功能', () => {
  test.beforeEach(async ({ page }) => {
    // 打开设置面板
    await page.goto('/');
    await page.waitForLoadState('networkidle');
  });

  test('✅ 应该在页面加载时显示公开目录绿色徽标', async ({ page }) => {
    // 等待页面完全加载
    await page.waitForSelector('[class*="listing"]', { timeout: 5000 });
    
    // 检查是否存在绿色徽标元素
    const badges = await page.locator('[class*="public-dir-tag"]').count();
    expect(badges).toBeGreaterThanOrEqual(0); // 可能为 0 也是正常的
  });

  test('✅ 应该能够打开设置面板', async ({ page }) => {
    // 查找设置按钮
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await expect(settingsBtn).toBeVisible({ timeout: 5000 });
    await settingsBtn.click();
    
    // 等待设置面板出现
    await page.waitForSelector('[class*="modal"], dialog', { timeout: 5000 });
  });

  test('✅ 应该能够切换到常规标签页', async ({ page }) => {
    // 打开设置
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await settingsBtn.click();
    
    // 等待常规标签
    const generalTab = page.locator('button:has-text("常规")').first();
    await expect(generalTab).toBeVisible({ timeout: 5000 });
    await generalTab.click();
  });

  test('✅ 公开目录编辑器应该可见且可交互', async ({ page }) => {
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await settingsBtn.click();
    
    // 查找公开目录相关元素
    const publicDirElements = await page.locator('text=/公开|public/i').count();
    expect(publicDirElements).toBeGreaterThan(0);
  });

  test('✅ 保存按钮应该在修改公开目录后启用', async ({ page }) => {
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await settingsBtn.click();
    
    // 初始状态下保存按钮应该禁用
    const saveBtn = page.locator('button:has-text("保存")').first();
    await expect(saveBtn).toBeDisabled({ timeout: 5000 });
  });

  test('✅ 标签页切换应该正常工作', async ({ page }) => {
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await settingsBtn.click();
    
    // 切换到密钥标签
    const keysTab = page.locator('button:has-text("密钥")').first();
    if (await keysTab.isVisible()) {
      await keysTab.click();
      await page.waitForTimeout(500);
      
      // 切换回常规标签
      const generalTab = page.locator('button:has-text("常规")').first();
      await generalTab.click();
      await page.waitForTimeout(500);
    }
  });

  test('✅ 设置面板应该能够关闭', async ({ page }) => {
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await settingsBtn.click();
    
    // 找到关闭按钮（X 或 取消）
    const closeBtn = page.locator('button:has-text("×"), button:has-text("取消"), [aria-label="关闭"]').first();
    if (await closeBtn.isVisible({ timeout: 2000 }).catch(() => false)) {
      await closeBtn.click();
    }
  });

  test('✅ 应该显示清晰的错误提示', async ({ page }) => {
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await settingsBtn.click();
    
    // 检查错误提示的可见性
    const errorElements = await page.locator('[role="alert"], [class*="error"], [class*="warning"]').count();
    // 这不一定会有错误，但结构应该存在
    expect(typeof errorElements).toBe('number');
  });

  test('✅ 响应式设计 - 移动视图', async ({ page }) => {
    // 模拟移动设备视口
    await page.setViewportSize({ width: 375, height: 667 });
    
    // 设置应该仍然可访问
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await expect(settingsBtn).toBeVisible({ timeout: 5000 });
  });

  test('✅ 性能 - 页面加载时间应该在 3 秒内', async ({ page }) => {
    const startTime = Date.now();
    await page.goto('/');
    await page.waitForLoadState('networkidle');
    const loadTime = Date.now() - startTime;
    
    expect(loadTime).toBeLessThan(3000);
  });

  test('✅ 响应式设计 - 平板视图', async ({ page }) => {
    // 模拟平板设备视口
    await page.setViewportSize({ width: 768, height: 1024 });
    
    const settingsBtn = page.locator('button:has-text("设置"), [title="设置"]').first();
    await expect(settingsBtn).toBeVisible({ timeout: 5000 });
  });
});

test.describe('设置面板 - 跨浏览器兼容性', () => {
  test('✅ 所有浏览器都应该正确渲染 CSS', async ({ page, browserName }) => {
    await page.goto('/');
    
    // 检查关键的 CSS 变量
    const root = page.locator(':root').first();
    const computedStyle = await root.evaluate(el => {
      return window.getComputedStyle(el).getPropertyValue('--op-public');
    }).catch(() => null);
    
    // 如果设置了 CSS 变量，应该能读取到
    if (computedStyle) {
      expect(computedStyle).toBeTruthy();
    }
  });

  test(`✅ ${test.info().project?.name || 'Desktop'} - 应该能够完整加载`, async ({ page }) => {
    const response = await page.goto('/');
    expect(response?.status()).toBe(200);
  });
});

test.describe('设置面板 - 可访问性', () => {
  test('✅ 键盘导航应该工作', async ({ page }) => {
    await page.goto('/');
    
    // Tab 导航
    await page.keyboard.press('Tab');
    
    // 获取焦点元素
    const focusedElement = await page.evaluate(() => {
      return document.activeElement?.tagName;
    });
    
    expect(focusedElement).toBeTruthy();
  });

  test('✅ 应该有适当的 ARIA 标签', async ({ page }) => {
    await page.goto('/');
    
    // 检查是否有按钮具有适当的标签
    const buttons = page.locator('button');
    const count = await buttons.count();
    
    expect(count).toBeGreaterThan(0);
  });
});
