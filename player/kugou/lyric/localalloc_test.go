package lyric

// 本文件只测畸形 krc1 的解析分配：明文已在 4 MiB 之内，解析时的分配总量不得再随行数、单行的逐字
// 标记数或 [language:] 轨的行数放大。分配上限只在非 race 构建下断言（见 racebuild_race_test.go），
// 返回值在两种构建下都断言。

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player/krc"
)

// allocDuring 返回 f 执行期间的累计堆分配字节数（TotalAlloc 增量）与 f 返回时相对执行前的堆增量
// （HeapAlloc 之差，含尚未回收的垃圾；期间发生过 GC 时可为负）。
func allocDuring(f func()) (total uint64, heap int64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

// langBombBody 是 [language:] 轨膨胀一例的正文：3 行，各 2 字。
const langBombBody = "[0,1000]<0,500,0>听<500,500,0>说\n" +
	"[1000,1000]<0,500,0>你<500,500,0>好\n" +
	"[2000,1000]<0,500,0>再<500,500,0>见\n"

// langBomb 返回标签文本约 tagBytes、翻译与音译轨每行一个单字片段的 KRC 明文（每对行在标签里约
// 16 bytes），正文为 langBombBody。
func langBomb(t *testing.T, tagBytes int) []byte {
	rows := (tagBytes - 200) / 16
	trans := make([][]string, rows)
	roma := make([][]string, rows)
	for i := range rows {
		trans[i] = []string{"c"}
		roma[i] = []string{"r"}
	}
	return []byte(languageTags(t, langBlock{LyricContent: trans, Type: 1}, langBlock{LyricContent: roma, Type: 0}) + langBombBody)
}

func wantTooLargeResult(t *testing.T, lines []Line, err error) {
	t.Helper()
	if !errors.Is(err, ErrLyricTooLarge) || lines != nil {
		t.Fatalf("得 (%d 行, %v)，want (nil, ErrLyricTooLarge)", len(lines), err)
	}
}

func wantNoLinesResult(t *testing.T, lines []Line, err error) {
	t.Helper()
	if lines != nil || err != nil {
		t.Fatalf("得 (%d 行, %v)，want (nil, nil)", len(lines), err)
	}
}

// 标签超长：正文 3 行照常解出，翻译与音译全空。
func wantLangDropped(t *testing.T, lines []Line, err error) {
	t.Helper()
	if err != nil || len(lines) != 3 || lines[1].Text != "你好" {
		t.Fatalf("得 (%+v, %v)，want 正文 3 行", lines, err)
	}
	for i, l := range lines {
		if l.SubText != "" || l.RomaText != "" {
			t.Fatalf("第 %d 行 SubText=%q、RomaText=%q，want 均为空", i, l.SubText, l.RomaText)
		}
	}
}

// 标签在上限内：正文 3 行，翻译与音译照常。
func wantLangKept(t *testing.T, lines []Line, err error) {
	t.Helper()
	if err != nil || len(lines) != 3 || lines[2].SubText != "c" || lines[2].RomaText != "r" {
		t.Fatalf("得 (%+v, %v)，want 正文 3 行、译文 c、音译 r", lines, err)
	}
}

func TestParseLocalAllocBounded(t *testing.T) {
	fill := func(line string) []byte {
		return []byte(strings.Repeat(line, maxKRCPlainBytes/len(line)))
	}
	bomb := langBomb(t, maxKRCPlainBytes-len(langBombBody))
	// 夹具自证：同一明文交给不设上限的解析，译文与音译照常取到；标签超长丢弃才使下面一例全空。
	if full := krc.ParsePlainKRC(string(bomb)); len(full) != 3 || full[0].SubText != "c" || full[0].RomaText != "r" {
		t.Fatalf("夹具：不设上限的解析得 %d 行，want 3 行、译文 c、音译 r", len(full))
	}
	cases := []struct {
		name   string
		plain  []byte
		minLen int    // 明文至少这么长，确认夹具接近要测的上限
		budget uint64 // 累计分配上限
		check  func(*testing.T, []Line, error)
	}{
		{"明文全是换行", fill("\n"), maxKRCPlainBytes - 1<<10, 64 << 20, wantNoLinesResult},
		{"最短的产出行", fill("[0,1]<0,1,0>a\n"), maxKRCPlainBytes - 1<<10, 64 << 20, wantTooLargeResult},
		{"最短的 [start,dur] 行（无逐字、不产出行）", fill("[0,1]a\n"), maxKRCPlainBytes - 1<<10, 64 << 20, wantNoLinesResult},
		{"单行塞满逐字标记", []byte("[0,1]" + strings.Repeat("<0,1,0>a", (maxKRCPlainBytes-6)/8) + "\n"), maxKRCPlainBytes - 1<<10, 64 << 20, wantTooLargeResult},
		{"[language:] 轨膨胀", bomb, maxKRCPlainBytes - 1<<10, 64 << 20, wantLangDropped},
		// 上限内最坏的标签：实测解码一次约 19 MiB，三次约 56 MiB，32 MiB 守住「只解码一次」。
		{"[language:] 标签接近 1 MiB", langBomb(t, 1<<20), 1<<20 - 1<<10, 32 << 20, wantLangKept},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := krcRaw(t, c.plain)
			if len(raw) > 64<<10 || len(c.plain) > maxKRCPlainBytes || len(c.plain) < c.minLen {
				t.Fatalf("夹具：krc %d bytes、明文 %d bytes，应为 64 KiB 以内、明文在 %d 与 %d 之间", len(raw), len(c.plain), c.minLen, maxKRCPlainBytes)
			}
			var lines []Line
			var err error
			total, heap := allocDuring(func() { lines, err = ParseLocal(raw) })
			c.check(t, lines, err)
			t.Logf("krc %d bytes：累计分配 %.1f MiB，返回时堆增量 %.1f MiB", len(raw), float64(total)/(1<<20), float64(heap)/(1<<20))
			if total > c.budget && !raceBuild {
				t.Fatalf("累计分配超过 %d MiB", c.budget>>20)
			}
		})
	}
}
