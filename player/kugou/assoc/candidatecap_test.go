package assoc

// 本文件只测候选的 64 条上限（设计 §6.5 扫描顺序、§6.10）：去重后的 LyricKey 超过 64 个时放弃关联一级，
// 缓存一级只留 UpdateTime 最大的 64 条；两处各打一条 Detail，未超限不打。

import (
	"bytes"
	"fmt"
	stdlog "log"
	"os"
	"strings"
	"testing"
)

// captureLog 返回 f 执行期间经标准库 log 输出的内容（logger 的各方法都经它输出）。
func captureLog(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	stdlog.SetOutput(&buf)
	defer stdlog.SetOutput(os.Stderr)
	f()
	return buf.String()
}

// manyKeysDB 造一库：hash h 有 n 个不同的 LyricKey（第 i 个的 UpdateTime 为 1000+i，最后一个最新），
// 交替分在两张表；另有 3 条缓存、others 个属于别的 hash 的 LyricKey。
func manyKeysDB(h string, n, others int) *fakeTables {
	var a, b []songRow
	var rows []lyricRow
	for i := range n {
		k := hex32("bb", i)
		if i%2 == 0 {
			a = append(a, songRow{h, k})
		} else {
			b = append(b, songRow{h, k})
		}
		rows = append(rows, lyricRow{k, `D:\assoc` + k + ".krc", "", 1000 + int64(i)})
	}
	for i := range others {
		k := hex32("dd", i)
		a = append(a, songRow{hex32("ee", i), k})
		rows = append(rows, lyricRow{k, `D:\other` + k + ".krc", "", 5000 + int64(i)})
	}
	for i := range 3 {
		rows = append(rows, lyricRow{hex32("cc", i), fmt.Sprintf("cache%d.lrc", i), h, int64(i)})
	}
	return newDB(songTable("LocalFile", a...), songTable("PlayQueueAssistItem_0", b...), lyricInfo(rows...))
}

func cacheTail() []candidate {
	return []candidate{{"cache2.lrc", SourceCache, 2}, {"cache1.lrc", SourceCache, 1}, {"cache0.lrc", SourceCache, 0}}
}

func TestCollectTooManyKeysSkipsManual(t *testing.T) {
	h := hex32("aa", 1)
	var got []candidate
	out := captureLog(t, func() { got = mustCollect(t, manyKeysDB(h, 65, 0), h) })
	wantCandidates(t, got, cacheTail())
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "[KuGou] [Assoc] [+]") || !strings.Contains(out, h) {
		t.Fatalf("日志 = %q，want 一条带 hash 的 Detail", out)
	}

	// 64 个：64 条关联在前（最新的在最前），3 条缓存在后，不打日志。
	out = captureLog(t, func() { got = mustCollect(t, manyKeysDB(h, 64, 0), h) })
	if len(got) != 67 || got[0] != (candidate{`D:\assoc` + hex32("bb", 63) + ".krc", SourceAssociation, 1063}) ||
		got[63] != (candidate{`D:\assoc` + hex32("bb", 0) + ".krc", SourceAssociation, 1000}) {
		t.Fatalf("64 个关联：候选 = %+v", got)
	}
	wantCandidates(t, got[64:], cacheTail())
	if out != "" {
		t.Fatalf("64 个关联：日志 = %q，want 无", out)
	}

	// 反例：别的 hash 的 LyricKey 不计数。本 hash 的 64 个加别的 100 个，关联照常取。
	if got = mustCollect(t, manyKeysDB(h, 64, 100), h); len(got) != 67 || got[0].Source != SourceAssociation {
		t.Fatalf("别的 hash 的 LyricKey 被计数：候选 %+v", got)
	}
}

// 同一个 LyricKey 出现在两张表的 70 行：按 1 个计，关联照常取到且只出现一次。
func TestCollectDuplicateKeysCountOnce(t *testing.T) {
	h, k := hex32("aa", 1), hex32("bb", 1)
	var a, b []songRow
	for i := range 70 {
		if i < 35 {
			a = append(a, songRow{h, k})
		} else {
			b = append(b, songRow{h, k})
		}
	}
	db := newDB(songTable("LocalFile", a...), songTable("RecentlyPlayedItem", b...), lyricInfo(lyricRow{k, `D:\assoc.krc`, "", 1}))
	wantCandidates(t, mustCollect(t, db, h), []candidate{{`D:\assoc.krc`, SourceAssociation, 1}})
}

// cacheRows 返回 hash h 的 n 条缓存，UpdateTime 为 0..n-1，第 i 行的 UpdateTime 为 order(i)。
func cacheRows(h string, n int, order func(i int) int) []lyricRow {
	var rows []lyricRow
	for i := range n {
		ut := order(i)
		rows = append(rows, lyricRow{hex32("cc", ut), fmt.Sprintf("c%d.lrc", ut), h, int64(ut)})
	}
	return rows
}

// 100 条缓存，以升序、降序、打乱三种次序写入：结果都是 UpdateTime 最大的 64 条、降序，并打一条 Detail。
// 恰 64 条：全留、不打日志。另有一条关联时它在前，不占缓存的 64 条。
func TestCollectCapsCache(t *testing.T) {
	h := hex32("aa", 1)
	var want []candidate
	for ut := 99; ut >= 36; ut-- {
		want = append(want, candidate{fmt.Sprintf("c%d.lrc", ut), SourceCache, int64(ut)})
	}
	shuffled := func(i int) int { return i * 37 % 100 } // 37 与 100 互素，是 0..99 的一个排列
	orders := map[string]func(int) int{
		"升序": func(i int) int { return i },
		"降序": func(i int) int { return 99 - i },
		"打乱": shuffled,
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			var got []candidate
			out := captureLog(t, func() { got = mustCollect(t, newDB(lyricInfo(cacheRows(h, 100, order)...)), h) })
			wantCandidates(t, got, want)
			if strings.Count(out, "\n") != 1 || !strings.Contains(out, h) || !strings.Contains(out, "100") {
				t.Fatalf("日志 = %q，want 一条带 hash 与条数的 Detail", out)
			}
		})
	}

	var got []candidate
	out := captureLog(t, func() {
		got = mustCollect(t, newDB(lyricInfo(cacheRows(h, 64, func(i int) int { return i * 5 % 64 })...)), h)
	})
	if len(got) != 64 || got[0].UpdateTime != 63 || got[63].UpdateTime != 0 || out != "" {
		t.Fatalf("恰 64 条：候选 %+v，日志 %q", got, out)
	}

	k := hex32("bb", 1)
	db := newDB(songTable("LocalFile", songRow{h, k}),
		lyricInfo(append(cacheRows(h, 100, shuffled), lyricRow{k, `D:\assoc.krc`, "", 0})...))
	wantCandidates(t, mustCollect(t, db, h), append([]candidate{{`D:\assoc.krc`, SourceAssociation, 0}}, want...))
}
