// Package lyric 解析 Apple Music 的 iTunes TTML 歌词（从进程内存扫出的连续 UTF-16 文档）。
//
// TTML 结构（真机《晴天》/《火烧云》实测）：
//
//	<tt xmlns=".../ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal"
//	    itunes:timing="Line"|"Word" xml:lang="..">
//	  <head><metadata>
//	    <iTunesMetadata><translations><translation xml:lang="zh-Hans">
//	      <text for="L1">译文</text> ...           ← 按 itunes:key 对齐（非时间戳）
//	    </translation></translations><songwriters>..</songwriters></iTunesMetadata>
//	  </metadata></head>
//	  <body dur="4:29.747">
//	    <div begin=".." end=".." itunes:songPart="Verse">
//	      <p begin="29.188" end="32.398" itunes:key="L1">故事的小黃花</p>   ← Line 型
//	      <p begin=".." itunes:key="L5"><span begin="t" end="t2">字</span>..</p> ← Word 型逐字
//	    </div> ...
//	  </body></tt>
//
// 同一结构下的其他写法：时间也可写成带度量后缀的偏移（begin="1.127s"，见 parseTS）；Word 型的背景人声是
// 没有时间的 <span ttm:role="x-bg"> 套着逐字 span（见 parseLineContent）；译文 <text> 里也可能套 <span>
// （见 flattenText）。
//
// 输出复用 player/krc.Line（与 kugou/sodamusic 一致，直接进 BuildLyricLine）：
// Time=begin，Text=行文本，SubText=按 itunes:key 匹配的译文，Detailed=逐字（仅 Word 型）。
package lyric

import (
	"encoding/xml"
	"math"
	"regexp"
	"strconv"
	"strings"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
)

// durAttr 抓 <body dur="4:29.747">。它**不是**真实音频时长：比位置结构里的时长（= SMTC EndTime）
// 长 0.03~3.77s（实测）。播放器只拿它判 TTML 是否可能属于当前歌（applemusic 包的 pickTTML），
// 时长与定位一律以位置结构为准。
var durAttr = regexp.MustCompile(`<body[^>]*\bdur="([^"]+)"`)

// DocDuration 从 TTML 文档取整曲时长（秒）。无 <body dur> → (0, false)。
func DocDuration(doc string) (float32, bool) {
	m := durAttr.FindStringSubmatch(doc)
	if m == nil {
		return 0, false
	}
	return parseTS(m[1])
}

type ttmlDoc struct {
	XMLName xml.Name
	Timing  string `xml:"timing,attr"` // itunes:timing
	// 译文轨：<head>..<translations><translation><text for="Lk">译</text>。Go xml 按 local name 匹配路径。
	Trans []transText `xml:"head>metadata>iTunesMetadata>translations>translation>text"`
	Ps    []pElem     `xml:"body>div>p"`
}

type transText struct {
	For string `xml:"for,attr"`
	// 内层原样保留、由 flattenText 取字：译文里可能套 <span>（背景人声的译文，或整句包在 span 里），
	// `,chardata` 只收直属文字，会丢掉 span 里的那部分。
	Inner string `xml:",innerxml"`
}

type pElem struct {
	Begin string `xml:"begin,attr"`
	End   string `xml:"end,attr"`
	Key   string `xml:"key,attr"` // itunes:key，用于对齐译文
	// 内层原样保留：Word 型是 <span> 与其间空白交错，需按文档顺序重建行文本，
	// 否则只拼 span 会丢掉 span 之间的空格（"Hello World" → "HelloWorld"）。
	Inner string `xml:",innerxml"`
}

// ParseTTML 解析一份 TTML 文档为歌词行。非法/空 → 返回 nil。
// PlayTime 此处不填（由调用方按 offset 统一套，见 applemusic 的 applyDetailedOffset）。
func ParseTTML(doc string) []krc.Line {
	var d ttmlDoc
	if err := xml.Unmarshal([]byte(doc), &d); err != nil {
		return nil
	}
	// 多个 <translation>（多种语言）的 <text> 在 d.Trans 里按文档顺序摊平：同一 key 先到先得。
	trans := make(map[string]string, len(d.Trans))
	for _, t := range d.Trans {
		if _, dup := trans[t.For]; t.For != "" && !dup {
			if s := flattenText(t.Inner); s != "" {
				trans[t.For] = s
			}
		}
	}

	lines := make([]krc.Line, 0, len(d.Ps))
	for _, p := range d.Ps {
		begin, ok := parseTS(p.Begin)
		if !ok {
			continue
		}
		text, words := parseLineContent(p.Inner)
		if text == "" {
			continue
		}
		line := krc.Line{
			Index:   len(lines),
			Time:    begin,
			Text:    text,
			SubText: trans[p.Key],
		}
		if len(words) > 0 {
			end, _ := parseTS(p.End)
			dur := end - begin
			if dur < 0 {
				dur = 0
			}
			line.Detailed = player.LyricTextDetailed{
				Timestamp: begin,
				Duration:  dur,
				Words:     words,
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// xmlSpaces 匹配连续的 XML 空白（空格、制表、回车、换行）。全角空格等是歌词自己的排版，不在其列。
var xmlSpaces = regexp.MustCompile(`[ \t\r\n]+`)

// foldSpace 把连续的 XML 空白折成一个空格，并去掉首尾空白。
func foldSpace(s string) string {
	return strings.TrimSpace(xmlSpaces.ReplaceAllString(s, " "))
}

// flattenText 取一段 XML 片段里的全部文字（按文档顺序，不看标签），空白按 foldSpace 折叠。
func flattenText(inner string) string {
	dec := xml.NewDecoder(strings.NewReader(inner))
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if cd, ok := tok.(xml.CharData); ok {
			b.Write(cd)
		}
	}
	return foldSpace(b.String())
}

// parseLineContent 按文档顺序遍历 <p> 内层：chardata（含 span 间空白）拼进行文本；每个解得出 begin 的
// <span> 取其 begin/end 与内层文本，既拼进行文本、又作为一个逐字词。Line 型（无 span）则整段即行文本。
// 行文本里连续的 XML 空白折成一个空格（foldSpace）：文档带换行缩进排版时，span 之间的换行不得进入行文本。
//
// span 之间的 chardata 同时并入前一个词（空白折成一个空格），与 KRC/YRC 词尾带空格的形态一致。
// 只拼进行文本不够：overlay 的逐字效果与 lyrics_detailed[].text 都直接拼接 words，英文、韩文等
// 以空格分词的歌会显示成「HelloWorld」（审查 P2）。行尾那个词的尾部空白去掉，与行文本去首尾空白一致。
//
// 解不出 begin 的 <span> 是容器，不整段读掉，其子节点照常处理：背景人声写作
// <span ttm:role="x-bg"><span begin end>(…</span><span begin end>…)</span></span>，外层没有时间，
// 整段读掉的话背景词只进行文本、不进 words。
func parseLineContent(inner string) (string, []player.LyricTextDetailedWord) {
	dec := xml.NewDecoder(strings.NewReader(inner))
	var b strings.Builder
	var words []player.LyricTextDetailedWord
	defer func() {
		if n := len(words); n > 0 {
			words[n-1].Text = strings.TrimRight(words[n-1].Text, " \t\r\n")
		}
	}()
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
			if n := len(words); n > 0 {
				if s := string(t); strings.TrimSpace(s) != "" {
					words[n-1].Text += s
				} else if s != "" && !strings.HasSuffix(words[n-1].Text, " ") {
					words[n-1].Text += " "
				}
			}
		case xml.StartElement:
			if t.Name.Local != "span" {
				continue
			}
			var beginA, endA string
			for _, a := range t.Attr {
				switch a.Name.Local {
				case "begin":
					beginA = a.Value
				case "end":
					endA = a.Value
				}
			}
			sb, ok := parseTS(beginA)
			if !ok {
				continue // 容器：子节点由外层循环照常处理
			}
			// 读该 span 的内层文本（吃到它的 EndElement 为止，容忍嵌套）。
			var sp strings.Builder
			depth := 1
			for depth > 0 {
				tk, err := dec.Token()
				if err != nil {
					break
				}
				switch tt := tk.(type) {
				case xml.CharData:
					sp.Write(tt)
				case xml.StartElement:
					depth++
				case xml.EndElement:
					depth--
				}
			}
			w := sp.String()
			b.WriteString(w)
			dur := float32(0)
			if se, ok2 := parseTS(endA); ok2 && se > sb {
				dur = se - sb
			}
			words = append(words, player.LyricTextDetailedWord{Timestamp: sb, Duration: dur, Text: w})
		}
	}
	return foldSpace(b.String()), words
}

// metricUnits 是 TTML 偏移时间的度量后缀与每单位的秒数（min 是 SMIL 时钟值的写法，与 m 同义）。
// 按序匹配，"ms" 须排在 "s" 之前，否则 "500ms" 会被切成 "500m" 加 "s"。
var metricUnits = []struct {
	suffix string
	sec    float64
}{
	{"ms", 0.001},
	{"min", 60},
	{"h", 3600},
	{"m", 60},
	{"s", 1},
}

// parseTS 解析 TTML 时间表达式为秒。两种写法：
//   - 时钟写法 `[h:][m:]ss.mmm`（"29.188" / "1:05.491" / "4:29.747"），至多三段；四段以上
//     （如带帧号的 hh:mm:ss:ff）拒绝，不按多一级 60 进位去算。
//   - 偏移写法：数值后紧跟度量 h / m / min / s / ms（"1.127s" / "500ms"）。真机见过整份文档都这样写
//     （2026-10-08），不认后缀时每一行都被跳过、整首被当成没有歌词。
//
// 含冒号的写法不接受后缀；数值与后缀之间不得有空白。
func parseTS(s string) (float32, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, false
	}
	num, unit := strings.TrimSpace(parts[len(parts)-1]), 1.0
	if len(parts) == 1 {
		for _, u := range metricUnits {
			if strings.HasSuffix(num, u.suffix) {
				num, unit = num[:len(num)-len(u.suffix)], u.sec
				break
			}
		}
	}
	last, err := strconv.ParseFloat(num, 64)
	if err != nil || math.IsNaN(last) || math.IsInf(last, 0) {
		return 0, false // ParseFloat 接受 "NaN"/"Inf"/"1e999"：非有限时间进入载荷会让 json.Marshal 失败
	}
	sec := last * unit
	mult := 60.0
	for i := len(parts) - 2; i >= 0; i-- {
		v, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil {
			return 0, false
		}
		sec += float64(v) * mult
		mult *= 60
	}
	if f := float32(sec); !math.IsInf(float64(f), 0) && !math.IsNaN(float64(f)) && f >= 0 {
		return f, true
	}
	return 0, false // 负数 / float32 溢出
}
