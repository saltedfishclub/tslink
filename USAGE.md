# USAGE

## 配置文件

配置使用 [TOML](https://toml.io) 格式，支持本地文件或远程 URL。

### `[core]` — 核心配置

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `auth_key` | string | **是** | — | Tailscale / Headscale 授权密钥（可在控制面生成） |
| `control_url` | string | 否 | `https://controlplane.tailscale.com` | 控制面地址，使用 Headscale 时改为自建实例地址 |
| `hostname` | string | 否 | 本机主机名 | 在 Tailnet 中的节点名称 |
| `ephemeral` | bool | 否 | `true` | 节点是否临时节点，离开 Tailnet 后自动删除 |
| `accept_routes` | bool | 否 | `true` | 是否接受其他节点发布的子网路由 |

### `[dns]` — DNS 解析配置

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `doh_servers` | []string | 否 | `[]` | DNS-over-HTTPS（RFC 8484）回落解析器地址列表，须为 `http(s)://` URL |

`dst_addr` 中的域名默认走 Tailnet 自身的 DNS 解析器（支持 MagicDNS 与 split-DNS）。当该解析器无法解析目标（例如宿主机本身没有可用的系统 DNS，或目标不在 Tailnet 的 split-DNS 路由内）时，会**依次**尝试 `doh_servers` 中配置的 DoH 端点解析公网域名。留空则关闭此回落。仅对原始域名发起 DoH 查询（Tailnet 内部 MagicDNS 名称无法通过 DoH 解析）。

### `[[forward.<name>]]` — 转发规则（Tailscale → 本地）

将 Tailscale 上的流量转发到本地服务。`<name>` 为自定义标签名。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `protocol` | string | **是** | `tcp` 或 `udp` |
| `tailscale_port` | int | **是** | 在 Tailscale IP 上监听的端口，1-65535 |
| `local_addr` | string | **是** | 本地转发目标地址，`host:port` 格式 |

**注意事项：** 容器环境下 `local_addr` 中的 `127.0.0.1` 指向容器自身；若要转发到宿主机服务，需使用桥接网络的 `host.docker.internal` 或 host 模式下的真实 IP。

### `[[connect.<name>]]` — 连接规则（本地 → Tailscale）

在本机监听端口，将接入的流量转发到 Tailnet 中的目标。`<name>` 为自定义标签名。

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `protocol` | string | **是** | — | `tcp`、`udp` 或 `minecraft` |
| `local_port` | int | **是** | — | 本地监听端口，1-65535 |
| `dst_addr` | string | **是** | — | Tailscale 目标地址，`host:port` 格式，支持 MagicDNS 主机名（如 `my-server.ts.net:8080`） |
| `local_addr` | string | 否 | `127.0.0.1` | 监听 IP 地址，设为 `0.0.0.0` 可暴露到局域网 |
| `lan_enable` | bool | 否 | `minecraft` 时为 `true`，其余为 `false` | 启用 LAN 多播发现（Minecraft 专用） |
| `lan_motd` | string | 否 | `Minecraft via Tailscale` | LAN 广播的 MOTD 文本 |

**Minecraft 模式说明：** `protocol = "minecraft"` 实质为 TCP 转发，额外在多播地址 `224.0.2.60:4445`（IPv4）和 `ff75:230::60:4445`（IPv6）上发送 LAN 广播，使局域网内的 Minecraft 客户端可直接发现服务器。

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-c` | `config.toml` | 配置文件路径或 HTTP(S) URL |
| `-config-url` | 构建时注入 | 远程配置 URL（`-c` 指定 URL 时优先使用 `-c`） |
| `-level` | `info` | 日志级别：`debug`、`info`、`warn`、`error` |
| `-json-format` | `false` | 使用 JSON 格式输出日志 |
| `-diagnose` | `false` | 输出 tsnet 内部调试信息（需 `-level debug`） |

## 配置来源

### 本地文件

```bash
tslink -c /path/to/config.toml
```

如果指定路径的文件不存在，tslink 会自动生成一份默认配置模板。

### 远程 URL

配置可通过 HTTP(S) 远程加载，启动后不会将密钥写入磁盘：

```bash
tslink -c https://example.com/tslink.toml
```

### 构建时注入

可在编译时注入默认配置 URL，适用于分发场景：

```bash
go build -ldflags "-X tslink/core.DefaultConfigURL=https://example.com/tslink.toml"
```

当未指定 `-c` 和 `-config-url` 时，自动使用该 URL。

## 完整示例

```toml
[core]
auth_key = "tskey-auth-..."            # 必填，Tailscale 授权密钥
control_url = "https://controlplane.tailscale.com"  # 可选，Headscale 用户改为自建实例
hostname = ""                           # 可选，留空使用本机主机名
ephemeral = true                        # 可选，临时节点
accept_routes = true                    # 可选，接受子网路由

[dns]
# 可选，Tailnet DNS 无法解析时回落到 DoH 解析公网域名；留空关闭
doh_servers = ["https://cloudflare-dns.com/dns-query", "https://dns.google/dns-query"]

# 示例1: 将 Tailnet 上 8080 端口的请求转发到本地 9090
[[forward.web]]
protocol = "tcp"
tailscale_port = 8080
local_addr = "127.0.0.1:9090"

# 示例2: 将 Tailnet 上的 UDP 流量转发到本地
[[forward.dns_udp]]
protocol = "udp"
tailscale_port = 5353
local_addr = "127.0.0.1:53"

# 示例3: 本机监听 9000，转发到 Tailnet 中某主机的 8080
[[connect.web]]
protocol = "tcp"
local_port = 9000
local_addr = "127.0.0.1"                # 可选，仅本机可访问
dst_addr = "other-host.ts.net:8080"

# 示例4: 本机监听 0.0.0.0:25565，转发到 Tailnet 中的 Minecraft 服务器
[[connect.minecraft]]
protocol = "minecraft"
local_port = 25565
dst_addr = "mc-server.ts.net:25566"
lan_enable = true                       # 可选，启用 LAN 多播发现
lan_motd = "Minecraft via Tailscale"    # 可选，自定义 MOTD

# 示例5: UDP 转发
[[connect.udp_example]]
protocol = "udp"
local_port = 24454
dst_addr = "tailnet-game.ts.net:24454"
```

## 容器部署

项目根目录提供了 `docker-compose.yml`，使用前需：

1. 准备好 `config.toml` 并放在项目根目录
2. 根据配置中的 `connect` 规则，修改 `docker-compose.yml` 中的 `ports:` 映射
3. 启动：

```bash
docker compose up -d
```

也可以直接拉取镜像部署：

```bash
docker pull ghcr.io/saltedfishclub/tslink:latest
docker run -d \
  --name tslink \
  -v $(pwd)/config.toml:/etc/tslink/config.toml:ro \
  -p 9000:9000 \
  -p 25565:25565 \
  ghcr.io/saltedfishclub/tslink:latest \
  -c /etc/tslink/config.toml
```
