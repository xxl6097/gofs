# gofs 上传 API 与代码示例

本文只覆盖**上传**相关接口，包括：

| 接口 | 用途 | 适合场景 |
| --- | --- | --- |
| `PUT /<文件路径>` | 流式上传单个文件 | 大文件、断点续传、脚本直传 |
| `POST /<目录路径>/` | `multipart/form-data` 表单上传 | 浏览器多文件、保留目录层级 |
| `GET /up?key=<密钥>` | 下发一键上传脚本 | 把上传交给 CI / 第三方 |
| `POST /fs/keys` | 创建上传密钥 | 生成只能上传、不能读取的凭证 |

> 默认监听 `0.0.0.0:5000`（所有网卡）；只在本机使用请加 `-b 127.0.0.1`。
> 下文用 `http://127.0.0.1:5000` 作为本机地址；如果启用了访问前缀
> （`--path-prefix /files`），完整地址是 `http://127.0.0.1:5000/files`，
> 上传路径也要拼在它后面。

---

## 1. 快速开始

启动一个允许上传的服务：

```bash
./gofs -A -a 'admin:secret@/:rw' ./data
```

上传一个文件：

```bash
# 账号密码（HTTP Basic）
curl -u admin:secret -T ./report.pdf \
  "http://127.0.0.1:5000/uploads/report.pdf"

# 或使用上传密钥
curl -T ./report.pdf \
  -H 'X-Gofs-Upload-Key: gofs_xxxxxxxx' \
  "http://127.0.0.1:5000/uploads/report.pdf"
```

上传成功返回 `201 Created`：

```http
HTTP/1.1 201 Created
X-Gofs-Offset: 2483912
X-Gofs-Path: /uploads/report.pdf

created /uploads/report.pdf (2483912 bytes)
```

---

## 2. 认证方式

上传接口支持四种身份，按优先级从高到低：

### 2.1 HTTP Basic（账号密码）

```http
Authorization: Basic base64(user:pass)
```

```bash
curl -u admin:secret -T a.txt http://127.0.0.1:5000/docs/a.txt
```

账号的写权限由启动参数里的 ACL 规则决定，例如
`user:pass@/dir1:rw,/dir2` 表示该账号在 `/dir1` 可读写、在 `/dir2` 只读。
上传要求目标路径具备**读写**权限。

⚠️ **明文 HTTP 默认可用，但 Basic 只是 Base64**：同网段抓包即可还原账号
密码。HTTP 部署开箱即用；需要强制 HTTPS 时服务端加
`--allow-insecure-auth=false`，此时除本机回环 / Unix socket 外，只有 HTTPS
（或可信代理声明 `X-Forwarded-Proto: https`）上的 Basic 请求才会被处理，
远程明文 HTTP 会得到 `403`。

也支持查询参数形式（不推荐，凭据会留在 URL / 日志 / 浏览器历史里，
同样受上面的传输安全限制）：

```bash
curl -T a.txt 'http://127.0.0.1:5000/docs/a.txt?user=admin&pass=secret'
```

### 2.2 上传密钥（脚本 / CI 推荐）

上传密钥是服务端签发的 bearer 凭据，**只能上传**：

- 只能用于 `PUT` / `POST`；
- 只能写入密钥的 `scope` 范围（含日期归档后的真实路径）；
- 不能读取、删除、重命名、创建目录、解压或管理其它密钥。

携带方式二选一：

```http
X-Gofs-Upload-Key: gofs_xxxxxxxx
```

```bash
# 推荐：请求头
curl -T backup.tar.gz \
  -H 'X-Gofs-Upload-Key: gofs_xxxxxxxx' \
  http://127.0.0.1:5000/incoming/backup.tar.gz

# 或查询参数，便于一行命令
curl -T backup.tar.gz \
  'http://127.0.0.1:5000/incoming/backup.tar.gz?key=gofs_xxxxxxxx'
```

### 2.3 浏览器会话 Cookie

页面登录后可下发 HttpOnly 会话 Cookie，浏览器发起的上传请求会自动携带：

```http
POST /fs/auth
Authorization: Basic base64(user:pass)
```

响应 `Set-Cookie: gofs_sid=...`（HTTPS 根路径部署时是 `__Host-gofs_sid`）。
之后同源 XHR / `fetch` 带 `credentials: 'same-origin'` 即可，无需在 JS 里保存密码。

> 浏览器写请求会做同源校验（CSRF 防护）：`Origin` / `Referer` 必须与
> 请求 `Host` 同源。非浏览器客户端（curl、Go、Python 等）通常不带这些头，
> 不受影响。确实需要关闭时启动参数为 `--no-csrf-protect`。

### 2.4 创建上传密钥

密钥管理只接受**账号凭据或会话**，上传密钥不能给自己签发新密钥。

```bash
curl -u admin:secret -X POST \
  -H 'Content-Type: application/json' \
  -d '{"name":"CI 上传","scope":"/uploads","ttl_seconds":86400}' \
  http://127.0.0.1:5000/fs/keys
```

请求字段：

| 字段 | 说明 |
| --- | --- |
| `name` | 密钥名称，便于识别 |
| `scope` | 允许上传的路径前缀，如 `/uploads`；`/` 表示不限 |
| `ttl_seconds` | 有效期（秒），`0` 表示永不过期；最长 10 年 |
| `note` | 备注，可选 |
| `kind` | 默认 `upload`；分享链接另用 `read` / `readwrite` |

响应 `201 Created`：

```json
{
  "key": {
    "id": "k_xxx",
    "name": "CI 上传",
    "prefix": "gofs_AbCd1234",
    "scope": "/uploads",
    "kind": "upload",
    "creator": "admin",
    "created_at": "2026-10-23T21:00:00+08:00",
    "expires_at": "2026-10-24T21:00:00+08:00",
    "permanent": false,
    "remaining_seconds": 86400,
    "use_count": 0
  },
  "token": "gofs_xxxxxxxxxxxxxxxx",
  "warn": "密钥明文只显示这一次，请立即复制保存；服务端只保存摘要，无法再次查看。",
  "usage": {
    "script": "bash <(curl -sS \"<服务地址>/up?key=gofs_xxx\") 文件或目录... [目标子目录]",
    "header": "curl -T local.txt -H 'X-Gofs-Upload-Key: gofs_xxx' \"<服务地址>/<目标路径>\"",
    "query": "curl -T local.txt \"<服务地址>/<目标路径>?key=gofs_xxx\""
  }
}
```

⚠️ `token` 明文**只返回这一次**，请立即保存。丢失后只能撤销重建。

---

## 3. `PUT /<文件路径>`：流式上传

这是上传大文件的首选方式：请求体就是文件内容，不经过 multipart，也不产生临时文件。

### 3.1 请求

```http
PUT /uploads/2026/report.pdf HTTP/1.1
Host: 127.0.0.1:5000
X-Gofs-Upload-Key: gofs_xxxxxxxx
Content-Length: 2483912
Content-Type: application/octet-stream

<文件二进制>
```

规则：

- URL 路径就是**目标文件路径**，必须以文件名结尾，**不要以 `/` 结尾**。
  Linux 上服务端会直接返回 `400 Bad Request`；macOS / Windows 上路径规范化
  会丢掉尾斜杠，请求可能被当作同名文件处理，因此客户端不要依赖该行为。
- 目标父目录不存在会自动创建（`MkdirAll`），不需要先 `MKCOL`。
- 默认语义是**覆盖写**：同名文件会被截断后写入。
- 单文件大小默认上限 10 GiB（`--upload-max-size`，可在页面设置里调整，`0` 为不限制）。
- 连续 2 分钟没有数据进展会断开（`--upload-read-timeout`）。
- 支持 chunked 请求体；带 `Content-Length` 时超限会被**提前**拒绝，
  这样客户端能收到完整的 `413` 而不是连接被重置。

查询参数：

| 参数 | 说明 |
| --- | --- |
| `key=<token>` | 上传密钥（也可以放请求头） |
| `dated=0` | 跳过日期归档，精确落到 URL 指定位置 |
| `dated=1` | 强制按日期归档，即使目标在子目录里 |
| `conflict=replace` | 同名目标先删除（目录会递归删除），再写入新文件 |
| `conflict=backup` | 同名目标先改名为 `名称-时间戳.扩展名`，再写入新文件 |

`conflict` 对 `X-Update-Range: append` 无效：续传必须保留磁盘上的半截文件，
不能因为带了参数就删掉续传点。

上传前可以用 `HEAD <目标 URL>?stat=1` 只探测路径是否存在：带 `stat=1` 时
服务端会绕过 `--render-spa` 的 HTML 回退，不存在就返回 404，而不是装作 200。
网页端正是用它来发现同名文件，再决定整批「替换 / 备份」。

### 3.2 日期自动归档

默认布局 `2006/01/02`（即 `年/月/日`），且**只对「直接传到服务根目录」的文件生效**：

| 请求路径 | 实际落点 |
| --- | --- |
| `PUT /report.pdf` | `/2026/10/23/report.pdf` |
| `PUT /docs/report.pdf` | `/docs/report.pdf` |
| `PUT /report.pdf?dated=0` | `/report.pdf` |
| `PUT /docs/report.pdf?dated=1` | `/docs/2026/10/23/report.pdf` |

实际落点以响应头 `X-Gofs-Path` 为准。上传密钥会在归档后再次复核 scope，
不会因为日期目录跑到允许范围之外。

### 3.3 断点续传（追加写）

带 `X-Update-Range: append` 时，服务端把请求体**追加**到目标文件末尾，
不再截断：

```http
PUT /uploads/big.iso HTTP/1.1
X-Gofs-Upload-Key: gofs_xxxxxxxx
X-Update-Range: append
Content-Length: 52428800

<从断点开始的剩余内容>
```

⚠️ 服务端不会校验偏移量，它只负责追加。客户端必须自己确认
「磁盘上已有的这 N 字节确实是本次上传写入的」，否则会拼出
「旧内容 + 新尾巴」的坏文件。

推荐流程：

1. `HEAD <目标 URL>` 取当前 `Content-Length`，记为 `offset`；
2. 如果 `offset >= 本地文件大小`，说明已经传完（或目标更大，需重新覆盖）；
3. 否则从本地文件的 `offset` 处开始发送，并带 `X-Update-Range: append`；
4. 校验响应头 `X-Gofs-Offset`（本次写入字节数）：`offset + X-Gofs-Offset == 文件大小`。

> **上传密钥不能发 `HEAD`**（密钥只允许 `PUT`/`POST`）。用上传密钥续传时，
> 偏移量只能由客户端自己记录；需要 `HEAD` 探测请改用账号凭据。

### 3.4 响应

成功：`201 Created`

```http
HTTP/1.1 201 Created
X-Gofs-Offset: 2483912
X-Gofs-Path: /uploads/report.pdf

created /uploads/report.pdf (2483912 bytes)
```

| 响应头 | 含义 |
| --- | --- |
| `X-Gofs-Offset` | **本次请求**写入的字节数（追加模式下不是文件总大小） |
| `X-Gofs-Path` | 文件真实落盘路径（经过日期归档后可能与请求路径不同） |
| `X-Gofs-Appended` | 追加模式下为 `true` |

`X-Gofs-Path` 为 ASCII 安全做了百分号编码，中文文件名需要先解码，
例如 Go 用 `url.PathUnescape`，Python 用 `urllib.parse.unquote`。

### 3.5 预置同名目标：`CONFLICT /<路径>?conflict=...`

网页端上传整个文件夹时，先在**根文件夹**上调用一次 `CONFLICT`，把旧目录整体
替换或备份，再逐个文件普通上传。这样备份得到的是一份完整的旧文件夹，而不是
根目录里每个同名文件各一份 `名称-时间戳` 碎片。

```http
CONFLICT /proj?conflict=backup HTTP/1.1
Authorization: Basic base64(user:pass)
```

| 参数 | 说明 |
| --- | --- |
| `conflict=replace` | 递归删除同名目标（含目录内容） |
| `conflict=backup` | 同名目标改名为 `名称-时间戳`（文件保留扩展名，目录不带扩展名） |

规则：

- 只处理同名目标，不写入任何内容；响应 `204 No Content`。目标不存在时同样
  返回 204（幂等），调用方可以放心地先 `CONFLICT` 再上传。
- 权限与上传一致：要求开启上传（`--allow-upload`）并对目标有写权限；目录会按
  后代逐项复核 ACL，目录只读子项不会被一次替换/备份绕过。
- 不允许对服务根目录执行；非法或缺失的 `conflict` 返回 `400`。
- 路径带尾斜杠时会先去重，`/proj/` 与 `/proj` 等价——备份落在同级的
  `proj-时间戳`，不会嵌进 `/proj/` 内部。

---

## 4. `POST /<目录路径>/`：multipart 表单上传

浏览器 `<input type="file" multiple>`、拖拽上传和目录上传都走这个接口。

### 4.1 请求

```http
POST /uploads/ HTTP/1.1
Host: 127.0.0.1:5000
Content-Type: multipart/form-data; boundary=----gofs
X-Gofs-Upload-Key: gofs_xxxxxxxx

------gofs
Content-Disposition: form-data; name="path"

docs/2026
------gofs
Content-Disposition: form-data; name="file"; filename="a.txt"
Content-Type: text/plain

hello
------gofs
Content-Disposition: form-data; name="file"; filename="b.txt"
Content-Type: text/plain

world
------gofs--
```

表单字段：

| 字段 | 说明 |
| --- | --- |
| `file` | 文件字段，可重复出现，一次请求可带多个文件 |
| `path` | 相对当前 URL 的目标子目录，可选；不允许出现 `..` |
| `dated` | `0` 跳过日期归档，`1` 强制归档，可选 |
| `conflict` | `replace` 先删除同名目标再写入；`backup` 先把同名目标改名为 `名称-时间戳.扩展名` 再写入 |

规则：

- URL 指向**目标目录**（建议以 `/` 结尾），文件落到
  `URL + path` 目录下。
- multipart 的文件名会被压平到基本名：`-F "file=@a/b/c.txt"` 只会得到 `c.txt`。
  要保留层级，必须用 `path` 字段指定子目录。
- 单次表单请求总量硬上限为 **1 GiB**（即使 `--upload-max-size` 更大），
  更大的文件请用流式 `PUT`。
- 超过 32 MiB 的部分会落到系统临时目录，服务端会尽量用 `rename` 落盘，
  跨文件系统时才退回逐字节复制。
- 目标文件逐个做权限 / 软链 / 真实路径复核；**全部校验通过后**才开始写盘。
- 单个文件名超过上限（默认 255 字节）只会跳过该文件，不会让整批失败。

### 4.2 响应

成功：`200 OK`

```json
{
  "saved": ["/uploads/docs/2026/a.txt", "/uploads/docs/2026/b.txt"],
  "count": 2,
  "dest": "/uploads/docs/2026"
}
```

有文件因名字过长被跳过时：

```json
{
  "saved": ["/uploads/a.txt"],
  "count": 1,
  "dest": "/uploads",
  "skipped": ["超长文件名..."],
  "message": "有 1 个文件名超过 255 字节，已跳过"
}
```

`saved` 是相对服务根目录的 URL 路径，不含 `--path-prefix`。

---

## 5. `GET /up?key=<密钥>`：一键上传脚本

用上传密钥换一段可直接执行的 Bash 脚本，密钥和服务地址由服务端填好：

```bash
# 直接执行，上传文件或目录（目录保留层级）
bash <(curl -sS "http://127.0.0.1:5000/up?key=gofs_xxxxxxxx") ./src docs

# 或先存下来
curl -sSLo gofs-up.sh "http://127.0.0.1:5000/up?key=gofs_xxxxxxxx"
chmod +x gofs-up.sh
./gofs-up.sh ./a.txt ./b/ 
```

脚本行为：

- 逐个文件发送 `multipart/form-data` 请求；
- 目录连层级一起上传，靠 `path` 字段保留子目录；
- 最后一个参数若不是已存在的本地路径，就当作目标子目录；
- 遇到 HTTP → HTTPS 跳转会在**同一路径**换协议重发，不跟随 `Location`；
- 结束打印成功 / 失败数量，有失败时退出码非 0，可直接用于 CI。

> 上传单个文件到服务根目录时仍会走日期归档。想精确落点，要么指定目标子目录，
> 要么启动时设 `--upload-date-layout=""`。

---

## 6. 代码示例

### 6.1 curl

```bash
BASE='http://127.0.0.1:5000'
KEY='gofs_xxxxxxxx'

# 1) 单个文件（上传密钥）
#    注意：URL 以 / 结尾时，curl -T 会自动把本地文件名追加到 URL 后面
curl -T ./report.pdf \
  -H "X-Gofs-Upload-Key: $KEY" \
  "$BASE/uploads/report.pdf"

# 2) 单个文件（账号密码）
curl -u admin:secret -T ./report.pdf \
  "$BASE/uploads/report.pdf"

# 3) 关掉日期归档
curl -T ./report.pdf \
  -H "X-Gofs-Upload-Key: $KEY" \
  "$BASE/report.pdf?dated=0"

# 4) multipart 多文件 + 子目录
curl -H "X-Gofs-Upload-Key: $KEY" \
  -F 'path=docs/2026' \
  -F 'file=@./a.txt' \
  -F 'file=@./b.txt' \
  "$BASE/uploads/"

# 5) 目录上传（逐个文件，保留相对路径）
SRC='./src'
find "$SRC" -type f | while IFS= read -r f; do
  rel=${f#"$SRC"/}
  dir=$(dirname "$rel"); [ "$dir" = "." ] && dir=""
  curl -sS -f \
    -H "X-Gofs-Upload-Key: $KEY" \
    -F "path=${dir:+$dir}" \
    -F "file=@$f" \
    "$BASE/uploads/" >/dev/null &&
    echo "  ✓ $rel" ||
    echo "  ✗ $rel" >&2
done

# 6) 断点续传：先查已落盘大小，再追加剩余部分
URL="$BASE/uploads/big.iso"
OFFSET=$(curl -sI -u admin:secret "$URL" |
  tr -d '\r' |
  awk 'tolower($1)=="content-length:" {print $2}')
[ -z "$OFFSET" ] && OFFSET=0
tail -c +$((OFFSET + 1)) ./big.iso |
  curl -T - \
    -H "X-Gofs-Upload-Key: $KEY" \
    -H 'X-Update-Range: append' \
    "$URL"

# 7) 一键脚本
bash <(curl -sS "$BASE/up?key=$KEY") ./src docs
```

### 6.2 Go

只用标准库，可直接放进项目：

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL string // 例如 https://files.example.com，含访问前缀
	Key     string // 上传密钥；也可改用 Basic 认证
	HTTP    *http.Client
}

// Put 上传一个本地文件。offset > 0 时使用追加模式续传。
// 注意：offset 必须是服务端已经落盘的、属于本次上传的字节数。
func (c *Client) Put(ctx context.Context, local, remote string, offset int64) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	if offset < 0 || offset > st.Size() {
		return fmt.Errorf("非法偏移量 %d（文件大小 %d）", offset, st.Size())
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/" + strings.TrimLeft(remote, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size() - offset
	req.Header.Set("X-Gofs-Upload-Key", c.Key)
	if offset > 0 {
		req.Header.Set("X-Update-Range", "append")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("上传失败: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	written, _ := strconv.ParseInt(resp.Header.Get("X-Gofs-Offset"), 10, 64)
	if offset > 0 && offset+written != st.Size() {
		return fmt.Errorf("续传后大小不一致：%d + %d != %d", offset, written, st.Size())
	}
	fmt.Printf("已上传 %s（本次 %d 字节）\n",
		resp.Header.Get("X-Gofs-Path"), written)
	return nil
}

// UploadDir 递归上传目录，保留目录结构。
func (c *Client) UploadDir(ctx context.Context, localDir, remoteDir string) error {
	localDir = filepath.Clean(localDir)
	return filepath.Walk(localDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(localDir, p)
		if err != nil {
			return err
		}
		remote := filepath.ToSlash(filepath.Join(remoteDir, rel))
		if err := c.Put(ctx, p, remote, 0); err != nil {
			return err
		}
		fmt.Println("  ✓", remote)
		return nil
	})
}

func main() {
	c := &Client{
		BaseURL: "http://127.0.0.1:5000",
		Key:     "gofs_xxxxxxxx",
		HTTP:    &http.Client{Timeout: 0}, // 大文件不要设总超时
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if err := c.Put(ctx, "./report.pdf", "/uploads/report.pdf", 0); err != nil {
		panic(err)
	}
	if err := c.UploadDir(ctx, "./src", "/uploads/src"); err != nil {
		panic(err)
	}
}
```

multipart 表单上传（流式，不在内存里缓存整个表单）：

> 需要在上面的 import 里补一个 `"mime/multipart"`。

```go
func (c *Client) PostForm(ctx context.Context, dir string, fields map[string]string, files []string) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()

	go func() {
		for k, v := range fields {
			if err := mw.WriteField(k, v); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		for _, p := range files {
			f, err := os.Open(p)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			part, err := mw.CreateFormFile("file", filepath.Base(p))
			if err == nil {
				_, err = io.Copy(part, f)
			}
			f.Close()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.CloseWithError(mw.Close())
	}()

	url := strings.TrimRight(c.BaseURL, "/") + "/" + strings.TrimLeft(dir, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Gofs-Upload-Key", c.Key)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("表单上传失败: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
	return nil
}
```

调用：

```go
err := c.PostForm(ctx, "/uploads/", map[string]string{
	"path":  "docs/2026",
	"dated": "0",
}, []string{"./a.txt", "./b.txt"})
```

> 用账号凭据续传时，先对目标 URL 发 `HEAD`，从 `Content-Length`
> 得到 `offset`，再调用 `c.Put(ctx, local, remote, offset)`。

### 6.3 Python

依赖 `requests`，示例使用了 `str | None` 类型标注，需要 Python 3.10+：

```python
import os
import requests
from urllib.parse import quote, unquote

BASE = "http://127.0.0.1:5000"
KEY = "gofs_xxxxxxxx"
HEADERS = {"X-Gofs-Upload-Key": KEY}


def upload(local_path: str, remote_path: str) -> None:
    """流式 PUT 上传单个文件。remote_path 是服务端文件路径。"""
    with open(local_path, "rb") as f:
        r = requests.put(
            f"{BASE}/{remote_path.lstrip('/')}",
            data=f,
            headers=HEADERS,
            timeout=(10, 3600),  # (连接, 读取)
        )
    r.raise_for_status()
    print(
        r.status_code,
        unquote(r.headers.get("X-Gofs-Path", "")),
        "本次写入", r.headers.get("X-Gofs-Offset"), "字节",
    )


def upload_multipart(files: list[str], subdir: str = "", dated: str | None = None) -> None:
    """multipart 上传多个文件，subdir 相当于表单的 path 字段。"""
    handles = []
    form = []
    try:
        for p in files:
            fh = open(p, "rb")
            handles.append(fh)
            form.append(("file", (os.path.basename(p), fh)))
        data = {}
        if subdir:
            data["path"] = subdir
        if dated is not None:
            data["dated"] = dated
        r = requests.post(
            f"{BASE}/uploads/",
            files=form,
            data=data,
            headers=HEADERS,
            timeout=(10, 3600),
        )
        r.raise_for_status()
        print(r.json())
    finally:
        for fh in handles:
            fh.close()


def upload_resume(local_path: str, remote_path: str, auth: tuple[str, str]) -> None:
    """用账号凭据 HEAD 查偏移，再用上传密钥追加剩余内容。"""
    url = f"{BASE}/{remote_path.lstrip('/')}"
    size = os.path.getsize(local_path)

    head = requests.head(url, auth=auth, timeout=10)
    offset = int(head.headers.get("Content-Length", "0")) if head.ok else 0
    if offset >= size:
        print("已存在同样大小或更大的文件，跳过")
        return

    with open(local_path, "rb") as f:
        f.seek(offset)
        r = requests.put(
            url,
            data=f,
            headers={**HEADERS, "X-Update-Range": "append"},
            timeout=(10, 3600),
        )
    r.raise_for_status()
    written = int(r.headers.get("X-Gofs-Offset", "0"))
    assert offset + written == size, f"续传后大小不一致: {offset}+{written} != {size}"
    print("续传完成：", unquote(r.headers.get("X-Gofs-Path", "")))


if __name__ == "__main__":
    upload("./report.pdf", "/uploads/report.pdf")
    upload_multipart(["./a.txt", "./b.txt"], subdir="docs/2026", dated="0")
    upload_resume("./big.iso", "/uploads/big.iso", ("admin", "secret"))
```

### 6.4 Node.js

Node 18+。用 `http.request` 可以显式设置 `Content-Length`，
避免走 chunked：

```js
import http from 'node:http';
import { createReadStream, openAsBlob, statSync } from 'node:fs';
import { basename, join } from 'node:path';
import { readdir } from 'node:fs/promises';

const BASE = 'http://127.0.0.1:5000';
const KEY = 'gofs_xxxxxxxx';

function upload(local, remote, offset = 0) {
  return new Promise((resolve, reject) => {
    const size = statSync(local).size;
    if (offset < 0 || offset > size) {
      return reject(new Error(`非法偏移量 ${offset}（文件大小 ${size}）`));
    }
    const u = new URL(`${BASE}/${remote.replace(/^\/+/, '')}`);
    const req = http.request(
      {
        method: 'PUT',
        hostname: u.hostname,
        port: u.port,
        path: u.pathname + u.search,
        headers: {
          'X-Gofs-Upload-Key': KEY,
          'Content-Length': size - offset,
          ...(offset > 0 ? { 'X-Update-Range': 'append' } : {}),
        },
      },
      (res) => {
        let body = '';
        res.setEncoding('utf8');
        res.on('data', (c) => (body += c));
        res.on('end', () => {
          if (res.statusCode !== 201) {
            return reject(new Error(`HTTP ${res.statusCode}: ${body.trim()}`));
          }
          const written = Number(res.headers['x-gofs-offset'] || 0);
          if (offset > 0 && offset + written !== size) {
            return reject(new Error(`续传后大小不一致: ${offset}+${written} != ${size}`));
          }
          console.log('已上传', decodeURIComponent(res.headers['x-gofs-path']), `${written} 字节`);
          resolve();
        });
      },
    );
    req.on('error', reject);
    createReadStream(local, { start: offset }).pipe(req);
  });
}

// multipart 表单（Node 20+，openAsBlob 读取文件避免整份进内存）
async function uploadForm(files, subdir = '') {
  const fd = new FormData();
  if (subdir) fd.append('path', subdir);
  for (const p of files) {
    fd.append('file', await openAsBlob(p), basename(p));
  }
  const res = await fetch(`${BASE}/uploads/`, {
    method: 'POST',
    headers: { 'X-Gofs-Upload-Key': KEY },
    body: fd,
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${await res.text()}`);
  console.log(await res.json());
}

// 目录递归上传（保留相对路径）
async function uploadDir(localDir, remoteDir = '') {
  const entries = await readdir(localDir, { withFileTypes: true });
  for (const e of entries) {
    const p = join(localDir, e.name);
    if (e.isDirectory()) {
      await uploadDir(p, `${remoteDir}${e.name}/`);
    } else if (e.isFile()) {
      const remote = `${remoteDir}${e.name}`;
      await upload(p, `/uploads/${remote}`);
    }
  }
}

await upload('./report.pdf', '/uploads/report.pdf');
await uploadForm(['./a.txt', './b.txt'], 'docs/2026');
await uploadDir('./src', 'src/');
```

> `openAsBlob` 来自 `node:fs`，需要 Node 20+。
> 低版本可以改用 `new Blob([await readFile(p)])`，但文件会整份进内存，
> 只适合小文件；大文件请走 `PUT`。

### 6.5 浏览器（XHR + 进度 + 续传）

登录一次，拿到 HttpOnly 会话 Cookie，之后上传由浏览器自动携带凭据：

```html
<input type="file" id="picker" multiple>

<script>
const BASE = ''; // 同源部署留空；跨域部署请用绝对地址并自行处理 CORS

function basicAuth(user, pass) {
  const bytes = new TextEncoder().encode(`${user}:${pass}`);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return 'Basic ' + btoa(bin);
}

async function login(user, pass) {
  const res = await fetch(`${BASE}/fs/auth`, {
    method: 'POST',
    headers: { Authorization: basicAuth(user, pass) },
    credentials: 'same-origin',
  });
  if (!res.ok) throw new Error('登录失败: HTTP ' + res.status);
}

function encodePath(p) {
  return p.split('/').map(encodeURIComponent).join('/');
}

// PUT 上传，支持从 offset 续传。
function putFile(file, dir, offset = 0, onProgress) {
  return new Promise((resolve, reject) => {
    const url = `${BASE}${encodePath(dir.replace(/\/+$/, ''))}/${encodeURIComponent(file.name)}`;
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', url, true);
    xhr.withCredentials = true; // 带上会话 Cookie
    if (offset > 0) xhr.setRequestHeader('X-Update-Range', 'append');

    if (xhr.upload && onProgress) {
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) onProgress(offset + e.loaded, file.size);
      };
    }
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve({
          path: xhr.getResponseHeader('X-Gofs-Path'),
          written: Number(xhr.getResponseHeader('X-Gofs-Offset') || 0),
        });
      } else {
        reject(new Error(`HTTP ${xhr.status} ${xhr.responseText.trim()}`));
      }
    };
    xhr.onerror = () => reject(new Error('网络错误'));
    xhr.onabort = () => reject(new Error('已取消'));

    xhr.send(offset > 0 ? file.slice(offset) : file);
  });
}

// 大文件断点续传：HEAD 查已落盘大小，再追加。
async function putWithResume(file, dir) {
  const url = `${BASE}${encodePath(dir.replace(/\/+$/, ''))}/${encodeURIComponent(file.name)}`;
  let offset = 0;
  try {
    const head = await fetch(url, {
      method: 'HEAD',
      credentials: 'same-origin',
    });
    if (head.ok) offset = Number(head.headers.get('Content-Length') || 0);
  } catch (_) { /* 忽略，从头传 */ }

  if (offset >= file.size) return { path: url, written: 0 };
  const out = await putFile(file, dir, offset, (done, total) => {
    console.log(`进度 ${((done / total) * 100).toFixed(1)}%`);
  });
  if (offset > 0 && offset + out.written !== file.size) {
    throw new Error('续传后大小不一致，请重新上传');
  }
  return out;
}

// multipart 多文件上传（不保留目录结构；目录上传请逐层拼 path）
async function postFiles(files, dir, subdir = '') {
  const fd = new FormData();
  for (const f of files) fd.append('file', f, f.name);
  if (subdir) fd.append('path', subdir);
  const res = await fetch(`${BASE}${encodePath(dir.replace(/\/+$/, ''))}/`, {
    method: 'POST',
    body: fd,
    credentials: 'same-origin',
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${await res.text()}`);
  return res.json();
}

document.getElementById('picker').addEventListener('change', async (e) => {
  await login('admin', 'secret'); // 实际项目里应由用户输入
  for (const file of e.target.files) {
    await putWithResume(file, '/uploads');
    console.log('上传完成:', file.name);
  }
});
</script>
```

> **浏览器跨域限制**：默认 CORS 的 `Access-Control-Allow-Headers` 不包含
> `X-Gofs-Upload-Key`，且跨站写请求会被 CSRF 同源校验拒绝。
> 跨域场景建议由同源后端代理上传，或直接使用账号登录后的同源页面。
> 同源页面里也可以把密钥放进 `?key=` 查询参数。

---

## 7. 错误码

错误响应体是**纯文本**（`Content-Type: text/plain`），不是 JSON。
成功响应：`PUT` 为 `201`，`POST` 为 `200`。

| 状态码 | 常见原因 |
| --- | --- |
| `400` | PUT 目标以 `/` 结尾（Linux）；POST 不是 `multipart/form-data`；没有文件；`path` 含 `..`；文件名过长 |
| `401` | 未带凭据；账号密码错误；上传密钥无效 / 已过期 / 已撤销 |
| `403` | 未开启 `--allow-upload`；目标路径没有写权限；密钥 scope 外；用只读分享令牌上传；目标是符号链接；跨站写请求；**用 `--allow-insecure-auth=false` 收紧后，在明文 HTTP 上提交账号密码** |
| `409` | PUT 的目标已存在且是目录；路径在请求期间被替换 |
| `413` | 超过单文件上传上限；multipart 表单总量超过 1 GiB |
| `429` | 认证失败次数过多（按来源 IP + 账号），看 `Retry-After`；服务端先验口令，正确凭据不会拿到 429 |
| `500` | 服务端写盘失败，具体原因只进服务端日志 |
| `503` | 并发上传任务过多，响应带 `Retry-After: 2`，可稍后重试；或解压安全上限未配置（`--extract-max-files` / `--extract-max-total` / `--extract-max-ratio` 任一为 0 时拒绝解压） |

常见错误文本：

```text
403 Forbidden: 未开启上传（--allow-upload）
403 Forbidden: 只读权限
403 Forbidden: 该密钥只允许上传到 /uploads，或创建者已无对应权限
413 Payload Too Large: 超过单文件上限 10.0 GiB（10737418240 字节）；管理员可在「服务设置」里调整该上限
503 Service Unavailable: 上传任务过多，请稍后重试
```

---

## 8. 注意事项

1. **上传需要目标路径的写权限。** 只开 `--allow-upload` 还不够；
   启用鉴权后，账号或密钥创建者必须对目标路径有 `rw` 权限。
2. **日期归档只认「直接传到服务根目录」。** 上传到 `/docs/a.txt` 不会归档；
   上传到 `/a.txt` 默认落到 `/年/月/日/a.txt`。脚本需要精确落点时加
   `?dated=0`（PUT）或 `-F dated=0`（POST），或直接指定目标子目录。
3. **multipart 有 1 GiB 硬上限。** 更大的文件请用流式 `PUT`，
   它不产生临时文件，默认上限 10 GiB。
4. **multipart 会压平文件名。** 保留目录层级必须用 `path` 字段，
   不能指望 `filename` 里带 `/`。
5. **PUT 默认覆盖写。** 断点续传是显式的 `X-Update-Range: append`，
   且服务端不校验偏移量，客户端必须自己确认偏移正确。
6. **上传密钥不能读。** 不能 `GET`/`HEAD`/`DELETE`/`MKCOL`，
   所以用密钥续传时无法自行探测远端大小；需要探测就改用账号凭据。
7. **浏览器写请求需要同源。** curl / 服务端程序不受影响；
   跨站页面发起的上传会被 CSRF 校验拒绝。
8. **保留路径 `/fs/*`、`/up`、`/s/*`。** 这些是服务内部接口和分享入口，
   不要把业务文件传到这些前缀下，以免与接口路由冲突。
9. **访问前缀要拼进 URL。** 启用 `--path-prefix /files` 后，
   上传地址是 `http://host:5000/files/<文件路径>`，而不是 `/<文件路径>`。
10. **失败重试策略。** `4xx` 重试没有意义（请求本身有问题）；
    网络中断、`5xx` 与 `429` 可以重试：优先遵循 `Retry-After`（网页端最多等 10 秒），
    否则从 250ms 起指数退避（封顶 3 秒），每个文件最多重试 2 次。网页端对小文件
    直接整文件重传，大文件用追加模式续传；连续 5 个任务失败时暂停上传 worker 1 秒，
    避免继续打满拥塞链路。脚本客户端请自行实现同样的策略。
