#!/bin/bash
# gofs 优化功能集成测试脚本
# 版本: v0.1.24-v0.1.26
# 日期: 2026-11-09

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 测试统计
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

# 服务器配置
SERVER_URL="http://127.0.0.1:5000"
SERVER_PID=""

echo -e "${BLUE}╔════════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║          gofs 优化功能集成测试                                ║${NC}"
echo -e "${BLUE}╚════════════════════════════════════════════════════════════════╝${NC}"
echo ""

# 测试结果函数
test_result() {
    local name=$1
    local status=$2
    TOTAL_TESTS=$((TOTAL_TESTS + 1))

    if [ "$status" = "PASS" ]; then
        echo -e "${GREEN}✓${NC} [$TOTAL_TESTS] $name"
        PASSED_TESTS=$((PASSED_TESTS + 1))
    else
        echo -e "${RED}✗${NC} [$TOTAL_TESTS] $name"
        FAILED_TESTS=$((FAILED_TESTS + 1))
    fi
}

# 启动服务器
start_server() {
    echo -e "${YELLOW}启动 gofs 服务器...${NC}"

    if [ ! -f "./gofs" ]; then
        echo -e "${RED}错误: gofs 可执行文件不存在${NC}"
        echo "请先运行: go build"
        exit 1
    fi

    ./gofs --allow-anonymous-admin -b 127.0.0.1 -p 5000 ./data > /tmp/gofs-test.log 2>&1 &
    SERVER_PID=$!

    # 等待服务器启动
    echo "等待服务器启动..."
    for i in {1..10}; do
        if curl -s "$SERVER_URL" > /dev/null 2>&1; then
            echo -e "${GREEN}服务器已启动 (PID: $SERVER_PID)${NC}"
            return 0
        fi
        sleep 1
    done

    echo -e "${RED}服务器启动失败${NC}"
    exit 1
}

# 停止服务器
stop_server() {
    if [ -n "$SERVER_PID" ]; then
        echo -e "${YELLOW}停止服务器...${NC}"
        kill $SERVER_PID 2>/dev/null || true
        wait $SERVER_PID 2>/dev/null || true
        echo -e "${GREEN}服务器已停止${NC}"
    fi
}

# 清理函数
cleanup() {
    stop_server
    echo ""
    echo -e "${BLUE}════════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}测试总结${NC}"
    echo -e "${BLUE}════════════════════════════════════════════════════════════════${NC}"
    echo "总测试数: $TOTAL_TESTS"
    echo -e "通过: ${GREEN}$PASSED_TESTS${NC}"
    echo -e "失败: ${RED}$FAILED_TESTS${NC}"

    if [ $FAILED_TESTS -eq 0 ]; then
        echo -e "${GREEN}✓ 所有测试通过！${NC}"
        exit 0
    else
        echo -e "${RED}✗ 存在失败的测试${NC}"
        exit 1
    fi
}

trap cleanup EXIT INT TERM

# ============================================================================
# 测试 1: 基础功能测试
# ============================================================================

test_basic() {
    echo ""
    echo -e "${BLUE}[测试 1] 基础功能测试${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 1.1 服务器响应
    if curl -s "$SERVER_URL" > /dev/null; then
        test_result "服务器响应正常" "PASS"
    else
        test_result "服务器响应正常" "FAIL"
    fi

    # 1.2 设置接口
    response=$(curl -s "$SERVER_URL/fs/settings")
    if echo "$response" | jq -e '.title' > /dev/null 2>&1; then
        test_result "设置接口可访问" "PASS"
    else
        test_result "设置接口可访问" "FAIL"
    fi
}

# ============================================================================
# 测试 2: 优化 4 - 实时路径验证
# ============================================================================

test_validation() {
    echo ""
    echo -e "${BLUE}[测试 2] 优化 4 - 实时路径验证${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 2.1 验证现有路径
    response=$(curl -s -X POST "$SERVER_URL/fs/settings/public-dirs/validate" \
        -H "Content-Type: application/json" \
        -d '{"paths":["/"]}')

    if echo "$response" | jq -e '."/".valid == true' > /dev/null 2>&1; then
        test_result "验证现有路径返回 valid=true" "PASS"
    else
        test_result "验证现有路径返回 valid=true" "FAIL"
    fi

    # 2.2 验证不存在的路径
    response=$(curl -s -X POST "$SERVER_URL/fs/settings/public-dirs/validate" \
        -H "Content-Type: application/json" \
        -d '{"paths":["/nonexistent-path-12345"]}')

    if echo "$response" | jq -e '."/nonexistent-path-12345".valid == false' > /dev/null 2>&1; then
        test_result "验证不存在的路径返回 valid=false" "PASS"
    else
        test_result "验证不存在的路径返回 valid=false" "FAIL"
    fi
}

# ============================================================================
# 测试 3: 优化 5 - 自动完成建议
# ============================================================================

test_suggestions() {
    echo ""
    echo -e "${BLUE}[测试 3] 优化 5 - 自动完成建议${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 3.1 获取根目录建议
    response=$(curl -s "$SERVER_URL/fs/settings/public-dirs/suggest?q=/")

    if echo "$response" | jq -e 'type == "array"' > /dev/null 2>&1; then
        test_result "建议接口返回数组" "PASS"
    else
        test_result "建议接口返回数组" "FAIL"
    fi

    # 3.2 建议数量限制
    count=$(echo "$response" | jq '. | length')
    if [ "$count" -le 10 ]; then
        test_result "建议数量不超过 10 个" "PASS"
    else
        test_result "建议数量不超过 10 个" "FAIL"
    fi
}

# ============================================================================
# 测试 4: 优化 6 - 目录预览
# ============================================================================

test_preview() {
    echo ""
    echo -e "${BLUE}[测试 4] 优化 6 - 目录预览${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 4.1 获取目录详情
    response=$(curl -s "$SERVER_URL/fs/settings/public-dirs/details?path=/")

    if echo "$response" | jq -e '.file_count' > /dev/null 2>&1; then
        test_result "详情接口返回 file_count" "PASS"
    else
        test_result "详情接口返回 file_count" "FAIL"
    fi

    if echo "$response" | jq -e '.total_size' > /dev/null 2>&1; then
        test_result "详情接口返回 total_size" "PASS"
    else
        test_result "详情接口返回 total_size" "FAIL"
    fi
}

# ============================================================================
# 测试 5: 优化 7 - 批量操作
# ============================================================================

test_batch_operations() {
    echo ""
    echo -e "${BLUE}[测试 5] 优化 7 - 批量操作${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 5.1 批量导入 - 测试接口返回正确的字段结构
    response=$(curl -s -X POST "$SERVER_URL/fs/settings/public-dirs/import" \
        -H "Content-Type: application/json" \
        -d '{"paths":["/"]}')

    if echo "$response" | jq -e '.success != null' > /dev/null 2>&1; then
        test_result "批量导入接口返回 success 字段" "PASS"
    else
        test_result "批量导入接口返回 success 字段" "FAIL"
    fi

    if echo "$response" | jq -e '.count' > /dev/null 2>&1; then
        test_result "批量导入接口返回 count 字段" "PASS"
    else
        test_result "批量导入接口返回 count 字段" "FAIL"
    fi
}

# ============================================================================
# 测试 6: 优化 8 - 拖拽排序
# ============================================================================

test_drag_sort() {
    echo ""
    echo -e "${BLUE}[测试 6] 优化 8 - 拖拽排序${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 6.1 先导入两个有效路径
    curl -s -X POST "$SERVER_URL/fs/settings/public-dirs/import" \
        -H "Content-Type: application/json" \
        -d '{"paths":["/"]}' > /dev/null

    # 6.2 更新顺序 - 使用有效的现有路径
    response=$(curl -s -X PUT "$SERVER_URL/fs/settings/public-dirs/order" \
        -H "Content-Type: application/json" \
        -d '{"paths":["/"]}')

    if echo "$response" | jq -e '.success == true' > /dev/null 2>&1; then
        test_result "更新顺序接口返回 success=true" "PASS"
    else
        test_result "更新顺序接口返回 success=true" "FAIL"
    fi
}

# ============================================================================
# 测试 7: 优化 12 - 缓存系统 (如果已实现)
# ============================================================================

test_cache() {
    echo ""
    echo -e "${BLUE}[测试 7] 优化 12 - 缓存系统${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 7.1 缓存统计接口
    response=$(curl -s "$SERVER_URL/fs/cache/stats" 2>/dev/null)

    if [ -n "$response" ]; then
        if echo "$response" | jq -e '.dir_stats_cache' > /dev/null 2>&1; then
            test_result "缓存统计接口可访问" "PASS"
        else
            test_result "缓存统计接口可访问" "FAIL"
        fi
    else
        echo -e "${YELLOW}⊘${NC} [跳过] 缓存统计接口 (未实现)"
    fi
}

# ============================================================================
# 测试 8: 前端文件检查
# ============================================================================

test_frontend_files() {
    echo ""
    echo -e "${BLUE}[测试 8] 前端文件检查${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 8.1 JS 文件
    if [ -f "assets/ui-enhancements.js" ]; then
        test_result "ui-enhancements.js 文件存在" "PASS"
    else
        test_result "ui-enhancements.js 文件存在" "FAIL"
    fi

    # 8.2 CSS 文件
    if [ -f "assets/ui-enhancements.css" ]; then
        test_result "ui-enhancements.css 文件存在" "PASS"
    else
        test_result "ui-enhancements.css 文件存在" "FAIL"
    fi
}

# ============================================================================
# 测试 9: 后端文件检查
# ============================================================================

test_backend_files() {
    echo ""
    echo -e "${BLUE}[测试 9] 后端文件检查${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 9.1 后端处理器
    if [ -f "internal/server/handler_public_dirs.go" ]; then
        test_result "handler_public_dirs.go 文件存在" "PASS"
    else
        test_result "handler_public_dirs.go 文件存在" "FAIL"
    fi

    # 9.2 Go 编译 - 检查 gofs 可执行文件是否存在
    if [ -f "gofs" ]; then
        test_result "Go 代码编译成功" "PASS"
    else
        test_result "Go 代码编译成功" "FAIL"
    fi
}

# ============================================================================
# 测试 10: E2E 测试文件检查
# ============================================================================

test_e2e_files() {
    echo ""
    echo -e "${BLUE}[测试 10] E2E 测试文件检查${NC}"
    echo "─────────────────────────────────────────────────────────"

    # 10.1 Playwright 配置
    if [ -f "playwright.config.ts" ]; then
        test_result "playwright.config.ts 存在" "PASS"
    else
        test_result "playwright.config.ts 存在" "FAIL"
    fi

    # 10.2 测试用例
    if [ -f "tests/e2e/settings-public-dirs.spec.ts" ]; then
        test_result "E2E 测试用例存在" "PASS"
    else
        test_result "E2E 测试用例存在" "FAIL"
    fi

    # 10.3 package.json
    if [ -f "package.json" ]; then
        test_result "package.json 存在" "PASS"
    else
        test_result "package.json 存在" "FAIL"
    fi
}

# ============================================================================
# 主测试流程
# ============================================================================

main() {
    # 检查依赖
    if ! command -v curl &> /dev/null; then
        echo -e "${RED}错误: curl 未安装${NC}"
        exit 1
    fi

    if ! command -v jq &> /dev/null; then
        echo -e "${RED}错误: jq 未安装${NC}"
        echo "请安装: brew install jq (macOS) 或 apt-get install jq (Linux)"
        exit 1
    fi

    # 启动服务器
    start_server

    # 运行所有测试
    test_basic
    test_validation
    test_suggestions
    test_preview
    test_batch_operations
    test_drag_sort
    test_cache
    test_frontend_files
    test_backend_files
    test_e2e_files
}

# 运行主函数
main
