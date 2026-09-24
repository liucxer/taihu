# 重建 spec：只用 golang-base-practices-skills 与 project-layout

## Goal

删除全部现有 spec（已完成，27 文件 5395 行，未提交），改为只由两个来源重建：cexll/golang-base-practices-skills 的 53 条规则（commit 26426d2，本地已克隆到 /tmp/gbp）与 golang-standards/project-layout 的 20 个顶层目录（本地已克隆到 /tmp/pl）。不再使用 Uber Go Style Guide 与 Everything Claude Code 的任何规则。

## Requirements

- TBD

## Acceptance Criteria

- [ ] TBD

## Notes

- Keep `prd.md` focused on requirements, constraints, and acceptance criteria.
- Lightweight tasks can remain PRD-only.
- For complex tasks, add `design.md` for technical design and `implement.md` for execution planning before `task.py start`.
