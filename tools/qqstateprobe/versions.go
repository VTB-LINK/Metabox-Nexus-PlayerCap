//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
)

// verOffsets 是读歌名与进度所需的那部分偏移，取值照抄 player/qqmusic/mem.go knownVersions
// （该表未导出）。mem.go 增改版本时须同步到这里。
type verOffsets struct {
	Struct1        uintptr // QQMusic.dll 内偏移：now-playing 显示结构体
	Struct2Ptr     uintptr // QQMusic.dll 内偏移：结构体 2 指针；非 0 时歌名优先取 [ptr]+0x64 的 WCHAR*
	FastTimerPtr   uintptr // QQMusic.dll 内偏移：快速进度计时器指针，进度在 [ptr]+FastTimerOff
	FastTimerOff   uintptr
	ProgressDllOff uintptr // QQMusic.dll 内偏移：DWORD 即进度 ms
	NameOff        uintptr // 相对 Struct1
	ProgressOff    uintptr // 相对 Struct1：结构体内进度（上面两路都没有时用）
	Wide           bool    // true=歌名为 WCHAR* 指针；false=MSVC 32 位 std::string（SSO）
}

var knownVersions = map[string]verOffsets{
	"20.05": {Struct1: 0xB63ED0, ProgressDllOff: 0xB61E78, NameOff: 0x70, ProgressOff: 0x68, Wide: true},
	"21.81": {Struct1: 0xB75840, NameOff: 0x00, ProgressOff: 0x6C},
	"22.16": {Struct1: 0xC87C80, Struct2Ptr: 0xC86B00, FastTimerPtr: 0xC157D8, FastTimerOff: 0x618, NameOff: 0x00, ProgressOff: 0x6C},
	"22.22": {Struct1: 0xC95EA0, FastTimerPtr: 0xC23994, FastTimerOff: 0x798, NameOff: 0x80, ProgressOff: 0xEC},
	"22.26": {Struct1: 0xC281E8, ProgressOff: 0x0C, Wide: true},
	"22.31": {Struct1: 0xC332D8, ProgressOff: 0x0C, Wide: true},
	"22.41": {Struct1: 0xC40548, ProgressOff: 0x0C, Wide: true},
	"22.52": {Struct1: 0xC58BD0, ProgressOff: 0x0C, Wide: true},
	"22.60": {Struct1: 0xC6DBA0, ProgressOff: 0x0C, Wide: true},
	"22.71": {Struct1: 0xC7BCF0, ProgressOff: 0x0C, Wide: true},
	"22.76": {Struct1: 0xC7DCF0, ProgressOff: 0x0C, Wide: true},
	"22.81": {Struct1: 0xCCB488, ProgressOff: 0x0C, Wide: true},
	"22.82": {Struct1: 0xCCB468, ProgressOff: 0x0C, Wide: true},
}

// pickOffsets 同 mem.go detectVersion：版本在表里即精确命中；否则按 nearestKnownVersion
// 取版本号距离最近的一项（读不到版本时 version 为 "unknown"，解析失败，退到最新已知版本）。
func pickOffsets(version string) (key string, exact bool) {
	if _, ok := knownVersions[version]; ok {
		return version, true
	}
	return nearestKnownVersion(version), false
}

// 以下三个函数照抄 mem.go。

func parseVer(s string) (int, bool) {
	maj, mnr, ok := strings.Cut(s, ".")
	if !ok {
		return 0, false
	}
	a, e1 := strconv.Atoi(maj)
	b, e2 := strconv.Atoi(mnr)
	if e1 != nil || e2 != nil {
		return 0, false
	}
	return a*1000 + b, true
}

func nearestKnownVersion(detected string) string {
	dv, ok := parseVer(detected)
	if !ok {
		return latestKnownVersion()
	}
	best := ""
	bestDist, bestVer := -1, -1
	for k := range knownVersions {
		kv, ok := parseVer(k)
		if !ok {
			continue
		}
		d := kv - dv
		if d < 0 {
			d = -d
		}
		if bestDist < 0 || d < bestDist || (d == bestDist && kv > bestVer) {
			bestDist, bestVer, best = d, kv, k
		}
	}
	return best
}

func latestKnownVersion() string {
	best, bestVer := "", -1
	for k := range knownVersions {
		if kv, ok := parseVer(k); ok && kv > bestVer {
			bestVer, best = kv, k
		}
	}
	return best
}

// ssoFromBuf 照抄 mem.go：从 24 字节的 MSVC 32 位 std::string 解出字符串。
// +0x10 length、+0x14 capacity；capacity > 15 为堆指针，否则为内联。
func ssoFromBuf(buf []byte, readHeap func(ptr, n uint32) []byte) string {
	if len(buf) < 24 {
		return ""
	}
	length := binary.LittleEndian.Uint32(buf[0x10:0x14])
	capacity := binary.LittleEndian.Uint32(buf[0x14:0x18])
	if capacity > 15 {
		if length > 0 && length < 1000 {
			if ptr := binary.LittleEndian.Uint32(buf[0x00:0x04]); ptr > 0x10000 {
				return string(bytes.Trim(readHeap(ptr, length), "\x00"))
			}
		}
		return ""
	}
	if i := bytes.IndexByte(buf, 0); i > 0 {
		return string(buf[:i])
	}
	return ""
}

// sanitizeString 照抄 mem.go：只留可打印字符并去首尾空白。
func sanitizeString(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= 32 && r != 127 {
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}
