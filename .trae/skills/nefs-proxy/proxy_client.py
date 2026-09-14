#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
proxy.py agent 客户端 — 封装节点上的 exec / 文件传输 / proxy 管理接口。

所有操作都通过 HTTP 调用节点上的 proxy.py agent（默认端口 9527，默认 token 95279527）。

用法:
  python3 proxy_client.py --node <ip|alias> exec   --cmd "ceph -s" [--timeout 60] [--cwd /tmp]
  python3 proxy_client.py --node <ip|alias> upload   --local <file> --remote <path>
  python3 proxy_client.py --node <ip|alias> download --remote <path> --local <file>
  python3 proxy_client.py --node <ip|alias> ls       [--path /tmp]
  python3 proxy_client.py --node <ip|alias> mkdir    --path /tmp/x
  python3 proxy_client.py --node <ip|alias> delete   --path /tmp/x
  python3 proxy_client.py --node <ip|alias> ping
  python3 proxy_client.py --node <ip|alias> proxy list
  python3 proxy_client.py --node <ip|alias> proxy add --listen-port 8080 --target-ip 10.0.0.5 --target-port 80 [--name x]
  python3 proxy_client.py --node <ip|alias> proxy delete --name x

节点别名: 194->100.71.7.194, 128->100.71.128.12, 146..152->10.151.26.x。
所有节点统一从本地 Mac 直接访问 http://<节点>:9527。
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_PORT = 9527
DEFAULT_TOKEN = "95279527"
ALIASES = {
    "194": "100.71.7.194",
    "128": "100.71.128.12",
    "jump": "100.71.128.12",
    "12": "100.71.128.12",
    "146": "10.151.26.146",
    "147": "10.151.26.147",
    "148": "10.151.26.148",
    "149": "10.151.26.149",
    "150": "10.151.26.150",
    "152": "10.151.26.152",
}


def request(base, path, method="GET", token=None, body=None, raw=None, timeout=3600):
    """发请求，返回 (status, json或bytes)。body 为 JSON dict，raw 为原始字节。"""
    url = base + path
    headers = {"X-Token": token or DEFAULT_TOKEN}
    data = None
    if raw is not None:
        data = raw
        headers["Content-Length"] = str(len(raw))
    elif body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = resp.read()
            try:
                return resp.status, json.loads(payload.decode("utf-8"))
            except ValueError:
                return resp.status, payload
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, json.loads(payload.decode("utf-8"))
        except ValueError:
            return e.code, payload
    except Exception as e:
        return None, {"error": str(e)}


def base_url(node):
    """所有节点统一从本地 Mac 直接访问 http://<节点>:9527。"""
    return "http://%s:%d" % (node, DEFAULT_PORT)


def jprint(obj):
    print(json.dumps(obj, ensure_ascii=False, indent=2))


def do_exec(base, token, args):
    body = {"cmd": args.cmd, "timeout": args.timeout}
    if args.cwd:
        body["cwd"] = args.cwd
    status, data = request(base, "/exec", "POST", token, body=body, timeout=args.timeout + 10)
    jprint(data)


def do_upload(base, token, args):
    with open(args.local, "rb") as f:
        content = f.read()
    path = "/upload?path=" + urllib.parse.quote(args.remote)
    status, data = request(base, path, "PUT", token, raw=content, timeout=3600)
    jprint(data)


def do_download(base, token, args):
    url = base + "/download?path=" + urllib.parse.quote(args.remote)
    req = urllib.request.Request(url, headers={"X-Token": token or DEFAULT_TOKEN})
    try:
        with urllib.request.urlopen(req, timeout=3600) as resp:
            payload = resp.read()
    except urllib.error.HTTPError as e:
        payload = e.read()
        if e.code != 200:
            jprint({"code": e.code, "error": payload.decode("utf-8", "replace")})
            sys.exit(1)
    except Exception as e:
        jprint({"code": 1, "error": str(e)})
        sys.exit(1)
    with open(args.local, "wb") as f:
        f.write(payload)
    jprint({"code": 0, "saved": args.local, "size": len(payload)})


def do_ls(base, token, args):
    status, data = request(base, "/ls?path=" + urllib.parse.quote(args.path), token=token)
    jprint(data)


def do_mkdir(base, token, args):
    status, data = request(base, "/mkdir?path=" + urllib.parse.quote(args.path), "POST", token)
    jprint(data)


def do_delete(base, token, args):
    status, data = request(base, "/delete?path=" + urllib.parse.quote(args.path), "POST", token)
    jprint(data)


def do_ping(base, token, args):
    t0 = time.time()
    status, data = request(base, "/ping", token=token, timeout=args.timeout)
    latency_ms = int((time.time() - t0) * 1000)
    online = status == 200 and isinstance(data, dict) and data.get("status") == "ok"
    if online:
        print(json.dumps({"online": True, "time": data.get("time"), "latency_ms": latency_ms},
                         ensure_ascii=False))
    else:
        err = data.get("error") if isinstance(data, dict) else str(data)
        print(json.dumps({"online": False, "error": err}, ensure_ascii=False))
        sys.exit(1)


def do_proxy(base, token, args):
    if args.proxy_action == "list":
        status, data = request(base, "/proxy", token=token)
        jprint(data)
    elif args.proxy_action == "add":
        body = {"name": args.name, "listen_ip": args.listen_ip,
                "listen_port": args.listen_port, "target_ip": args.target_ip,
                "target_port": args.target_port, "backlog": args.backlog}
        if not body["name"]:
            body["name"] = None  # 由 agent 自动生成
        status, data = request(base, "/proxy/add", "POST", token, body=body)
        jprint(data)
    elif args.proxy_action == "delete":
        status, data = request(base, "/proxy/delete", "POST", token,
                               body={"name": args.name})
        jprint(data)


def main():
    p = argparse.ArgumentParser(description="proxy.py agent 客户端")
    p.add_argument("--node", required=True, help="目标节点 IP 或别名（194/128/146..152）")
    p.add_argument("--token", default=DEFAULT_TOKEN,
                   help="agent token（默认 %s）" % DEFAULT_TOKEN)
    sub = p.add_subparsers(dest="action")

    e = sub.add_parser("exec", help="执行命令")
    e.add_argument("--cmd", required=True)
    e.add_argument("--timeout", type=int, default=3600)
    e.add_argument("--cwd", default=None)

    up = sub.add_parser("upload", help="上传文件")
    up.add_argument("--local", required=True)
    up.add_argument("--remote", required=True)

    dl = sub.add_parser("download", help="下载文件")
    dl.add_argument("--remote", required=True)
    dl.add_argument("--local", required=True)

    ls = sub.add_parser("ls", help="列目录")
    ls.add_argument("--path", default="/")

    mk = sub.add_parser("mkdir", help="建目录")
    mk.add_argument("--path", required=True)

    de = sub.add_parser("delete", help="删除文件/目录")
    de.add_argument("--path", required=True)

    pg = sub.add_parser("ping", help="健康检查（默认 3s 超时，超时视为不在线）")
    pg.add_argument("--timeout", type=int, default=3, help="ping 超时秒数（默认 3）")

    pr = sub.add_parser("proxy", help="proxy 规则管理")
    prs = pr.add_subparsers(dest="proxy_action")
    prs.add_parser("list", help="查看规则")
    p_add = prs.add_parser("add", help="新增规则")
    p_add.add_argument("--name", default=None)
    p_add.add_argument("--listen-ip", default="0.0.0.0")
    p_add.add_argument("--listen-port", type=int, required=True)
    p_add.add_argument("--target-ip", required=True)
    p_add.add_argument("--target-port", type=int, required=True)
    p_add.add_argument("--backlog", type=int, default=128)
    p_del = prs.add_parser("delete", help="删除规则")
    p_del.add_argument("--name", required=True)

    args = p.parse_args()
    if not args.action:
        p.print_help()
        sys.exit(1)

    node = ALIASES.get(args.node, args.node)
    base = base_url(node)

    if args.action == "ping":
        do_ping(base, args.token, args)
    elif args.action == "exec":
        do_exec(base, args.token, args)
    elif args.action == "upload":
        do_upload(base, args.token, args)
    elif args.action == "download":
        do_download(base, args.token, args)
    elif args.action == "ls":
        do_ls(base, args.token, args)
    elif args.action == "mkdir":
        do_mkdir(base, args.token, args)
    elif args.action == "delete":
        do_delete(base, args.token, args)
    elif args.action == "proxy":
        if not args.proxy_action:
            print("proxy 子命令: list / add / delete", file=sys.stderr)
            sys.exit(1)
        do_proxy(base, args.token, args)


if __name__ == "__main__":
    main()