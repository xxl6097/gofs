# WebDAV 功能使用指南

## 概述

已为 gofs 项目添加完整的 WebDAV 服务器功能，支持：

- ✅ WebDAV 账号管理（创建、删除、更新）
- ✅ 目录访问控制（为每个账号指定可访问的目录）
- ✅ 安全密码存储（PBKDF2 加密）
- ✅ RESTful API 管理接口
- ✅ 数据持久化（JSON 文件存储）

## 文件结构

```
internal/webdav/
├── webdav.go          # WebDAV 服务器核心
├── webdav_test.go     # 单元测试
├── password.go        # 密码哈希和验证
├── password_test.go   # 密码测试
├── storage.go         # 数据持久化
└── api.go             # HTTP API 接口

examples/webdav/
├── main.go            # 示例程序
└── README.md          # 详细文档
```

## 快速开始

### 1. 构建示例程序

```bash
cd examples/webdav
go build -o webdav-server main.go
```

### 2. 启动服务器

```bash
# 基本用法
./webdav-server

# 指定参数
./webdav-server -root /path/to/data -port 8080 -accounts /path/to/accounts.json
```

启动后访问 http://localhost:8080 查看管理面板。

### 3. 创建 WebDAV 账号

```bash
curl -X POST http://localhost:8080/api/webdav/accounts \
  -H "Content-Type: application/json" \
  -d '{
    "username": "alice",
    "password": "secret123",
    "allowed_dirs": ["/docs", "/shared"]
  }'
```

### 4. 连接 WebDAV

**WebDAV 地址:** `http://localhost:8080/dav/`

**客户端连接方法：**

- **Windows:** 文件资源管理器 → 映射网络驱动器
- **macOS:** Finder → 连接服务器 (⌘K)
- **Linux:** 文件管理器 → 连接到服务器

## API 接口

### 列出所有账号
```bash
GET /api/webdav/accounts
```

### 创建账号
```bash
POST /api/webdav/accounts
Content-Type: application/json

{
  "username": "bob",
  "password": "pass456",
  "allowed_dirs": ["/projects"]
}
```

### 更新账号权限
```bash
PUT /api/webdav/accounts/bob
Content-Type: application/json

{
  "allowed_dirs": ["/projects", "/archive"]
}
```

### 修改密码
```bash
POST /api/webdav/accounts/bob/password
Content-Type: application/json

{
  "new_password": "newpass789"
}
```

### 删除账号
```bash
DELETE /api/webdav/accounts/bob
```

## 权限控制说明

每个账号可以指定多个允许访问的目录：

```json
{
  "username": "user1",
  "allowed_dirs": ["/docs", "/shared", "/projects/team1"]
}
```

**访问规则：**
- 账号只能访问配置的目录及其子目录
- 使用前缀匹配机制
- 访问未授权目录返回 403 Forbidden
- 路径遍历攻击自动拦截

**示例：**
- 允许 `/docs` → 可访问 `/docs/` 及所有子目录
- 不允许 `/admin` → 访问 `/admin/` 会被拒绝
- `/docs/../admin` → 自动规范化后检查，仍然被拒绝

## 安全性

### 密码存储
- 算法：PBKDF2-HMAC-SHA256
- 迭代次数：210,000
- 盐长度：16 字节（随机生成）
- 密钥长度：32 字节
- 存储格式：`pbkdf2$iterations$salt$hash`

### 认证方式
- HTTP Basic Authentication
- 常量时间密码比较（防止时序攻击）
- 建议生产环境使用 HTTPS

### 访问控制
- 基于路径的前缀匹配
- 自动路径规范化
- 防止路径遍历攻击
- 所有路径必须在服务根目录内

## 数据存储

账号数据默认保存在：
```
~/.config/gofs/webdav_accounts.json
```

文件格式：
```json
{
  "version": 1,
  "accounts": [
    {
      "username": "alice",
      "password_hash": "pbkdf2$210000$...$...",
      "allowed_dirs": ["/docs", "/shared"],
      "created_at": 1234567890,
      "updated_at": 1234567890
    }
  ]
}
```

## 命令行客户端使用

```bash
# 上传文件
curl -u alice:secret123 -T file.txt http://localhost:8080/dav/docs/file.txt

# 下载文件
curl -u alice:secret123 -O http://localhost:8080/dav/docs/file.txt

# 创建目录
curl -u alice:secret123 -X MKCOL http://localhost:8080/dav/docs/newfolder/

# 删除文件
curl -u alice:secret123 -X DELETE http://localhost:8080/dav/docs/file.txt

# 列出目录（PROPFIND）
curl -u alice:secret123 -X PROPFIND http://localhost:8080/dav/docs/
```

## 编程接口

### Go 客户端示例

```go
import "github.com/studio-b12/gowebdav"

client := gowebdav.NewClient("http://localhost:8080/dav/", "alice", "secret123")

// 上传
data := []byte("Hello WebDAV")
client.Write("docs/hello.txt", data, 0644)

// 下载
content, _ := client.Read("docs/hello.txt")

// 列出目录
files, _ := client.ReadDir("docs/")
```

### Python 客户端示例

```python
from webdav3.client import Client

options = {
    'webdav_hostname': 'http://localhost:8080/dav/',
    'webdav_login': 'alice',
    'webdav_password': 'secret123'
}

client = Client(options)
client.upload_sync(remote_path='docs/file.txt', local_path='local.txt')
client.download_sync(remote_path='docs/file.txt', local_path='download.txt')
```

## 集成到主项目

如需将 WebDAV 集成到主 gofs 服务器：

```go
import (
    "github.com/xxl6097/gofs"
    "github.com/xxl6097/gofs/internal/webdav"
    "net/http"
)

func main() {
    // 创建 gofs 主服务器
    srv, _ := gofs.New(
        gofs.WithRoot("./data"),
        gofs.WithPort(8080),
    )
    
    // 创建 WebDAV 服务器
    webdavServer, _ := webdav.New("./data")
    storage := webdav.NewStorage("./webdav_accounts.json")
    
    // 加载已有账号
    accounts, _ := storage.Load()
    webdavServer.LoadAccounts(accounts)
    
    // 创建 API
    api := webdav.NewAPI(webdavServer, storage)
    
    // 注册路由
    mux := http.NewServeMux()
    mux.Handle("/", srv.Handler())
    mux.Handle("/dav/", http.StripPrefix("/dav", webdavServer.Handler()))
    mux.HandleFunc("/api/webdav/accounts", func(w http.ResponseWriter, r *http.Request) {
        switch r.Method {
        case http.MethodGet:
            api.HandleListAccounts(w, r)
        case http.MethodPost:
            api.HandleCreateAccount(w, r)
        }
    })
    // ... 其他路由
    
    http.ListenAndServe(":8080", mux)
}
```

## 测试

运行单元测试：
```bash
go test ./internal/webdav/... -v
```

所有测试已通过：
- ✅ 密码哈希和验证
- ✅ WebDAV 认证
- ✅ 路径权限控制
- ✅ 账号管理（CRUD）

## 常见问题

**Q: Windows 无法连接 HTTP WebDAV？**

A: Windows 默认只支持 HTTPS WebDAV。需要修改注册表：
```
HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\WebClient\Parameters
BasicAuthLevel = 2（DWORD）
```
然后重启 WebClient 服务。

**Q: 如何启用 HTTPS？**

A: 方法1：使用 nginx 反向代理终止 SSL
方法2：修改代码使用 `http.ListenAndServeTLS`

**Q: 可以限制上传大小吗？**

A: 可以在 HTTP 处理器中添加中间件检查 Content-Length。

**Q: 支持哪些 WebDAV 操作？**

A: 支持所有标准 WebDAV 方法：
- PROPFIND（列出目录）
- GET（下载文件）
- PUT（上传文件）
- DELETE（删除）
- MKCOL（创建目录）
- COPY（复制）
- MOVE（移动/重命名）
- LOCK/UNLOCK（锁定，可选）

## 依赖

```bash
go get golang.org/x/net/webdav
```

## 性能考虑

- 密码验证使用 PBKDF2（210,000 次迭代），首次验证约需 100ms
- 推荐生产环境使用密码缓存或会话管理
- WebDAV 使用内存锁系统（可选外部存储）

## 后续改进建议

1. 添加会话管理减少密码验证开销
2. 支持配额限制（每账号存储空间）
3. 添加访问日志和审计
4. 支持 LDAP/OAuth 等外部认证
5. Web 管理界面（当前仅有 API）

## 许可证

与 gofs 主项目相同。
