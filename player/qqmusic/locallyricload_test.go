package qqmusic

// 本文件只测关联歌词文件的读取结果（loadLocalLyrics，设计 §6.6）：按内容判读 qrc 与各种编码的 lrc、读不到与不可用的
// 分类、不做均分兜底、翻译与音译的合并与只跳过该轨。规模上限在 locallyriclimits_test.go。

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/text/encoding/simplifiedchinese"

	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

const (
	testMainQRC = "[ti:测试歌]\n[ar:测试歌手]\n[offset:0]\n" +
		"[1000,2000]第(1000,500)一(1500,500)行(2000,1000)\n" +
		"[3000,2000]A&amp;B(3000,1000)C(4000,1000)\n"
	testTransLRC = "[ti:测试歌]\n[00:01.00]//\n[00:01.00]第一行译文\n[00:03.00]第二行译文\n"
	testRomaQRC  = "[ti:测试歌]\n[offset:0]\n[1000,2000]da(1000,500)i (1500,500)ichi(2000,1000)\n[3000,2000]ei(3000,1000)bi(4000,1000)\n"
	testPlainLRC = "[ti:测试歌]\n[00:01.00]第一行\n[00:03.50]第二行\n"
)

// mustUnusable 断言 err 是不可用而不是读不到。
func mustUnusable(t *testing.T, what string, lines []lyricLine, err error) {
	t.Helper()
	if !errors.Is(err, errLyricFileUnusable) || errors.Is(err, errLyricFileMissing) || lines != nil {
		t.Errorf("%s: loadLocalLyrics = %d lines, %v; want errLyricFileUnusable only", what, len(lines), err)
	}
}

// 缓存目录里的三个 qrc：原文与音译是 QRC 逐字，翻译是 LRC；原文的 &amp; 经反转义。
func TestLoadLocalLyricsQRCWithTransAndRoma(t *testing.T) {
	dir := t.TempDir()
	main := writeLyricFile(t, dir, "测试歌手 - 测试歌 - 0 - 0_qm.qrc", qrcFile(t, qrcXML(testMainQRC)))
	files := assoc.LyricFiles{
		Trans: writeLyricFile(t, dir, "测试歌手 - 测试歌 - 0 - 0_qmts.qrc", qrcFile(t, qrcXML(testTransLRC))),
		Roma:  writeLyricFile(t, dir, "测试歌手 - 测试歌 - 0 - 0_qmRoma.qrc", qrcFile(t, qrcXML(testRomaQRC))),
	}
	lines, _, err := loadLocalLyrics(readShared, main, files)
	if err != nil || len(lines) != 2 {
		t.Fatalf("loadLocalLyrics = %+v, %v; want 2 lines", lines, err)
	}
	for i, want := range []struct {
		time             float32
		text, sub, roma  string
		words, romaWords int
	}{
		{1, "第一行", "第一行译文", "dai ichi", 3, 3},
		{3, "A&BC", "第二行译文", "eibi", 2, 2},
	} {
		l := lines[i]
		if l.Time != want.time || l.Text != want.text || l.SubText != want.sub || l.RomaText != want.roma ||
			len(l.Detailed.Words) != want.words || len(l.RomaDetailed.Words) != want.romaWords {
			t.Errorf("line %d = %+v\nwant time %v text %q sub %q roma %q with %d words and %d roma words",
				i, l, want.time, want.text, want.sub, want.roma, want.words, want.romaWords)
		}
	}
}

// 无文件头的 lrc：UTF-8、UTF-8 BOM、UTF-16LE BOM、GBK 解出同样的两行。
func TestLoadLocalLyricsTextEncodings(t *testing.T) {
	gbk, err := simplifiedchinese.GBK.NewEncoder().String(testPlainLRC)
	if err != nil {
		t.Fatal(err)
	}
	u16 := []byte{0xff, 0xfe}
	for _, u := range utf16.Encode([]rune(testPlainLRC)) {
		u16 = append(u16, byte(u), byte(u>>8))
	}
	dir := t.TempDir()
	for name, b := range map[string][]byte{
		"utf8.lrc":    []byte(testPlainLRC),
		"utf8bom.lrc": append([]byte{0xef, 0xbb, 0xbf}, testPlainLRC...),
		"utf16le.lrc": u16,
		"gbk.lrc":     []byte(gbk),
	} {
		lines, _, err := loadLocalLyrics(readShared, writeLyricFile(t, dir, name, b), assoc.LyricFiles{})
		if err != nil || len(lines) != 2 || lines[0].Text != "第一行" || lines[1].Text != "第二行" || lines[1].Time != 3.5 {
			t.Errorf("%s: loadLocalLyrics = %+v, %v; want 第一行@1 and 第二行@3.5", name, lines, err)
		}
	}
}

// 路径：相对路径不可用；文件或目录不存在为读不到；超过 4 MiB 不可用；中文与 270 字符以上的长路径能读。
// 变异自证（实测）：去掉绝对路径检查，相对路径 rel.lrc 读出歌词而红。
func TestLoadLocalLyricsPaths(t *testing.T) {
	dir := t.TempDir()
	writeLyricFile(t, dir, "rel.lrc", []byte(testPlainLRC))
	t.Chdir(dir)
	for _, p := range []string{"rel.lrc", `.\rel.lrc`, `\rel.lrc`, "C:rel.lrc", ""} {
		lines, _, err := loadLocalLyrics(readShared, p, assoc.LyricFiles{})
		mustUnusable(t, "relative path "+p, lines, err)
	}

	for _, p := range []string{filepath.Join(dir, "missing.lrc"), filepath.Join(dir, "missing", "a.lrc")} {
		lines, _, err := loadLocalLyrics(readShared, p, assoc.LyricFiles{})
		if !errors.Is(err, errLyricFileMissing) || errors.Is(err, errLyricFileUnusable) || lines != nil {
			t.Errorf("%s: loadLocalLyrics = %d lines, %v; want errLyricFileMissing only", p, len(lines), err)
		}
	}

	big := []byte(testPlainLRC + strings.Repeat(" ", maxLyricFileBytes+1-len(testPlainLRC)))
	lines, _, err := loadLocalLyrics(readShared, writeLyricFile(t, dir, "big.lrc", big), assoc.LyricFiles{})
	mustUnusable(t, "4 MiB + 1 bytes", lines, err)
	lines, _, err = loadLocalLyrics(readShared, writeLyricFile(t, dir, "max.lrc", big[:maxLyricFileBytes]), assoc.LyricFiles{})
	if err != nil || len(lines) != 2 {
		t.Errorf("exactly 4 MiB: loadLocalLyrics = %d lines, %v; want 2 lines", len(lines), err)
	}

	long := dir
	for utf16Len(long) <= 270 {
		long = filepath.Join(long, "QQ音乐关联歌词_测试目录")
	}
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	p := writeLyricFile(t, long, "歌手 - 歌名.lrc", []byte(testPlainLRC))
	if lines, _, err := loadLocalLyrics(readShared, p, assoc.LyricFiles{Trans: p}); err != nil || len(lines) != 2 || lines[0].SubText != "第一行" {
		t.Errorf("long Chinese path (%d UTF-16 units): loadLocalLyrics = %+v, %v; want 2 lines", utf16Len(p), lines, err)
	}
}

// 不均分：没有带时间戳的行的文件作原文不可用，作翻译或音译不产生任何 SubText、RomaText。
func TestLoadLocalLyricsNoEvenSpread(t *testing.T) {
	dir := t.TempDir()
	main := writeLyricFile(t, dir, "main.lrc", []byte(testPlainLRC))
	for i, text := range []string{"plain\ntext", "[ti:x]", "[00:00.00]\nnot lyric data"} {
		p := writeLyricFile(t, dir, "untimed"+string(rune('0'+i))+".lrc", []byte(text))
		lines, _, err := loadLocalLyrics(readShared, p, assoc.LyricFiles{})
		mustUnusable(t, "untimed main "+text, lines, err)

		lines, _, err = loadLocalLyrics(readShared, main, assoc.LyricFiles{Trans: p, Roma: p})
		if err != nil || len(lines) != 2 {
			t.Fatalf("untimed tracks %q: loadLocalLyrics = %+v, %v; want the 2 main lines", text, lines, err)
		}
		for _, l := range lines {
			if l.SubText != "" || l.RomaText != "" {
				t.Errorf("untimed tracks %q produced sub %q roma %q", text, l.SubText, l.RomaText)
			}
		}
	}
}

// 内容不可用：带文件头而解不开的 qrc、去掉 qmc1 层的 qrc（按文本解码是乱码）、空文件。
func TestLoadLocalLyricsUnusableContent(t *testing.T) {
	dir := t.TempDir()
	valid := qrcFile(t, qrcXML(testMainQRC))
	noQMC1 := append([]byte(nil), valid...)
	testQMC1(noQMC1)
	for name, b := range map[string][]byte{
		"bad block length":        valid[:len(valid)-1],
		"ciphertext without qmc1": noQMC1[11:],
		"empty file":              nil,
	} {
		lines, _, err := loadLocalLyrics(readShared, writeLyricFile(t, dir, name, b), assoc.LyricFiles{})
		mustUnusable(t, name, lines, err)
	}
}

// 翻译或音译读不到、路径相对、解不开、超出合并规模：只跳过该轨，原文与另一轨照常；failed 依次列出跳过的各轨，读不到的
// 记为暂缺（missing），读成了而超出合并规模的记为 skipped，其余记为文件本身不可用，供读取者看守与重读。全部读成时 failed 为空。
// 变异自证（实测）：附加轨读不到也记为不可用，「translation missing」「romanization missing」「both broken」三例红；
// 超出合并规模不入 failed、或入了而不标 skipped，「merge limit」一例都红。
func TestLoadLocalLyricsTrackFailureSkipsOnlyThatTrack(t *testing.T) {
	dir := t.TempDir()
	main := writeLyricFile(t, dir, "main.qrc", qrcFile(t, qrcXML(testMainQRC)))
	trans := writeLyricFile(t, dir, "trans.qrc", qrcFile(t, qrcXML(testTransLRC)))
	roma := writeLyricFile(t, dir, "roma.qrc", qrcFile(t, qrcXML(testRomaQRC)))
	broken := writeLyricFile(t, dir, "broken.qrc", append(qrcFile(t, qrcXML(testRomaQRC)), 0))
	missing := filepath.Join(dir, "missing.qrc")
	for _, c := range []struct {
		name            string
		files           assoc.LyricFiles
		wantSub, wantRo bool
		wantFailed      []trackFailure
	}{
		{"both tracks", assoc.LyricFiles{Trans: trans, Roma: roma}, true, true, nil},
		{"no tracks", assoc.LyricFiles{}, false, false, nil},
		{"translation missing", assoc.LyricFiles{Trans: missing, Roma: roma}, false, true, []trackFailure{{missing, true, false}}},
		{"translation relative", assoc.LyricFiles{Trans: "trans.qrc", Roma: roma}, false, true, []trackFailure{{"trans.qrc", false, false}}},
		{"translation unusable", assoc.LyricFiles{Trans: broken, Roma: roma}, false, true, []trackFailure{{broken, false, false}}},
		{"romanization broken", assoc.LyricFiles{Trans: trans, Roma: broken}, true, false, []trackFailure{{broken, false, false}}},
		{"romanization missing", assoc.LyricFiles{Trans: trans, Roma: missing}, true, false, []trackFailure{{missing, true, false}}},
		{"both broken", assoc.LyricFiles{Trans: broken, Roma: missing}, false, false, []trackFailure{{broken, false, false}, {missing, true, false}}},
	} {
		lines, failed, err := loadLocalLyrics(readShared, main, c.files)
		if err != nil || len(lines) != 2 || lines[0].Text != "第一行" {
			t.Errorf("%s: loadLocalLyrics = %+v, %v; want the 2 main lines", c.name, lines, err)
			continue
		}
		if !slices.Equal(failed, c.wantFailed) {
			t.Errorf("%s: failed = %+v, want %+v", c.name, failed, c.wantFailed)
		}
		if got := lines[0].SubText != ""; got != c.wantSub {
			t.Errorf("%s: has SubText = %v, want %v", c.name, got, c.wantSub)
		}
		if got := lines[0].RomaText != "" && len(lines[0].RomaDetailed.Words) > 0; got != c.wantRo {
			t.Errorf("%s: has RomaText = %v, want %v", c.name, got, c.wantRo)
		}
	}

	// 超出合并规模（1,000 行与 1,000 行）：读成了也跳过，记为 skipped（不是文件本身不可用）。
	big := writeLyricFile(t, dir, "big.lrc", timedLRC(1000, "原文"))
	bigTrans := writeLyricFile(t, dir, "bigtrans.lrc", timedLRC(1000, "译文"))
	lines, failed, err := loadLocalLyrics(readShared, big, assoc.LyricFiles{Trans: bigTrans})
	if err != nil || len(lines) != 1000 || lines[0].SubText != "" {
		t.Errorf("merge limit: loadLocalLyrics = %d lines, %v; want 1000 main lines without SubText", len(lines), err)
	}
	if want := []trackFailure{{bigTrans, false, true}}; !slices.Equal(failed, want) {
		t.Errorf("merge limit: failed = %+v, want %+v", failed, want)
	}
}

// 原文读不成：failed 只有原文一项（读不到为暂缺，路径相对、解不开为不可用），翻译、音译都不读。
// 变异自证（实测）：原文失败时 failed 为空，三例都红。
func TestLoadLocalLyricsMainFailureReportsOnlyMain(t *testing.T) {
	dir := t.TempDir()
	trans := writeLyricFile(t, dir, "trans.qrc", qrcFile(t, qrcXML(testTransLRC)))
	broken := writeLyricFile(t, dir, "broken.qrc", append(qrcFile(t, qrcXML(testMainQRC)), 0))
	missing := filepath.Join(dir, "missing.qrc")
	for _, c := range []struct {
		name, main string
		missing    bool
	}{
		{"main missing", missing, true},
		{"main unusable", broken, false},
		{"main relative", "main.qrc", false},
	} {
		var read []string
		counting := func(path string, max int64) ([]byte, error) {
			read = append(read, path)
			return readShared(path, max)
		}
		lines, failed, err := loadLocalLyrics(counting, c.main, assoc.LyricFiles{Trans: trans, Roma: trans})
		if err == nil || lines != nil {
			t.Errorf("%s: loadLocalLyrics = %d lines, %v; want an error", c.name, len(lines), err)
		}
		if want := []trackFailure{{c.main, c.missing, false}}; !slices.Equal(failed, want) {
			t.Errorf("%s: failed = %+v, want %+v", c.name, failed, want)
		}
		if slices.Contains(read, trans) {
			t.Errorf("%s: read the translation after the main track failed", c.name)
		}
	}
}
