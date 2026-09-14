# 一键部署 mgmt 三节点集群（本机 PowerShell 运行）
# 依赖: nefs-proxy 通道(9527)、三节点已有镜像 IMAGE、mgmt-deploy skill 脚本
#
# 前提(由 skill 手动完成):
#   1. 编译 + 构建镜像, docker save/load 到三节点 (见 SKILL.md)
#   2. 若 12 有裸机 mgmt 已停掉释放 5901/5902
#
# 用法:
#   powershell -File deploy_cluster.ps1 [-Image nefs-mgmt:1.6.0-deploy] [-Listen 12379] [-ClusterName nefs-mgmt-deploy]
#   -ScriptLocal 指向 deploy_node.sh 本地路径, 默认取本脚本同目录脚本
# 每个节点执行前会询问确认(只打印命令不自动跑, 需人工确认), 也可传 -Force 直接执行

param(
  [string]$Image = "nefs-mgmt:1.6.0-deploy",
  [string]$Listen = "12379",
  [string]$ClusterName = "nefs-mgmt-deploy",
  [string]$ScriptLocal = "",
  [switch]$Force
)

$Token = "95279527"
$Nodes = @{
  11 = "100.71.128.11"
  12 = "100.71.128.12"
  13 = "100.71.128.13"
}
$Peers = "1:$($Nodes[11]):$Listen,2:$($Nodes[12]):$Listen,3:$($Nodes[13]):$Listen"

$SkillRoot = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent  # .../mgmt-deploy
if (-not $ScriptLocal) { $ScriptLocal = Join-Path $SkillRoot "scripts\deploy_node.sh" }
$TplLocal = Join-Path $SkillRoot "conf\mgmt.conf.tpl"
if (-not (Test-Path $ScriptLocal)) { throw "deploy_node.sh 不存在: $ScriptLocal" }
if (-not (Test-Path $TplLocal)) { throw "mgmt.conf.tpl 不存在: $TplLocal" }

function Invoke-NodeExec($ip, $cmd, $timeout=120) {
  $body = @{ cmd = $cmd; timeout = $timeout } | ConvertTo-Json -Compress
  return (Invoke-RestMethod -Uri "http://${ip}:9527/exec" -Method POST `
    -Headers @{"X-Token"=$Token} -ContentType "application/json" -Body $body).stdout
}

function Invoke-NodeUpload($ip, $local, $remote) {
  & curl.exe -s -X PUT -H "X-Token: $Token" --data-binary "@$local" "http://${ip}:9527/upload?path=$remote"
}

Write-Host "== 部署 plan =="
Write-Host "  Image   : $Image"
Write-Host "  Listen  : $Listen"
Write-Host "  Cluster : $ClusterName"
Write-Host "  Peers   : $Peers"
if (-not $Force) {
  $ans = Read-Host "确认部署到 $($Nodes.Values -join ', ') ? [y/N]"
  if ($ans -ne 'y') { Write-Host "已取消"; exit 0 }
}

foreach ($k in 11,12,13) {
  $ip = $Nodes[$k]
  Write-Host "===== 部署节点 $k ($ip) ====="
  # 1. 上传脚本与模板到节点
  Invoke-NodeUpload $ip $ScriptLocal "/tmp/deploy_node.sh"
  Invoke-NodeUpload $ip $TplLocal "/tmp/mgmt.conf.tpl"
  # 2. 上传模板路径: 脚本默认取 ../conf, 解压到 /tmp/skill 保证相对引用
  Invoke-NodeExec $ip "mkdir -p /tmp/mgmt-skill/scripts /tmp/mgmt-skill/conf && cp /tmp/deploy_node.sh /tmp/mgmt-skill/scripts/ && cp /tmp/mgmt.conf.tpl /tmp/mgmt-skill/conf/ && chmod +x /tmp/mgmt-skill/scripts/deploy_node.sh"
  # 3. 执行节点脚本
  $cmd = "bash /tmp/mgmt-skill/scripts/deploy_node.sh $k $ip `"$Peers`" `"$ClusterName`" /export/Data/mgmt-deploy /export/Logs/mgmt-deploy '' $Image $Listen"
  $out = Invoke-NodeExec $ip $cmd
  Write-Host $out
}

Write-Host "===== 全部节点启动指令已下发, 等待 Raft 选主(约 15s) ====="
Start-Sleep -Seconds 15
foreach ($k in 11,12,13) {
  $ip = $Nodes[$k]
  $out = Invoke-NodeExec $ip "curl -s -m 5 http://127.0.0.1:${Listen}/mgmt/v1/liveness; echo; curl -s -m 5 http://127.0.0.1:${Listen}/mgmt/v1/mgmt-cluster-info"
  Write-Host "--- $k ---"
  Write-Host $out
}