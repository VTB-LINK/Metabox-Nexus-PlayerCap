package sodamusic

// 本文件测「从酷狗借音译」的文本对齐核心 mergeKugouRoma。
//
// 要害两条（承重不变量，AGENTS §2）：
//   1. 对齐**只按归一化主歌词文本**——命中即两行是同一句词、发音标注天然通用；对不上就留空。
//      绝不按时间就近借（跨平台时间轴不可靠，会把 A 行的音译贴到文本不同的 B 行上）。
//   2. **只动 RomaText**，主歌词 Text / 翻译 SubText / 逐字 Detailed 一字不改；失败/未命中/
//      酷狗返回 nil 一律安全降级为「该行留空」，绝不 panic、绝不波及主歌词与翻译。

import (
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// ★ 核心：按归一化文本对齐把酷狗音译搬进 RomaText，标点/大小写差异经 NormalizeLyricText 抹平后
// 仍命中；酷狗没有的行留空。同时验证主歌词 Text 与翻译 SubText 一字未动。
//
// 变异自证：把 mergeKugouRoma 里 romaByText 的键从 NormalizeLyricText(kl.Text) 改成 kl.Text
// 原文，行0（"Only one" vs "only, one!"）当场对不上而红。
func TestMergeKugouRomaTextAlign(t *testing.T) {
	soda := []krc.Line{
		{Index: 0, Time: 10.0, Text: "Only one", SubText: "唯一"},
		{Index: 1, Time: 20.0, Text: "Kimi dake wo", SubText: "只有你"},
		{Index: 2, Time: 30.0, Text: "line soda has but kugou lacks"},
	}
	kugou := []klyric.Line{
		{Text: "only, one!", RomaText: "オンリーワン"},    // 标点/大小写不同，归一后 = 汽水行0
		{Text: "Kimi Dake Wo", RomaText: "きみ だけ を"}, // 大小写不同，归一后 = 汽水行1
		{Text: "totally different line", RomaText: "should-not-leak"},
	}
	matched := mergeKugouRoma(soda, kugou)
	if matched != 2 {
		t.Fatalf("matched = %d, want 2", matched)
	}
	if soda[0].RomaText != "オンリーワン" {
		t.Fatalf("行0 RomaText = %q，标点/大小写归一后应命中", soda[0].RomaText)
	}
	if soda[1].RomaText != "きみ だけ を" {
		t.Fatalf("行1 RomaText = %q", soda[1].RomaText)
	}
	if soda[2].RomaText != "" {
		t.Fatalf("行2 酷狗无对应文本，RomaText 应留空，got %q（音译串不该外泄）", soda[2].RomaText)
	}
	// 承重：只动 RomaText，主歌词与翻译一字不改。
	if soda[0].Text != "Only one" || soda[0].SubText != "唯一" {
		t.Fatalf("行0 主歌词/翻译被污染：Text=%q SubText=%q", soda[0].Text, soda[0].SubText)
	}
	if soda[1].Text != "Kimi dake wo" || soda[1].SubText != "只有你" {
		t.Fatalf("行1 主歌词/翻译被污染：Text=%q SubText=%q", soda[1].Text, soda[1].SubText)
	}
}

// 未命中（酷狗曲库是另一版本/母带/翻唱，文本对不上）→ 该行留空，且主歌词 Text / 翻译 SubText /
// 逐字 Detailed 全部不动。这是「借不到绝不波及主歌词」的最小证据。
func TestMergeKugouRomaNoMatchLeavesMainUntouched(t *testing.T) {
	soda := []krc.Line{{
		Index: 0, Time: 10.0, Text: "汽水专属这一句", SubText: "译文",
		Detailed: player.LyricTextDetailed{
			Timestamp: 10.0,
			Words:     []player.LyricTextDetailedWord{{Text: "汽水", Timestamp: 10.0}},
		},
	}}
	kugou := []klyric.Line{{Text: "完全不同的词", RomaText: "buyao"}}
	if matched := mergeKugouRoma(soda, kugou); matched != 0 {
		t.Fatalf("matched = %d, want 0", matched)
	}
	if soda[0].RomaText != "" {
		t.Fatalf("无对齐，RomaText 应留空，got %q", soda[0].RomaText)
	}
	if soda[0].Text != "汽水专属这一句" || soda[0].SubText != "译文" {
		t.Fatalf("主歌词/翻译被动了：Text=%q SubText=%q", soda[0].Text, soda[0].SubText)
	}
	if len(soda[0].Detailed.Words) != 1 || soda[0].Detailed.Words[0].Text != "汽水" {
		t.Fatalf("逐字轨被动了：%+v", soda[0].Detailed)
	}
}

// 酷狗此曲有匹配文本但**无 type=0 音译轨**（RomaText 空）→ 不算命中、不写空串占位。
func TestMergeKugouRomaNoRomaTrack(t *testing.T) {
	soda := []krc.Line{{Index: 0, Time: 1.0, Text: "same text"}}
	kugou := []klyric.Line{{Text: "same text", RomaText: ""}}
	if matched := mergeKugouRoma(soda, kugou); matched != 0 {
		t.Fatalf("matched = %d, want 0（酷狗无音译不应算命中）", matched)
	}
	if soda[0].RomaText != "" {
		t.Fatalf("RomaText 应留空，got %q", soda[0].RomaText)
	}
}

// 边界安全：空 soda / 酷狗返回 nil / 双 nil 都不 panic，且不产生对齐。
func TestMergeKugouRomaNilSafe(t *testing.T) {
	if matched := mergeKugouRoma(nil, []klyric.Line{{Text: "x", RomaText: "y"}}); matched != 0 {
		t.Fatalf("空 soda：matched = %d, want 0", matched)
	}
	soda := []krc.Line{{Index: 0, Time: 1.0, Text: "x"}}
	if matched := mergeKugouRoma(soda, nil); matched != 0 {
		t.Fatalf("酷狗 nil：matched = %d, want 0", matched)
	}
	if soda[0].RomaText != "" {
		t.Fatalf("酷狗 nil 时 RomaText 应留空，got %q", soda[0].RomaText)
	}
	_ = mergeKugouRoma(nil, nil) // 双 nil 不 panic 即可
}

// 副歌重复行（文本相同、大小写不同）→ 两次都对齐同一音译。map 键去重，多次命中互不影响。
func TestMergeKugouRomaRepeatedChorus(t *testing.T) {
	soda := []krc.Line{
		{Index: 0, Time: 10, Text: "chorus line"},
		{Index: 1, Time: 20, Text: "verse"},
		{Index: 2, Time: 30, Text: "Chorus Line"}, // 副歌重复
	}
	kugou := []klyric.Line{
		{Text: "chorus line", RomaText: "R1"},
		{Text: "verse", RomaText: "R2"},
	}
	if matched := mergeKugouRoma(soda, kugou); matched != 3 {
		t.Fatalf("matched = %d, want 3（副歌两次都应对齐）", matched)
	}
	if soda[0].RomaText != "R1" || soda[2].RomaText != "R1" {
		t.Fatalf("副歌重复行音译应一致：行0=%q 行2=%q", soda[0].RomaText, soda[2].RomaText)
	}
	if soda[1].RomaText != "R2" {
		t.Fatalf("行1 RomaText = %q, want R2", soda[1].RomaText)
	}
}

// —— 字级对齐（跨断行差异 M:N）测试 ————————————————————————————————————————
//
// 要害（承重不变量，AGENTS §2 / 任务硬约束）：
//   1. 字级对齐把酷狗**保留到字**的音译按字序贴到汽水主歌词的字上，再按**汽水行边界**重拼行级
//      RomaText——覆盖行级文本匹配对不上的断行差异段（酷狗 2 行 vs 汽水 3 行等 M:N）。
//   2. **分层不劣化**：字级优先，字级没覆盖的行回落**现有**行级归一化匹配，仍不行留空。合并结果
//      对任何行都 ⊇ 纯行级结果；重合行两层同值、不冲突。
//   3. **只动 RomaText**：主歌词 Text / 翻译 SubText / 逐字 Detailed 一字不改；酷狗无字级音译、
//      token 跨行、字没覆盖全一律安全降级，绝不 panic、绝不产出缺字/串行的音译。

// kugouCharLine 造一条**字级可靠**的酷狗行：Detailed.Words 逐字、RomaWords 逐字音译片段、
// 行级 RomaText = TrimSpace(Join(roma))（模拟 krc.ParsePlainKRC 的产物）。chars 与 roma 等长。
func kugouCharLine(chars, roma []string) klyric.Line {
	words := make([]player.LyricTextDetailedWord, len(chars))
	for i, c := range chars {
		words[i] = player.LyricTextDetailedWord{Text: c}
	}
	return krc.Line{
		Text:      strings.Join(chars, ""),
		RomaText:  strings.TrimSpace(strings.Join(roma, "")),
		RomaWords: roma,
		Detailed:  player.LyricTextDetailed{Words: words},
	}
}

// kugouLineOnly 造一条**只有行级音译**的酷狗行（老 KRC / LRC 回落：有 RomaText、无逐字、无 RomaWords）。
func kugouLineOnly(text, roma string) klyric.Line {
	return krc.Line{Text: text, RomaText: roma}
}

// sodaMainLine 造一条汽水主歌词行：只有主歌词（逐字 Words + Text），无音译（汽水平台无音译源）。
func sodaMainLine(idx int, chars []string) krc.Line {
	words := make([]player.LyricTextDetailedWord, len(chars))
	for i, c := range chars {
		words[i] = player.LyricTextDetailedWord{Text: c}
	}
	return krc.Line{Index: idx, Text: strings.Join(chars, ""), Detailed: player.LyricTextDetailed{Words: words}}
}

// ★ 核心：酷狗 2 行 vs 汽水 3 行（M:N 断行差异）。主歌词逐字相同、只断行不同，纯行级文本匹配
// 一行都对不上（现状会全留空），字级对齐按字序重拼后 3 行全中。全字序：君だけを見つめてる。
// 变异自证：把 alignCharLevel 的接受判据 `lineAligned[li] != total` 去掉（不要求全覆盖），
// 汽水行会拿到缺字/串行的音译；把字级层整个短路（return nil），本例 matched 归 0 而红。
func TestMergeKugouRomaCharLevelMN(t *testing.T) {
	kugou := []klyric.Line{
		kugouCharLine([]string{"君", "だ", "け", "を", "見"}, []string{"kimi ", "da ", "ke ", "wo ", "mi "}),
		kugouCharLine([]string{"つ", "め", "て", "る"}, []string{"tsu ", "me ", "te ", "ru "}),
	}
	soda := []krc.Line{
		sodaMainLine(0, []string{"君", "だ", "け", "を"}),
		sodaMainLine(1, []string{"見", "つ", "め"}), // 跨酷狗断行（見在酷狗行0末、つめ在酷狗行1）
		sodaMainLine(2, []string{"て", "る"}),
	}
	st := mergeKugouRomaStats(soda, kugou)
	if st.charLines != 3 || st.lineLines != 0 || st.matched != 3 {
		t.Fatalf("stats=%+v, want charLines=3 lineLines=0 matched=3（M:N 应全靠字级重拼）", st)
	}
	want := []string{"kimi da ke wo", "mi tsu me", "te ru"}
	for i, w := range want {
		if soda[i].RomaText != w {
			t.Fatalf("汽水行%d RomaText=%q, want %q（字级重拼错）", i, soda[i].RomaText, w)
		}
	}
	// 纯行级对照：断行不同 → 一行都对不上，坐实字级带来的净增益。
	if lineOnly := mergeKugouRoma(soda3(), lineLevelOnly(kugou)); lineOnly != 0 {
		t.Fatalf("纯行级本应 0 行对齐（断行不同），got %d —— 字级增益的前提不成立", lineOnly)
	}
	// 承重：只动 RomaText，主歌词逐字未变。
	if soda[1].Text != "見つめ" || len(soda[1].Detailed.Words) != 3 || soda[1].Detailed.Words[0].Text != "見" {
		t.Fatalf("汽水行1 主歌词/逐字被污染：Text=%q Detailed=%+v", soda[1].Text, soda[1].Detailed)
	}
}

// soda3 复造上例的三行汽水主歌词（给纯行级对照用，避免复用已被改写的 soda）。
func soda3() []krc.Line {
	return []krc.Line{
		sodaMainLine(0, []string{"君", "だ", "け", "を"}),
		sodaMainLine(1, []string{"見", "つ", "め"}),
		sodaMainLine(2, []string{"て", "る"}),
	}
}

// lineLevelOnly 把酷狗行降级成「只有行级音译」形态（抹掉逐字与 RomaWords），模拟老 KRC，用于
// 证明**纯行级**层对 M:N 断行无能为力。
func lineLevelOnly(in []klyric.Line) []klyric.Line {
	out := make([]klyric.Line, len(in))
	for i, l := range in {
		out[i] = krc.Line{Text: l.Text, RomaText: l.RomaText}
	}
	return out
}

// 分层降级：字级覆盖的行走字级、字级覆盖不到的行回落行级归一化匹配。构造酷狗前两行字级可靠、
// 第三行只有行级音译；汽水第三行的字不在酷狗字序里 → 字级不覆盖 → 回落行级命中。
func TestMergeKugouRomaCharThenLineFallback(t *testing.T) {
	kugou := []klyric.Line{
		kugouCharLine([]string{"君", "だ", "け", "を", "見"}, []string{"kimi ", "da ", "ke ", "wo ", "mi "}),
		kugouCharLine([]string{"つ", "め", "て", "る"}, []string{"tsu ", "me ", "te ", "ru "}),
		kugouLineOnly("特殊な行", "toku"), // 只有行级音译，无逐字
	}
	soda := []krc.Line{
		sodaMainLine(0, []string{"君", "だ", "け", "を"}),      // 字级
		sodaMainLine(1, []string{"見", "つ", "め", "て", "る"}), // 字级（跨酷狗断行）
		sodaMainLine(2, []string{"特", "殊", "な", "行"}),      // 字级无从覆盖 → 回落行级 → toku
	}
	st := mergeKugouRomaStats(soda, kugou)
	if st.charLines != 2 || st.lineLines != 1 || st.matched != 3 {
		t.Fatalf("stats=%+v, want charLines=2 lineLines=1 matched=3（分层降级）", st)
	}
	want := []string{"kimi da ke wo", "mi tsu me te ru", "toku"}
	for i, w := range want {
		if soda[i].RomaText != w {
			t.Fatalf("汽水行%d RomaText=%q, want %q", i, soda[i].RomaText, w)
		}
	}
}

// 无字级音译安全：酷狗只给行级音译（老 KRC / LRC 回落，无 RomaWords）→ 字级层空转、整体退回
// **纯行级** = 引入本改动前的现状。既证安全、也证不劣化（行级能填的仍照填）。
func TestMergeKugouRomaNoCharRomaFallsBackToLine(t *testing.T) {
	kugou := []klyric.Line{kugouLineOnly("君だけを", "kimi dake wo")}
	soda := []krc.Line{sodaMainLine(0, []string{"君", "だ", "け", "を"})}
	// 字级层单独看：无逐字音译 → 返回 nil。
	if got := alignCharLevel(soda, kugou); len(got) != 0 {
		t.Fatalf("酷狗无字级音译时 alignCharLevel 应空，got %#v", got)
	}
	st := mergeKugouRomaStats(soda, kugou)
	if st.charLines != 0 || st.lineLines != 1 || st.matched != 1 {
		t.Fatalf("stats=%+v, want charLines=0 lineLines=1 matched=1（退回纯行级）", st)
	}
	if soda[0].RomaText != "kimi dake wo" {
		t.Fatalf("行级回落 RomaText=%q, want kimi dake wo", soda[0].RomaText)
	}
}

// 安全：酷狗一个 2 字 token「だけ」被汽水拆到两行（token 跨行 straddle）→ 两行都拒绝字级
// （否则同一音译会重复贴到相邻两行），交回落 / 留空。这是「宁缺毋错」的字级侧闸。
func TestAlignCharLevelRejectsStraddle(t *testing.T) {
	kugou := []klyric.Line{
		kugouCharLine([]string{"君", "だけ", "を"}, []string{"kimi ", "dake ", "wo "}), // 「だけ」是一个 2 字 token
	}
	soda := []krc.Line{
		sodaMainLine(0, []string{"君", "だ"}), // だ 属 token「だけ」的前半
		sodaMainLine(1, []string{"け", "を"}), // け 属 token「だけ」的后半 → 该 token 跨了汽水两行
	}
	got := alignCharLevel(soda, kugou)
	if len(got) != 0 {
		t.Fatalf("token 跨行应两行都拒绝字级，got %#v（会把 dake 重复贴两行）", got)
	}
}

// 不劣化地基：断行一致的重合行，字级重拼与行级匹配**必得同值**（都 = 该酷狗行片段 Join），
// 两层不冲突。保证「字级优先」不会把一个行级本来能正确填的行改填成别的值。
func TestMergeKugouRomaCoincidentAgreesWithLine(t *testing.T) {
	chars := []string{"君", "だ", "け", "を"}
	roma := []string{"kimi ", "da ", "ke ", "wo "}
	kugou := []klyric.Line{kugouCharLine(chars, roma)}
	soda := []krc.Line{sodaMainLine(0, chars)} // 断行与酷狗完全一致
	st := mergeKugouRomaStats(soda, kugou)
	if st.matched != 1 || st.charLines != 1 {
		t.Fatalf("stats=%+v, want matched=1 charLines=1（重合行由字级填）", st)
	}
	if soda[0].RomaText != kugou[0].RomaText {
		t.Fatalf("重合行字级重拼=%q 应 == 酷狗行级 RomaText=%q（分层不该分叉）", soda[0].RomaText, kugou[0].RomaText)
	}
}

// —— 小书写体假名折叠（normLyricRune）测试 ————————————————————————————————————
//
// 要害（改动1，宁缺毋错）：
//   1. **小书写体假名 = 同音的大小写变体**，字级对齐前折叠到大书写体，救回汽水异体写法整行。
//   2. **清音 / 浊音 / 半浊音是不同音**，绝不折叠——字级对不上、行级也不等，该行留空。
//   3. 映射表逐对自证 + 清浊音原样 + 生产表与 oracle 双向核对，任何清浊音混入映射即红。

// 改动1（救回）：汽水 サィレンス（小 ィ U+30A3）与酷狗 サイレンス（大 イ U+30A4）同音、仅书写体
// 大小不同 → normLyricRune 折叠后字级全对齐、音译救回（改动前 ィ≠イ 整行对不上而留空）。
func TestAlignCharLevelSmallKanaRescued(t *testing.T) {
	kugou := []klyric.Line{
		kugouCharLine([]string{"サ", "イ", "レ", "ン", "ス"}, []string{"sa ", "i ", "re ", "n ", "su "}),
	}
	soda := []krc.Line{
		sodaMainLine(0, []string{"サ", "ィ", "レ", "ン", "ス"}), // 小 ィ，与酷狗同音异写
	}
	st := mergeKugouRomaStats(soda, kugou)
	if st.charLines != 1 || st.matched != 1 {
		t.Fatalf("stats=%+v, want charLines=1 matched=1（小写假名折叠后字级全对齐）", st)
	}
	if soda[0].RomaText != "sa i re n su" {
		t.Fatalf("小写假名救回失败：RomaText=%q, want %q", soda[0].RomaText, "sa i re n su")
	}
}

// 改动1（留空）：清音/浊音、异体差异**绝不**折叠 → 字级对不上、行级也不等 → 该行留空。实测
// Aimer - ninelie：汽水 並ふ / イメーヅ vs 酷狗 並ぶ / イメージ（ふ↔ぶ 浊点、ヅ↔ジ 异体，均不同
// 音），若归一化会把不同音的字误配，故宁缺毋错保持留空。
func TestAlignCharLevelVoicedKanaNotNormalized(t *testing.T) {
	kugou := []klyric.Line{
		kugouCharLine([]string{"並", "ぶ"}, []string{"nara ", "bu "}),
		kugouCharLine([]string{"イ", "メ", "ー", "ジ"}, []string{"i ", "me ", "- ", "ji "}),
	}
	soda := []krc.Line{
		sodaMainLine(0, []string{"並", "ふ"}),           // ふ vs 酷狗 ぶ（浊点，不同音）
		sodaMainLine(1, []string{"イ", "メ", "ー", "ヅ"}), // ヅ vs 酷狗 ジ（异体，不同音）
	}
	st := mergeKugouRomaStats(soda, kugou)
	if st.matched != 0 {
		t.Fatalf("stats=%+v, want matched=0（清浊/异体不折叠，字级+行级都不该命中）", st)
	}
	if soda[0].RomaText != "" || soda[1].RomaText != "" {
		t.Fatalf("清浊/异体差异行应留空：行0=%q 行1=%q", soda[0].RomaText, soda[1].RomaText)
	}
}

// 映射表逐对自证 + 变异闸。small2large 是**独立手写的 oracle**（与 smallToLargeKana 分开维护，
// 交叉核对）：① 每个小写假名经 normLyricRune 折叠到 oracle 指定的大写体、大写体本身归一化恒等；
// ② 抽查关键映射的 Unicode **码点**（独立于字形渲染，防近形误输入把浊音写进表，尤其 tsu 系）；
// ③ 生产表与 oracle **双向逐对相等**（生产表多一对/少一对/改一对即红）；④ 清音/浊音/半浊音经
// normLyricRune **原样不变**。**变异自证**：把任一清浊音（如 づ→つ）纳入 smallToLargeKana，②③④
// 至少一处即红。
func TestNormLyricRuneKanaFold(t *testing.T) {
	// 独立手写 oracle：小书写体 → 大书写体（**只收同音大小写变体，绝无清浊音**）。
	small2large := map[rune]rune{
		'ぁ': 'あ', 'ぃ': 'い', 'ぅ': 'う', 'ぇ': 'え', 'ぉ': 'お',
		'っ': 'つ',
		'ゃ': 'や', 'ゅ': 'ゆ', 'ょ': 'よ',
		'ゎ': 'わ',
		'ゕ': 'か', 'ゖ': 'け',
		'ァ': 'ア', 'ィ': 'イ', 'ゥ': 'ウ', 'ェ': 'エ', 'ォ': 'オ',
		'ッ': 'ツ',
		'ャ': 'ヤ', 'ュ': 'ユ', 'ョ': 'ヨ',
		'ヮ': 'ワ',
		'ヵ': 'カ', 'ヶ': 'ケ',
	}

	// ① 逐对：小写折叠到大写；大写体归一化恒等（幂等）。
	for small, large := range small2large {
		if got, ok := normLyricRune(small); !ok || got != large {
			t.Fatalf("normLyricRune(%q)=(%q,%v), want (%q,true)（小写假名应折叠到大写）", small, got, ok, large)
		}
		if got, ok := normLyricRune(large); !ok || got != large {
			t.Fatalf("normLyricRune(%q)=(%q,%v), want (%q,true)（大写体应恒等）", large, got, ok, large)
		}
	}

	// ② 码点抽查（十进制/十六进制不受字形迷惑）：tsu 系必须映到 U+3064/U+30C4，绝不是浊音
	//    U+3065(づ)/U+30C5(ヅ)；另各组取代表核对一对。
	cp := []struct{ small, large rune }{
		{0x3041, 0x3042}, // ぁ→あ
		{0x3063, 0x3064}, // っ→つ（NOT づ U+3065）
		{0x3083, 0x3084}, // ゃ→や
		{0x308E, 0x308F}, // ゎ→わ
		{0x3095, 0x304B}, // ゕ→か
		{0x3096, 0x3051}, // ゖ→け
		{0x30A1, 0x30A2}, // ァ→ア
		{0x30C3, 0x30C4}, // ッ→ツ（NOT ヅ U+30C5）
		{0x30E3, 0x30E4}, // ャ→ヤ
		{0x30F5, 0x30AB}, // ヵ→カ
		{0x30F6, 0x30B1}, // ヶ→ケ
	}
	for _, c := range cp {
		if got, ok := smallToLargeKana[c.small]; !ok || got != c.large {
			t.Fatalf("码点核对 U+%04X → 期望 U+%04X，实际 (%U,%v)", c.small, c.large, got, ok)
		}
	}

	// ③ 生产表与 oracle 双向逐对相等（防偷加清浊音映射 / 改错目标）。
	if len(smallToLargeKana) != len(small2large) {
		t.Fatalf("smallToLargeKana 有 %d 对、oracle 有 %d 对——增删过，请核对是否混入清浊音",
			len(smallToLargeKana), len(small2large))
	}
	for k, v := range smallToLargeKana {
		if want, ok := small2large[k]; !ok || want != v {
			t.Fatalf("smallToLargeKana[%q]=%q 不在 oracle 或不一致（疑似混入非小写假名映射）", k, v)
		}
	}

	// ④ 清音/浊音/半浊音绝不折叠：经 normLyricRune 原样返回（假名 ToLower 恒等）。成对/成组各
	//    自不同音，任一被折叠都会误配。这一段就是「若把清浊音纳入映射即红」的变异自证。
	voiced := []rune{
		'ふ', 'ぶ', 'ぷ', // ha 行：清 / 浊 / 半浊
		'は', 'ば', 'ぱ',
		'つ', 'づ', // tsu / zu（づ 绝不等于 つ）
		'し', 'じ',
		'ツ', 'ヅ', 'ジ', // 片假名同理：ヅ、ジ 绝不折叠成 ツ
		'か', 'が', // ka / ga
	}
	for _, r := range voiced {
		if got, ok := normLyricRune(r); !ok || got != r {
			t.Fatalf("normLyricRune(%q)=(%q,%v), want (%q,true)（清浊/半浊音绝不折叠，应原样）", r, got, ok, r)
		}
	}
}

// —— 对抗审查补强：全局 LCS 单调性守护（多同形字来源行不窜位）————————————————————
//
// 背景：一轮对抗审查提出 candidate——alignCharLevel 用的是跨**所有**酷狗行的全局 rune LCS，接受
// 判据（full coverage + no straddle）只约束「token 不跨汽水行」、**不约束 token 来自哪个酷狗
// 来源行」，担心某汽水行的字被对齐到别处含同形字的另一行、重拼出错来源音译。该 candidate 经
// 证伪：LCS 是**单调**的——两侧内容一致时，重复/同形字被上下文钉在其顺序位，绝不会窜到更靠前
// 的同形字。既有 TestMergeKugouRomaCoincidentAgreesWithLine 只覆盖「汽水行==酷狗唯一一行、无
// 其它同形来源」的孤立场景，没固化「多同形字来源行按顺序各归各」。下面两例把这条隐含保证钉死，
// 防止日后有人改对齐算法（换成贪心/就近等破坏单调性的做法）而无声破坏它。

// 同形字多来源行：酷狗两行都含「了」但音译不同（行0 = liao、行1 = le），LCS 单调保证汽水里
// 后出现的「了」对到**后面**的来源行、绝不窜到前面同形字的来源。这正是对抗审查点名的「长行在前
// （了解）+ 短重复行在后（了）+ 同形字」场景。
//
// 变异自证：把 alignCharLevel 的全局 LCS 换成按字贪心/就近（破坏单调），汽水行1 的「了」会窜到
// 酷狗行0 的「了」(liao)、得错误来源音译；单调 LCS 保证它对到酷狗行1 的「了」(le)。
func TestAlignCharLevelHomographMonotonic(t *testing.T) {
	kugou := []klyric.Line{
		kugouCharLine([]string{"了", "解"}, []string{"liao ", "jie "}), // 行0：了 = liao
		kugouCharLine([]string{"了"}, []string{"le "}),                // 行1：同形字「了」= le
	}
	soda := []krc.Line{
		sodaMainLine(0, []string{"了", "解"}),
		sodaMainLine(1, []string{"了"}),
	}
	st := mergeKugouRomaStats(soda, kugou)
	if st.charLines != 2 || st.matched != 2 {
		t.Fatalf("stats=%+v, want charLines=2 matched=2", st)
	}
	if soda[0].RomaText != "liao jie" {
		t.Fatalf("汽水行0 RomaText=%q, want %q", soda[0].RomaText, "liao jie")
	}
	if soda[1].RomaText != "le" {
		t.Fatalf("汽水行1 RomaText=%q, want %q（同形字「了」应对到酷狗行1、不窜到行0 的 liao）", soda[1].RomaText, "le")
	}
}

// M:N 断行 + 同形字：酷狗断行与汽水不同，且同形字「あ」落在不同来源行。LCS 单调保证按顺序各归
// 其位——汽水行0 的「あ」取酷狗行0 的 a1、汽水行1 的「あ」取酷狗行1 的 a2，不因同形而互串。
// 同时覆盖「跨酷狗断行重拼」（汽水行1 的 い 来自酷狗行0、あ 来自酷狗行1）。
//
// 变异自证：破坏单调（贪心就近）会让两个「あ」争抢同一来源，重拼出错来源音译。
func TestAlignCharLevelHomographAcrossMN(t *testing.T) {
	kugou := []klyric.Line{
		kugouCharLine([]string{"あ", "い"}, []string{"a1 ", "i "}), // 行0：あ = a1
		kugouCharLine([]string{"あ"}, []string{"a2 "}),            // 行1：同形字「あ」= a2
	}
	soda := []krc.Line{
		sodaMainLine(0, []string{"あ"}),      // 取酷狗行0 的 あ → a1
		sodaMainLine(1, []string{"い", "あ"}), // い 取酷狗行0、あ 取酷狗行1 → i a2
	}
	st := mergeKugouRomaStats(soda, kugou)
	if st.charLines != 2 || st.matched != 2 {
		t.Fatalf("stats=%+v, want charLines=2 matched=2", st)
	}
	if soda[0].RomaText != "a1" {
		t.Fatalf("汽水行0 RomaText=%q, want a1（同形字「あ」取酷狗行0）", soda[0].RomaText)
	}
	if soda[1].RomaText != "i a2" {
		t.Fatalf("汽水行1 RomaText=%q, want %q（「あ」取酷狗行1、不窜到行0 的 a1）", soda[1].RomaText, "i a2")
	}
}
