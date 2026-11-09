package lyric

// 本文件只测主歌词的行向量怎么读（loadLyrics）：end 取 +0x4C 而不是容量末尾、向量本身不对时报错、
// 哪些行跳过、行起始时间不合理就停。假内存与轨的布局沿用 roma_test.go（fakeMem、track、entry）。

import (
	"math"
	"strings"
	"testing"
)

// texts 把读出的各行文本拼在一起，便于一眼看出读到了哪几行。
func texts(lines []LyricLine) string {
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l.Text)
	}
	return sb.String()
}

// 行向量的 end 在 +0x4C；+0x50 是容量末尾，end 之后、容量之内的是残留项（2026-11-02 真机，全民K歌
// 2.21.176.1220：两首歌按 +0x4C 是 29、30 行，按 +0x50 都是 42 项）。残留项的行起始时间接着往后走、
// 文本也正常时，三道时间闸都挡不住，只能靠不去读它。
//
// 变异自证：把 loadLyrics 里读 end 的地址从 beginAddr+4 改成 beginAddr+8（即 +0x50），读进了残留的
// 两行而红（实测）。
func TestLoadLyricsUsesVectorEndNotCapacity(t *testing.T) {
	f := newFakeMem()
	f.track(testMain, testVtable, mainPath,
		[]uint32{f.entry(1.22, "Uh ", "you"), f.entry(6.019, "저기")},
		f.entry(9.917, "上一首歌留下的"), f.entry(12.5, "残留"))
	lines, err := loadLyrics(f, testMain)
	if err != nil || len(lines) != 2 {
		t.Fatalf("应只读 end 之前的 2 行，got %d 行（%q）err=%v", len(lines), texts(lines), err)
	}
	if lines[0].Text != "Uh you" || lines[1].Text != "저기" || lines[1].Time != 6.019 {
		t.Errorf("行内容不对：%+v", lines)
	}
}

// 行向量本身不对时报错、一行不给：头部读不出、空向量、end 在 begin 之前、begin 为 0、行数超过 1000。
// 恰好 1000 项不算异常（这里各项都读不出、逐项跳过，返回 0 行、不报错）。
//
// 变异自证：把 `endPtr <= beginPtr` 改成 `endPtr < beginPtr`，「begin 与 end 相等」一例不报错而红；去掉
// `beginPtr == 0` 一支，「begin 为 0」一例不报错而红；把行数上限的 `>` 改成 `>=`，「恰好 1000 项」一例
// 报错而红；去掉行数上限，「1001 项」一例不报错而红（均实测）。
func TestLoadLyricsRejectsBadVector(t *testing.T) {
	const b uint32 = 0x00600000
	cases := []struct {
		name    string
		build   func(f *fakeMem)
		wantErr bool
	}{
		{"子结构读不出", func(f *fakeMem) {}, true},
		{"end 读不出", func(f *fakeMem) { f.put32(testMain+trackLinesOff, b) }, true},
		{"空向量（全是 0）", func(f *fakeMem) { f.track(testMain, testVtable, mainPath, nil) }, true},
		{"begin 与 end 相等", func(f *fakeMem) { f.put32(testMain+trackLinesOff, b); f.put32(testMain+trackLinesOff+4, b) }, true},
		{"end 在 begin 之前", func(f *fakeMem) { f.put32(testMain+trackLinesOff, b); f.put32(testMain+trackLinesOff+4, b-4) }, true},
		{"begin 为 0", func(f *fakeMem) { f.put32(testMain+trackLinesOff, 0); f.put32(testMain+trackLinesOff+4, 0x40) }, true},
		{"1001 项", func(f *fakeMem) { f.put32(testMain+trackLinesOff, b); f.put32(testMain+trackLinesOff+4, b+1001*4) }, true},
		{"恰好 1000 项", func(f *fakeMem) { f.put32(testMain+trackLinesOff, b); f.put32(testMain+trackLinesOff+4, b+1000*4) }, false},
	}
	for _, c := range cases {
		f := newFakeMem()
		c.build(f)
		lines, err := loadLyrics(f, testMain)
		if (err != nil) != c.wantErr || len(lines) != 0 {
			t.Errorf("%s：got %d 行 err=%v，want 0 行、报错=%v", c.name, len(lines), err, c.wantErr)
		}
	}
}

// 一项读不出（entry 指针为 0、指向读不出的地址、行里一个字都没有）只跳过这一项，后面照读。Index 是这
// 一行在向量里的位置，不是收下的第几行——LoadRoma 按它配音译轨的同号行。
//
// 变异自证：把 entry 指针为 0 时的 continue 改成 break，后面的行都没读到而红；把 Index 改成
// len(lyrics)，末行的 Index 成了 1 而红（均实测）。
func TestLoadLyricsSkipsUnreadableEntry(t *testing.T) {
	for name, bad := range map[string]func(f *fakeMem) uint32{
		"entry 指针为 0": func(f *fakeMem) uint32 { return 0 },
		"entry 读不出":   func(f *fakeMem) uint32 { return 0x00700000 }, // 没映射的地址
		"行里一个字都没有":    func(f *fakeMem) uint32 { return f.entry(3) },
	} {
		f := newFakeMem()
		f.track(testMain, testVtable, mainPath, []uint32{f.entry(1, "a"), bad(f), f.entry(5, "c")})
		lines, err := loadLyrics(f, testMain)
		if err != nil || texts(lines) != "ac" || lines[1].Index != 2 {
			t.Errorf("%s：应跳过这一项、读到 a 与 c（c 的 Index 为 2），got %+v err=%v", name, lines, err)
		}
	}
}

// 行起始时间不合理（NaN、±Inf、负数、超出 IsPlausiblePlayTime 的 [0,100000)、issue #46 真机录到的 1.5e18）
// 就停：之后的行一概不读，哪怕时间与文本都正常。第一行就不合理时一行不给。另两道闸：已有行之后行起始
// ≤0（中途归零）、比上一行早超过 1 秒（回退），同样就停；开场行起始为 0、回退不足 1 秒照常收下。
//
// 变异自证：删掉 loadLyrics 里 `if !IsPlausiblePlayTime(timeVal) { break }`，NaN、+Inf、100000、1.5e18、
// 「第一行就是 NaN」「第一行就是负数」六例读进了不合理的那行而红（负数与 -Inf 在第二行时仍由「中途归零」
// 挡住）；把它的 break 改成 continue，跳过不合理的那行、读进了后面的行而红；删掉「中途归零」一道，「中途归零」一例红；
// 把它的 len(lyrics) > 0 去掉，「开场行从 0 起」一例红；删掉「回退」一道，「回退超过 1 秒」一例红（均实测）。
func TestLoadLyricsStopsAtImplausibleLineTime(t *testing.T) {
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	second := func(bad float32) func(f *fakeMem) []uint32 {
		return func(f *fakeMem) []uint32 { return []uint32{f.entry(1, "a"), f.entry(bad, "b"), f.entry(3, "c")} }
	}
	cases := []struct {
		name  string
		build func(f *fakeMem) []uint32
		want  string
	}{
		{"NaN", second(nan), "a"},
		{"+Inf", second(inf), "a"},
		{"-Inf", second(-inf), "a"},
		{"负数", second(-1), "a"},
		{"100000（上界不含）", second(100000), "a"},
		{"issue #46 的 1.5e18", second(1.501759e18), "a"},
		{"中途归零", second(0), "a"},
		{"回退超过 1 秒", func(f *fakeMem) []uint32 { return []uint32{f.entry(5, "a"), f.entry(3.5, "b"), f.entry(6, "c")} }, "a"},
		{"第一行就是 NaN", func(f *fakeMem) []uint32 { return []uint32{f.entry(nan, "a"), f.entry(1, "b")} }, ""},
		{"第一行就是负数", func(f *fakeMem) []uint32 { return []uint32{f.entry(-1, "a"), f.entry(1, "b")} }, ""},
		// 对照：照常收下。
		{"开场行从 0 起", func(f *fakeMem) []uint32 { return []uint32{f.entry(0, "a"), f.entry(1, "b")} }, "ab"},
		{"回退不足 1 秒", func(f *fakeMem) []uint32 { return []uint32{f.entry(5, "a"), f.entry(4.5, "b")} }, "ab"},
	}
	for _, c := range cases {
		f := newFakeMem()
		f.track(testMain, testVtable, mainPath, c.build(f))
		lines, err := loadLyrics(f, testMain)
		if err != nil || texts(lines) != c.want {
			t.Errorf("%s：读到 %q err=%v，want %q", c.name, texts(lines), err, c.want)
		}
	}
}
