package qqmusic

// 本文件只测读取者怎样读 QQ 内存里当前歌曲的记录认键（设计 §6.4 第 3～5 步、§6.8「输出」、§6.9）：有候选即读，不论 byUser
// 与偏移；按记录的网址逐字节选条目，认不出按无条目（在线、偏移 0）；每个代次一轮（第 0、1、2、4、8 秒，认出之后照读完），
// 各次的结论并成本代次的结论，只在认出与不唯一时改动，变了才重新求结果；日程用完而本代次不是认出时上报一次；结论按代次缓存，
// 关联表变化只按缓存重选；扫描不完整不下结论；id 键与 mid 键的记录并存时取 id 键；后缀误命中不显示别的歌的关联歌词；前缀用
// Struct1 的原样歌名；扫描在块间察觉新写入的歌或会话取消即中止；待换算之后编号补上即补读一次，不占日程；窄字符版本（22.16）
// 照样认键。编号怎样补全见 assocfill_test.go。QQ 的内存是假的（fakeMem）。

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

const lyrB = `D:\Lyrics\b_qm.qrc`

// mid12A 是 mid 003A 的 stream12 写法：与 midURL("003A") 同为 mid 键、网址不同，两者并存即不唯一（第 5 版 id 键与 mid 键并存
// 取 id 键，不再以此造不唯一）。
const mid12A = "http://stream12.qqmusic.qq.com/003A.wma"

// recordEnv 的关联表为 items，关联歌词 lyrOK、lyrB 分别是 testAssocLRC、testAssocLRC2（即 assocLines、assocLines2）。
func recordEnv(items ...assocItem) *readerEnv {
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(items...))
	e.fs.put(lyrOK, []byte(testAssocLRC))
	e.fs.put(lyrB, []byte(testAssocLRC2))
	return e
}

// withScanLimits 把扫描的上限换成 lim，用例结束时还原。须在启动读取者之前调用。
func withScanLimits(t *testing.T, lim scanLimits) {
	t.Helper()
	orig := recordScanLimits
	recordScanLimits = lim
	t.Cleanup(func() { recordScanLimits = orig })
}

// 没有候选时不读记录，结果为在线、偏移 0（设计 §6.4 第 3 步）：没有条目；条目属于别的歌；只有归不进的写法（带查询串、
// example.com、本地路径）；songmid 缺位而表里只有本首的 mid 键条目（本任务只用 songKey 自带的编号）。编号缺位（本地文件）与
// 关联表不可用也不读。内存里都放着这首歌的记录：一旦读了，扫描次数即不为 0。反例：只有一条 byUser=0、偏移 0 的 mid 键条目、
// songmid 已知 → 第 0 秒即读。
// 变异自证（实测）：Candidates 恢复相关过滤，反例不读而红；不看有无候选、一律读，前四例扫描了而红。
func TestAssocRecordNotScannedWithoutCandidates(t *testing.T) {
	song := songKey{gen: 1, songID: 100, songMid: "003Song", name: "百"}
	for _, c := range []struct {
		name    string
		items   []assocItem // nil 即关联表不存在
		key     songKey
		src     lyricSource
		scanned bool
	}{
		{"没有条目", []assocItem{}, song, srcOnline, false},
		{"条目属于别的歌", []assocItem{{song: idURL(9), lyric: lyrOK, byUser: true}, {song: midURL("003Other"), lyric: lyrB, offset: 300}}, song, srcOnline, false},
		{"只有归不进的写法", []assocItem{
			{song: midURL("003Song") + "?fromtag=1", lyric: lyrOK, byUser: true}, {song: "http://example.com/100.wma", lyric: lyrB},
			{song: `E:\Music\百.flac`, lyric: lyrOK, offset: 300},
		}, song, srcOnline, false},
		{"songmid 缺位、只有本首的 mid 键条目", []assocItem{{song: midURL("003Song"), lyric: lyrOK, byUser: true}}, songKey{gen: 1, songID: 100, name: "百"}, srcOnline, false},
		{"编号缺位", []assocItem{{song: midURL("003Song"), lyric: lyrOK, byUser: true}, {song: idURL(100), lyric: lyrB, offset: 300}}, songKey{gen: 1, name: "百"}, srcNone, false},
		{"关联表不存在", nil, song, srcOnline, false},
		{"反例：只有一条 byUser=0、偏移 0 的 mid 键条目", []assocItem{{song: midURL("003Song"), lyric: lyrOK}}, song, srcOnline, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newReaderEnv()
			if c.items != nil {
				e = recordEnv(c.items...)
			}
			e.mem.play("百", midURL("003Song"), "003Song")
			e.mem.putRecord(fakeAlbum, fakeSinger, "百", idURL(100), "003Song")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(c.key)
				got := latestResult(t, r)
				if c.items == nil {
					if got.kind != resUnknown || got.gen != 1 {
						t.Fatalf("关联表不存在：结果 %+v，want gen 1 的未知", got)
					}
				} else {
					wantDesired(t, got, 1, c.src, 0, false, "")
				}
				sleepUntil(e, 30*time.Second)
				scans := e.mem.scanTimes()
				if c.scanned {
					if len(scans) == 0 || scans[0] != 0 {
						t.Fatalf("扫描时刻 %v，want 第 0 秒起读", scans)
					}
					return
				}
				wantTimes(t, "扫描", scans, nil)
			})
		})
	}
}

// 有候选即读，按记录网址逐字节选条目，选中的条目不论 byUser 都读其歌词（设计 §7「选条目」「记录认出」「取用」）。多数例
// songid 为 100、songmid 为 003A（两个编号都已知，即会话补全编号之后）；各例第 0 秒即扫描一次（各条结果相同也读）。
// 变异自证（实测）：不按记录、取第一条候选，十一例红；选条目的网址比较改为按 ClassifySongURL 归一，只差主机号、主机名大小写、
// 协议与「同一 songid 三条」四例红；取用恢复只看 byUser=1，byUser=0 的六例红；恢复「所选条目须在候选里」，「只有 songmid、
// 记录为 id 键」一例红。
func TestAssocRecordSelectsByRecord(t *testing.T) {
	const s12, s4 = "http://stream12.qqmusic.qq.com/100.wma", "http://stream4.qqmusic.qq.com/100.wma"
	both := songKey{gen: 1, songID: 100, songMid: "003A", name: "百"}
	idOnly := songKey{gen: 1, songID: 100, name: "百"}
	midOnly := songKey{gen: 1, songMid: "003A", name: "百"}
	for _, c := range []struct {
		name        string
		items       []assocItem
		key         songKey
		url, param0 string
		src         lyricSource
		offset      int
		lines       []string
	}{
		{"mid 键 byUser=1、id 键 byUser=0，记录为 mid 键", []assocItem{{song: midURL("003A"), lyric: lyrOK, byUser: true}, {song: idURL(100), lyric: lyrB}},
			both, midURL("003A"), "003A", srcAssoc, 0, assocLines},
		{"mid 键 byUser=1、id 键 byUser=0，记录为 id 键", []assocItem{{song: midURL("003A"), lyric: lyrOK, byUser: true}, {song: idURL(100), lyric: lyrB}},
			both, idURL(100), "003A", srcAssoc, 0, assocLines2},
		{"mid 键 byUser=0、id 键 byUser=1，记录为 id 键", []assocItem{{song: midURL("003A"), lyric: lyrOK}, {song: idURL(100), lyric: lyrB, byUser: true}},
			both, idURL(100), "003A", srcAssoc, 0, assocLines2},
		{"mid 键 byUser=0、id 键 byUser=1，记录为 mid 键", []assocItem{{song: midURL("003A"), lyric: lyrOK}, {song: idURL(100), lyric: lyrB, byUser: true}},
			both, midURL("003A"), "003A", srcAssoc, 0, assocLines},
		{"两种键都 byUser=1，记录为 mid 键", []assocItem{{song: midURL("003A"), lyric: lyrOK, byUser: true}, {song: idURL(100), lyric: lyrB, byUser: true}},
			both, midURL("003A"), "003A", srcAssoc, 0, assocLines},
		{"两种键都 byUser=1，记录为 id 键", []assocItem{{song: midURL("003A"), lyric: lyrOK, byUser: true}, {song: idURL(100), lyric: lyrB, byUser: true}},
			both, idURL(100), "003A", srcAssoc, 0, assocLines2},
		{"两种键都 byUser=0 且歌词相同（lyricUrl 只差大小写）", []assocItem{{song: midURL("003A"), lyric: lyrOK}, {song: idURL(100), lyric: strings.ToUpper(lyrOK)}},
			both, idURL(100), "003A", srcAssoc, 0, assocLines},
		{"同一 songid 三条，记录为 stream4（设计审的反例）", []assocItem{
			{song: midURL("003A"), lyric: lyrOK, byUser: true}, {song: s12, lyric: lyrOK, byUser: true}, {song: s4, lyric: lyrB, byUser: true},
		}, both, s4, "003A", srcAssoc, 0, assocLines2},
		{"只有一条 byUser=0、偏移 700", []assocItem{{song: idURL(100), lyric: lyrOK, offset: 700}},
			idOnly, idURL(100), "003A", srcAssoc, 700, assocLines},
		{"只有一条 byUser=0、偏移 0 的 mid 键条目", []assocItem{{song: midURL("003A"), lyric: lyrB}},
			midOnly, midURL("003A"), "003A", srcAssoc, 0, assocLines2},
		{"记录与条目只差主机号", []assocItem{{song: s12, lyric: lyrOK, byUser: true}}, idOnly, s4, "003A", srcOnline, 0, nil},
		{"记录与条目只差主机名大小写", []assocItem{{song: "http://Stream4.QQMusic.qq.com/003A.wma", lyric: lyrOK, byUser: true}},
			both, midURL("003A"), "003A", srcOnline, 0, nil},
		{"记录与条目只差协议", []assocItem{{song: "https://stream4.qqmusic.qq.com/003A.wma", lyric: lyrOK, byUser: true}},
			both, midURL("003A"), "003A", srcOnline, 0, nil},
		{"记录网址在表里不存在", []assocItem{{song: idURL(100), lyric: lyrOK, byUser: true}}, both, midURL("003A"), "003A", srcOnline, 0, nil},
		{"只有 songid，记录为 mid 键而表里有它的条目（待换算）", []assocItem{{song: idURL(100), lyric: lyrB, offset: 300}, {song: midURL("003A"), lyric: lyrOK}},
			idOnly, midURL("003A"), "003A", srcOnline, 0, nil},
		{"只有 songmid，记录为 id 键（参数 0 相同）", []assocItem{{song: midURL("003A"), lyric: lyrOK}, {song: idURL(100), lyric: lyrB}},
			midOnly, idURL(100), "003A", srcAssoc, 0, assocLines2},
		{"songmid 已知，参数 0 不同（作废）", []assocItem{{song: idURL(100), lyric: lyrOK, byUser: true}},
			both, idURL(100), "003B", srcOnline, 0, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := recordEnv(c.items...)
			e.mem.play("百", c.url, c.param0)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(c.key)
				got := latestResult(t, r)
				wantDesired(t, got, 1, c.src, c.offset, false, "", c.lines...)
				if got.reopen != 0 {
					t.Fatalf("结果的 reopen %d，want 0", got.reopen)
				}
				wantTimes(t, "扫描", e.mem.scanTimes(), secs(0))
			})
		})
	}
}

// 日程（设计 §6.4 第 4 步「何时读」）：找不到时第 0、1、2、4、8 秒各扫一次后停，期间结果一直是在线、偏移 0、不再另发；
// 用完时上报一次 qqmusic.song_record_unresolved，带 QQ 版本与本代次的结论、不带歌名。第 2 秒认出：照日程扫到第 8 秒，不上报。
// 认出 A 而第 8 秒不唯一：本代次作废、结果转为在线，上报一次，结论与命中数取不唯一的那一次；认出 A 而第 8 秒找不到：保持 A，
// 不发结果、不上报。
// 变异自证（实测）：认出即停，第 2 秒认出一例与第 8 秒的两例扫描时刻不符而红；日程用完后每 8 秒再读，找不到一例扫到第 56 秒
// 而红；用完不上报而红；上报看这一次的结论而不看本代次的，「第 8 秒找不到」一例上报了而红。
func TestAssocRecordSchedule(t *testing.T) {
	items := []assocItem{{song: idURL(100), lyric: lyrOK, byUser: true}}
	key := songKey{gen: 1, songID: 100, name: "百"}
	t.Run("找不到", func(t *testing.T) {
		reports := stubAssocReports(t)
		e := recordEnv(items...)
		e.mem.setPlaying(fakeAlbum, fakeSinger, "百")
		e.mem.putRecord(fakeAlbum, fakeSinger, "另一首", idURL(200), "003Other")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, 7900*time.Millisecond)
			if got := reports(); len(got) != 0 {
				t.Fatalf("日程用完之前上报了 %+v", got)
			}
			sleepUntil(e, time.Minute)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
			noAssocResult(t, r)
			wantTimes(t, "读关联歌词", e.fs.readsOf(lyrOK), nil)
		})
		got := reports()
		if len(got) != 1 || got[0].key != "qqmusic.song_record_unresolved" || got[0].tags["qqmusic.version"] != testQQVersion ||
			got[0].extra["verdict"] != assoc.RecordNotFound.String() {
			t.Fatalf("上报 %+v，want 一次 qqmusic.song_record_unresolved，带 QQ 版本与结论 not found", got)
		}
		if s := fmt.Sprint(got[0].tags, got[0].extra); strings.Contains(s, "百") {
			t.Fatalf("上报带了歌名：%s", s)
		}
	})
	t.Run("第 2 秒认出", func(t *testing.T) {
		reports := stubAssocReports(t)
		e := recordEnv(items...)
		e.mem.setPlaying(fakeAlbum, fakeSinger, "百")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, 1500*time.Millisecond)
			noAssocResult(t, r)
			e.mem.putRecord(fakeAlbum, fakeSinger, "百", idURL(100), "003Song")
			sleepUntil(e, 2*time.Second)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, time.Minute)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
			noAssocResult(t, r)
		})
		if got := reports(); len(got) != 0 {
			t.Fatalf("认出了仍上报 %+v", got)
		}
	})
	// 两个编号都已知，mid 键 → A、id 键 → B。第 8 秒另放同一 mid 的 stream12 写法的记录，两种 mid 键网址并存即不唯一（第 4 版
	// 放的是 id 键记录，第 5 版 id 键与 mid 键并存即取 id 键，故改夹具，期望不变）。
	for _, c := range []struct {
		name   string
		change func(e *readerEnv)
		src    lyricSource // 第 8 秒之后的结果；在线即发了作废的结果，关联即没有新结果
		report bool
	}{
		{"认出 A 而第 8 秒不唯一", func(e *readerEnv) { e.mem.putRecord(fakeAlbum, fakeSinger, "百", mid12A, "003A") }, srcOnline, true},
		{"认出 A 而第 8 秒找不到", func(e *readerEnv) { e.mem.clearRecords() }, srcAssoc, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			reports := stubAssocReports(t)
			e := recordEnv(assocItem{song: midURL("003A"), lyric: lyrOK, byUser: true}, assocItem{song: idURL(100), lyric: lyrB, byUser: true})
			e.mem.play("百", midURL("003A"), "003A")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, songID: 100, songMid: "003A", name: "百"})
				wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
				sleepUntil(e, 7500*time.Millisecond)
				noAssocResult(t, r)
				c.change(e)
				sleepUntil(e, time.Minute)
				if c.src == srcOnline {
					wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				}
				noAssocResult(t, r)
				wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
			})
			got := reports()
			if !c.report {
				if len(got) != 0 {
					t.Fatalf("本代次仍为认出却上报了 %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].key != "qqmusic.song_record_unresolved" || got[0].extra["verdict"] != assoc.RecordAmbiguous.String() ||
				got[0].extra["qualified"] != 2 {
				t.Fatalf("上报 %+v，want 一次 qqmusic.song_record_unresolved，结论 ambiguous、合格 2 处", got)
			}
		})
	}
}

// 认出之后照日程复扫（设计 §6.4 第 4 步「何时读」）：第 0 秒认出 A，第 1、2、4、8 秒照扫；各次结论相同，第 0 秒之后不再发结果、
// 不再读关联歌词。反例（第 3 版的做法）：认出即停，只扫第 0 秒。
// 变异自证（实测）：recordDue 恢复认出即停，扫描时刻只有第 0 秒而红；结论不变也重新求结果，第 1 秒起多发结果而红。
func TestAssocRecordRescanAfterFound(t *testing.T) {
	e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, byUser: true})
	e.mem.play("百", idURL(100), "003Song")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
		for _, at := range secs(1, 2, 4, 8, 60) {
			sleepUntil(e, at)
			noAssocResult(t, r)
		}
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
		wantTimes(t, "读关联歌词", e.fs.readsOf(lyrOK), secs(0))
	})
}

// 复扫改换（设计 §6.4 第 4 步「结论」、§6.8 转移，06:42 那一例）：两个编号都已知，表里 mid 键 → A、id 键 → B。第 0 秒记录为
// mid 键，认出 A；第 0.5 秒 QQ 把记录改写为 id 键，第 1 秒复扫认出 B，同一代次的结果换成 B，Detail「复扫改换」一条，带原网址与
// 新网址；第 2、4、8 秒仍为 B，不再发结果。
// 变异自证（实测）：认出即停，第 1 秒没有结果而红；复扫改换不打 Detail 而红。
func TestAssocRecordRescanSwitchesKey(t *testing.T) {
	logs := captureAssocLog(t)
	e := recordEnv(assocItem{song: midURL("003A"), lyric: lyrOK, byUser: true}, assocItem{song: idURL(100), lyric: lyrB})
	e.mem.play("百", midURL("003A"), "003A")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, songMid: "003A", name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
		sleepUntil(e, 500*time.Millisecond)
		e.mem.clearRecords()
		e.mem.putRecord(fakeAlbum, fakeSinger, "百", idURL(100), "003A")
		sleepUntil(e, time.Second)
		if got := latestResult(t, r); got.reopen != 0 {
			t.Fatalf("复扫改换的结果 reopen %d，want 0", got.reopen)
		} else {
			wantDesired(t, got, 1, srcAssoc, 0, false, "", assocLines2...)
		}
		sleepUntil(e, time.Minute)
		noAssocResult(t, r)
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
	})
	want := fmt.Sprintf(i18n.T("A rescan changed the current QQ Music song record from %s to %s"), midURL("003A"), idURL(100))
	if n := countLog(logs(), "+", want); n != 1 {
		t.Fatalf("复扫改换的 Detail %d 条，want 1\n%s", n, logs())
	}
}

// 复扫判不唯一（设计 §6.4 第 4 步「结论」、§6.8 转移）：两个编号都已知，表里 mid 键 → A、id 键 → B。第 0 秒认出 A；第 1.5 秒内存里
// 又出现同一 mid 的 stream12 写法的记录，第 2 秒两种 mid 键网址都合格，本代次作废，结果为在线、偏移 0，Detail「复扫作废」一条
// （原网址与结论名）；第 3 秒 stream12 那条被移走，第 4 秒只剩 A，换回 A。两次各重新求结果一次，第 8 秒仍为 A 不再发；日程结束
// 时为认出，不上报。第 4 版以 id 键与 mid 键两条并存造不唯一，第 5 版并存即取 id 键（见 TestAssocRecordIDPriorityAlbumPage），
// 故改用两种 mid 键网址，期望不变。
// 变异自证（实测）：不唯一不推翻已认出的，第 2 秒没有结果而红；作废不打 Detail 而红。
func TestAssocRecordRescanAmbiguousVoids(t *testing.T) {
	logs := captureAssocLog(t)
	reports := stubAssocReports(t)
	e := recordEnv(assocItem{song: midURL("003A"), lyric: lyrOK, byUser: true}, assocItem{song: idURL(100), lyric: lyrB})
	e.mem.play("百", midURL("003A"), "003A")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, songMid: "003A", name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
		sleepUntil(e, 1500*time.Millisecond)
		noAssocResult(t, r)
		e.mem.putRecord(fakeAlbum, fakeSinger, "百", mid12A, "003A")
		sleepUntil(e, 2*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 3*time.Second)
		noAssocResult(t, r)
		e.mem.clearRecords()
		e.mem.putRecord(fakeAlbum, fakeSinger, "百", midURL("003A"), "003A")
		sleepUntil(e, 4*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
		sleepUntil(e, time.Minute)
		noAssocResult(t, r)
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
		wantTimes(t, "读 A", e.fs.readsOf(lyrOK), secs(0, 4))
		wantTimes(t, "读 B", e.fs.readsOf(lyrB), nil)
	})
	want := fmt.Sprintf(i18n.T("A rescan changed the current QQ Music song record from %s to %s"), midURL("003A"), i18n.T(assoc.RecordAmbiguous.String()))
	if n := countLog(logs(), "+", want); n != 1 {
		t.Fatalf("复扫作废的 Detail %d 条，want 1\n%s", n, logs())
	}
	if got := reports(); len(got) != 0 {
		t.Fatalf("日程结束时为认出，却上报了 %+v", got)
	}
}

// 11-24 专辑页那一例（设计 §6.4 第 4 步「结论」第 2 条，§7「复扫改换」第 5 版）：同一前缀两条记录，低地址的为 mid 键 003A、
// 高地址的为 id 键 100，参数 0 都是 003A；表里 mid 键 → A、id 键 → B。写入只有 songid 100、详情结果尚无：第 0 秒即认出 id 键、
// 结果为关联 B，不等会话交来 songmid，读取者不兜底请求；这一次 mid 键那条待换算、不计入放下的种数，认出的 Detail 是不带种数的
// 那一条。第 0.5 秒会话交来已得 003A（封面请求返回）：重新求结果仍为 B，本代次已认出、不补读；第 1 秒起复扫两条都合格，仍为 B、
// 不发结果，认出的 Detail 带「放下 mid 键网址 1 种」，第 1、2、4、8 秒各一条。全程没有「不唯一」的 Detail，不读 A。
// 反例：两条都是 mid 键 003A：第 0 秒待换算、在线；会话交来 003A 即补读，认出 mid 键、关联 A。
// 变异自证（实测）：去掉 id 键优先（并存仍判不唯一）、待换算挪到 id 键优先之前，第 0 秒为在线而红；SetAside 计入待换算的
// 命中，第 0 秒的认出 Detail 带了种数而红。
func TestAssocRecordIDPriorityAlbumPage(t *testing.T) {
	fmtFound := i18n.T("Identified the current QQ Music song record: %s (%d qualified, %d voided, %d malformed hits); read %.1f MiB in %v")
	fmtSetAside := i18n.T("Identified the current QQ Music song record: %s (%d qualified, %d voided, %d malformed hits; %d mid-key URLs set aside for the id key); read %.1f MiB in %v")
	found, _, _ := strings.Cut(fmtFound, "%.1f")
	setAside, _, _ := strings.Cut(fmtSetAside, "%.1f")
	ambiguous := i18n.T(assoc.RecordAmbiguous.String())
	for _, c := range []struct {
		name     string
		high     string // 高地址那条记录的网址
		src0     lyricSource
		lines0   []string
		srcGot   lyricSource // 会话交来 003A 之后
		linesGot []string
		scans    []time.Duration
	}{
		{"mid 键与 id 键并存", idURL(100), srcAssoc, assocLines2, srcAssoc, assocLines2, secs(0, 1, 2, 4, 8)},
		{"反例：两条都是 mid 键", midURL("003A"), srcOnline, nil, srcAssoc, assocLines, secs(0, 0.5, 1, 2, 4, 8)},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureAssocLog(t)
			stubAssocReports(t)
			e := recordEnv(assocItem{song: midURL("003A"), lyric: lyrOK, byUser: true}, assocItem{song: idURL(100), lyric: lyrB, byUser: true})
			e.mem.play("百", midURL("003A"), "003A")
			e.mem.putRecord(fakeAlbum, fakeSinger, "百", c.high, "003A")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				k := songKey{gen: 1, songID: 100, name: "百"}
				r.Submit(k)
				wantDesired(t, latestResult(t, r), 1, c.src0, 0, false, "", c.lines0...)
				if c.high == idURL(100) {
					if n := countLog(logs(), "+", fmt.Sprintf(found, idURL(100), 1, 0, 0)); n != 1 {
						t.Fatalf("第 0 秒不带种数的认出 Detail %d 条，want 1\n%s", n, logs())
					}
					if n := countLog(logs(), "+", fmt.Sprintf(setAside, idURL(100), 2, 0, 0, 1)); n != 0 {
						t.Fatalf("第 0 秒 mid 键那条待换算，不该有带种数的认出 Detail，实得 %d 条\n%s", n, logs())
					}
				}
				sleepUntil(e, 500*time.Millisecond)
				noAssocResult(t, r)
				k.detail = songDetail{state: detailGot, songMid: "003A"}
				r.Submit(k)
				wantDesired(t, latestResult(t, r), 1, c.srcGot, 0, false, "", c.linesGot...)
				sleepUntil(e, time.Minute)
				noAssocResult(t, r)
				wantTimes(t, "扫描", e.mem.scanTimes(), c.scans)
				wantTimes(t, "兜底请求", e.byID.callTimes(), nil)
				if c.high == idURL(100) {
					wantTimes(t, "读 A", e.fs.readsOf(lyrOK), nil)
				} else {
					wantTimes(t, "读 B", e.fs.readsOf(lyrB), nil)
				}
			})
			if n := countLog(logs(), "+", ambiguous); n != 0 {
				t.Fatalf("不该有「不唯一」的 Detail，实得 %d 条\n%s", n, logs())
			}
			if c.high == idURL(100) {
				if n := countLog(logs(), "+", fmt.Sprintf(setAside, idURL(100), 2, 0, 0, 1)); n != 4 {
					t.Fatalf("带「放下 mid 键网址 1 种」的认出 Detail %d 条，want 4（第 1、2、4、8 秒）\n%s", n, logs())
				}
				if n := countLog(logs(), "+", fmt.Sprintf(found, idURL(100), 1, 0, 0)); n != 1 {
					t.Fatalf("不带种数的认出 Detail %d 条，want 1（第 0 秒）\n%s", n, logs())
				}
			}
		})
	}
}

// 认出的 id 键网址在表里没有条目（设计 §7「结论（单次，id 键优先）」）：两个编号都已知，表里只有 mid 键 003A → A，内存里 mid 键
// 与 id 键两条记录并存 → 认出 id 键、选不中条目，在线、偏移 0，不读 A（结论只由记录与编号定，不看关联表）。反例：内存里只有
// mid 键那条 → 认出 mid 键、关联 A。
func TestAssocRecordIDPriorityNoIDEntry(t *testing.T) {
	for _, c := range []struct {
		name   string
		withID bool
		src    lyricSource
		lines  []string
	}{
		{"mid 键与 id 键并存", true, srcOnline, nil},
		{"反例：只有 mid 键记录", false, srcAssoc, assocLines},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := recordEnv(assocItem{song: midURL("003A"), lyric: lyrOK, byUser: true})
			e.mem.play("百", midURL("003A"), "003A")
			if c.withID {
				e.mem.putRecord(fakeAlbum, fakeSinger, "百", idURL(100), "003A")
			}
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, songID: 100, songMid: "003A", name: "百"})
				wantDesired(t, latestResult(t, r), 1, c.src, 0, false, "", c.lines...)
				wantTimes(t, "扫描", e.mem.scanTimes(), secs(0))
				if c.withID {
					wantTimes(t, "读 A", e.fs.readsOf(lyrOK), nil)
				}
			})
		})
	}
}

// 复扫不推翻（设计 §6.4 第 4 步「结论」）：只有 songid，表里 id 键 100 → A、另一首的 mid 键 003Other → B。第 0 秒认出 A；第 0.5 秒
// 起记录变为下列之一，第 1、2 秒的复扫各得找不到、不完整、待换算或读不出：本代次保持 A，不发新结果、不读 B。第 3.5 秒复原，第 4、
// 8 秒照日程再扫，仍为 A，日程结束不上报。读不出前缀与歌名不符时不扫描，扫描时刻只有第 0、4、8 秒。
// 变异自证（实测）：找不到、不完整、待换算推翻已认出的，各自那一例红；零值推翻已认出的，读不出前缀与歌名不符两例红。
func TestAssocRecordRescanKeepsOnTransient(t *testing.T) {
	recordA := func(e *readerEnv) {
		e.mem.clearRecords()
		e.mem.putRecord(fakeAlbum, fakeSinger, "百", idURL(100), "003Song")
	}
	playing := func(singer, name string) func(e *readerEnv) {
		return func(e *readerEnv) { e.mem.setPlaying(fakeAlbum, singer, name) }
	}
	for _, c := range []struct {
		name           string
		change, revert func(e *readerEnv)
		scans          []time.Duration
	}{
		{"找不到（记录被移走）", func(e *readerEnv) { e.mem.clearRecords() }, recordA, secs(0, 1, 2, 4, 8)},
		{"不完整（记录所在的块读失败）", func(e *readerEnv) {
			e.mem.setHooks(nil, func(addr uintptr, _ int) int {
				if addr == fakeRecordBase {
					return 0
				}
				return -1
			})
		}, func(e *readerEnv) { e.mem.setHooks(nil, nil) }, secs(0, 1, 2, 4, 8)},
		{"待换算（只有 songid、表里有的 mid 键记录）", func(e *readerEnv) {
			e.mem.clearRecords()
			e.mem.putRecord(fakeAlbum, fakeSinger, "百", midURL("003Other"), "003Other")
		}, recordA, secs(0, 1, 2, 4, 8)},
		{"读不出前缀（歌手为空）", playing("", "百"), playing(fakeSinger, "百"), secs(0, 4, 8)},
		{"歌名不符", playing(fakeSinger, "别的歌"), playing(fakeSinger, "百"), secs(0, 4, 8)},
	} {
		t.Run(c.name, func(t *testing.T) {
			reports := stubAssocReports(t)
			e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, byUser: true}, assocItem{song: midURL("003Other"), lyric: lyrB, byUser: true})
			e.mem.play("百", idURL(100), "003Song")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, songID: 100, name: "百"})
				wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
				sleepUntil(e, 500*time.Millisecond)
				c.change(e)
				for _, at := range secs(1, 2) {
					sleepUntil(e, at)
					noAssocResult(t, r)
				}
				sleepUntil(e, 3500*time.Millisecond)
				c.revert(e)
				sleepUntil(e, time.Minute)
				noAssocResult(t, r)
				wantTimes(t, "扫描", e.mem.scanTimes(), c.scans)
				wantTimes(t, "读 A", e.fs.readsOf(lyrOK), secs(0))
				wantTimes(t, "读 B", e.fs.readsOf(lyrB), nil)
			})
			if got := reports(); len(got) != 0 {
				t.Fatalf("日程结束时为认出，却上报了 %+v", got)
			}
		})
	}
}

// 每个代次读当前歌曲记录的次数有上限（设计 §6.4 第 4 步「何时读」）：日程 5 次加补读 1 次。
//   - 结论交替：日程内 5 次（第 0、1、2、4、8 秒），各次的结论在认出 A 与不唯一之间交替、本代次的结论每次都变，也不另加；第 8 秒
//     之后内存每半秒仍在变，到第 60 秒不再扫。不唯一由同一 mid 的两种写法并存造成（第 4 版用 id 键与 mid 键并存，第 5 版并存
//     即取 id 键，故改夹具，期望不变）。
//   - 补读：只有 songid，表里有本首的 id 键条目（因而扫）与 mid 键条目 MidA，记录为 mid 键 MidA。会话报已失败，兜底第 0、2 秒失败、
//     第 6 秒得 MidA：第 0、1、2、4 秒各次待换算，第 6 秒补读一次认出，第 8 秒照日程再扫，共 6 次；第 9 秒会话又带来已得（同一
//     MidA），本代次已认出、不再补读，不出现第 7 次。
//
// 变异自证（实测）：本代次的结论变了即另起一轮日程，「结论交替」第 8 秒之后仍扫而红；补读占日程（顶掉第 8 秒那次），「补读」
// 少了第 8 秒而红。去掉 recordDue 里的次数上限两例都仍绿：补读只在待换算之后编号补上时排，补上之后不再待换算，日程 5 次加
// 补读 1 次已是能达到的最多次数，上限是兜底，由 TestAssocRecordDueCapped 守。
func TestAssocRecordReadsBounded(t *testing.T) {
	t.Run("结论交替", func(t *testing.T) {
		e := recordEnv(assocItem{song: midURL("003A"), lyric: lyrOK, byUser: true}, assocItem{song: idURL(100), lyric: lyrB, byUser: true})
		e.mem.play("百", midURL("003A"), "003A")
		records := func(withB bool) {
			e.mem.clearRecords()
			e.mem.putRecord(fakeAlbum, fakeSinger, "百", midURL("003A"), "003A")
			if withB {
				e.mem.putRecord(fakeAlbum, fakeSinger, "百", mid12A, "003A")
			}
		}
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(songKey{gen: 1, songID: 100, songMid: "003A", name: "百"})
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			for _, s := range []struct {
				at    time.Duration
				withB bool
			}{{time.Second, true}, {2 * time.Second, false}, {4 * time.Second, true}, {8 * time.Second, false}} {
				sleepUntil(e, s.at-500*time.Millisecond)
				records(s.withB)
				sleepUntil(e, s.at)
				if s.withB {
					wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				} else {
					wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
				}
			}
			for at := 8500 * time.Millisecond; at < time.Minute; at += 500 * time.Millisecond {
				sleepUntil(e, at)
				records(at%time.Second == 0)
			}
			sleepUntil(e, time.Minute)
			noAssocResult(t, r)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
		})
	})
	t.Run("补读", func(t *testing.T) {
		stubAssocReports(t)
		e := recordEnv(assocItem{song: idURL(100), lyric: lyrB, offset: 300}, assocItem{song: midURL("MidA"), lyric: lyrOK})
		e.mem.play("百", midURL("MidA"), "MidA")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			k := songKey{gen: 1, songID: 100, name: "百", detail: songDetail{state: detailFailed}}
			r.Submit(k)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, 5*time.Second)
			noAssocResult(t, r)
			e.byID.set(100, "MidA")
			sleepUntil(e, 6*time.Second)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, 9*time.Second)
			k.detail = songDetail{state: detailGot, songMid: "MidA"}
			r.Submit(k)
			sleepUntil(e, time.Minute)
			wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0, 2, 6))
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 6, 8))
		})
	})
}

// 次数上限本身（白盒，设计 §6.4 第 4 步「何时读」）：本代次已读满 recordReadsMax 次时，即使排着补读、日程未完，recordDue 也为
// 零值；反例：差一次读满时为补读的时刻。上面「补读」一例走不到上限（见其注释），上限由这一例守。
// 变异自证（实测）：去掉 recordDue 里的次数上限而红。
func TestAssocRecordDueCapped(t *testing.T) {
	reread := time.Unix(1_700_000_005, 0)
	w := &assocWorker{
		d:   assocDeps{record: (&fakeMem{}).source()},
		cur: songKey{gen: 1, songID: 100}, hasSong: true, tableOK: true,
		entries: []assoc.Entry{{SongURL: idURL(100), LyricURL: lyrOK, Kind: assoc.KindID, ID: 100}},
		rec:     recordState{base: time.Unix(1_700_000_000, 0), step: 3, reads: recordReadsMax, rereadAt: reread},
	}
	if at := w.recordDue(); !at.IsZero() {
		t.Fatalf("已读满 %d 次，recordDue = %v，want 零值", recordReadsMax, at)
	}
	w.rec.reads = recordReadsMax - 1
	if at := w.recordDue(); !at.Equal(reread) {
		t.Fatalf("差一次读满，recordDue = %v，want 补读的时刻 %v", at, reread)
	}
}

// 待换算之后补上编号即补读一次，不占日程（设计 §6.4 第 4 步「何时读」「归属核对」）：只有 songid 100，表里有本首的 id 键条目
// （因而有候选、照日程扫）与 mid 键条目 MidA（指向 lyrOK），记录为 mid 键 MidA，第 0、1、2 秒各次都待换算、为在线、偏移 0。
// 第 2.5 秒同代次写入已得：补上的是 MidA → 当即补读一次并认出，换成 MidA 条目所指的歌词；补上的是另一首的 MidX → 补读后参数 0
// 不符、作废，仍为在线、偏移 0，不读 lyrOK。补读不占日程：其后照旧在第 4、8 秒扫，共 6 次。id 键条目所指的 lyrB 一次都不读。
// 变异自证（实测）：编号补上不排补读，「补上 MidA」一例第 2.5 秒仍为在线、「补上另一首的 MidX」一例少了第 2.5 秒而红；补读占日程
// （顶掉第 4 秒那次），两例都少了第 4 秒而红。
func TestAssocRecordPendingReread(t *testing.T) {
	for _, c := range []struct {
		name  string
		mid   string
		src   lyricSource
		lines []string
	}{
		{"补上 MidA", "MidA", srcAssoc, assocLines},
		{"补上另一首的 MidX", "MidX", srcOnline, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubAssocReports(t)
			e := recordEnv(assocItem{song: idURL(100), lyric: lyrB, offset: 300}, assocItem{song: midURL("MidA"), lyric: lyrOK})
			e.mem.play("百", midURL("MidA"), "MidA")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				k := songKey{gen: 1, songID: 100, name: "百"}
				r.Submit(k)
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				sleepUntil(e, 2500*time.Millisecond)
				noAssocResult(t, r)
				k.detail = songDetail{state: detailGot, songMid: c.mid}
				r.Submit(k)
				wantDesired(t, latestResult(t, r), 1, c.src, 0, false, "", c.lines...)
				sleepUntil(e, time.Minute)
				wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 2.5, 4, 8))
				if c.src == srcOnline {
					wantTimes(t, "读 lyrOK", e.fs.readsOf(lyrOK), nil)
				}
				wantTimes(t, "读 lyrB", e.fs.readsOf(lyrB), nil)
			})
		})
	}
}

// 读一次慢于日程间隔（设计 §7「日程」）：第 0 秒那次扫描耗时 3 秒，其间到期的第 1、2 秒不各补一次，由紧接着的下一次（第 3 秒）
// 一并抵过；此后照日程在第 4、8 秒扫，本代次共 4 次，不超过上限。
// 变异自证（实测）：readRecord 每次只推进一格日程（到期的各次逐次补读），第 3 秒扫了两次而红。
func TestAssocRecordSlowScan(t *testing.T) {
	stubAssocReports(t)
	e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, byUser: true})
	e.mem.setPlaying(fakeAlbum, fakeSinger, "百")
	var slow sync.Once
	e.mem.onRead = func(uintptr) { slow.Do(func() { time.Sleep(3 * time.Second) }) }
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		sleepUntil(e, 3500*time.Millisecond)
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, time.Minute)
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 3, 4, 8))
	})
}

// 本版本未核实记录格式（record 为 nil）：从不扫，有候选也是在线、偏移 0。
// 变异自证（实测）：不按记录、取第一条候选，结果成了在线关联而红。
func TestAssocRecordUnverifiedVersion(t *testing.T) {
	e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, byUser: true}, assocItem{song: midURL("003Song"), lyric: lyrB, byUser: true})
	e.noRecord = true
	e.mem.play("百", idURL(100), "003Song")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, songMid: "003Song", name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 30*time.Second)
		wantTimes(t, "扫描", e.mem.scanTimes(), nil)
		wantTimes(t, "读关联歌词", e.fs.readsOf(lyrOK), nil)
	})
}

// 窄字符版本（设计 §6.4 第 4 步「前缀」「版本」）：真读取者，假内存按 22.16 的偏移放三个窄字符串（专辑、歌名在堆上，歌手
// 内联）与一条 UTF-8 记录。两个编号都已知，表里 mid 键 → A、id 键 → B，记录为 id 键。
//   - 认出：第 0 秒认出 id 键网址（Detail「认出」一条，带该网址），结果为 B。
//   - 首次读不出：专辑一开始就是 GBK 字节，读不出前缀，整轮扫描 0 次，结果为在线、偏移 0，Detail「读不出专辑、歌手与歌名」一条。
//   - 认出之后读不出：第 0 秒认出 B；第 0.5 秒专辑换成 GBK 字节，第 1 秒起读不出前缀、不扫描，本代次的结论按 MergeConclusion
//     保持 B，不发新结果、不读 A，Detail「读不出」一条，日程用完不上报。
//
// 变异自证（实测）：窄字符版本也走宽字符一支，「认出」与「认出之后读不出」两例红；MergeConclusion 让零值（读不出前缀）推翻
// 已认出的，「认出之后读不出」一例红。readNarrowField 去掉 UTF-8 校验时三例仍绿：GBK 字节另由 assoc.RecordNeedle 拒绝
// （含非法 UTF-8 即为假），这一层由 TestReadSongPrefixNarrowUnreadable 的 GBK 一例守。
func TestAssocRecordNarrowVersion(t *testing.T) {
	const album, singer, name = kimi, "supercell", inst
	key := songKey{gen: 1, songID: 100, songMid: "003A", name: name}
	unreadable := i18n.T("Cannot read the album, singer and title of the current song from QQ Music memory")
	identified, _, _ := strings.Cut(i18n.T("Identified the current QQ Music song record: %s (%d qualified, %d voided, %d malformed hits); read %.1f MiB in %v"), "%s")
	setup := func(playingAlbum string) *readerEnv {
		e := recordEnv(assocItem{song: midURL("003A"), lyric: lyrOK, byUser: true}, assocItem{song: idURL(100), lyric: lyrB, byUser: true})
		e.mem.useNarrow()
		e.mem.setPlaying(playingAlbum, singer, name)
		e.mem.putRecord(album, singer, name, idURL(100), "003A")
		return e
	}
	t.Run("认出", func(t *testing.T) {
		logs := captureAssocLog(t)
		e := setup(album)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines2...)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0))
			if n := countLog(logs(), "+", identified+idURL(100)); n != 1 {
				t.Fatalf("认出 %s 的 Detail %d 条，want 1\n%s", idURL(100), n, logs())
			}
		})
	})
	t.Run("首次读不出", func(t *testing.T) {
		logs := captureAssocLog(t)
		stubAssocReports(t)
		e := setup(gbkWudi)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, time.Minute)
			noAssocResult(t, r)
			wantTimes(t, "扫描", e.mem.scanTimes(), nil)
			wantTimes(t, "读 A", e.fs.readsOf(lyrOK), nil)
			wantTimes(t, "读 B", e.fs.readsOf(lyrB), nil)
		})
		if n := countLog(logs(), "+", unreadable); n != 1 {
			t.Fatalf("读不出前缀的 Detail %d 条，want 1\n%s", n, logs())
		}
	})
	t.Run("认出之后读不出", func(t *testing.T) {
		logs := captureAssocLog(t)
		reports := stubAssocReports(t)
		e := setup(album)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines2...)
			sleepUntil(e, 500*time.Millisecond)
			e.mem.setPlaying(gbkWudi, singer, name)
			sleepUntil(e, time.Second)
			noAssocResult(t, r)
			if n := countLog(logs(), "+", unreadable); n != 1 {
				t.Fatalf("第 1 秒读不出前缀的 Detail %d 条，want 1\n%s", n, logs())
			}
			sleepUntil(e, time.Minute)
			noAssocResult(t, r)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0))
			wantTimes(t, "读 A", e.fs.readsOf(lyrOK), nil)
			wantTimes(t, "读 B", e.fs.readsOf(lyrB), secs(0))
		})
		if n := countLog(logs(), "+", unreadable); n != 1 {
			t.Fatalf("读不出前缀的 Detail %d 条，want 1\n%s", n, logs())
		}
		if got := reports(); len(got) != 0 {
			t.Fatalf("本代次仍为认出却上报了 %+v", got)
		}
	})
}

// 结论按代次缓存（设计 §6.4 第 4 步「何时读」）：认出之后关联表两次变化（第 2 秒重读改偏移、第 4 秒重读添一条本首不作候选的
// mid 键条目，songmid 缺位）都只按缓存的网址重选，不另读：扫描只按日程（第 0、1、2、4 秒，同一时刻不多扫）。reopen 加一
// （第 5.5 秒）或 gen 加一（第 6.25 秒）即新一轮，上一轮未读的各次作废。换到一首没有候选的歌（第 7 秒）不扫，关联表变化后才有
// 的（第 9 秒重读）当即开始，照日程扫到第 17 秒。
// 变异自证（实测）：关联表变化即重开一轮、reopen 不算新代次、只在写入歌曲时开始一轮（关联表变化后才有候选的不开始），三种各自红。
func TestAssocRecordCachedPerGeneration(t *testing.T) {
	e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, offset: 100})
	e.mem.play("百", idURL(100), "003Song")
	e.mem.putRecord(fakeAlbum, fakeSinger, "千", idURL(1000), "003Qian")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 100, false, "", assocLines...)

		sleepUntil(e, 500*time.Millisecond)
		e.fs.put(testTablePath, tableDat(assocItem{song: idURL(100), lyric: lyrOK, offset: 200}))
		sleepUntil(e, 2*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 200, false, "", assocLines...)

		sleepUntil(e, 2500*time.Millisecond)
		e.fs.put(testTablePath, tableDat(assocItem{song: idURL(100), lyric: lyrOK, offset: 200}, assocItem{song: midURL("003M"), lyric: lyrB, byUser: true}))
		sleepUntil(e, 4*time.Second)
		wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 2, 4))
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 200, false, "", assocLines...)
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4))

		sleepUntil(e, 5500*time.Millisecond)
		r.Submit(songKey{gen: 1, reopen: 1, songID: 100, name: "百"})
		if got := latestResult(t, r); got.reopen != 1 {
			t.Fatalf("重开之后的结果 reopen %d，want 1", got.reopen)
		} else {
			wantDesired(t, got, 1, srcAssoc, 200, false, "", assocLines...)
		}
		sleepUntil(e, 6250*time.Millisecond)
		r.Submit(songKey{gen: 2, songID: 100, name: "百"})
		wantDesired(t, latestResult(t, r), 2, srcAssoc, 200, false, "", assocLines...)

		sleepUntil(e, 7*time.Second)
		e.mem.setPlaying(fakeAlbum, fakeSinger, "千")
		r.Submit(songKey{gen: 3, songID: 1000, name: "千"})
		wantDesired(t, latestResult(t, r), 3, srcOnline, 0, false, "")
		sleepUntil(e, 7500*time.Millisecond)
		e.fs.put(testTablePath, tableDat(assocItem{song: idURL(100), lyric: lyrOK, offset: 200}, assocItem{song: midURL("003M"), lyric: lyrB, byUser: true},
			assocItem{song: idURL(1000), lyric: lyrB, offset: 300}))
		sleepUntil(e, 9*time.Second)
		wantDesired(t, latestResult(t, r), 3, srcAssoc, 300, false, "", assocLines2...)
		sleepUntil(e, 40*time.Second)
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 5.5, 6.25, 9, 10, 11, 13, 17))
	})
}

// 一轮日程中途关联表变为没有候选（第 0.5 秒改写为只有别的歌的条目、第 2 秒的节拍重读，先于第 2 秒的那次读）：第 2、4、8 秒
// 不扫，也不上报。反例：改写为本首 byUser=0、偏移 0 的条目，仍有候选，日程照常。
// 变异自证（实测）：日程不看有无候选，第一例红；Candidates 恢复相关过滤，反例红。
func TestAssocRecordScheduleStopsWithoutCandidates(t *testing.T) {
	for _, c := range []struct {
		name    string
		after   assocItem
		scans   []time.Duration
		reports int // 日程用完才上报
	}{
		{"改为只有别的歌的条目", assocItem{song: idURL(200), lyric: lyrOK, byUser: true}, secs(0, 1), 0},
		{"反例：改为本首 byUser=0、偏移 0 的条目", assocItem{song: idURL(100), lyric: lyrOK}, secs(0, 1, 2, 4, 8), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			reports := stubAssocReports(t)
			e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, byUser: true})
			e.mem.setPlaying(fakeAlbum, fakeSinger, "百")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, songID: 100, name: "百"})
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				sleepUntil(e, 500*time.Millisecond)
				e.fs.put(testTablePath, tableDat(c.after))
				sleepUntil(e, time.Minute)
				wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 2))
				wantTimes(t, "扫描", e.mem.scanTimes(), c.scans)
			})
			if got := len(reports()); got != c.reports {
				t.Fatalf("上报 %d 次，want %d 次", got, c.reports)
			}
		})
	}
}

// 完整性（设计 §7「完整性」）：同一首的旧 id 键记录读得到，当前 mid 键记录在读取上限之外、所在的块读失败、只读到一部分，
// 或扫到它之前区域查询报错（不是到头的 ERROR_INVALID_PARAMETER），两键关联不同的歌词。都标不完整、不认出旧键（在线、
// 偏移 0），Detail 记下原因与失败块数；第 1 秒读完整时（旧记录已被原处改写）认出当前键。只有不完整时的「没有命中」不判
// 找不到：第 1 秒读完整才打找不到。QQ 中途退出时查询与读取都失败，同样按不完整、按日程再读，会话随后由 CheckValid 结束。
// 变异自证（实测）：不完整也当作完整下结论，前四例与「只有不完整时的没有命中」都红；不完整时不打 Detail，同五例红；
// 查询报错都当作到头，「区域查询失败」一例认出了旧键而红。
func TestAssocRecordIncomplete(t *testing.T) {
	const curBase = 0x400000
	cur := func() fakeRegion {
		return privRegion(curBase, append(bytes200(), recordBytes(fakeAlbum, fakeSinger, "三百", midURL("003Cur"), "003Cur")...))
	}
	key := songKey{gen: 1, songID: 300, songMid: "003Cur", name: "三百"}
	for _, c := range []struct {
		name   string
		limit  bool                          // 读取总量上限 64 KiB，当前记录之前有 128 KiB 的私有可写区
		fail   func(addr uintptr, n int) int // 第 0 秒那次的读失败
		qfail  func(addr uintptr) error      // 第 0 秒那次的查询失败
		reason string
	}{
		{"读取上限", true, nil, nil, "read limit reached"},
		{"块读失败", false, func(addr uintptr, _ int) int {
			if addr == curBase {
				return 0
			}
			return -1
		}, nil, "chunk reads failed"},
		{"只读到一部分", false, func(addr uintptr, _ int) int {
			if addr == curBase {
				return 100
			}
			return -1
		}, nil, "chunk reads failed"},
		{"区域查询失败", false, nil, func(addr uintptr) error {
			if addr == curBase {
				return windows.ERROR_ACCESS_DENIED
			}
			return nil
		}, "memory region query failed or did not advance"},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureAssocLog(t)
			if c.limit {
				withScanLimits(t, scanLimits{chunk: 4 << 10, total: 64 << 10})
			}
			e := recordEnv(assocItem{song: idURL(300), lyric: lyrOK, byUser: true}, assocItem{song: midURL("003Cur"), lyric: lyrB, byUser: true})
			e.mem.setPlaying(fakeAlbum, fakeSinger, "三百")
			e.mem.putRecord(fakeAlbum, fakeSinger, "三百", idURL(300), "003Cur")
			if c.limit {
				junk := fakeRegion{memRegion{base: 0x200000, size: 128 << 10, state: windows.MEM_COMMIT, protect: windows.PAGE_READWRITE, typ: memPrivate}, nil}
				e.mem.setExtra(junk, cur())
			} else {
				e.mem.setExtra(cur())
				e.mem.setHooks(nil, c.fail)
				e.mem.setQueryFail(c.qfail)
			}
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(key)
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				sleepUntil(e, 500*time.Millisecond)
				e.mem.clearRecords()
				e.mem.setExtra(cur())
				e.mem.setHooks(nil, nil)
				e.mem.setQueryFail(nil)
				sleepUntil(e, time.Second)
				wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines2...)
				wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1))
				wantTimes(t, "读旧键的歌词", e.fs.readsOf(lyrOK), nil)
			})
			if n := countLog(logs(), "+", i18n.T(c.reason)); n != 1 {
				t.Fatalf("不完整的 Detail（%s）%d 条，want 1\n%s", i18n.T(c.reason), n, logs())
			}
		})
	}
	t.Run("只有不完整时的没有命中", func(t *testing.T) {
		logs := captureAssocLog(t)
		e := recordEnv(assocItem{song: idURL(300), lyric: lyrOK, byUser: true})
		e.mem.setPlaying(fakeAlbum, fakeSinger, "三百")
		e.mem.setHooks(nil, func(addr uintptr, _ int) int {
			if addr == fakeRecordBase {
				return 0
			}
			return -1
		})
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			if n := countLog(logs(), "+", i18n.T(assoc.RecordNotFound.String())); n != 0 {
				t.Fatalf("不完整时打了找不到\n%s", logs())
			}
			sleepUntil(e, 500*time.Millisecond)
			e.mem.setHooks(nil, nil)
			sleepUntil(e, time.Second)
			if n := countLog(logs(), "+", i18n.T(assoc.RecordNotFound.String())); n != 1 {
				t.Fatalf("读完整之后找不到的 Detail %d 条，want 1\n%s", n, logs())
			}
		})
		if n := countLog(logs(), "+", i18n.T("chunk reads failed")); n != 1 {
			t.Fatalf("不完整的 Detail %d 条，want 1\n%s", n, logs())
		}
	})
}

// bytes200 是记录之前垫的 200 字节，使「只读到一部分」时记录落在没读到的部分里。
func bytes200() []byte { return []byte(strings.Repeat("\xEE", 200)) }

// 后缀误命中（设计 §7「后缀误命中」）：另一首的专辑 XAlbum 以本首的专辑 Album 结尾、歌手与歌名相同，当前记录因歌手写法不同
// （S/T）没命中。只有 songid 时另一首的 mid 键网址不论在不在表里（byUser=1 或 0 都一样）都待换算、按认不出；songmid 已知时
// 参数 0 不同、作废。各例都不读、不显示另一首的关联歌词。
// 变异自证（实测）：只有 songid 时把 mid 键网址计为合格，前两例读了另一首的歌词而红；不核对参数 0（songmid 已知时也计合格），
// 第三例读了另一首的歌词而红。
func TestAssocRecordSuffixFalseHit(t *testing.T) {
	for _, c := range []struct {
		name    string
		key     songKey
		inTable bool // 另一首的条目在表里
		byUser  bool
	}{
		{"只有 songid、另一首 byUser=1 的条目在表里（待换算）", songKey{gen: 1, songID: 100, name: "N"}, true, true},
		{"只有 songid、另一首 byUser=0 的条目在表里（待换算）", songKey{gen: 1, songID: 100, name: "N"}, true, false},
		{"songmid 已知、另一首参数 0 不同（作废）", songKey{gen: 1, songID: 100, songMid: "003Cur", name: "N"}, true, true},
		{"只有 songid、另一首不在表里（待换算）", songKey{gen: 1, songID: 100, name: "N"}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubAssocReports(t)
			items := []assocItem{{song: idURL(100), lyric: lyrOK, byUser: true}}
			if c.inTable {
				items = append(items, assocItem{song: midURL("003Other"), lyric: lyrB, byUser: c.byUser})
			}
			e := recordEnv(items...)
			e.mem.setPlaying("Album", "S", "N")
			e.mem.putRecord("XAlbum", "S", "N", midURL("003Other"), "003Other")
			e.mem.putRecord("Album", "S/T", "N", idURL(100), "003Cur")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(c.key)
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				sleepUntil(e, 30*time.Second)
				select {
				case got := <-r.Results():
					wantDesired(t, got, 1, srcOnline, 0, false, "")
				default:
				}
				wantTimes(t, "读另一首的歌词", e.fs.readsOf(lyrB), nil)
				wantTimes(t, "读本首的歌词", e.fs.readsOf(lyrOK), nil)
			})
		})
	}
}

// 后缀误命中之后关联表加进另一首的条目（第 4 版代码审的反例，设计 §7「后缀误命中」）：只有 songid 100，Struct1 为
// Album/S/N，内存里只有另一首 XAlbum/S/N 的记录（mid 键 MidB），关联表起初只有 idURL(100)。第 0 秒为在线；第 0.5 秒同一代次
// 关联表追加 midURL(MidB) → 歌词 B，第 2 秒重读之后仍为在线、偏移 0，B 的文件一次都不读。结论按代次缓存、关联表变化只按它
// 重选，缓存的结论须不依赖当时的关联表。
// 变异自证（实测）：恢复「只有 songid 时 mid 键网址在表里没有条目即合格」，第 0 秒认出 MidB、第 2 秒重选到 B 而红。
func TestAssocRecordSuffixFalseHitTableGrows(t *testing.T) {
	stubAssocReports(t)
	e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, byUser: true})
	e.mem.setPlaying("Album", "S", "N")
	e.mem.putRecord("XAlbum", "S", "N", midURL("MidB"), "MidB")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "N"})
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 500*time.Millisecond)
		e.fs.put(testTablePath, tableDat(assocItem{song: idURL(100), lyric: lyrOK, byUser: true}, assocItem{song: midURL("MidB"), lyric: lyrB, byUser: true}))
		sleepUntil(e, 2*time.Second)
		wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 2))
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 30*time.Second)
		select {
		case got := <-r.Results():
			wantDesired(t, got, 1, srcOnline, 0, false, "")
		default:
		}
		wantTimes(t, "读另一首的歌词", e.fs.readsOf(lyrB), nil)
	})
}

// 前缀用 Struct1 的原样歌名（设计 §6.4 第 4 步「前缀」）：Struct1 歌名带首尾空格或控制字符、会话的歌名是 sanitizeString 过的，
// 两者经 sanitize 相等即照常扫，前缀带着空格与控制字符，认出原样歌名的那条记录、不认去掉之后的那条。歌名不同（歌已换）
// 不扫，按日程再读：第 0.5 秒 Struct1 换成这首，第 1 秒扫描并认出，其后照日程复扫。
// 变异自证（实测）：前缀用会话的歌名，前两例只命中去空白的那条（mid 键网址，只有 songid 即待换算、按认不出）而红；
// 不比歌名，第三例第 0 秒即扫描而红。
func TestAssocRecordNameSanitized(t *testing.T) {
	items := []assocItem{{song: idURL(100), lyric: lyrOK, byUser: true}, {song: midURL("003Trim"), lyric: lyrB, byUser: true}}
	key := songKey{gen: 1, songID: 100, name: "百"}
	for _, raw := range []string{" 百 ", "百\x01"} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			e := recordEnv(items...)
			e.mem.setPlaying(fakeAlbum, fakeSinger, raw)
			e.mem.putRecord(fakeAlbum, fakeSinger, raw, idURL(100), "003Song")
			e.mem.putRecord(fakeAlbum, fakeSinger, "百", midURL("003Trim"), "003Trim")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(key)
				wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
				wantTimes(t, "扫描", e.mem.scanTimes(), secs(0))
			})
		})
	}
	t.Run("歌名不同", func(t *testing.T) {
		e := recordEnv(items...)
		e.mem.setPlaying(fakeAlbum, fakeSinger, "别的歌")
		e.mem.putRecord(fakeAlbum, fakeSinger, "百", idURL(100), "003Song")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			wantTimes(t, "扫描", e.mem.scanTimes(), nil)
			sleepUntil(e, 500*time.Millisecond)
			e.mem.setPlaying(fakeAlbum, fakeSinger, "百")
			sleepUntil(e, time.Second)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, 30*time.Second)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(1, 2, 4, 8))
		})
	})
}

// 扫描阻塞在第二块（记录区）的读取里，期间会话写入下一首：放开后在下一块之前中止，旧代次不下结论、不读关联歌词、不发结果，
// 转去处理新歌（它没有候选，不扫）。会话取消同理：放开后不再读、不发结果，读取者退出。
// 变异自证（实测）：块间不查新写入，新写入一例红；块间不查 ctx，会话取消一例红。
func TestAssocRecordAbortsOnNewSong(t *testing.T) {
	setup := func() (*readerEnv, *gate, func() []uintptr) {
		e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, byUser: true})
		e.mem.play("百", idURL(100), "003Song")
		g := &gate{}
		var mu sync.Mutex
		var addrs []uintptr
		e.mem.onRead = func(addr uintptr) {
			mu.Lock()
			addrs = append(addrs, addr)
			mu.Unlock()
			if addr == fakeRecordBase {
				g.wait()
			}
		}
		return e, g, func() []uintptr {
			synctest.Wait()
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(addrs)
		}
	}
	t.Run("新写入的歌", func(t *testing.T) {
		e, g, reads := setup()
		e.beforeStart = g.arm
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			defer g.release()
			r.Submit(songKey{gen: 1, songID: 100, name: "百"})
			synctest.Wait()
			r.Submit(songKey{gen: 2, songID: 200, name: "二百"})
			g.release()
			wantDesired(t, latestResult(t, r), 2, srcOnline, 0, false, "")
			noAssocResult(t, r)
			if got := reads(); !slices.Equal(got, []uintptr{fakeRecordBase}) {
				t.Fatalf("读了 %#x，want 只读到记录区那一块、不读末块", got)
			}
			wantTimes(t, "读关联歌词", e.fs.readsOf(lyrOK), nil)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0))
		})
	})
	t.Run("会话取消", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e, g, reads := setup()
			g.arm()
			defer g.release()
			e.setT0(time.Now())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, ok := startAssocReader(ctx, e.deps())
			if !ok {
				t.Fatal("startAssocReader 返回假")
			}
			synctest.Wait()
			r.Submit(songKey{gen: 1, songID: 100, name: "百"})
			synctest.Wait()
			cancel()
			g.release()
			synctest.Wait()
			noAssocResult(t, r)
			if got := reads(); !slices.Equal(got, []uintptr{fakeRecordBase}) {
				t.Fatalf("读了 %#x，want 只读到记录区那一块", got)
			}
			if n := assocReaders.Load(); n != 0 {
				t.Fatalf("取消之后存活的读取者 %d 个，want 0", n)
			}
			if n := e.mem.closeCount(); n != 1 {
				t.Fatalf("句柄关闭 %d 次，want 1", n)
			}
		})
	})
}

// 结论名与不完整的原因注入日志，在源头经 i18n.T：中文下各有译文（与英文原文不同），互不相同。
// 变异自证（实测）：译表删去 pending song ID completion 一条而红。
func TestRecordVerdictTranslated(t *testing.T) {
	orig := i18n.Language()
	t.Cleanup(func() { i18n.SetLanguage(orig) })
	i18n.SetLanguage(i18n.Chinese)
	seen := map[string]bool{}
	for v := assoc.RecordNone; v <= assoc.RecordIncomplete; v++ {
		en := v.String()
		zh := i18n.T(en)
		if en == "unknown" || zh == en || seen[zh] {
			t.Errorf("结论 %d 的名字 %q 译作 %q：须有、且互不相同的译文", v, en, zh)
		}
		seen[zh] = true
	}
	for _, st := range []recordScanStats{{capped: true}, {queryFailed: true}, {failedChunks: 2}} {
		i18n.SetLanguage(i18n.Chinese)
		zh := incompleteReason(st)
		i18n.SetLanguage(i18n.English)
		en := incompleteReason(st)
		if zh == en || seen[zh] {
			t.Errorf("不完整的原因（%+v）中文 %q、英文 %q：须有、且互不相同的译文", st, zh, en)
		}
		seen[zh] = true
	}
}
