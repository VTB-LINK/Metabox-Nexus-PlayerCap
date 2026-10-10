package lyric

import (
	"syscall"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

// FindPlayTimeAddr 通过 AOB 特征搜索定位播放时间的 float 地址
// 时间结构体布局:
//
//	+0x00: float 播放时间（秒）
//	+0x04: 不固定（0 或 4）
//	+0x08: 字体大小参数（单行=0x1E/30, 双行=0x1C/28）
//	+0x0C: 行高参数  （单行=0x2D/45, 双行=0x2A/42）
//	+0x10: 有效指针（渲染数据）
//
// 搜索策略：两种已知的模式都找，通过校验的全算候选；不止一个时挑读数正在走的那个（pickAdvancing）。
// others 是落选的候选，调用方留着盯（wesing.go 的 altWatch）：选址那一刻歌若是暂停的，哪个都不走，
// 只能凭读数猜一个，开播后才分得出来。
func FindPlayTimeAddr(handle syscall.Handle) (addr uint32, others []uint32, err error) {
	log.Info("Locating play time via signature scan...")

	patterns := []struct {
		name    string
		pattern string
	}{
		{i18n.T("single-line(30/45)"), timePatternSingle},
		{i18n.T("double-line(28/42)"), timePatternDouble},
	}

	regions := proc.EnumWritableRegions(handle)

	var cands []uint32
	var names []string
	for _, p := range patterns {
		pattern, mask := proc.ParseAOBPattern(p.pattern)
		results := proc.AOBScan(handle, pattern, mask, regions)
		if len(results) == 0 {
			continue
		}
		log.Info("Pattern %s matched %d locations, validating...", p.name, len(results))

		for _, hitAddr := range results {
			if a, ok := validateTimeAddr(handle, hitAddr); ok {
				cands = append(cands, a)
				names = append(names, p.name)
			}
		}
	}
	if len(cands) == 0 {
		return 0, nil, i18n.Errorf("Play time address not found")
	}

	best := 0
	if len(cands) > 1 {
		best = pickAdvancing(func(a uint32) (float32, bool) {
			v, err := proc.ReadFloat32(handle, a)
			return v, err == nil && IsPlausiblePlayTime(v)
		}, cands, timeSampleRounds, func() { time.Sleep(timeSampleStep) })
		log.Info("%d play-time candidates; using 0x%08X [%s]", len(cands), cands[best], names[best])
	}
	log.Detail("Play time address: 0x%08X (current: %.2fs) [%s]", cands[best], readTimeOrZero(handle, cands[best]), names[best])
	for i, a := range cands {
		if i != best {
			others = append(others, a)
		}
	}
	return cands[best], others, nil
}

// 多个候选时最多采样 timeSampleRounds 轮、每轮隔 timeSampleStep：在放的歌 40ms 就看得出哪个在走，
// 都不走（暂停着）最多白等 0.32s。
const (
	timeSampleRounds = 8
	timeSampleStep   = 40 * time.Millisecond
	timeAdvanceEps   = 0.01 // 读数比采样起点大出这么多秒才算在走
)

// pickAdvancing 在几个候选里挑读数正在变大的那个，返回它的下标。rounds 轮里哪个都没走（歌暂停着），
// 取读数不为 0 的第一个；都是 0 就取第一个。
//
// 为什么要挑（2026-10-09 真机）：全民K歌开着音译时歌词是双行排版，在走的是「双行(28/42)」那个结构；
// 进程里同时还留着一个「单行(30/45)」的结构，时间恒为 0、+0x10 的指针也合法。旧实现按模式顺序取第一个
// 通过校验的，于是锁在这个不走的结构上：歌词行一行不推、状态报暂停，死地址自愈重新选址选中的还是它。
func pickAdvancing(read func(addr uint32) (float32, bool), addrs []uint32, rounds int, wait func()) int {
	base := make([]float32, len(addrs))
	valid := make([]bool, len(addrs))
	for i, a := range addrs {
		base[i], valid[i] = read(a)
	}
	for r := 0; r < rounds; r++ {
		wait()
		for i, a := range addrs {
			if v, ok := read(a); ok && valid[i] && v > base[i]+timeAdvanceEps {
				return i
			}
		}
	}
	// 哪个都没走：真机上那个不走的旧结构读数恒为 0，而暂停中的真结构停在暂停的位置上（2026-10-09：
	// 0 对 239.79）。取第一个的话，暂停期间报出去的位置是 0、当前行也推不出来。这只是没有更好的凭据
	// 时的取舍，不是「0 不合法」——开头就暂停时真结构也是 0；选错了也只错到开播，altWatch 会换过去。
	for i := range addrs {
		if valid[i] && base[i] > 0 {
			return i
		}
	}
	return 0
}

// FindSongDuration 从 UI 字符串 "mm:ss | mm:ss" 中提取歌曲总时长（秒）
// WeSing 在内存中以 UTF-16LE 存储进度文本，格式为 "当前时间 | 总时长"
func FindSongDuration(handle syscall.Handle) (float32, error) {
	// 搜索 UTF-16LE 编码的 " | " (空格 竖线 空格)
	// 20 00 7C 00 20 00
	// 前后应该是 "mm:ss" 格式（数字+冒号）
	// 完整模式: ?? 00 ?? 00 3A 00 ?? 00 ?? 00 20 00 7C 00 20 00 ?? 00 ?? 00 3A 00 ?? 00 ?? 00
	//           m1    m2    :     s1    s2    SP    |     SP    M1    M2    :     S1    S2
	pattern, mask := proc.ParseAOBPattern(
		"?? 00 ?? 00 3A 00 ?? 00 ?? 00 20 00 7C 00 20 00 ?? 00 ?? 00 3A 00 ?? 00 ?? 00")

	regions := proc.EnumWritableRegions(handle)
	results := proc.AOBScan(handle, pattern, mask, regions)

	for _, addr := range results {
		// 读取 13 个 UTF-16LE 字符 = 26 字节
		buf, err := proc.ReadBytes(handle, addr, 26)
		if err != nil {
			continue
		}

		// 解析为字符
		chars := make([]byte, 13)
		allDigitsOrColon := true
		for i := 0; i < 13; i++ {
			ch := buf[i*2]
			hi := buf[i*2+1]
			if hi != 0 { // 非 ASCII
				allDigitsOrColon = false
				break
			}
			chars[i] = ch
		}
		if !allDigitsOrColon {
			continue
		}

		// 验证格式: "NN:NN | NN:NN"
		text := string(chars)
		if len(text) != 13 || text[2] != ':' || text[5] != ' ' || text[6] != '|' || text[7] != ' ' || text[10] != ':' {
			continue
		}

		// 验证数字
		if !isDigit(text[0]) || !isDigit(text[1]) || !isDigit(text[3]) || !isDigit(text[4]) ||
			!isDigit(text[8]) || !isDigit(text[9]) || !isDigit(text[11]) || !isDigit(text[12]) {
			continue
		}

		// 解析总时长 (右半部分 "MM:SS")
		totalMin := int(text[8]-'0')*10 + int(text[9]-'0')
		totalSec := int(text[11]-'0')*10 + int(text[12]-'0')
		duration := float32(totalMin*60 + totalSec)

		if duration > 0 {
			log.Detail("Song duration: %s (%02d:%02d = %.0fs)", text, totalMin, totalSec, duration)
			return duration, nil
		}
	}

	return 0, i18n.Errorf("Song duration string not found")
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// IsPlausiblePlayTime 判定一个 float 是否可能是合法的播放时间（秒）。
//
// **必须写成「接受式」（v >= 0 && v < 100000），绝不能写成德摩根转换后的
// 「拒绝式」（v < 0 || v >= 100000）。** 两者对实数完全等价，但 IEEE 754 规定
// NaN 的所有比较都返回 false，于是：
//
//	接受式: NaN >= 0 && NaN < 100000  →  false && false = false  → 拒绝 ✓
//	拒绝式: NaN < 0 || NaN >= 100000  →  false || false = false  → 放行 ✗
//
// validateTimeAddr 曾用拒绝式，NaN 因此通过校验成为「播放时间地址」，此后每次
// 读出的都是 NaN，一路污染到 WS 写协程——而 encoding/json 编码不了 NaN，会让
// WriteJSON 报错。
//
// 抽成共享函数是为了消灭拷贝漂移：调用方 wesing.go 在校验缓存地址时本来就写的是
// 接受式（正确），而这里写成了拒绝式（错误）——同一个判定两份拷贝、一对一错。
func IsPlausiblePlayTime(v float32) bool {
	return v >= 0 && v < 100000
}

// 播放时间结构的两种已知形态（+0x08 字号、+0x0C 行高）。扫描（FindPlayTimeAddr）与事后复核
// （IsPlayTimeStruct）都从这两条出，免得各写一份、改漏一处。
const (
	timePatternSingle = "?? ?? ?? ?? ?? ?? ?? ?? 1E 00 00 00 2D 00 00 00"
	timePatternDouble = "?? ?? ?? ?? ?? ?? ?? ?? 1C 00 00 00 2A 00 00 00"
)

// IsPlayTimeStruct 报告 addr 处**此刻**是不是一个播放时间结构：排版参数是已知的两组之一，读数合理，
// +0x10 是指针。FindPlayTimeAddr 给出的候选在扫到的那一刻都满足；沿用一个早先的候选之前要拿它再核
// 一遍——候选会被带到之后的歌里去，那块内存到时可能已经另作他用，而「读得通、值也合理」分不出来
// （AGENTS §7.1）。
func IsPlayTimeStruct(handle syscall.Handle, addr uint32) bool {
	if _, ok := validateTimeAddr(handle, addr); !ok {
		return false
	}
	head, err := proc.ReadBytes(handle, addr, timeStructHead)
	return err == nil && matchesTimePattern(head)
}

// timeStructHead 是两条模式覆盖的字节数。
const timeStructHead = 16

// matchesTimePattern 报告 head（结构开头的 timeStructHead 字节）是否符合两条模式之一。
func matchesTimePattern(head []byte) bool {
	for _, p := range []string{timePatternSingle, timePatternDouble} {
		pattern, mask := proc.ParseAOBPattern(p)
		if len(head) < len(pattern) {
			continue
		}
		ok := true
		for i := range pattern {
			if mask[i] && head[i] != pattern[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func validateTimeAddr(handle syscall.Handle, hitAddr uint32) (uint32, bool) {
	timeVal, err := proc.ReadFloat32(handle, hitAddr)
	if err != nil || !IsPlausiblePlayTime(timeVal) {
		return 0, false
	}
	ptr10, err := proc.ReadUint32(handle, hitAddr+0x10)
	if err != nil || ptr10 < 0x00100000 || ptr10 > 0x7FFFFFFF {
		return 0, false
	}
	return hitAddr, true
}

func readTimeOrZero(handle syscall.Handle, addr uint32) float32 {
	v, err := proc.ReadFloat32(handle, addr)
	if err != nil {
		return 0
	}
	return v
}

// ReadPlayTime 从已知地址读取当前播放时间
func ReadPlayTime(handle syscall.Handle, timeAddr uint32) (float32, error) {
	return proc.ReadFloat32(handle, timeAddr)
}
