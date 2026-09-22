#!/usr/bin/env python3
"""比 verify_spec_refs.py 更严的锚点校验：除存在性/越界外，还打印每条引用的**实际行内容**。

补掉了 verify_spec_refs.py 的三个盲区：
  1. 只认反引号内的 token —— 代码块里 `// path:line` 形式的锚点全被漏掉
  2. 只认含 '/' 的 token —— 裸文件名引用被**静默跳过**（C-09 就是这类，62 处）
  3. 只查行号是否越界，不查那一行**是不是真的说了那件事**

用法:
  python3 verify_anchor_content.py <spec 文件>            # 默认只报告问题
  python3 verify_anchor_content.py <spec 文件> --dump     # 额外打印每条引用的行内容
"""
import os
import re
import sys
from typing import Optional

REPO = os.environ.get("TAIHU_REPO", ".")

# 反引号内的引用，或代码块里 `// path:line` 形式的注释
BACKTICK = re.compile(r"`([A-Za-z0-9_./\-]+\.(?:go|md|yaml|yml|sh|json|mod))(?::(\d+)(?:-(\d+))?)?`")
INLINE = re.compile(r"//\s*([A-Za-z0-9_./\-]+\.(?:go|md|yaml|yml|sh|json|mod))(?::(\d+)(?:-(\d+))?)")
ROOT_FILES = {"Makefile", "README.md", "go.mod", ".gitignore"}


def resolve(path: str, spec_file: str) -> Optional[str]:
    """把引用解析成仓库内路径。裸文件名返回 None（判为歧义）。"""
    if path in ROOT_FILES:
        return path
    if "/" not in path:
        return None
    # 只有显式 `./x.md` 才是「同目录兄弟文件」；`.trellis/spec/...` 是仓库根相对
    if path.startswith("./"):
        return os.path.normpath(os.path.join(REPO, os.path.dirname(spec_file), path))
    return path


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    spec_file = sys.argv[1]
    dump = "--dump" in sys.argv

    text = open(spec_file, encoding="utf-8").read()
    # 去掉反引号形式已经覆盖的区域，避免同一锚点报两次
    found, seen = [], set()
    for rx in (BACKTICK, INLINE):
        for m in rx.finditer(text):
            raw, start, end = m.group(1), m.group(2), m.group(3)
            key = (raw, start, end)
            if key in seen:
                continue
            seen.add(key)
            found.append((raw, start, end))

    missing, oob, bare, ok = [], [], [], 0
    for raw, start, end in found:
        target = resolve(raw, spec_file)
        if target is None:
            bare.append(raw)
            continue
        if not os.path.exists(target):
            missing.append(raw)
            continue
        if start:
            lines = open(target, encoding="utf-8", errors="replace").read().splitlines()
            rng = range(int(start), int(end or start) + 1)
            if not all(1 <= i <= len(lines) for i in rng):
                oob.append(f"{raw}:{start}{'-' + end if end else ''}（共 {len(lines)} 行）")
                continue
            ok += 1
            if dump:
                body = " ⏎ ".join(lines[i - 1].strip()[:78] for i in rng)
                print(f"  [OK] {raw}:{start}{'-' + end if end else ''}\n        {body or '<空行>'}")
        else:
            ok += 1
            if dump:
                print(f"  [OK] {raw}（无行号）")

    print(f"\n=== {spec_file} ===")
    print(f"有效引用 {ok} 条；文件不存在 {len(missing)}；行号越界 {len(oob)}；裸文件名 {len(set(bare))}")
    for x in dict.fromkeys(missing):
        print(f"  [缺失] {x}")
    for x in oob:
        print(f"  [越界] {x}")
    for x in dict.fromkeys(bare):
        print(f"  [裸名] {x}   ← 补全为仓库相对路径")
    return 1 if (missing or oob or bare) else 0


if __name__ == "__main__":
    sys.exit(main())
