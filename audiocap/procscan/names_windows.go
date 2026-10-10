//go:build windows

package procscan

import (
	"Metabox-Nexus-PlayerCap/player/applemusic"
	"Metabox-Nexus-PlayerCap/player/cloudmusic"
	cmwd "Metabox-Nexus-PlayerCap/player/cloudmusic/watchdog"
	"Metabox-Nexus-PlayerCap/player/kugou"
	kugouwd "Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/player/qqmusic"
	"Metabox-Nexus-PlayerCap/player/sodamusic"
	sodawd "Metabox-Nexus-PlayerCap/player/sodamusic/watchdog"
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
}

// Players 是稳定顺序的播放器名列表（供 manager 遍历门控）。
var Players = []string{wesing.PlayerName, cloudmusic.PlayerName, qqmusic.PlayerName, kugou.PlayerName, sodamusic.PlayerName, applemusic.PlayerName}
