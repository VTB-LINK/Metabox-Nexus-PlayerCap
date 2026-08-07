package sodamusic

import (
	"strconv"
	"strings"

	"Metabox-Nexus-PlayerCap/player/krc"
)

// matchToleranceMs 是主歌词行起始与译轨时间戳的允许偏差（毫秒）。
//
// 取 10 不是拍脑袋的经验值，是**译轨自身的精度**：汽水的 translations.cn 是 `[mm:ss.xx]`
// 厘秒制（真机样本《We Are The World》：`[00:26.56]`），而 KRC 行起始是毫秒制。两者同源时
// 厘秒侧最多与毫秒侧差一个厘秒格（±10ms），故 10 恰好覆盖「同一行因精度不同而对不上」这一种
// 情形，不多不少。
//
// 误配风险可忽略：要认错行，得有两行歌词起始相隔 ≤10ms——那不是歌词，是解析事故。
const matchToleranceMs = 10

// applySodaTranslations 把汽水的翻译轨（tlyric 式独立 LRC）按**绝对时间戳**合并进 KRC 行的 SubText。
//
// **与酷狗内嵌 [language:] 轨（按行号对齐）机制不同**：汽水是网易云 tlyric 式——一份独立的
// LRC，时间戳与主歌词行起始同源（KRC `[26560,..]`=26.56s ↔ 译文 `[00:26.56]`=26560ms）。
//
// 匹配口径为「最近邻，±10ms 以内」。真机实测两口径在现有数据上**结果完全相同**——
// 2026-08-08 采《We Are The World》105 行：精确相等 105/105，±10ms 亦 105/105，最近邻距离
// 全为 0。放宽到 10ms 不是为了修一个已观测到的失败，而是封掉一个结构性隐患：
//
//	同日采《听海》（32 行，无译轨）实测行起始 45519 / 53253 / 60457 —— **毫秒精度，105 行
//	那种整 10ms 对齐只占 1/32**。也就是说「KRC 可以是毫秒精度」是真实存在的形态，一旦它与
//	厘秒制译轨相遇，精确相等会**整轨静默丢译文**（无日志、无报错，只是 sub_text 全空）。
//	现有样本里有译轨的歌恰好都是整 10ms 对齐（105/105），但那是相关性不是保证。
//
// 匹配不上的行（译轨没覆盖到，如间奏/纯音乐段）SubText 留空——绝不强塞，宁缺毋错。
func applySodaTranslations(lines []krc.Line, translationLRC string) {
	if translationLRC == "" || len(lines) == 0 {
		return
	}
	tmap := parseTranslationLRC(translationLRC)
	if len(tmap) == 0 {
		return
	}
	for i := range lines {
		if text, ok := lookupNearest(tmap, roundToMs(lines[i].Time)); ok {
			lines[i].SubText = text
		}
	}
}

// lookupNearest 在 tmap 里找与 ms 相差不超过 matchToleranceMs 的条目，**由近及远**取第一个命中。
// 按距离升序探测而非遍历整表：命中即最近邻，且与译轨行数无关（O(容差) 而非 O(行数)）。
func lookupNearest(tmap map[int]string, ms int) (string, bool) {
	if text, ok := tmap[ms]; ok {
		return text, true
	}
	for d := 1; d <= matchToleranceMs; d++ {
		if text, ok := tmap[ms-d]; ok {
			return text, true
		}
		if text, ok := tmap[ms+d]; ok {
			return text, true
		}
	}
	return "", false
}

// parseTranslationLRC 解析 tlyric LRC → map[毫秒]译文。
//
// 逐行剥掉前缀 `[..]` 时间戳标签（一行可带多个，压缩型 LRC），各时间戳都指向同一段文本；
// 解不出的标签（元数据 [ti:]/[by:] 或畸形）跳过。无时间戳/空文本的行不入表。
func parseTranslationLRC(lrc string) map[int]string {
	out := make(map[int]string)
	for _, raw := range strings.Split(lrc, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var times []int
		for strings.HasPrefix(raw, "[") {
			end := strings.Index(raw, "]")
			if end < 0 {
				break
			}
			if ms, ok := lrcTagToMs(raw[1:end]); ok {
				times = append(times, ms)
			}
			raw = raw[end+1:]
		}
		text := strings.TrimSpace(raw)
		if text == "" || len(times) == 0 {
			continue
		}
		for _, ms := range times {
			out[ms] = text
		}
	}
	return out
}

// lrcTagToMs 解析一个 LRC 时间戳标签体（不含方括号）为毫秒，形如 `mm:ss.xx` / `mm:ss:xx` / `mm:ss`。
// 非时间戳（元数据标签如 ti/ar/by）返回 false。
func lrcTagToMs(tag string) (int, bool) {
	tag = strings.ReplaceAll(tag, ",", ".")
	colon := strings.Index(tag, ":")
	if colon < 0 {
		return 0, false
	}
	minStr := strings.TrimSpace(tag[:colon])
	min, err := strconv.Atoi(minStr)
	if err != nil {
		return 0, false
	}
	// 秒部分可能是 ss.xx 或 ss:xx（把首个 ':' 归一成 '.'）或纯 ss。
	rest := strings.Replace(strings.TrimSpace(tag[colon+1:]), ":", ".", 1)
	sec, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return 0, false
	}
	return int((float64(min)*60+sec)*1000 + 0.5), true
}

// roundToMs 把秒（float32）四舍五入到毫秒整数，与 parseTranslationLRC 的键同口径。
func roundToMs(sec float32) int {
	return int(float64(sec)*1000 + 0.5)
}
