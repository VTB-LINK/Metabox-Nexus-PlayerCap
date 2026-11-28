package telemetry

// 启动时的隐私提示。
//
// # 它必须与代码实际采集的东西一致
//
// 这段文字不是法务模板，是**对本程序行为的描述**。改采集范围就得改它 —— 说少了是不诚实，
// 说多了会让主播以为我们拿了没拿的东西。逐条对应关系：
//
//	sysscope.go   → 遥测段的系统那行（os/device context + locale/timezone/arch tag）
//	appinfo.go    → 遥测段的程序那行（playercap context：version/exe_path/config/overwritten/sources）
//	userip.go     → 遥测段的网络那行（user.ip_address = {{auto}}，由 Sentry 服务端填）
//	report.go     → 遥测段的播放器那行（ReportOnce 的 key 与 extra）
//	audiocap/manager/telemetry.go
//	              → 遥测段的音频采集那行（三个事件的 tags 与 extra：退出码、运行时长、失败次数、stderr 末几行）
//	appinfo.go 的 applyProcess
//	              → 同上那行（音频采集 helper 的事件带 process 标签）
//	panic.go      → 遥测段的崩溃那行（Guard 上报的 Exception 与栈）
//	clientid/     → 更新段（版本检查请求头，见 clientid.HeaderNames）
//
// ✅ 有门禁：privacynotice_test.go 逐项核对遥测段；audionotice_test.go 核对音频采集的事件（取
// manager.SampleTelemetryEvents，与真实上报共用构造函数）；gatewaynotice_test.go 核对更新段。
//
// # 为什么分成两段、各自开关
//
// 两件事的开关根本不是同一个：
//
//	遥测   由 DSN 有没有注入决定（Enabled）
//	版本检查 由版本号是不是发布版决定（main.isReleaseVersion）
//
// 早先整块文案只由 Enabled 门控。加入版本检查上报之后，那种做法两头都会撒谎：
// 未注入 DSN 的发布版会**不打招呼就把设备标识发出去**；而如果反过来无条件打印整块，
// 本地构建又会看到一段「我们会上报崩溃栈到 sentry.io」——它一个字节都不会发。
// 所以按段拼装：哪一段真的会发生，才印哪一段。
//
// 两段都不发生时（本地 go build 的常态）一个字都不打、也不等待。

import (
	"fmt"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

// 提示正文分段。
//
// 措辞三条原则：**只陈述事实、不辩解、不吓人**。主播不是法务，他要的是「你拿了什么、
// 干什么用」，两句话说清就够；把它写成隐私政策反而没人读。
const (
	noticeHeader = `===========================================================
   隐私提示
===========================================================
`

	noticeTelemetrySection = `   本程序在崩溃、或检测到预期之外的状态时会上报遥测诊断数据。

   上报内容：
     · 系统：Windows 版本与 edition、语言、时区、CPU 架构与核数、设备名
     · 程序：版本号、安装路径（含 Windows 用户名）、config.yml 的全部配置项
     · 播放器：版本号与异常类型（版本不受支持、歌词解密失败等）
     · 音频采集：采集进程异常退出或放弃时的退出码、运行时长、失败次数与诊断日志末几行（含播放器名、PID、系统错误码）
     · 网络：本机公网 IP（由遥测服务端记录）
     · 崩溃时的调用栈

   不上报：歌词内容、歌曲信息、账号密码。

   用途：定位直播事故、了解播放器版本分布。除此之外不作他用。
   接收方：遥测服务端（sentry.io），数据保留 30 天。
`

	// 两段之间的分隔线。前后各留一个空行 —— 两段紧贴时读起来像一段，
	// 而它们是两件不同的事、发给两个不同的接收方。
	noticeDivider = `
   ---------------------------------------------------------

`

	// 更新段。每一行都对应 clientid.HeaderNames() 里的一个请求头，
	// 由 gatewaynotice_test.go 逐项核对——新增请求头而不改这段，那条门禁会先一步红。
	noticeUpdateSection = `   本程序启动时、以及运行期间每天一次，会向更新服务器查询最新版本。

   这个请求会带上：
     · 匿名设备标识：由本机标识单向哈希而来，不可还原，也不与其他用途关联
     · 程序版本号、Windows 版本与 edition、CPU 架构
     · 本次请求是「启动」还是「每日在线」

   用途：统计有多少台设备运行软件、哪些版本正在使用中。除此之外不作他用。
   接收方：更新服务器（gateway.vtb.link）。
`

	// RUM 匿名设备活跃与产品使用统计段（正式发布版且构建期注入配置时启用）。
	noticeRUMSection = `   正式发布版还会上报匿名的设备活跃与产品使用统计（可经远端策略按需关闭或降采样）。

   上报内容：
     · 匿名设备标识：复用基于注册表 MachineGuid 单向哈希生成的匿名标识，不新增本地状态文件
     · 生命周期与更新配置：启动、跨日心跳、退出，自动更新与配置加载、迁移、重建状态
     · 功能与运行指标：播放器健康、阻断、干预与兼容信号，歌词与音画功能运行指标，脱敏后的客户端来源分类
     · 基础系统画像：Windows 版本与 edition、语言、时区、CPU 架构与核数、是否以管理员权限运行
     · 网络：本机公网 IP（随网络请求到达网关与统计服务端）

   不上报：歌名、歌手、专辑名、歌词正文、完整本地路径、Windows 用户名、封面完整 URL、前端查询参数与未知配置文本。

   用途：统计设备活跃与功能使用情况，及时定位版本兼容与更新问题。除此之外不作他用。
   接收方：经网关转发至统计服务端（posthog.com）。
`

	noticeFooter = `===========================================================`
)

// 英文对照段落：面向非中文分发时由 noticeFor 选用。**它们是同一份采集契约的两种语言**，
// 内容必须与上面的中文段落逐条一致 —— privacynotice_en_test.go 核对英文覆盖面，
// 与中文侧的 privacynotice_test.go / gatewaynotice_test.go 同构。
// 分隔线（noticeDivider）与页脚（noticeFooter）是语言中立的，两语共用。
const (
	noticeHeaderEN = `===========================================================
   Privacy Notice
===========================================================
`

	noticeTelemetrySectionEN = `   This program reports telemetry diagnostics on a crash, or when it detects an unexpected state.

   Reported:
     · System: Windows version and edition, language, time zone, CPU architecture and core count, device name
     · Program: version, install path (includes the Windows user name), all config.yml settings
     · Player: version and anomaly type (unsupported version, lyric decryption failure, etc.)
     · Audio capture: on an abnormal exit or give-up of the capture process, its exit code, run time, failure count and the last lines of its diagnostic log (including player names, PIDs and system error codes)
     · Network: this machine's public IP (recorded by the telemetry server)
     · Call stack at the moment of a crash

   Not reported: lyric content, song information, account credentials.

   Purpose: diagnosing live-stream incidents and understanding player-version distribution. Nothing else.
   Recipient: telemetry server (sentry.io), data retained for 30 days.
`

	noticeUpdateSectionEN = `   On startup, and once a day while running, this program queries the update server for the latest version.

   This request carries:
     · An anonymous device ID: a one-way hash of a machine identifier, not reversible and not linked to any other use
     · Program version, Windows version and edition, CPU architecture
     · Whether this request is a "startup" or a "daily online" check

   Purpose: counting how many devices run the software and which versions are in use. Nothing else.
   Recipient: update server (gateway.vtb.link).
`

	noticeRUMSectionEN = `   Release builds also report anonymous device-activity and product-usage statistics (which can be disabled or downsampled remotely on demand).

   Reported:
     · Anonymous device ID: reuses the one-way hash derived from the registry MachineGuid, with no additional local state file
     · Lifecycle, updates and config: startup, daily heartbeat, exit, auto-update status, and config load/migration/rebuild states
     · Feature and runtime metrics: player health, blocking, intervention and compatibility signals, lyric and audio/effect metrics, and sanitized client surface categories
     · Basic system profile: Windows version and edition, language, time zone, CPU architecture and core count, and whether running as administrator
     · Network: this machine's public IP (received with the network request by the gateway and analytics server)

   Not reported: song name, artist, album, lyric text, full local paths, Windows user name, full cover URLs, frontend query parameters, or unknown config text.

   Purpose: measuring active devices and feature usage, and spotting compatibility or update issues early. Nothing else.
   Recipient: forwarded via the gateway to the analytics server (posthog.com).
`
)

// privacyNotice 是**全部**段落拼起来的完整文案，供门禁核对采集面。
//
// 它不一定等于主播看到的那一份 —— 实际打印的内容由 PrintPrivacyNotice 按开关拼装。
// 门禁要核的是「所有可能发出去的东西都在文案里有交代」，所以这里必须是全集。
const privacyNotice = noticeHeader + noticeTelemetrySection + noticeDivider + noticeUpdateSection + noticeDivider + noticeRUMSection + noticeFooter

// privacyNoticeEN 是英文全集，供英文门禁核对采集面（同 privacyNotice 之于中文）。
const privacyNoticeEN = noticeHeaderEN + noticeTelemetrySectionEN + noticeDivider + noticeUpdateSectionEN + noticeDivider + noticeRUMSectionEN + noticeFooter

// bannerLogo 是启动 Banner 顶部 VTB-LIVE ASCII logo 的副本（与 main.go 各存一份）。
// 隐私提示停留最久、用户看得最清，在其顶部再打印一份以提高 logo 曝光。有意复制而非跨包引用：
// telemetry 不反向依赖 main，且 logo 极少变动，两处各自维护的代价可接受。
const bannerLogo = `___    __________________         ______ ___________    ____________
__ |  / /___  __/___  __ )        ___  / ____  _/__ |  / /___  ____/
__ | / / __  /   __  __  |__________  /   __  /  __ | / / __  __/
__ |/ /  _  /    _  /_/ / _/_____/_  /_____/ /   __ |/ /  _  /___
_____/   /_/     /_____/          /_____//___/   _____/   /_____/   `

// PrintPrivacyNotice 打印隐私提示，然后倒计时 wait 后自动继续。
//
// versionCheckActive 传 main.isReleaseVersion(Version) —— 即「这次启动会不会真的去查版本」。
// 它与遥测的开关相互独立，见文件头「为什么分成两段」。
//
// **两段都不适用时立即返回**：不打印、不等待。那是本地 go build 的常态。
//
// 不做「按任意键跳过」：那要读 stdin，而 stdin 在服务化/重定向场景下会立刻 EOF 或永久阻塞，
// 两种都比单纯等 10 秒糟。主播看不完可以回头翻日志——这段也在 stdout 里。
func PrintPrivacyNotice(wait time.Duration, versionCheckActive, rumActive bool) {
	text := noticeFor(Enabled(), versionCheckActive, rumActive)
	if text == "" {
		return
	}

	// logo 与倒计时同属 AGENTS.md §6.2 允许 fmt 的例外（都是用户可见的启动输出）。
	// 只在提示确实要显示时（text 非空）才打印，保持本地 dev 构建一个字都不打的现状。
	fmt.Println(bannerLogo)
	fmt.Println()
	fmt.Println(text)
	Countdown(wait)
}

// noticeFor 按三个开关拼出**这次真正要打印的**文案；三段都不适用时返回空串。
//
// 拆出来是为了可测：拼装逻辑（哪段该出现、哪段不该出现）才是这里唯一会出错的地方，
// 而它一旦藏在 PrintPrivacyNotice 里就只能靠抓 stdout 来验，那种测试脆且读不出意图。
func noticeFor(telemetryOn, versionCheckOn, rumOn bool) string {
	header, telemetrySec, updateSec, rumSec := noticeHeader, noticeTelemetrySection, noticeUpdateSection, noticeRUMSection
	if i18n.Language() == i18n.English {
		header, telemetrySec, updateSec, rumSec = noticeHeaderEN, noticeTelemetrySectionEN, noticeUpdateSectionEN, noticeRUMSectionEN
	}
	sections := make([]string, 0, 3)
	if telemetryOn {
		sections = append(sections, telemetrySec)
	}
	if versionCheckOn {
		sections = append(sections, updateSec)
	}
	if rumOn {
		sections = append(sections, rumSec)
	}
	if len(sections) == 0 {
		return ""
	}
	return header + strings.Join(sections, noticeDivider) + noticeFooter
}

// Countdown 原地倒计时，结束后把那行擦掉，免得它留在日志里碍眼。
//
// 用 `\r` 覆写 —— 与更新进度条同属 AGENTS.md §6.2 允许 fmt 的例外。
//
// 导出是因为它另被 config.yml 的重建提示使用（根包 configrebuild.go 的 showRebuildNotice）：
// 两处给用户看的都是同一行逐秒倒计时，不另写一份。
func Countdown(wait time.Duration) {
	for i := int(wait.Seconds()); i > 0; i-- {
		fmt.Printf(i18n.T("\r   continuing in %2d s..."), i)
		time.Sleep(time.Second)
	}
	// 用空格盖掉整行再回到行首：直接打 \n 会在日志里留下一行倒计时残迹。
	fmt.Printf("\r%s\r", strings.Repeat(" ", 32))
}
