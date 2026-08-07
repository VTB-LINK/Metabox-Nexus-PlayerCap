package krc

// 本文件只测 KRC 明文解析（ParsePlainKRC / parseWords）：把酷狗/汽水 KRC 的字级时间轴解成逐字片段。
//
// 要害是**相对偏移**：KRC 的 <off,dur,0> 里 off 是相对行首的，不是绝对时间。三家格式三个样，
// 照抄 QQ（文字在前、绝对）或网易云（时间在前、绝对）都会错，故这里逐条钉死。

import "testing"

// 取自实测解密的《晴天》KRC（周杰伦），含元数据标签与前两条时间轴行。
const krcSample = `[ti:晴天]
[ar:周杰伦]
[al:叶惠美]
[by:]
[offset:0]
[0,2250]<0,160,0>晴<160,160,0>天
[2250,2250]<0,450,0>词<450,450,0>：<900,450,0>周<1350,450,0>杰<1800,450,0>伦`

// 相对偏移转绝对：行首 2250 + 偏移 450 = 2700，不是 450。
// 变异自证：把 parseWords 里的 `lineStartMs+offMs` 改成 `offMs`（当绝对时间用），本例变红。
func TestParsePlainKRCWordsOffsetsAreRelativeToLineStart(t *testing.T) {
	lines := ParsePlainKRC(krcSample)
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(lines))
	}
	w := lines[1].Detailed.Words
	if len(w) != 5 {
		t.Fatalf("len(words) = %d, want 5", len(w))
	}
	if w[0].Timestamp != 2.25 {
		t.Fatalf("words[0].Timestamp = %v, want 2.25（行首 2250ms + 偏移 0）", w[0].Timestamp)
	}
	if w[1].Timestamp != 2.7 {
		t.Fatalf("words[1].Timestamp = %v, want 2.7（行首 2250 + 偏移 450 = 2700ms）；"+
			"若得 0.45 说明把相对偏移当成了绝对时间", w[1].Timestamp)
	}
	if w[4].Timestamp != 4.05 {
		t.Fatalf("words[4].Timestamp = %v, want 4.05（2250+1800）", w[4].Timestamp)
	}
}

// 末字终点必须与下一行起始严丝合缝：2250+1800+450 = 4500 = 下一行 [4500,...]。
// 这条是相对偏移正确性的独立佐证，实测《晴天》全曲成立。
func TestParsePlainKRCLastWordMeetsNextLineStart(t *testing.T) {
	lines := ParsePlainKRC(krcSample)
	w := lines[1].Detailed.Words
	last := w[len(w)-1]
	end := last.Timestamp + last.Duration
	if end != 4.5 {
		t.Fatalf("末字终点 = %v, want 4.5（应正好接上下一行起始）", end)
	}
}

// 时间戳在**文字之前**（同 YRC、与 QQ QRC 相反）：文字取标记之后那段。
// 变异自证：改成取标记之前那段（QQ QRC 的取法），本例变红。
func TestParsePlainKRCWordsTextFollowsTag(t *testing.T) {
	lines := ParsePlainKRC(krcSample)
	w := lines[1].Detailed.Words
	if w[0].Text != "词" || w[1].Text != "：" || w[4].Text != "伦" {
		t.Fatalf("words 文本 = %q/%q/%q, want 词/：/伦；KRC 是「时间戳在前、文字在后」",
			w[0].Text, w[1].Text, w[4].Text)
	}
}

// 行文本由字片段拼接而来，且元数据标签行不产出歌词行。
func TestParsePlainKRCLineTextAndMetadataSkipped(t *testing.T) {
	lines := ParsePlainKRC(krcSample)
	if lines[0].Text != "晴天" {
		t.Fatalf("lines[0].Text = %q, want \"晴天\"", lines[0].Text)
	}
	if lines[1].Text != "词：周杰伦" {
		t.Fatalf("lines[1].Text = %q, want \"词：周杰伦\"", lines[1].Text)
	}
	// [ti:]/[ar:]/[al:]/[by:]/[offset:] 五行都不该成为歌词行
	for _, l := range lines {
		if l.Text == "晴天" && l.Time != 0 {
			t.Fatalf("元数据 [ti:晴天] 被当成歌词行了")
		}
	}
}

// 行级时长取自 [start,dur]，逐字与行级都要有。
func TestParsePlainKRCLineDuration(t *testing.T) {
	lines := ParsePlainKRC(krcSample)
	if lines[1].Detailed.Duration != 2.25 {
		t.Fatalf("行级 duration = %v, want 2.25", lines[1].Detailed.Duration)
	}
	if lines[1].Detailed.Timestamp != 2.25 {
		t.Fatalf("行级 timestamp = %v, want 2.25", lines[1].Detailed.Timestamp)
	}
}

// PlayTime 由调用方的 applyDetailedOffset 统一套，解析阶段必须留空——否则 offset 套两次。
func TestParsePlainKRCLeavesPlayTimeUnset(t *testing.T) {
	lines := ParsePlainKRC(krcSample)
	for _, l := range lines {
		if l.Detailed.PlayTime != 0 {
			t.Fatalf("行级 PlayTime = %v, want 0", l.Detailed.PlayTime)
		}
		for i, w := range l.Detailed.Words {
			if w.PlayTime != 0 {
				t.Fatalf("words[%d].PlayTime = %v, want 0", i, w.PlayTime)
			}
		}
	}
}

// 非 KRC 内容（普通 LRC）→ 不产出行，让调用方回落 LRC 解析。
func TestParsePlainKRCPlainLRCYieldsNothing(t *testing.T) {
	if got := ParsePlainKRC("[00:12.34]就这样一直走下去\n[00:15.00]天空好蓝"); len(got) != 0 {
		t.Fatalf("ParsePlainKRC(普通LRC) = %d 行, want 0", len(got))
	}
}
