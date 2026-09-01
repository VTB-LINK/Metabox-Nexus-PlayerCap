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
	// RomaText 逐行音译（发音标注）。KRC 内嵌 [language:] type=0 轨才有；无轨回落为空。
	// 纯文本、无独立时间轴——继承本行时间，applyDetailedOffset 不碰它。
	// 与 SubText（翻译）是两条独立轨、互不覆盖：任一轨解析失败绝不波及另一轨或主歌词。
	// 「音译」的形式因来源而异（酷狗 KRC type=0 实测为汉语谐音，网易云同槽位为罗马音）；
	// 字段沿用 wire 既有的 roma_text 名，不随内容形态改名。
	RomaText string
	// RomaWords 是本行音译的**逐字片段**（type=0 的 lyricContent[i] 原样、未 Join、未 Trim），
	// 语义上 RomaWords[j] 对应 Detailed.Words[j] 那个字的音译（酷狗 KRC「按行号+字级位置对齐」，
	// 见 parseRomanizationWords）。**纯内部字段、不进 wire**：krc.Line 从不被直接序列化（下游一律
	// 经 player.BuildLyricLine 显式搬字段进 player.LyricLine），故新增它对 kugou/sodamusic/all
	// 的 wire 零影响；对外产物仍只有行级 RomaText。它与 RomaText 同源（都来自 type=0 轨）、
	// 只是一个未 Join、一个 Join，互不影响：kugou 只读行级 RomaText，字级供汽水借音译用。
	//
	// 它只为一件事存在：汽水借酷狗音译时按**字序**对齐、跨断行差异重拼行级 RomaText（见
	// sodamusic/roma.go）。是否可用由调用方按 len(RomaWords)==len(Detailed.Words) 自行判定——
	// 相等才逐字配对，不等（老 KRC / 异常轨 / 片段数与字数不符）则该行不参与字级、回落行级或
	// 留空，绝不臆测配对。
	RomaWords []string
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
	roma := parseRomanization(krc) // 同一行号口径对齐的音译（nil=无音译轨），与 trans 各自独立
	// romaWords 是同一 type=0 轨的**逐字片段**（未 Join），口径与 roma 完全一致（同一 rawIdx
	// 行号对齐）。与 roma 各取所需、互不影响：roma 走 Join 供行级 RomaText，romaWords 供字级对齐。
	romaWords := parseRomanizationWords(krc)
	var lines []Line
	rawIdx := -1 // 第几个 [start,dur] 行（含被跳过的 0 词行），与 trans/roma 索引严格对齐
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
		// 音译与翻译共用同一 rawIdx 口径（含被跳过的 0 词行），故 type=0 与 type=1 的对齐
		// 严格一致、绝不错位；两者各查各的轨，互不覆盖。
		var romaText string
		if rawIdx < len(roma) {
			romaText = roma[rawIdx]
		}
		// 同一 rawIdx 口径取本行的逐字音译片段。行级 romaText 与字级 romaWords 同源不同形，
		// 各存各的：kugou 只用 romaText，汽水字级对齐用 romaWords。缺轨则为 nil，安全。
		var romaWordFrags []string
		if rawIdx < len(romaWords) {
			romaWordFrags = romaWords[rawIdx]
		}
		lines = append(lines, Line{
			Index:     len(lines),
			Time:      lineTimestamp,
			Text:      plain,
			SubText:   sub,
			RomaText:  romaText,
			RomaWords: romaWordFrags,
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

// parseTranslation 从 KRC 头部 [language:] 标签解出**中文翻译轨**（type=1），返回按主歌词
// [start,dur] 行序号对齐的 []string（第 i 项 = 第 i 个 [start,dur] 行的译文，缺则空串）。
// 无 [language:] 标签 / 解不开 / 无翻译轨时返回 nil。对齐口径见 parseLanguageTrack。
//
// 抽取自 parseLanguageTrack 后**行为逐字节不变**：仍是「取首条 type==1、按行号对齐、
// TrimSpace(Join(frags,""))」，只是把 type 参数化。守卫见 romanization_test.go。
func parseTranslation(krc string) []string {
	return parseLanguageTrack(krc, 1)
}

// parseRomanization 从 KRC 头部 [language:] 标签解出**逐行音译轨**（type=0），返回口径与
// parseTranslation 完全一致（同一 parseLanguageTrack、同一 [start,dur] 行号对齐）——这是
// 两轨绝不错位的前提。无 [language:] 标签 / 解不开 / 无音译轨时返回 nil。
//
// type=0 的 lyricContent 是逐字音译片段（如 [["ro","ma"],...]），Join("") 即整行音译，与
// type=1 每行单串走同一条 Join 路径，两轨口径天然一致。**只取 type=0，绝不触碰 type=1**，
// 与 SubText（翻译）互不污染。
//
// 「音译」的形式因来源而异：酷狗 KRC type=0 实测为汉语谐音（如 이유 넌 → 一哟 弄），
// 网易云同槽位为罗马音——本函数不区分内容形态，照取、照对齐、照写 RomaText。
func parseRomanization(krc string) []string {
	return parseLanguageTrack(krc, 0)
}

// parseRomanizationWords 解出 type=0 音译轨的**逐字片段**（不 Join、不 Trim），返回按主歌词
// [start,dur] 行序号对齐的 [][]string：第 i 项 = 第 i 行的逐字音译片段序列，即 type=0 块的
// lyricContent[i]（`["ro","ma"]` 这种，每片段对应该行一个字）。无 [language:] / 解不开 /
// 无 type=0 轨 → nil；某行无片段则该项为空/ nil 切片。
//
// 与 parseRomanization（行级 Join）**同源、同 [start,dur] 行号口径、互不影响**：行级那条
// 给 kugou 直接显示，字级这条给汽水借音译按字序对齐用（见 sodamusic/roma.go）。刻意**不复用**
// parseLanguageTrack——那个函数把每行 Join 成单串是行级路径的承重语义（kugou 依赖、有门禁钉死），
// 这里恰恰要的是 Join 之前的原始片段，故单开一份只多一次 base64+json（每首歌一次，非热路径）。
//
// 不 Trim 片段：片段内/间的空格是音译排版的一部分（如 "kimi " + "dake " Join 后再整体 TrimSpace
// = "kimi dake"，与行级 RomaText 的 TrimSpace(Join) 口径一致）。留给对齐侧聚合后统一 Trim。
func parseRomanizationWords(krc string) [][]string {
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
		if blk.Type != 0 {
			continue
		}
		return blk.LyricContent // 原样返回逐字片段，绝不 Join/Trim（对照 parseLanguageTrack 的 out[i]）
	}
	return nil
}

// parseLanguageTrack 从 KRC 头部 [language:] 标签解出指定 type 的一条轨，返回按主歌词
// [start,dur] 行序号对齐的 []string（第 i 项 = 第 i 个 [start,dur] 行的文本，缺则空串）。
// 无 [language:] 标签 / 解不开 / 无该 type 轨时返回 nil；有多条同 type 块时取首条。
//
// [language:] 是 base64 编码的 JSON：
//
//	{"content":[
//	   {"lyricContent":[["译文行"],...], "type":1},   // type=1 中文翻译（parseTranslation 取）
//	   {"lyricContent":[["ro","ma"],...],"type":0}    // type=0 逐行音译（parseRomanization 取）
//	 ], "version":1}
//
// **与网易云的机制根本不同，别照抄 MergeTlyric/MergeRomalrc**：网易云是独立 tlyric/romalrc
// LRC、按**毫秒时间戳**匹配；酷狗是同一份 KRC 内嵌、**按行号位置对齐**——lyricContent[i]
// 就是第 i 个 [start,dur] 行的文本，本身无时间戳。故调用方必须数「每个 [start,dur] 行」
// （含被跳过的 0 词行）来对齐，一旦按「产出的 Line 数」对齐，遇到被跳过的行就整轨错位。
// 翻译（type=1）与音译（type=0）共用这套口径，两轨对齐严格一致、绝不互相污染。
//
// 注：汽水音乐的翻译不走这条内嵌轨（它是独立字段），故对汽水明文 type=1 恒返回 nil，
// SubText 由汽水侧自行写回；type=0 若平台 KRC 内嵌则照常解出、无则 nil——这是有意的：
// 本包只认 KRC 内嵌轨这一种真源。
func parseLanguageTrack(krc string, wantType int) []string {
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
		if blk.Type != wantType {
			continue
		}
		out := make([]string, len(blk.LyricContent))
		for i, frags := range blk.LyricContent {
			out[i] = strings.TrimSpace(strings.Join(frags, ""))
		}
		return out
	}
	return nil
}
