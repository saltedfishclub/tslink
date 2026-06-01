# tslink

基于 [Tailscale](https://tailscale.com) `tsnet` 的轻量双向流量转发工具，可将本机服务暴露到 Tailnet，也可将 Tailnet 服务通过本机端口对外暴露。

## 特性

- **双向转发**：`forward`（Tailscale → 本地）与 `connect`（本地 → Tailscale）两种模式
- **TCP / UDP 全支持**：透明转发 TCP 流与 UDP 数据包
- **Minecraft 专用模式**：支持局域网广播发现（MOTD），让本地设备发现 Tailnet 上的 Minecraft 服务器
- **MagicDNS 主机名补全**：`dst_addr` 支持短主机名（如 `home:8080`），启动时自动补全为 `home.<suffix>:8080`
- **连接类型识别**：区分 `direct` 直连与 `derp` 中继，便于排查延迟问题
- **对端连通性诊断**：定期 ping 目标节点并报告延迟与连接路径（direct/DERP）
- **Web 管理**：内置 Tailscale Web Client（端口 `5252`），可在线管理节点配置
- **多配置源**：支持本地 TOML 文件、HTTP/HTTPS URL、构建时注入默认 URL

## 环境要求

- Go 1.26+
- Tailscale / Headscale 授权密钥

## 快速开始

```powershell
go build -o tslink.exe .
.\tslink.exe -c config.toml
```

详细配置与使用说明见 [USAGE.md](USAGE.md)。

## 工作原理

```
forward (你 -> 其他人):
  Tailscale Client ──TCP/UDP──> [本机 Tailscale IP:port] ──转发──> [本地服务 127.0.0.1:port]

connect (其他人 -> 你):
  本地/LAN 客户端 ──TCP/UDP──> [本机监听 port] ──转发──> [Tailscale 目标 host:port]
```
