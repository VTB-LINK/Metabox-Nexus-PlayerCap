// Package krc 解析酷狗 KRC 明文歌词（字级时间轴 + 内嵌翻译轨），单一真源。
//
// 两个播放器共用这份解析：
//   - 酷狗（kugou）：从 krcs.kugou.com 取回 base64 密文，自行 krcDecrypt 后喂明文进来。
//   - 汽水音乐（sodamusic）：字节的 transport 直接给**已解密的明文** KRC，跳过解密。
//
// 解密（base64 → krc1 魔数 → 定长 XOR → zlib）是酷狗专属，**不在本包**——本包只吃明文，
// 因为「谁产出明文」两家不同（酷狗自己解，汽水平台已解）。这样切分让脆弱的「相对偏移 /
// rawIdx 对齐」不变量只有一份真源，符合 AGENTS.md「单一真源」。
package krc

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"Metabox-Nexus-PlayerCap/player"
)

// Line 是一条解析后的 KRC 歌词行（Detailed 带字级时间轴）。
type Line struct {
	Index int
	Time  float32 // seconds
	Text  string
	// SubText 第二行歌词（翻译）。KRC 内嵌 [language:] 轨才有；无轨回落为空。
	// 纯文本、无独立时间轴——继承本行时间，applyDetailedOffset 不碰它。
	// 汽水音乐的译文是独立字段（不在 KRC 里），由调用方解析后自行写回 SubText。
	SubText string
	// Detailed 逐字时间轴。PlayTime 此处不填，由调用方按 offset 统一套
	// （见 kugou.go / sodamusic 的 applyDetailedOffset）。
	Detailed player.LyricTextDetailed
}

var (
	// lineRegex 匹配 KRC 行首：[行起始ms,行时长ms]
	lineRegex = regexp.MustCompile(`^\[(\d+),(\d+)\](.*)$`)
	// wordRegex 匹配 KRC 字级标记：<相对偏移ms,时长ms,0>
	wordRegex = regexp.MustCompile(`<(\d+),(\d+),\d+>`)
	// langTagRegex 匹配 KRC 头部 [language:base64json]（酷狗把翻译/音译塞这里）。
	// base64 字母表不含 ']'，故 [^\]]+ 恰好截到闭合方括号。
	langTagRegex = regexp.MustCompile(`\[language:([^\]]+)\]`)
)

// ParsePlainKRC 解析 KRC 明文全文为 Line（含逐字）。
//
// 元数据标签（[ti:]/[ar:]/[al:]/[by:]/[offset:]）一律跳过——与 LRC 解析的行为一致：
// 它对 [offset:] 也是丢弃的（解不出 "offset" 这个分钟数 → 整行没文本 → skip）。
// 这里保持同样处理，不引入未经验证的偏移符号约定；真要支持需先找到 offset 非 0 的样本验证方向。
//
// 非 KRC 内容（普通 LRC / 空串）→ 返回 0 行，让调用方回落到 LRC 解析。
func ParsePlainKRC(krc string) []Line {
	trans := parseTranslation(krc) // 按 [start,dur] 行序号对齐的译文（nil=无翻译轨）
	var lines []Line
	rawIdx := -1 // 第几个 [start,dur] 行（含被跳过的 0 词行），与 trans 索引严格对齐
	for _, raw := range strings.Split(krc, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		m := lineRegex.FindStringSubmatch(raw)
		if m == nil {
			continue // 元数据标签行 / 空行——不占翻译槽位
		}
		// 每个 [start,dur] 行占一个翻译槽位。必须在**任何** continue 之前自增：被 0 词/空文本
		// 跳过的行在译轨里照样有一格，漏加会让其后整轨译文错位一格（承重，见 parseTranslation）。
		rawIdx++
		lineStartMs, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		lineDurMs, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		words := parseWords(m[3], lineStartMs)
		if len(words) == 0 {
			continue
		}
		var text strings.Builder
		for _, w := range words {
			text.WriteString(w.Text)
		}
		plain := strings.TrimSpace(text.String())
		if plain == "" {
			continue
		}
		lineTimestamp := float32(lineStartMs) / 1000.0
		lineDuration := float32(lineDurMs) / 1000.0
		if lineDurMs == 0 {
			for _, w := range words {
				lineDuration += w.Duration
			}
		}
		var sub string
		if rawIdx < len(trans) {
			sub = trans[rawIdx]
		}
		lines = append(lines, Line{
			Index:   len(lines),
			Time:    lineTimestamp,
			Text:    plain,
			SubText: sub,
			Detailed: player.LyricTextDetailed{
				Timestamp: lineTimestamp,
				Duration:  lineDuration,
				Words:     words,
			},
		})
	}
	return lines
}

// parseWords 从 KRC 行体解析逐字片段。行体形如：
//
//	<0,450,0>词<450,450,0>：<900,450,0>周
//
// **三家三个样，照抄任何一家都会错**：
//
//	QQ QRC  ：文字在前、时间戳在后，偏移**绝对**    风(28837,439)
//	网易云 YRC：时间戳在前、文字在后，偏移**绝对**    (28837,439,0)风
//	酷狗 KRC ：时间戳在前、文字在后，偏移**相对行首** <0,439,0>风   ← 本函数（汽水音乐同款）
//
// 故绝对时间 = lineStartMs + 偏移。把 KRC 的偏移当绝对时间用，每行的字会全挤到歌曲开头
// （行级却是对的），症状极像"前端没对齐"，最难查。
//
// 实证：《晴天》行 [2250,2250] 的末字 <1800,450,0> → 2250+1800+450 = 4500，正好等于下一行
// 起始 [4500,...]，逐字与行级严丝合缝。
//
// 不对片段文本做 TrimSpace：空格是逐字排版的一部分。
// PlayTime 此处不填（offset 未知），由调用方的 applyDetailedOffset 统一套。
func parseWords(body string, lineStartMs int) []player.LyricTextDetailedWord {
	tuples := wordRegex.FindAllStringSubmatchIndex(body, -1)
	if len(tuples) == 0 {
		return nil
	}
	words := make([]player.LyricTextDetailedWord, 0, len(tuples))
	for i, m := range tuples {
		offMs, err := strconv.Atoi(body[m[2]:m[3]])
		if err != nil {
			continue
		}
		durMs, err := strconv.Atoi(body[m[4]:m[5]])
		if err != nil {
			continue
		}
		// 文本 = 本标记之后、到下一个标记开头之间那段（同 YRC 的取法）
		textEnd := len(body)
		if i+1 < len(tuples) {
			textEnd = tuples[i+1][0]
		}
		text := body[m[1]:textEnd]
		if text == "" {
			continue
		}
		words = append(words, player.LyricTextDetailedWord{
			Timestamp: float32(lineStartMs+offMs) / 1000.0, // ← 相对转绝对
			Duration:  float32(durMs) / 1000.0,
			Text:      text,
		})
	}
	return words
}

// parseTranslation 从 KRC 头部的 [language:] 标签解出**中文翻译轨**，返回按主歌词
// [start,dur] 行序号对齐的 []string（第 i 项 = 第 i 个 [start,dur] 行的译文，缺则空串）。
// 无 [language:] 标签 / 解不开 / 无翻译轨时返回 nil。
//
// [language:] 是 base64 编码的 JSON：
//
//	{"content":[
//	   {"lyricContent":[["译文行"],...], "type":1},   // type=1 中文翻译 ← 本函数只取它
//	   {"lyricContent":[["ro","ma"],...],"type":0}    // type=0 罗马音（wire 无第三行字段，不做）
//	 ], "version":1}
//
// **与网易云的机制根本不同，别照抄 MergeTlyric**：网易云是独立 tlyric LRC、按**毫秒时间戳**
// 匹配；酷狗是同一份 KRC 内嵌、**按行号位置对齐**——lyricContent[i] 就是第 i 个 [start,dur]
// 行的译文，本身无时间戳。故调用方必须数「每个 [start,dur] 行」（含被跳过的 0 词行）来对齐，
// 一旦按「产出的 Line 数」对齐，遇到被跳过的行就整轨错位。
//
// 注：汽水音乐的翻译不走这条内嵌轨（它是独立字段），故对汽水明文本函数恒返回 nil，
// SubText 由汽水侧自行写回——这是有意的：本包只认 KRC 内嵌轨这一种真源。
func parseTranslation(krc string) []string {
	m := langTagRegex.FindStringSubmatch(krc)
	if m == nil {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return nil
	}
	var doc struct {
		Content []struct {
			LyricContent [][]string `json:"lyricContent"`
			Type         int        `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	for _, blk := range doc.Content {
		if blk.Type != 1 {
			continue // 只取中文翻译；type=0 是罗马音
		}
		out := make([]string, len(blk.LyricContent))
		for i, frags := range blk.LyricContent {
			out[i] = strings.TrimSpace(strings.Join(frags, ""))
		}
		return out
	}
	return nil
}
