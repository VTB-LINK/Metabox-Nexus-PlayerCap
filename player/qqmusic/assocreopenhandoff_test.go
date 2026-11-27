package qqmusic

// 本文件测同一首重开时读取者与会话的交接（设计 §6.8「重开」、§7「会话」的重开之后与代次隔离）：真读取者配假依赖（假文件、
// 假内存里的当前歌曲记录），会话一侧照 runSession 的换歌拍与 reopenSong 写入、经 takeResult 取结果、由 lyricState 取舍。
// 重开之前的结果（残留在通道里的、读取者在重开之前开始读而迟到的）一律被拒；停用不论代次都收。

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// 关联表：mid 键 003M → lyrOK（关联 A，即 assocLines）、id 键 100 → lyrB（关联 B，即 assocLines2），都是 byUser=1；
// stream12 写法的 id 键 100 → reopenLyrC（关联 C，即 reopenLinesC），byUser=0、偏移 700。当前歌曲 songid 100、songmid 003M，
// 记录的参数 0 都是 003M。
const (
	reopenS12  = "http://stream12.qqmusic.qq.com/100.wma"
	reopenS4   = "http://stream4.qqmusic.qq.com/100.wma" // 表里没有这一写法的条目
	reopenLyrC = `D:\Lyrics\c_qm.qrc`
)

var (
	reopenKey    = songKey{gen: 1, songID: 100, songMid: "003M", name: "百"}
	onlineTexts  = []string{"a|", "b|"}     // shiftFixture 的原文|翻译
	reopenLinesC = []string{"译文一|", "译文二|"} // reopenLyrC 的原文|翻译
	reopenTables = []assocItem{
		{song: midURL("003M"), lyric: lyrOK, byUser: true},
		{song: idURL(100), lyric: lyrB, byUser: true},
		{song: reopenS12, lyric: reopenLyrC, offset: 700},
	}
)

// reopenEnv 的假内存里当前歌曲是「百」，记录的网址为 url。
func reopenEnv(url string) *readerEnv {
	e := recordEnv(reopenTables...)
	e.fs.put(reopenLyrC, []byte(testTransLRC2))
	e.mem.play("百", url, "003M")
	return e
}

// reopenRecord 把 QQ 内存里当前歌曲的记录换成网址为 url 的那一条（QQ 从另一入口重开同一首）。
func reopenRecord(e *readerEnv, url string) {
	e.mem.clearRecords()
	e.mem.putRecord(fakeAlbum, fakeSinger, "百", url, "003M")
}

// reopenSession 是会话一侧：歌词状态与最近写给读取者的当前歌曲。
type reopenSession struct {
	t   *testing.T
	r   *assocReader
	st  lyricState
	key songKey
}

// claim 照换歌拍：reset、写入，在线取词成功后存在线原始行、按已接受的结果（刚换歌即在线、偏移 0）折算。
func (s *reopenSession) claim(k songKey) {
	s.key = k
	s.st.reset(k.gen, false, time.Time{})
	s.r.Submit(k)
	s.st.setOnlineRaw(shiftFixture())
	s.st.refresh(k.hasIDs(), stateOffsetSec)
}

// reopen 照 runSession 的 reopenSong：reopen 加一、只换代次、写入，gen 不变。
func (s *reopenSession) reopen() {
	s.key.reopen++
	s.st.reopened(s.key.reopen)
	s.r.Submit(s.key)
}

// takeNow 等读取者停下，照每拍取至多一条结果，断言是否要求重发、给出的行（原文|翻译）与此后显示的来源和条目偏移。
func (s *reopenSession) takeNow(changed bool, src lyricSource, offsetMs int, lines ...string) {
	s.t.Helper()
	synctest.Wait()
	s.check(takeResult(&s.st, s.r.Results(), s.key.hasIDs(), stateOffsetSec))(changed, src, offsetMs, lines...)
}

// takeGiven 以只装着 res 的通道调用 takeResult，断言同 takeNow：先从读取者那里取出结果核对过内容，再交给会话。
func (s *reopenSession) takeGiven(res assocResult, changed bool, src lyricSource, offsetMs int, lines ...string) {
	s.t.Helper()
	ch := make(chan assocResult, 1)
	ch <- res
	s.check(takeResult(&s.st, ch, s.key.hasIDs(), stateOffsetSec))(changed, src, offsetMs, lines...)
}

func (s *reopenSession) check(got []lyricLine, ch bool) func(bool, lyricSource, int, ...string) {
	return func(changed bool, src lyricSource, offsetMs int, lines ...string) {
		s.t.Helper()
		if ch != changed || !slices.Equal(lineTexts(got), lines) || s.st.source() != src || s.st.shown.lyricOffsetMs != offsetMs {
			s.t.Fatalf("takeResult 给出 %q、changed %v，此后来源 %d、偏移 %d；want %q、changed %v，来源 %d、偏移 %d",
				lineTexts(got), ch, s.st.source(), s.st.shown.lyricOffsetMs, lines, changed, src, offsetMs)
		}
	}
}

// recvResult 等读取者停下，取走通道里的那一条并核对代次、种类、来源与行。
func recvResult(t *testing.T, r *assocReader, gen, reopen uint64, src lyricSource, lines ...string) assocResult {
	t.Helper()
	got := latestResult(t, r)
	if got.gen != gen || got.reopen != reopen || got.kind != resDesired || got.source != src || !slices.Equal(lineTexts(got.lines), lines) {
		t.Fatalf("结果（%d，%d）kind %d source %d lines %q，want（%d，%d）的期望状态 source %d lines %q",
			got.gen, got.reopen, got.kind, got.source, lineTexts(got.lines), gen, reopen, src, lines)
	}
	return got
}

// 读取者在重开之前开始读关联歌词（阻塞在歌词读取里），期间会话重开并写入：放开后迟到的（1，0）关联 A 被拒、显示仍是在线，
// 随后（1，1）按新记录认出的关联 B 被收。读取者处理（1，1）时阻塞在扫描里，使迟到的那条留在通道里被会话取到。
// 变异自证（实测）：apply 只比 gen，迟到的 A 被收而红；reopened 像 reset 那样清空而红。
func TestReopenLateLyricReadRejected(t *testing.T) {
	e := reopenEnv(midURL("003M"))
	var lyr, scan gate
	e.beforeStart = func() { lyr.arm(); scan.arm() }
	e.fs.onRead = func(p string) {
		if p == lyrOK {
			lyr.wait()
		}
	}
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		defer lyr.release()
		defer scan.release()
		s := &reopenSession{t: t, r: r}
		s.claim(reopenKey)
		synctest.Wait()
		reopenRecord(e, idURL(100))
		e.mem.setHooks(func(uintptr) { scan.wait() }, nil)
		s.reopen()
		lyr.release()
		late := recvResult(t, r, 1, 0, srcAssoc, assocLines...)
		s.takeGiven(late, false, srcOnline, 0)
		scan.release()
		s.takeNow(true, srcAssoc, 0, assocLines2...)
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 0))
	})
}

// 重开之前的结果残留在通道里：（1，0）的关联 A 会话还没取，QQ 从另一入口重开，新入口的记录在表里没有条目。takeResult 拒收
// 残留的 A、不重发、不打来源变化；读取者随后给出（1，1）的在线、偏移 0，与显示的相同，也不重发。
// 变异自证（实测）：apply 只比 gen，残留的 A 被收而红；reopened 像 reset 那样清空而红。
func TestReopenResidueRejected(t *testing.T) {
	logs := captureAssocLog(t)
	e := reopenEnv(midURL("003M"))
	var scan gate
	e.beforeStart = scan.arm
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		defer scan.release()
		s := &reopenSession{t: t, r: r}
		s.claim(reopenKey)
		synctest.Wait()
		reopenRecord(e, reopenS4)
		e.mem.setHooks(func(uintptr) { scan.wait() }, nil)
		s.reopen()
		synctest.Wait()
		if n := len(r.Results()); n != 1 {
			t.Fatalf("通道里应残留（1，0）的那一条，实得 %d 条", n)
		}
		s.takeNow(false, srcOnline, 0)
		scan.release()
		synctest.Wait()
		if n := len(r.Results()); n != 1 {
			t.Fatalf("放开扫描后应有（1，1）的结果，实得 %d 条", n)
		}
		s.takeNow(false, srcOnline, 0)
		wantTimes(t, "读关联 A", e.fs.readsOf(lyrOK), secs(0))
	})
	if n := countLog(logs(), "*", "歌词来源改为"); n != 0 {
		t.Fatalf("不应打来源变化的日志，实得 %d 条：\n%s", n, logs())
	}
}

// 连续两次重开（reopen 1、2）：（1，0）给出 stream12 那条（byUser=0）的关联 C、偏移 700；读取者正读（1，1）的关联 A 时又
// 重开到（1，2）。放开后（1，1）的 A 迟到、被拒，显示仍是关联 C、偏移 700；（1，2）按新记录认出的关联 B 被收。
// 变异自证（实测）：apply 只比 gen，迟到的 A 被收而红；reopened 像 reset 那样清空而红。
func TestReopenTwiceOnlyLatest(t *testing.T) {
	e := reopenEnv(reopenS12)
	var lyrA, lyrBGate gate
	e.beforeStart = func() { lyrA.arm(); lyrBGate.arm() }
	e.fs.onRead = func(p string) {
		switch p {
		case lyrOK:
			lyrA.wait()
		case lyrB:
			lyrBGate.wait()
		}
	}
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		defer lyrA.release()
		defer lyrBGate.release()
		s := &reopenSession{t: t, r: r}
		s.claim(reopenKey)
		s.takeNow(true, srcAssoc, 700, reopenLinesC...)
		reopenRecord(e, midURL("003M"))
		s.reopen()
		synctest.Wait()
		reopenRecord(e, idURL(100))
		s.reopen()
		lyrA.release()
		late := recvResult(t, r, 1, 1, srcAssoc, assocLines...)
		s.takeGiven(late, false, srcAssoc, 700)
		lyrBGate.release()
		s.takeNow(true, srcAssoc, 0, assocLines2...)
		if s.key.reopen != 2 {
			t.Fatalf("会话的 reopen %d，want 2", s.key.reopen)
		}
	})
}

// 停用不受代次过滤（设计 §7「代次隔离」的停用例）：已接受（1，0）的关联 A，读取者重读关联表时 panic、发出（1，0）的停用后
// 退出。会话随后重开到（1，1）再取：退回在线、偏移 0，不保留 A；之后再到的期望状态（任何代次）都不收。换歌到（2，0）之后
// 才取到（1，0）的停用：同样转为停用（显示本就是在线、偏移 0，不重发），此后新 gen 的期望状态也不收。
// 变异自证（实测）：停用也比代次，两例都保留了 A 或收了之后的期望状态而红；refresh 不查停用，「重开之后取到」一例红。
func TestStoppedBeforeReopenStillApplies(t *testing.T) {
	for _, c := range []struct {
		name string
		then func(s *reopenSession)
		want func(s *reopenSession)
		gen  uint64
	}{
		{"重开之后取到", func(s *reopenSession) { s.reopen() }, func(s *reopenSession) {
			s.takeNow(true, srcOnline, 0, onlineTexts...)
		}, 1},
		{"换歌之后取到", func(s *reopenSession) { s.claim(songKey{gen: 2, songID: 200, name: "二百"}) }, func(s *reopenSession) {
			s.takeNow(false, srcOnline, 0)
		}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubAssocReports(t)
			e := reopenEnv(midURL("003M"))
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				s := &reopenSession{t: t, r: r}
				s.claim(reopenKey)
				s.takeNow(true, srcAssoc, 0, assocLines...)
				e.fs.setHooks(func(p string) {
					if p == testTablePath {
						panic("injected")
					}
				}, nil)
				e.fs.put(testTablePath, tableDat(reopenTables...))
				sleepUntil(e, 3*time.Second)
				if n := assocReaders.Load(); n != 0 {
					t.Fatalf("停用之后存活的读取者 %d 个，want 0", n)
				}
				if n := len(r.Results()); n != 1 {
					t.Fatalf("通道里应有（1，0）的停用，实得 %d 条", n)
				}
				c.then(s)
				c.want(s)
				for _, res := range []assocResult{
					atReopen(desiredRes(c.gen, srcAssoc, 0, assocFixture("B"), "B"), s.key.reopen),
					desiredRes(c.gen, srcAssoc, 0, assocFixture("B"), "B"),
					onlineRes(c.gen, -1000),
				} {
					s.takeGiven(res, false, srcOnline, 0)
				}
			})
		})
	}
}

// 重开换键（设计 §6.8 转移的最后一条）：记录由 mid 键网址变为 id 键网址，结果由关联 A 换到关联 B；再从一个新入口重开，记录
// 的网址在表里没有条目，转为在线、偏移 0，用的是换歌拍保存的在线原始行。每次重开扫描一次，gen 一直是 1。
// 变异自证（实测）：reopened 像 reset 那样清空，最后一步的在线原始行为空而红。
func TestReopenSwitchesKey(t *testing.T) {
	e := reopenEnv(midURL("003M"))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		s := &reopenSession{t: t, r: r}
		s.claim(reopenKey)
		s.takeNow(true, srcAssoc, 0, assocLines...)
		reopenRecord(e, idURL(100))
		s.reopen()
		s.takeNow(true, srcAssoc, 0, assocLines2...)
		reopenRecord(e, reopenS4)
		s.reopen()
		s.takeNow(true, srcOnline, 0, onlineTexts...)
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 0, 0))
		if s.key.gen != 1 || s.st.gen != 1 {
			t.Fatalf("重开不改 gen：会话 %d、状态机 %d，want 1", s.key.gen, s.st.gen)
		}
	})
}

// 复扫改换（设计 §7「复扫改换」、§6.8 转移）：不重开，同一代次（1，0）里第 0 秒认出 mid 键（关联 A）；第 0.5 秒 QQ 把记录改写为
// id 键，第 1 秒复扫认出 id 键 → takeResult 换成关联 B、changed 为真一次；第 1.5 秒 mid 键的记录又出现、两种网址并存，第 2 秒复扫
// 按 id 键优先仍认出 id 键 → 仍为 B、changed 为假（第 4 版此处为不唯一，第 5 版并存即取 id 键）；第 3 秒记录改为同一 id 的两种写法
// （streamundefined 与 stream4），第 4 秒复扫不唯一 → 转为在线、偏移 0，用换歌拍保存的在线原始行；第 8 秒仍不唯一，与上次相同 →
// takeResult 不变。
// 变异自证（实测）：recordDue 恢复认出即停，第 1 秒仍为 A 而红；不唯一不推翻已认出的，第 4 秒仍为 B 而红。
func TestRescanSwitchHandoff(t *testing.T) {
	e := reopenEnv(midURL("003M"))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		s := &reopenSession{t: t, r: r}
		s.claim(reopenKey)
		s.takeNow(true, srcAssoc, 0, assocLines...)
		sleepUntil(e, 500*time.Millisecond)
		s.takeNow(false, srcAssoc, 0)
		reopenRecord(e, idURL(100))
		sleepUntil(e, time.Second)
		s.takeNow(true, srcAssoc, 0, assocLines2...)
		sleepUntil(e, 1500*time.Millisecond)
		s.takeNow(false, srcAssoc, 0)
		e.mem.putRecord(fakeAlbum, fakeSinger, "百", midURL("003M"), "003M")
		sleepUntil(e, 2*time.Second)
		s.takeNow(false, srcAssoc, 0)
		sleepUntil(e, 3*time.Second)
		reopenRecord(e, idURL(100))
		e.mem.putRecord(fakeAlbum, fakeSinger, "百", reopenS4, "003M")
		sleepUntil(e, 4*time.Second)
		s.takeNow(true, srcOnline, 0, onlineTexts...)
		for _, at := range secs(8, 60) {
			sleepUntil(e, at)
			s.takeNow(false, srcOnline, 0)
		}
		wantTimes(t, "扫描", e.mem.scanTimes(), secs(0, 1, 2, 4, 8))
		if s.key != reopenKey || s.st.gen != 1 || s.st.reopen != 0 {
			t.Fatalf("复扫不改代次：会话 %+v、状态机（%d，%d），want（1，0）", s.key, s.st.gen, s.st.reopen)
		}
	})
}
