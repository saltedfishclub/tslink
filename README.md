# tsnet_pure 使用说明

`tsnet_pure` 是一个基于 `tailscale.com/tsnet` 的轻量转发工具，支持：

- `forward`：将 **Tailscale 入站** 请求转发到本机服务（你 -> 其他人）。
- `connect`：将 **本机/LAN 入站** 请求转发到 Tailscale 目标（其他人 -> 你）。

支持 `tcp`、`udp`，并对 `minecraft` 协议提供局域网发现能力。

## 1. 环境要求

- Go 1.26+
- 可用的 Tailscale `auth_key`（或 Headscale 对应密钥）

## 2. 安装与构建

在项目根目录执行：

```powershell
go mod tidy
go build -o tslink.exe .
```

如果你已有编译产物，也可以直接运行现成的 `tslink.exe`。

## 3. 配置文件

默认使用根目录 `config.toml`，可通过参数 `-c` 指定其他路径。

项目已提供 `config.example.toml`，可复制后修改：

```powershell
Copy-Item .\config.example.toml .\config.toml
```

### 3.1 `[core]` 配置

```toml
[core]
auth_key = ""  # 必填
control_url = "https://controlplane.tailscale.com"  # 官方控制面或 Headscale
hostname = ""  # 留空时自动使用机器名
ephemeral = true
accept_routes = true
```

- `auth_key`：Tailscale/Headscale 授权密钥。
- `control_url`：默认是官方控制面，使用 Headscale 时改为你的实例地址。
- `hostname`：节点名，为空则自动取系统主机名。
- `ephemeral`：是否使用临时节点。
- `accept_routes`：是否自动启用路由接收（`RouteAll`）。

### 3.2 `[[forward.<tag>]]` 规则（你 -> 其他人）

```toml
[[forward.web]]
protocol = "tcp"
tailscale_port = 8080
local_addr = "127.0.0.1:9090"
```

含义：监听本机 Tailscale IP 的 `8080`，转发到本地 `127.0.0.1:9090`。

字段说明：

- `protocol`：`tcp` 或 `udp`
- `tailscale_port`：对 Tailnet 暴露端口
- `local_addr`：本地目标地址（`host:port`）

### 3.3 `[[connect.<tag>]]` 规则（其他人 -> 你）

```toml
[[connect.web]]
protocol = "tcp"
local_port = 9000
dst_addr = "any-client-in.ts.net:8080"
```

含义：监听本机 `9000`，流量转发到 Tailscale 目标 `any-client-in.ts.net:8080`。

字段说明：

- `protocol`：`tcp`、`udp` 或 `minecraft`
- `local_port`：本地监听端口
- `local_addr`：本地监听地址（可选，默认 `0.0.0.0`）
- `dst_addr`：Tailscale 目标地址（`host:port`）
- `lan_enable`：仅 `minecraft` 场景常用；不填时 `minecraft` 默认启用
- `lan_motd`：Minecraft 局域网广播提示文案

Minecraft 示例：

```toml
[[connect.minecraft]]
protocol = "minecraft"
local_port = 25565
dst_addr = "any-client-in.ts.net:25566"
lan_enable = true
lan_motd = "Minecraft via Tailscale"
```

## 4. 启动方式

```powershell
.\tslink.exe -c config.toml
```

常用参数：

- `-c`：配置文件路径，默认 `config.toml`
- `-level`：日志级别，默认 `info`（可用：`debug|info|warn|error`）
- `-json-format`：输出 JSON 日志
- `-diagnose`：启用 tsnet debug 日志

示例：

```powershell
.\tslink.exe -c .\config.toml -level debug -diagnose
```

## 5. 运行与退出

- 启动后程序会初始化 tsnet 并按配置启动所有 `forward/connect` 规则。
- 按 `Ctrl + C` 可优雅退出。
- 内置 watchdog 可能在异常时自动触发重启逻辑。

## 6. 常见问题

### 6.1 首次启动失败/无法入网

- 检查 `auth_key` 是否正确、是否过期。
- 若使用 Headscale，确认 `control_url` 可访问且 TLS/证书配置正确。

### 6.2 端口无法访问

- 检查本地防火墙与目标服务是否真的在 `local_addr` 监听。
- 确认 `dst_addr` 可在 Tailnet 内解析并连通。
- 核对 `protocol` 是否与目标服务一致（`tcp/udp` 不可混用）。

### 6.3 日志排查建议

- 使用 `-level debug` 查看更详细转发日志。
- 需要 tsnet 内部信息时加 `-diagnose`。
