package lyric

import (
	"syscall"

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
// 搜索策略：分别尝试已知的模式组合
func FindPlayTimeAddr(handle syscall.Handle) (uint32, error) {
	log.Info("通过特征搜索定位播放时间...")

	patterns := []struct {
		name    string
		pattern string
	}{
		{i18n.T("单行(30/45)"), "?? ?? ?? ?? ?? ?? ?? ?? 1E 00 00 00 2D 00 00 00"},
		{i18n.T("双行(28/42)"), "?? ?? ?? ?? ?? ?? ?? ?? 1C 00 00 00 2A 00 00 00"},
	}

	regions := proc.EnumWritableRegions(handle)

	for _, p := range patterns {
		pattern, mask := proc.ParseAOBPattern(p.pattern)
		results := proc.AOBScan(handle, pattern, mask, regions)
		if len(results) == 0 {
			continue
		}
		log.Info("模式 %s 命中 %d 处，验证...", p.name, len(results))

		for _, hitAddr := range results {
			if addr, ok := validateTimeAddr(handle, hitAddr); ok {
				log.Detail("播放时间地址: 0x%08X (当前: %.2fs) [%s]", addr, readTimeOrZero(handle, addr), p.name)
				return addr, nil
			}
		}
	}

	return 0, i18n.Errorf("未找到播放时间地址")
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
			log.Detail("歌曲总时长: %s (%02d:%02d = %.0fs)", text, totalMin, totalSec, duration)
			return duration, nil
		}
	}

	return 0, i18n.Errorf("未找到歌曲时长字符串")
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
