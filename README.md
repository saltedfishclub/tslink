# tslink

基于 [Tailscale](https://tailscale.com) `tsnet` 的轻量双向流量转发工具，可将本机服务暴露到 Tailnet，也可将 Tailnet 服务通过本机端口对外暴露。

## 特性

- **双向转发**：`forward`（Tailscale → 本地）与 `connect`（本地 → Tailscale）两种模式
- **TCP / UDP 全支持**：透明转发 TCP 流与 UDP 数据包
- **Minecraft 专用模式**：支持局域网广播发现（MOTD），让本地设备发现 Tailnet 上的 Minecraft 服务器
- **MagicDNS 主机名补全**：`dst_addr` 支持按照 Tailscale 规则正确解析 Split DNS 和 Magic DNS
- **连接类型识别**：区分 `direct` 直连与 `derp` 中继，便于排查延迟问题
- **对端连通性诊断**：定期 ping 目标节点并报告延迟与连接路径（direct/DERP）
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
