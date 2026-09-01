package qqmusic

// 本文件只测音译歌词（QQ 的 roma → roma_text）这条链：mergeRoma 的时间就近匹配、attachRoma
// 的解密与降级、以及 toLyricLines 的接线。
//
// 与翻译链的两条关键差异，本文件逐一钉死：
//  1. 音译写 RomaText、翻译写 SubText，两条轨**相互独立、互不污染**（双向都测）。
//  2. roma 与主歌词 lyric 同源同为 QRC 毫秒精度，正确匹配时间差 ≈0；容差只是防抖安全网。
//     故 fixture 的 roma 行首刻意较主歌词偏 2~7ms（模拟意外抖动）——既证明就近匹配成立，
//     又让「把 romaToleranceMs 改成 0（等价精确匹配）」的变异立刻变红。
//
// roma fixture 为构造数据（无法连真机取真实解密结果）：主歌词日文 QRC + 同结构罗马音 QRC，
// 行首时间戳对齐主歌词。真机端到端核对由主会话的 qqromaprobe 配合 attachRoma 的临时日志完成。

import (
	"strings"
	"testing"
)

// jpQRC 主歌词（QRC 逐字，3 行）：君の声 / 遠くで / 響くよ，行首 0 / 3 / 6 秒。
const jpQRC = `[ti:テスト]
[0,3000]君(0,1000)の(1000,1000)声(2000,1000)
[3000,3000]遠(3000,1000)く(4000,1000)で(5000,1000)
[6000,3000]響(6000,1500)くよ(7500,1500)`

// jpRoma 音译（QRC 逐字罗马音），与 jpQRC 同结构、行首刻意偏 2 / 7 / 5 ms：
//
//	[0,3000]   ↔ [2,3000]      差 2ms → kiminokoe
//	[3000,...] ↔ [3007,...]    差 7ms → tookude
//	[6000,...] ↔ [5995,...]    差 5ms → hibikuyo
const jpRoma = `[ti:テスト]
[2,3000]kimi(2,1000)no(1002,1000)koe(2002,1000)
[3007,3000]tooku(3007,1000)de(5007,1000)
[5995,3000]hibiku(5995,1500)yo(7495,1500)`

// jpTrans 翻译（标准 LRC），行首精确对齐主歌词：你的声音 / 在远方 / 回响着。
const jpTrans = `[00:00.00]你的声音
[00:03.00]在远方
[00:06.00]回响着`

// 主用例：QRC 罗马音必须解析并按时间就近落到正确的行上。
//
// 变异自证：把 romaToleranceMs 改成 0（等价精确匹配），本例第一行（差 2ms）立刻变红——
// 这正是「roma 同源但行首未必逐毫秒相等」时精确匹配会踩的坑。
func TestMergeRomaMatchesWithinTolerance(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	if len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3；jpQRC 样本解析就不对，后面的断言无意义", len(lines))
	}
	mergeRoma(lines, jpRoma)

	want := []string{"kiminokoe", "tookude", "hibikuyo"}
	for i, w := range want {
		if lines[i].RomaText != w {
			t.Fatalf("lines[%d].RomaText = %q, want %q（行 t=%.3f）；"+
				"roma 与主歌词恒差个位数毫秒，必须按容差就近匹配而非精确匹配",
				i, lines[i].RomaText, w, lines[i].Time)
		}
	}
}

// 超出容差的音译行绝不能被拉过来配上：宁可没有 roma_text，也不能配错行。
//
// 变异自证：把 romaToleranceMs 放大到 1000，本例变红。
func TestMergeRomaRejectsBeyondTolerance(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	// 把音译整体推后 500ms，远超 20ms 容差
	mergeRoma(lines, `[00:00.50]zurashita`)
	for i := range lines {
		if lines[i].RomaText != "" {
			t.Fatalf("lines[%d].RomaText = %q, want \"\"；差 500ms 的音译行不该匹配", i, lines[i].RomaText)
		}
	}
}

// 隔离（音译→翻译方向）：只合并 roma 时，sub_text 必须一律保持空——音译绝不写翻译轨。
//
// 变异自证：把 mergeRoma 里的 lines[i].RomaText 改成 lines[i].SubText，本例变红。
func TestMergeRomaDoesNotTouchSubText(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	mergeRoma(lines, jpRoma)
	for i := range lines {
		if lines[i].SubText != "" {
			t.Fatalf("lines[%d].SubText = %q, want \"\"；音译合并绝不能污染翻译轨", i, lines[i].SubText)
		}
	}
}

// 隔离（翻译→音译方向）：只合并 trans 时，roma_text 必须一律保持空——翻译绝不写音译轨。
//
// 变异自证：把 mergeTrans 里的 lines[i].SubText 改成 lines[i].RomaText，本例变红。
func TestMergeTransDoesNotTouchRomaText(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	mergeTrans(lines, jpTrans)
	for i := range lines {
		if lines[i].RomaText != "" {
			t.Fatalf("lines[%d].RomaText = %q, want \"\"；翻译合并绝不能污染音译轨", i, lines[i].RomaText)
		}
	}
}

// 音译与翻译同时存在时，两条轨各归各的、互不覆盖，且各自内容不串。
func TestAttachRomaAndTransAreIndependent(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	attachTrans(lines, jpTrans, 0) // crypt=0 且非 hex → 明文
	attachRoma(lines, jpRoma, 0)

	wantSub := []string{"你的声音", "在远方", "回响着"}
	wantRoma := []string{"kiminokoe", "tookude", "hibikuyo"}
	for i := range lines {
		if lines[i].SubText != wantSub[i] {
			t.Fatalf("lines[%d].SubText = %q, want %q", i, lines[i].SubText, wantSub[i])
		}
		if lines[i].RomaText != wantRoma[i] {
			t.Fatalf("lines[%d].RomaText = %q, want %q", i, lines[i].RomaText, wantRoma[i])
		}
		if lines[i].SubText == lines[i].RomaText {
			t.Fatalf("lines[%d] 的 sub_text 与 roma_text 相等（%q）；两条轨串了", i, lines[i].SubText)
		}
	}
}

// "//" 是 QQ 的「本行不发音/不翻译」占位符，绝不能进 roma_text。
// 顺带验证 mergeRoma 也能吃普通 LRC 源（格式兼容的另一条分支）。
//
// 变异自证：删掉 mergeRoma 里的 transIsDroppable 判断，本例变红。
func TestMergeRomaDropsNoLyricMark(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	mergeRoma(lines, "[00:00.00]//\n[00:03.00]tooku")
	if lines[0].RomaText != "" {
		t.Fatalf("lines[0].RomaText = %q, want \"\"；\"//\" 是占位符不是音译内容", lines[0].RomaText)
	}
	if lines[1].RomaText != "tooku" {
		t.Fatalf("lines[1].RomaText = %q, want \"tooku\"；过滤占位符时误伤了正常音译行", lines[1].RomaText)
	}
}

// QQ 自家文案（版权/来源声明）若混进音译轨也绝不能进 roma_text。
func TestMergeRomaDropsQQMeta(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	mergeRoma(lines, "[00:00.00]QQ音乐享有本翻译作品的著作权\n[00:03.00]tooku")
	if lines[0].RomaText != "" {
		t.Fatalf("lines[0].RomaText = %q, want \"\"；QQ 产品文案不是音译内容", lines[0].RomaText)
	}
	if lines[1].RomaText != "tooku" {
		t.Fatalf("lines[1].RomaText = %q, want \"tooku\"；过滤文案时误伤了正常音译行", lines[1].RomaText)
	}
}

// 无音译（中文歌 roma 恒为空）时不得改动任何东西。
func TestMergeRomaEmptyIsNoop(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	mergeRoma(lines, "")
	for i := range lines {
		if lines[i].RomaText != "" || lines[i].SubText != "" {
			t.Fatalf("lines[%d] = {sub:%q roma:%q}, want 全空；空 roma 必须是 no-op",
				i, lines[i].SubText, lines[i].RomaText)
		}
	}
}

// 接线：attachRoma 必须真的把明文 roma 合进去。
//
// 变异自证：把 attachRoma 内部的 mergeRoma 调用删掉，本例变红。
func TestAttachRomaMergesPlaintext(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	attachRoma(lines, jpRoma, 0) // crypt=0 且非 hex → 按明文处理
	if lines[0].RomaText != "kiminokoe" {
		t.Fatalf("lines[0].RomaText = %q, want 音译文本；attachRoma 没把明文 roma 合进去",
			lines[0].RomaText)
	}
}

// 降级：roma「是密文但解不开」时，必须跳过音译，且**不得**污染主歌词或翻译。
//
// 与翻译链同形状的坑：坏密文若被放行给 parseLRC，整串 hex 无时间戳会命中末尾「无时间戳按
// 时长均分」的兜底，凭空造出假音译行乱配 roma_text。
//
// 变异自证：把 attachRoma 里 err != nil 分支的 return 删掉（改成继续 mergeRoma），本例变红。
func TestAttachRomaBadCiphertextDoesNotPoisonLyrics(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	attachTrans(lines, jpTrans, 0) // 先合入正常翻译，用来验证音译失败不牵连它
	wantText := lines[1].Text
	wantSub := lines[1].SubText
	n := len(lines)

	// 合法 hex、长度是 8 的倍数，但 3DES 解出来不是 zlib → 「是密文但坏了」
	badCipher := strings.Repeat("ab", 64)
	attachRoma(lines, badCipher, 1)

	if len(lines) != n {
		t.Fatalf("行数从 %d 变成 %d；解不开的 roma 密文触发了均分兜底造假行", n, len(lines))
	}
	for i := range lines {
		if lines[i].RomaText != "" {
			t.Fatalf("lines[%d].RomaText = %q, want \"\"；解不开的 roma 必须整条跳过", i, lines[i].RomaText)
		}
	}
	if lines[1].Text != wantText {
		t.Fatalf("主歌词被音译解密失败带坏了: %q, want %q", lines[1].Text, wantText)
	}
	if lines[1].SubText != wantSub {
		t.Fatalf("翻译轨被音译解密失败带坏了: %q, want %q；两条轨必须互不牵连", lines[1].SubText, wantSub)
	}
}

// 接线：RomaText 必须从内部 lyricLine 流到对外的 player.LyricLine（all_lyrics 的载荷），
// 且不与 sub_text 混用、不动 play_time 语义。
//
// 变异自证：把 toLyricLines 里的 l.RomaText 改回字面量 ""（修复前的样子），本例变红。
func TestToLyricLinesCarriesRomaText(t *testing.T) {
	lines, _, _ := parseLRC(jpQRC, 0)
	mergeRoma(lines, jpRoma)

	out := toLyricLines(lines, 0.2)
	if len(out) != len(lines) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(lines))
	}
	if out[1].RomaText != "tookude" {
		t.Fatalf("out[1].RomaText = %q, want \"tookude\"；toLyricLines 没把 RomaText 传给 BuildLyricLine",
			out[1].RomaText)
	}
	if out[1].SubText != "" {
		t.Fatalf("out[1].SubText = %q, want \"\"；音译不该串进翻译轨", out[1].SubText)
	}
	// 顺带钉住：roma_text 的加入不能动到既有的 play_time 语义（timestamp - offset）
	if got, want := out[1].PlayTime, float32(3.0-0.2); got != want {
		t.Fatalf("out[1].PlayTime = %v, want %v；RomaText 不该影响 play_time", got, want)
	}
}
