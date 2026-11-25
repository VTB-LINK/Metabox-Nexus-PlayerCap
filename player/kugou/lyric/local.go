package lyric

// 本地歌词文件（酷狗关联的或其歌词目录里的 .krc、.lrc）的解析与上限（设计 §6.6）。

import (
	"bytes"
	"errors"
	"math"
	"sort"
	"strings"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
)

// MaxLocalFileBytes 是本地歌词文件的大小上限。
const MaxLocalFileBytes = 4 << 20

// 解析的上限。只防畸形或膨胀的输入占满内存，取能覆盖十小时以上逐字歌词的值（依据见设计 §6.6）。
const (
	maxKRCPlainBytes = 4 << 20 // KRC 解压后的明文；在线取词的 KRC 同受此限
	maxLines         = 50000   // 解析出的行数
	maxWords         = 200000  // 逐字总数：各行 Detailed.Words 与 RomaDetailed.Words 之和
	maxTextBytes     = 4 << 20 // 展开文本总量：各行 Text、SubText、RomaText 与全部逐字 Text 的字节数之和
)

// ErrLyricTooLarge 表示歌词文件或其解析结果超出上限。
var ErrLyricTooLarge = i18n.Sentinel("lyric exceeds size limits")

// onBoundedLRCLine 在 parseLRCBounded 每生成一行时调用，供测试计数；出货代码中为空操作。
var onBoundedLRCLine = func() {}

// ParseLocal 解析本地歌词文件的全部字节，按内容判读、不看扩展名：
//   - 以 krc1 开头：DecryptKRC 后按 KRC 明文解析。
//   - 其余解码为文本（见 player.DecodeLyricFileText），先按 KRC 明文解析，零行再按 LRC 解析。
//
// 文件超过 MaxLocalFileBytes、KRC 明文或解析结果超限时返回 ErrLyricTooLarge。解不出行（含 krc1
// 解密失败）时返回 nil, nil。
func ParseLocal(b []byte) ([]Line, error) {
	if len(b) > MaxLocalFileBytes {
		return nil, ErrLyricTooLarge
	}
	if bytes.HasPrefix(b, []byte("krc1")) {
		plain, err := DecryptKRC(b)
		if errors.Is(err, ErrLyricTooLarge) {
			return nil, err
		}
		if err != nil {
			return nil, nil
		}
		return parseKRCBounded(plain)
	}
	text, err := player.DecodeLyricFileText(b)
	if err != nil {
		return nil, nil
	}
	if lines, err := parseKRCBounded(text); err != nil || len(lines) > 0 {
		return lines, err
	}
	return parseLRCBounded(text)
}

// parseKRCBounded 按 KRC 明文解析：行数与逐字总数在解析中由 krc.ParsePlainKRCBounded 限住，展开
// 文本总量在解析完后核对（行数与逐字已受限，文本不超过明文的数倍）。
func parseKRCBounded(plain string) ([]Line, error) {
	lines, ok := krc.ParsePlainKRCBounded(plain, maxLines, maxWords)
	if !ok {
		return nil, ErrLyricTooLarge
	}
	text := 0
	for _, l := range lines {
		text += len(l.Text) + len(l.SubText) + len(l.RomaText)
		for _, w := range l.Detailed.Words {
			text += len(w.Text)
		}
		for _, w := range l.RomaDetailed.Words {
			text += len(w.Text)
		}
	}
	if text > maxTextBytes {
		return nil, ErrLyricTooLarge
	}
	return lines, nil
}

// parseLRCBounded 是本地歌词文件用的 LRC 解析，语法同 parseLRC（在线取词仍用 parseLRC），只多一条：
// 时间为 NaN、±Inf 或负数的时间标签不产生行，同一行其余合法时间戳照常。边解析边累计行数与展开文本
// 总量（一行多个时间戳展开成多行、各计一次），将超限时立即返回 ErrLyricTooLarge，不构造完整结果。
// 与 parseLRC 的两处实现差别不改变结果：逐行迭代而不切分全文；按时间稳定排序（与 parseLRC 的插入
// 排序结果相同，行多且逆序时不退化为平方）。
func parseLRCBounded(lrc string) ([]Line, error) {
	var lines []Line
	textBytes := 0
	for raw := range strings.Lines(lrc) {
		rest := strings.TrimSpace(raw)
		var timestamps []float32
		for strings.HasPrefix(rest, "[") {
			end := strings.Index(rest, "]")
			if end < 0 {
				break
			}
			// t >= 0 对 NaN 为假；上界排除 +Inf。
			if t, ok := parseTimestamp(rest[1:end]); ok && t >= 0 && t <= math.MaxFloat32 {
				timestamps = append(timestamps, t)
			}
			rest = rest[end+1:]
		}
		text := strings.TrimSpace(rest)
		if text == "" {
			continue
		}
		for _, ts := range timestamps {
			if len(lines) >= maxLines || textBytes+len(text) > maxTextBytes {
				return nil, ErrLyricTooLarge
			}
			textBytes += len(text)
			lines = append(lines, Line{Time: ts, Text: text})
			onBoundedLRCLine()
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Time < lines[j].Time })
	for i := range lines {
		lines[i].Index = i
	}
	return lines, nil
}
