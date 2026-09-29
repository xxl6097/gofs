// Command gofs 是一个参照 sigoden/dufs 设计的文件服务器，
// 额外提供压缩包内容预览与在线解压能力。
//
// 用法示例：
//
//	gofs                         以只读模式服务当前目录
//	gofs -A ./data               服务 ./data 并允许所有操作
//	gofs -A --allow-extract .    开放 zip / tar / tar.gz / gz 的在线解压
//	gofs -a admin:123@/:rw -A .  启用账号密码
//
// 想把文件服务嵌进自己的程序里，直接 import github.com/xxl6097/gofs。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/xxl6097/gofs"
)

func main() {
	// 信号处理留在命令行入口：库不该替调用方决定什么时候退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := gofs.RunCLI(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, gofs.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "gofs: %v\n", err)
		os.Exit(1)
	}
}
