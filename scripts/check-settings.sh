#!/usr/bin/env bash
# gofs 服务设置端到端检查：权限、范围约束、CSRF、上限即时生效与残片清理。
#
# 需要先起一个带鉴权的实例（默认 admin:secret 可写根，test:test 只读；启动根 /tmp/gofs-test）：
#   ./gofs -A -b 127.0.0.1 -p 5000 -a 'admin:secret@/:rw' -a 'test:test@/' /tmp/gofs-test
#
# 用法：
#   scripts/check-settings.sh
#   GOFS_URL=http://127.0.0.1:5001 GOFS_ROOT_DIR=/srv/data scripts/check-settings.sh
set -u

B="${GOFS_URL:-http://127.0.0.1:5000}"
ROOT_DIR="${GOFS_ROOT_DIR:-/tmp/gofs-test}"
USER="${GOFS_USER:-admin:secret}"
LIMIT_DEFAULT="${GOFS_LIMIT_DEFAULT:-10737418240}"   # 与 --upload-max-size 的默认值保持一致
PY="${PYTHON:-python3}"

EP="${B}/__gofs__/settings"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

pass=0
fail=0
ok()  { printf '  \033[32m✓\033[0m %s\n' "${1}"; pass=$((pass + 1)); }
bad() { printf '  \033[31m✗\033[0m %s\n' "${1}"; fail=$((fail + 1)); }
expect() { # <期望码> <实际码> <说明>
  if [ "${1}" = "${2}" ]; then ok "${3} → ${2}"; else bad "${3} → 期望 ${1}，实际 ${2}（$(head -c 140 "${TMP}/body")）"; fi
}

# jget <字段名>：从最近一次写入 ${TMP}/body 的 JSON 里取值
jget() { "${PY}" -c 'import json,sys;d=json.load(open(sys.argv[1]));v=d.get(sys.argv[2]);print("" if v is None else v)' "${TMP}/body" "${1}"; }

req() { # <method> <auth:yes|no> <body> [额外 -H ...]
  local m="${1}" a="${2}" body="${3}"
  shift 3
  local args=(-s --noproxy '*' -X "${m}" -o "${TMP}/body" -w '%{http_code}')
  [ "${a}" = yes ] && args+=(-u "${USER}")
  args+=(-H 'Content-Type: application/json' -H "Origin: ${B}")
  [ -n "${body}" ] && args+=(-d "${body}")
  local h
  for h in "$@"; do args+=(-H "${h}"); done
  curl "${args[@]}" "${EP}"
}
get() { req GET yes ''; }
put() { req PUT yes "${1}"; }

echo "① 权限"
get >/dev/null
INIT_ROOT="$(jget root)"
printf '  初始根目录：%s\n' "${INIT_ROOT}"
expect 200 "$(get)" '有凭据可读取设置'
expect 401 "$(req GET no '')" '无凭据读取被拒'
code=$(curl -s --noproxy '*' -u test:test -X PUT -o "${TMP}/body" -w '%{http_code}' \
  -H 'Content-Type: application/json' -H "Origin: ${B}" -d '{"upload_max_size":1}' "${EP}")
expect 403 "${code}" '无根目录写权限的账号改设置被拒'

echo "② 根目录切换的范围约束"
expect 200 "$(put "{\"root\":\"${ROOT_DIR}/docs\"}")" '切到允许范围内的子目录'
expect 400 "$(put '{"root":"/"}')" '越界目录被拒'
expect 400 "$(put '{"root":"/tmp/definitely-not-here-xyz"}')" '不存在的目录被拒'
expect 400 "$(put "{\"root\":\"${ROOT_DIR}/../definitely-not-here\"}")" '带 .. 的路径被拒'
expect 400 "$(put '{"root":""}')" '空路径被拒'
expect 200 "$(put "{\"root\":\"${ROOT_DIR}\"}")" '切回原目录'

echo "③ 跨站来源校验"
code=$(curl -s --noproxy '*' -u "${USER}" -X PUT -o "${TMP}/body" -w '%{http_code}' \
  -H 'Content-Type: application/json' -H 'Origin: null' -d '{"upload_max_size":1}' "${EP}")
expect 403 "${code}" 'Origin: null 被拒'
code=$(curl -s --noproxy '*' -u "${USER}" -X PUT -o "${TMP}/body" -w '%{http_code}' \
  -H 'Content-Type: application/json' -d '{"upload_max_size":1}' "${EP}")
expect 200 "${code}" '不带 Origin 的命令行客户端放行'

echo "④ 上传上限：取值校验与即时生效"
expect 400 "$(put '{"upload_max_size":-5}')" '负数被拒'
expect 200 "$(put '{"upload_max_size":1024}')" '设为 1 KiB'
head -c 3000 /dev/urandom > "${TMP}/3k.bin"
code=$(curl -s --noproxy '*' -u "${USER}" -X PUT --data-binary @"${TMP}/3k.bin" \
  -H "Origin: ${B}" -o "${TMP}/body" -w '%{http_code}' "${B}/__too-large__.bin")
expect 413 "${code}" '超出上限的上传被拒（413）'
if [ -e "${ROOT_DIR}/__too-large__.bin" ]; then
  bad '超限后残留了残缺文件'
  rm -f "${ROOT_DIR}/__too-large__.bin"
else
  ok '超限后未留下残缺文件'
fi
printf 'x' > "${TMP}/1b.bin"
code=$(curl -s --noproxy '*' -u "${USER}" -X PUT --data-binary @"${TMP}/1b.bin" \
  -H "Origin: ${B}" -o /dev/null -w '%{http_code}' "${B}/__ok__.bin?dated=0")
expect 201 "${code}" '上限内的小文件正常上传'
rm -f "${ROOT_DIR}/__ok__.bin"
expect 200 "$(put '{"upload_max_size":0}')" '设为「不限制」'
if [ "$(jget upload_max_size_unlimited)" = "True" ]; then
  ok '不限制状态在响应里如实反映'
else
  bad "不限制状态未反映（$(cat "${TMP}/body")）"
fi
expect 200 "$(put "{\"upload_max_size\":${LIMIT_DEFAULT}}")" '恢复启动默认值'

echo "⑤ 最终状态"
get >/dev/null
FINAL_ROOT="$(jget root)"
FINAL_LIMIT="$(jget upload_max_size)"
if [ "${FINAL_ROOT}" = "${INIT_ROOT}" ]; then
  ok "根目录已回到起点（${FINAL_ROOT}）"
else
  bad "根目录未回到起点：${FINAL_ROOT} 与 ${INIT_ROOT} 不同"
fi
if [ "${FINAL_LIMIT}" = "${LIMIT_DEFAULT}" ]; then
  ok "上传上限已回到起点（${FINAL_LIMIT}）"
else
  bad "上传上限未回到起点：${FINAL_LIMIT} 与 ${LIMIT_DEFAULT} 不同"
fi

echo
echo "────────────────────────────────────────"
printf '通过 %d 项，失败 %d 项\n' "${pass}" "${fail}"
[ "${fail}" -eq 0 ]
