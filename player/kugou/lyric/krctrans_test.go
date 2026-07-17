package lyric

// 本文件只测酷狗的第二行歌词（翻译）：parseKRCTranslation 解 [language:] 轨，以及
// parseKRC 把译文按 [start,dur] 行号对齐进 Line.SubText。
//
// 与网易云的翻译**机制不同**：网易云是独立 tlyric LRC、按毫秒时间戳匹配；酷狗是同一份 KRC
// 内嵌、按行号位置对齐。最容易做错的是对齐——遇到被跳过的 0 词行若不占索引就整轨错位。

import "testing"

// langTagA：type=1 译轨 5 行 ["","","","英勇无畏 维新革命","光明磊落反战国家"] + 一条 type=0 罗马音干扰轨。
// 由真实 [language:] JSON 结构 base64 而来（见 scratchpad 生成脚本）。
const langTagA = "[language:eyJjb250ZW50IjogW3sibGFuZ3VhZ2UiOiAwLCAibHlyaWNDb250ZW50IjogW1siIl0sIFsiIl0sIFsiIl0sIFsi6Iux5YuH5peg55WPIOe7tOaWsOmdqeWRvSJdLCBbIuWFieaYjuejiuiQveWPjeaImOWbveWutiJdXSwgInR5cGUiOiAxfSwgeyJsYW5ndWFnZSI6IDAsICJseXJpY0NvbnRlbnQiOiBbWyJzZSBuICJdLCBbInNoaSAiXSwgWyJreW8gIl0sIFsiZGEgaSAiXSwgWyJyYSBpICJdXSwgInR5cGUiOiAwfV0sICJ2ZXJzaW9uIjogMX0=]"

// TestParseKRCTranslationTakesType1 —— 只取中文翻译轨(type=1)，绝不误取罗马音(type=0)。
// 变异自证：把 parseKRCTranslation 里的 `blk.Type != 1` 改成 `!= 0`，本例会拿到 "se n" 而红。
func TestParseKRCTranslationTakesType1(t *testing.T) {
	tr := parseKRCTranslation(langTagA + "\n[0,1000]<0,500,0>x\n")
	want := []string{"", "", "", "英勇无畏 维新革命", "光明磊落反战国家"}
	if len(tr) != len(want) {
		t.Fatalf("译轨行数=%d, want %d（是否误取了 type=0 罗马音轨？）", len(tr), len(want))
	}
	for i := range want {
		if tr[i] != want[i] {
			t.Fatalf("译轨[%d]=%q, want %q", i, tr[i], want[i])
		}
	}
}

// 无 [language:] 标签 → nil；parseKRC 照常出词、SubText 全空。
func TestParseKRCTranslationAbsent(t *testing.T) {
	if tr := parseKRCTranslation("[ti:x]\n[0,1000]<0,500,0>甲\n"); tr != nil {
		t.Fatalf("无 [language:] 应返回 nil，got %#v", tr)
	}
}

// parseKRC 端到端：译文按 [start,dur] 行号对齐进 SubText。
// 5 个主行：idx0/1/2 头部(译空)，idx3/4 正文(有译)。
func TestParseKRCSubTextAligned(t *testing.T) {
	krc := langTagA + "\n" +
		"[0,1000]<0,500,0>千<500,500,0>本\n" +
		"[1000,1000]<0,500,0>词<500,500,0>：\n" +
		"[2000,1000]<0,500,0>曲<500,500,0>：\n" +
		"[32119,2870]<0,900,0>大<900,900,0>胆\n" +
		"[34989,3180]<0,900,0>磊<900,900,0>落\n"
	lines := parseKRC(krc)
	if len(lines) != 5 {
		t.Fatalf("行数=%d, want 5", len(lines))
	}
	if lines[0].SubText != "" || lines[1].SubText != "" || lines[2].SubText != "" {
		t.Fatalf("头部 3 行译文应为空，got %q/%q/%q", lines[0].SubText, lines[1].SubText, lines[2].SubText)
	}
	if lines[3].Text != "大胆" || lines[3].SubText != "英勇无畏 维新革命" {
		t.Fatalf("行3 = %q/%q, want 大胆/英勇无畏 维新革命", lines[3].Text, lines[3].SubText)
	}
	if lines[4].Text != "磊落" || lines[4].SubText != "光明磊落反战国家" {
		t.Fatalf("行4 = %q/%q, want 磊落/光明磊落反战国家", lines[4].Text, lines[4].SubText)
	}
}

// ★ 承重用例：主歌词里夹一个 0 词行（会被 parseKRC 跳过、不产出 Line），但它在译轨里**照样占一格**。
// rawIdx 必须为它自增，否则其后的译文整轨错位一格。
// 变异自证：把 parseKRC 里的 `rawIdx++` 移到 append 之前的 `if len(words)==0 { continue }` 之后，
// 大胆/磊落 就会分别错拿到 ""/英勇无畏 而红。
const langTagB = "[language:eyJjb250ZW50IjogW3sibGFuZ3VhZ2UiOiAwLCAibHlyaWNDb250ZW50IjogW1siIl0sIFsiIl0sIFsiIl0sIFsiIl0sIFsi6Iux5YuH5peg55WPIOe7tOaWsOmdqeWRvSJdLCBbIuWFieaYjuejiuiQveWPjeaImOWbveWutiJdXSwgInR5cGUiOiAxfV0sICJ2ZXJzaW9uIjogMX0=]"

func TestParseKRCSubTextSkipsBlankLineWithoutMisalign(t *testing.T) {
	krc := langTagB + "\n" +
		"[0,1000]<0,500,0>千<500,500,0>本\n" + // idx0
		"[1000,1000]<0,500,0>词<500,500,0>：\n" + // idx1
		"[2000,1000]<0,500,0>曲<500,500,0>：\n" + // idx2
		"[3000,1000]\n" + // idx3：0 词空行 → 被 parseKRC 跳过，但占译轨一格
		"[32119,2870]<0,900,0>大<900,900,0>胆\n" + // idx4
		"[34989,3180]<0,900,0>磊<900,900,0>落\n" // idx5
	lines := parseKRC(krc)
	if len(lines) != 5 {
		t.Fatalf("产出行数=%d, want 5（0 词行应被跳过）", len(lines))
	}
	// 关键：尽管中间跳了一行，大胆/磊落 仍拿到正确译文
	if lines[3].Text != "大胆" || lines[3].SubText != "英勇无畏 维新革命" {
		t.Fatalf("对齐错位！行3 = %q/%q, want 大胆/英勇无畏 维新革命", lines[3].Text, lines[3].SubText)
	}
	if lines[4].Text != "磊落" || lines[4].SubText != "光明磊落反战国家" {
		t.Fatalf("对齐错位！行4 = %q/%q, want 磊落/光明磊落反战国家", lines[4].Text, lines[4].SubText)
	}
}
