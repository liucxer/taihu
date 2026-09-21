# 架构层约定

> 本层管 taihu 仓库的**结构性不变量**：目录分层与依赖方向、全仓库代码风格、错误体系、提交信息格式 —— 这些规则跨包生效，任何一次改动都可能踩到。

---

## 本层文件

| 文件 | 用途 |
|------|------|
| [layering.md](./layering.md) | 依赖方向不变量：`internal/` 不得 import `pkg/`、SDK 不得直接依赖存储引擎；两条 `make` 门禁的实现与理由；包依赖全貌 |
| [code-style.md](./code-style.md) | 全仓库代码约定：中文注释与「为什么」取向、命名（receiver 单字母、首字母缩写全大写）、错误包装措辞、三条日志通道、import 分组 |
| [error-model.md](./error-model.md) | 错误体系：`internal/ierr` 唯一事实源、包内未导出 sentinel、跨进程传 code 不传字符串、`internal/rpcclient` → `pkg/taihu-client` 的 re-export 链 |
| [commits.md](./commits.md) | 提交信息：`type(scope): 中文标题` + 中文 bullet 正文 + 纯重构的「行为不变：」核对段 |

---

## Pre-Development Checklist

动手前逐条自检：

- [ ] 我知道这次改动落在哪一层吗？先读 [layering.md](./layering.md) 确认新代码该进 `internal/` 还是 `pkg/`。
- [ ] 我要新增的 import 会不会打破方向不变量？改完必须 `make check-layering` 与 `make check-sdk-only` 都能过。
- [ ] 我要加的日志走哪条通道？先读 [code-style.md](./code-style.md) 的「日志三通道」，不要新开第四种。
- [ ] 我要返回的错误是库错误（`internal/ierr` 的 sentinel）还是包内控制信号（未导出 sentinel）？见 [error-model.md](./error-model.md)。
- [ ] 我要暴露给外部调用方的类型出现在导出签名里吗？若是，去 `internal/rpcclient/reexport.go` / `pkg/taihu-client/reexport.go` 补 alias。
- [ ] 改动了平台相关代码（`_linux` / `_other` 后缀文件）吗？本机是 darwin，务必跑 `make check-linux`。
- [ ] 提交信息准备好了吗？格式见 [commits.md](./commits.md)；纯重构请预留「行为不变：」核对段。

---

## Quality Check

改完在仓库根目录执行：

```bash
make check        # check-fmt + check-layering + check-sdk-only + go vet ./...
make check-linux  # 平台专有代码改动后必跑（darwin 编得过不代表 linux 编得过）
```

`make check` 的四个子目标全部以 `OK` 结尾才算过：

```
check-fmt: OK
check-layering: OK
check-sdk-only: OK
go vet ./...
```

分层相关的单点复核（门禁之外，用来确认自己没绕过）：

```bash
# internal/ 不得 import pkg/ —— 应无输出
grep -rn '"github.com/liucxer/taihu/pkg/' --include='*.go' internal/

# pkg/ 非测试代码的 taihu 内部依赖 —— 应只有 internal/cluster、internal/rpcclient、internal/version
grep -rn '"github.com/liucxer/taihu/' --include='*.go' pkg/ | grep -v '_test.go'
```

`make check-linux` 覆盖 darwin 看不见的部分（`//go:build linux` 文件、`unix.POLLIN`、`Syscall6` 参数个数、结构体 size/offset 断言），定义见 `Makefile:60-70`。
