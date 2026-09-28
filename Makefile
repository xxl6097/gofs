.PHONY: all build test vet fmt run clean cross testdata

BINARY := gofs
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

# 前端 UI 测试（jsdom）用的路径，可用环境变量覆盖
NODE_MODULES ?= $(HOME)/.workbuddy/binaries/node/workspace/node_modules
NODE_BIN     ?= $(shell ls $(HOME)/.workbuddy/binaries/node/versions/*/bin/node 2>/dev/null | tail -1 || echo node)
GOFS_URL     ?= http://127.0.0.1:5000
export NODE_MODULES NODE_BIN GOFS_URL

all: fmt vet test build

## build: 构建当前平台的二进制
build:
	go build -trimpath -ldflags "-s -w" -o $(BINARY) .
	@ls -lh $(BINARY)

## test: 运行全部测试
test:
	go test ./...

## testv: 运行全部测试并查看每个用例
testv:
	go test ./internal/server/ -v

## uitest: 用 jsdom 跑真实页面交互（需先启动服务，且开启鉴权）
##   用法：另开一个终端跑 `make run-auth`，然后 `make uitest`
uitest:
	NODE_PATH=$(NODE_MODULES) $(NODE_BIN) scripts/test-ui.mjs $(GOFS_URL)

## run-auth: 以鉴权模式启动，供 uitest 使用
run-auth: build
	./$(BINARY) -A -b 127.0.0.1 -p 5000 -a 'admin:secret@/:rw' ./data

## vet: 静态检查
vet:
	go vet ./...

## fmt: 格式化
fmt:
	gofmt -w .
	gofmt -l .

## run: 以全权限模式服务 ./data 目录
run: build
	./$(BINARY) -A -b 127.0.0.1 -p 5000 ./data

## testdata: 生成端到端手工测试用的目录与压缩包（/tmp/gofs-test）
testdata:
	python3 scripts/gen_testdata.py

## cross: 交叉编译常用平台
cross:
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/gofs-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/gofs-linux-arm64 .
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/gofs-darwin-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/gofs-darwin-amd64 .
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/gofs-windows-amd64.exe .
	@ls -lh dist/

## clean: 清理构建产物
clean:
	rm -rf $(BINARY) dist
