#!/bin/bash

./gofs --allow-anonymous-admin -b 127.0.0.1 -p 5000 ./data > /tmp/gofs-test.log 2>&1 &
SERVER_PID=$!
sleep 3

echo "=== 当前公开目录 ==="
curl -s "http://127.0.0.1:5000/fs/settings" | jq '.public_dirs' 2>/dev/null || echo "无法获取"

echo ""
echo "=== 批量导入响应 ==="
curl -s -X POST "http://127.0.0.1:5000/fs/settings/public-dirs/import" \
    -H "Content-Type: application/json" \
    -d '{"paths":["/test1","/test2"]}' | jq .

kill $SERVER_PID 2>/dev/null || true
wait $SERVER_PID 2>/dev/null || true
