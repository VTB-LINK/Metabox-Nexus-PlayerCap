//go:build windows

package i18n

import "golang.org/x/sys/windows"

// LazyProc 做成包级 var（同 telemetry/sysinfo.go：别在调用点反复查找符号）。
var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procGetUserDefaultUILanguage = kernel32.NewProc("GetUserDefaultUILanguage")
)

// langChinese 是 LANG_CHINESE（winnt.h）主语言 ID，涵盖简繁全部变体：
// 简体 zh-CN/zh-SG、繁体 zh-TW/zh-HK/zh-MO。用主语言 ID 一个判断即可覆盖。
const langChinese = 0x04

// DetectSystemLanguage 读 Windows 显示语言（UI language）：主语言为中文即返回 Chinese，
// 其余一切语言（含读取失败）一律 English。
//
// 用 GetUserDefaultUILanguage 而非 GetUserDefaultLocaleName —— 后者是区域格式 locale，
// 会被「区域」设置改变、与显示语言可能不同，不是本项目要的判据（只看显示语言）。
func DetectSystemLanguage() Lang {
	r, _, _ := procGetUserDefaultUILanguage.Call() // 无参，返回 LANGID(WORD)
	if uint16(r)&0x3ff == langChinese {            // PRIMARYLANGID(langid) == LANG_CHINESE
		return Chinese
	}
	return English
}
