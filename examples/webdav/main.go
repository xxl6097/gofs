package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/xxl6097/gofs/internal/webdav"
)

func main() {
	// 命令行参数
	var (
		rootDir     = flag.String("root", ".", "WebDAV 服务根目录")
		port        = flag.Int("port", 8080, "监听端口")
		accountFile = flag.String("accounts", "", "账号数据文件路径（默认: ~/.config/gofs/webdav_accounts.json）")
	)
	flag.Parse()

	// 解析根目录
	absRoot, err := filepath.Abs(*rootDir)
	if err != nil {
		log.Fatalf("解析根目录失败: %v", err)
	}

	// 确定账号文件路径
	accountFilePath := *accountFile
	if accountFilePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("获取用户主目录失败: %v", err)
		}
		accountFilePath = filepath.Join(home, ".config", "gofs", "webdav_accounts.json")
	}

	// 创建 WebDAV 服务器
	server, err := webdav.New(absRoot)
	if err != nil {
		log.Fatalf("创建 WebDAV 服务器失败: %v", err)
	}

	// 创建存储
	storage := webdav.NewStorage(accountFilePath)

	// 加载已有账号
	accounts, err := storage.Load()
	if err != nil {
		log.Printf("警告: 加载账号失败: %v", err)
	} else {
		server.LoadAccounts(accounts)
		log.Printf("已加载 %d 个账号", len(accounts))
	}

	// 创建 API 处理器
	api := webdav.NewAPI(server, storage)

	// 设置路由
	mux := http.NewServeMux()

	// WebDAV 服务（挂载在 /dav）
	mux.Handle("/dav/", http.StripPrefix("/dav", server.Handler()))

	// 管理 API
	mux.HandleFunc("/api/webdav/accounts", func(w http.ResponseWriter, r *http.Request) {
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
		if r.URL.Path == "/api/webdav/accounts/" {
			http.Error(w, "用户名不能为空", http.StatusBadRequest)
			return
		}

		// 检查是否是修改密码请求
		if len(r.URL.Path) > len("/api/webdav/accounts/") {
			path := r.URL.Path[len("/api/webdav/accounts/"):]
			if len(path) > 0 && r.Method == http.MethodPost {
				// 检查是否以 /password 结尾
				if len(path) > 9 && path[len(path)-9:] == "/password" {
					api.HandleChangePassword(w, r)
					return
				}
			}
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

	// 欢迎页面
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>WebDAV 服务器</title>
    <style>
        body { font-family: Arial, sans-serif; max-width: 800px; margin: 50px auto; padding: 20px; }
        h1 { color: #333; }
        .section { margin: 30px 0; padding: 20px; background: #f5f5f5; border-radius: 5px; }
        code { background: #e0e0e0; padding: 2px 5px; border-radius: 3px; }
        pre { background: #e0e0e0; padding: 10px; border-radius: 5px; overflow-x: auto; }
    </style>
</head>
<body>
    <h1>🚀 WebDAV 服务器运行中</h1>

    <div class="section">
        <h2>服务信息</h2>
        <p><strong>根目录:</strong> %s</p>
        <p><strong>WebDAV 地址:</strong> <code>http://localhost:%d/dav/</code></p>
        <p><strong>账号文件:</strong> %s</p>
    </div>

    <div class="section">
        <h2>管理 API</h2>
        <ul>
            <li><code>GET /api/webdav/accounts</code> - 列出所有账号</li>
            <li><code>POST /api/webdav/accounts</code> - 创建账号</li>
            <li><code>PUT /api/webdav/accounts/{username}</code> - 更新账号权限</li>
            <li><code>DELETE /api/webdav/accounts/{username}</code> - 删除账号</li>
            <li><code>POST /api/webdav/accounts/{username}/password</code> - 修改密码</li>
        </ul>
    </div>

    <div class="section">
        <h2>使用示例</h2>
        <h3>1. 创建账号</h3>
        <pre>curl -X POST http://localhost:%d/api/webdav/accounts \
  -H "Content-Type: application/json" \
  -d '{
    "username": "user1",
    "password": "pass123",
    "allowed_dirs": ["/docs", "/shared"]
  }'</pre>

        <h3>2. 列出账号</h3>
        <pre>curl http://localhost:%d/api/webdav/accounts</pre>

        <h3>3. 使用 WebDAV 客户端连接</h3>
        <p><strong>Windows:</strong> 在文件资源管理器中，右键"此电脑" → "映射网络驱动器" → 输入 <code>http://localhost:%d/dav/</code></p>
        <p><strong>macOS:</strong> Finder → "前往" → "连接服务器" → 输入 <code>http://localhost:%d/dav/</code></p>
        <p><strong>Linux:</strong> 使用 davfs2 或文件管理器的"连接到服务器"功能</p>
    </div>
</body>
</html>
`, absRoot, *port, accountFilePath, *port, *port, *port, *port)
	})

	// 创建 HTTP 服务器
	addr := fmt.Sprintf(":%d", *port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 启动服务器
	go func() {
		log.Printf("✅ WebDAV 服务器启动成功")
		log.Printf("   根目录: %s", absRoot)
		log.Printf("   监听地址: http://localhost:%d", *port)
		log.Printf("   WebDAV 地址: http://localhost:%d/dav/", *port)
		log.Printf("   账号文件: %s", accountFilePath)
		log.Printf("   管理面板: http://localhost:%d/", *port)
		log.Println()
		log.Println("按 Ctrl+C 停止服务")

		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务器错误: %v", err)
		}
	}()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("\n正在关闭服务器...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("服务器关闭错误: %v", err)
	}

	log.Println("服务器已关闭")
}
