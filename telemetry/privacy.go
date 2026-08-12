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
//	panic.go      → 遥测段的崩溃那行（Guard 上报的 Exception 与栈）
//	clientid/     → 更新段（版本检查请求头，见 clientid.HeaderNames）
//
// ✅ 有门禁：privacynotice_test.go 逐项核对遥测段；gatewaynotice_test.go 核对更新段。
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

	noticeFooter = `===========================================================`
)

// privacyNotice 是**全部**段落拼起来的完整文案，供门禁核对采集面。
//
// 它不一定等于主播看到的那一份 —— 实际打印的内容由 PrintPrivacyNotice 按开关拼装。
// 门禁要核的是「所有可能发出去的东西都在文案里有交代」，所以这里必须是全集。
const privacyNotice = noticeHeader + noticeTelemetrySection + noticeDivider + noticeUpdateSection + noticeFooter

// PrintPrivacyNotice 打印隐私提示，然后倒计时 wait 后自动继续。
//
// versionCheckActive 传 main.isReleaseVersion(Version) —— 即「这次启动会不会真的去查版本」。
// 它与遥测的开关相互独立，见文件头「为什么分成两段」。
//
// **两段都不适用时立即返回**：不打印、不等待。那是本地 go build 的常态。
//
// 不做「按任意键跳过」：那要读 stdin，而 stdin 在服务化/重定向场景下会立刻 EOF 或永久阻塞，
// 两种都比单纯等 10 秒糟。主播看不完可以回头翻日志——这段也在 stdout 里。
func PrintPrivacyNotice(wait time.Duration, versionCheckActive bool) {
	text := noticeFor(Enabled(), versionCheckActive)
	if text == "" {
		return
	}

	fmt.Println(text)
	countdown(wait)
}

// noticeFor 按两个开关拼出**这次真正要打印的**文案；两段都不适用时返回空串。
//
// 拆出来是为了可测：拼装逻辑（哪段该出现、哪段不该出现）才是这里唯一会出错的地方，
// 而它一旦藏在 PrintPrivacyNotice 里就只能靠抓 stdout 来验，那种测试脆且读不出意图。
func noticeFor(telemetryOn, versionCheckOn bool) string {
	sections := make([]string, 0, 2)
	if telemetryOn {
		sections = append(sections, noticeTelemetrySection)
	}
	if versionCheckOn {
		sections = append(sections, noticeUpdateSection)
	}
	if len(sections) == 0 {
		return ""
	}
	return noticeHeader + strings.Join(sections, noticeDivider) + noticeFooter
}

// countdown 原地倒计时，结束后把那行擦掉，免得它留在日志里碍眼。
//
// 用 `\r` 覆写 —— 与更新进度条同属 AGENTS.md §6.2 允许 fmt 的例外。
func countdown(wait time.Duration) {
	for i := int(wait.Seconds()); i > 0; i-- {
		fmt.Printf("\r   %2d 秒后自动继续...", i)
		time.Sleep(time.Second)
	}
	// 用空格盖掉整行再回到行首：直接打 \n 会在日志里留下一行倒计时残迹。
	fmt.Printf("\r%s\r", strings.Repeat(" ", 32))
}
