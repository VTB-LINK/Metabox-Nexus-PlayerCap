package lyric

import (
	"strings"
	"testing"
)

// escapeKeyword 必须把空格编成 %20 而非 +：酷狗 catalog / krcs 不认 + 作空格（按字面加号处理），
// 用 + 会让多词 keyword（“歌手 歌名”）退化成搜字面加号串、搜出一堆 UGC 翻唱挤掉正主——实测
// “BoA Only One” 用 + 时 catalog 首条是翻唱、%20 时才是正主 Only One - BoA。
//
// 变异自证：把 escapeKeyword 改回 url.QueryEscape（空格→+），本测试立刻变红。
func TestEscapeKeywordUsesPercent20ForSpace(t *testing.T) {
	got := escapeKeyword("BoA Only One")
	if want := "BoA%20Only%20One"; got != want {
		t.Fatalf("escapeKeyword(%q) = %q, want %q（空格必须是 %%20，否则酷狗搜不到正主）",
			"BoA Only One", got, want)
	}
	if strings.Contains(got, "+") {
		t.Fatalf("编码结果残留 +：%q；酷狗把 + 当字面加号，多词搜索会退化成翻唱", got)
	}
}

// 非空格特殊字符仍需转义（& 会截断 query），空 keyword（kugou 走 hash 的情形）编码后仍为空。
func TestEscapeKeywordEscapesSpecialAndEmpty(t *testing.T) {
	if got := escapeKeyword(""); got != "" {
		t.Fatalf("空 keyword 编码应为空，得 %q", got)
	}
	got := escapeKeyword("a & b")
	if strings.Contains(got, "&") || strings.Contains(got, "+") {
		t.Fatalf("特殊字符/空格未安全编码：%q（& 需转义、空格需 %%20）", got)
	}
}
