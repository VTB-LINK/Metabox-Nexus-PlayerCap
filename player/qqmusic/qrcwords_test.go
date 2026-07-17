package qqmusic

// 本文件只测 parseQRCWords：把 QRC 行体的字级时间戳解析成逐字片段。
// 这是「一错就让整首逐字高亮错位」的路径，且 QQ 的 QRC 与网易云的 YRC **顺序相反**
// （QRC 文字在前、时间戳在后），最容易照着 ParseYRC 抄反。

import "testing"

// qrcBody 取自实测解密出的 QRC（《我问天我问地》首句，见 commit 说明）。
const qrcBody = `风(28837,439)已(29276,441)经(29717,500)停(30217,499)息 (30716,1500)雨(32216,500)`

func TestParseQRCWordsOrderAndTiming(t *testing.T) {
	words := parseQRCWords(qrcBody)
	if len(words) != 6 {
		t.Fatalf("len(words) = %d, want 6；解析出的片段数不对", len(words))
	}
	// 顺序：文字必须是它**前面**那段，不是后面那段（抄 YRC 会整体错位一格）
	if words[0].Text != "风" {
		t.Fatalf("words[0].Text = %q, want \"风\"；QRC 是「文字在前」，别按 YRC 的 (时间)文字 解析", words[0].Text)
	}
	if words[1].Text != "已" {
		t.Fatalf("words[1].Text = %q, want \"已\"", words[1].Text)
	}
	// 时间：ms → s
	if words[0].Timestamp != 28.837 {
		t.Fatalf("words[0].Timestamp = %v, want 28.837", words[0].Timestamp)
	}
	if words[0].Duration != 0.439 {
		t.Fatalf("words[0].Duration = %v, want 0.439", words[0].Duration)
	}
}

// 空格是逐字排版的一部分，绝不能 TrimSpace 掉——"息 " 的尾空格决定断字位置。
// 变异自证：给片段文本加 strings.TrimSpace，本例变红。
func TestParseQRCWordsKeepsSpacing(t *testing.T) {
	words := parseQRCWords(qrcBody)
	if len(words) < 5 {
		t.Fatalf("len(words) = %d, too few", len(words))
	}
	if words[4].Text != "息 " {
		t.Fatalf("words[4].Text = %q, want \"息 \"（尾空格必须保留）", words[4].Text)
	}
}

// 最后一个时间戳之后的残留一律丢弃：这天然挡掉 QRC 外层 XML 的 `"/>` 尾巴。
// 变异自证：若改成「把尾部残留也当一个片段」，本例会多出一个 `"/>` 片段。
func TestParseQRCWordsDropsTrailingXMLTail(t *testing.T) {
	words := parseQRCWords(`雨(32216,500)"/>`)
	if len(words) != 1 {
		t.Fatalf("len(words) = %d, want 1；末尾时间戳之后的残留不该成为片段", len(words))
	}
	if words[0].Text != "雨" {
		t.Fatalf("words[0].Text = %q, want \"雨\"", words[0].Text)
	}
}

// 非 QRC 源（普通 LRC 行体，无字级时间戳）→ 返回 nil，让 Detailed 保持零值 → 序列化成 {}。
func TestParseQRCWordsPlainLRCReturnsNil(t *testing.T) {
	if got := parseQRCWords("就这样一直走下去"); got != nil {
		t.Fatalf("parseQRCWords(无时间戳) = %#v, want nil", got)
	}
}

// PlayTime 由 applyDetailedOffset 统一套，解析阶段必须留空——否则 offset 会被套两次。
func TestParseQRCWordsLeavesPlayTimeUnset(t *testing.T) {
	words := parseQRCWords(qrcBody)
	for i, w := range words {
		if w.PlayTime != 0 {
			t.Fatalf("words[%d].PlayTime = %v, want 0（offset 由 applyDetailedOffset 统一套）", i, w.PlayTime)
		}
	}
}
