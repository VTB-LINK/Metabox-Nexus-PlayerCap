//go:build windows

package procscan

import (
	"slices"

	"Metabox-Nexus-PlayerCap/player/applemusic"
	"Metabox-Nexus-PlayerCap/player/cloudmusic"
	cmwd "Metabox-Nexus-PlayerCap/player/cloudmusic/watchdog"
	"Metabox-Nexus-PlayerCap/player/kugou"
	kugouwd "Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/player/kuwo"
	kuwowd "Metabox-Nexus-PlayerCap/player/kuwo/watchdog"
	"Metabox-Nexus-PlayerCap/player/qqmusic"
	"Metabox-Nexus-PlayerCap/player/sodamusic"
	sodawd "Metabox-Nexus-PlayerCap/player/sodamusic/watchdog"
	"Metabox-Nexus-PlayerCap/player/spotify"
	spotifywd "Metabox-Nexus-PlayerCap/player/spotify/watchdog"
	"Metabox-Nexus-PlayerCap/player/wesing"
)

// PlayerProcess 是「播放器名 → 目标进程名」的映射（单一真源：各 player 包 PlayerName ↔ 对应
// watchdog 的 TargetProcessName）。manager 一律以播放器名做键，仅在求 PID/存活时映射到进程名。
// 本文件 windows 门控：这些常量所在的包是 windows-only，非 windows 构建下 procscan 只剩
// procscan.go + procscan_test.go，不牵连 player 包。
var PlayerProcess = map[string]string{
	wesing.PlayerName:     wesing.TargetProcessName,    // "wesing" → "WeSing.exe"
	cloudmusic.PlayerName: cmwd.TargetProcessName,      // "cloudmusicv3" → "cloudmusic.exe"
	qqmusic.PlayerName:    qqmusic.TargetProcessName,   // "qqmusic" → "qqmusic.exe"
	kugou.PlayerName:      kugouwd.TargetProcessName,   // "kugou" → "KuGou.exe"
	sodamusic.PlayerName:  sodawd.TargetProcessName,    // "sodamusic" → "SodaMusic.exe"
	applemusic.PlayerName: applemusic.AudioProcessName, // "applemusic" → "AMPLibraryAgent.exe"：出声的不是 AppleMusic.exe，见该常量
	spotify.PlayerName:    spotifywd.TargetProcessName, // "spotify" → "Spotify.exe"：播放由主进程（CEF 浏览器进程）自己走 WASAPI，子进程都在它的进程树里
	kuwo.PlayerName:       kuwowd.TargetProcessName,    // "kuwo" → "kwmusic.exe"：捕获包含 KwService 子进程的进程树；已在 9.6.0.0_UG1 真机核对 RMS 非零
}

// Players 是稳定顺序的播放器名列表（供 manager 遍历门控）。
var Players = []string{wesing.PlayerName, cloudmusic.PlayerName, qqmusic.PlayerName, kugou.PlayerName, sodamusic.PlayerName, applemusic.PlayerName, spotify.PlayerName, kuwo.PlayerName}

// PlayerProcesses 返回播放器对应的精确进程名白名单（供 MainPIDAny / AliveAny 使用）。
// 酷狗在默认映射未被覆写时返回双进程名副本（先 HD_KuGou.exe、后 KuGou.exe），避免外部改动污染共享切片；
// 其余播放器或临时覆写映射返回单元素切片；未映射或空串返回 nil。
func PlayerProcesses(player string) []string {
	proc := PlayerProcess[player]
	if proc == "" {
		return nil
	}
	if player == kugou.PlayerName && proc == kugouwd.TargetProcessName {
		return slices.Clone(kugouwd.TargetProcessNames)
	}
	return []string{proc}
}
