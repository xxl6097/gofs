#!/bin/bash

./gofs --allow-anonymous-admin -b 127.0.0.1 -p 5000 ./data > /tmp/gofs-test.log 2>&1 &
SERVER_PID=$!
sleep 3

echo "=== 建议接口原始响应 ==="
curl -s "http://127.0.0.1:5000/fs/settings/public-dirs/suggest?q=/" | head -c 200
echo ""

echo ""
echo "=== 更新顺序原始响应 ==="
curl -s -X PUT "http://127.0.0.1:5000/fs/settings/public-dirs/order" \
    -H "Content-Type: application/json" \
    -d '{"paths":["/test2","/test1"]}' | head -c 200
echo ""

echo ""
echo "=== 缓存统计原始响应 ==="
curl -s "http://127.0.0.1:5000/fs/cache/stats" | head -c 200
echo ""

kill $SERVER_PID 2>/dev/null || true
wait $SERVER_PID 2>/dev/null || true
