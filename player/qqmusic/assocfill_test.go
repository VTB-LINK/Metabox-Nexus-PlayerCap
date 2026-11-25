package qqmusic

// 本文件只测读取者怎样补全当前歌曲缺的那个编号（设计 §6.4 第 2 步「交接」「兜底」、§6.8「输入」）：缺的那一个依次取会话交来的
// 已得、读取者兜底所得，会话的优先；同一代次的写入只换详情结果，不清本首的认定、Detail 额度与记录日程；只在会话报已失败、
// 本会话读记录、恰有一个编号时兜底请求，失败按 2、4、8、16、32 秒退避、之后保持 60 秒，直到换歌或会话带来已得；兜底所得跨
// 重开保留、换歌即清；接口给出另一首的编号时不用。请求经 fakeDetails 桩，不联网。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
)

var (
	failedDetail = songDetail{state: detailFailed}
	gotMidA      = songDetail{state: detailGot, songMid: "MidA"}
)

// withDetail 返回带详情结果 d 的 k。
func withDetail(k songKey, d songDetail) songKey {
	k.detail = d
	return k
}

// fillKey 是只有 songid 100 的在线歌曲「百」，尚无详情结果。
var fillKey = songKey{gen: 1, songID: 100, name: "百"}

// fillEnv 的关联表只有本首的 mid 键条目 MidA（指向 lyrOK，即 assocLines），假内存里本首的记录为 mid 键 MidA。songmid 补上之前
// 没有候选、不扫。
func fillEnv() *readerEnv {
	e := recordEnv(assocItem{song: midURL("MidA"), lyric: lyrOK})
	e.mem.play("百", midURL("MidA"), "MidA")
	return e
}

// fillFailedLog 是兜底失败的那条 Detail（当前语言下）。
func fillFailedLog(err error) string {
	return fmt.Sprintf(i18n.T("Completing the other ID of the current QQ Music song failed; will retry: %v"), err)
}

// 同一代次的写入只换详情结果（设计 §6.4 第 2 步「交接」末句）。本地文件认定到一半（第 1.5 秒）写入已失败：本地认定的日程照旧
// （第 0、1、2、4、8 秒，不从写入时重排），同一原因的 Detail 不重打。本地文件已认定时写入已失败：不清、不重查占用，结果照旧。
// 在线歌曲记录日程进行中（第 1.5 秒）写入已得：扫描照旧在第 0、1、2、4、8 秒，不另起一轮。
// 变异自证（实测）：同代次写入照第 3 版清本首状态（localPath、Detail 额度、本地日程），两个本地例红。
func TestAssocDetailSameGenOnlyUpdates(t *testing.T) {
	local := songKey{gen: 1, name: "一"}
	t.Run("本地文件认定到一半", func(t *testing.T) {
		logs := captureAssocLog(t)
		e := localEnv()
		e.rm.fn = func([]string) ([]uint32, error) {
			return nil, i18n.Errorf("RmGetList failed: %w", windows.ERROR_ACCESS_DENIED)
		}
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(local)
			wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
			sleepUntil(e, 1500*time.Millisecond)
			r.Submit(withDetail(local, failedDetail))
			wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
			sleepUntil(e, 30*time.Second)
			wantTimes(t, "本地认定", e.rm.callTimes(), secs(0, 1, 2, 4, 8))
		})
		head := strings.TrimSuffix(i18n.T("Cannot identify the local file QQ Music is playing: %v"), "%v")
		if n := countLog(logs(), "+", head); n != 1 {
			t.Fatalf("认定失败的 Detail %d 条，want 1（同一原因不重打）\n%s", n, logs())
		}
	})
	t.Run("本地文件已认定", func(t *testing.T) {
		e := localEnv()
		e.rm.fn = openBy([]string{localOne}, testQQPID)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(local)
			wantDesired(t, latestResult(t, r), 1, srcLocal, 100, false, localOne, assocLines...)
			calls := e.rm.callTimes()
			sleepUntil(e, 1500*time.Millisecond)
			r.Submit(withDetail(local, failedDetail))
			wantDesired(t, latestResult(t, r), 1, srcLocal, 100, false, localOne, assocLines...)
			sleepUntil(e, 30*time.Second)
			wantTimes(t, "本地认定", e.rm.callTimes(), calls)
		})
	})
	t.Run("在线歌曲记录日程中", func(t *testing.T) {
		e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK, byUser: true})
		e.mem.play("百", idURL(100), "003Song")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(fillKey)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, 1500*time.Millisecond)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1))
			r.Submit(withDetail(fillKey, songDetail{state: detailGot, songMid: "003Song"}))
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, time.Minute)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
		})
	})
}

// 补上之前缺的那种键的条目不作候选，补上即重新求结果、此前没有候选的当即开始读记录（设计 §6.4 第 2 步）：只有 songid、表里只有
// mid 键条目。尚无时不扫、结果为在线、偏移 0，30 秒内不兜底请求；第 30 秒同代次写入已得（MidA）→ 当即开始一轮并认出，结果为
// 该条目所指的歌词，其后照日程复扫。
// 变异自证（实测）：候选与选条目只用 songKey 自带的编号，写入已得之后仍不扫、为在线而红。
func TestAssocDetailGotCompletesCandidates(t *testing.T) {
	e := fillEnv()
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(fillKey)
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 30*time.Second)
		noAssocResult(t, r)
		wantTimes(t, "扫描", e.mem.scanTimes(), nil)
		r.Submit(withDetail(fillKey, gotMidA))
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
		sleepUntil(e, time.Minute)
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(30, 31, 32, 34, 38))
		wantTimes(t, "兜底请求", e.byID.callTimes(), nil)
	})
}

// 只在会话报已失败时兜底请求（设计 §6.4 第 2 步「兜底」）：尚无（会话的封面请求还没回来，抢先发即重复请求）与已得都不请求；
// 已失败即当即请求一次，桩返回 MidA，认出只有 mid 键条目的那首。三例都不走按 songmid 的请求。
// 变异自证（实测）：尚无时也兜底，「尚无」一例请求了一次、认出而红。
func TestAssocFillOnlyWhenFailed(t *testing.T) {
	for _, c := range []struct {
		name  string
		d     songDetail
		calls []time.Duration
		src   lyricSource
		lines []string
	}{
		{"尚无", songDetail{}, nil, srcOnline, nil},
		{"已得", gotMidA, nil, srcAssoc, assocLines},
		{"已失败", failedDetail, secs(0), srcAssoc, assocLines},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := fillEnv()
			e.byID.set(100, "MidA")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(withDetail(fillKey, c.d))
				sleepUntil(e, time.Minute)
				wantDesired(t, latestResult(t, r), 1, c.src, 0, false, "", c.lines...)
				wantTimes(t, "按 songid 的兜底请求", e.byID.callTimes(), c.calls)
				wantTimes(t, "按 songmid 的兜底请求", e.byMid.callTimes(), nil)
			})
		})
	}
}

// 兜底一直失败（设计 §6.4 第 2 步「兜底」）：请求时刻为写入已失败之后第 0、2、6、14、30、62、122 秒（退避 2、4、8、16、32 秒后
// 保持 60 秒），同一原因的 Detail 只打一条，失败不另发结果；第 125 秒换歌（gen 2，尚无）之后不再请求。
// 变异自证（实测）：退避不封顶，第 122 秒那次推到第 126 秒而红；失败不打 Detail 而红。
func TestAssocFillBackoff(t *testing.T) {
	logs := captureAssocLog(t)
	e := fillEnv()
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(withDetail(fillKey, failedDetail))
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 125*time.Second)
		noAssocResult(t, r)
		want := secs(0, 2, 6, 14, 30, 62, 122)
		wantTimes(t, "兜底请求", e.byID.callTimes(), want)
		r.Submit(songKey{gen: 2, songID: 200, name: "二百"})
		wantDesired(t, latestResult(t, r), 2, srcOnline, 0, false, "")
		sleepUntil(e, 400*time.Second)
		wantTimes(t, "换歌之后的兜底请求", e.byID.callTimes(), want)
	})
	if n := countLog(logs(), "+", fillFailedLog(errDetailOffline)); n != 1 {
		t.Fatalf("兜底失败的 Detail %d 条，want 1\n%s", n, logs())
	}
}

// 会话之后带来已得即以会话的为准、停止兜底（设计 §6.4 第 2 步「兜底」）：兜底一直失败（第 0、2、6 秒），第 7 秒同代次写入已得 →
// 此后不再请求，用会话的 MidA 认出。兜底先成功（第 0 秒），会话随后（第 5 秒）又写入已失败 → 不再请求，结果照旧。兜底所得
// （MidX，表里没有它的条目，没有候选、不扫）与会话随后带来的已得（MidA）不同 → 用会话的，当即开始一轮并认出 MidA。
// 变异自证（实测）：不理会会话的已得（只要会话报过已失败就照退避请求），第一例第 7 秒之后仍请求而红；兜底所得优先于会话的
// 已得，第三例仍为在线而红。
func TestAssocFillStopsOnSessionGot(t *testing.T) {
	t.Run("兜底退避中会话带来已得", func(t *testing.T) {
		e := fillEnv()
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(withDetail(fillKey, failedDetail))
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, 7*time.Second)
			r.Submit(withDetail(fillKey, gotMidA))
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, 3*time.Minute)
			wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0, 2, 6))
		})
	})
	t.Run("兜底先成功、会话又报已失败", func(t *testing.T) {
		e := fillEnv()
		e.byID.set(100, "MidA")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(withDetail(fillKey, failedDetail))
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, 5*time.Second)
			r.Submit(withDetail(fillKey, failedDetail))
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, 3*time.Minute)
			wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0))
		})
	})
	t.Run("兜底所得与会话的已得不同，用会话的", func(t *testing.T) {
		e := fillEnv()
		e.byID.set(100, "MidX")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(withDetail(fillKey, failedDetail))
			sleepUntil(e, 5*time.Second)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			wantTimes(t, "扫描", e.mem.scanTimes(), nil)
			r.Submit(withDetail(fillKey, gotMidA))
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, time.Minute)
			wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0))
		})
	})
}

// 同一代次里已在用的编号换了值即作废已认出的结论、补读一次（第 4 版代码审 Task 16 的反例，设计 §6.4 第 4 步「何时读」）。songid
// 100（Title），关联表有 MidA（指向 lyrOK，即 assocLines）与 MidX（指向 lyrB，即 assocLines2）两条 mid 键条目，内存有
// Album/Singer/Title 的 MidA 记录与 DeluxeAlbum/Singer/Title 的 MidX 记录（前缀后缀误命中）。会话报已失败：
//   - 兜底得 MidX：第 0 秒起认出 X（显示 X 的歌词）；第 9 秒同代次会话带来已得 MidA → 作废 X、当即补读，按 MidA 核对认出 A，
//     此后不再给出 X 的歌词。
//   - 次数已满：表里另有本首的 id 键条目（只有 songid 时也扫），兜底第 6 秒才得 MidX，第 0、1、2、4 秒待换算、第 6 秒补读认出 X、
//     第 8 秒照日程，已读满 6 次；第 9 秒会话带来 MidA → 作废 X、不再读，结果为在线、偏移 0。
//   - 反例：兜底得 MidA、认出 A，第 9 秒会话带来同一 MidA → 编号没变，不作废、不补读。补上原本缺的编号也不作废，见
//     TestAssocDetailSameGenOnlyUpdates 的「在线歌曲记录日程中」。
//
// 变异自证（实测）：去掉作废，前两例仍给出 X 的歌词而红；作废而不排补读，第一例停在在线而红；去掉 recordDue 的次数上限，第二例
// 第 9 秒又读了一次、认出 A 而红；编号一变（含补上原本缺的）即作废，TestAssocDetailSameGenOnlyUpdates「在线歌曲记录日程中」
// 第 1.5 秒多扫一次而红。
func TestAssocFillSessionOverridesFound(t *testing.T) {
	setup := func(items ...assocItem) *readerEnv {
		e := recordEnv(append([]assocItem{{song: midURL("MidA"), lyric: lyrOK}, {song: midURL("MidX"), lyric: lyrB}}, items...)...)
		e.mem.setPlaying("Album", "Singer", "Title")
		e.mem.putRecord("Album", "Singer", "Title", midURL("MidA"), "MidA")
		e.mem.putRecord("DeluxeAlbum", "Singer", "Title", midURL("MidX"), "MidX")
		return e
	}
	key := songKey{gen: 1, songID: 100, name: "Title", detail: failedDetail}
	t.Run("兜底得 MidX 已认出，会话带来 MidA", func(t *testing.T) {
		stubAssocReports(t)
		e := setup()
		e.byID.set(100, "MidX")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines2...)
			sleepUntil(e, 9*time.Second)
			noAssocResult(t, r)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
			r.Submit(withDetail(key, gotMidA))
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, time.Minute)
			noAssocResult(t, r)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8, 9))
			wantTimes(t, "读 X 的歌词", e.fs.readsOf(lyrB), secs(0))
			wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0))
		})
	})
	t.Run("次数已满时会话带来 MidA", func(t *testing.T) {
		stubAssocReports(t)
		e := setup(assocItem{song: idURL(100), lyric: lyrOK, offset: 700})
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, 5*time.Second)
			e.byID.set(100, "MidX")
			sleepUntil(e, 6*time.Second)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines2...)
			sleepUntil(e, 9*time.Second)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 6, 8))
			r.Submit(withDetail(key, gotMidA))
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, time.Minute)
			noAssocResult(t, r)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 6, 8))
			wantTimes(t, "读 A 的歌词", e.fs.readsOf(lyrOK), nil)
		})
	})
	t.Run("反例：会话带来同一编号", func(t *testing.T) {
		e := setup()
		e.byID.set(100, "MidA")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, 9*time.Second)
			r.Submit(withDetail(key, gotMidA))
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			sleepUntil(e, time.Minute)
			wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
		})
	})
}

// 错误编号与后缀误命中的读取者一侧（设计 §7「错误编号与后缀误命中」）：songid=100，内存里有 A（Album/Singer/Title，stream4/MidA，
// 参数 0 MidA）与 B（DeluxeAlbum/Singer/Title，stream4/MidB，参数 0 MidB），关联表有本首的 id 键条目（使只有 songid 时也有候选、
// 照日程扫）、A 的 mid 键条目（指向 lyrOK）与 B 的 mid 键条目（指向 lyrB）。会话报已失败；兜底的桩以错误回应（fetchSongMidByID
// 对 id=200 的详情只报错）→ 30 秒内两处命中都待换算，结果一直是在线、偏移 0；第 31 秒桩改为返回 MidA → 第 62 秒那次请求得到
// MidA、补读一次：A 合格、B 作废，认出 A，选中 A 的 mid 键条目（偏移 0，不是 id 键条目的 700）。B 的歌词一次都不读。
// 变异自证（实测）：补上的编号不用于归属核对（ConcludeRecord 只用 songKey 自带的），第 62 秒之后仍为在线而红。
func TestAssocFillWrongDetailUnused(t *testing.T) {
	stubAssocReports(t)
	e := recordEnv(
		assocItem{song: idURL(100), lyric: lyrOK, offset: 700},
		assocItem{song: midURL("MidA"), lyric: lyrOK},
		assocItem{song: midURL("MidB"), lyric: lyrB, byUser: true},
	)
	e.mem.setPlaying("Album", "Singer", "Title")
	e.mem.putRecord("Album", "Singer", "Title", midURL("MidA"), "MidA")
	e.mem.putRecord("DeluxeAlbum", "Singer", "Title", midURL("MidB"), "MidB")
	wrong := i18n.Errorf("song detail for songid %d did not confirm the songid (%s)", 100,
		fmt.Sprintf(i18n.T("track id %s, mid %q, code %d"), "200", "MidB", 0))
	e.byID.setFn(func(uint32) (string, error) { return "", wrong })
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "Title", detail: failedDetail})
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 31*time.Second)
		noAssocResult(t, r)
		wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0, 2, 6, 14, 30))
		e.byID.setFn(nil)
		e.byID.set(100, "MidA")
		sleepUntil(e, 62*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
		sleepUntil(e, 3*time.Minute)
		noAssocResult(t, r)
		wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0, 2, 6, 14, 30, 62))
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8, 62))
		wantTimes(t, "读 B 的歌词", e.fs.readsOf(lyrB), nil)
	})
}

// 只有 songmid（设计 §6.4 第 2 步「兜底」）：兜底走 resolveMid（fetchSongIDByMid）、不调 resolveID，补上 songid 后本首的 id 键条目
// 入候选并认出。反例：会话交来的 songmid 不合形式（含 &、全是数字）→ 不请求，按补全失败处理，同一原因的 Detail 只一条。
// 变异自证（实测）：兜底一律走 resolveID，第一例 resolveMid 没被调用而红；不核对 songmid 的形式，两个反例请求了而红。
func TestAssocFillSongMidOnly(t *testing.T) {
	setup := func(mid string) *readerEnv {
		e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK})
		e.mem.play("百", idURL(100), mid)
		e.byMid.set(mid, 100)
		e.byID.set(100, mid)
		return e
	}
	t.Run("songmid 合形式", func(t *testing.T) {
		e := setup("MidA")
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(songKey{gen: 1, songMid: "MidA", name: "百", detail: failedDetail})
			sleepUntil(e, time.Minute)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
			wantTimes(t, "按 songmid 的兜底请求", e.byMid.callTimes(), secs(0))
			wantTimes(t, "按 songid 的兜底请求", e.byID.callTimes(), nil)
		})
	})
	for _, mid := range []string{"MidA&x=1", "123456"} {
		t.Run("songmid 不合形式 "+mid, func(t *testing.T) {
			logs := captureAssocLog(t)
			e := setup(mid)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, songMid: mid, name: "百", detail: failedDetail})
				sleepUntil(e, 3*time.Minute)
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				wantTimes(t, "按 songmid 的兜底请求", e.byMid.callTimes(), nil)
				wantTimes(t, "按 songid 的兜底请求", e.byID.callTimes(), nil)
			})
			if n := countLog(logs(), "+", fillFailedLog(i18n.Errorf("malformed songmid %q", mid))); n != 1 {
				t.Fatalf("songmid 不合形式的 Detail %d 条，want 1\n%s", n, logs())
			}
		})
	}
}

// 本会话不读记录（record 为 nil：22.82 以外、复制句柄失败）时不兜底：已失败也不请求，只有 songid、只有 songmid 各一例。
// 变异自证（实测）：fillDue 不看 record，两例都请求了而红。
func TestAssocFillNotWithoutRecord(t *testing.T) {
	for _, k := range []songKey{
		{gen: 1, songID: 100, name: "百", detail: failedDetail},
		{gen: 1, songMid: "MidA", name: "百", detail: failedDetail},
	} {
		t.Run(fmt.Sprintf("songid %d、songmid %q", k.songID, k.songMid), func(t *testing.T) {
			e := fillEnv()
			e.noRecord = true
			e.byID.set(100, "MidA")
			e.byMid.set("MidA", 100)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(k)
				sleepUntil(e, time.Minute)
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				wantTimes(t, "按 songid 的兜底请求", e.byID.callTimes(), nil)
				wantTimes(t, "按 songmid 的兜底请求", e.byMid.callTimes(), nil)
			})
		})
	}
}

// 恰有一个编号才兜底（设计 §6.4 第 2 步「兜底」）：两个编号都有、编号都缺（本地文件）时已失败也不请求，也不打兜底失败的 Detail。
// 变异自证（实测）：「恰有一个」改为「有编号即可」，「两个编号都有」一例请求了而红；去掉编号个数的条件，另有「编号都缺」一例
// 打了兜底失败的 Detail 而红（空 songmid 不合形式，不请求）。
func TestAssocFillOnlyWithOneID(t *testing.T) {
	for _, c := range []struct {
		name string
		key  songKey
		src  lyricSource
	}{
		{"两个编号都有", songKey{gen: 1, songID: 100, songMid: "MidA", name: "百", detail: failedDetail}, srcAssoc},
		{"编号都缺", songKey{gen: 1, name: "百", detail: failedDetail}, srcNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureAssocLog(t)
			e := fillEnv()
			e.byID.set(100, "MidA")
			e.byMid.set("MidA", 100)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(c.key)
				sleepUntil(e, time.Minute)
				if got := latestResult(t, r); got.kind != resDesired || got.source != c.src {
					t.Fatalf("结果 %+v，want 来源 %d 的期望状态", got, c.src)
				}
				wantTimes(t, "按 songid 的兜底请求", e.byID.callTimes(), nil)
				wantTimes(t, "按 songmid 的兜底请求", e.byMid.callTimes(), nil)
			})
			head := strings.TrimSuffix(i18n.T("Completing the other ID of the current QQ Music song failed; will retry: %v"), "%v")
			if n := countLog(logs(), "+", head); n != 0 {
				t.Fatalf("不该兜底却打了 %d 条兜底失败的 Detail\n%s", n, logs())
			}
		})
	}
}

// 兜底所得跨重开保留、换歌即清（设计 §6.4 第 2 步「交接」、§6.8「输入」）：第 0 秒兜底得 MidA、认出；第 10 秒重开（reopen 1，会话
// 交来的仍是已失败）→ 新一轮扫描（第 10、11、12、14、18 秒）用兜底的 MidA 认出，不再请求；第 20 秒换歌（gen 2，同一 songid，
// 尚无）→ 兜底所得清空：没有候选、不扫，为在线、偏移 0，也不请求。
// 变异自证（实测）：gen 变化不清兜底所得，换歌一项沿用 MidA、认出而红；重开即清兜底所得，重开之后又请求了一次而红。
func TestAssocFillKeptAcrossReopen(t *testing.T) {
	e := fillEnv()
	e.byID.set(100, "MidA")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		k := withDetail(fillKey, failedDetail)
		r.Submit(k)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
		sleepUntil(e, 10*time.Second)
		k.reopen = 1
		r.Submit(k)
		if got := latestResult(t, r); got.reopen != 1 {
			t.Fatalf("重开之后的结果 reopen %d，want 1", got.reopen)
		} else {
			wantDesired(t, got, 1, srcAssoc, 0, false, "", assocLines...)
		}
		sleepUntil(e, 20*time.Second)
		scans := secs(0, 1, 2, 4, 8, 10, 11, 12, 14, 18)
		wantTimes(t, "扫描", e.mem.scanTimes(), scans)
		r.Submit(songKey{gen: 2, songID: 100, name: "百"})
		wantDesired(t, latestResult(t, r), 2, srcOnline, 0, false, "")
		sleepUntil(e, time.Minute)
		wantTimes(t, "换歌之后的扫描", e.mem.scanTimes(), scans)
		wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0))
	})
}
