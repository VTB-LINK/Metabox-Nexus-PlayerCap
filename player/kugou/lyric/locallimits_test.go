package lyric

// 本文件只测本地歌词的上限（ParseLocal 与 parseLRCBounded）：文件大小、行数、逐字总数与展开文本
// 总量，各取上限与上限加一两侧。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func wantTooLarge(t *testing.T, b []byte) {
	t.Helper()
	lines, err := ParseLocal(b)
	if !errors.Is(err, ErrLyricTooLarge) || lines != nil {
		t.Fatalf("得 (%d 行, %v)，want (nil, ErrLyricTooLarge)", len(lines), err)
	}
}

func wantLineCount(t *testing.T, b []byte, n int) {
	t.Helper()
	if lines := mustParseLocal(t, b); len(lines) != n {
		t.Fatalf("得 %d 行，want %d", len(lines), n)
	}
}

// 上限取设计 §6.6 的值。其余用例按常量取两侧，常量被误改时它们照样通过，由本例钉住。
func TestLocalLimitsMatchDesign(t *testing.T) {
	got := [5]int{MaxLocalFileBytes, maxKRCPlainBytes, maxLines, maxWords, maxTextBytes}
	if want := [5]int{4 << 20, 4 << 20, 50000, 200000, 4 << 20}; got != want {
		t.Fatalf("文件、明文、行数、逐字、文本上限 = %v，want %v", got, want)
	}
}

// 超过 4 MiB 的文件不解析；恰好 4 MiB 照常解析。
func TestParseLocalTooLarge(t *testing.T) {
	pad := func(n int) []byte {
		b := []byte("[00:01.00]听说\n")
		return append(b, strings.Repeat(" ", n-len(b))...)
	}
	wantTooLarge(t, pad(MaxLocalFileBytes+1))
	wantLineCount(t, pad(MaxLocalFileBytes), 1)
}

func TestParseLocalLineLimit(t *testing.T) {
	t.Run("LRC", func(t *testing.T) {
		wantTooLarge(t, lrcLines(maxLines+1))
		wantLineCount(t, lrcLines(maxLines), maxLines)
	})
	t.Run("明文 KRC", func(t *testing.T) {
		wantTooLarge(t, []byte(krcLines(maxLines+1, "x")))
		wantLineCount(t, []byte(krcLines(maxLines, "x")), maxLines)
	})
}

// 一行带很多时间戳的 LRC 展开成多行、各计一次文本：超过 maxTextBytes 即停，不构造完整结果。
func TestParseLocalExpandedTextLimit(t *testing.T) {
	text := strings.Repeat("a", 1024)
	generated := 0
	orig := onBoundedLRCLine
	onBoundedLRCLine = func() { generated++ }
	t.Cleanup(func() { onBoundedLRCLine = orig })

	b := lrcOneLine(20000, text)
	if len(b) >= MaxLocalFileBytes {
		t.Fatalf("夹具 %d bytes，应小于 %d", len(b), MaxLocalFileBytes)
	}
	wantTooLarge(t, b)
	if limit := maxTextBytes/1024 + 1; generated > limit {
		t.Fatalf("停下时已生成 %d 行，应不超过 %d", generated, limit)
	}

	// 恰好 maxTextBytes 成功，多一个时间戳超限。
	wantLineCount(t, lrcOneLine(maxTextBytes/1024, text), maxTextBytes/1024)
	wantTooLarge(t, lrcOneLine(maxTextBytes/1024+1, text))
}

// krcText 生成 n 行 KRC 明文，每行一个字、字的文本为 1000 个 a；head 放在开头（[language:] 标签）。
func krcText(head string, n int) []byte {
	return []byte(head + krcLines(n, strings.Repeat("a", 1000)))
}

// 展开文本总量计入各行 Text、SubText、RomaText 与全部逐字 Text。每行 Text 与逐字各 1000 bytes：
// 2000 行共 4,000,000 bytes，未超 4 MiB（4,194,304）；以下各超限例为 4,200,000 bytes 或更多。
func TestParseLocalKRCTextTotal(t *testing.T) {
	trans := make([][]string, 2000)
	roma := make([][]string, 2000)
	for i := range trans {
		trans[i] = []string{strings.Repeat("c", 100)} // 2000 × 100 = 200,000
		roma[i] = []string{strings.Repeat("r", 50)}   // RomaText 与逐字音译各 2000 × 50 = 100,000，只计其一则不超
	}
	cases := []struct {
		name  string
		plain []byte
		lines int // 0 表示应超限
	}{
		{"明文近 4 MiB", krcText("", 4000), 0}, // 展开 8,000,000
		{"计入逐字", krcText("", 2100), 0},      // Text 2,100,000 未超，加逐字 4,200,000
		{"计入翻译", krcText(languageTag(t, 1, trans), 2000), 0},
		{"计入音译与逐字音译", krcText(languageTag(t, 0, roma), 2000), 0},
		{"未超", krcText("", 2000), 2000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := krcRaw(t, c.plain)
			if len(raw) >= MaxLocalFileBytes || len(c.plain) > maxKRCPlainBytes {
				t.Fatalf("夹具：krc %d bytes、明文 %d bytes", len(raw), len(c.plain))
			}
			if c.lines == 0 {
				wantTooLarge(t, raw)
				return
			}
			wantLineCount(t, raw, c.lines)
		})
	}
	if n := len(krcText("", 4000)); n < 3900<<10 {
		t.Fatalf("夹具：明文 %d bytes，应接近 4 MiB", n)
	}
}

// 逐字总数为 Detailed.Words 与 RomaDetailed.Words 之和：各 100,000 共 200,000 成功，再多一字超限。
func TestParseLocalWordLimit(t *testing.T) {
	const perLine = 100
	const lines = maxWords / 2 / perLine
	roma := make([][]string, lines)
	var body strings.Builder
	for i := range lines {
		fmt.Fprintf(&body, "[%d,1000]", i*1000)
		roma[i] = make([]string, perLine)
		for j := range perLine {
			fmt.Fprintf(&body, "<%d,10,0>a", j*10)
			roma[i][j] = "r"
		}
		body.WriteString("\n")
	}
	plain := languageTag(t, 0, roma) + body.String()

	got := mustParseLocal(t, krcRaw(t, []byte(plain)))
	words := 0
	for _, l := range got {
		words += len(l.Detailed.Words) + len(l.RomaDetailed.Words)
	}
	if len(got) != lines || words != maxWords {
		t.Fatalf("得 %d 行、逐字 %d，want %d 行、%d", len(got), words, lines, maxWords)
	}

	over := plain + fmt.Sprintf("[%d,10]<0,10,0>a\n", lines*1000)
	wantTooLarge(t, krcRaw(t, []byte(over)))
}
