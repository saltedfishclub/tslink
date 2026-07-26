package gui

// Lang selects the UI label set. Chinese is the project's primary audience;
// English exists because a machine without a CJK font cannot draw Chinese, and
// silently rendering tofu boxes would be worse than translating.
type Lang int

const (
	LangZH Lang = iota
	LangEN
)

// Name is the language's own name, for the settings toggle.
func (l Lang) Name() string {
	if l == LangEN {
		return "English"
	}
	return "中文"
}

// Key identifies a translatable string.
type Key int

const (
	KAppTitle Key = iota
	KAppSubtitle

	// Navigation.
	KNavOverview
	KNavPeers
	KNavDiag
	KNavLogs
	KNavSettings

	// Service lifecycle.
	KStateStarting
	KStateConnecting
	KStateRunning
	KStateDegraded
	KStateStopped
	KStateError
	KStateRetrying

	// Splash steps.
	KStepConfig
	KStepFonts
	KStepTsnet
	KStepRules
	KStepDiscovery
	KStepMonitors
	KStepReady
	KSplashHint
	KSplashStuckHint
	KSplashExportLog
	KSplashRetry

	// Shared vocabulary.
	KYes
	KNo
	KUnknown
	KSupported
	KUnsupported
	KEnabled
	KDisabled
	KNone
	KRefresh
	KRetry
	KClose
	KCopy
	KCopied
	KDetails
	KLoading
	KError
	KNever
	KJustNow
	KSecondsAgo
	KMinutesAgo
	KHoursAgo
	KTotal
	KOnline
	KOffline

	// Overview.
	KOvTailnet
	KOvSelf
	KOvPeersOnline
	KOvForwardRules
	KOvConnectRules
	KOvUptime
	KOvHealth
	KOvQuickDiag
	KOvNoIssues

	// Peers page.
	KPeersTitle
	KPeersLinked
	KPeersEmpty
	KPeersResolving
	KPeerLatency
	KPeerRoute
	KPeerRouteDirect
	KPeerRouteDERP
	KPeerRoutePeerRelay
	KPeerRouteOffline
	KPeerRouteUnknown
	KPeerAvg
	KPeerMin
	KPeerMax
	KPeerJitter
	KPeerLoss
	KPeerRx
	KPeerTx
	KPeerLastSeen
	KPeerLastHandshake
	KPeerAddresses
	KPeerEndpoint
	KPeerOS
	KPeerExitNode
	KPeerTags
	KGraphTitle
	KGraphEmpty
	KGraphWindow
	KGraphLegendHint

	// Local services (overview).
	KSvcTitle
	KSvcSubtitle
	KSvcEmpty
	KSvcBroadcast

	// Diagnostics page.
	KDiagTitle
	KDiagRun
	KDiagRunning
	KDiagRerun
	KDiagNever
	KDiagLastRun
	KDiagCopyReport
	KDiagSecIface
	KDiagSecUDP
	KDiagSecNAT
	KDiagSecPortMap
	KDiagSecOverseas
	KDiagSecEgress
	KDiagSecTailscale
	KDiagNatType
	KDiagNatMapping
	KDiagNatFiltering
	KDiagNatHairpin
	KDiagNatPortPreserve
	KDiagUdpV4
	KDiagUdpV6
	KDiagUdpPortsOK
	KDiagUdpPortsBlocked
	KDiagIfaceDefaultV4
	KDiagIfaceDefaultV6
	KDiagUPnP
	KDiagNATPMP
	KDiagPCP
	KDiagGateway
	KDiagExternalIP
	KDiagOverseasTarget
	KDiagEgressMethod
	KDiagEgressIP
	KDiagEgressGeo
	KDiagEgressDivergent
	KDiagEgressDivergentHint
	KDiagEgressDivergentHTTP
	KDiagGeoSkipped
	KDiagPreferredDERP
	KDiagDerpLatency
	KDiagCaptivePortal
	KDiagMappingVaries
	KDiagSkipGeo
	KDiagSkipGeoHint

	// NAT names.
	KNatOpen
	KNatFullCone
	KNatRestricted
	KNatPortRestricted
	KNatSymmetric
	KNatUDPBlocked
	KNatSymmetricFW
	KNatUnknown

	// Logs page.
	KLogsTitle
	KLogsSearch
	KLogsLevel
	KLogsSource
	KLogsFollow
	KLogsAll
	KLogsEmpty
	KLogsCopyAll
	KLogsSaveFile
	KLogsUpload
	KLogsUploading
	KLogsUploaded
	KLogsUploadFail
	KLogsRedact
	KLogsRedactHint
	KLogsShown
	KLogsDropped
	KLogsIncludeDiag

	// Settings.
	KSetTheme
	KSetThemeDark
	KSetThemeLight
	KSetLanguage
	KSetAbout
	KSetConfigPath
	KSetVersion
	KSetFont
	KSetFontMissing

	kCount
)

var zhStrings = [kCount]string{
	KAppTitle:    "tslink",
	KAppSubtitle: "Tailscale 内网穿透",

	KNavOverview: "概览",
	KNavPeers:    "节点",
	KNavDiag:     "网络诊断",
	KNavLogs:     "日志",
	KNavSettings: "设置",

	KStateStarting:   "正在启动",
	KStateConnecting: "正在连接",
	KStateRunning:    "运行中",
	KStateDegraded:   "降级运行",
	KStateStopped:    "已停止",
	KStateError:      "出错",
	KStateRetrying:   "正在重试",

	KStepConfig:      "读取配置",
	KStepFonts:       "加载字体",
	KStepTsnet:       "接入 Tailscale 网络",
	KStepRules:       "解析转发规则",
	KStepDiscovery:   "启动局域网发现",
	KStepMonitors:    "启动状态监控",
	KStepReady:       "准备就绪",
	KSplashHint:      "首次接入 Tailscale 可能需要十几秒",
	KSplashStuckHint: "当前步骤耗时异常，可导出日志以便排查",
	KSplashExportLog: "导出日志",
	KSplashRetry:     "启动失败，正在重试",

	KYes:         "是",
	KNo:          "否",
	KUnknown:     "未知",
	KSupported:   "支持",
	KUnsupported: "不支持",
	KEnabled:     "已启用",
	KDisabled:    "已禁用",
	KNone:        "无",
	KRefresh:     "刷新",
	KRetry:       "重试",
	KClose:       "关闭",
	KCopy:        "复制",
	KCopied:      "已复制",
	KDetails:     "详情",
	KLoading:     "加载中",
	KError:       "错误",
	KNever:       "从未",
	KJustNow:     "刚刚",
	KSecondsAgo:  "秒前",
	KMinutesAgo:  "分钟前",
	KHoursAgo:    "小时前",
	KTotal:       "共",
	KOnline:      "在线",
	KOffline:     "离线",

	KOvTailnet:      "Tailnet",
	KOvSelf:         "本机",
	KOvPeersOnline:  "在线节点",
	KOvForwardRules: "转发规则",
	KOvConnectRules: "连接规则",
	KOvUptime:       "运行时长",
	KOvHealth:       "健康状况",
	KOvQuickDiag:    "运行网络诊断",
	KOvNoIssues:     "未发现问题",

	KPeersTitle:         "Tailscale 节点",
	KPeersLinked:        "已关联",
	KPeersEmpty:         "暂无节点",
	KPeersResolving:     "正在解析配置中的节点",
	KPeerLatency:        "延迟",
	KPeerRoute:          "链路",
	KPeerRouteDirect:    "直连",
	KPeerRouteDERP:      "DERP 中继",
	KPeerRoutePeerRelay: "对等中继",
	KPeerRouteOffline:   "离线",
	KPeerRouteUnknown:   "未知",
	KPeerAvg:            "平均",
	KPeerMin:            "最低",
	KPeerMax:            "最高",
	KPeerJitter:         "抖动",
	KPeerLoss:           "丢包",
	KPeerRx:             "接收",
	KPeerTx:             "发送",
	KPeerLastSeen:       "最后在线",
	KPeerLastHandshake:  "最后握手",
	KPeerAddresses:      "地址",
	KPeerEndpoint:       "端点",
	KPeerOS:             "系统",
	KPeerExitNode:       "出口节点",
	KPeerTags:           "标签",
	KGraphTitle:         "延迟图谱",
	KGraphEmpty:         "正在采集延迟数据",
	KGraphWindow:        "最近",
	KGraphLegendHint:    "点击图例可隐藏对应节点",

	KSvcTitle:     "本机服务",
	KSvcSubtitle:  "tslink 在本机监听并转发到对应服务器",
	KSvcEmpty:     "配置中没有连接规则",
	KSvcBroadcast: "已广播",

	KDiagTitle:               "网络诊断",
	KDiagRun:                 "开始诊断",
	KDiagRunning:             "诊断中",
	KDiagRerun:               "重新诊断",
	KDiagNever:               "尚未运行诊断",
	KDiagLastRun:             "上次运行",
	KDiagCopyReport:          "复制诊断报告",
	KDiagSecIface:            "本机出口地址",
	KDiagSecUDP:              "UDP 连通性",
	KDiagSecNAT:              "NAT 类型",
	KDiagSecPortMap:          "端口映射",
	KDiagSecOverseas:         "境外连通性",
	KDiagSecEgress:           "出口 IP 与归属地",
	KDiagSecTailscale:        "Tailscale 内部状态",
	KDiagNatType:             "NAT 类型",
	KDiagNatMapping:          "映射行为",
	KDiagNatFiltering:        "过滤行为",
	KDiagNatHairpin:          "发夹回环",
	KDiagNatPortPreserve:     "端口保持",
	KDiagUdpV4:               "IPv4 UDP",
	KDiagUdpV6:               "IPv6 UDP",
	KDiagUdpPortsOK:          "可用端口",
	KDiagUdpPortsBlocked:     "被封端口",
	KDiagIfaceDefaultV4:      "默认 IPv4 源地址",
	KDiagIfaceDefaultV6:      "默认 IPv6 源地址",
	KDiagUPnP:                "UPnP IGD",
	KDiagNATPMP:              "NAT-PMP",
	KDiagPCP:                 "PCP",
	KDiagGateway:             "网关",
	KDiagExternalIP:          "外部地址",
	KDiagOverseasTarget:      "测试目标",
	KDiagEgressMethod:        "探测方式",
	KDiagEgressIP:            "出口 IP",
	KDiagEgressGeo:           "归属地",
	KDiagEgressDivergent:     "出口不一致",
	KDiagEgressDivergentHint: "STUN（UDP）本身就看到多个公网 IP，直连打洞会受影响",
	KDiagEgressDivergentHTTP: "仅 HTTP 探测看到不同的公网 IP，STUN（UDP）出口一致，通常不影响打洞",
	KDiagGeoSkipped:          "已跳过归属地查询",
	KDiagPreferredDERP:       "首选 DERP",
	KDiagDerpLatency:         "DERP 延迟",
	KDiagCaptivePortal:       "门户劫持",
	KDiagMappingVaries:       "映射随目标变化",
	KDiagSkipGeo:             "不查询归属地",
	KDiagSkipGeoHint:         "归属地查询会把你的公网 IP 发送给第三方服务",

	KNatOpen:           "开放网络",
	KNatFullCone:       "完全锥形",
	KNatRestricted:     "地址限制锥形",
	KNatPortRestricted: "端口限制锥形",
	KNatSymmetric:      "对称型",
	KNatUDPBlocked:     "UDP 被阻断",
	KNatSymmetricFW:    "对称型防火墙",
	KNatUnknown:        "无法判定",

	KLogsTitle:       "日志",
	KLogsSearch:      "搜索日志…",
	KLogsLevel:       "级别",
	KLogsSource:      "来源",
	KLogsFollow:      "自动跟随",
	KLogsAll:         "全部",
	KLogsEmpty:       "没有匹配的日志",
	KLogsCopyAll:     "复制到剪贴板",
	KLogsSaveFile:    "保存到文件",
	KLogsUpload:      "上传并分享",
	KLogsUploading:   "正在上传",
	KLogsUploaded:    "上传成功，链接已复制",
	KLogsUploadFail:  "上传失败",
	KLogsRedact:      "隐去密钥",
	KLogsRedactHint:  "上传前会自动隐去 authkey 等凭据",
	KLogsShown:       "已显示",
	KLogsDropped:     "条早期日志已被丢弃",
	KLogsIncludeDiag: "附带诊断报告",

	KSetTheme:       "主题",
	KSetThemeDark:   "深色",
	KSetThemeLight:  "浅色",
	KSetLanguage:    "语言",
	KSetAbout:       "关于",
	KSetConfigPath:  "配置文件",
	KSetVersion:     "版本",
	KSetFont:        "中文字体",
	KSetFontMissing: "未找到中文字体，界面已切换为英文",
}

var enStrings = [kCount]string{
	KAppTitle:    "tslink",
	KAppSubtitle: "Tailscale link layer",

	KNavOverview: "Overview",
	KNavPeers:    "Peers",
	KNavDiag:     "Diagnostics",
	KNavLogs:     "Logs",
	KNavSettings: "Settings",

	KStateStarting:   "Starting",
	KStateConnecting: "Connecting",
	KStateRunning:    "Running",
	KStateDegraded:   "Degraded",
	KStateStopped:    "Stopped",
	KStateError:      "Error",
	KStateRetrying:   "Retrying",

	KStepConfig:      "Loading configuration",
	KStepFonts:       "Loading fonts",
	KStepTsnet:       "Joining the tailnet",
	KStepRules:       "Resolving forward rules",
	KStepDiscovery:   "Starting LAN discovery",
	KStepMonitors:    "Starting monitors",
	KStepReady:       "Ready",
	KSplashHint:      "The first tailnet join can take a dozen seconds",
	KSplashStuckHint: "This step is taking unusually long — export the log to investigate",
	KSplashExportLog: "Export log",
	KSplashRetry:     "Startup failed, retrying",

	KYes:         "Yes",
	KNo:          "No",
	KUnknown:     "Unknown",
	KSupported:   "Supported",
	KUnsupported: "Not supported",
	KEnabled:     "Enabled",
	KDisabled:    "Disabled",
	KNone:        "None",
	KRefresh:     "Refresh",
	KRetry:       "Retry",
	KClose:       "Close",
	KCopy:        "Copy",
	KCopied:      "Copied",
	KDetails:     "Details",
	KLoading:     "Loading",
	KError:       "Error",
	KNever:       "Never",
	KJustNow:     "just now",
	KSecondsAgo:  "s ago",
	KMinutesAgo:  "m ago",
	KHoursAgo:    "h ago",
	KTotal:       "Total",
	KOnline:      "Online",
	KOffline:     "Offline",

	KOvTailnet:      "Tailnet",
	KOvSelf:         "This node",
	KOvPeersOnline:  "Peers online",
	KOvForwardRules: "Forward rules",
	KOvConnectRules: "Connect rules",
	KOvUptime:       "Uptime",
	KOvHealth:       "Health",
	KOvQuickDiag:    "Run diagnostics",
	KOvNoIssues:     "No issues found",

	KPeersTitle:         "Tailscale peers",
	KPeersLinked:        "Linked",
	KPeersEmpty:         "No peers yet",
	KPeersResolving:     "Resolving the peers named in the config",
	KPeerLatency:        "Latency",
	KPeerRoute:          "Route",
	KPeerRouteDirect:    "Direct",
	KPeerRouteDERP:      "DERP relay",
	KPeerRoutePeerRelay: "Peer relay",
	KPeerRouteOffline:   "Offline",
	KPeerRouteUnknown:   "Unknown",
	KPeerAvg:            "avg",
	KPeerMin:            "min",
	KPeerMax:            "max",
	KPeerJitter:         "jitter",
	KPeerLoss:           "loss",
	KPeerRx:             "Rx",
	KPeerTx:             "Tx",
	KPeerLastSeen:       "Last seen",
	KPeerLastHandshake:  "Last handshake",
	KPeerAddresses:      "Addresses",
	KPeerEndpoint:       "Endpoint",
	KPeerOS:             "OS",
	KPeerExitNode:       "Exit node",
	KPeerTags:           "Tags",
	KGraphTitle:         "Latency graph",
	KGraphEmpty:         "Collecting latency samples",
	KGraphWindow:        "last",
	KGraphLegendHint:    "Click a legend entry to hide that peer",

	KSvcTitle:     "Local services",
	KSvcSubtitle:  "Listening on this machine, forwarded to each server",
	KSvcEmpty:     "No connect rules configured",
	KSvcBroadcast: "Broadcast",

	KDiagTitle:               "Network diagnostics",
	KDiagRun:                 "Run diagnostics",
	KDiagRunning:             "Running",
	KDiagRerun:               "Run again",
	KDiagNever:               "Not run yet",
	KDiagLastRun:             "Last run",
	KDiagCopyReport:          "Copy report",
	KDiagSecIface:            "Local egress addresses",
	KDiagSecUDP:              "UDP connectivity",
	KDiagSecNAT:              "NAT type",
	KDiagSecPortMap:          "Port mapping",
	KDiagSecOverseas:         "Overseas reachability",
	KDiagSecEgress:           "Egress IP and geolocation",
	KDiagSecTailscale:        "Tailscale internals",
	KDiagNatType:             "NAT type",
	KDiagNatMapping:          "Mapping behaviour",
	KDiagNatFiltering:        "Filtering behaviour",
	KDiagNatHairpin:          "Hairpinning",
	KDiagNatPortPreserve:     "Port preserving",
	KDiagUdpV4:               "IPv4 UDP",
	KDiagUdpV6:               "IPv6 UDP",
	KDiagUdpPortsOK:          "Reachable ports",
	KDiagUdpPortsBlocked:     "Blocked ports",
	KDiagIfaceDefaultV4:      "Default IPv4 source",
	KDiagIfaceDefaultV6:      "Default IPv6 source",
	KDiagUPnP:                "UPnP IGD",
	KDiagNATPMP:              "NAT-PMP",
	KDiagPCP:                 "PCP",
	KDiagGateway:             "Gateway",
	KDiagExternalIP:          "External address",
	KDiagOverseasTarget:      "Target",
	KDiagEgressMethod:        "Method",
	KDiagEgressIP:            "Egress IP",
	KDiagEgressGeo:           "Location",
	KDiagEgressDivergent:     "Egress mismatch",
	KDiagEgressDivergentHint: "STUN (UDP) itself saw more than one public IP, so direct connections will suffer",
	KDiagEgressDivergentHTTP: "Only the HTTP probes disagreed; the STUN (UDP) egress is consistent, so hole punching is usually unaffected",
	KDiagGeoSkipped:          "Geolocation skipped",
	KDiagPreferredDERP:       "Preferred DERP",
	KDiagDerpLatency:         "DERP latency",
	KDiagCaptivePortal:       "Captive portal",
	KDiagMappingVaries:       "Mapping varies by destination",
	KDiagSkipGeo:             "Skip geolocation",
	KDiagSkipGeoHint:         "Geolocation sends your public IP to a third-party service",

	KNatOpen:           "Open internet",
	KNatFullCone:       "Full cone",
	KNatRestricted:     "Address-restricted cone",
	KNatPortRestricted: "Port-restricted cone",
	KNatSymmetric:      "Symmetric",
	KNatUDPBlocked:     "UDP blocked",
	KNatSymmetricFW:    "Symmetric firewall",
	KNatUnknown:        "Undetermined",

	KLogsTitle:       "Logs",
	KLogsSearch:      "Search logs…",
	KLogsLevel:       "Level",
	KLogsSource:      "Source",
	KLogsFollow:      "Follow",
	KLogsAll:         "All",
	KLogsEmpty:       "No matching log entries",
	KLogsCopyAll:     "Copy to clipboard",
	KLogsSaveFile:    "Save to file",
	KLogsUpload:      "Upload and share",
	KLogsUploading:   "Uploading",
	KLogsUploaded:    "Uploaded, link copied",
	KLogsUploadFail:  "Upload failed",
	KLogsRedact:      "Redact secrets",
	KLogsRedactHint:  "Credentials such as authkeys are removed before upload",
	KLogsShown:       "shown",
	KLogsDropped:     "earlier entries were dropped",
	KLogsIncludeDiag: "Include diagnostics",

	KSetTheme:       "Theme",
	KSetThemeDark:   "Dark",
	KSetThemeLight:  "Light",
	KSetLanguage:    "Language",
	KSetAbout:       "About",
	KSetConfigPath:  "Config file",
	KSetVersion:     "Version",
	KSetFont:        "CJK font",
	KSetFontMissing: "No CJK font found, the UI fell back to English",
}

// Tr returns the localised string for k, falling back to English and then to a
// visible placeholder rather than an empty label.
func Tr(l Lang, k Key) string {
	if k < 0 || k >= kCount {
		return "?"
	}
	if l == LangZH {
		if s := zhStrings[k]; s != "" {
			return s
		}
	}
	if s := enStrings[k]; s != "" {
		return s
	}
	return "?"
}
