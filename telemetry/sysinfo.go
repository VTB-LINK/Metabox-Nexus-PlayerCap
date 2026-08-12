package telemetry

// 系统信息采集。**本文件引入 x/sys/windows，使 telemetry 包只能在 Windows 构建。**
// 这符合现状（本项目是纯 Windows，见 AGENTS.md §3.7），且不违反 §0.3 —— 那条只要求
// config/ 与 logger/ 保持 Linux 可构建，而依赖方向是单向的：telemetry → logger，反过来没有。
//
// # 为什么这些值全得自己采
//
// sentry-go 的默认 Environment 集成给的 contexts["os"].name 是 **runtime.GOOS**，
// 也就是字符串 "windows"，没有版本、没有 build、没有 edition。前端项目里浏览器白送的那些
// 丰富度，在 Go 这边等于零。好消息是默认集成**只在 key 不存在时才填**，我们预写的不会被覆盖。
//
// 下面每个 API 的选择都有实测理由，别按直觉换：
//
//	想当然                          实测（2026-07-17，本机 Win10 Enterprise 19045）
//	GetVersionEx 能读版本            **必错** —— 返回 6.2 build 9200（Win8），因为 exe 没嵌清单
//	time.Now().Zone() 够用           返回 "+08"；time.Local 是 "Local" —— Go 在 Windows 上给不出时区名
//	StandardName 能当时区 key        本机是「马来西亚半岛标准时间」——**本地化的**，同一台机器换个
//	                                 系统语言就变。必须用 TimeZoneKeyName（"Singapore Standard Time"）

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// LazyDLL 的 proc 必须是包级 var（AGENTS.md §0.4 的同源理由：别在热路径上反复查找符号）。
// x/sys v0.45 没有封装下面这四个，只能手搓；RtlGetVersion / RtlGetNtVersionNumbers 它有。
var (
	kernel32                          = windows.NewLazySystemDLL("kernel32.dll")
	procGetProductInfo                = kernel32.NewProc("GetProductInfo")
	procGetUserDefaultLocaleName      = kernel32.NewProc("GetUserDefaultLocaleName")
	procGetDynamicTimeZoneInformation = kernel32.NewProc("GetDynamicTimeZoneInformation")
	procGetNativeSystemInfo           = kernel32.NewProc("GetNativeSystemInfo")
)

// 这两个做成变量是为了可注入 —— shim 检测的整个意义在于「两者不一致」，而开发机上它们
// 永远一致（没人会给自己的开发环境开兼容模式）。不可注入的话，那段判据就只能靠「本机恰好
// 没开兼容模式」来验证，等于没测。同样的手法见 config/config.go 的 osExecutable。
var (
	rtlGetVersion          = windows.RtlGetVersion
	rtlGetNtVersionNumbers = windows.RtlGetNtVersionNumbers
)

// dynamicTimeZoneInformation 对应 DYNAMIC_TIME_ZONE_INFORMATION。
// 只有它带 TimeZoneKeyName —— 非本地化的时区标识，见文件头。
type dynamicTimeZoneInformation struct {
	Bias                        int32
	StandardName                [32]uint16
	StandardDate                windows.Systemtime
	StandardBias                int32
	DaylightName                [32]uint16
	DaylightDate                windows.Systemtime
	DaylightBias                int32
	TimeZoneKeyName             [128]uint16
	DynamicDaylightTimeDisabled byte
}

// systemInfo 对应 SYSTEM_INFO。用 GetNativeSystemInfo 而不是 GetSystemInfo：
// 后者在 WOW64 下会谎报成 x86。
type systemInfo struct {
	ProcessorArchitecture     uint16
	Reserved                  uint16
	PageSize                  uint32
	MinimumApplicationAddress uintptr
	MaximumApplicationAddress uintptr
	ActiveProcessorMask       uintptr
	NumberOfProcessors        uint32
	ProcessorType             uint32
	AllocationGranularity     uint32
	ProcessorLevel            uint16
	ProcessorRevision         uint16
}

// osInfo 是一次系统信息快照。
type osInfo struct {
	// Major/Minor/Build 来自 RtlGetNtVersionNumbers —— **真实版本，免疫兼容性 shim**。
	Major, Minor, Build uint32

	// ShimMajor/ShimMinor/ShimBuild 来自 RtlGetVersion —— 兼容性 shim 让它看到的版本。
	ShimMajor, ShimMinor, ShimBuild uint32

	// Shimmed 表示两者不一致，即**这台机器上有兼容性 shim 在生效**。见 shimNote。
	Shimmed bool

	Edition  string // "Enterprise" / "Professional" / ...
	Locale   string // "zh-CN"
	TimeZone string // "Singapore Standard Time"（非本地化）
	Arch     string // "x64"
	CPUCores uint32
}

// collectOSInfo 采一次系统信息。任何一项失败都只是留空，不影响其余项，更不影响主程序。
func collectOSInfo() osInfo {
	var o osInfo

	// 版本号读两遍，这是刻意的 —— 见 osInfo.Shimmed。
	o.Major, o.Minor, o.Build = rtlGetNtVersionNumbers()
	if v := rtlGetVersion(); v != nil {
		o.ShimMajor, o.ShimMinor, o.ShimBuild = v.MajorVersion, v.MinorVersion, v.BuildNumber
		o.Shimmed = o.ShimMajor != o.Major || o.ShimMinor != o.Minor || o.ShimBuild != o.Build

		// edition 要拿版本号当入参，用 shim 看到的那个：GetProductInfo 本身也活在
		// 兼容层里，喂它真实版本反而可能对不上。
		var pt uint32
		if r, _, _ := procGetProductInfo.Call(
			uintptr(v.MajorVersion), uintptr(v.MinorVersion), 0, 0,
			uintptr(unsafe.Pointer(&pt)),
		); r != 0 {
			o.Edition = editionName(pt)
		}
	}

	buf := make([]uint16, 85) // LOCALE_NAME_MAX_LENGTH
	if r, _, _ := procGetUserDefaultLocaleName.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
	); r != 0 {
		o.Locale = windows.UTF16ToString(buf)
	}

	var tz dynamicTimeZoneInformation
	// 返回 TIME_ZONE_ID_INVALID(0xFFFFFFFF) 才是失败；0/1/2 分别是 unknown/standard/daylight，
	// 都算成功。别写成 `r != 0`——那会把「当前处于标准时间」当成失败丢掉时区。
	if r, _, _ := procGetDynamicTimeZoneInformation.Call(uintptr(unsafe.Pointer(&tz))); r != 0xFFFFFFFF {
		o.TimeZone = windows.UTF16ToString(tz.TimeZoneKeyName[:])
	}

	var si systemInfo
	procGetNativeSystemInfo.Call(uintptr(unsafe.Pointer(&si))) // 无返回值，不会失败
	o.Arch = archName(si.ProcessorArchitecture)
	o.CPUCores = si.NumberOfProcessors

	return o
}

// OSSummary 返回出网请求要标注的三项系统信息：真实版本号、edition、CPU 架构。
//
// # 为什么它在这儿，而不在调用方自己采
//
// 唯一的调用方是版本检查请求（main.go 的 clientEnvFor）。让它自己去调 RtlGetNtVersionNumbers
// 就是把 sysinfo.go 里那一坨实测结论复制第二份——文件头写得很清楚，这些 API 每一个都有
// 「按直觉换就错」的坑（GetVersionEx 恒返回 Win8、StandardName 是本地化的）。复制出去的那份
// 迟早跟这里分叉，而分叉的表现是报表里悄悄多出一种 OS 版本。
//
// # 与 Enabled() 无关
//
// **本函数不看 DSN。** 版本检查是发布版一定会做的事，与遥测启不启用是两条独立的线；
// 拿 Enabled() 门控它会让未注入 DSN 的发布版报上去的系统信息全空。
//
// # 为什么不缓存
//
// 这几个值在进程内不变，缓存看似白赚。但 rtlGetVersion / rtlGetNtVersionNumbers 是可注入的
// 包级变量（见文件头「做成变量是为了可注入」），一缓存就会让先跑到的用例把结果钉死给后面
// 所有用例——sysinfo 的 shim 检测测试正是靠注入这两个变量做的。一次调用六个 syscall，
// 而它一个进程里只被调一次，省这个没意义。
func OSSummary() (version, edition, arch string) {
	o := collectOSInfo()
	return o.version(), o.Edition, o.Arch
}

// version 返回 "10.0.19045" 形态的真实版本号。
func (o osInfo) version() string {
	return fmt.Sprintf("%d.%d.%d", o.Major, o.Minor, o.Build)
}

// shimVersion 返回兼容层让 RtlGetVersion 看到的版本号。
func (o osInfo) shimVersion() string {
	return fmt.Sprintf("%d.%d.%d", o.ShimMajor, o.ShimMinor, o.ShimBuild)
}

// editionName 把 PRODUCT_* 码翻成人话。认不出的原样带上 hex —— **别返回 "Unknown" 就完事**，
// 那会把唯一能查的线索丢掉。Windows 的 PRODUCT_* 有一百多个，这里只列主播可能用到的。
func editionName(p uint32) string {
	switch p {
	case 0x65:
		return "Home"
	case 0x62:
		return "Home N"
	case 0x63:
		return "Home China"
	case 0x64:
		return "Home Single Language"
	case 0x30:
		return "Professional"
	case 0x31:
		return "Professional N"
	case 0x67:
		return "Professional Workstation"
	case 0x04:
		return "Enterprise"
	case 0x1B:
		return "Enterprise N"
	case 0x7D:
		return "Enterprise LTSB"
	case 0xB2:
		return "Enterprise LTSC"
	case 0x48:
		return "Education"
	case 0x3F:
		return "Education N"
	case 0x01:
		return "Ultimate"
	case 0x06:
		return "Business"
	case 0x07:
		return "Server Standard"
	case 0x08:
		return "Server Datacenter"
	default:
		return fmt.Sprintf("0x%x", p)
	}
}

// archName 翻 PROCESSOR_ARCHITECTURE_*。同样：认不出就带上原值。
func archName(a uint16) string {
	switch a {
	case 9:
		return "x64"
	case 0:
		return "x86"
	case 12:
		return "arm64"
	case 5:
		return "arm"
	default:
		return fmt.Sprintf("unknown(%d)", a)
	}
}
