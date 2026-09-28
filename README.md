# gofs

参照 [sigoden/dufs](https://github.com/sigoden/dufs) 的设计，用 Go 重写的一个实用文件服务器，
并**额外实现了压缩包在线解压**。

单二进制、零第三方依赖（只用 Go 标准库）、内嵌前端，`go build` 出来直接就能跑。

## 特性

**基础能力（对齐 dufs）**

- 静态文件服务，目录浏览支持 HTML / JSON / 纯文本三种视图
- 上传：拖拽上传、表单上传、`PUT` 直传、`X-Update-Range: append` 追加续传，
  并**默认按 年/月/日 三级目录自动归档**（可关闭或自定义布局）
- 删除、新建目录（`MKCOL`）、移动 / 重命名（`MOVE`）
- 递归搜索（`?q=`）
- 目录打包下载为 zip（可选压缩级别）：**不勾选就打包整个目录，
  勾选了就只打包所选**，选中的目录会连同内容一起递归打包
- 单文件下载支持 HTTP Range 断点续传、`?hash` 取 SHA-256
- 基于路径的账号权限控制（`user:pass@/dir:rw,/dir2`），最长前缀匹配，
  页面走 **HTTP Basic 认证**（自带登录框，不弹浏览器原生对话框）
- 隐藏路径 glob、访问路径前缀、CORS、HTTPS、unix socket
- **手机端交互适配**：触摸目标 ≥40px、无 hover 依赖、操作收进「更多」菜单、
  阻断 iOS 输入框自动缩放（详见下文）
- **面向大文件与恶意使用的防护**：流式传输（内存与文件大小无关）、
  「无进展」超时（慢速大文件不被误杀）、断点续传、CSRF / 暴力破解 /
  慢速攻击 / 资源耗尽防护、存储型 XSS 与日志注入防护（详见下文）
- 自定义日志格式（`$remote_addr` `$status` `$http_user_agent` …）
- 可替换的自定义前端资源（`--assets`）
- 内嵌 SPA / index.html 渲染模式（`--render-index` / `--render-try-index` / `--render-spa`）

**在线文本编辑**

- 支持 txt / markdown / html / json / xml / yaml / toml / ini / csv 以及各语言源码
- 点文件名直接进编辑器；行号、Tab 缩进、回车自动续缩进、`Ctrl/⌘ + S` 保存
- **Markdown 渲染预览**（自研渲染器，支持标题/列表/表格/代码块/引用/任务列表）
- **HTML 沙箱预览**、**JSON 格式化与着色**、**XML 结构校验**
- 保存时用**内容摘要做乐观锁**，文件被别人改过会提示而不是静默覆盖
- 保留原文件的 **CRLF/LF 换行风格**与 **UTF-8 BOM**，不会把整份文件的换行都改掉
- 二进制文件、超大文件、UTF-16 编码一律拒绝，避免改坏文件

**上传密钥**

- 给脚本 / 第三方的最小权限凭证：**只能上传**，不能读、删、改、解压
- 可限定目标目录、可设有效期（1 小时 ~ 永不过期）、可随时撤销
- 明文只在创建时显示一次，服务端只存 SHA-256 摘要
- 创建者的权限是上限：只对 `/docs` 有写权限的账号签不出覆盖全盘的密钥

**在线解压（本项目的增量）**

- 支持 `zip` / `tar` / `tar.gz` / `tgz` / `gz`，按文件头嗅探而非只看扩展名
- **内容预览**：不发请求落盘，直接在网页里列出压缩包清单，并可流式取出单个文件
- **在线解压**：解压到服务端磁盘，通过 SSE 实时推送进度（当前条目 / 文件数 / 字节数 / 跳过数）
- 安全防护：Zip Slip、软链逃逸、解压炸弹、权限位提权
- 目录列表里自动给压缩包打「可解压」标记

## 快速开始

```bash
# 构建
go build -o gofs .

# 以只读模式服务当前目录
./gofs

# 服务指定目录，并开放全部操作（含在线解压）
./gofs -A ./data

# 明确只开解压，不开上传删除
./gofs --allow-extract ./uploads

# 需要账号密码：admin 全目录读写，guest 只读
./gofs -a admin:123@/:rw -a guest:guest@/ -A ./data

# 只开在线编辑，不开上传删除
./gofs --allow-edit ./docs

# 完整示例：鉴权 + 编辑 + 解压 + 密钥
./gofs -A -a admin:123@/:rw -b 127.0.0.1 -p 5000 ./data
```

启动后会打印当前生效的配置：

```
gofs 0.1.0
  监听      : http://127.0.0.1:5000
  服务目录  : /home/me/data
  权限      : upload=on edit=on delete=on search=on archive=on extract=on keys=on
  鉴权      : 已启用（1 条规则），页面使用 Basic 认证
  上传密钥  : /home/me/.config/gofs/keys.json
```

启动后访问 `http://127.0.0.1:5000`。

## 命令行选项

```
gofs [选项] [serve-path]

  [serve-path]               要服务的路径，默认 "."

  -b, --bind <addrs>         监听地址或 unix socket
  -p, --port <port>          监听端口，默认 5000
      --path-prefix <path>   访问路径前缀
      --hidden <value>       目录列表中隐藏的路径 glob，例如 tmp,*.log
  -a, --auth <rules>         鉴权规则，例如 user:pass@/dir1:rw,/dir2
  -A, --allow-all            允许所有操作
      --allow-upload         允许上传
      --allow-edit           允许在线编辑文本文件（默认跟随 --allow-upload）
      --edit-max-size <n>    在线编辑允许打开的最大文件尺寸，默认 2MiB
      --allow-keys           允许管理上传密钥（默认跟随 --allow-upload）
      --key-file <path>      上传密钥存储路径，默认 <用户配置目录>/gofs/keys.json；
                             置空则只保存在内存中
      --allow-delete         允许删除
      --allow-search         允许搜索
      --allow-archive        允许目录打包为 zip 下载
      --allow-extract        允许在线解压压缩包
      --allow-symlink        允许符号链接指向根目录之外
      --enable-cors          开启 CORS
      --render-index         目录下 index.html 作为首页，不存在返回 404
      --render-try-index     优先 index.html，无则目录列表
      --render-spa           SPA 模式，未命中回退 index.html
      --assets <path>        自定义前端资源目录
      --log-format <format>  自定义 HTTP 日志格式（none 可关闭）
      --log-file <file>      日志文件
      --compress <level>     打包压缩级别：none/low/medium/high，默认 low
      --upload-date-layout <layout>
                             上传时自动归档的日期目录布局（Go 时间布局字面量），
                             默认 "2006/01/02"，即在目标目录下按 年/月/日 建三级目录；
                             置空（--upload-date-layout=""）则关闭
      --no-upload-dated      关闭上传按日期自动归档
      --tls-cert <path>      HTTPS 证书
      --tls-key <path>       HTTPS 私钥
      --extract-max-total <n> 解压写出字节上限，默认 10GiB
      --extract-max-files <n> 解压文件数上限，默认 100000
      --extract-max-ratio <n> 单文件压缩比上限，默认 200
  -V, --version              打印版本
  -h, --help                 打印帮助
```

所有选项都可以用 `GOFS_` 前缀的环境变量设置，例如
`GOFS_PORT=8080 GOFS_ALLOW_EXTRACT=true ./gofs ./data`。
多条鉴权规则在环境变量里用 `|` 分隔：

```bash
GOFS_AUTH='admin:123@/:rw|guest:guest@/' ./gofs ./data
```

## 在线编辑文本

目录列表里可编辑的文件会显示「编辑」按钮，**点击文件名也能直接打开编辑器**。

### 支持范围

按扩展名与文件名识别（`internal/textfile`），覆盖 txt / log / md / markdown / rst /
html / vue / svelte / css / scss / less / js / ts / jsx / tsx / json / jsonc / yaml /
xml / svg / toml / ini / conf / properties / env / csv / tsv / sql / sh / bash /
py / go / java / kt / c / cpp / h / cs / rs / rb / php / lua / r / swift / dart /
tf / hcl / proto / graphql … 以及 `Makefile` `Dockerfile` `.gitignore` `.env` 等无扩展名文件。

判定分两步：

1. **列表阶段只看文件名**，不读内容——列举目录时不该产生大量磁盘 IO；
2. **打开时再做内容嗅探**：含 NUL 字节或大量非法 UTF-8 一律判定为二进制并拒绝，
   防止编辑器把图片当文本打开后再保存，把文件彻底毁掉。

### 编辑器能力

- 行号、`Tab` 缩进（多行选区整体缩进 / 反缩进）、回车自动延续上一行缩进
- `Ctrl/⌘ + S` 保存，`Esc` 关闭（有未保存改动会二次确认）
- 状态栏显示行数、字符数、换行风格、BOM、文件大小与修改状态
- 超过 5000 行自动关闭行号渲染，避免大文件把 DOM 撑爆

### 预览

| 类型 | 预览方式 |
| --- | --- |
| Markdown | 自研渲染器，支持标题 / **粗体** / *斜体* / 行内代码 / 围栏代码块 / 列表 / 任务列表 / 表格 / 引用 / 分隔线 / 链接 / 图片 |
| HTML | `iframe sandbox=""`（**禁止脚本、禁止同源**，否则预览自己上传的 HTML 会让其中的脚本拿到凭据） |
| JSON | 格式化 + 语法着色，非法时指出解析错误 |
| XML | `DOMParser` 结构校验，非法时指出出错位置 |

Markdown 渲染前先对所有文本做 HTML 转义，链接还会过滤 `javascript:` / `data:` 协议。

### 安全与一致性

**乐观锁用内容摘要而非时间戳。** 打开文件时返回 `hash`（SHA-256 前 16 位），
保存时带上；服务端比对当前内容摘要，不一致返回 `409` 并附带 `current_hash`，
由用户决定是否强制覆盖。用时间戳会在「同一毫秒内两次保存」时误判。

**换行风格会被保留。** 浏览器的 `<textarea>` 提交时会把 CRLF 规范化成 LF，
若直接写回，Windows 换行文件的每一行都会变，在 git diff 里表现为整个文件被改。
服务端检测原文件风格并在写回时还原。

**BOM 会被保留。** 读取时剥离 UTF-8 BOM 交给编辑器，写回时重新加上。
UTF-16 编码的文件直接拒绝编辑（客户端无法正确处理）。

**写入是原子的。** 先写同目录临时文件再 `rename`，不会出现写到一半的残缺文件。

### API

```bash
# 读取
curl -u admin:123 'http://127.0.0.1:5000/__gofs__/text?path=/README.md'

# 保存（base_hash 来自读取响应）
curl -u admin:123 -X PUT http://127.0.0.1:5000/__gofs__/text \
  -H 'Content-Type: application/json' \
  -d '{"path":"/README.md","content":"# 新内容\n","base_hash":"3bca2438f174ef9a"}'

# 冲突时强制覆盖
curl -u admin:123 -X PUT http://127.0.0.1:5000/__gofs__/text \
  -H 'Content-Type: application/json' \
  -d '{"path":"/README.md","content":"...","base_hash":"...","force":true}'
```

## 上传密钥

给脚本或第三方一个**只能上传**的凭证，不用交出账号密码。

登录后点顶栏「密钥」即可管理。密钥的能力被刻意收得很窄：

| 能做什么 | 不能做什么 |
| --- | --- |
| `PUT` / `POST` 上传文件到指定目录 | 读取目录列表或文件内容 |
| 受日期归档规则约束 | 删除、重命名、新建目录 |
| — | 解压压缩包、在线编辑、管理其它密钥 |

最后一条尤其重要：**密钥不能用来管理密钥**，否则一把只能传文件的凭证
就能给自己签发新凭证，等于权限提升。

### 有效期

创建时可选 1 小时 / 6 小时 / 1 天 / 7 天 / 30 天 / 永不过期。
列表里显示每把密钥的状态、剩余时长、最后使用时间与使用次数，可随时撤销。

### 安全设计

- **明文只显示一次**。服务端只保存 `SHA-256(明文)`，`keys.json` 泄露也无法直接使用。
  界面上只展示可读前缀（`gofs_AbCd1234…`）供人工核对。
- **密钥文件权限 0600**，默认放在用户配置目录（`~/.config/gofs/keys.json`），
  不放在服务目录里——否则它会出现在自己的目录列表中。
- **校验用定长时间比较**，避免通过响应耗时逐字节猜测。
- **创建者的权限是上限**：签发时会检查调用者对该目录是否有写权限，
  只对 `/docs` 有权限的账号签不出覆盖 `/` 的密钥。
- **作用范围在归一化路径上比对**，`//uploads`、`/uploads/../etc` 这类写法绕不过去。

### 用法

```bash
# 上传（推荐用请求头）
curl -T backup.tar.gz \
  -H 'X-Gofs-Upload-Key: gofs_xxxxxxxx' \
  http://127.0.0.1:5000/incoming/backup.tar.gz

# 或写在查询参数里，便于一行搞定
curl -T backup.tar.gz 'http://127.0.0.1:5000/incoming/backup.tar.gz?key=gofs_xxxxxxxx'
```

失败时：密钥无效或过期返回 `401`，超出允许目录返回 `403`。

### API

```bash
GET    /__gofs__/keys                 列出全部密钥（不含明文）
POST   /__gofs__/keys                 创建，body {"name","scope","ttl_seconds"}
                                      响应中的 token 只返回这一次
DELETE /__gofs__/keys?id=<id>         撤销
```

## 认证

页面使用 **HTTP Basic 认证**，但不依赖浏览器原生登录框。

原生框有几个毛病：无法登出（凭据被浏览器缓存，通常要重启浏览器才失效）、
无法承载「登录后回到原页面」这类逻辑、移动端体验差、样式与应用割裂。
所以这里由应用自己弹登录框，凭据存在 `sessionStorage`（勾选「记住我」则存
`localStorage`），每次请求通过 `Authorization` 头带上。

### 未登录时的三种行为

| 请求类型 | 行为 | 原因 |
| --- | --- | --- |
| 浏览器导航目录（`Accept: text/html`，无 API 参数） | 返回**应用外壳** `200`，带 `auth_required: true` | 前端才有机会弹自己的登录框 |
| 应用内部请求（带 `X-Gofs-Ajax: 1`） | `401`，**不带**挑战头 | 避免弹出原生框打断应用流程 |
| 直接访问文件（如 `/a.txt`） | `401` + `WWW-Authenticate` 挑战 | 这时原生框反而是最自然的 |

外壳里**不含任何真实数据**：目录列表是空的，权限位全为 false，
上传归档等配置也不下发，未登录者拿不到任何文件名或目录结构。

### 刷新页面为什么不用重新登录

浏览器不会因为我们用 JS 把凭据存进 `sessionStorage` 就自动给页面请求加
`Authorization` 头。所以按 F5 时，服务端拿到的仍是一个**没有凭据的普通导航**，
只能返回空壳——里面既没有用户名、权限位也全是 false。

前端的处理是：

1. 页面加载时立刻从 `sessionStorage` / `localStorage` 读出凭据，
   **先用里面的用户名把界面填上**（否则会闪出「匿名」，登出按钮也会消失）；
2. 调一次 `GET /__gofs__/auth?path=<当前路径>`，用本地凭据换回真实的身份、
   权限与配置，然后用这些重新渲染；
3. 如果这一步返回 `401`，说明凭据已失效 —— 清掉本地存储并弹出登录框。

**登录成功后同样不做页面跳转**，而是前端自己 `fetch` 数据渲染 ——
跳转会陷入「跳转后浏览器不带凭据」的死循环。

### 受限账号在子目录里也能拿到完整按钮

`/__gofs__/auth` 支持 `?path=` 参数，按指定路径计算权限。
一个只被授予 `/docs:rw` 的账号，站在 `/docs` 里应该看到完整的操作按钮，
而不是被当成只读——不传 `path` 时才按根目录计算。
路径里含 `..` 会被直接忽略，不会借机提升权限。

### 命令行

`curl -u user:pass` 照常可用；`--anyauth` 也能走挑战流程。

## 上传按年月日归档

**默认开启**：拖拽或上传的文件不会直接堆在当前目录，而是自动放进 `年/月/日` 三级目录。

```
拖拽 photo.jpg 到根目录      →  /2026/09/28/photo.jpg
拖拽 run.log 到 /projects/logs →  /projects/logs/2026/09/28/run.log
```

设计上有几点考虑：

- **归档基准是「当前浏览的目录」**，不是服务根。在哪个目录上传，就在那个目录下建三级日期目录，
  这样多项目混用时目录不会交叉。
- **连续拖拽不会越陷越深**。上传完成后页面停留在原目录（不会跳进日期目录），
  否则第二次拖拽就会生成 `2026/09/28/2026/09/28/`。
- **上传前会明确告知落点**。拖拽浮层显示目标路径，上传弹窗里也标注「按日期归档：文件将存入 …」，
  上传完成的提示带一个「查看」按钮，一键跳到日期目录。
- **权限按最终路径复核**。路径重写后会重新走一遍 ACL 判定，避免绕过路径级权限。

### 自定义布局

`--upload-date-layout` 接受任意 Go 时间布局字面量：

```bash
gofs --upload-date-layout "2006/01/02" ./data   # 年/月/日（默认）
gofs --upload-date-layout "2006-01-02" ./data   # 2026-09-28
gofs --upload-date-layout "2006/01" ./data      # 只到月份
gofs --upload-date-layout "2006/01/02/15" ./data # 再加小时
gofs --no-upload-dated ./data                   # 关闭，文件直接落在当前目录
```

布局会在启动时用参照时刻试格式化做校验，含 `..`、绝对路径、反斜杠或冒号的写法会被直接拒绝。

### 单次跳过

不想归档时，加 `?dated=0`（也接受 `false` / `no` / `off`）：

```bash
curl -T local.txt 'http://127.0.0.1:5000/direct.txt?dated=0'
```

表单上传可用同名字段：`dated=0`。

### 响应头

`PUT` 成功后会返回文件真实落盘的路径，方便脚本确认位置：

```
HTTP/1.1 201 Created
X-Gofs-Path: /projects/logs/2026/09/28/run.log
X-Gofs-Offset: 2048
```

`X-Gofs-Path` 是百分号编码的（HTTP 头只能承载 ASCII），中文文件名不会变成乱码：

```
X-Gofs-Path: /2026/09/28/%E6%8A%A5%E5%91%8A.pdf
```

## 打包下载

顶栏的「打包下载」按钮有两种行为，取决于当前有没有勾选条目：

| 当前状态 | 按钮文案 | 打包内容 | 下载文件名 |
| --- | --- | --- | --- |
| 未勾选任何条目 | 打包下载 | 当前目录的全部内容 | `<目录名>.zip` |
| 勾选了 N 项 | 打包所选 (N) | 只有勾选的条目 | 单项 `<名字>.zip`，多项 `<目录名>-selected.zip` |

勾选目录时会**递归**打包其内容（包内保留目录本身作为条目，解压后结构不变）。
因此「不勾选直接打包」与「全选后打包」得到的结果是一致的 —— 测试里专门做了这项对照。

### HTTP API

```bash
# 打包整个目录（默认）
curl -OJ 'http://localhost:5000/docs?zip'

# 只打包选中的条目：GET 用重复的 pick= 参数
curl -OJ 'http://localhost:5000/docs?zip&pick=/docs/readme.md&pick=/docs/guide.txt'

# 选中很多时用 POST，避免 URL 超长（浏览器侧走的就是这条）
curl -OJ -X POST 'http://localhost:5000/docs?zip' \
  -H 'Content-Type: application/json' \
  -d '{"picks":["/docs/readme.md","/docs/sub"]}'
```

`pick` 以 `/` 开头时按「相对服务根」解析，否则按「相对当前目录」解析。
不支持逗号分隔 —— 文件名本身可以含逗号，切分会造成歧义。

响应头：

```
X-Gofs-Archive-Mode: all | picked      # 打包的是全部还是所选
X-Gofs-Archive-Entries: 11             # 包内条目数
X-Gofs-Archive-Bytes: 2099             # 原始字节数
X-Gofs-Archive-Skipped: 2              # 被跳过的选中项数（仅 picked 模式）
```

### 安全设计

每个选中项都要过三关，任一不过就被跳过（通过 `X-Gofs-Archive-Skipped` 告知，
前端会提示「有 N 项被跳过（不存在或没有读取权限）」）：

1. **不含 `..`** —— 显式拒绝。`path.Clean` 会把 `../../a` 静默改写成 `a`，
   虽未真正逃逸，但掩盖了调用方的意图（与解压侧的处理保持一致）
2. **能解析到服务根之内** —— 由路径解析器拦掉越界与符号链接逃逸
3. **逐项复核读权限** —— 路径级 ACL 同样生效，不能借打包把没权限的内容带出去

全部选中项都不可用时返回 **400**（而不是给一个空 zip），便于排查路径写错。

刻意**不**要求选中项必须位于当前目录之下：站内搜索是跨目录的，
强制落在当前目录会把「搜索后打包所选」这个用法直接堵死。权限由逐项复核保证，
包内路径优先取「相对当前目录」，不在其下的（搜索命中）退回「相对服务根」，
避免同名文件互相覆盖。

软链接一律不打包（可能指向服务根之外）。

## 压缩包在线解压

### 网页上用

1. 目录列表里带「可解压」标记的文件，右侧会多出 **解压** 和 **内容** 两个按钮
2. **内容** → 弹出压缩包清单，可逐个「取出」单个文件（不落盘，流式读取）
3. **解压** → 填写目标目录，确认后弹出实时进度条

### HTTP API

**列出压缩包内容**

```bash
curl 'http://127.0.0.1:5000/__gofs__/extract?path=/backup.zip'
```

响应：

```json
{
  "path": "/backup.zip",
  "format": "zip",
  "default_dest": "/backup",
  "total": 3,
  "entries": [
    {"name": "a.txt", "size": 12, "compressed_size": 14, "is_dir": false, "modtime": "2026-09-28T11:00:00+08:00"}
  ]
}
```

**取出包内单个文件（流式，不解压落盘）**

```bash
curl -O 'http://127.0.0.1:5000/__gofs__/extract?path=/backup.zip&file=a.txt'
```

**执行解压（SSE 进度流）**

```bash
curl -N -X PUT http://127.0.0.1:5000/__gofs__/extract \
  -H 'Content-Type: application/json' \
  -d '{"path":"/backup.zip","dest":"/restored","overwrite":false}'
```

返回 `text/event-stream`，逐条推送：

```
event: start
data: {"src":"/backup.zip","dest":"/restored","format":"zip","total":1203}

event: progress
data: {"current":"logs/2026-09/app.log","files":512,"dirs":8,"skipped":0,"bytes":8388608,"total":1203}

event: done
data: {"report":{"files":1203,"dirs":64,"bytes":104857600,"skipped":2,"warnings":["已跳过链接条目 ..."],"elapsed_ms":1840},"url":"/restored/"}
```

出错时推 `event: error`，并在 `data.partial` 里带上已完成的部分统计。

也支持用查询参数调用，方便一行 curl：

```bash
curl -N -X PUT 'http://127.0.0.1:5000/__gofs__/extract?path=/backup.zip&dest=/restored&overwrite'
```

### 安全设计

在线解压面对的是不受信任的输入，因此做了这几层防护：

| 风险 | 处理方式 |
| --- | --- |
| **Zip Slip**（`../../x` 写到目标目录外） | 每个条目路径先统一分隔符、拒绝绝对路径与盘符，再 `SafeJoin` 校验必须落在目标目录内；越界条目跳过并记入 `warnings` |
| **软链接逃逸**（tar 里的 symlink 指向 `/etc/passwd`） | `TypeSymlink` / `TypeLink` 条目一律跳过，不落盘 |
| **解压炸弹**（高压缩比 + 小体积） | 三重限制：条目数上限、写出总字节上限、单文件压缩比上限；实时累计，超限立即中止 |
| **权限位提权**（包内 4755 的 setuid 文件） | 忽略包内 mode，文件恒为 `0644`、目录恒为 `0755` |
| **路径歧义** | `dest` / `path` 中出现 `..` 段直接 400 拒绝，而不是静默规范化 |
| **覆盖已有数据** | 目标目录已存在且非空时必须显式 `overwrite=true`；非覆盖模式跳过同名文件 |
| **磁盘打满** | 写出过程用 `io.Writer` 包装器实时拦截，超限即熔断，不会先写满再检查 |

上限可通过 `--extract-max-total` / `--extract-max-files` / `--extract-max-ratio` 调整。

### 关于加密 zip

暂不支持。遇到 `zip` 的加密标记（general purpose bit 0）会直接返回明确错误，
而不是解出一堆损坏文件。如需支持可引入 ZipCrypto 解密。

### 关于中文文件名

`zip` 规范里文件名编码有歧义：Windows 上用 GBK 打包的 zip，Go 标准库不会自动转码。
本项目不引入第三方编码库以保持零依赖，因此这类 zip 的文件名可能显示为乱码
（`tar` / `tar.gz` 不受影响）。如需处理，可引入 `golang.org/x/text/encoding/simplifiedchinese`。

## 安全与防护

文件服务器最容易被击穿的三处是**磁盘、内存与文件描述符**。
下面这些默认值的目标是：被恶意使用时服务退化（返回 413 / 429 / 503），
而不是崩溃或把机器拖垮。全部可调，`0` 通常表示不限制。

### 大文件：能传多大，怎么保证传得完

**流式，不占内存。** 上传与下载都走固定缓冲的 `io.Copy`
（下载用 `http.ServeContent`），内存占用与文件大小无关。
实测 1 GiB 上传/下载各耗时 1 秒上下，进程内存无明显增长。

**超时按「有没有进展」算，不按总时长。** 这是支持大文件的关键取舍：

| 做法 | 后果 |
| --- | --- |
| 固定总时长（如「30 分钟必须传完」） | 10 GB 在 1 MB/s 的链路要 3 小时，会被硬生生掐断 |
| **无进展超时**（默认 2 分钟） | 只要还在传就一直续期，慢速大文件也能传完；只有真正停滞（连上不发/不读）才断开 |

实测：客户端每 0.5 秒读一块、总耗时 8 秒（阈值 2 秒）的慢速下载**完整传完**；
而连上后完全停止读取的连接会在阈值后被断开（读到残留数据后 EOF）。
上传同理 —— 停滞的连接会被及时释放，不占着并发名额。

**断点续传。** 上传中断后不必从头再来：

```bash
# 1) 首传（覆盖写）。中断后服务端留下的是本次内容的前缀
curl -T big.iso http://host/upload/big.iso
# 2) 问服务端已经有多少字节
curl -sI http://host/upload/big.iso | grep -i content-length
# 3) 从断点续传（服务端追加到末尾）
curl -X PUT -H 'X-Update-Range: append' --data-binary @<(tail -c +$((OFFSET+1)) big.iso) \
     http://host/upload/big.iso
```

安全性来自「首传是覆盖写」（`O_TRUNC`）：失败后残留的那部分**一定是本次的内容**，
接着往后写不会把别的文件拼进来。响应头 `X-Gofs-Offset` 是本次写入的字节数，
可以用它确认最终大小等于源文件大小。

**下载支持 Range**，所以客户端的续传、多线程下载、播放器拖动都是现成的。
前端上传大文件走的是 XHR `PUT`（流式直传），**不经 multipart 临时文件**，
所以不占额外的临时磁盘空间；网页端在中断后会自动按上面的协议续传。

### 上传防护

| 限制 | 默认 | 为什么 |
| --- | --- | --- |
| 单次上传字节数 | 10 GiB | 没有它，一个请求就能把磁盘写满 |
| 上传读超时（无进展） | 2 分钟 | 防「每秒发几个字节」的连接长期占名额 |
| 并发上传/解压任务 | 64 | 这两类操作长时间占连接与磁盘 IO |
| 文件名长度 | 255 字节 | 超长名交给文件系统会变成 500，提前拒绝更好 |

带 `Content-Length` 的超限请求会在**读任何数据之前**就被拒绝 —— 否则客户端
此时还在往外发数据，它收到的往往是连接重置，而不是那条 413 说明。
超限后若已写了半截文件会被清理掉（追加模式除外，那是续传的续接点）。

### 攻击面

**跨站写请求（CSRF）。** Basic 凭据由浏览器自动附带，恶意页面只要能让浏览器发一个
请求，就能借用户已登录的会话删文件。所以写方法（POST/PUT/DELETE/MKCOL/MOVE）会校验
`Origin`/`Referer` 与请求 Host 是否同源：

- 浏览器发起的跨站请求**一定**带这两个头之一 → 403；
- curl / 脚本这类非浏览器客户端通常不带 → 不受影响；
- 反向代理改写 Host 而保留 Origin 时可能误判，用 `--no-csrf-protect` 关闭。

**认证暴力破解。** Basic 认证可以被无限次尝试，按来源统计失败次数，
超过阈值（默认 10 次 / 5 分钟）后临时封禁并返回 429。
只有「带了凭据但不对」才算失败 —— 完全没带凭据只是未登录，
否则一个不带凭据的爬虫就能把正常用户的来源封掉。成功登录会清零计数。

**慢速攻击。** `ReadHeaderTimeout` 10 秒掐断「连上后慢慢发请求头」（Slowloris）；
请求头总量限制 64 KiB（标准库默认 1 MiB，请求头是每个连接都要缓冲的）。

**资源耗尽。**

| 面 | 限制 | 说明 |
| --- | --- | --- |
| 并发请求 | 512 | 兜住文件描述符与内存，取不到名额时短暂排队后返回 503 |
| 目录列举 | 20000 条 | 十万级文件的目录一次性序列化会吃掉大量内存；响应带 `truncated` 与 `total_all` |
| 打包 | 20 万条 / 50 GiB | 扫描阶段就累计检查，超限返回 413 而不是把内存撑爆 |
| 解压 | 10 GiB / 10 万条 / 200 倍压缩比 | 防解压炸弹；超限时清理残缺文件 |
| `?hash` | 512 MiB | 算摘要要读完整个文件，对超大文件开放等于送出一个 CPU/IO 耗尽入口 |
| 认证失败记录 | 惰性清理 | 避免被大量伪造来源撑爆内存 |

**存储型 XSS。** 文件服务器天然会把上传的 `.html` / `.svg` 原样吐回浏览器，
而浏览器会把它当作**本站页面**渲染 —— 一个能上传文件的人只要放一个
`<script>fetch('/__gofs__/keys',{method:'POST'…})</script>` 再诱导管理员打开，
就能借管理员的登录态为所欲为。所以：

- 所有文件响应带 `X-Content-Type-Options: nosniff`（禁止按内容猜类型）；
- HTML / SVG / XML 额外带 `Content-Security-Policy: sandbox`，
  页面仍可正常预览渲染，但里面的脚本读不到本站数据。需要连脚本一起预览时用
  `--no-html-sandbox`。

**日志注入。** URL、User-Agent 以及**文件名**都会进日志。文件名这一点容易被忽略 ——
Linux 上文件名可以含换行，于是 `rm -rf $'a\n127.0.0.1 - - "GET /admin" 200'`
这样一个名字就能在日志里伪造出额外一行。所有日志行在写出前会清理控制字符与 ANSI 转义。

### 防护选项一览

```
--upload-max-size <n>      单次上传最大字节数，默认 10GiB；0 表示不限制
--upload-read-timeout <d>  上传无进展多久断开，默认 2m
--download-timeout <d>     下载无进展多久断开，默认 2m；0 表示不限制
--list-max-entries <n>     目录列举上限，默认 20000
--archive-max-items <n>    打包条目上限，默认 200000
--archive-max-bytes <n>    打包字节上限，默认 50GiB
--max-concurrent <n>       同时处理的请求数上限，默认 512
--max-concurrent-jobs <n>  同时进行的上传/解压数上限，默认 64
--auth-fail-limit <n>      认证失败多少次后封禁，默认 10；0 表示关闭
--auth-fail-window <d>     失败统计窗口与封禁时长，默认 5m
--max-header-bytes <n>     请求头上限，默认 64KiB
--max-name-bytes <n>       单个文件名最大字节数，默认 255
--hash-max-size <n>        ?hash 摘要的最大文件尺寸，默认 512MiB
--no-csrf-protect          关闭写操作的跨站来源校验
--no-html-sandbox          关闭 HTML/SVG/XML 的 CSP sandbox
```

启动时会打印当前生效的防护值，便于确认。

## 与 dufs 的差异

| 项 | dufs | gofs |
| --- | --- | --- |
| 语言 | Rust | Go |
| 在线解压 | 仅支持目录打包下载 | 支持 zip / tar / tar.gz / gz 的解压、预览、单文件取出 |
| 解压进度 | — | SSE 实时推送 |
| 在线编辑 | 支持（简单文本框） | 支持，带行号、Markdown/HTML/JSON/XML 预览、摘要乐观锁、换行与 BOM 保留 |
| 上传密钥 | — | 支持，可限目录 / 设有效期 / 撤销，只存摘要 |
| 登录体验 | 浏览器原生 Basic 弹窗 | 应用内登录框（仍是 Basic 协议） |
| 上传归档 | — | 默认按 年/月/日 三级目录归档，可自定义布局 |
| WebDAV | 支持 | 未实现（仅实现了 MKCOL / MOVE 等基础方法） |
| Digest 鉴权 | 支持 | 仅 Basic（支持明文密码） |
| 哈希密码 | 支持 `$6$` sha512-crypt | 未实现 |
| 配置文件 | 支持 YAML | 仅命令行与环境变量 |
| 依赖 | 多个 crate | 零第三方依赖 |

## 项目结构

```
gofs/
├── main.go                      入口：参数解析、监听、优雅关闭
├── internal/
│   ├── config/config.go         配置与命令行/环境变量解析
│   │   └── config_test.go       日期归档布局等参数解析测试
│   ├── auth/auth.go             路径级账号权限（最长前缀匹配、定长时间比较）
│   ├── fsutil/fsutil.go         受限路径解析、目录列举、递归搜索
│   ├── textfile/                文本类型识别与文本处理
│   │   ├── textfile.go          扩展名/文件名 → 语言、二进制嗅探、换行与 BOM
│   │   └── textfile_test.go
│   ├── uploadkey/               上传密钥
│   │   ├── uploadkey.go         密钥存储、摘要校验、有效期、持久化
│   │   └── uploadkey_test.go
│   ├── archive/                 压缩包统一抽象
│   │   ├── archive.go           格式嗅探、安全拼接、配额限制器
│   │   ├── zip.go               zip 读取与解压
│   │   ├── tar.go               tar / tar.gz 读取与解压
│   │   └── gz.go                单文件 gzip
│   └── server/                  HTTP 层
│       ├── server.go            路由、权限判定、中间件
│       ├── handler_auth.go      登录校验
│       ├── handler_key.go       上传密钥管理
│       ├── handler_text.go      在线编辑读写
│       ├── handler_file.go      PUT / DELETE / MKCOL / MOVE
│       ├── handler_list.go      目录渲染、搜索、内嵌资源
│       ├── handler_archive.go   打包为 zip（全部 / 所选）
│       ├── handler_extract.go   在线解压（SSE）
│       ├── log.go               日志
│       └── server_test.go       集成测试
├── assets/                      内嵌前端（原生 HTML/CSS/JS，无框架）
│   ├── index.html
│   ├── style.css
│   ├── app.js                   目录浏览、上传、认证、Toast、弹窗
│   ├── editor.js                文本编辑器与四种预览
│   └── keys.js                  上传密钥管理界面
└── scripts/
    ├── gen_testdata.py          生成测试用压缩包
    ├── test-ui.mjs              jsdom 真实页面交互测试
    └── measure-layout.mjs       用 CDP 量表格列宽（真浏览器布局回归）
```

## 开发

```bash
go build ./...          # 构建
go vet ./...            # 静态检查
gofmt -l .              # 格式检查
go test ./...           # 全部测试（142 个用例）
go test ./internal/server/ -v   # 查看详细用例
```

### 前端 UI 测试

后端集成测试覆盖不到「页面上的按钮到底出没出现」这类问题，
所以另有一个用 jsdom 跑真实页面的脚本：

```bash
# 终端 A：以鉴权模式启动
./gofs -A -a admin:secret@/:rw ./data

# 终端 B：跑一遍用户视角的交互
node scripts/test-ui.mjs http://127.0.0.1:5000
```

它会真的加载页面、等待登录框、填表单提交，然后断言：

- 登录后**所有功能入口都可见**（上传 / 新建 / 密钥 / 打包下载 / 下载 / 编辑 / 解压 / 内容 / 重命名 / 删除 / 打开）
- 点击可编辑文件能打开编辑器，且内容、行号、状态栏、保存按钮都在
- 预览面板能展开并产出内容
- **编辑后未保存时，退出 / 右上角 ✕ 都会弹出确认框**（点取消改动仍在，点放弃才关闭）
- **刷新页面后登录态保持**（用户名不会退化成「匿名」，凭据失效时回到登录框）
- **打包按钮随选中变化**：「打包下载」↔「打包所选 (N)」，且点击时分别发出
  GET（全部）/ POST（带 picks 的所选）请求
- **手机端「更多」菜单**：每行都有该按钮、点开后菜单项与行内按钮一一对应
  （同一份操作定义）、点选后菜单关闭
- **跨模块接口完整性**（`window.GOFS` 的 28 个约定接口逐个校验）
- **确认弹窗的返回值语义**（点「确定」必须得到 `true`，点「取消」得到 `false`）

> **为什么值得单独测这些：** 有三类 bug 只在特定路径上出现，`go vet`
> 和接口测试都发现不了，用户只会反馈“按钮点了没反应”：
>
> 1. **权限快照** —— 权限对象在脚本加载时被做成快照，登录后没更新，
>    导致除「下载」「打开」外的**全部按钮消失**，看起来像功能没实现；
> 2. **接口漏暴露** —— 某个 `G.xxx` 忘了赋值，调用处直接抛异常；
> 3. **确认弹窗返回值竞态** —— 按钮回调先 `close` 再 `resolve`，
>    `onClose` 里的 `resolve(false)` 抢先定值，用户点「确定」也只拿到 `false`，
>    于是「强制覆盖」「撤销密钥」全部静默失效。

测试分布：`server` 114 个、`uploadkey` 13 个、`textfile` 9 个、`config` 6 个。

覆盖内容：目录列举三种视图、搜索、**打包下载**（全部 / `?pick=` 所选 / POST 请求体 /
选中目录递归 / 跨目录命中 / `..` 拒绝 / 全部无效报错 / 部分跳过计数 / 路径级 ACL /
软链跳过 / 文件名策略）、四种格式的预览与解压、
Zip Slip / 软链 / 炸弹 / 配额 / 路径穿越防护、上传与追加、**上传日期归档**
（默认开启 / 自定义布局 / `?dated=0` 跳过 / 表单上传 / 中文名响应头编码）、
**在线编辑**（读取 / 保存 / 摘要冲突 / 强制覆盖 / CRLF 与 BOM 保留 / NUL 拒绝 /
二进制与 UTF-16 拒绝 / 体积上限 / 只读账号 / 路径穿越）、
**认证**（登录校验 / 未登录外壳 / Ajax 不挑战 / 文件导航挑战 / 外壳不泄露配置 /
受限账号可登录）、**上传密钥**（创建列表撤销 / 上传 / 范围限制 / 过期 / 伪造 /
不能读删改解压 / 不能管理密钥 / 签发范围受创建者权限约束 / 与日期归档协同）、
MKCOL/MOVE/DELETE、隐藏路径、路径前缀、HTML 页面注入、
**服务端防护**（上传超限与残片清理 / 追加超限保留续传点 / 跨站写请求拦截与同源放行 /
认证失败封禁与到期恢复 / 目录截断 / 打包上限 / 超长文件名 /
CSP sandbox 与 nosniff / 摘要大小上限 / 并发闸门 / 日志控制字符清理）等。

### 手机端适配

页面在手机上不是「缩小版桌面」，几处交互按触摸设备的实际约束重做过。

**操作入口收进「更多」菜单。** 一行最多有 5 个操作（下载/解压/内容/重命名/删除），
行内平铺会把文件名挤到只剩 100px 出头，而且每个按钮都远小于可点尺寸。
所以 ≤560px 时行内只留一个 `⋯`，点开是占满宽度的操作列表（每项 48px 高）。
宽屏时这个按钮由 CSS 隐藏——**两种呈现共用同一份操作定义**，
不会出现「桌面点得到、手机点不到」的功能差异。

**触摸目标一律 ≥40px。** 顶栏图标按钮 32→40、操作按钮、勾选框（用 `label`
把整格变成可点区域）、品牌 logo、面包屑、文件名（加垂直留白）。
判据分两级：按钮类要求两个方向都够大，文字链接只要求高度
（`.env` 这种文件名天然很窄，强求宽度没意义）。

**根除三类只在触摸设备上出现的毛病：**

| 症状 | 原因 | 做法 |
| --- | --- | --- |
| 操作按钮看不清 | `.ops` 靠 `tr:hover` 才变不透明，触摸设备没有 hover | `@media (hover: none)` 下恒为不透明 |
| 整页横向溢出 60px | `.topbar-actions` 是 flex 项目，默认不缩到比内容窄，内部换行没机会生效 | 加 `min-width: 0` + 内部 `flex-wrap` |
| 输入框一聚焦页面就放大 | iOS 对 `font-size < 16px` 的输入框会自动缩放，且很难缩回去 | 触摸设备下输入框与编辑器统一 16px |

改编辑器字号时**必须同步改行号栏**：两者共用 1.65 行高，只改一边会让行号与代码
逐行错开（`editor.js` 只用 `scrollTop` 同步滚动，没有硬编码行高，所以调字号是安全的）。

**回归命令：**

```bash
make touch-sweep     # 12 个宽度 × 触摸模拟：无横向溢出 / 目标 ≥40px / 无半透明
make touch-modals    # 单独校验编辑器与操作菜单里的按钮
```

### 表格布局回归（真浏览器）

上面两个测试都碰不到**渲染后的几何**：列宽够不够、内容有没有画到隔壁单元格上，
是 CSS 与 `table-layout` 决定的，jsdom 不做布局计算。所以另有一个用 CDP 直连
headless Chrome 的测量脚本：

```bash
# 终端 A：起一个带调试端口的 headless Chrome
make cdp

# 终端 B：扫一遍所有响应式断点
make layout-sweep          # 或单测一个宽度：make layout W=1280
```

每个宽度都会校验三件事，任一不过就退出码非 0：

1. **表头可见列数 == 数据行可见列数** —— 只给 `th` 加 `display:none` 而漏掉 `td`，
   `table-layout: fixed` 的列宽分配会整体错位（实测能把操作列从 196px 挤到 34px）
2. **操作按钮没有溢出自己的单元格** —— 溢出会直接画到「修改时间」列上
3. **按钮没有折行** —— 折行不覆盖，但行高会变大

加 `TOUCH=1`（或直接 `make touch-sweep`）时再补两条：**页面无横向溢出**、
**可点目标达到最小边长**、**可点元素不能是半透明**。

目标开了鉴权时用 `MEASURE_AUTH=user:pass make layout`。

背后的几条 CSS 约束（改动列宽前请先读）：

- `.op-btn` 有 `flex: none`，**不会被压缩**。最宽的一行是压缩包：
  下载 / 解压 / 内容 / 重命名 / 删除，实测需要 **318px**（图标模式下 166px），
  所以 `.col-ops` 在宽屏给 352px、窄屏给 196px；
  触摸设备上按钮被放大到 40px，图标模式要 216px，故再放宽到 252px。
- `.ops` 有 `flex-wrap: wrap` 兜底：宽度实在不够时折行，绝不允许压到相邻列。
- 手机宽度（≤560px）改用「更多」菜单，`.col-ops` 缩到 64px，把宽度还给文件名。

> 这条测量链路是有来历的：用户反馈「修改时间列和操作列重合」，
> 表面看只是列宽不够，量下去才发现是两个叠加的问题 ——
> 宽屏下 250px 的列装不下 318px 的按钮（向左溢出），
> 窄屏下隐藏时间列时只隐藏了 `th`，导致列数错位、整个表格的列宽分配都乱了。

## License

MIT
