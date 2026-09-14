#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
NEFS tools 统一脚本 — 单文件单命令

    python3 proxy.py server --root /tmp        # 默认端口 9527，默认 token 95279527

一个 HTTP server、一个命令，同时提供四类服务：

    - 命令执行:   POST/GET /exec   (body {"cmd": "...", "timeout": 3600, "cwd": "/tmp"})
    - 文件传输:   PUT/POST /upload?path=...、GET /download?path=...、GET /ls?path=...、
                  POST /mkdir?path=...、POST /delete?path=...
    - proxy 管理（动态端口转发规则）:
                  GET  /proxy             查看规则列表
                  POST /proxy/add         body {"name","listen_ip","listen_port","target_ip","target_port","backlog"}
                  POST /proxy/delete      body {"name": ...} 或 ?name=...   删除规则
                  DELETE /proxy?name=...  删除规则
    - 健康检查:   GET /ping (无需 token)

proxy 规则也可启动时预加载：--config proxy.json（JSON 格式与旧 proxy 一致），
或用 --listen-port/--target-ip/--target-port 预加载单条规则。

curl 示例:
    # 查看 / 新增 / 删除 proxy 规则
    curl -s -H "X-Token: 95279527" http://<node>:9527/proxy
    curl -s -X POST http://<node>:9527/proxy/add -H "X-Token: 95279527" \
         -H 'Content-Type: application/json' \
         -d '{"name":"web-80","listen_port":8080,"target_ip":"10.0.0.5","target_port":80}'
    curl -s -X POST http://<node>:9527/proxy/delete -H "X-Token: 95279527" \
         -H 'Content-Type: application/json' -d '{"name":"web-80"}'
"""

import argparse
import hmac
import json
import logging
import os
import select
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.parse
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, HTTPServer
from socketserver import ThreadingMixIn

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
log = logging.getLogger("nefs_tools")

DEFAULT_BACKLOG = 128
DEFAULT_PORT = 9527
DEFAULT_TOKEN = "95279527"
DEFAULT_TIMEOUT = 3600  # 接口超时统一 3600s
CHUNK = 65536


# python3.7+ 才有 http.server.ThreadingHTTPServer，这里手动拼一个兼容 3.6
class ThreadingHTTPServer(ThreadingMixIn, HTTPServer):
    daemon_threads = True


def fail(msg):
    """打印错误并退出。"""
    print("错误: %s" % msg, file=sys.stderr)
    sys.exit(1)


# ============================== proxy: 动态端口转发 ==============================

@dataclass
class ProxyRule:
    """一条转发规则。"""
    listen_ip: str = "0.0.0.0"
    listen_port: int = 0
    target_ip: str = ""
    target_port: int = 0
    name: str = ""
    backlog: int = DEFAULT_BACKLOG
    _lock: threading.Lock = field(default_factory=threading.Lock, repr=False)

    def validate(self):
        if not self.listen_port or not (1 <= self.listen_port <= 65535):
            raise ValueError("规则 [%s] 监听端口非法: %s" % (self.name, self.listen_port))
        if not self.target_ip or not self.target_port or not (1 <= self.target_port <= 65535):
            raise ValueError("规则 [%s] 目标地址不完整: %s:%s" % (self.name, self.target_ip, self.target_port))

    def to_dict(self):
        return {
            "name": self.name,
            "listen_ip": self.listen_ip,
            "listen_port": self.listen_port,
            "target_ip": self.target_ip,
            "target_port": self.target_port,
            "backlog": self.backlog,
        }


def load_config(path):
    """加载配置文件，返回 dict。JSON 原生支持；YAML 需要 pyyaml。"""
    with open(path, "r", encoding="utf-8") as f:
        text = f.read().strip()
    if not text:
        raise ValueError("配置文件为空: %s" % path)
    if path.endswith((".yaml", ".yml")):
        try:
            import yaml
        except ImportError:
            raise ValueError("解析 YAML 需要安装 pyyaml: pip install pyyaml") from None
        cfg = yaml.safe_load(text)
    else:
        cfg = json.loads(text)
    if not isinstance(cfg, dict) or "rules" not in cfg:
        raise ValueError("配置文件缺少 rules 列表")
    if not isinstance(cfg["rules"], list) or not cfg["rules"]:
        raise ValueError("rules 不能为空")
    return cfg


def parse_rules(cfg):
    """把配置 dict 解析为 ProxyRule 列表。"""
    rules = []
    for i, item in enumerate(cfg["rules"]):
        if not isinstance(item, dict):
            raise ValueError("rules[%d] 必须是对象" % i)
        rule = ProxyRule(
            listen_ip=str(item.get("listen_ip", "0.0.0.0")),
            listen_port=int(item["listen_port"]),
            target_ip=str(item["target_ip"]),
            target_port=int(item["target_port"]),
            name=str(item.get("name", "rule-%d" % (i + 1))),
            backlog=int(item.get("backlog", DEFAULT_BACKLOG)),
        )
        rule.validate()
        rules.append(rule)
    return rules


def pipe(src, dst, tag):
    """双向搬运数据：从 src 读到字节写到 dst。"""
    try:
        while True:
            data = src.recv(65536)
            if not data:
                break
            dst.sendall(data)
    except OSError as e:
        log.debug("[%s] 连接读写异常: %s", tag, e)
    finally:
        try:
            src.shutdown(socket.SHUT_RD)
        except OSError:
            pass
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass


def set_keepalive(sock, idle=20, interval=10, count=3):
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPIDLE, idle)
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPINTVL, interval)
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPCNT, count)
    log.debug("[keepalive] fd=%d idle=%ds interval=%ds count=%d", sock.fileno(), idle, interval, count)


def proxy_handle(client, addr, rule):
    """处理单个客户端连接：连到目标，双向转发。"""
    tag = "%s:%s" % (addr[0], addr[1])
    log.info("[%s] 收到连接 %s，转发到 %s:%d",
             rule.name, tag, rule.target_ip, rule.target_port)
    try:
        upstream = socket.create_connection((rule.target_ip, rule.target_port), timeout=30)
    except OSError as e:
        log.error("[%s] 无法连接到目标 %s:%d: %s",
                  rule.name, rule.target_ip, rule.target_port, e)
        client.close()
        return
    upstream.settimeout(None)
    client.settimeout(None)
    set_keepalive(client)
    set_keepalive(upstream)
    t1 = threading.Thread(target=pipe, args=(client, upstream, tag), daemon=True)
    t2 = threading.Thread(target=pipe, args=(upstream, client, tag), daemon=True)
    t1.start()
    t2.start()
    t1.join()
    t2.join()
    client.close()
    upstream.close()
    log.info("[%s] 连接 %s 已关闭", rule.name, tag)


class ProxyManager:
    """动态管理的转发规则表：支持 add / remove / list。"""

    def __init__(self):
        self._rules = {}
        self._lock = threading.Lock()

    def _serve(self, rule, sock, stop_event):
        # 用非阻塞 accept + select 轮询 + stop_event，删除规则时能确定性退出
        # （Linux 上 close() 无法唤醒阻塞在 accept() 的线程，会残留短暂窗口）
        sock.listen(rule.backlog)
        sock.setblocking(False)
        log.info("规则 [%s] 已启动: %s:%d -> %s:%d",
                 rule.name, rule.listen_ip, rule.listen_port, rule.target_ip, rule.target_port)
        try:
            while not stop_event.is_set():
                r, _, _ = select.select([sock], [], [], 0.5)
                if not r:
                    continue
                try:
                    client, addr = sock.accept()
                except OSError:
                    break  # socket 被关闭（删除规则/退出）
                client.setblocking(True)
                threading.Thread(target=proxy_handle, args=(client, addr, rule), daemon=True).start()
        except OSError:
            pass
        finally:
            try:
                sock.close()
            except OSError:
                pass
        log.info("规则 [%s] 已停止", rule.name)

    def add(self, rule):
        """新增规则并启动监听；端口占用/名字重复抛 ValueError。"""
        with self._lock:
            if rule.name in self._rules:
                raise ValueError("规则名已存在: %s" % rule.name)
            rule.validate()
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                sock.bind((rule.listen_ip, rule.listen_port))
            except OSError as e:
                sock.close()
                raise ValueError("监听 %s:%d 失败: %s" % (rule.listen_ip, rule.listen_port, e))
            stop_event = threading.Event()
            t = threading.Thread(target=self._serve, args=(rule, sock, stop_event), daemon=True)
            t.start()
            self._rules[rule.name] = {"rule": rule, "sock": sock, "stop": stop_event, "thread": t}
        return rule

    def remove(self, name):
        """删除规则并停止监听；不存在抛 KeyError。"""
        with self._lock:
            item = self._rules.pop(name, None)
        if item is None:
            raise KeyError("规则不存在: %s" % name)
        item["stop"].set()  # select 循环在 ≤0.5s 内退出
        try:
            item["sock"].close()
        except OSError:
            pass
        return item["rule"]

    def list(self):
        """返回规则列表（含监听线程存活状态）。"""
        with self._lock:
            out = []
            for item in self._rules.values():
                d = item["rule"].to_dict()
                d["alive"] = item["thread"].is_alive()
                out.append(d)
            return out


# ============================== server: 命令执行 + 文件传输 + proxy 管理 ==============================

def run_command(cmd, timeout, cwd=None):
    """执行 shell 命令，返回 (exit_code, stdout, stderr, timed_out)。"""
    start = time.time()
    try:
        proc = subprocess.Popen(
            cmd,
            shell=True,
            cwd=cwd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,  # 独立进程组，超时可整组 kill
        )
    except OSError as e:
        return -1, b"", str(e).encode("utf-8"), False
    try:
        stdout, stderr = proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
        stdout, stderr = proc.communicate()
        log.warning("命令超时被 kill (timeout=%ss): %s", timeout, cmd)
        return proc.returncode, stdout, stderr, True
    log.info("[exec] done in %dms, exit=%s", (time.time() - start) * 1000, proc.returncode)
    return proc.returncode, stdout, stderr, False


class AgentHandler(BaseHTTPRequestHandler):
    """同一端口提供命令执行（/exec）+ 文件传输（/upload 等）+ proxy 管理（/proxy*）。"""
    server_version = "nefs_agent_server"
    timeout = DEFAULT_TIMEOUT  # HTTP socket 超时统一 3600s

    def log_message(self, fmt, *args):
        log.info("[%s] %s", self.client_address[0], fmt % args)

    # ---- 公共 ----
    def _authed(self):
        token = self.server.token
        if token is None:
            return True
        return hmac.compare_digest(self.headers.get("X-Token", ""), token)

    def send_json(self, obj, status=200):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _require_auth(self):
        if not self._authed():
            self.send_json({"code": 401, "error": "unauthorized"}, status=401)
            return False
        return True

    def _read_body(self):
        length = int(self.headers.get("Content-Length", 0))
        return self.rfile.read(length) if length else b""

    def _param(self, params, key, default=""):
        """从 JSON body 或 query 字典取参数（兼容 list 值）。"""
        v = params.get(key, default)
        if isinstance(v, (list, tuple)):
            v = v[0] if v else default
        if v is None:
            v = default
        return str(v).strip()

    # ---- 命令执行 ----
    def _handle_exec(self, params):
        if not isinstance(params, dict):
            return self.send_json({"code": 400, "error": "请求体必须是对象"}, status=400)
        cmd = self._param(params, "cmd")
        if not cmd:
            return self.send_json({"code": 400, "error": "缺少 cmd"}, status=400)
        try:
            timeout = float(self._param(params, "timeout", str(DEFAULT_TIMEOUT)))
            timeout = max(0.1, timeout)
        except (TypeError, ValueError):
            return self.send_json({"code": 400, "error": "timeout 非法"}, status=400)
        cwd = self._param(params, "cwd") or None
        if cwd and not os.path.isdir(cwd):
            return self.send_json({"code": 400, "error": "cwd 不存在: %s" % cwd}, status=400)

        log.info("[exec] cmd=%r timeout=%s cwd=%s", cmd, timeout, cwd)
        start = time.time()
        exit_code, stdout, stderr, timed_out = run_command(cmd, timeout, cwd)
        self.send_json({
            "code": 1 if timed_out else 0,
            "cmd": cmd,
            "exit_code": exit_code,
            "stdout": stdout.decode("utf-8", "replace"),
            "stderr": stderr.decode("utf-8", "replace"),
            "timed_out": timed_out,
            "elapsed_ms": int((time.time() - start) * 1000),
        })

    # ---- 文件传输 ----
    def _resolve(self, raw_path):
        """把 ?path= 参数解析成磁盘路径；绝对路径直接用，相对路径拼 --root。"""
        p = urllib.parse.unquote(raw_path)
        if os.path.isabs(p):
            return os.path.normpath(p)
        return os.path.normpath(os.path.join(self.server.root, p))

    def _require_path(self, qs):
        p = self._param(qs, "path")
        if not p:
            self.send_json({"code": 400, "error": "缺少 path 参数"}, status=400)
            return None
        return self._resolve(p)

    def handle_upload(self, qs):
        target = self._require_path(qs)
        if target is None:
            return
        try:
            parent = os.path.dirname(target)
            if parent and not os.path.isdir(parent):
                os.makedirs(parent, exist_ok=True)
            length = int(self.headers.get("Content-Length", 0))
            remaining = length
            with open(target, "wb") as f:
                while remaining > 0:
                    chunk = self.rfile.read(min(CHUNK, remaining))
                    if not chunk:
                        break
                    f.write(chunk)
                    remaining -= len(chunk)
            st = os.stat(target)
            log.info("[upload] %s (%d bytes)", target, st.st_size)
            self.send_json({"code": 0, "path": target, "size": st.st_size})
        except (OSError, ValueError) as e:
            log.error("上传失败 %s: %s", target, e)
            self.send_json({"code": 1, "error": str(e)}, status=500)

    def handle_download(self, qs):
        target = self._require_path(qs)
        if target is None:
            return
        if not os.path.exists(target):
            return self.send_json({"code": 404, "error": "文件不存在: %s" % target}, status=404)
        if os.path.isdir(target):
            return self.send_json({"code": 400, "error": "是目录，请用 /ls 列目录"}, status=400)
        try:
            size = os.path.getsize(target)
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Disposition",
                             'attachment; filename="%s"' % os.path.basename(target))
            self.send_header("Content-Length", str(size))
            self.end_headers()
            with open(target, "rb") as f:
                shutil.copyfileobj(f, self.wfile)
            log.info("[download] %s (%d bytes)", target, size)
        except OSError as e:
            log.error("下载失败 %s: %s", target, e)
            self.send_json({"code": 1, "error": str(e)}, status=500)

    def handle_ls(self, qs):
        target = self._require_path(qs)
        if target is None:
            return
        if not os.path.isdir(target):
            return self.send_json({"code": 404, "error": "目录不存在: %s" % target}, status=404)
        try:
            entries = []
            for name in sorted(os.listdir(target)):
                fp = os.path.join(target, name)
                try:
                    st = os.lstat(fp)
                except OSError:
                    continue
                if os.path.isdir(fp):
                    ftype, size = "dir", 0
                else:
                    ftype, size = "file", st.st_size
                entries.append({
                    "name": name, "type": ftype, "size": size, "mtime": int(st.st_mtime),
                })
            self.send_json({"code": 0, "path": target, "entries": entries})
        except OSError as e:
            self.send_json({"code": 1, "error": str(e)}, status=500)

    def handle_mkdir(self, qs):
        target = self._require_path(qs)
        if target is None:
            return
        try:
            os.makedirs(target, exist_ok=True)
            self.send_json({"code": 0, "path": target})
        except OSError as e:
            self.send_json({"code": 1, "error": str(e)}, status=500)

    def handle_delete(self, qs):
        target = self._require_path(qs)
        if target is None:
            return
        if not os.path.exists(target):
            return self.send_json({"code": 404, "error": "路径不存在: %s" % target}, status=404)
        try:
            if os.path.isdir(target):
                shutil.rmtree(target)
            else:
                os.remove(target)
            self.send_json({"code": 0, "deleted": target})
        except OSError as e:
            self.send_json({"code": 1, "error": str(e)}, status=500)

    # ---- proxy 管理 ----
    def _handle_proxy_list(self):
        return self.send_json({"code": 0, "rules": self.server.proxy.list()})

    def _handle_proxy_add(self, params):
        if not isinstance(params, dict):
            return self.send_json({"code": 400, "error": "请求体必须是对象"}, status=400)
        if not (self._param(params, "listen_port") and self._param(params, "target_ip")
                and self._param(params, "target_port")):
            return self.send_json({"code": 400, "error": "缺少 listen_port/target_ip/target_port"}, status=400)
        try:
            rule = ProxyRule(
                listen_ip=self._param(params, "listen_ip", "0.0.0.0"),
                listen_port=int(self._param(params, "listen_port")),
                target_ip=self._param(params, "target_ip"),
                target_port=int(self._param(params, "target_port")),
                name=self._param(params, "name", ""),
                backlog=int(self._param(params, "backlog", str(DEFAULT_BACKLOG))),
            )
            if not rule.name:
                rule.name = "rule-%d" % int(time.time())
            added = self.server.proxy.add(rule)
            return self.send_json({"code": 0, "rule": added.to_dict()})
        except (TypeError, ValueError) as e:
            return self.send_json({"code": 400, "error": str(e)}, status=400)

    def _handle_proxy_delete(self, params):
        name = self._param(params, "name")
        if not name:
            return self.send_json({"code": 400, "error": "缺少 name"}, status=400)
        try:
            self.server.proxy.remove(name)
            return self.send_json({"code": 0, "deleted": name})
        except KeyError as e:
            return self.send_json({"code": 404, "error": e.args[0]}, status=404)

    # ---- 路由 ----
    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/ping":
            return self.send_json({"status": "ok", "time": int(time.time())})
        if not self._require_auth():
            return
        qs = urllib.parse.parse_qs(parsed.query)
        if parsed.path == "/exec":
            params = {
                "cmd": qs.get("cmd", [""])[0],
                "timeout": qs.get("timeout", [str(DEFAULT_TIMEOUT)])[0],
            }
            return self._handle_exec(params)
        if parsed.path == "/ls":
            return self.handle_ls(qs)
        if parsed.path == "/download":
            return self.handle_download(qs)
        if parsed.path == "/proxy":
            return self._handle_proxy_list()
        return self.send_json({"code": 404, "error": "not found"}, status=404)

    def do_POST(self):
        if not self._require_auth():
            return
        parsed = urllib.parse.urlparse(self.path)
        qs = urllib.parse.parse_qs(parsed.query)
        if parsed.path == "/exec":
            raw = self._read_body()
            try:
                params = json.loads(raw) if raw else {}
            except ValueError:
                return self.send_json({"code": 400, "error": "body 不是合法 JSON"}, status=400)
            return self._handle_exec(params)
        if parsed.path == "/upload":
            return self.handle_upload(qs)
        if parsed.path == "/mkdir":
            return self.handle_mkdir(qs)
        if parsed.path == "/delete":
            return self.handle_delete(qs)
        if parsed.path == "/proxy/add":
            raw = self._read_body()
            try:
                params = json.loads(raw) if raw else qs
            except ValueError:
                params = qs
            return self._handle_proxy_add(params)
        if parsed.path == "/proxy/delete":
            raw = self._read_body()
            try:
                params = json.loads(raw) if raw else qs
            except ValueError:
                params = qs
            return self._handle_proxy_delete(params)
        return self.send_json({"code": 404, "error": "not found"}, status=404)

    def do_PUT(self):
        if not self._require_auth():
            return
        parsed = urllib.parse.urlparse(self.path)
        qs = urllib.parse.parse_qs(parsed.query)
        if parsed.path == "/upload":
            return self.handle_upload(qs)
        return self.send_json({"code": 404, "error": "not found"}, status=404)

    def do_DELETE(self):
        if not self._require_auth():
            return
        parsed = urllib.parse.urlparse(self.path)
        qs = urllib.parse.parse_qs(parsed.query)
        if parsed.path == "/proxy":
            return self._handle_proxy_delete(qs)
        return self.send_json({"code": 404, "error": "not found"}, status=404)


def cmd_server(args):
    server = ThreadingHTTPServer((args.host, args.port), AgentHandler)
    server.token = args.token
    server.root = os.path.abspath(args.root)
    server.proxy = ProxyManager()

    # 预加载 proxy 规则（可选）：--config 优先，否则单条规则参数
    try:
        if args.config:
            for rule in parse_rules(load_config(args.config)):
                server.proxy.add(rule)
        elif args.listen_port and args.target_ip and args.target_port:
            server.proxy.add(ProxyRule(
                listen_ip=args.listen_ip or "0.0.0.0",
                listen_port=args.listen_port,
                target_ip=args.target_ip,
                target_port=args.target_port,
                name="cli-rule",
                backlog=args.backlog,
            ))
    except (ValueError, OSError) as e:
        fail("预加载 proxy 规则失败: %s" % e)

    log.info("agent 服务器已启动: http://%s:%d  root=%s  token=%s  (exec + transfile + proxy 同一进程)",
             args.host, args.port, server.root, "on" if args.token else "off")

    def shutdown(*_):
        log.info("收到退出信号，退出")
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


# ============================== 入口 ==============================

def main():
    parser = argparse.ArgumentParser(
        prog="tools.py",
        description="NEFS tools 统一脚本：一个 server 同时提供命令执行 + 文件传输 + proxy 动态转发。")
    p_server = parser.add_subparsers(dest="command").add_parser(
        "server", help="节点 agent：命令执行 + 文件传输 + proxy 管理（同一端口）")

    p_server.add_argument("--host", default="0.0.0.0", help="监听地址（默认 0.0.0.0）")
    p_server.add_argument("--port", type=int, default=DEFAULT_PORT,
                          help="HTTP 监听端口（默认 %d）" % DEFAULT_PORT)
    p_server.add_argument("--root", default=os.getcwd(),
                          help="文件传输相对路径的基准目录（默认当前目录）")
    p_server.add_argument("--token", default=DEFAULT_TOKEN,
                          help="鉴权 token（默认 %s），请求需带 X-Token 头" % DEFAULT_TOKEN)
    p_server.add_argument("--config", default=None,
                          help="启动时预加载的 proxy 规则配置文件（JSON，与旧 proxy 格式一致）")
    p_server.add_argument("--listen-ip", help="预加载单条 proxy 规则：本地监听 IP")
    p_server.add_argument("--listen-port", type=int, help="预加载单条 proxy 规则：本地监听端口")
    p_server.add_argument("--target-ip", help="预加载单条 proxy 规则：目标 IP")
    p_server.add_argument("--target-port", type=int, help="预加载单条 proxy 规则：目标端口")
    p_server.add_argument("--backlog", type=int, default=DEFAULT_BACKLOG,
                          help="proxy 监听队列长度（默认 %d）" % DEFAULT_BACKLOG)
    p_server.set_defaults(func=cmd_server)

    args = parser.parse_args()
    if not args.command:
        parser.print_help()
        sys.exit(1)
    args.func(args)


if __name__ == "__main__":
    main()