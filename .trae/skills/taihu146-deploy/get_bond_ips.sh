#!/bin/bash
# 查询本节点 bond1.2175 / bond2.2372 的 IPv4 地址（部署前逐节点执行，防止 IP 漂移）。
export PATH=/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH
B1=$(ip -4 -o addr show bond1.2175 2>/dev/null | awk '{print $4}' | cut -d/ -f1)
B2=$(ip -4 -o addr show bond2.2372 2>/dev/null | awk '{print $4}' | cut -d/ -f1)
echo "BOND1=$B1 BOND2=$B2"