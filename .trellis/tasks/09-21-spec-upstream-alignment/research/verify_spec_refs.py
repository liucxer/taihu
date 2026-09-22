#!/usr/bin/env python3
"""校验 .trellis/spec/ 里所有 `path:line` 引用是否真实存在。

规则：
  - 只认反引号里的 token，形如 `internal/aio/aio.go:70-71`、`Makefile:45`
  - token 必须含 '/' （或位于仓库根的已知文件名白名单），避免把 `foo.go` 这种泛指当引用
  - 带行号的，还要检查行的确存在（不超过文件总行数）

退出码非 0 表示有失效引用。
"""
import os
import re
import sys

REPO = sys.argv[1] if len(sys.argv) > 1 else "."
SPEC = os.path.join(REPO, ".trellis", "spec")

# 反引号内的 token；只要含 / 或者属于根文件白名单
TOKEN = re.compile(r"`([A-Za-z0-9_./\-]+?)(?::(\d+)(?:-(\d+))?)?`")
ROOT_FILES = {
    "Makefile", "README.md", "AGENTS.md", ".gitattributes", "go.mod",
    ".gitignore", "configs/taihu-server.example.sh",
}

# 已知失效、但在 spec 里被**显式标注为历史路径**的引用。
# 它们出现在原文引述里（Makefile:39 的注释），spec 已在紧邻处写明该路径已不存在。
# 加进这里是为了让"剩余告警"全都是真正待修的东西。
KNOWN_DEAD = {
    "internal/transport/server_shm.go",   # 已拆为 server_shm_linux.go / server_shm_other.go
    "pkg/rpcclient/pool_test.go",         # 评审文档当时的路径，pkg/rpcclient 已迁到 internal/rpcclient
}


def is_ref(path: str) -> bool:
    if path in ROOT_FILES:
        return True
    if "/" not in path:
        return False
    if path.startswith(("http", "github.com")):
        return False
    return bool(re.search(r"\.(go|md|yaml|yml|sh|json|mod|txt)$", path))


def main() -> int:
    missing, badline, ok = [], [], 0
    for dirpath, _, filenames in os.walk(SPEC):
        for fn in sorted(filenames):
            if not fn.endswith(".md"):
                continue
            full = os.path.join(dirpath, fn)
            rel_self = os.path.relpath(full, REPO)
            with open(full, encoding="utf-8") as fh:
                text = fh.read()
            for m in TOKEN.finditer(text):
                path, start, end = m.group(1), m.group(2), m.group(3)
                if not is_ref(path):
                    continue
                target = os.path.join(REPO, path)
                if not os.path.exists(target):
                    if path not in KNOWN_DEAD:
                        missing.append(f"{rel_self}: 文件不存在 -> {path}")
                    continue
                if start:
                    try:
                        with open(target, encoding="utf-8", errors="replace") as tf:
                            total = sum(1 for _ in tf)
                    except OSError:
                        total = 0
                    hi = int(end or start)
                    if total and (hi > total or int(start) < 1):
                        badline.append(
                            f"{rel_self}: 行号越界 -> {path}:{start}"
                            f"{'-' + end if end else ''}（该文件共 {total} 行）"
                        )
                        continue
                    ok += 1
                else:
                    ok += 1

    print(f"引用统计：有效 {ok} 条；文件不存在 {len(missing)} 条；行号越界 {len(badline)} 条")
    for item in missing:
        print("  [缺失] " + item)
    for item in badline:
        print("  [越界] " + item)
    return 1 if (missing or badline) else 0


if __name__ == "__main__":
    sys.exit(main())
