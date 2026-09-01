package krc

// 本文件测酷狗内嵌音译轨（type=0）：parseRomanization 只取 type=0、绝不误取 type=1（翻译），
// 以及 ParsePlainKRC 把音译按 [start,dur] 行号对齐进 Line.RomaText——与翻译轨（SubText，
// type=1）**同一套行号口径**、两轨互不污染。
//
// 「音译」的形式因来源而异：酷狗 KRC type=0 实测为汉语谐音，网易云同槽位为罗马音；解析层
// 不区分内容形态，字段统称 roma_text。用例沿用 translation_test.go 的 langTagA/langTagB
// （langTagA 自带一条 type=0 罗马音形态的干扰轨），此处把它当作**要取的目标轨**来断言。

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// mkLangTag 用给定的 type=1（翻译）与 type=0（音译）两轨构造一个 [language:] 头部标签。
// 直接由结构体编码 base64，避免手抄出错；每行一个片段（lyricContent[i]=[]string{items[i]}），
// 与酷狗「按行号位置对齐」口径一致。传 nil 表示不放该轨。
func mkLangTag(trans, roma []string) string {
	type block struct {
		LyricContent [][]string `json:"lyricContent"`
		Type         int        `json:"type"`
	}
	wrap := func(items []string) [][]string {
		out := make([][]string, len(items))
		for i, s := range items {
			out[i] = []string{s}
		}
		return out
	}
	var doc struct {
		Content []block `json:"content"`
		Version int     `json:"version"`
	}
	doc.Version = 1
	if trans != nil {
		doc.Content = append(doc.Content, block{LyricContent: wrap(trans), Type: 1})
	}
	if roma != nil {
		doc.Content = append(doc.Content, block{LyricContent: wrap(roma), Type: 0})
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return "[language:" + base64.StdEncoding.EncodeToString(b) + "]"
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParseRomanizationTakesType0 —— 只取音译轨(type=0)，绝不误取中文翻译(type=1)。
// 变异自证：把 parseRomanization 改成 parseLanguageTrack(krc, 1)，本例会拿到中文翻译而红。
func TestParseRomanizationTakesType0(t *testing.T) {
	roma := parseRomanization(langTagA + "\n[0,1000]<0,500,0>x\n")
	want := []string{"se n", "shi", "kyo", "da i", "ra i"}
	if len(roma) != len(want) {
		t.Fatalf("音译轨行数=%d, want %d（是否误取了 type=1 翻译轨？）", len(roma), len(want))
	}
	for i := range want {
		if roma[i] != want[i] {
			t.Fatalf("音译轨[%d]=%q, want %q", i, roma[i], want[i])
		}
	}
	// 反向断言：绝不等于 type=1 翻译文本（互不污染的一半）。
	if roma[3] == "英勇无畏 维新革命" {
		t.Fatalf("音译轨[3] 取到了翻译文本，type=0/type=1 串轨了")
	}
}

// TestParseRomanizationJoinsFragments —— type=0 每行是逐字音译片段，Join("") = 整行音译
// （如 ["ro","ma"] → "roma"），与 type=1 每行单串走同一条 Join 路径。
func TestParseRomanizationJoinsFragments(t *testing.T) {
	doc := `{"content":[{"lyricContent":[["ro","ma"],["bo","ku"]],"type":0}],"version":1}`
	tag := "[language:" + base64.StdEncoding.EncodeToString([]byte(doc)) + "]"
	roma := parseRomanization(tag)
	want := []string{"roma", "boku"}
	if len(roma) != len(want) {
		t.Fatalf("行数=%d, want %d", len(roma), len(want))
	}
	for i := range want {
		if roma[i] != want[i] {
			t.Fatalf("roma[%d]=%q, want %q（片段拼接=整行音译）", i, roma[i], want[i])
		}
	}
}

// TestParseRomanizationAbsent —— 无 [language:] 标签 → nil；ParsePlainKRC 照常出词、RomaText 全空。
func TestParseRomanizationAbsent(t *testing.T) {
	if roma := parseRomanization("[ti:x]\n[0,1000]<0,500,0>甲\n"); roma != nil {
		t.Fatalf("无 [language:] 应返回 nil，got %#v", roma)
	}
	// ParsePlainKRC 侧：无 [language:] 时每行 RomaText 留空，不报错、不强塞。
	lines := ParsePlainKRC("[0,1000]<0,500,0>甲<500,500,0>乙\n")
	if len(lines) != 1 || lines[0].RomaText != "" {
		t.Fatalf("无 [language:] 时 RomaText 应为空，got %#v", lines)
	}
}

// TestParseRomanizationNoType0Track —— 只含 type=1 翻译轨、无 type=0 时音译返回 nil（不误取翻译）。
// langTagB 只有 type=1（见 translation_test.go）。对称断言翻译轨照常取到，证明确有 type=1、只缺 type=0。
func TestParseRomanizationNoType0Track(t *testing.T) {
	if roma := parseRomanization(langTagB); roma != nil {
		t.Fatalf("仅含 type=1 时音译应 nil，got %#v", roma)
	}
	if tr := parseTranslation(langTagB); len(tr) == 0 {
		t.Fatalf("langTagB 的 type=1 翻译轨应非空——用例前提不成立")
	}
}

// TestParsePlainKRCRomaTextAligned —— 端到端：音译按 [start,dur] 行号对齐进 RomaText，
// 且与翻译轨（SubText）在同一份 KRC 上**双向互不污染**。
// langTagA 的 type=0 每行都有值（含头部行），type=1 只有 idx3/idx4 有值——正好检验两轨独立。
func TestParsePlainKRCRomaTextAligned(t *testing.T) {
	krc := langTagA + "\n" +
		"[0,1000]<0,500,0>千<500,500,0>本\n" +
		"[1000,1000]<0,500,0>词<500,500,0>：\n" +
		"[2000,1000]<0,500,0>曲<500,500,0>：\n" +
		"[32119,2870]<0,900,0>大<900,900,0>胆\n" +
		"[34989,3180]<0,900,0>磊<900,900,0>落\n"
	lines := ParsePlainKRC(krc)
	if len(lines) != 5 {
		t.Fatalf("行数=%d, want 5", len(lines))
	}
	wantRoma := []string{"se n", "shi", "kyo", "da i", "ra i"}
	for i, w := range wantRoma {
		if lines[i].RomaText != w {
			t.Fatalf("lines[%d].RomaText=%q, want %q", i, lines[i].RomaText, w)
		}
	}
	// ★ 两轨互不污染（双向）：idx3 上 SubText=翻译、RomaText=音译，各是各的；
	// idx0 上 SubText 空但 RomaText 有值——证明音译不继承翻译的空缺、反之亦然。
	if lines[3].SubText != "英勇无畏 维新革命" || lines[3].RomaText != "da i" {
		t.Fatalf("行3 sub/roma=%q/%q, want 英勇无畏 维新革命/da i（两轨串了？）", lines[3].SubText, lines[3].RomaText)
	}
	if lines[0].SubText != "" || lines[0].RomaText != "se n" {
		t.Fatalf("行0 sub/roma=%q/%q, want \"\"/se n（音译不该继承翻译的空缺）", lines[0].SubText, lines[0].RomaText)
	}
}

// TestParsePlainKRCRomaTextSkipsBlankLineWithoutMisalign —— ★ 承重：主歌词里夹一个 0 词行
// （被 ParsePlainKRC 跳过、不产出 Line），但它在音译轨里**照样占一格**。rawIdx 必须为它自增，
// 否则其后音译整轨错位一格。type=0 与 type=1 同口径对齐，此处两轨一并断言。
// 变异自证：把 ParsePlainKRC 里的 rawIdx++ 移到 `if len(words)==0 { continue }` 之后，
// 大胆/磊落 会分别错拿到 "SKIP"/"daitan"（音译）与 ""/英勇无畏（翻译）而红。
func TestParsePlainKRCRomaTextSkipsBlankLineWithoutMisalign(t *testing.T) {
	trans := []string{"", "", "", "", "英勇无畏", "光明磊落"}
	roma := []string{"qianben", "kotoba", "kyoku", "SKIP", "daitan", "rairaku"}
	krc := mkLangTag(trans, roma) + "\n" +
		"[0,1000]<0,500,0>千<500,500,0>本\n" + // rawIdx0
		"[1000,1000]<0,500,0>词<500,500,0>：\n" + // rawIdx1
		"[2000,1000]<0,500,0>曲<500,500,0>：\n" + // rawIdx2
		"[3000,1000]\n" + // rawIdx3：0 词空行 → 跳过，但占轨一格
		"[32119,2870]<0,900,0>大<900,900,0>胆\n" + // rawIdx4
		"[34989,3180]<0,900,0>磊<900,900,0>落\n" // rawIdx5
	lines := ParsePlainKRC(krc)
	if len(lines) != 5 {
		t.Fatalf("产出行数=%d, want 5（0 词行应被跳过）", len(lines))
	}
	// 关键：尽管中间跳了一行，大胆/磊落 仍拿到 rawIdx4/5 的音译，而非错位的 rawIdx3/4。
	if lines[3].Text != "大胆" || lines[3].RomaText != "daitan" {
		t.Fatalf("音译对齐错位！行3 text/roma=%q/%q, want 大胆/daitan", lines[3].Text, lines[3].RomaText)
	}
	if lines[4].Text != "磊落" || lines[4].RomaText != "rairaku" {
		t.Fatalf("音译对齐错位！行4 text/roma=%q/%q, want 磊落/rairaku", lines[4].Text, lines[4].RomaText)
	}
	// 同一份 KRC 上翻译轨也按同口径对齐——双轨对齐一致（互不污染 + 同错位口径）。
	if lines[3].SubText != "英勇无畏" || lines[4].SubText != "光明磊落" {
		t.Fatalf("翻译对齐错位！行3/4 sub=%q/%q, want 英勇无畏/光明磊落", lines[3].SubText, lines[4].SubText)
	}
}

// TestParseLanguageTrackTypeIsolation —— 「抽取 parseLanguageTrack 后 parseTranslation 行为
// 不变」的守卫，并钉死两轨类型隔离：
//   - parseTranslation 仍只取 type=1（与抽取前逐字节一致）
//   - parseRomanization 只取 type=0
//   - 同一份 [language:] 上两者各取各的、结果不同、绝不串轨
//
// 变异自证：把 parseTranslation 改成 parseLanguageTrack(krc, 0)，第一组断言即红。
func TestParseLanguageTrackTypeIsolation(t *testing.T) {
	tr := parseTranslation(langTagA)
	wantTr := []string{"", "", "", "英勇无畏 维新革命", "光明磊落反战国家"}
	if !equalStrs(tr, wantTr) {
		t.Fatalf("parseTranslation(langTagA)=%#v, want %#v（抽取后 type=1 行为变了）", tr, wantTr)
	}
	roma := parseRomanization(langTagA)
	wantRoma := []string{"se n", "shi", "kyo", "da i", "ra i"}
	if !equalStrs(roma, wantRoma) {
		t.Fatalf("parseRomanization(langTagA)=%#v, want %#v", roma, wantRoma)
	}
	// 两轨结果必须不同（同一份 [language:] 上各取各的）。
	if equalStrs(tr, roma) {
		t.Fatalf("翻译轨与音译轨结果相同，type 隔离失效")
	}
}
