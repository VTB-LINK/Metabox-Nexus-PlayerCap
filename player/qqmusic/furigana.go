package qqmusic

import (
	"strings"
	"unicode/utf8"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
)

// maxKanaTagBytes 是单曲累计允许收集的 [kana:] 标签体字节上限（256 KiB），
// 防止恶意或损坏的大标签导致无界内存分配。
const maxKanaTagBytes = 256 << 10

const (
	kanaTagPrefix         = "[kana:"
	qrcLyricContentMarker = `LyricContent="`
)

// kanaEntry 是 [kana:] 中的单条振假名词条：count 为覆盖的目标字符数（1..9），
// ruby 为空表示空跳过词条（按 count 消耗连续目标字符但不显示注音）。
type kanaEntry struct {
	count int
	ruby  string
}

// subToken 是对齐阶段的行内子词元：连续数字聚为单个子词元，其余每个码点各自为一个子词元。
// first 一律经 utf8.DecodeRuneInString 解码，避免对 UTF-8 字符串按字节取首字符。
type subToken struct {
	text  string
	first rune
}

// kanaCollector 在歌词行扫描阶段流式收集主轨的 [kana:...] 元数据行。
// 一旦遇到缺闭合 ']'、累计字节超限或词条语法非法，立即置为无效并释放已收集切片，
// 后续再遇 [kana:] 行仍消费该行但不分配也不解析。
type kanaCollector struct {
	seen       bool
	invalid    bool
	reason     string
	totalBytes int
	items      []kanaEntry
}

// matchKanaLine 检查单行是否为 [kana:...] 元数据行（含紧贴的 QRC XML LyricContent 头壳）；
// 命中时返回剥除 "[kana:" 前缀后的剩余串与 true。
func matchKanaLine(line string) (rest string, ok bool) {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "<") {
		if idx := strings.Index(s, qrcLyricContentMarker); idx >= 0 {
			s = s[idx+len(qrcLyricContentMarker):]
		}
	}
	if !strings.HasPrefix(s, kanaTagPrefix) {
		return "", false
	}
	return s[len(kanaTagPrefix):], true
}

// feed 检查单行是否为 [kana:...] 标签行；命中并消费时返回 true，非标签行返回 false。
func (c *kanaCollector) feed(line string) bool {
	rest, ok := matchKanaLine(line)
	if !ok {
		return false
	}
	c.feedRest(rest)
	return true
}

// feedRest 消费经 matchKanaLine 剥除 "[kana:" 前缀后的标签体剩余串。
func (c *kanaCollector) feedRest(rest string) {
	c.seen = true
	if c.invalid {
		return
	}

	end := strings.LastIndexByte(rest, ']')
	if end < 0 {
		c.fail(i18n.T("unclosed [kana:] tag"))
		return
	}
	body := rest[:end]
	if len(body) > maxKanaTagBytes-c.totalBytes {
		c.fail(i18n.T("[kana:] tag exceeds size limit"))
		return
	}
	c.totalBytes += len(body)

	parsed, ok := parseKanaTag(body)
	if !ok {
		c.fail(i18n.T("malformed [kana:] tag"))
		return
	}
	c.items = append(c.items, parsed...)
}

func (c *kanaCollector) fail(reason string) {
	c.invalid = true
	c.reason = reason
	c.items = nil
}

// hasTag 报告扫描过程中是否见到过至少一行 [kana:] 标签（含无效标签）。
func (c *kanaCollector) hasTag() bool {
	return c.seen
}

// entries 返回收集到的词条序列、是否全部合法以及非法时的本地化原因。
func (c *kanaCollector) entries() ([]kanaEntry, bool, string) {
	if c.invalid {
		return nil, false, c.reason
	}
	return c.items, true, ""
}

// isKanaReadingChar 判断码点是否属于 QQMusicLyricFrame.dll 接受的假名读音字符范围
// （U+3041..U+30FE：平假名、平假名叠字符、片假名、中点、长音符、片假名叠字符）。
func isKanaReadingChar(r rune) bool {
	return r >= 0x3041 && r <= 0x30FE
}

// skipKanaTiming 解析单个假名音节后紧跟的 (<startMs>,<durMs>)，每段限制 1～9 位 ASCII 十进制数字。
func skipKanaTiming(body string, i int) (int, bool) {
	if i >= len(body) || body[i] != '(' {
		return 0, false
	}
	i++

	start := i
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		i++
	}
	if d := i - start; d < 1 || d > 9 {
		return 0, false
	}

	if i >= len(body) || body[i] != ',' {
		return 0, false
	}
	i++

	start = i
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		i++
	}
	if d := i - start; d < 1 || d > 9 {
		return 0, false
	}

	if i >= len(body) || body[i] != ')' {
		return 0, false
	}
	return i + 1, true
}

// parseKanaTag 解析单个 [kana:...] 标签体（不含首尾方括号与 "kana:" 前缀）为有序词条序列。
// 任一位置出现语法畸形、非法 UTF-8、增补平面字符或同词条内有无时间混写，一律返回 (nil, false)。
func parseKanaTag(body string) ([]kanaEntry, bool) {
	if body == "" {
		return nil, true
	}
	var entries []kanaEntry
	var rubyBuf strings.Builder
	for i := 0; i < len(body); {
		r, size := utf8.DecodeRuneInString(body[i:])
		if r < '1' || r > '9' {
			return nil, false
		}
		count := int(r - '0')
		i += size

		if i >= len(body) {
			entries = append(entries, kanaEntry{count: count})
			break
		}
		next, nextSize := utf8.DecodeRuneInString(body[i:])
		if next >= '1' && next <= '9' {
			entries = append(entries, kanaEntry{count: count})
			continue
		}
		if !isKanaReadingChar(next) {
			return nil, false
		}

		rubyBuf.Reset()
		if afterFirst := i + nextSize; afterFirst < len(body) && body[afterFirst] == '(' {
			for i < len(body) {
				kr, kSize := utf8.DecodeRuneInString(body[i:])
				if !isKanaReadingChar(kr) {
					break
				}
				rubyBuf.WriteRune(kr)
				i += kSize
				nextIdx, ok := skipKanaTiming(body, i)
				if !ok {
					return nil, false
				}
				i = nextIdx
			}
		} else {
			for i < len(body) {
				kr, kSize := utf8.DecodeRuneInString(body[i:])
				if !isKanaReadingChar(kr) {
					break
				}
				rubyBuf.WriteRune(kr)
				i += kSize
			}
		}

		if i < len(body) {
			tail, _ := utf8.DecodeRuneInString(body[i:])
			if tail < '1' || tail > '9' {
				return nil, false
			}
		}
		entries = append(entries, kanaEntry{count: count, ruby: rubyBuf.String()})
	}
	return entries, true
}

// isSubTokenDigit 判断码点是否参与数字子词元聚类（半角 '0'..'9' 与全角 '０'..'９'）。
func isSubTokenDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 0xFF10 && r <= 0xFF19)
}

// isKanaTarget 判断码点是否为 [kana:] 游标对齐的目标字符（々、全角数字、CJK 基本区与扩展 A 区）。
// 半角 ASCII 数字 '0'..'9' 不是目标字符。
func isKanaTarget(r rune) bool {
	return r == 0x3005 ||
		(r >= 0xFF10 && r <= 0xFF19) ||
		(r >= 0x4E00 && r <= 0x9FBB) ||
		(r >= 0x3400 && r <= 0x4DB5)
}

func subTokenize(s string) []subToken {
	if s == "" {
		return nil
	}
	var toks []subToken
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		start := i
		i += size
		if isSubTokenDigit(r) {
			for i < len(s) {
				nr, nSize := utf8.DecodeRuneInString(s[i:])
				if !isSubTokenDigit(nr) {
					break
				}
				i += nSize
			}
		}
		toks = append(toks, subToken{text: s[start:i], first: r})
	}
	return toks
}

// sameTargetSubTokens 检查两段子词元流中全部目标子词元（isKanaTarget(first)）的文本序列是否逐项相等。
func sameTargetSubTokens(a, b []subToken) bool {
	i, j := 0, 0
	for {
		for i < len(a) && !isKanaTarget(a[i].first) {
			i++
		}
		for j < len(b) && !isKanaTarget(b[j].first) {
			j++
		}
		if i == len(a) || j == len(b) {
			return i == len(a) && j == len(b)
		}
		if a[i].text != b[j].text {
			return false
		}
		i++
		j++
	}
}

// isQRCXMLClosingTail 识别合法 QRC 末行紧贴外层 XML 闭合标签的尾部白名单。
func isQRCXMLClosingTail(tail string) bool {
	switch strings.TrimSpace(tail) {
	case `"/>`, `"/></LyricInfo>`, `"/></LyricInfo></QrcInfos>`:
		return true
	default:
		return false
	}
}

// attachFurigana 将 [kana:] 词条序列按全曲目标字符游标守恒对齐到主歌词各行。
// 入口处先清空各行已有 Furigana；仅当全部约束与全曲终点守恒（cursor == len(entries)）
// 同时成立时才一次性写回新结果；空词条或任一失配均保证全曲所有行 Furigana 恒为 nil。
// 若全曲守恒通过但个别含注音候选的行因文本与逐字不匹配而局部降级为 nil，返回 (true, reason)。
func attachFurigana(lines []lyricLine, entries []kanaEntry) (bool, string) {
	for i := range lines {
		lines[i].Furigana = nil
	}
	if len(entries) == 0 {
		return true, ""
	}
	if len(lines) == 0 {
		return false, i18n.T("kana entry count mismatch")
	}

	cand := make([]player.LyricFurigana, len(lines))
	cursor := 0
	partialDegraded := false

	for i := range lines {
		l := &lines[i]
		var wordsText string
		var effectiveLineText string
		var toks []subToken

		if len(l.Detailed.Words) > 0 {
			var wb strings.Builder
			for _, w := range l.Detailed.Words {
				wb.WriteString(w.Text)
			}
			wordsText = wb.String()
			effectiveLineText = l.Text
			trimmed := strings.TrimSpace(wordsText)
			if trimmed != l.Text && strings.HasPrefix(l.Text, trimmed) && isQRCXMLClosingTail(l.Text[len(trimmed):]) {
				effectiveLineText = trimmed
			}
			toks = subTokenize(wordsText)
			if trimmed != effectiveLineText && !sameTargetSubTokens(toks, subTokenize(effectiveLineText)) {
				return false, i18n.T("untimed lyric tail alters furigana targets")
			}
		} else {
			effectiveLineText = l.Text
			toks = subTokenize(l.Text)
		}

		var rawSegs []player.LyricFuriganaSegment
		var plain strings.Builder
		lineHasRuby := false
		flushPlain := func() {
			if plain.Len() > 0 {
				rawSegs = append(rawSegs, player.LyricFuriganaSegment{Text: plain.String()})
				plain.Reset()
			}
		}
		for tIdx := 0; tIdx < len(toks); {
			tok := toks[tIdx]
			if !isKanaTarget(tok.first) {
				plain.WriteString(tok.text)
				tIdx++
				continue
			}
			if cursor >= len(entries) {
				return false, i18n.T("kana entry count mismatch")
			}
			ent := entries[cursor]
			cursor++
			if ent.count <= 0 {
				return false, i18n.T("malformed [kana:] tag")
			}

			var base strings.Builder
			matched := 0
			cur := tIdx
			for matched < ent.count {
				if cur >= len(toks) || !isKanaTarget(toks[cur].first) {
					return false, i18n.T("kana entry misaligned with lyrics")
				}
				for _, r := range toks[cur].text {
					if isKanaTarget(r) {
						matched++
					}
				}
				base.WriteString(toks[cur].text)
				cur++
			}
			if ent.ruby == "" {
				plain.WriteString(base.String())
			} else {
				flushPlain()
				lineHasRuby = true
				rawSegs = append(rawSegs, player.LyricFuriganaSegment{
					Text: base.String(),
					Ruby: ent.ruby,
				})
			}
			tIdx = cur
		}
		flushPlain()

		cand[i] = player.BuildFurigana(rawSegs, effectiveLineText, wordsText)
		if lineHasRuby && cand[i] == nil {
			partialDegraded = true
		}
	}

	if cursor != len(entries) {
		return false, i18n.T("kana entry count mismatch")
	}
	for i := range lines {
		lines[i].Furigana = cand[i]
	}
	if partialDegraded {
		return true, i18n.T("line text mismatch on some lines")
	}
	return true, ""
}
