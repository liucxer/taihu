---
name: "taihu-deploy"
description: "提交 taihu 代码并推到远端，打包为 taihu.<commit>.tar.gz 推送到 128.12(100.71.128.12) 解压编译。TRIGGER: 用户要求提交部署/打包推送/在 128.12 编译 taihu 时使用。"
---

# taihu-deploy

将当前 taihu 仓库按 3 步部署到 128.12（100.71.128.12）：提交推送 → 打包推包 → 远端解压编译。
依赖 nefs-proxy 的 `proxy_client.py`（端口 9527，默认 token `95279527`，128.12 直连可达）。

## 前置

- 工作目录：taihu 仓库根（Go module）。
- 远端 128.12 可直连；上传/执行复用 `~/.claude/skills/nefs-proxy/proxy_client.py`。
- 本 skill 不改动任何业务代码，只做构建部署动作。

```bash
PY=~/.claude/skills/nefs-proxy/proxy_client.py
NODE=100.71.128.12        # 或别名 128
DIST=/tmp                 # 128.12 上解压目录
```

## 步骤 1：提交代码并推送

确认改动，先看状态与待提交内容，避免误提交敏感/产物文件：

```bash
git status
git diff --stat
git log --oneline -5
```

按仓库现有 commit 风格提交，**仅 add 需要交付的源码**（勿 `git add -A` 兜底），再推送：

```bash
git add <相关文件路径>
git commit -m "<$msg>"          # msg 沿用仓库 commit 惯例（中文/英文随仓库）
git push
```

推送后用 `git rev-parse --short HEAD` 得到 `SHA`，作为后续包名与构建标识。

## 步骤 2：打包 taihu.<SHA>.tar.gz 并推送 128.12

仅打包仓库**受版本控制**的源码（含 `go.mod/go.sum`、`*.go`、`cmd/`、`third_party/`），排除 `.git` 与本地产物：

```bash
SHA=$(git rev-parse --short HEAD)
PKG="taihu.${SHA}.tar.gz"
git ls-files -z | tar --null -czf "$PKG" -T -
```

> `git ls-files` 打包跟踪文件即可（最干净）；若存在**未纳入 git 的新源码**（尚未 `git add`），如新文件，先 `git add -N <path>` 临时纳管再打包。确认无 `.git`/二进制后推送：

```bash
python3 $PY --node $NODE upload --local "$PKG" --remote "/tmp/$PKG"
```

## 步骤 3：128.12 解压并编译

用短 exec 到 128.12 后**目录去重解压**并编译。用 exec（默认 timeout 长，够 build 用）：

```bash
REMOTE_DIR="$DIST/taihu"
python3 $PY --node $NODE exec --cmd "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR && tar -xzf /tmp/taihu.$SHA.tar.gz -C $REMOTE_DIR && cd $REMOTE_DIR && go version && go build ./... && echo COMPILE_ALL_DONE" --timeout 600
```

- 若 128.12 无 Go / 版本不符：先安装匹配版本（`go version` 确认）。
- `go build ./...` 编译通过即成功（末尾打印 `COMPILE_ALL_DONE` 作为完成标记）。
- 命令较长但 exec 有 600s timeout，若超时/断连，改为 `setsid nohup` 后台编译 + 轮询日志（参考 nefs-proxy 长命令规范）。

## 典型调用（示例）

```bash
# step1 提交推送
git add storage.go device.go kv_pebble.go
git commit -m "storage: xx"
git push
# step2 打包推送
python3 ~/.claude/skills/nefs-proxy/proxy_client.py --node 128 upload --local taihu.abc123.tar.gz --remote /tmp/taihu.abc123.tar.gz
# step3 远端编译
python3 ~/.claude/skills/nefs-proxy/proxy_client.py --node 128 exec --cmd "rm -rf /tmp/taihu && mkdir -p /tmp/taihu && tar -xzf /tmp/taihu.abc123.tar.gz -C /tmp/taihu && cd /tmp/taihu && go build ./... && echo COMPILE_ALL_DONE" --timeout 600
```

## 注意事项

- 打包含 `.git`/密钥/`.img`/二进制时，先清理或指明清单，禁止推送到远端。
- `go.sum` 缺失会导致 `go build` 联网下载依赖失败；确认网络或提前 `go mod vendor` 一并打包。
- 仓库含本地 CGO/grocksdb 历史时，当前实现为纯 Go（pebble），无需 CGO；若后续引入 CGO 依赖，128.12 需对应编译工具链。
- 涉及 token/IP 属内网测试环境，勿外传。