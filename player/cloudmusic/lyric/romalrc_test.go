package lyric

// 本文件只测 MergeRomalrc（网易云逐行罗马音/音译的合并）。romalrc 与 tlyric（翻译）完全同构，
// 都是标准 LRC，按毫秒时间戳就近对齐；唯一区别是写入 RomaText 而非 SubText。用例覆盖：
// 时间对齐正确、匹配不上的行留空、无 romalrc 时 RomaText 留空，以及音译绝不污染 sub_text
// （翻译与音译是两条独立的轨，互不干扰）。

import "testing"

// 时间对齐：romalrc 按毫秒时间戳对到已有的 LRC 行上，逐行写入 RomaText。
// 样本形如网易云日文歌（id=3366383159）返回的 romalrc。
func TestMergeRomalrc(t *testing.T) {
	lyrics := ParseLRC("[00:12.34]夜に駆ける\n[00:15.67]沈むように\n[00:18.90]溶けてゆくように")
	romalrc := "[00:12.34]yoru ni kakeru\n[00:15.67]shizumu you ni\n[00:18.90]tokete yuku you ni"

	MergeRomalrc(lyrics, romalrc)

	want := []string{"yoru ni kakeru", "shizumu you ni", "tokete yuku you ni"}
	for i, w := range want {
		if lyrics[i].RomaText != w {
			t.Fatalf("lyrics[%d].RomaText = %q, want %q", i, lyrics[i].RomaText, w)
		}
	}
}

// romalrc 缺某个时间戳时，该行 RomaText 留空，绝不强塞。
func TestMergeRomalrcUnmatchedLineStaysEmpty(t *testing.T) {
	lyrics := ParseLRC("[00:12.34]夜に駆ける\n[00:15.67]沈むように")
	romalrc := "[00:12.34]yoru ni kakeru" // 缺 15.67 这行

	MergeRomalrc(lyrics, romalrc)

	if lyrics[0].RomaText != "yoru ni kakeru" {
		t.Fatalf("lyrics[0].RomaText = %q, want \"yoru ni kakeru\"", lyrics[0].RomaText)
	}
	if lyrics[1].RomaText != "" {
		t.Fatalf("lyrics[1].RomaText = %q, want 空（romalrc 无此行）", lyrics[1].RomaText)
	}
}

// 无 romalrc（空串）：RomaText 全部留空。这是本次示例歌之外的常态（多数中文歌无音译轨）。
func TestMergeRomalrcEmptyLeavesRomaTextEmpty(t *testing.T) {
	lyrics := ParseLRC("[00:12.34]夜に駆ける\n[00:15.67]沈むように")

	MergeRomalrc(lyrics, "")

	for i := range lyrics {
		if lyrics[i].RomaText != "" {
			t.Fatalf("lyrics[%d].RomaText = %q, want 空（无 romalrc）", i, lyrics[i].RomaText)
		}
	}
}

// 承重不变量：音译（romalrc→RomaText）与翻译（tlyric→SubText）是两条独立的轨，绝不互相污染。
// 同一批歌词先并翻译、再并音译，两个字段必须各自正确、互不覆盖。
// 样本形如网易云韩文歌（id=1905106688）：既有 tlyric 又有 romalrc。
func TestMergeRomalrcDoesNotTouchSubText(t *testing.T) {
	lyrics := ParseLRC("[00:20.00]사랑해\n[00:23.50]보고 싶어")
	tlyric := "[00:20.00]我爱你\n[00:23.50]我想你"
	romalrc := "[00:20.00]saranghae\n[00:23.50]bogo sipeo"

	MergeTlyric(lyrics, tlyric)
	MergeRomalrc(lyrics, romalrc)

	if lyrics[0].SubText != "我爱你" || lyrics[0].RomaText != "saranghae" {
		t.Fatalf("lyrics[0] = sub %q / roma %q, want 我爱你 / saranghae", lyrics[0].SubText, lyrics[0].RomaText)
	}
	if lyrics[1].SubText != "我想你" || lyrics[1].RomaText != "bogo sipeo" {
		t.Fatalf("lyrics[1] = sub %q / roma %q, want 我想你 / bogo sipeo", lyrics[1].SubText, lyrics[1].RomaText)
	}
}

// 反向也验一次：只并音译、不并翻译时，SubText 必须保持空——音译绝不写进 sub_text。
func TestMergeRomalrcLeavesSubTextEmptyWhenNoTlyric(t *testing.T) {
	lyrics := ParseLRC("[00:20.00]사랑해\n[00:23.50]보고 싶어")
	romalrc := "[00:20.00]saranghae\n[00:23.50]bogo sipeo"

	MergeRomalrc(lyrics, romalrc)

	for i := range lyrics {
		if lyrics[i].SubText != "" {
			t.Fatalf("lyrics[%d].SubText = %q, want 空（音译绝不污染 sub_text）", i, lyrics[i].SubText)
		}
		if lyrics[i].RomaText == "" {
			t.Fatalf("lyrics[%d].RomaText 意外为空", i)
		}
	}
}
