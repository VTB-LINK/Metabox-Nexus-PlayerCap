package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/taskbar"
	"Metabox-Nexus-PlayerCap/telemetry"
)

// config.yml 的强制重建在 main 一侧的部分：按网关的 config_force_update 判定、重建、告知用户、留下标记
// （设计 doc/design/config-version-and-offset-delay-design.md §5.4～§5.6）。判定与重建本身在 config 包。

// 重建之后设给后续进程的两个环境变量（子进程继承本进程的环境）。
const (
	// envConfigRebuilt 记本进程树已经处理过的门槛。带着它的进程拿到响应时，响应里的门槛不高于它就跳过
	// 判定：同一棵进程树里同一个门槛至多处理一次，与文件内容无关——模板漏了版本键、盖章值算错这类缺陷
	// 也只多重启一次，不会循环。更高的门槛是合法的新要求，照常判定。
	envConfigRebuilt = "PLAYERCAP_CONFIG_REBUILT"
	// envConfigBackup 记重建留下的备份的完整路径，重启出来的进程在启动完成处再提示一行。
	envConfigBackup = "PLAYERCAP_CONFIG_BACKUP"
)

// rebuildNoticeWait 是重建提示的停留时间，同隐私提示。它使服务晚这么久启动，只发生在重建的那一次。
const rebuildNoticeWait = 10 * time.Second

// 可注入，测试用桩替换；生产路径恒为下面的值。
var (
	// restartForRebuild 在重建之后、本次又不更新时重启本进程以加载新的 config.yml。此刻调用是安全的：
	// checkAndUpdate 早于 srv.Start、心跳与音频管理器，端口未绑定，没有需要收尾的状态。
	restartForRebuild = restartSelf
	// showRebuildNotice 把重建提示打到控制台、让任务栏变色并闪烁，停留 rebuildNoticeWait：其间在提示框
	// 之下逐秒倒计时，用的是隐私提示的那一行（telemetry.Countdown），结束后该行被擦掉。
	showRebuildNotice = func(lines []string) {
		fmt.Println()
		for _, line := range lines {
			fmt.Println(line)
		}
		fmt.Println()
		// 不在这里 Detach：本次若还要更新，下载进度仍要用任务栏后端。
		taskbar.Init()
		taskbar.Warning()
		taskbar.Flash()
		telemetry.Countdown(rebuildNoticeWait)
		taskbar.Clear()
	}
)

// rebuildConfigIfForced 按网关字段 forceUpdate（config_force_update）判定并重建 config.yml，返回是否
// 重建成功。判定成立而重建没有成功时返回 false：不因重建而重启，以已加载的配置继续运行（config 包已
// 告警并给出人工补救的办法）。
//
// 重建成功后设两个环境变量再返回，随后的重启（含由更新带出的重启）把它们带给新进程。
func rebuildConfigIfForced(cfg *config.Config, forceUpdate string) bool {
	if !cfg.ShouldRebuild(forceUpdate, Version, os.Getenv(envConfigRebuilt)) {
		return false
	}
	mainLog.Warn("config.yml (version %s) is older than %s, the version below which the gateway requires it to be rebuilt; rebuilding it from the template of this version", cfg.FileVersionLabel, normalizeSemver(forceUpdate))
	report := cfg.ForceRebuild(Version)
	if !report.Done {
		return false
	}
	os.Setenv(envConfigRebuilt, normalizeSemver(forceUpdate))
	os.Setenv(envConfigBackup, report.Backup)
	showRebuildNotice(rebuildNoticeLines(report))
	return true
}

// rebuildNoticeLines 返回重建提示的各行（样式同自动更新的提示框）：已重建、备份的文件名、旧文件与
// 模板取值不同的键（「键: 原值 → 重建后的值」，原值已是当前语义下的值，可直接照填）、备份里的 offset
// 是哪种写法、接下来会重启。取值的写法同启动横幅上被拒原值的写法（bannerLiteral）。
//
// 备份只写文件名，并说明它与 config.yml 同目录（renameToBackup 把它建在 config.yml 所在的目录）：
// 完整路径太长，终端折行后续行没有左边框。完整路径见提示框之上 config.ForceRebuild 的那行日志，
// 重启出来的进程在启动完成处还会再提示一行（envConfigBackup）。
func rebuildNoticeLines(report config.RebuildReport) []string {
	lines := []string{
		"╔═════════════════════════════════════════════════════════╗",
		i18n.T("║  ⚠ config.yml has been rebuilt from the template of this version"),
		fmt.Sprintf(i18n.T("║  Previous file kept as %s (same folder as config.yml)"), filepath.Base(report.Backup)),
	}
	switch {
	case !report.DiffKnown:
		lines = append(lines, i18n.T("║  The previous file could not be parsed, so its settings are not listed"))
	case len(report.Diffs) == 0:
		lines = append(lines, i18n.T("║  None of its settings differed from the template"))
	default:
		lines = append(lines, i18n.T("║  Settings that differed from the template (previous → now):"))
		for _, d := range report.Diffs {
			now := i18n.T("(not set in the template)")
			if d.InTemplate {
				now = bannerLiteral(d.New)
			}
			lines = append(lines, fmt.Sprintf("║    %s: %s → %s", d.Key, bannerLiteral(d.Old), now))
		}
	}
	if report.DiffKnown {
		if report.BackupLegacy {
			lines = append(lines, i18n.T("║  Offsets in the backup still use the old form (positive = earlier); use the values listed above"))
		} else {
			lines = append(lines, i18n.T("║  Offsets in the backup already use the new form (positive = later)"))
		}
	}
	return append(lines,
		i18n.T("║  The program will restart to load the new config.yml"),
		"╚═════════════════════════════════════════════════════════╝")
}
