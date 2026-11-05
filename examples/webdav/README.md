# WebDAV 功能

为 gofs 添加的 WebDAV 服务器功能，支持创建 WebDAV 账号并为每个账号指定可访问的目录。

## 功能特性

- ✅ **账号管理**：创建、删除、更新 WebDAV 账号
- ✅ **目录权限控制**：为每个账号指定可访问的目录列表
- ✅ **密码安全**：使用 PBKDF2 算法加密存储密码
- ✅ **持久化存储**：账号数据保存到 JSON 文件
- ✅ **RESTful API**：完整的 HTTP API 管理接口
- ✅ **标准 WebDAV 协议**：兼容所有主流 WebDAV 客户端

## 快速开始

### 1. 构建示例程序

```bash
cd examples/webdav
go build -o webdav-server
```

### 2. 启动服务器

```bash
# 使用当前目录作为根目录
./webdav-server

# 或指定根目录和端口
./webdav-server -root /path/to/data -port 8080

# 指定账号数据文件位置
./webdav-server -accounts /path/to/accounts.json
```

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

**Windows:**
- 打开文件资源管理器
- 右键点击"此电脑" → "映射网络驱动器"
- 输入地址：`http://localhost:8080/dav/`
- 输入用户名和密码

**macOS:**
- 打开 Finder
- 菜单栏选择"前往" → "连接服务器"（⌘K）
- 输入地址：`http://localhost:8080/dav/`
- 输入用户名和密码

**Linux:**
```bash
# 使用 davfs2
sudo mount -t davfs http://localhost:8080/dav/ /mnt/webdav

# 或使用文件管理器的"连接到服务器"功能
```

## API 接口

### 列出所有账号

```bash
GET /api/webdav/accounts

# 响应
{
  "accounts": [
    {
      "username": "alice",
      "allowed_dirs": ["/docs", "/shared"],
      "created_at": 1234567890,
      "updated_at": 1234567890
    }
  ]
}
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

## 使用示例

### 命令行客户端

```bash
# 使用 curl 上传文件
curl -u alice:secret123 -T file.txt http://localhost:8080/dav/docs/file.txt

# 下载文件
curl -u alice:secret123 -O http://localhost:8080/dav/docs/file.txt

# 创建目录
curl -u alice:secret123 -X MKCOL http://localhost:8080/dav/docs/newfolder/

# 删除文件
curl -u alice:secret123 -X DELETE http://localhost:8080/dav/docs/file.txt
```

### Python 客户端

```python
from webdav3.client import Client

options = {
    'webdav_hostname': 'http://localhost:8080/dav/',
    'webdav_login': 'alice',
    'webdav_password': 'secret123'
}

client = Client(options)

# 上传文件
client.upload_sync(remote_path='docs/file.txt', local_path='local_file.txt')

# 下载文件
client.download_sync(remote_path='docs/file.txt', local_path='downloaded.txt')

# 列出目录
files = client.list('docs/')
for file in files:
    print(file)
```

### Go 客户端

```go
package main

import (
    "github.com/studio-b12/gowebdav"
)

func main() {
    client := gowebdav.NewClient("http://localhost:8080/dav/", "alice", "secret123")
    
    // 上传文件
    data := []byte("Hello WebDAV")
    client.Write("docs/hello.txt", data, 0644)
    
    // 下载文件
    content, _ := client.Read("docs/hello.txt")
    println(string(content))
    
    // 列出目录
    files, _ := client.ReadDir("docs/")
    for _, file := range files {
        println(file.Name())
    }
}
```

## 目录权限说明

每个账号可以指定多个允许访问的目录：

```json
{
  "username": "user1",
  "allowed_dirs": ["/docs", "/shared", "/projects/team1"]
}
```

- 账号只能访问列表中的目录及其子目录
- 路径匹配采用前缀匹配
- 访问未授权的目录会返回 403 Forbidden
- 至少需要指定一个允许的目录

**示例：**
- 允许 `/docs`：可以访问 `/docs/` 及其所有子目录
- 不允许 `/admin`：访问 `/admin/` 会被拒绝
- 路径遍历保护：即使指定 `/docs/../admin` 也会被拒绝

## 安全性

### 密码存储
- 使用 PBKDF2-HMAC-SHA256 算法
- 210,000 次迭代
- 随机盐（16 字节）
- 存储格式：`pbkdf2$iterations$salt$hash`

### 认证
- HTTP Basic Authentication
- 建议在生产环境使用 HTTPS
- 支持所有标准 WebDAV 客户端

### 权限控制
- 基于路径的访问控制
- 前缀匹配机制
- 防止路径遍历攻击
- 目录必须在服务根目录内

## 文件存储

账号数据默认保存在：
```
~/.config/gofs/webdav_accounts.json
```

格式示例：
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

## 集成到 gofs

如果要将 WebDAV 功能集成到主 gofs 服务器：

```go
import (
    "github.com/xxl6097/gofs"
    "github.com/xxl6097/gofs/internal/webdav"
)

func main() {
    // 创建 gofs 服务器
    srv, _ := gofs.New(
        gofs.WithRoot("./data"),
        gofs.WithPort(8080),
    )
    
    // 创建 WebDAV 服务器
    webdavServer, _ := webdav.New("./data")
    storage := webdav.NewStorage("./webdav_accounts.json")
    api := webdav.NewAPI(webdavServer, storage)
    
    // 注册路由
    mux := http.NewServeMux()
    mux.Handle("/", srv.Handler())
    mux.Handle("/dav/", http.StripPrefix("/dav", webdavServer.Handler()))
    mux.HandleFunc("/api/webdav/accounts", api.HandleListAccounts)
    // ... 其他路由
    
    http.ListenAndServe(":8080", mux)
}
```

## 依赖

```bash
go get golang.org/x/net/webdav
```

## 测试

运行测试：
```bash
cd internal/webdav
go test -v
```

## 常见问题

**Q: 为什么 Windows 连接失败？**

A: Windows 默认不支持 HTTP WebDAV，需要修改注册表：
```
HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\WebClient\Parameters
BasicAuthLevel = 2
```

**Q: 如何使用 HTTPS？**

A: 使用反向代理（如 nginx）终止 SSL，或在代码中启用 TLS：
```go
http.ListenAndServeTLS(":8443", "cert.pem", "key.pem", handler)
```

**Q: 可以限制上传文件大小吗？**

A: 可以在 HTTP 处理器中添加中间件限制请求体大小。

**Q: 支持哪些 WebDAV 操作？**

A: 支持所有标准操作：PROPFIND, GET, PUT, DELETE, MKCOL, COPY, MOVE, LOCK, UNLOCK 等。

## 许可证

与 gofs 主项目相同的许可证。
