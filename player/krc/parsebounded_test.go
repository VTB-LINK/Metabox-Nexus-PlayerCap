package krc

// 本文件只测 ParsePlainKRCBounded 的上限：行数与逐字总数（Detailed 与 RomaDetailed 的 Words
// 之和）恰好等于上限时照常返回、与 ParsePlainKRC 结果相同，多一行或多一字即返回 nil, false；
// [language:] 标签文本超长时只丢翻译与音译。

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

// boundedSample 有 3 个产出行：逐字 2、3、1 个；第 2 行带片段数与字数相等的音译，RomaDetailed
// 另有 3 字，逐字总数 9。元数据行与无逐字标记的 [start,dur] 行不产出行。
func boundedSample() string {
	return mkLangTagWords(nil, [][]string{nil, nil, {"ni ", "hao ", "ma"}}) + "\n" +
		"[ti:t]\n" +
		"[0,1000]<0,500,0>听<500,500,0>说\n" +
		"[1000,500]无逐字标记\n" +
		"[2000,1500]<0,500,0>你<500,500,0>好<1000,500,0>吗\n" +
		"[4000,500]<0,500,0>呀\n"
}

func TestParsePlainKRCBoundedAtLimit(t *testing.T) {
	s := boundedSample()
	want := ParsePlainKRC(s)
	if len(want) != 3 || len(want[1].RomaDetailed.Words) != 3 {
		t.Fatalf("夹具：得 %d 行、第 2 行逐字音译 %d 个，want 3 行、3 个", len(want), len(want[1].RomaDetailed.Words))
	}
	got, ok := ParsePlainKRCBounded(s, 3, 9)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("恰好等于上限：ok=%v、得 %+v，want 与 ParsePlainKRC 相同", ok, got)
	}
}

func TestParsePlainKRCBoundedOverLimit(t *testing.T) {
	s := boundedSample()
	for name, lim := range map[string][2]int{
		"多一行": {2, 9},
		"多一字": {3, 8}, // 只计 Detailed 时为 6，不超
	} {
		if got, ok := ParsePlainKRCBounded(s, lim[0], lim[1]); ok || got != nil {
			t.Fatalf("%s：(%d 行, %v)，want (nil, false)", name, len(got), ok)
		}
	}
}

// 一行之内按剩余逐字配额封顶：只数产出的字（文本为空的标记不计），恰好等于配额照常返回，多一字
// 即返回 nil, false；ParsePlainKRCBounded 遇到单行超出同样整份返回 false。
func TestParseWordsQuota(t *testing.T) {
	body := "<0,1,0><5,1,0>a<6,1,0>b<7,1,0>c" // 4 个标记，首个文本为空，产出 3 字
	if w, ok := parseWords(body, 1000, 3); !ok || len(w) != 3 || w[0].Text != "a" || w[0].Timestamp != 1.005 {
		t.Fatalf("配额 3：得 (%+v, %v)，want 3 字、首字 a 起于 1.005", w, ok)
	}
	if w, ok := parseWords(body, 1000, 2); ok || w != nil {
		t.Fatalf("配额 2：得 (%+v, %v)，want (nil, false)", w, ok)
	}
	if w, ok := parseWords("<0,1,0>", 0, 0); !ok || len(w) != 0 {
		t.Fatalf("只有空文本标记、配额 0：得 (%+v, %v)，want 0 字、true", w, ok)
	}
	if got, ok := ParsePlainKRCBounded("[0,10]<0,1,0>a<1,1,0>b\n", 1, 1); ok || got != nil {
		t.Fatalf("单行 2 字、逐字上限 1：(%d 行, %v)，want (nil, false)", len(got), ok)
	}
}

// [language:] 标签文本（base64，解码前）恰好 maxLanguageTagBytes 时照常取翻译与音译；多 1 字节即按
// 没有翻译与音译处理、正文照常。多出的字节是 base64 解码时忽略的 "\r"，标签内容不变：不设上限的
// ParsePlainKRC 对同一输入照常取到翻译，说明丢弃只因长度。
func TestParsePlainKRCBoundedLanguageTagCap(t *testing.T) {
	if maxLanguageTagBytes != 1<<20 {
		t.Fatalf("maxLanguageTagBytes = %d，want 1 MiB（设计 §6.6）", maxLanguageTagBytes)
	}
	head := `{"content":[{"lyricContent":[["译文"],["`
	tail := `"]],"type":1},{"lyricContent":[["ni ","hao"]],"type":0}],"version":1}`
	filler := strings.Repeat("a", maxLanguageTagBytes/4*3-len(head)-len(tail))
	payload := base64.StdEncoding.EncodeToString([]byte(head + filler + tail))
	if len(payload) != maxLanguageTagBytes {
		t.Fatalf("夹具：标签文本 %d bytes，want %d", len(payload), maxLanguageTagBytes)
	}
	const body = "\n[0,1000]<0,500,0>你<500,500,0>好\n"
	at := "[language:" + payload + "]" + body
	over := "[language:" + payload[:4] + "\r" + payload[4:] + "]" + body

	got, ok := ParsePlainKRCBounded(at, 10, 10)
	if !ok || len(got) != 1 || got[0].SubText != "译文" || got[0].RomaText != "ni hao" || len(got[0].RomaDetailed.Words) != 2 {
		t.Fatalf("恰好 %d bytes：ok=%v、得 %+v，want 一行、译文与音译照常", maxLanguageTagBytes, ok, got)
	}
	got, ok = ParsePlainKRCBounded(over, 10, 10)
	if !ok || len(got) != 1 || got[0].Text != "你好" || len(got[0].Detailed.Words) != 2 ||
		got[0].SubText != "" || got[0].RomaText != "" || got[0].RomaWords != nil || len(got[0].RomaDetailed.Words) != 0 {
		t.Fatalf("多 1 字节：ok=%v、得 %+v，want 一行正文、无译文与音译", ok, got)
	}
	if full := ParsePlainKRC(over); len(full) != 1 || full[0].SubText != "译文" || full[0].RomaText != "ni hao" {
		t.Fatalf("ParsePlainKRC 不设上限：得 %+v，want 译文与音译照常", full)
	}
}

// 不产出行的输入不受上限影响：上限为 0 时照常返回 nil, true。
func TestParsePlainKRCBoundedNoLines(t *testing.T) {
	for _, s := range []string{"", "[ti:t]\n[ar:a]\n", "[00:01.00]听说\n", "[0,1]a\n[1,1]\n"} {
		if got, ok := ParsePlainKRCBounded(s, 0, 0); !ok || got != nil {
			t.Fatalf("%q：(%d 行, %v)，want (nil, true)", s, len(got), ok)
		}
	}
}
