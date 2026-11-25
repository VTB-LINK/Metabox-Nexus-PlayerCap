package qqmusic

// 本文件只测本地歌词的规模上限（loadLocalLyrics，设计 §6.6 第 3、4 步）：每轨至多 50,000 行、逐字至多 200,000 个；
// 翻译、音译与原文合并的规模 (n+1)×(m+1) 至多 1,000,000，超出时跳过该轨、不进 alignByTime；音译复制给各行的逐字
// 总数至多 200,000、文本至多 4 MiB，超出时整轨跳过。

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

// qrcWordsLine 生成一行 QRC：起始 startMs，n 个逐字单元，每个的文本为 unit、时间为 (startMs,1)。
func qrcWordsLine(startMs, n int, unit string) string {
	return fmt.Sprintf("[%d,1000]", startMs) + strings.Repeat(fmt.Sprintf("%s(%d,1)", unit, startMs), n) + "\n"
}

// romaOf 返回 lines 中带 RomaText 的行数与 RomaDetailed 的逐字总数。
func romaOf(lines []lyricLine) (withText, words int) {
	for _, l := range lines {
		if l.RomaText != "" {
			withText++
		}
		words += len(l.RomaDetailed.Words)
	}
	return withText, words
}

// 原文逐字恰好 200,000 个可用，200,001 个不可用。
// 变异自证（实测）：去掉逐字总数判断，200,001 一例红。
func TestLoadLocalLyricsMainWordLimit(t *testing.T) {
	dir := t.TempDir()
	at := writeLyricFile(t, dir, "at.qrc", []byte(qrcWordsLine(0, maxLocalWords, "a")))
	if lines, _, err := loadLocalLyrics(readShared, at, assoc.LyricFiles{}); err != nil || len(lines) != 1 || len(lines[0].Detailed.Words) != maxLocalWords {
		t.Errorf("%d words: loadLocalLyrics = %d lines, %v; want 1 line with all words", maxLocalWords, len(lines), err)
	}
	over := writeLyricFile(t, dir, "over.qrc", []byte(qrcWordsLine(0, maxLocalWords+1, "a")))
	lines, _, err := loadLocalLyrics(readShared, over, assoc.LyricFiles{})
	mustUnusable(t, "200,001 words", lines, err)
}

// 999 行原文同一时刻、音译一行 2,000 个逐字：就近匹配把这一行配给全部 999 行，复制约 200 万个逐字，超出预算，
// 整轨跳过、原文照常。合并这一步不为任何一行生成逐字音译：分配的内存远小于复制所需的约 64 MB。只量合并这一步，
// 不量解析：解析这一行 2,000 个逐字，不带 -race 分配约 1.3 MB、带 -race 约 38 MB（实测；推断是 -race 下 sync.Pool
// 不复用 regexp 的匹配状态），与本项无关。
// 变异自证（实测）：去掉复制预算的判断即红。
func TestLoadLocalLyricsRomaCopyBudgetSkipsDuplicates(t *testing.T) {
	dir := t.TempDir()
	var main strings.Builder
	for i := range 999 {
		fmt.Fprintf(&main, "[00:00.00]m%d\n", i)
	}
	mainPath := writeLyricFile(t, dir, "main.lrc", []byte(main.String()))
	romaPath := writeLyricFile(t, dir, "roma.qrc", []byte(qrcWordsLine(0, 2000, "r")))

	lines, _, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{Roma: romaPath})
	if err != nil || len(lines) != 999 || lines[998].Text != "m998" {
		t.Fatalf("loadLocalLyrics = %d lines, %v; want the 999 main lines", len(lines), err)
	}
	if n, w := romaOf(lines); n != 0 || w != 0 {
		t.Errorf("%d lines got RomaText and %d roma words were copied; want the track skipped", n, w)
	}

	mainLines, err1 := loadLyricTrack(readShared, mainPath)
	romaLines, err2 := loadLyricTrack(readShared, romaPath)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err = mergeRomaBounded(mainLines, romaLines)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Error("mergeRomaBounded = nil, want the copy budget exceeded")
	}
	if n := after.TotalAlloc - before.TotalAlloc; n >= 1<<20 {
		t.Errorf("mergeRomaBounded allocated %d bytes, want less than 1 MiB", n)
	}
}

// 复制的逐字总数恰好 200,000 个合并，200,001 个整轨跳过（不部分合并）。两行原文配同一行 100,000 个逐字的音译，
// 第三行配另一行音译：无逐字为 200,000，有 1 个逐字为 200,001。
// 变异自证（实测）：逐字预算 `>` 改为 `>=`，200,000 一例红。
func TestLoadLocalLyricsRomaCopyWordBudget(t *testing.T) {
	dir := t.TempDir()
	mainPath := writeLyricFile(t, dir, "main.lrc", []byte("[00:00.00]m0\n[00:00.00]m1\n[00:10.00]m2\n"))
	shared := qrcWordsLine(0, maxLocalWords/2, "x")
	for _, c := range []struct {
		name, third string
		merged      bool
	}{
		{"200,000 words", "[10000,1000]y\n", true},
		{"200,001 words", "[10000,1000]y(10000,1)\n", false},
	} {
		roma := writeLyricFile(t, dir, c.name+".qrc", []byte(shared+c.third))
		lines, _, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{Roma: roma})
		if err != nil || len(lines) != 3 {
			t.Fatalf("%s: loadLocalLyrics = %d lines, %v", c.name, len(lines), err)
		}
		n, w := romaOf(lines)
		if c.merged && (n != 3 || w != maxLocalWords) || !c.merged && (n != 0 || w != 0) {
			t.Errorf("%s: %d lines got RomaText, %d roma words copied; merged should be %v", c.name, n, w, c.merged)
		}
	}
}

// 复制的音译文本恰好 4 MiB 合并，多 1 字节整轨跳过。四行原文配同一行 1 MiB 的音译（无逐字），第五行在 10 秒处：
// 没有音译与之相配为 4 MiB，配上 1 字节的音译为 4 MiB + 1。
// 变异自证（实测）：文本预算 `>` 改为 `>=`，4 MiB 一例红；去掉文本预算，4 MiB + 1 一例红。
func TestLoadLocalLyricsRomaCopyTextBudget(t *testing.T) {
	dir := t.TempDir()
	mainPath := writeLyricFile(t, dir, "main.lrc", []byte("[00:00.00]m0\n[00:00.00]m1\n[00:00.00]m2\n[00:00.00]m3\n[00:10.00]m4\n"))
	shared := "[0,1000]" + strings.Repeat("x", maxLocalTextBytes/4) + "\n"
	for _, c := range []struct {
		name, fifth string
		merged      int
	}{
		{"4 MiB", "", 4},
		{"4 MiB + 1", "[10000,1000]y\n", 0},
	} {
		roma := writeLyricFile(t, dir, c.name+".qrc", []byte(shared+c.fifth))
		lines, _, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{Roma: roma})
		if err != nil || len(lines) != 5 {
			t.Fatalf("%s: loadLocalLyrics = %d lines, %v", c.name, len(lines), err)
		}
		if n, _ := romaOf(lines); n != c.merged {
			t.Errorf("%s: %d lines got RomaText, want %d", c.name, n, c.merged)
		}
	}
}

// timedLRC 生成 n 行逐秒的 LRC，第 i 行文本为 prefix 加 i。
func timedLRC(n int, prefix string) []byte {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "[%d:%02d.00]%s%d\n", i/60, i%60, prefix, i)
	}
	return []byte(b.String())
}

// countAlign 让 alignByTime 每次调用累计到返回的计数里，测试结束时还原钩子。
func countAlign(t *testing.T) *int {
	t.Helper()
	n := 0
	orig := onAlignByTime
	onAlignByTime = func() { n++ }
	t.Cleanup(func() { onAlignByTime = orig })
	return &n
}

// 原文 50,000 行可用、50,001 行不可用；翻译 50,001 行只跳过翻译。
// 变异自证（实测）：行数判断改为 `> maxLocalLines+1`，50,001 行两例红。
func TestLoadLocalLyricsLineLimit(t *testing.T) {
	dir := t.TempDir()
	atLimit := writeLyricFile(t, dir, "at.lrc", timedLRC(maxLocalLines, "l"))
	over := writeLyricFile(t, dir, "over.lrc", timedLRC(maxLocalLines+1, "l"))
	one := writeLyricFile(t, dir, "one.lrc", timedLRC(1, "l"))

	if lines, _, err := loadLocalLyrics(readShared, atLimit, assoc.LyricFiles{}); err != nil || len(lines) != maxLocalLines {
		t.Errorf("%d lines: loadLocalLyrics = %d lines, %v; want all of them", maxLocalLines, len(lines), err)
	}
	lines, _, err := loadLocalLyrics(readShared, over, assoc.LyricFiles{})
	mustUnusable(t, "50,001 lines", lines, err)

	lines, _, err = loadLocalLyrics(readShared, one, assoc.LyricFiles{Trans: over})
	if err != nil || len(lines) != 1 || lines[0].SubText != "" {
		t.Errorf("50,001-line translation: loadLocalLyrics = %+v, %v; want the main line without SubText", lines, err)
	}
}

// (n+1)×(m+1) 恰为 1,000,000（999 行与 999 行）照常合并，1,000 行与 1,000 行跳过翻译与音译、原文照常、不调用 alignByTime。
// 变异自证（实测）：规模判断 `>` 改为 `>=`，999 行一例红；去掉规模判断，1,000 行一例红。
func TestLoadLocalLyricsMergeLimit(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		n      int
		merged bool
	}{{999, true}, {1000, false}} {
		main := writeLyricFile(t, dir, fmt.Sprintf("main%d.lrc", c.n), timedLRC(c.n, "原文"))
		files := assoc.LyricFiles{
			Trans: writeLyricFile(t, dir, fmt.Sprintf("trans%d.lrc", c.n), timedLRC(c.n, "译文")),
			Roma:  writeLyricFile(t, dir, fmt.Sprintf("roma%d.lrc", c.n), timedLRC(c.n, "yin")),
		}
		aligned := countAlign(t)
		lines, _, err := loadLocalLyrics(readShared, main, files)
		if err != nil || len(lines) != c.n {
			t.Fatalf("%d lines: loadLocalLyrics = %d lines, %v", c.n, len(lines), err)
		}
		last := lines[c.n-1]
		if last.Text != fmt.Sprint("原文", c.n-1) {
			t.Errorf("%d lines: last main line = %q", c.n, last.Text)
		}
		wantSub, wantRoma, wantAlign := "", "", 0
		if c.merged {
			wantSub, wantRoma, wantAlign = fmt.Sprint("译文", c.n-1), fmt.Sprint("yin", c.n-1), 1
		}
		if last.SubText != wantSub || last.RomaText != wantRoma || *aligned != wantAlign {
			t.Errorf("%d lines: SubText %q RomaText %q alignByTime calls %d; want %q %q %d",
				c.n, last.SubText, last.RomaText, *aligned, wantSub, wantRoma, wantAlign)
		}
	}
}
