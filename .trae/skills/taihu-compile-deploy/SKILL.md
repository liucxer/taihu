---
name: "编译部署skill"
description: "编译部署 taihu 到 128.12：本地 commit（不 push）取 commitid → 打包 taihu.<commit>.tar.gz → 同步到 128.12 解压编译部署。TRIGGER: 用户要求编译部署/打包同步到 128.12 编译/部署 taihu 时使用。"
---

# 编译部署 skill

将当前 taihu 仓库按 3 步编译部署到 128.12（100.71.128.12）。与 `taihu-deploy` 的区别：**本 skill 只 commit、不 push**。

依赖 nefs-proxy（端口 9527，token `95279527`，128.12 直连）。实测 `proxy_client.py` 与 128.12 存在 HTTP 兼容问题，改用 **curl + X-Token** 亦可；两法皆可，二者择一。

## 前置

- 工作目录：taihu 仓库根（Go module）。
- 远端 128.12 可直连，已装 Go（`/usr/local/go`，可选 `export PATH=/usr/local/go/bin:$PATH`）。
- 本 skill 只做提交/打包/远端编译，不改业务代码。

## 步骤 1：commit（不 push），取 commitid

```bash
git status
git diff --stat
git log --oneline -5
```

仅 add 要交付的源码（勿 `git add -A` 兜底），提交（沿用仓库 commit 惯例），**不 push**：

```bash
git add <相关文件路径>
git commit -m "<$msg>"
SHA=$(git rev-parse --short HEAD)   # 作为后续包名与构建标识
```

## 步骤 2：打包 taihu.<SHA>.tar.gz

仅打包**受版本控制**的源码（含 go.mod/go.sum、*.go、cmd/），排除 `.git` 与本地产物：

```bash
SHA=$(git rev-parse --short HEAD)
PKG="dist/taihu.${SHA}.tar.gz"   # 打包产物统一收敛到 dist/，避免污染仓库一级目录
git ls-files -z | tar --null -czf "$PKG" -T -
```

> 若存在刷新增的源码（未 `git add`），先 `git add -N <path>` 临时纳管再打包。确认无 `.git`/密钥/二进制。

## 步骤 3：同步到 128.12，解压编译部署

用 curl 上传，再 exec 解压编译，产出 bench 二进制：

```bash
NODE=100.71.128.12
TOKEN=95279527
DIST=/tmp/taihu-src
BIN=/tmp/taihu-bench.new

curl -s --max-time 120 -X PUT -T "$PKG" -H "X-Token: $TOKEN" \
  "http://$NODE:9527/upload?path=/tmp/$(basename "$PKG")"

curl -s -G -H "X-Token: $TOKEN" "http://$NODE:9527/exec" \
  --data-urlencode "cmd=rm -rf $DIST && mkdir -p $DIST && tar -xzf /tmp/$(basename "$PKG") -C $DIST && cd $DIST && export PATH=/usr/local/go/bin:\$PATH && SHA=\$(git rev-parse --short HEAD) && TS=\$(git log -1 --format=%cd --date=format:%Y%m%d%H%M) && go build -ldflags \"-X github.com/liucxer/taihu/internal/version.Commit=\$SHA -X github.com/liucxer/taihu/internal/version.BuildTime=\$TS\" -o $BIN ./cmd/taihu-bench && echo BUILD_ALL_DONE" \
  --data-urlencode "timeout=280"
```

- `SHA`/`TS` 取自远端最后一次 commit（短哈希 + `YYYYMMDDHHMM`），经 `-ldflags -X` 注入 `internal/version`，二进制 `-version` 输出形如 `8abdf4d_202609111002`。

- `BUILD_ALL_DONE` 为完成标记；编译测试可用 `go test ./...` 替代/追加。
- 若需编译整个 module（非仅 bench）：`go build ./...`。
- 长命令若超时/断连：`setsid nohup` 后台编译 + 轮询日志（参考 nefs-proxy 长命令规范）。

## 注意事项

- **只 commit 不 push**：如需推送另走 `taihu-deploy`。
- 打包含 `.git`/密钥/`.img`/二进制时清理，禁止推远端。
- tar 可能出现 "time stamp in the future" 告警，属无害。
- token/IP 属内网测试环境，勿外传。