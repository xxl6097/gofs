#!/bin/bash

# 启动服务器
./gofs --allow-anonymous-admin -b 127.0.0.1 -p 5000 ./data > /tmp/gofs-test.log 2>&1 &
SERVER_PID=$!

# 等待服务器启动
sleep 3

echo "=== 测试 5: 建议接口 ==="
curl -s "http://127.0.0.1:5000/fs/settings/public-dirs/suggest?q=/" | jq . || echo "建议接口失败"

echo ""
echo "=== 测试 7: 批量导入 ==="
curl -s -X POST "http://127.0.0.1:5000/fs/settings/public-dirs/import" \
    -H "Content-Type: application/json" \
    -d '{"paths":["/test1","/test2"]}' | jq . || echo "批量导入失败"

echo ""
echo "=== 测试 8: 更新顺序 ==="
curl -s -X PUT "http://127.0.0.1:5000/fs/settings/public-dirs/order" \
    -H "Content-Type: application/json" \
    -d '{"paths":["/test2","/test1"]}' | jq . || echo "更新顺序失败"

echo ""
echo "=== 测试 12: 缓存统计 ==="
curl -s "http://127.0.0.1:5000/fs/cache/stats" | jq . || echo "缓存统计失败"

# 清理
kill $SERVER_PID 2>/dev/null || true
wait $SERVER_PID 2>/dev/null || true
