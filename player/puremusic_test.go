package player

// 本文件只测 IsPureMusicOnly：把「平台其实没歌词、只给了一句提示语」认出来。
//
// 它是纯函数，所以**这些测试证明不了接线**（cloudmusic/kugou 有没有真的调它、调用点对不对，
// 那三个包 CI 永远跑不了，只能真机验 —— AGENTS.md §8）。这里只钉死判据本身，
// 尤其是 len == 1 那道护栏：没有它，这就退化成纯关键词匹配。

import "testing"

const hint = "纯音乐，请欣赏"

// line 造一行歌词，只关心 Text。
func line(idx int, text string) LyricLine {
	return LyricLine{Index: idx, Text: text}
}

// TestIsPureMusicOnlyRecognizesHint 认出提示语。
func TestIsPureMusicOnlyRecognizesHint(t *testing.T) {
	if !IsPureMusicOnly([]LyricLine{line(0, hint)}) {
		t.Errorf("%q 单独一行应被认作提示语", hint)
	}
}

// TestIsPureMusicOnlyNeedsExactlyOneLine 钉死 len == 1 那道护栏。
//
// **这是本文件最重要的一条**。没有它，「某首歌真的唱了这七个字」就会被整首吞掉——
// 而歌词里出现「纯音乐，请欣赏」并非不可想象（翻唱、玩梗、旁白）。
// 加上护栏后，误判需要「整首歌只有一行 **且** 那行恰好是这七个字」。
//
// 变异自证：把 len(lyrics) == 1 改成 len(lyrics) >= 1 即红。
func TestIsPureMusicOnlyNeedsExactlyOneLine(t *testing.T) {
	multi := []LyricLine{
		line(0, hint),
		line(1, "这是真的歌词"),
	}
	if IsPureMusicOnly(multi) {
		t.Error("首行恰好是提示语、但后面还有真歌词——不能当作纯音乐，整首会被吞掉")
	}
}

// TestIsPureMusicOnlyIgnoresOtherText 单行但不是提示语 → 不认。
//
// 一首歌的歌词只有一行是完全合法的（短歌、纯人声采样）。
func TestIsPureMusicOnlyIgnoresOtherText(t *testing.T) {
	for _, text := range []string{
		"纯音乐",          // 不完整，不认
		"纯音乐，请欣赏 ",     // 带尾随空格，不认
		"Instrumental", // 别的说法，平台没这么给过
		"",             // 空行
		"这首歌只有一行歌词",
	} {
		if IsPureMusicOnly([]LyricLine{line(0, text)}) {
			t.Errorf("%q 不在提示语表里，不该被认作纯音乐", text)
		}
	}
}

// TestIsPureMusicOnlyEmpty 空歌词不是「只有提示语」——它本来就没歌词，
// 调用方走的是别的分支（qqmusic 的 API 直接返回零行）。
func TestIsPureMusicOnlyEmpty(t *testing.T) {
	if IsPureMusicOnly(nil) {
		t.Error("nil 不该被认作提示语")
	}
	if IsPureMusicOnly([]LyricLine{}) {
		t.Error("空 slice 不该被认作提示语")
	}
}
