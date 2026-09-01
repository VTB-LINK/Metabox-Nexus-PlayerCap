package krc

// 本文件测 type=0 音译轨的**字级**接线：parseRomanizationWords 解出逐字片段、ParsePlainKRC
// 把它按 [start,dur] 行号对齐进 Line.RomaWords（供汽水借音译按字序对齐用），并钉死：
//   - RomaWords 是**未 Join 的原始逐字片段**，RomaWords[j] 对应 Detailed.Words[j] 那个字；
//   - 行级 RomaText（Join 后）与字级 RomaWords **同源共存、互不影响**——kugou 只读 RomaText，
//     加了 RomaWords 后它逐字节不变（回归钉死）；
//   - 「片段数 == 字数」的不变量：成立才逐字配对；不成立（line-shaped type=0）时原样存下、
//     由对齐侧按 len 判定回落，krc 层绝不臆测配对/补齐。
//
// 真机实证（BoA - Only One，2026-09）：62/62 行 len(RomaWords)==len(Detailed.Words) 全成立，
// type=0 确为逐字片段（如 멀어져만 가는 그대 → ["摸","咯","叫","满 ","嘎","嫩 ","可","带"]，
// 词间空格并入片段）。本文件用合成 fixture 覆盖该结构与其反例。

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// mkLangTagWords 构造一个 [language:] 头部：type=1 翻译（每行一个片段）+ type=0 音译（原样
// 多片段/行，即字级）。roma 直接作为 type=0 的 lyricContent（[][]string），不做包裹——这正是
// 与 mkLangTag（每行强制包成单片段）的区别，用来喂字级形态。传 nil 表示不放该轨。
func mkLangTagWords(trans []string, roma [][]string) string {
	type block struct {
		LyricContent [][]string `json:"lyricContent"`
		Type         int        `json:"type"`
	}
	var doc struct {
		Content []block `json:"content"`
		Version int     `json:"version"`
	}
	doc.Version = 1
	if trans != nil {
		wrapped := make([][]string, len(trans))
		for i, s := range trans {
			wrapped[i] = []string{s}
		}
		doc.Content = append(doc.Content, block{LyricContent: wrapped, Type: 1})
	}
	if roma != nil {
		doc.Content = append(doc.Content, block{LyricContent: roma, Type: 0})
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return "[language:" + base64.StdEncoding.EncodeToString(b) + "]"
}

// TestParsePlainKRCRomaWordsAligned —— 字级：RomaWords 按行号对齐、逐字存原始片段，且与 Words
// 一一对应（len 相等）；同一份 KRC 上行级 RomaText 仍是 TrimSpace(Join) 不变（共存回归）。
func TestParsePlainKRCRomaWordsAligned(t *testing.T) {
	roma := [][]string{
		{"kimi ", "da ", "ke ", "wo "}, // 行0：君だけを（4 字 4 片段）
		{"mi ", "tsu ", "me "},         // 行1：見つめ（3 字 3 片段）
	}
	krcStr := mkLangTagWords(nil, roma) + "\n" +
		"[0,1000]<0,250,0>君<250,250,0>だ<500,250,0>け<750,250,0>を\n" +
		"[1000,1000]<0,333,0>見<333,333,0>つ<666,334,0>め\n"
	lines := ParsePlainKRC(krcStr)
	if len(lines) != 2 {
		t.Fatalf("行数=%d, want 2", len(lines))
	}
	// 字级：RomaWords 原样（未 Join、未 Trim），逐字与 Words 一一对应。
	if !equalStrs(lines[0].RomaWords, roma[0]) {
		t.Fatalf("行0 RomaWords=%#v, want %#v（应原样存逐字片段）", lines[0].RomaWords, roma[0])
	}
	if !equalStrs(lines[1].RomaWords, roma[1]) {
		t.Fatalf("行1 RomaWords=%#v, want %#v", lines[1].RomaWords, roma[1])
	}
	// ★ 不变量：每行片段数 == 字数（真机 62/62 成立）。
	for i := range lines {
		if len(lines[i].RomaWords) != len(lines[i].Detailed.Words) {
			t.Fatalf("行%d 片段数 %d != 字数 %d（一一对应不变量破了）", i, len(lines[i].RomaWords), len(lines[i].Detailed.Words))
		}
	}
	// ★ 回归：行级 RomaText 仍是 TrimSpace(Join)，加了字级字段后逐字节不变（kugou 只读它）。
	if lines[0].RomaText != "kimi da ke wo" {
		t.Fatalf("行0 RomaText=%q, want %q（行级 Join 被字级改动波及了？）", lines[0].RomaText, "kimi da ke wo")
	}
	if lines[1].RomaText != "mi tsu me" {
		t.Fatalf("行1 RomaText=%q, want %q", lines[1].RomaText, "mi tsu me")
	}
}

// TestParsePlainKRCRomaWordsLineShapedMismatch —— type=0 是 line-shaped（每行单片段，如
// langTagA / 真机某些行的形态）时，RomaWords 原样存下（len 1），与字数不等 → 对齐侧据此回落。
// krc 层**绝不**为了凑数把单片段拆成逐字，也不补齐：忠实解析、把配对与否的判定留给上层。
func TestParsePlainKRCRomaWordsLineShapedMismatch(t *testing.T) {
	roma := [][]string{
		{"kimidakewo"},         // 行0：1 片段，但主歌词 4 字 → 不等
		{"mi ", "tsu ", "me "}, // 行1：3 片段 == 3 字 → 相等
	}
	krcStr := mkLangTagWords(nil, roma) + "\n" +
		"[0,1000]<0,250,0>君<250,250,0>だ<500,250,0>け<750,250,0>を\n" +
		"[1000,1000]<0,333,0>見<333,333,0>つ<666,334,0>め\n"
	lines := ParsePlainKRC(krcStr)
	if len(lines) != 2 {
		t.Fatalf("行数=%d, want 2", len(lines))
	}
	if !equalStrs(lines[0].RomaWords, []string{"kimidakewo"}) {
		t.Fatalf("行0 RomaWords=%#v, want [kimidakewo]（应原样单片段，不拆）", lines[0].RomaWords)
	}
	if len(lines[0].RomaWords) == len(lines[0].Detailed.Words) {
		t.Fatalf("行0 片段数不应等于字数（line-shaped 应触发不等，交给对齐侧回落）")
	}
	// 行级 RomaText 照常可用（供行级回落层），不受字级 len 不等影响。
	if lines[0].RomaText != "kimidakewo" {
		t.Fatalf("行0 RomaText=%q, want kimidakewo", lines[0].RomaText)
	}
	// 行1 相等，正常逐字。
	if len(lines[1].RomaWords) != len(lines[1].Detailed.Words) {
		t.Fatalf("行1 片段数应 == 字数")
	}
}

// TestParsePlainKRCRomaWordsSkipsBlankLineWithoutMisalign —— ★ 承重：主歌词里夹一个 0 词行
// （被跳过、不产 Line），但它在 type=0 轨里照样占一格。rawIdx 必须为它自增，否则其后 RomaWords
// 整轨错位一格。与 SubText/RomaText 同口径（romanization_test.go / translation_test.go 已测
// 行级，此处补字级）。
func TestParsePlainKRCRomaWordsSkipsBlankLineWithoutMisalign(t *testing.T) {
	roma := [][]string{
		{"A"},          // rawIdx0 甲
		{"B"},          // rawIdx1 乙
		{"C"},          // rawIdx2 丙
		{"SKIP"},       // rawIdx3 0 词空行占的一格——绝不能被后面的行取走
		{"D1 ", "D2 "}, // rawIdx4 大胆
		{"E1 ", "E2 "}, // rawIdx5 磊落
	}
	krcStr := mkLangTagWords(nil, roma) + "\n" +
		"[0,1000]<0,500,0>甲\n" + // rawIdx0
		"[1000,1000]<0,500,0>乙\n" + // rawIdx1
		"[2000,1000]<0,500,0>丙\n" + // rawIdx2
		"[3000,1000]\n" + // rawIdx3：0 词空行 → 跳过，占轨一格
		"[4000,1000]<0,500,0>大<500,500,0>胆\n" + // rawIdx4
		"[5000,1000]<0,500,0>磊<500,500,0>落\n" // rawIdx5
	lines := ParsePlainKRC(krcStr)
	if len(lines) != 5 {
		t.Fatalf("产出行数=%d, want 5（0 词行应被跳过）", len(lines))
	}
	// 关键：尽管中间跳了一行，大胆/磊落 仍拿到 rawIdx4/5 的逐字片段，而非错位的 rawIdx3/4。
	if lines[3].Text != "大胆" || !equalStrs(lines[3].RomaWords, []string{"D1 ", "D2 "}) {
		t.Fatalf("字级对齐错位！行3 text=%q RomaWords=%#v, want 大胆/[D1  D2 ]", lines[3].Text, lines[3].RomaWords)
	}
	if lines[4].Text != "磊落" || !equalStrs(lines[4].RomaWords, []string{"E1 ", "E2 "}) {
		t.Fatalf("字级对齐错位！行4 text=%q RomaWords=%#v, want 磊落/[E1  E2 ]", lines[4].Text, lines[4].RomaWords)
	}
}

// TestParsePlainKRCRomaWordsAbsent —— 无 [language:] / 无 type=0 轨 → RomaWords 全 nil，
// 与 RomaText 全空并存，不报错、不强塞。
func TestParsePlainKRCRomaWordsAbsent(t *testing.T) {
	lines := ParsePlainKRC("[0,1000]<0,500,0>甲<500,500,0>乙\n")
	if len(lines) != 1 {
		t.Fatalf("行数=%d, want 1", len(lines))
	}
	if lines[0].RomaWords != nil {
		t.Fatalf("无 [language:] 时 RomaWords 应为 nil，got %#v", lines[0].RomaWords)
	}
	if lines[0].RomaText != "" {
		t.Fatalf("无 [language:] 时 RomaText 应为空，got %q", lines[0].RomaText)
	}
	// 仅 type=1（无 type=0）：RomaWords 仍 nil。langTagB 只含 type=1（见 translation_test.go）。
	only1 := ParsePlainKRC(langTagB + "\n[0,1000]<0,500,0>甲\n")
	if only1[0].RomaWords != nil {
		t.Fatalf("仅 type=1 时 RomaWords 应 nil，got %#v", only1[0].RomaWords)
	}
}

// TestParseRomanizationWordsVsLineLevelSameSource —— 字级 parseRomanizationWords 与行级
// parseRomanization 同源同口径：前者 Join 后即后者（逐行）。钉死「一个未 Join、一个 Join、
// 内容一致」，防日后两条轨取值分叉。
func TestParseRomanizationWordsVsLineLevelSameSource(t *testing.T) {
	roma := [][]string{{"ro", "ma"}, {"bo", "ku"}}
	tag := mkLangTagWords(nil, roma)
	words := parseRomanizationWords(tag)
	line := parseRomanization(tag)
	if len(words) != 2 || len(line) != 2 {
		t.Fatalf("字级 %d 行 / 行级 %d 行, want 各 2", len(words), len(line))
	}
	for i := range words {
		joined := ""
		for _, f := range words[i] {
			joined += f
		}
		if joined != line[i] {
			t.Fatalf("行%d：字级 Join=%q != 行级=%q（两轨分叉了）", i, joined, line[i])
		}
	}
}
