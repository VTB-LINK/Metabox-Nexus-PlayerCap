package clientid

// 出网请求头的组装。见包注释。

import (
	"fmt"
	"net/http"
)

// productName 是 UA 里的产品名，与出货二进制同名（AGENTS.md §13.1）。
const productName = "Metabox-Nexus-PlayerCap"

// 两种上报时机。**分开是为了让网关一份日志能同时回答两个问题**：
//
//	今天有多少台机器在线   → 不分时机，按 HeaderID 去重
//	今天启动了多少次       → 只数 PingStart，且不去重
//
// 两个数相除就是人均启动次数——它反常地高通常不是「主播勤快」，而是崩溃重启或
// 播放器接不上在反复重试，是个能提前发现故障的信号。
const (
	PingStart = "start"
	PingDaily = "daily"
)

// 请求头名字。
//
// **改名会让网关侧的报表断档。** 旧版客户端仍在发旧名，而它们要等自动更新一轮一轮
// 淘汰干净才会消失。真要改，网关侧必须同时接受新旧两个名字，直到旧版本清零。
const (
	HeaderID      = "X-Client-Id"
	HeaderVersion = "X-Client-Version"
	HeaderOS      = "X-Client-OS"
	HeaderEdition = "X-Client-Edition"
	HeaderArch    = "X-Client-Arch"
	HeaderPing    = "X-Client-Ping"
	HeaderUA      = "User-Agent"
)

// Env 是随请求报上去的客户端环境。**留空的字段不会发出对应的头**，不会发空值。
//
// 空值头在网关侧比缺头更难查：前者看着像「采到了，是空的」，后者才是「没采到」。
// 同 telemetry/sysscope.go 的 setTagIfNotEmpty，理由一样。
type Env struct {
	Version   string // main.Version 原值，如 "3.0.0-rc.14"
	OSVersion string // "10.0.19045"，真实版本（免疫兼容性 shim）
	OSEdition string // "Enterprise"
	Arch      string // "x64"
	Ping      string // PingStart 或 PingDaily
}

// HeaderNames 是本包会写进请求的**全部**请求头。
//
// 它是给门禁用的真源：telemetry/gatewaynotice_test.go 拿它逐项核对隐私提示，
// 新增一个头而不改提示，那条测试会先一步红。别把这个列表写成手抄的副本。
func HeaderNames() []string {
	return []string{
		HeaderID,
		HeaderVersion,
		HeaderOS,
		HeaderEdition,
		HeaderArch,
		HeaderPing,
		HeaderUA,
	}
}

// Apply 给出网请求打上客户端标识与环境。
//
// **不返回 error，任何一项缺失都只是少一个头。** 这些头是给我们看报表用的，而它挂在
// 版本检查 / 自动更新这条链路上——为了一个统计字段让更新失败是本末倒置。
func Apply(req *http.Request, env Env) {
	set := func(name, value string) {
		if value != "" {
			req.Header.Set(name, value)
		}
	}

	set(HeaderID, ID())
	set(HeaderVersion, env.Version)
	set(HeaderOS, env.OSVersion)
	set(HeaderEdition, env.OSEdition)
	set(HeaderArch, env.Arch)
	set(HeaderPing, env.Ping)
	set(HeaderUA, userAgent(env))
}

// userAgent 拼一个人读得懂的 UA，取代 Go 默认的 "Go-http-client/1.1"。
//
// 形如：Metabox-Nexus-PlayerCap/3.0.0-rc.14 (Windows NT 10.0.19045; x64)
//
// 版本号取不到就整个不发——一个只有产品名、没有版本的 UA 在日志里认得出是我们，
// 却回答不了任何问题，不如让它保持 Go 默认值，一眼能看出这是条异常请求。
func userAgent(env Env) string {
	if env.Version == "" {
		return ""
	}

	platform := "Windows"
	if env.OSVersion != "" {
		platform = "Windows NT " + env.OSVersion
	}
	if env.Arch != "" {
		platform += "; " + env.Arch
	}

	return fmt.Sprintf("%s/%s (%s)", productName, env.Version, platform)
}
