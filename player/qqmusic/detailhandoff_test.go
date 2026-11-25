package qqmusic

// 本文件测会话怎样随当前歌曲把详情结果交给读取者（设计 §6.4 第 2 步「交接」、§6.8「换歌拍」「补取块」、§7「编号补全的交接」
// 「请求计数」）。runSession 要真进程、没有离线夹具，这里照它复述与交接相关的几步（detailReplay），同 reopenreread_test.go 的
// 做法：backfillWindow、applyDetail、exhaustDetail 与读取者（fillEnv 等的桩）都是真的，取词、取封面与读取者的两个兜底查询是
// 计数桩，时钟是 synctest 的假时钟。runSession 与这份复述是否一致由 assocwiring_test.go 的源码扫描守形状。

import (
	"encoding/json"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

// detailPoll 是复述的拍长。补取的重试节流由 backfillWindow 管（每 500 ms 一次），与拍长无关。
const detailPoll = 100 * time.Millisecond

// detailReplay 照 runSession 复述：换歌分支（gen 加一、尚无，写入；编号就绪即取词、取封面并写入详情结果，取词失败或编号缺位即
// arm 补取）；补取块（switch backfill.next：放弃与保持两支经 exhaustDetail 写已失败；重试时编号由缺到有即 gen 加一、reopen 归零、
// 详情结果清为尚无并写入，取词成功即取封面并写入详情结果，之后仍为尚无即经 exhaustDetail 写已失败）；判恢复时补取未了结即
// 重设窗口；同一首重开。
type detailReplay struct {
	r          *assocReader
	t0         time.Time
	key        songKey // assocKey
	backfill   backfillWindow
	pending    bool                                     // lyricsPending
	paused     bool                                     // isPaused
	lrcOK      bool                                     // fetchLRC 的桩：真即取词成功
	cover      func(sid uint32, smid string) songDetail // fetchCoverURL 的桩，只取详情结果
	lrcCalls   int
	coverCalls int
	submits    []time.Duration // 每次 Submit 的时刻
}

func newDetailReplay(r *assocReader, cover func(sid uint32, smid string) songDetail) *detailReplay {
	return &detailReplay{r: r, t0: time.Now(), cover: cover}
}

func (p *detailReplay) submit() {
	p.submits = append(p.submits, time.Since(p.t0))
	p.r.Submit(p.key)
}

func (p *detailReplay) fetchLRC() bool {
	p.lrcCalls++
	return p.lrcOK
}

func (p *detailReplay) fetchCover(sid uint32, smid string) songDetail {
	p.coverCalls++
	return p.cover(sid, smid)
}

// handoff 是两处 fetchCoverURL 之后的写入：if k, ok := applyDetail(assocKey, 所查编号, det); ok { assocKey = k; Submit }。
func (p *detailReplay) handoff(sid uint32, smid string, det songDetail) bool {
	if k, ok := applyDetail(p.key, sid, smid, det); ok {
		p.key = k
		p.submit()
		return true
	}
	return false
}

// exhaust 是补取窗口用尽（放弃与保持两支）时的写入。
func (p *detailReplay) exhaust() {
	if k, ok := exhaustDetail(p.key); ok {
		p.key = k
		p.submit()
	}
}

// claimKey 是换歌分支取词之前的部分：gen 加一、整体换新（尚无）并写入。
func (p *detailReplay) claimKey(name string, sid uint32, smid string) {
	p.paused, p.pending = false, false
	p.key = songKey{gen: p.key.gen + 1, songID: sid, songMid: smid, name: name}
	p.submit()
}

// claimFetch 是换歌分支取词及其后的部分：编号就绪即取词、取封面并写入详情结果；取词失败或编号缺位即 arm 补取。
func (p *detailReplay) claimFetch(sid uint32, smid string) {
	ok := false
	if sid != 0 || smid != "" {
		ok = p.fetchLRC()
		p.handoff(sid, smid, p.fetchCover(sid, smid))
	}
	if !ok {
		p.pending = true
		p.backfill.arm(time.Now())
	}
}

func (p *detailReplay) claim(name string, sid uint32, smid string) {
	p.claimKey(name, sid, smid)
	p.claimFetch(sid, smid)
}

// fill 是一拍的补取块，sid、smid 是本拍读到的编号。
func (p *detailReplay) fill(sid uint32, smid string) {
	if !p.pending {
		return
	}
	switch p.backfill.next(time.Now(), p.paused) {
	case backfillGiveUp:
		p.pending = false
		p.exhaust()
	case backfillHold:
		p.exhaust()
	case backfillRetry:
		if sid == 0 && smid == "" {
			return
		}
		if !p.key.hasIDs() {
			p.key.gen++
			p.key.reopen = 0
			p.key.songID, p.key.songMid = sid, smid
			p.key.detail = songDetail{}
			p.submit()
		}
		if p.fetchLRC() {
			p.pending = false
			p.handoff(sid, smid, p.fetchCover(sid, smid))
			p.exhaust() // 所查编号与 assocKey 的不同而衔接不上时仍为尚无：补取已结束，交读取者兜底
		}
	}
}

// resume 是判恢复：补取未了结即重设窗口（#227）。
func (p *detailReplay) resume() {
	p.paused = false
	if p.pending {
		p.backfill.arm(time.Now())
	}
}

// reopen 是同一首重开：reopen 加一并写入，详情结果保留。
func (p *detailReplay) reopen() {
	p.key.reopen++
	p.submit()
}

// beats 自 from 到 to（含）每 detailPoll 一拍补取；ids 给出每拍读到的编号。
func (p *detailReplay) beats(e *readerEnv, from, to time.Duration, ids func(at time.Duration) (uint32, string)) {
	for at := from; at <= to; at += detailPoll {
		sleepUntil(e, at)
		p.fill(ids(at))
	}
}

// idsFrom 是自 at0 起读到 songid 100、之前编号缺位的读数。
func idsFrom(at0 time.Duration) func(at time.Duration) (uint32, string) {
	return func(at time.Duration) (uint32, string) {
		if at >= at0 {
			return 100, ""
		}
		return 0, ""
	}
}

func ms(ns ...int) []time.Duration {
	out := []time.Duration{}
	for _, n := range ns {
		out = append(out, time.Duration(n)*time.Millisecond)
	}
	return out
}

func wantCalls(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s %d 次，want %d", what, got, want)
	}
}

// 设计 §7「编号补全的交接」第 2 轮设计审的反例：0 秒认领编号缺位的歌；1 秒起读到 songid=100，此后每次取词都失败；5 秒判暂停
// （窗口 8 秒到期之前）。详情接口的桩返回 id=100、mid=MidA，关联表与内存记录都指向 MidA。补取只在取词成功之后请求封面，所以
// 窗口到期（8.1 秒那一拍进入保持）时详情结果仍为尚无，记为已失败、写入恰一次，此后保持的各拍不再写；读取者兜底请求一次、认出
// MidA。推进到 30 秒，封面请求 0 次、读取者请求 1 次。30 秒判恢复：补取重设窗口，下一拍取词成功、封面核验通过即改为已得、写入，
// 读取者不再请求。
// 变异自证（实测）：exhaustDetail 对已失败也返回真，保持的每拍都写入（8.1 秒起每 100 ms 一次）而红；本复述的保持一支不写
// （runSession 里漏写由 assocwiring_test.go 的门禁报出），窗口用尽后少一次写入而红。
func TestDetailHandoffHoldWhilePaused(t *testing.T) {
	e := fillEnv()
	e.byID.set(100, "MidA")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		p := newDetailReplay(r, func(uint32, string) songDetail { return gotMidA })
		p.claim("百", 0, "")
		p.beats(e, detailPoll, 5*time.Second, idsFrom(time.Second))
		p.paused = true
		p.beats(e, 5*time.Second+detailPoll, 30*time.Second, idsFrom(time.Second))

		wantTimes(t, "写入读取者", p.submits, ms(0, 1100, 8100)) // 认领、编号由缺到有（gen 2）、窗口用尽记为已失败
		if want := withDetail(songKey{gen: 2, songID: 100, name: "百"}, failedDetail); p.key != want {
			t.Fatalf("assocKey %+v，want %+v", p.key, want)
		}
		wantCalls(t, "取词", p.lrcCalls, 14) // 1.1～7.6 秒每 500 ms 一次
		wantCalls(t, "封面请求", p.coverCalls, 0)
		wantTimes(t, "读取者按 songid 的兜底请求", e.byID.callTimes(), ms(8100))
		wantTimes(t, "读取者按 songmid 的兜底请求", e.byMid.callTimes(), nil)
		wantDesired(t, latestResult(t, r), 2, srcAssoc, 0, false, "", assocLines...)

		p.resume()
		p.lrcOK = true
		p.beats(e, 30*time.Second+detailPoll, time.Minute, idsFrom(0))
		wantTimes(t, "写入读取者", p.submits, ms(0, 1100, 8100, 30100))
		if want := withDetail(songKey{gen: 2, songID: 100, name: "百"}, gotMidA); p.key != want {
			t.Fatalf("恢复之后 assocKey %+v，want %+v", p.key, want)
		}
		wantCalls(t, "封面请求", p.coverCalls, 1)
		wantTimes(t, "读取者按 songid 的兜底请求", e.byID.callTimes(), ms(8100))
		wantDesired(t, latestResult(t, r), 2, srcAssoc, 0, false, "", assocLines...)
	})
}

// 窗口在编号落位之前用尽（暂停中保持）：编号缺位的 assocKey 记为已失败（读取者没有编号可查，不请求）；恢复后编号落位、gen 加一，
// 新 gen 的详情结果为尚无（设计 §6.4 第 2 步：编号由缺到有时 assocKey 整体换新）。这一拍取词失败、没有封面请求，读取者等着、
// 不请求；下一次取词成功，封面核验通过即为已得。读取者一次也不请求。
// 变异自证（实测）：本复述的补取块编号由缺到有时不清详情结果（沿用已失败），gen 2 的 assocKey 带着已失败而红；runSession 里同样
// 的漏写由 assocwiring_test.go 的门禁报出。
func TestDetailHandoffExhaustedBeforeIDs(t *testing.T) {
	e := fillEnv()
	e.byID.set(100, "MidA")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		p := newDetailReplay(r, func(uint32, string) songDetail { return gotMidA })
		p.claim("百", 0, "")
		p.beats(e, detailPoll, 5*time.Second, idsFrom(time.Hour))
		p.paused = true
		p.beats(e, 5*time.Second+detailPoll, 20*time.Second, idsFrom(time.Hour))
		wantTimes(t, "写入读取者", p.submits, ms(0, 8100))
		if want := withDetail(songKey{gen: 1, name: "百"}, failedDetail); p.key != want {
			t.Fatalf("assocKey %+v，want %+v", p.key, want)
		}

		p.resume()
		p.beats(e, 20*time.Second+detailPoll, 20*time.Second+detailPoll, idsFrom(0)) // 20.1 秒：编号落位，取词失败
		if want := (songKey{gen: 2, songID: 100, name: "百"}); p.key != want {
			t.Fatalf("编号落位之后 assocKey %+v，want %+v（尚无）", p.key, want)
		}
		sleepUntil(e, 20*time.Second+3*detailPoll)
		wantTimes(t, "读取者按 songid 的兜底请求", e.byID.callTimes(), nil)
		p.lrcOK = true
		p.beats(e, 20*time.Second+4*detailPoll, time.Minute, idsFrom(0)) // 20.6 秒取词成功
		wantTimes(t, "写入读取者", p.submits, ms(0, 8100, 20100, 20600))
		wantCalls(t, "封面请求", p.coverCalls, 1)
		wantTimes(t, "读取者按 songid 的兜底请求", e.byID.callTimes(), nil)
		wantDesired(t, latestResult(t, r), 2, srcAssoc, 0, false, "", assocLines...)
	})
}

// 补取换了所查编号（终审第 4 版第 2 条，Codex 的反例）：0 秒认领两编号都缺的歌；1 秒起 FindSongMid 得 MidA（1.1 秒那次补取
// gen 加一、assocKey 只有 songmid），取词失败；2 秒起 songid 100 落位（songid 已知即不再扫堆，所查为 (100, "")），2.1 秒那次
// 取词与封面都成功。关联表只有 id=100 的条目，内存里本首的记录为 id 键网址、参数 0 为 MidA。
//   - 封面核验得 MidA：与 assocKey 的 songmid 相同，衔接为已得、songid 100。读取者据此有了候选，2.1 秒起扫描、认出 id 键网址，
//     给出关联歌词，一次也不请求；第 15 秒重开，详情结果保留、重扫一轮，照旧给出关联歌词。
//   - 封面核验得 MidB：衔接不上，不写已得；补取已结束而详情结果仍为尚无，记为已失败并写入，读取者以 assocKey 的 MidA 兜底
//     （按 songmid 查得 songid 100）后认出；重开不再请求。
//
// 变异自证（实测）：applyDetail 不衔接（所查编号不同即不写），「核验得 MidA」一例的 assocKey 为已失败、读取者另发了兜底请求而红；
// 本复述补取成功之后不经 exhaustDetail，「核验得 MidB」一例的 assocKey 停在尚无、读取者不兜底、结果为在线而红（runSession 里
// 漏写由 assocwiring_test.go 的门禁报出）。
func TestDetailHandoffBackfillSwitchesIDs(t *testing.T) {
	ids := func(at time.Duration) (uint32, string) {
		switch {
		case at >= 2*time.Second:
			return 100, ""
		case at >= time.Second:
			return 0, "MidA"
		}
		return 0, ""
	}
	for _, c := range []struct {
		name   string
		mid    string          // 封面核验所得的 mid
		detail songDetail      // 补取之后 assocKey 的详情结果
		byMid  []time.Duration // 读取者按 songmid 的兜底请求
	}{
		{"核验得 MidA", "MidA", songDetail{state: detailGot, songID: 100}, nil},
		{"核验得 MidB", "MidB", failedDetail, ms(2100)},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := recordEnv(assocItem{song: idURL(100), lyric: lyrOK})
			e.mem.play("百", idURL(100), "MidA")
			e.byMid.set("MidA", 100)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				p := newDetailReplay(r, func(sid uint32, smid string) songDetail {
					if sid == 100 && smid == "" {
						return songDetail{state: detailGot, songMid: c.mid}
					}
					return failedDetail
				})
				p.claim("百", 0, "")
				p.beats(e, detailPoll, 1900*time.Millisecond, ids)
				p.lrcOK = true
				p.beats(e, 2*time.Second, 3*time.Second, ids)
				if want := withDetail(songKey{gen: 2, songMid: "MidA", name: "百"}, c.detail); p.key != want {
					t.Fatalf("补取之后 assocKey %+v，want %+v", p.key, want)
				}
				wantTimes(t, "写入读取者", p.submits, ms(0, 1100, 2100))
				wantCalls(t, "取词", p.lrcCalls, 3)
				wantCalls(t, "封面请求", p.coverCalls, 1)

				sleepUntil(e, 15*time.Second)
				wantDesired(t, latestResult(t, r), 2, srcAssoc, 0, false, "", assocLines...)
				wantTimes(t, "读取者按 songmid 的兜底请求", e.byMid.callTimes(), c.byMid)
				wantTimes(t, "读取者按 songid 的兜底请求", e.byID.callTimes(), nil)
				wantTimes(t, "扫描", e.mem.scanTimes(), ms(2100, 3100, 4100, 6100, 10100))

				p.reopen()
				sleepUntil(e, 30*time.Second)
				if got := latestResult(t, r); got.reopen != 1 {
					t.Fatalf("重开之后的结果 reopen %d，want 1", got.reopen)
				} else {
					wantDesired(t, got, 2, srcAssoc, 0, false, "", assocLines...)
				}
				wantTimes(t, "重开之后的扫描", e.mem.scanTimes(), ms(2100, 3100, 4100, 6100, 10100, 15000, 16000, 17000, 19000, 23000))
				wantTimes(t, "重开之后读取者按 songmid 的兜底请求", e.byMid.callTimes(), c.byMid)
				wantCalls(t, "重开之后的封面请求", p.coverCalls, 1)
			})
		})
	}
}

// 请求计数（设计 §7）：换歌拍取词成功、封面请求失败 → 已失败，读取者兜底一次并认出；封面核验通过 → 已得，读取者一次也不请求，
// 同一 gen 的详情查询只有会话的那一次。两例第 10 秒都重开同一首：详情结果保留，读取者重扫一轮、不增加请求，会话也不重查。
// 变异自证（实测）：本复述的换歌分支取过封面不写详情结果，两例的 assocKey 一直为尚无而红（TestDetailHandoffWrongCover 与
// TestDetailHandoffGenIsolation 同红）；runSession 里漏写由 assocwiring_test.go 的门禁报出。
func TestDetailHandoffCoverFailedFallback(t *testing.T) {
	for _, c := range []struct {
		name  string
		cover songDetail
		fills []time.Duration
	}{
		{"封面请求失败", failedDetail, ms(0)},
		{"封面核验通过", gotMidA, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := fillEnv()
			e.byID.set(100, "MidA")
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				p := newDetailReplay(r, func(uint32, string) songDetail { return c.cover })
				p.lrcOK = true
				p.claim("百", 100, "")
				if want := withDetail(songKey{gen: 1, songID: 100, name: "百"}, c.cover); p.key != want {
					t.Fatalf("assocKey %+v，want %+v", p.key, want)
				}
				sleepUntil(e, 10*time.Second)
				wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
				p.reopen()
				sleepUntil(e, time.Minute)
				if got := latestResult(t, r); got.reopen != 1 {
					t.Fatalf("重开之后的结果 reopen %d，want 1", got.reopen)
				} else {
					wantDesired(t, got, 1, srcAssoc, 0, false, "", assocLines...)
				}
				wantTimes(t, "写入读取者", p.submits, ms(0, 0, 10000))
				wantCalls(t, "取词", p.lrcCalls, 1)
				wantCalls(t, "封面请求", p.coverCalls, 1)
				wantTimes(t, "读取者按 songid 的兜底请求", e.byID.callTimes(), c.fills)
				wantTimes(t, "读取者按 songmid 的兜底请求", e.byMid.callTimes(), nil)
				wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8, 10, 11, 12, 14, 18))
			})
		})
	}
}

// 错误编号的会话一侧（设计 §7「错误编号与后缀误命中」）：songid=100，内存里有 A（Album/Singer/Title，MidA）与 B（DeluxeAlbum/
// Singer/Title，MidB），关联表有本首的 id 键条目、A 与 B 的 mid 键条目。封面接口给出另一首（id=200、mid=MidB）、不带 id、或不带 id
// 而 mid 为本首的：经 detailFromTrack 核验不过，记为已失败（带 seen，会话一侧打 Detail），不选中 B；读取者兜底请求得到同一份
// 响应，同样不用、照退避重发。40 秒内结果一直是在线、偏移 0，A、B 的歌词都不读。
// 变异自证（实测）：detailFromTrack 不核对 id（只看 code 与 mid 的形式），三例的详情结果都成了已得（MidB、MidB、MidA）而红。
func TestDetailHandoffWrongCover(t *testing.T) {
	for _, c := range []struct {
		name string
		id   string // track_info 的 id 原文，空即缺省
		mid  string
	}{
		{"id 是另一首的", "200", "MidB"},
		{"缺 id", "", "MidB"},
		{"缺 id、mid 为本首的", "", "MidA"},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubAssocReports(t)
			e := recordEnv(
				assocItem{song: idURL(100), lyric: lyrOK, offset: 700},
				assocItem{song: midURL("MidA"), lyric: lyrOK},
				assocItem{song: midURL("MidB"), lyric: lyrB, byUser: true},
			)
			e.mem.setPlaying("Album", "Singer", "Title")
			e.mem.putRecord("Album", "Singer", "Title", midURL("MidA"), "MidA")
			e.mem.putRecord("DeluxeAlbum", "Singer", "Title", midURL("MidB"), "MidB")
			track := func(queried uint32) songDetail { return detailFromTrack(queried, 0, json.RawMessage(c.id), c.mid) }
			// 读取者的兜底（fetchSongMidByID）对同一份响应同样核验，不是已得即报错。
			e.byID.setFn(func(id uint32) (string, error) {
				d := track(id)
				if d.state != detailGot {
					return "", i18n.Errorf("song detail for songid %d did not confirm the songid (%s)", id, d.seen)
				}
				return d.songMid, nil
			})
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				p := newDetailReplay(r, func(sid uint32, _ string) songDetail { return track(sid) })
				p.lrcOK = true
				p.claim("Title", 100, "")
				if p.key.detail.state != detailFailed || p.key.detail.seen == "" || p.key.detail.songMid != "" || p.key.detail.songID != 0 {
					t.Fatalf("详情结果 %+v，want 已失败、带 seen、不带编号", p.key.detail)
				}
				if q, ok := unconfirmedDetail(p.key, 100, "", p.key.detail); !ok || q != "100" {
					t.Fatalf("unconfirmedDetail = %q, %v；want 100、打 Detail", q, ok)
				}
				sleepUntil(e, 40*time.Second)
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				wantCalls(t, "封面请求", p.coverCalls, 1)
				wantTimes(t, "读取者按 songid 的兜底请求", e.byID.callTimes(), secs(0, 2, 6, 14, 30))
				wantTimes(t, "读 B 的歌词", e.fs.readsOf(lyrB), nil)
				wantTimes(t, "读 A 的歌词", e.fs.readsOf(lyrOK), nil)
			})
		})
	}
}

// 代次隔离（设计 §7「编号补全的交接」）：甲（songid 100）认领时封面已得 MidA、认出甲。第 5 秒同一拍里先换歌（乙，songid 200，
// gen 2，尚无），再以上一首的编号写入（已得 MidA，或核验不过的已失败）：所查编号不等于新 assocKey 里的，不写；乙的封面已得 MidB，
// 只写到 gen 2。读取者 gen 2 认出乙、给出乙的歌词，甲的歌词只在 gen 1 读过；读取者一次也不请求。
// 变异自证（实测）：applyDetail 不比所查编号，上一首的已得写进了 gen 2 而红（TestApplyDetail 同红）。
func TestDetailHandoffGenIsolation(t *testing.T) {
	e := recordEnv(assocItem{song: midURL("MidA"), lyric: lyrOK}, assocItem{song: midURL("MidB"), lyric: lyrB})
	e.mem.play("甲", midURL("MidA"), "MidA")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		p := newDetailReplay(r, func(sid uint32, _ string) songDetail {
			return map[uint32]songDetail{100: gotMidA, 200: gotMidB}[sid]
		})
		p.lrcOK = true
		p.claim("甲", 100, "")
		sleepUntil(e, 5*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)

		e.mem.play("乙", midURL("MidB"), "MidB")
		p.claimKey("乙", 200, "")
		for _, d := range []songDetail{gotMidA, failedSeen} {
			if p.handoff(100, "", d) {
				t.Fatalf("以上一首的编号写入了 %+v", d)
			}
		}
		if want := (songKey{gen: 2, songID: 200, name: "乙"}); p.key != want {
			t.Fatalf("assocKey %+v，want %+v（尚无）", p.key, want)
		}
		p.claimFetch(200, "")
		if want := withDetail(songKey{gen: 2, songID: 200, name: "乙"}, gotMidB); p.key != want {
			t.Fatalf("assocKey %+v，want %+v", p.key, want)
		}
		wantTimes(t, "写入读取者", p.submits, ms(0, 0, 5000, 5000))
		sleepUntil(e, time.Minute)
		wantDesired(t, latestResult(t, r), 2, srcAssoc, 0, false, "", assocLines2...)
		wantTimes(t, "读甲的歌词", e.fs.readsOf(lyrOK), secs(0))
		wantTimes(t, "读乙的歌词", e.fs.readsOf(lyrB), secs(5))
		wantTimes(t, "读取者的兜底请求", e.byID.callTimes(), nil)
	})
}
