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
	"math"
	"regexp"
	"slices"
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
	// 的 wire 零影响。它与 RomaText 同源（都来自 type=0 轨）、只是一个未 Join、一个 Join，
	// 互不影响。
	//
	// 用它的有两处：本包由它与 Detailed 构造 RomaDetailed（见 romaDetailed）；汽水借酷狗音译时按
	// **字序**对齐、跨断行差异重拼行级 RomaText（见 sodamusic/roma.go）。是否可用都按
	// len(RomaWords)==len(Detailed.Words) 判定——相等才逐字配对，不等（老 KRC / 异常轨 / 片段数与
	// 字数不符）则该行不参与字级、回落行级或留空，绝不臆测配对。
	RomaWords []string
	// Detailed 逐字时间轴。PlayTime 此处不填，由调用方按 offset 统一套
	// （见 kugou.go / sodamusic 的 applyDetailedOffset）。
	Detailed player.LyricTextDetailed
	// RomaDetailed 音译的逐字时间轴，进 wire 的 roma_text_detailed（由调用方赋给 RomaTextDetailed）。
	// 片段数与字数相等的行才有（见 romaDetailed），其余为零值、只留行级的 RomaText。type=0 轨的片段
	// 自己不带时间，第 j 个单元的时间取 Detailed.Words[j] 的。Words 是另行分配的切片，不与 Detailed
	// 共享。PlayTime 同 Detailed，此处不填，由调用方的 applyDetailedOffset 一并套。
	RomaDetailed player.LyricTextDetailed
}

var (
	// wordRegex 匹配 KRC 字级标记：<相对偏移ms,时长ms,0>
	wordRegex = regexp.MustCompile(`<(\d+),(\d+),\d+>`)
	// langTagRegex 匹配 KRC 头部 [language:base64json]（酷狗把翻译/音译塞这里）。
	// base64 字母表不含 ']'，故 [^\]]+ 恰好截到闭合方括号。
	langTagRegex = regexp.MustCompile(`\[language:([^\]]+)\]`)
)

// maxLanguageTagBytes 是 ParsePlainKRCBounded 接受的 [language:] 标签文本长度（标签内 base64 文本的
// 字节数，解码之前）上限，约覆盖 5 小时的翻译加音译。超过即按没有翻译与音译处理，正文照常解析：
// 这条轨的 JSON 解码开销远大于其文本（实测 4 MiB 明文、62.9 万个空片段行时累计分配约 94 MiB）。
// 不设上限的 ParsePlainKRC 不受此限。
const maxLanguageTagBytes = 1 << 20

// ParsePlainKRC 解析 KRC 明文全文为 Line（含逐字），不设上限（设上限见 ParsePlainKRCBounded）。
//
// 元数据标签（[ti:]/[ar:]/[al:]/[by:]/[offset:]）一律跳过——与 LRC 解析的行为一致：
// 它对 [offset:] 也是丢弃的（解不出 "offset" 这个分钟数 → 整行没文本 → skip）。
// 这里保持同样处理，不引入未经验证的偏移符号约定；真要支持需先找到 offset 非 0 的样本验证方向。
//
// 非 KRC 内容（普通 LRC / 空串）→ 返回 0 行，让调用方回落到 LRC 解析。
func ParsePlainKRC(krc string) []Line {
	lines, _ := parsePlainKRC(krc, math.MaxInt, math.MaxInt, math.MaxInt)
	return lines
}

// ParsePlainKRCBounded 同 ParsePlainKRC，另设上限：解析出的行数超过 maxLines、或逐字总数（各行
// Detailed.Words 与 RomaDetailed.Words 之和）超过 maxWords 时立即停止，返回 nil, false；
// [language:] 标签文本超过 maxLanguageTagBytes 时按没有翻译与音译处理、正文照常解析。未超限且
// 标签未超长时返回与 ParsePlainKRC 相同的结果与 true。
func ParsePlainKRCBounded(krc string, maxLines, maxWords int) ([]Line, bool) {
	return parsePlainKRC(krc, maxLines, maxWords, maxLanguageTagBytes)
}

// parsePlainKRC 是 ParsePlainKRC 与 ParsePlainKRCBounded 的共同实现，maxLangBytes 是 [language:]
// 标签文本的长度上限。全文逐行迭代、不整段切分；逐字在解析每一行时就按剩余配额封顶；[language:]
// 轨只解码一次，译文与音译按行取用、不预先展开成整轨。
func parsePlainKRC(krc string, maxLines, maxWords, maxLangBytes int) ([]Line, bool) {
	langs := decodeLanguage(krc, maxLangBytes)
	trans, _ := languageTrack(langs, 1) // 翻译轨（type=1），按 [start,dur] 行序号对齐；nil=无翻译轨
	// 音译轨（type=0），同一行号口径，与 trans 各自独立。每行的片段原样进 RomaWords（字级对齐用），
	// Join 后去掉首尾空白即行级 RomaText。
	roma, _ := languageTrack(langs, 0)
	var lines []Line
	words := 0
	rawIdx := -1 // 第几个 [start,dur] 行（含被跳过的 0 词行），与 trans/roma 索引严格对齐
	for raw := range strings.Lines(krc) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		startStr, durStr, body, ok := splitLineHead(raw)
		if !ok {
			continue // 元数据标签行 / 空行——不占翻译槽位
		}
		// 每个 [start,dur] 行占一个翻译槽位。必须在**任何** continue 之前自增：被 0 词/空文本
		// 跳过的行在译轨里照样有一格，漏加会让其后整轨译文错位一格（承重，见 parseTranslation）。
		rawIdx++
		lineStartMs, err := strconv.Atoi(startStr)
		if err != nil {
			continue
		}
		lineDurMs, err := strconv.Atoi(durStr)
		if err != nil {
			continue
		}
		lineWords, ok := parseWords(body, lineStartMs, maxWords-words)
		if !ok {
			return nil, false
		}
		if len(lineWords) == 0 {
			continue
		}
		var text strings.Builder
		for _, w := range lineWords {
			text.WriteString(w.Text)
		}
		plain := strings.TrimSpace(text.String())
		if plain == "" {
			continue
		}
		lineTimestamp := float32(lineStartMs) / 1000.0
		lineDuration := float32(lineDurMs) / 1000.0
		if lineDurMs == 0 {
			for _, w := range lineWords {
				lineDuration += w.Duration
			}
		}
		sub := trackLine(trans, rawIdx)
		// 音译与翻译共用同一 rawIdx 口径（含被跳过的 0 词行），故 type=0 与 type=1 的对齐
		// 严格一致、绝不错位；两者各查各的轨，互不覆盖。
		romaText := trackLine(roma, rawIdx)
		// 同一 rawIdx 口径取本行的逐字音译片段。行级 romaText 与字级片段同源不同形，
		// 各存各的：romaText 进 roma_text，片段供下面构造逐字音译、供汽水字级对齐。
		// 缺轨则为 nil，安全。
		var romaWordFrags []string
		if rawIdx < len(roma) {
			romaWordFrags = roma[rawIdx]
		}
		detailed := player.LyricTextDetailed{
			Timestamp: lineTimestamp,
			Duration:  lineDuration,
			Words:     lineWords,
		}
		romaD := romaDetailed(detailed, romaWordFrags, romaText)
		words += len(lineWords) + len(romaD.Words)
		if len(lines) >= maxLines || words > maxWords {
			return nil, false
		}
		if len(lines) == cap(lines) {
			// 按倍数扩容：5 万行时累计分配约为终值的 2 倍；append 对大切片按 1.25 倍扩容，约为 5 倍。
			lines = slices.Grow(lines, len(lines)+1)
		}
		lines = append(lines, Line{
			Index:        len(lines),
			Time:         lineTimestamp,
			Text:         plain,
			SubText:      sub,
			RomaText:     romaText,
			RomaWords:    romaWordFrags,
			Detailed:     detailed,
			RomaDetailed: romaD,
		})
	}
	return lines, true
}

// splitLineHead 切出 KRC 行首 [行起始ms,行时长ms]：raw 以 "[" 开头，其后依次是一段 ASCII 数字、","、
// 一段 ASCII 数字、"]" 时，返回两段数字与其后的全部内容。语义同正则 ^\[(\d+),(\d+)\](.*)$（raw 已按
// 换行切开，不含 "\n"），但不分配内存：逐行调用正则的分配随行数增长，16 MiB 明文全是 [0,1]a 这样
// 不产出行的短行时累计分配三百多 MB。
func splitLineHead(raw string) (start, dur, body string, ok bool) {
	rest, ok := strings.CutPrefix(raw, "[")
	if !ok {
		return "", "", "", false
	}
	start, rest, ok = strings.Cut(rest, ",")
	if !ok || !isASCIIDigits(start) {
		return "", "", "", false
	}
	dur, body, ok = strings.Cut(rest, "]")
	if !ok || !isASCIIDigits(dur) {
		return "", "", "", false
	}
	return start, dur, body, true
}

// isASCIIDigits 报告 s 非空且只含 0～9。
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// romaDetailed 由一行主歌词的逐字 detailed 与 type=0 轨这一行的逐字片段 frags 构造逐字音译。
//
// 片段数与字数相等才构造：第 j 个单元的文本是 frags[j] 原样（不 Trim，词间的空格是数据的一部分），
// 起始与时长取 detailed.Words[j] 的；行级的起始与时长取 detailed 的。片段数与字数不符的行（每行只有
// 一个片段的老 KRC、异常轨）不臆测配对，返回零值。其余约定（空片段不成单元、各单元拼起来去掉首尾
// 空白等于 romaText、时间轴合法）由 player.BuildRomaDetailed 把关，不成立同样返回零值。
func romaDetailed(detailed player.LyricTextDetailed, frags []string, romaText string) player.LyricTextDetailed {
	if len(frags) != len(detailed.Words) {
		return player.LyricTextDetailed{}
	}
	units := make([]player.LyricTextDetailedWord, len(frags))
	for j, w := range detailed.Words {
		units[j] = player.LyricTextDetailedWord{Timestamp: w.Timestamp, Duration: w.Duration, Text: frags[j]}
	}
	return player.BuildRomaDetailed(player.LyricTextDetailed{
		Timestamp: detailed.Timestamp,
		Duration:  detailed.Duration,
		Words:     units,
	}, romaText)
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
//
// limit 是本行至多产出的字数：将产出第 limit+1 个字时即停，返回 nil, false。标记逐个匹配，不先
// 取出整行的全部标记；文本为空或数字溢出而被跳过的标记不计数。
func parseWords(body string, lineStartMs, limit int) ([]player.LyricTextDetailedWord, bool) {
	var words []player.LyricTextDetailedWord
	m := wordRegex.FindStringSubmatchIndex(body)
	for m != nil {
		// 文本 = 本标记之后、到下一个标记开头之间那段（同 YRC 的取法）
		textEnd := len(body)
		next := wordRegex.FindStringSubmatchIndex(body[m[1]:])
		if next != nil {
			for i := range next {
				next[i] += m[1]
			}
			textEnd = next[0]
		}
		offMs, offErr := strconv.Atoi(body[m[2]:m[3]])
		durMs, durErr := strconv.Atoi(body[m[4]:m[5]])
		if text := body[m[1]:textEnd]; offErr == nil && durErr == nil && text != "" {
			if len(words) == limit {
				return nil, false
			}
			if len(words) == cap(words) {
				words = slices.Grow(words, len(words)+1) // 按倍数扩容，理由同 parsePlainKRC 的行切片
			}
			words = append(words, player.LyricTextDetailedWord{
				Timestamp: float32(lineStartMs+offMs) / 1000.0, // ← 相对转绝对
				Duration:  float32(durMs) / 1000.0,
				Text:      text,
			})
		}
		m = next
	}
	return words, true
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
// 进 roma_text，字级这条供构造逐字音译（romaDetailed）、供汽水借音译按字序对齐（见
// sodamusic/roma.go）。行级那条把每行 Join 成单串是承重语义（kugou 依赖、有门禁钉死），这里要的
// 是 Join 之前的原始片段；两者取自同一次解码（decodeLanguage）的同一条轨。
//
// 不 Trim 片段：片段内/间的空格是音译排版的一部分（如 "kimi " + "dake " Join 后再整体 TrimSpace
// = "kimi dake"，与行级 RomaText 的 TrimSpace(Join) 口径一致）。留给对齐侧聚合后统一 Trim。
func parseRomanizationWords(krc string) [][]string {
	content, _ := languageTrack(decodeLanguage(krc, math.MaxInt), 0)
	return content // 原样返回逐字片段，绝不 Join/Trim（对照 trackLine）
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
	content, ok := languageTrack(decodeLanguage(krc, math.MaxInt), wantType)
	if !ok {
		return nil
	}
	out := make([]string, len(content))
	for i := range content {
		out[i] = trackLine(content, i)
	}
	return out
}

// languageBlock 是 [language:] JSON 里的一条轨（结构见 parseLanguageTrack）。
type languageBlock struct {
	LyricContent [][]string `json:"lyricContent"`
	Type         int        `json:"type"`
}

// decodeLanguage 解出 KRC 头部 [language:] 标签里的全部轨（base64 编码的 JSON）；无标签、标签文本
// 超过 maxBytes、解不开时返回 nil。parsePlainKRC 每次解析只调用一次，翻译、音译与逐字音译都从这一次
// 的结果里取。
func decodeLanguage(krc string, maxBytes int) []languageBlock {
	m := langTagRegex.FindStringSubmatch(krc)
	if m == nil || len(m[1]) > maxBytes {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return nil
	}
	var doc struct {
		Content []languageBlock `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc.Content
}

// languageTrack 返回 blocks 中首条 type 为 wantType 的轨的逐行片段；没有该轨时 ok 为假。
func languageTrack(blocks []languageBlock, wantType int) (content [][]string, ok bool) {
	for _, blk := range blocks {
		if blk.Type == wantType {
			return blk.LyricContent, true
		}
	}
	return nil, false
}

// trackLine 返回轨 content 第 i 行（第 i 个 [start,dur] 行）的文本：片段拼接后去掉首尾空白；
// 越界为空串。
func trackLine(content [][]string, i int) string {
	if i >= len(content) {
		return ""
	}
	return strings.TrimSpace(strings.Join(content[i], ""))
}
