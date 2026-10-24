package server

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

// upScriptName 是脚本端点的路径。
//
// 取个短名字是有意的：它的用法是一行命令，
//
//	bash <(curl -sS "<服务地址>/up?key=<密钥>") 文件 目录
//
// 名字越长，这行越难读。
// 路径取成 /up 而不是塞进 fs 命名空间：它是要被用户抄进命令行的一条
// 命令，越短越好读。代价是可能和根目录下真实的 up 文件/目录撞名 ——
// 见 handleUpScript 里「没带 key 就交回正常流程」的处理。
const upScriptName = "/up"

// upScriptTpl 是下发给用户的一键上传脚本。
//
// 设计取舍：
//   - **独立可跑**：拿到就能 `bash up.sh 文件 目录`，不需要再改任何地方。
//     密钥与服务地址由服务端填好（见 upScript），这正是「一键」的部分。
//   - **保留目录层级**：上传一个目录时，里面的子目录结构在服务端原样重建。
//     靠的是 multipart 的 path 字段 —— 单靠文件名不行，服务端会把
//     多部分文件名压平到它的基本名。
//   - 只用 curl、bash 和标准 Unix 工具（find/dirname/basename/mktemp），
//     没有别的依赖。
//
// __KEY__ 与 __BASE__ 在脚本里处于单引号中，替换前做 shell 转义；
// __SCOPE__ 只出现在注释里，替换前去掉控制字符（注释不认引号，换行会
// 提前结束注释）。
const upScriptTpl = `#!/bin/bash
# gofs 一键上传脚本
#
#   bash up.sh 文件或目录... [目标子目录]
#
# 不带目标子目录时直接传到 __SCOPE__。
# 这把密钥只能上传，不能浏览、删除或修改；随时可以在页面上撤销。

set -u

KEY='__KEY__'
BASE='__BASE__'

if [ $# -eq 0 ]; then
  echo "用法: bash $0 文件或目录... [目标子目录]" >&2
  exit 1
fi

args=("$@")

# 最后一个参数若不是已存在的路径，就当作目标子目录。
# 这样「传一个不存在的名字」和「指定一个目录」不会互相妨碍。
DEST=''
last="${args[${#args[@]}-1]}"
if [ "${#args[@]}" -gt 1 ] && [ ! -e "$last" ]; then
  DEST="${last%/}"
  args=("${args[@]:0:${#args[@]}-1}")
fi

ok=0
bad=0

# 每次上传的服务端响应体写进这个临时文件。上传接口返回的 JSON/错误说明
# 是排查 403/413/409 的关键，不能像以前那样直接丢进 /dev/null。
BODY_FILE=$(mktemp "${TMPDIR:-/tmp}/gofs-up.XXXXXX") || {
  echo "无法创建临时文件" >&2
  exit 1
}
trap 'rm -f "$BODY_FILE"' EXIT

# print_body <缩进>：打印最近一次上传的响应体；为空时什么也不做。
print_body() {
  local body
  body=$(<"$BODY_FILE") || body=""
  [ -n "$body" ] || return 0
  printf '%s%s\n' "$1" "$body"
}

# 反代终止 TLS 时，服务端可能不知道外部用的是 HTTPS，把地址写成了 http://。
# 先用一个**不带密钥**的 HEAD 探一下：明文地址若被跳到 https，就把 BASE
# 一次换掉 —— 既避免把密钥和文件先发到明文连接上，也避免每个文件白传一遍。
if [ "${BASE#http://}" != "$BASE" ]; then
  probe=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' \
    --connect-timeout 5 --max-time 10 -I "$BASE" 2>/dev/null) || probe=""
  probe_code=${probe%% *}
  probe_loc=${probe#* }
  if [ "${probe_code#3}" != "$probe_code" ] && [ "${probe_loc#https://}" != "$probe_loc" ]; then
    BASE="https://${BASE#http://}"
  fi
fi

# post_once <地址> <本地文件> <目标子目录>
# 输出 "<HTTP 状态码> <重定向地址>"；只有 curl 本身失败时才返回非 0。
# 不用 -f：它把 3xx 也当成成功，而反代的「HTTP → HTTPS」跳转正是 3xx ——
# 上传内容根本没发出去，脚本却会报成功。
post_once() {
  local url="$1" file="$2" target="$3"
  : > "$BODY_FILE"
  if [ -n "$target" ]; then
    curl -sS -o "$BODY_FILE" -w '%{http_code} %{redirect_url}' \
      -H "X-Gofs-Upload-Key: $KEY" \
      -F "path=$target" -F "file=@$file" "$url"
  else
    curl -sS -o "$BODY_FILE" -w '%{http_code} %{redirect_url}' \
      -H "X-Gofs-Upload-Key: $KEY" \
      -F "file=@$file" "$url"
  fi
}

# post <本地文件> <想放的相对路径>
post() {
  local rel="$2" dir target url resp code
  dir=$(dirname "$rel")
  [ "$dir" = "." ] && dir=""
  target="${DEST:+$DEST/}$dir"
  target="${target%/}"

  url="$BASE"
  if ! resp=$(post_once "$url" "$1" "$target"); then
    echo "  ✗ $rel（连接失败）" >&2
    print_body "    " >&2
    bad=$((bad + 1))
    return
  fi
  code=${resp%% *}

  # 上面的探针没测出跳转时的兜底（比如 HEAD 被反代拦了）。
  # 反代只做 HTTP→HTTPS 跳转时，真正接收上传的是 https 那一侧。
  # 在**同一路径**上换协议重发：不跟着 Location 走，因为有的反代
  # 跳转时会丢掉路径和查询串，跟着走会把文件传到根目录或别的范围里。
  # 301/302 也在这里手动重发 POST —— 交给 curl 跟会变成 GET。
  if [ "${code#3}" != "$code" ] && [ "${url#http://}" != "$url" ]; then
    url="https://${url#http://}"
    if ! resp=$(post_once "$url" "$1" "$target"); then
      echo "  ✗ $rel（改用 https 重试失败）" >&2
      print_body "    " >&2
      bad=$((bad + 1))
      return
    fi
    code=${resp%% *}
  fi

  case "$code" in
    2??)
      echo "  ✓ ${target:+$target/}$(basename "$rel")"
      print_body "    "
      ok=$((ok + 1))
      ;;
    3??)
      # 跳过去了却没找到可用的 https 地址：多半是反代没把
      # X-Forwarded-Proto 传给服务端，或服务端没配 --trusted-proxies。
      echo "  ✗ $rel（HTTP $code 跳转未完成，检查反代的 X-Forwarded-Proto 与 --trusted-proxies）" >&2
      print_body "    " >&2
      bad=$((bad + 1))
      ;;
    *)
      echo "  ✗ $rel（HTTP $code）" >&2
      print_body "    " >&2
      bad=$((bad + 1))
      ;;
  esac
}

for a in "${args[@]}"; do
  a="${a%/}"
  if [ -d "$a" ]; then
    # 目录：连层级一起传，顶层目录名也保留。
    root=$(basename "$a")
    while IFS= read -r f; do
      post "$f" "$root/${f#"$a"/}"
    done < <(find "$a" -type f)
  elif [ -f "$a" ]; then
    post "$a" "$(basename "$a")"
  else
    echo "  跳过（不存在）：$a" >&2
    bad=$((bad + 1))
  fi
done

echo "完成：成功 $ok 个，失败 $bad 个"
[ "$bad" -eq 0 ]
`

// handleUpScript 下发一键上传脚本。
//
//	GET /up?key=<上传密钥>
//
// 为什么由服务端下发而不是给一段固定脚本：脚本里要把密钥和服务地址填好，
// 否则用户还得自己改两处 —— 那就谈不上「一键」了。
//
// 鉴权用的是**上传密钥本身**，不要账号密码：这个端点存在的意义就是让
// 拿到密钥的人（脚本、CI、第三方）自助拿到上传工具。
// 密钥非法就直接拒绝，不下发。
func (s *Server) handleUpScript(w http.ResponseWriter, r *http.Request) {
	// /up 是个顶层短路径，很可能和用户自己根目录下的 up 文件或 up/ 目录撞名。
	// 没带 key 参数时交回正常流程 —— 这样根目录里那个 up 照常能访问/下载，
	// 只有显式带上 ?key= 才是「取上传脚本」。
	if uploadKeyToken(r) == "" {
		s.handleRoot(w, r)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.keys == nil {
		http.Error(w, "404 Not Found: 本服务未启用上传密钥", http.StatusNotFound)
		return
	}

	token := uploadKeyToken(r)
	k, err := s.keys.Verify(token)
	if err != nil {
		s.logger.Errorf("下发上传脚本失败：密钥无效（%v）", err)
		http.Error(w, "401 Unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if !s.cfg.AllowUpload {
		http.Error(w, "403 Forbidden: 本服务未开启上传（--allow-upload）", http.StatusForbidden)
		return
	}

	base := s.publicBaseURL(r) + scopeDir(k.Scope)
	body := strings.NewReplacer(
		"__KEY__", shellSingleQuote(token),
		"__BASE__", shellSingleQuote(base),
		"__SCOPE__", scriptCommentText(k.Scope),
	).Replace(upScriptTpl)

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	// 脚本里带着密钥，不能被中间层或浏览器缓存下来。
	w.Header().Set("Cache-Control", "no-store")
	// 带附件名，用浏览器直接打开这个链接时会下载成文件而不是显示一坨文本。
	w.Header().Set("Content-Disposition", `attachment; filename="gofs-up.sh"`)
	fmt.Fprint(w, body)
}

// publicBaseURL 推出这个服务对外的根地址（不含 --path-prefix 之后的路径）。
//
// 反向代理通常会把 TLS 终止在上游，此时 r.TLS 为空，只看它会得出 http ——
// 那会让脚本里的地址不可用。所以优先采信 X-Forwarded-Proto，
// 与 clientIP 采信 X-Forwarded-For 是同一个前提：部署在可信代理之后。
func (s *Server) publicBaseURL(r *http.Request) string {
	scheme := s.externalScheme(r)
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + r.Host + s.PathPrefix()
}

// shellSingleQuote 把一个值安全地放进单引号里。
//
// 单引号串里唯一需要处理的就是单引号本身：收起来、转义、再打开。
// 密钥是服务端生成的 base64url，本来不含单引号，但地址可能来自 Host 头 ——
// 那是客户端可控的，不转义就等于把命令注入递给了脚本。
func shellSingleQuote(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// scriptCommentText 把值变成可安全放进 shell 注释的文本。
//
// 注释从 # 开始到行尾，引号在其中不生效，因此换行/回车会提前结束注释、
// 把后面的内容变成命令。控制字符统一换成空格；scope 在创建时已经拒绝
// 控制字符，这里兜住历史 keys.json 中可能存在的旧数据。
func scriptCommentText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// scopeDir 把密钥的范围变成「上传时 POST 到的目录」。
//
// 密钥只能写进自己的范围，所以脚本里的地址必须是范围本身而不是服务根 ——
// 否则第一个请求就会被 403 挡下来。
func scopeDir(scope string) string {
	if scope == "" || scope == "/" {
		return "/"
	}
	return strings.TrimSuffix(scope, "/") + "/"
}
