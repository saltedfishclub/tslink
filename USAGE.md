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

## 图形界面 `tslink-gui`

`tslink-gui` 是可选的桌面前端，使用 [Gio](https://gioui.org) 直接绘制界面——不含 WebView、不打包浏览器，Linux / macOS / Windows 各是一个原生可执行文件（约 30 MB）。

它在自身进程内运行与无界面版**完全相同**的转发服务，因此配置文件、规则语义和行为都一致：

```bash
tslink-gui -c config.toml
```

### 命令行参数

除下列参数外，`-c`、`-config-url`、`-level`、`-json-format`、`-diagnose` 与无界面版含义相同。

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-light` | `false` | 以浅色主题启动（默认深色） |
| `-ipinfo-token` | `$IPINFO_TOKEN` | ipinfo.io 的 API Token，可选，用于提高归属地查询的速率限制 |
| `-pprof` | 空 | 在指定地址暴露 `net/http/pprof`，如 `127.0.0.1:6060`。仅允许回环地址 |

无论 `-level` 设为什么，界面内的日志缓冲区**始终按 debug 级别**记录最近 20000 条，所以出问题后不必重启加 `-level debug` 再复现一次。

### 界面说明

| 页面 | 内容 |
|------|------|
| **概览** | 在线节点数、局域网服务器数、规则数、运行时长，以及最近一次诊断的结论 |
| **节点** | 每个 Tailscale 节点的在线状态、链路类型、实时延迟、抖动与丢包；顶部为多节点**延迟图谱**（最近 20 分钟，鼠标悬停可查看某一时刻的取值，点击图例可隐藏某个节点） |
| **局域网** | 监听 `224.0.2.60:4445` / `[ff75:230::60]:4445` 的 Minecraft LAN 广播。由 tslink 自己广播的条目会标记为「本机广播」——**配置了规则却听不到自己的广播，说明隧道或组播链路有问题** |
| **网络诊断** | 见下 |
| **日志** | 按级别、来源、关键字检索，复制 / 保存 / 上传 |
| **设置** | 主题、语言、版本与配置来源 |

启动过程中，界面显示分步进度的加载动画；实时日志以**半透明浮层**固定在底部，因此启动卡住时直接截图就包含了排查所需的信息。服务就绪后，浮层可通过标题栏按钮随时唤出。

### 网络诊断

点击「开始诊断」后并行执行以下检查，整体不超过 45 秒：

| 检查项 | 说明 |
|--------|------|
| **NAT 类型** | 依 RFC 5780 做映射行为与过滤行为探测，并映射到常见的完全锥形 / 地址限制 / 端口限制 / 对称型命名。对称型 NAT 会导致打洞失败、连接回退到 DERP 中继 |
| **UDP 连通性** | 对国内与境外 STUN 服务器分别探测 IPv4/IPv6，并识别疑似被封锁的目标端口 |
| **本机出口地址** | 列出所有接口上的 IPv4 与 IPv6 地址，标注默认出口以及 CGNAT / Tailscale / 私有 / 公网等类型 |
| **端口映射** | 自行实现的 UPnP IGD（SSDP + SOAP）、NAT-PMP（RFC 6886）与 PCP（RFC 6887）探测，能拿到路由器型号与外部地址 |
| **境外连通性** | 以 `cp.cloudflare.com/generate_204` 为主，辅以 gstatic / Google，并用国内基准（小米 / 百度）区分「完全没网」与「只是出不了境」 |
| **出口 IP 与归属地** | 通过 STUN（裸 UDP，绕过 HTTP 代理）、强制 IPv4、强制 IPv6、以及走系统代理四种方式分别探测，再用 ipinfo.io（失败时回退 ip-api.com / ip.sb）查询归属地 |
| **Tailscale 内部状态** | 直接调用 tailscale 自己的 netcheck，取得 DERP 各区域延迟、首选中继、门户劫持判定，以及它自己看到的 UPnP/PMP/PCP 结果 |

STUN 服务器**同时包含国内与境外**两组（小米、B 站、腾讯、芒果 TV、Cloudflare 任播 / Google、Cloudflare、Nextcloud、BlackBerry、SipNet、StunProtocol）。这不只是为了容错：当本机启用了代理或分流工具时，不同探测路径会得到**不同的公网 IP**，诊断页会把这种分歧单独标出来——这通常正是「为什么对端连不上我」的答案。

> 归属地查询会把你的公网 IP 发送给第三方服务。不希望如此时，勾选「不查询归属地」即可跳过。
>
> 出口 IP 的「不一致」判定按 IPv4 / IPv6 分别计算，双栈主机同时拥有一个 v4 和一个 v6 出口属于正常情况，不会被误报。

### 导出与分享日志

日志页提供三种导出方式，都会附带一段环境信息头（版本、系统、配置来源、运行阶段、节点数），以及最近一次的完整诊断报告：

- **复制到剪贴板**
- **保存到文件**：写入用户主目录，文件名形如 `tslink-log-20260726-084500.txt`
- **上传并分享**：依次尝试 0x0.st、paste.rs、dpaste.org、termbin.com，成功后返回链接并自动复制

导出默认开启「隐去密钥」，会移除 `auth_key` 等凭据以及形如 `tskey-...` 的字符串。**上传是公开的**——任何拿到链接的人都能看到内容，其中包含你的公网 IP 与内网地址，请自行判断。

### 从源码构建

Windows 无需额外依赖。macOS 需要 Xcode Command Line Tools。Linux 需要 X11 / Wayland / EGL 的开发头文件：

```bash
sudo apt install -y pkg-config libwayland-dev libx11-dev libx11-xcb-dev \
  libxkbcommon-dev libxkbcommon-x11-dev libgles2-mesa-dev libegl1-mesa-dev \
  libffi-dev libxcursor-dev libxrandr-dev libxinerama-dev libxi-dev libxxf86vm-dev

go build -o tslink-gui ./cmd/tslink-gui
```

界面语言默认跟随中文字体的可用性：找不到任何中文字体时自动切换为英文，以免显示成方块（也可在设置里手动切换）。

由于 Gio 依赖 CGO，GUI 无法像无界面版那样交叉编译，需要在目标平台上分别构建。
