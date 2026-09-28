#!/usr/bin/env bash
# 用户管理的「部署形态」检查：持久化、功能开关、未启用鉴权时的拒绝。
#
# 与 check-users.sh 的分工：那个脚本跑单个实例上的完整 CRUD 与权限生效；
# 这个脚本要**反复重启**实例，所以单独放一份。
set -u
cd "$(dirname "$0")/.."
BIN=./gofs
ROOT="${GOFS_ROOT_DIR:-/tmp/gofs-test}"
PY=python3
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok()  { printf '  \033[32m✓\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=$((fail+1)); }
expect() { if [ "$1" = "$2" ]; then ok "$3 → $2"; else bad "$3 → 期望 $1，实际 $2（$(head -c 140 "$TMP/body" | tr -d '\n')）"; fi; }
names() { $PY -c 'import json,sys;print(" ".join(u["name"] for u in json.load(open(sys.argv[1]))["users"]))' "$TMP/body" 2>/dev/null; }

PIDS=()
cleanup() {
  # 注意：不要写 "${PIDS[@]:-}" —— bash 会把数组的 :- 形式当成取子串，直接语法错误
  # （zsh 却能通过，所以这种写法很能骗过本机检查）。空数组用长度判断即可。
  if [ "${#PIDS[@]}" -gt 0 ]; then
    kill "${PIDS[@]}" 2>/dev/null
  fi
}
trap 'cleanup; rm -rf "$TMP"' EXIT

start() { # <port> <额外参数...>
  local port="$1"; shift
  "$BIN" -A -b "127.0.0.1:$port" "$@" "$ROOT" >"$TMP/s-$port.log" 2>&1 &
  PIDS+=($!)
  sleep 1.2
}
stop_all() { cleanup; PIDS=(); sleep 0.6; }

UF=/tmp/gofs-users-persist.json
rm -f "$UF"

echo "① 重启后用户与密码都还在"
start 5011 -a 'admin:secret@/:rw' --user-file "$UF"
code=$(curl -s --noproxy '*' -u admin:secret -X POST -H 'Content-Type: application/json' \
  -H 'Origin: http://127.0.0.1:5011' \
  -d '{"name":"keeper","password":"keeperpw1","rules":[{"path":"/docs","perm":"rw"}]}' \
  -o "$TMP/body" -w '%{http_code}' "http://127.0.0.1:5011/__gofs__/users")
expect 200 "$code" '创建 keeper'
stop_all

start 5011 -a 'admin:secret@/:rw' --user-file "$UF"
code=$(curl -s --noproxy '*' -u admin:secret -o "$TMP/body" -w '%{http_code}' "http://127.0.0.1:5011/__gofs__/users")
expect 200 "$code" '重启后仍能读取用户列表'
if [ "$(names)" = "admin keeper" ]; then ok "重启后 keeper 还在（$(names)）"; else bad "重启后列表异常：$(names)"; fi
code=$(curl -s --noproxy '*' -u keeper:keeperpw1 -o /dev/null -w '%{http_code}' "http://127.0.0.1:5011/docs?json")
expect 200 "$code" '重启后 keeper 的密码依然有效（摘要可校验）'
code=$(curl -s --noproxy '*' -u keeper:keeperpw1 -X PUT -H 'Origin: http://127.0.0.1:5011' --data-binary 'x' -o /dev/null -w '%{http_code}' "http://127.0.0.1:5011/docs/k.txt?dated=0")
expect 201 "$code" '重启后 keeper 的写权限依然生效'
rm -f "$ROOT/docs/k.txt"
stop_all

echo "② 用户表有问题时启动就报错，而不是静默降级"
# 期望「启动失败」的检查不能简单用 $(...) 抓输出 —— 万一把服务真的起起来了，
# 命令替换会一直等下去（这就是我第一次跑时脚本被卡死的原因）。
# 用「起在后台 + 等一会儿 + 看它还在不在」来判断，并且无论如何都收尸。
expect_start_fail() { # <描述> <额外参数...>
  local desc="$1"; shift
  "$BIN" -A -b 127.0.0.1:5016 "$@" "$ROOT" >"$TMP/fail.log" 2>&1 &
  local pid=$!
  sleep 1.0
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null
    bad "${desc}（服务居然正常起来了，应当拒绝启动）"
    return
  fi
  wait "$pid" 2>/dev/null
  if grep -qE '无效|失败|高于' "$TMP/fail.log"; then
    ok "${desc}　→　$(head -1 "$TMP/fail.log")"
  else
    bad "${desc}（但错误信息不明确：$(head -1 "$TMP/fail.log")）"
  fi
}
printf '{"version":1,"users":[{"name":"bad name","salt":"AAAAAAAAAAAAAAAAAAAAAA==","hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","iter":1000,"rules":[{"path":"/","perm":"r"}]}]}' > "$TMP/bad-name.json"
expect_start_fail '用户表里有非法用户名时拒绝启动' -a 'admin:secret@/:rw' --user-file "$TMP/bad-name.json"
printf '{"version":1,"users":[{"name":"ok","salt":"AAAAAAAAAAAAAAAAAAAAAA==","hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","iter":1000,"rules":[]}]}' > "$TMP/no-rule.json"
expect_start_fail '用户表里有「没有任何路径权限」的账号时拒绝启动' -a 'admin:secret@/:rw' --user-file "$TMP/no-rule.json"
printf '{"version":999,"users":[]}' > "$TMP/newer.json"
expect_start_fail '用户表版本高于本程序时拒绝启动' -a 'admin:secret@/:rw' --user-file "$TMP/newer.json"
printf 'not json at all' > "$TMP/broken.json"
expect_start_fail '用户表不是合法 JSON 时拒绝启动' -a 'admin:secret@/:rw' --user-file "$TMP/broken.json"
printf '{"version":1,"users":[]}' > "$TMP/empty.json"
start 5017 -a 'admin:secret@/:rw' --user-file "$TMP/empty.json"
code=$(curl -s --noproxy '*' -u admin:secret -o "$TMP/body" -w '%{http_code}' "http://127.0.0.1:5017/__gofs__/users")
expect 200 "$code" '空的用户表是正常情况（首次运行），服务照常启动'
if [ "$(names)" = "admin" ]; then ok '空表只有启动参数的账号'; else bad "列表异常：$(names)"; fi
stop_all

echo "③ --no-user-manage 关闭功能"
start 5014 -a 'admin:secret@/:rw' --no-user-manage
code=$(curl -s --noproxy '*' -u admin:secret -o "$TMP/body" -w '%{http_code}' "http://127.0.0.1:5014/__gofs__/users")
expect 403 "$code" '--no-user-manage 下接口返回 403'
if curl -s --noproxy '*' -u admin:secret -H 'Accept: text/html' "http://127.0.0.1:5014/" | grep -q '"allow_users":false'; then
  ok '页面里 allow_users=false（入口会隐藏）'
else
  bad '页面里没有 allow_users=false'
fi
stop_all

echo "④ 未启用鉴权时不允许建用户"
start 5015 -A
code=$(curl -s --noproxy '*' -X POST -H 'Content-Type: application/json' -H 'Origin: http://127.0.0.1:5015' \
  -d '{"name":"x","password":"xxxxxx1","rules":[{"path":"/","perm":"r"}]}' \
  -o "$TMP/body" -w '%{http_code}' "http://127.0.0.1:5015/__gofs__/users")
expect 400 "$code" '未启用鉴权时拒绝创建用户（否则会把自己关在门外）'
if grep -q '未启用鉴权' "$TMP/body"; then ok '错误信息说明了原因'; else bad "错误信息不明确：$(cat "$TMP/body")"; fi
if curl -s --noproxy '*' -H 'Accept: text/html' "http://127.0.0.1:5015/" | grep -q '"allow_users":false'; then
  ok '未启用鉴权时页面不显示用户入口'
else
  bad '未启用鉴权时页面仍显示用户入口'
fi
stop_all

rm -f "$UF"
echo
echo "────────────────────────────────────────"
printf '通过 %d 项，失败 %d 项\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
