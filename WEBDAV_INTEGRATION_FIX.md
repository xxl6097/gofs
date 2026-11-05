# WebDAV 功能集成指南

## 问题诊断

当前问题：前端显示 "加载失败: 404 Not Found"

**原因：** 后端 API 路由 `/api/webdav/accounts` 还未注册到服务器。

## 解决方案

### 方案 1：修改主服务器文件（推荐）

找到 gofs 的主服务器初始化代码（可能在 `server.go` 或 `gofs.go` 中），添加 WebDAV 路由注册。

#### 步骤 1：导入 WebDAV 包

```go
import (
    "github.com/xxl6097/gofs/internal/webdav"
    "path/filepath"
    "os"
)
```

#### 步骤 2：在服务器初始化时注册路由

```go
// 在服务器的 HTTP 处理器设置中添加

// 确定账号文件路径
accountsFile := filepath.Join(os.Getenv("HOME"), ".config", "gofs", "webdav_accounts.json")

// 创建 WebDAV 服务器
webdavServer, err := webdav.New(cfg.Root) // cfg.Root 是服务根目录
if err != nil {
    log.Printf("WebDAV 初始化失败: %v", err)
} else {
    // 创建存储
    storage := webdav.NewStorage(accountsFile)
    
    // 加载已有账号
    accounts, err := storage.Load()
    if err != nil {
        accounts = make(map[string]*webdav.Account)
    }
    webdavServer.LoadAccounts(accounts)
    
    // 创建 API
    api := webdav.NewAPI(webdavServer, storage)
    
    // 注册路由
    mux.Handle("/dav/", http.StripPrefix("/dav", webdavServer.Handler()))
    
    mux.HandleFunc("/api/webdav/accounts", func(w http.ResponseWriter, r *http.Request) {
        // 可选：添加管理员权限检查
        switch r.Method {
        case http.MethodGet:
            api.HandleListAccounts(w, r)
        case http.MethodPost:
            api.HandleCreateAccount(w, r)
        default:
            http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
        }
    })
    
    mux.HandleFunc("/api/webdav/accounts/", func(w http.ResponseWriter, r *http.Request) {
        path := strings.TrimPrefix(r.URL.Path, "/api/webdav/accounts/")
        if path == "" {
            http.Error(w, "用户名不能为空", http.StatusBadRequest)
            return
        }
        
        if strings.HasSuffix(path, "/password") && r.Method == http.MethodPost {
            api.HandleChangePassword(w, r)
            return
        }
        
        switch r.Method {
        case http.MethodPut:
            api.HandleUpdateAccount(w, r)
        case http.MethodDelete:
            api.HandleDeleteAccount(w, r)
        default:
            http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
        }
    })
}
```

### 方案 2：使用独立示例程序（快速测试）

如果主服务器集成较复杂，可以先使用独立示例程序测试功能：

```bash
# 1. 构建示例程序
cd examples/webdav
go build -o webdav-server main.go

# 2. 启动服务器
./webdav-server -root /path/to/data -port 8080

# 3. 访问管理界面
open http://localhost:8080
```

示例程序已经包含了完整的路由注册，可以直接使用。

### 方案 3：使用辅助函数（简化集成）

使用我刚创建的 `integration.go` 文件：

```go
import "github.com/xxl6097/gofs/internal/webdav"

// 在服务器初始化代码中
accountsFile := filepath.Join(configDir, "webdav_accounts.json")
err := webdav.RegisterWebDAVRoutes(mux, cfg.Root, accountsFile)
if err != nil {
    log.Printf("WebDAV 路由注册失败: %v", err)
}
```

## 查找需要修改的文件

根据 gofs 项目结构，路由注册代码可能在以下位置：

1. `server.go` - 主服务器文件
2. `gofs.go` - 库入口文件
3. `internal/server/*.go` - 服务器实现文件
4. `cmd/gofs/main.go` - 命令行入口

### 查找方法

```bash
# 查找路由注册的位置
grep -rn "HandleFunc\|Handle\|mux\|ServeMux" . --include="*.go" | grep -v vendor | grep -v test

# 查找服务器初始化代码
grep -rn "New\|ListenAndServe" . --include="*.go" | grep -v vendor | grep -v test
```

## 验证集成

### 1. 检查路由是否注册

启动服务器后，测试 API 端点：

```bash
# 测试列出账号（应该返回空列表或已有账号）
curl -v http://localhost:8080/api/webdav/accounts

# 如果返回 404，说明路由未注册
# 如果返回 200 和 JSON，说明集成成功
```

### 2. 检查前端加载

打开浏览器开发者工具（F12），查看：
- **Network** 标签：查看 API 请求是否成功
- **Console** 标签：查看是否有 JavaScript 错误

### 3. 完整功能测试

```bash
# 创建测试账号
curl -X POST http://localhost:8080/api/webdav/accounts \
  -H "Content-Type: application/json" \
  -d '{
    "username": "test",
    "password": "test123",
    "allowed_dirs": ["/"]
  }'

# 测试 WebDAV 连接
curl -u test:test123 http://localhost:8080/dav/
```

## 权限控制（可选）

如果需要限制只有管理员才能管理 WebDAV 账号，在路由处理器中添加权限检查：

```go
mux.HandleFunc("/api/webdav/accounts", func(w http.ResponseWriter, r *http.Request) {
    // 检查是否为管理员
    if !isAdmin(r) {
        http.Error(w, "需要管理员权限", http.StatusForbidden)
        return
    }
    
    // 处理请求...
})
```

## 常见问题

**Q: 404 Not Found**
A: 路由未注册，按上述步骤添加路由注册代码

**Q: 403 Forbidden**
A: 需要管理员权限，检查权限控制逻辑

**Q: 文件保存失败**
A: 检查账号文件路径是否有写权限

**Q: WebDAV 连接失败**
A: 确认 `/dav/` 路由已注册，检查用户名密码是否正确

## 需要帮助？

如果需要具体的集成代码，请提供：
1. 主服务器文件的路径和内容片段
2. 当前的路由注册方式
3. 服务器初始化代码的位置

我可以提供针对性的集成代码。
