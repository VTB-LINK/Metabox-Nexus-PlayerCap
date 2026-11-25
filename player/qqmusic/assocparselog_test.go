package qqmusic

// 本文件只测关联表解析的 Detail（设计 §6.9）：每次解析成功一条，带条目数、文件字节数与耗时，供真机核对大曲库的开销；不计每首
// 歌的 Detail 额度，同一份表再解析照打；读取或解析失败不打。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

// parsedTableLog 是关联表解析成功的那条 Detail 的英文原文。
const parsedTableLog = "Parsed the QQ Music lyric association table: %d entries, %d bytes in %v"

// 第 0 秒解析 3 条的表，打一条。第 0.5 秒原样重写，此后读四次都出错、错误各不相同（第 2、3、5、9 秒），读取失败的 Detail
// 用满当前歌曲每首 4 条的额度；第 17 秒读成，同样的那条再打一次。第 17.5 秒写坏（截断），第 19 秒读坏，不打；第 19.5 秒换成
// 5 条的表，第 20 秒读成，打一条 5 条的。泡内时钟在计算期间不前进，耗时恒为 0s。
// 变异自证（实测）：不打这条 Detail，三处计数都红；经 once 记，第 17、20 秒的两条缺而红；打在判错之前（失败也打），共 8 条而红。
func TestAssocParseLogsEntries(t *testing.T) {
	logs := captureAssocLog(t)
	three := tableDat(
		assocItem{song: midURL("003A"), lyric: `D:\a.qrc`},
		assocItem{song: idURL(100), lyric: `D:\b.qrc`, byUser: true},
		assocItem{song: `E:\m\c.flac`, lyric: `D:\c.qrc`, offset: -1000},
	)
	five := tableDat(
		assocItem{song: midURL("003A"), lyric: `D:\a.qrc`}, assocItem{song: midURL("003B"), lyric: `D:\b.qrc`},
		assocItem{song: midURL("003C"), lyric: `D:\c.qrc`}, assocItem{song: midURL("003D"), lyric: `D:\d.qrc`},
		assocItem{song: `E:\m\e.flac`, lyric: `D:\e.qrc`},
	)
	torn := five[:len(five)-3]
	busy := 0 // 只由读取者 goroutine 读写
	failTable := func(p string) error {
		if p != testTablePath {
			return nil
		}
		busy++
		return fmt.Errorf("sharing violation #%d", busy)
	}
	e := newReaderEnv()
	e.noRecord = true
	e.fs.put(testTablePath, three)
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 200, name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 500*time.Millisecond)
		e.fs.put(testTablePath, three)
		e.fs.setHooks(nil, failTable)
		sleepUntil(e, 16*time.Second)
		e.fs.setHooks(nil, nil)
		sleepUntil(e, 17500*time.Millisecond)
		e.fs.put(testTablePath, torn)
		sleepUntil(e, 19500*time.Millisecond)
		e.fs.put(testTablePath, five)
		sleepUntil(e, time.Minute)
		wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 2, 3, 5, 9, 17, 19, 20))
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
	})
	out := logs()
	if n := countLog(out, "+", strings.SplitN(i18n.T("Reading the QQ Music lyric association table failed; will retry: %v"), "%", 2)[0]); n != 4 {
		t.Fatalf("夹具：读取失败的 Detail %d 条，want 4（用满每首的额度、截断那次不再打）\n%s", n, out)
	}
	for _, c := range []struct {
		entries, size, want int
	}{
		{3, len(three), 2},
		{5, len(five), 1},
		{5, len(torn), 0},
	} {
		line := fmt.Sprintf(i18n.T(parsedTableLog), c.entries, c.size, time.Duration(0))
		if n := countLog(out, "+", line); n != c.want {
			t.Errorf("%q：%d 条，want %d\n%s", line, n, c.want, out)
		}
	}
	if n := countLog(out, "+", strings.SplitN(i18n.T(parsedTableLog), "%", 2)[0]); n != 3 {
		t.Errorf("解析的 Detail 共 %d 条，want 3\n%s", n, out)
	}
}
