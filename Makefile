BINARY  := taihu
BIN_DIR := bin
MAIN    := ./cmd/taihu

VERSION_PKG := github.com/liucxer/taihu/internal/version
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null)
BUILDTIME   := $(shell git log -1 --format=%cd --date=format:%Y%m%d%H%M 2>/dev/null)
LDFLAGS     := -X '$(VERSION_PKG).Commit=$(COMMIT)' -X '$(VERSION_PKG).BuildTime=$(BUILDTIME)'

.PHONY: all build clean check check-fmt check-linux

all: build

build:
	mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(MAIN)

clean:
	rm -rf $(BIN_DIR)

check: check-fmt
	go vet ./...

# third_party/ 是上游 fork，保持与上游一致的格式，不纳入本地 gofmt 校验。
check-fmt:
	@bad=$$(gofmt -l . | grep -v '^third_party/' || true); \
	if [ -n "$$bad" ]; then echo "以下文件未通过 gofmt："; echo "$$bad"; exit 1; fi; \
	echo "check-fmt: OK"

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
