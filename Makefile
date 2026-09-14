BINARY  := taihu
BIN_DIR := bin
MAIN    := ./cmd/taihu

VERSION_PKG := github.com/liucxer/taihu/internal/version
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null)
BUILDTIME   := $(shell git log -1 --format=%cd --date=format:%Y%m%d%H%M 2>/dev/null)
LDFLAGS     := -X '$(VERSION_PKG).Commit=$(COMMIT)' -X '$(VERSION_PKG).BuildTime=$(BUILDTIME)'

.PHONY: all build clean check check-fmt check-layering check-sdk-only check-linux

all: build

build:
	mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(MAIN)

clean:
	rm -rf $(BIN_DIR)

check: check-fmt check-layering check-sdk-only
	go vet ./...

# third_party/ 是上游 fork，保持与上游一致的格式，不纳入本地 gofmt 校验。
check-fmt:
	@bad=$$(gofmt -l . | grep -v '^third_party/' || true); \
	if [ -n "$$bad" ]; then echo "以下文件未通过 gofmt："; echo "$$bad"; exit 1; fi; \
	echo "check-fmt: OK"

# ── 分层不变量 ────────────────────────────────────────────────────────────────
# pkg/ 只放**客户端 SDK**（外部调用方唯一入口），非客户端面一律下沉 internal/。
# 引擎（internal/storage）、传输层、元数据层都是服务端与客户端共用的下层实现。
#
# 两条方向都要守，少一条边界就会被绕回来：
#   check-layering  internal/ 不得反向依赖 pkg/ —— 违反即成环，也说明引擎又爬回了对外目录
#   check-sdk-only  pkg/ 不得直接依赖存储引擎 —— SDK 只应依赖 transport/cluster/metastore/ierr/version
#
# 两条都用 grep 源码，**不用 go list**：go list 在本机（darwin）看不见
# `//go:build linux` 的文件，而 internal/transport/server_shm.go 恰恰是最容易漏改的那个 ——
# 用 go list 会本机通过、Linux 上炸。grep 对 build tag 无感。代价是注释/字符串里的
# 路径字面量会误报，这里可接受（宁可误报）。
#
# check-sdk-only 排除 _test.go：同模块测试里 pkg/rpcclient、pkg/rpccluster 需要起一个
# 真实引擎+server 做端到端接线，那是合法的；这条只约束库代码。
check-layering:
	@viol=$$(grep -rn '"github.com/liucxer/taihu/pkg/' --include='*.go' internal/ || true); \
	if [ -n "$$viol" ]; then \
	  echo "违规：internal/ 不得依赖 pkg/（pkg/ 只放客户端 SDK）："; echo "$$viol"; exit 1; \
	fi; \
	echo "check-layering: OK"

check-sdk-only:
	@viol=$$(grep -rn '"github.com/liucxer/taihu/internal/\(storage\|device\|aio\|bufpool\|layout\)"' \
	  --include='*.go' pkg/ | grep -v '_test.go' || true); \
	if [ -n "$$viol" ]; then \
	  echo "违规：pkg/ 直接依赖存储引擎（SDK 不得依赖引擎内部，测试除外）："; echo "$$viol"; exit 1; \
	fi; \
	echo "check-sdk-only: OK"

# 跨平台编译核对：必须在 macOS/linux 上都能编过。
# 仓库有大量平台专有代码（unix.POLLIN 仅 linux 存在、Syscall6 参数个数、
# 结构体 size/offset 断言、_test 后缀必须排在 GOOS 之后），这些错误在 macOS
# 上编得过、只在 linux 上暴露，因此每次改动平台相关代码后都应跑一遍。
# test -c 只编译测试二进制不执行 —— 本机是 macOS，linux 测试跑不了，但能编过。
check-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /dev/null ./...
	@echo "check-linux: OK"
