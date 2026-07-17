package telemetry

// 启动时的隐私提示。
//
// # 它必须与代码实际采集的东西一致
//
// 这段文字不是法务模板，是**对本包行为的描述**。改采集范围就得改它 —— 说少了是不诚实，
// 说多了会让主播以为我们拿了没拿的东西。逐条对应关系：
//
//	sysscope.go   → 系统那行（os/device context + locale/timezone/arch tag）
//	appinfo.go    → 程序那行（playercap context：version/exe_path/config/overwritten/sources）
//	userip.go     → 网络那行（user.ip_address = {{auto}}，由 Sentry 服务端填）
//	report.go     → 播放器那行（ReportOnce 的 key 与 extra）
//	panic.go      → 崩溃那行（Guard 上报的 Exception 与栈）
//
// ✅ 有门禁：privacynotice_test.go 逐项核对文案与实际采集面。
//
// # 为什么只在遥测启用时打
//
// 未注入 DSN 时一个字节都不会外发，这时候摆一段「我们会上报…」的告示是在撒谎，还白等 10 秒。
// 本地 go build 与任何未配 secret 的构建都走这条路。

import (
	"fmt"
	"strings"
	"time"
)

// privacyNotice 是提示正文。
//
// 措辞三条原则：**只陈述事实、不辩解、不吓人**。主播不是法务，他要的是「你拿了什么、干什么用」，
// 两句话说清就够；把它写成隐私政策反而没人读。
const privacyNotice = `===========================================================
   隐私提示
===========================================================
   本程序在崩溃、或检测到预期之外的状态时会上报遥测诊断数据。

   上报内容：
     · 系统：Windows 版本与 edition、语言、时区、CPU 架构与核数、设备名
     · 程序：版本号、安装路径（含 Windows 用户名）、config.yml 的全部配置项
     · 播放器：版本号与异常类型（版本不受支持、歌词解密失败等）
     · 网络：本机公网 IP（由遥测服务端记录）
     · 崩溃时的调用栈

   不上报：歌词内容、歌曲信息、账号密码。

   用途：定位直播事故、了解播放器版本分布。除此之外不作他用。
   接收方：遥测服务端（sentry.io），数据保留 30 天。
===========================================================`

// PrintPrivacyNotice 打印隐私提示，然后倒计时 wait 后自动继续。
//
// **未启用遥测时立即返回**：不打印、不等待。
//
// 不做「按任意键跳过」：那要读 stdin，而 stdin 在服务化/重定向场景下会立刻 EOF 或永久阻塞，
// 两种都比单纯等 10 秒糟。主播看不完可以回头翻日志——这段也在 stdout 里。
func PrintPrivacyNotice(wait time.Duration) {
	if !Enabled() {
		return
	}

	fmt.Println(privacyNotice)
	countdown(wait)
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
