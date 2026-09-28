#!/usr/bin/env bash
# gofs 用户管理接口端到端检查。
# 前置：./gofs -A -b 127.0.0.1 -p 5000 -a 'admin:secret@/:rw' -a 'test:test@/' \
#         --user-file /tmp/gofs-users.json /tmp/gofs-test
set -u
B="${GOFS_URL:-http://127.0.0.1:5000}"
EP="$B/__gofs__/users"
O="Origin: $B"
CT="Content-Type: application/json"
PY=python3
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok()  { printf '  \033[32m✓\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=$((fail+1)); }
expect() { if [ "$1" = "$2" ]; then ok "$3 → $2"; else bad "$3 → 期望 $1，实际 $2（$(head -c 160 "$TMP/body" | tr -d '\n')）"; fi; }

jget() { $PY -c 'import json,sys;d=json.load(open(sys.argv[1]));
print(d.get(sys.argv[2],""))' "$TMP/body" "$1" 2>/dev/null || true; }

req() { # <method> <user:pass|-> <body|-> <url>
  local m="$1" cred="$2" body="$3" url="$4"
  local args=(-s --noproxy '*' -X "$m" -o "$TMP/body" -w '%{http_code}')
  [ "$cred" != "-" ] && args+=(-u "$cred")
  args+=(-H "$CT" -H "$O")
  [ "$body" != "-" ] && args+=(-d "$body")
  curl "${args[@]}" "$url"
}
# 取某个用户的规则摘要
rules_of() { $PY -c '
import json,sys
d=json.load(open(sys.argv[1]))
for u in d["users"]:
    if u["name"]==sys.argv[2]:
        print(",".join(r["path"]+":"+r["perm"] for r in u["rules"]))
        break
' "$TMP/body" "$1" 2>/dev/null || true; }

echo "① 列表与权限"
expect 200 "$(req GET admin:secret - "$EP")" '管理员可读取用户列表'
if $PY -c 'import json,sys
d=json.load(open(sys.argv[1]))
names={u["name"] for u in d["users"]}
assert {"admin","test"} <= names, names' "$TMP/body" 2>/dev/null; then
  ok '列表包含启动参数里的 admin 与 test'
else
  bad "列表内容不对：$(cat "$TMP/body")"
fi
if $PY -c 'import json,sys
d=json.load(open(sys.argv[1]))
by={u["name"]:u for u in d["users"]}
assert by["admin"]["from_startup"] and by["test"]["from_startup"]' "$TMP/body" 2>/dev/null; then
  ok '启动参数的账号都标记了 from_startup'
else
  bad 'from_startup 标记缺失'
fi
expect 403 "$(req GET test:test - "$EP")" '只读账号读不了用户列表'
expect 401 "$(req GET - - "$EP")" '无凭据读不了用户列表'
if grep -qi 'password\|hash\|salt' "$TMP/body" 2>/dev/null; then bad '响应里出现了密码材料'; else ok '响应里不含任何密码材料'; fi

echo "② 新建用户"
expect 200 "$(req POST admin:secret '{"name":"alice","password":"alicepass","rules":[{"path":"/docs","perm":"rw"},{"path":"/","perm":"r"}]}' "$EP")" '创建 alice（/docs 可读写、/ 只读）'
if [ "$(rules_of alice)" = "/docs:rw,/:r" ]; then ok "alice 的规则已生效（$(rules_of alice)）"; else bad "规则不对：$(rules_of alice)"; fi
if grep -q '"created_at"' "$TMP/body"; then ok '返回里带了创建时间'; else bad '缺少 created_at'; fi
expect 400 "$(req POST admin:secret '{"name":"alice","password":"alicepass","rules":[{"path":"/","perm":"r"}]}' "$EP")" '重名创建被拒'
expect 400 "$(req POST admin:secret '{"name":"admin","password":"whatever1","rules":[{"path":"/","perm":"r"}]}' "$EP")" '与启动参数重名被拒'
expect 400 "$(req POST admin:secret '{"name":"bob","password":"123","rules":[{"path":"/","perm":"r"}]}' "$EP")" '密码过短被拒'
expect 400 "$(req POST admin:secret '{"name":"bob","password":"bobpass1","rules":[]}' "$EP")" '空规则被拒'
expect 400 "$(req POST admin:secret '{"name":"bob","password":"bobpass1","rules":[{"path":"/a","perm":"rw"},{"path":"/a","perm":"r"}]}' "$EP")" '同路径重复被拒'
expect 400 "$(req POST admin:secret '{"name":"bob","password":"bobpass1","rules":[{"path":"/../../etc","perm":"rw"}]}' "$EP")" '带 .. 的路径被拒'
expect 400 "$(req POST admin:secret '{"name":"a b","password":"bobpass1","rules":[{"path":"/","perm":"r"}]}' "$EP")" '非法用户名被拒'
expect 400 "$(req POST admin:secret '{"name":"bob","rules":[{"path":"/","perm":"r"}]}' "$EP")" '新建时没给密码被拒'

echo "③ 新建的账号能登录、且按路径生效"
code=$(curl -s --noproxy '*' -u alice:alicepass -o "$TMP/body" -w '%{http_code}' "$B/docs?json")
expect 200 "$code" 'alice 能读 /docs'
code=$(curl -s --noproxy '*' -u alice:alicepass -X PUT -H "$O" -o "$TMP/body" -w '%{http_code}' --data-binary 'x' "$B/docs/alice-w.txt?dated=0")
expect 201 "$code" 'alice 能在 /docs 下写'
code=$(curl -s --noproxy '*' -u alice:alicepass -X PUT -H "$O" -o "$TMP/body" -w '%{http_code}' --data-binary 'x' "$B/outside-w.txt?dated=0")
expect 403 "$code" 'alice 不能写在 /docs 之外'
code=$(curl -s --noproxy '*' -u alice:alicepass -X PUT -H "$O" -o "$TMP/body" -w '%{http_code}' --data-binary 'x' "$B/docs/../outside2.txt?dated=0")
expect 403 "$code" 'alice 不能用 .. 绕出 /docs'
code=$(curl -s --noproxy '*' -u alice:wrongpass -o /dev/null -w '%{http_code}' "$B/docs?json")
expect 401 "$code" 'alice 密码错误时被拒'
rm -f /tmp/gofs-test/docs/alice-w.txt

echo "④ 修改与删除"
expect 200 "$(req PUT admin:secret '{"name":"alice","rules":[{"path":"/docs","perm":"r"}]}' "$EP")" '改权限（不改密码）'
if [ "$(rules_of alice)" = "/docs:r" ]; then ok '规则已更新'; else bad "规则未更新：$(rules_of alice)"; fi
code=$(curl -s --noproxy '*' -u alice:alicepass -X PUT -H "$O" -o /dev/null -w '%{http_code}' --data-binary 'x' "$B/docs/nope.txt?dated=0")
expect 403 "$code" '降级为只读后不能再写'
expect 200 "$(req PUT admin:secret '{"name":"alice","password":"newpass123","rules":[{"path":"/docs","perm":"rw"}]}' "$EP")" '重置密码'
code=$(curl -s --noproxy '*' -u alice:newpass123 -o /dev/null -w '%{http_code}' "$B/docs?json")
expect 200 "$code" '新密码可用'
code=$(curl -s --noproxy '*' -u alice:alicepass -o /dev/null -w '%{http_code}' "$B/docs?json")
expect 401 "$code" '旧密码立即失效（缓存已清）'
expect 400 "$(req PUT admin:secret '{"name":"admin","rules":[{"path":"/","perm":"r"}]}' "$EP")" '改启动参数里的账号被拒'
expect 400 "$(req PUT admin:secret '{"name":"nobody","rules":[{"path":"/","perm":"r"}]}' "$EP")" '改不存在的用户被拒'
expect 400 "$(req DELETE admin:secret - "$EP?name=admin")" '删启动参数里的账号被拒'
expect 403 "$(req POST test:test '{"name":"x","password":"xxxxxx1","rules":[{"path":"/","perm":"r"}]}' "$EP")" '只读账号不能建用户'

echo "⑤ 落盘"
# 落盘路径以服务端返回的 user_file 为准：脚本不该猜（不同实例可能用不同的 --user-file）
req GET admin:secret - "$EP" >/dev/null
UF=$($PY -c 'import json,sys;print(json.load(open(sys.argv[1])).get("user_file",""))' "$TMP/body" 2>/dev/null)
if [ -z "$UF" ]; then
  bad '服务端未告知 user_file（无法校验落盘）'
else
  ok "服务端用户表路径：$UF"
  if [ -f "$UF" ]; then ok '用户表已落盘'; else bad '用户表未落盘'; fi
  if grep -q '"salt"' "$UF" && ! grep -q 'alicepass\|newpass123' "$UF"; then
    ok '落盘文件里只有摘要，没有明文密码'
  else
    bad '落盘文件内容异常'
  fi
  if [ "$(stat -f '%Lp' "$UF" 2>/dev/null)" = "600" ]; then
    ok '用户表权限为 600'
  else
    bad "用户表权限 $(stat -f '%Lp' "$UF" 2>/dev/null)"
  fi
fi

echo "⑥ 清理"
expect 200 "$(req DELETE admin:secret - "$EP?name=alice")" '删除 alice'
if [ "$(rules_of alice)" = "" ]; then ok 'alice 已从列表消失'; else bad 'alice 仍在列表里'; fi
code=$(curl -s --noproxy '*' -u alice:newpass123 -o /dev/null -w '%{http_code}' "$B/docs?json")
expect 401 "$code" '被删账号无法再登录'

echo
echo "────────────────────────────────────────"
printf '通过 %d 项，失败 %d 项\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
