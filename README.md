# tslink

基于 [Tailscale](https://tailscale.com) `tsnet` 的轻量双向流量转发工具，可将本机服务暴露到 Tailnet，也可将 Tailnet 服务通过本机端口对外暴露。

## 特性

- **双向转发**：`forward`（Tailscale → 本地）与 `connect`（本地 → Tailscale）两种模式
- **TCP / UDP 全支持**：透明转发 TCP 流与 UDP 数据包
- **Minecraft 专用模式**：支持局域网广播发现（MOTD），让本地设备发现 Tailnet 上的 Minecraft 服务器
- **MagicDNS 主机名补全**：`dst_addr` 支持按照 Tailscale 规则正确解析 Split DNS 和 Magic DNS
- **连接类型识别**：区分 `direct` 直连与 `derp` 中继，便于排查延迟问题
- **对端连通性诊断**：定期 ping 目标节点并报告延迟与连接路径（direct/DERP）
- **原生图形界面**：可选的 `tslink-gui`，基于 [Gio](https://gioui.org) 绘制，无 WebView、单文件、跨平台
- **网络诊断**：NAT 类型判定（RFC 5780）、UDP 连通性、UPnP/NAT-PMP/PCP、境外可达性、出口 IP 与归属地
- **Web 管理**：内置 Tailscale Web Client（端口 `5252`），可在线管理节点配置
- **多配置源**：支持本地 TOML 文件、HTTP/HTTPS URL、构建时注入默认 URL

## 环境要求

- Tailscale / Headscale 授权密钥

## 快速开始

1. 前往 [Releases](https://github.com/saltedfishclub/tslink/releases) 下载对应平台的二进制文件，赋予执行权限后放入 PATH

2. 准备配置文件：
   ```bash
   cp config.example.toml config.toml
   vim config.toml  # 填入 auth_key 并配置转发规则
   ```

3. 启动：
   ```bash
   tslink -c config.toml
   ```

## 图形界面

如果你更习惯图形界面，下载 `tslink-gui` 并用同一份配置启动：

```bash
tslink-gui -c config.toml
```

它在同一个进程里运行完整的转发服务，并额外提供：

- **节点**：所关联 Tailscale 节点的在线状态、链路类型（直连 / DERP / 对等中继）与**延迟图谱**
- **局域网**：监听 `224.0.2.60:4445`，列出局域网内广播的 Minecraft 服务器，并标出哪些是 tslink 自己转发的
- **网络诊断**：NAT 类型、UDP 连通性、本机全部 IPv4/IPv6 出口、UPnP/NAT-PMP/PCP、境外连通性（`cp.cloudflare.com`）、出口 IP 与归属地
- **日志**：全量检索、过滤，一键复制或**上传到公共 paste 服务**生成分享链接
- 启动过程中日志以半透明浮层呈现，截图求助时无需再单独翻日志

`tslink-gui` 与无界面的 `tslink` 是两个独立的二进制：服务器和容器部署继续用后者，它不含任何图形依赖。

## 容器部署

```bash
cp config.example.toml config.toml
# 编辑 config.toml，然后对照修改 docker-compose.yml 中的端口映射
docker compose up -d
```

详细配置与使用说明见 [USAGE.md](USAGE.md)。

## 工作原理

```
forward (你 -> 其他人):
  Tailscale Client ──TCP/UDP──> [本机 Tailscale IP:port] ──转发──> [本地服务 127.0.0.1:port]

connect (其他人 -> 你):
  本地/LAN 客户端 ──TCP/UDP──> [本机监听 port] ──转发──> [Tailscale 目标 host:port]
```
