package qqmusic

// 本文件只测读取者给出的期望状态（设计 §6.8 的表，六行各一例）与在线歌曲的撤销关联（§6.8 转移第 1、2 条）：来源、偏移、
// 所带的行、待重试与采纳的本地路径；文件暂缺时每 5 秒重读、读到即换；以及内容摘要随来源、三轨内容与偏移而变、相同输入相同。
// 在线歌曲有候选时，选哪一条由假内存里当前歌曲的记录决定（认键另由 assocrecord_test.go 测），这里放上与所选条目一致的记录。

import (
	"strings"
	"testing"
	"time"
)

const (
	lyrOK           = `D:\Lyrics\ok_qm.qrc`
	lyrTrans        = `D:\Lyrics\ok_qmts.qrc`
	lyrMissing      = `D:\Lyrics\missing.lrc`
	lyrBad          = `D:\Lyrics\bad.lrc`
	localOK         = `E:\Music\本地歌.flac`
	localMiss       = `E:\Music\缺词歌.flac`
	localBad        = `E:\Music\坏词歌.flac`
	localGone       = `E:\Music\已删除.flac`
	lyrLocal        = `E:\Music\本地歌.lrc`
	lyrLocalMissing = `E:\Music\缺词歌.lrc`
)

// desiredEnv 的关联表覆盖期望状态表的各行：在线 id 键 byUser=0 的可用、暂缺两种；在线 mid 键 byUser=1 的可用、暂缺、
// 不可用三种；本地文件的可用、暂缺、不可用三种，另有一条音频文件已不存在的本地条目（不注册）。歌词索引给 lyrOK 配了翻译，
// 索引里的路径大小写与关联表不同。
func desiredEnv() *readerEnv {
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(
		assocItem{song: idURL(2), lyric: lyrOK, offset: -500},
		assocItem{song: idURL(8), lyric: lyrMissing, offset: -700},
		assocItem{song: midURL("003ByUserOk"), lyric: lyrOK, byUser: true, offset: 500},
		assocItem{song: midURL("003Missing"), lyric: lyrMissing, byUser: true, offset: 900},
		assocItem{song: midURL("003Unusable"), lyric: lyrBad, byUser: true, offset: 400},
		assocItem{song: localOK, lyric: lyrLocal, offset: -1000},
		assocItem{song: localMiss, lyric: lyrLocalMissing, byUser: true, offset: 200},
		assocItem{song: localBad, lyric: lyrBad, offset: 100},
		assocItem{song: localGone, lyric: lyrLocal},
	))
	e.fs.put(testIndexPath, indexDat(strings.ToUpper(lyrOK), lyrTrans, ""))
	e.fs.put(lyrOK, []byte(testAssocLRC))
	e.fs.put(lyrTrans, []byte(testTransLRC2))
	e.fs.put(lyrBad, []byte("plain text without timestamps"))
	e.fs.put(lyrLocal, []byte(testAssocLRC))
	for _, p := range []string{localOK, localMiss, localBad} {
		e.fs.put(p, []byte("audio"))
	}
	return e
}

// 在线歌曲（设计 §6.4 第 6 步）：选中的条目不论 byUser 都读其歌词。无条目 → 在线、偏移 0；选中 byUser=0、偏移 -500 的条目
// 且文件可用 → 在线关联、条目的偏移、行取其 lyricUrl 的文件并带翻译（第 3 版为在线、偏移 -500）；byUser=1 同；byUser=0 而
// 所指文件不存在 → 在线、偏移 0、待重试；byUser=1 而文件不可用 → 在线、偏移 0、不待重试。歌词索引的路径不区分大小写。
// 表里另有本地条目：第 5 版起在线歌曲也做本地认定（设计 §6.5），五首各在写入时查一次占用、都没被打开，结果不受影响
// （第 4 版此处断言在线歌曲不查占用，第 5 版改为断言每首查一次）。
// 变异自证（实测）：取用恢复只看 byUser=1（load = ok && e.ByUser），byUser=0 可用一例来源成了在线而红；文件暂缺或不可用时
// 保留条目偏移，byUser=0 暂缺一例偏移为 -700 而红；查歌词索引不转小写，byUser=0 可用一例没有翻译而红。
func TestAssocDesiredOnline(t *testing.T) {
	e := desiredEnv()
	e.mem.putRecord(fakeAlbum, fakeSinger, "二", idURL(2), "002Two")
	e.mem.putRecord(fakeAlbum, fakeSinger, "八", idURL(8), "008Eight")
	e.mem.putRecord(fakeAlbum, fakeSinger, "三", midURL("003ByUserOk"), "003ByUserOk")
	e.mem.putRecord(fakeAlbum, fakeSinger, "四", midURL("003Unusable"), "003Unusable")
	withTrans := []string{"关联第一行|译文一", "关联第二行|译文二"}
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 1, name: "无条目"})
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		e.mem.setPlaying(fakeAlbum, fakeSinger, "二")
		r.Submit(songKey{gen: 2, songID: 2, name: "二"})
		wantDesired(t, latestResult(t, r), 2, srcAssoc, -500, false, "", withTrans...)
		e.mem.setPlaying(fakeAlbum, fakeSinger, "三")
		r.Submit(songKey{gen: 3, songID: 3, songMid: "003ByUserOk", name: "三"})
		wantDesired(t, latestResult(t, r), 3, srcAssoc, 500, false, "", withTrans...)
		e.mem.setPlaying(fakeAlbum, fakeSinger, "八")
		r.Submit(songKey{gen: 4, songID: 8, name: "八"})
		wantDesired(t, latestResult(t, r), 4, srcOnline, 0, true, "")
		e.mem.setPlaying(fakeAlbum, fakeSinger, "四")
		r.Submit(songKey{gen: 5, songMid: "003Unusable", name: "四"})
		wantDesired(t, latestResult(t, r), 5, srcOnline, 0, false, "")
		wantTimes(t, "本地认定", e.rm.callTimes(), secs(0, 0, 0, 0, 0))
	})
}

// 撤销关联（设计 §6.8 转移第 1、2 条）：已认出、选中的条目由关联本地 lrc（byUser=1、偏移 -2000）改为指向该曲自己的缓存
// （byUser=0、偏移 0），关联表变化（第 2 秒重读）后结果为在线关联、行取该缓存、偏移 0；条目被删除（第 4 秒重读）→ 在线、
// 偏移 0。结论按代次缓存，两次都按认出的网址重选。
// 变异自证（实测）：取用恢复只看 byUser=1，撤销之后一例来源成了在线而红。
func TestAssocDesiredRevokeReadsOwnCache(t *testing.T) {
	const userLRC, ownCache = `E:\Lyrics\用户关联.lrc`, `D:\QQTestCache\QQMusicLyricNew\测试歌手 - 百 - 0 - 测试专辑_qm.qrc`
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Song"), lyric: userLRC, byUser: true, offset: -2000}))
	e.fs.put(userLRC, []byte(testAssocLRC))
	e.fs.put(ownCache, []byte(testAssocLRC2))
	e.mem.play("百", midURL("003Song"), "003Song")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songMid: "003Song", name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, -2000, false, "", assocLines...)

		sleepUntil(e, 500*time.Millisecond)
		e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Song"), lyric: ownCache}))
		sleepUntil(e, 2*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines2...)

		sleepUntil(e, 2500*time.Millisecond)
		e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Other"), lyric: userLRC, byUser: true}))
		sleepUntil(e, 4*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		wantTimes(t, "读该曲自己的缓存", e.fs.readsOf(ownCache), secs(2))
	})
}

// byUser=1 的文件暂缺（不存在）：在线、偏移 0、待重试；不存在期间每 5 秒兜底重读；第 12 秒文件出现，第 13 秒的节拍看到
// 变化、第 14 秒仍未变即重读，换成在线关联与条目的偏移，此后不再重读。
// 变异自证（实测）：暂缺不置待重试，首个结果不带待重试而红。
func TestAssocDesiredOnlineMissingFileRetries(t *testing.T) {
	e := desiredEnv()
	e.mem.play("七", midURL("003Missing"), "003Missing")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 7, songMid: "003Missing", name: "七"})
		wantDesired(t, latestResult(t, r), 7, srcOnline, 0, true, "")
		sleepUntil(e, 12*time.Second)
		e.fs.put(lyrMissing, []byte(testAssocLRC2))
		sleepUntil(e, 13900*time.Millisecond)
		if got := latestResult(t, r); got.source != srcOnline || !got.pending {
			t.Fatalf("文件出现之后、稳定一拍之前的结果 %+v，want 在线、待重试", got)
		}
		sleepUntil(e, 14*time.Second)
		wantDesired(t, latestResult(t, r), 7, srcAssoc, 900, false, "", assocLines2...)
		sleepUntil(e, 40*time.Second)
		wantTimes(t, "读暂缺的歌词", e.fs.readsOf(lyrMissing), secs(0, 5, 10, 14))
		noAssocResult(t, r)
	})
}

// 原文可用而翻译、音译暂缺：照常给出原文、置待重试；翻译、音译出现后按变化重读即带上（第 2 秒出现、第 4 秒读到，第 7 秒
// 出现、第 9 秒读到；摘要随之变化），都读到后不再重读。
// 变异自证（实测）：加载函数不报附加轨暂缺，或读取者不理它，首个结果都不带待重试而红。
func TestAssocDesiredAuxTrackMissingRetries(t *testing.T) {
	const auxTrans, auxRoma = `D:\Lyrics\ok_qmts_aux.qrc`, `D:\Lyrics\ok_qmRoma_aux.qrc`
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Aux"), lyric: lyrOK, byUser: true}))
	e.fs.put(testIndexPath, indexDat(lyrOK, auxTrans, auxRoma))
	e.fs.put(lyrOK, []byte(testAssocLRC))
	e.mem.play("一", midURL("003Aux"), "003Aux")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songMid: "003Aux", name: "一"})
		first := latestResult(t, r)
		wantDesired(t, first, 1, srcAssoc, 0, true, "", assocLines...)

		sleepUntil(e, 2*time.Second)
		e.fs.put(auxTrans, []byte(testTransLRC2))
		sleepUntil(e, 5*time.Second)
		withTrans := latestResult(t, r)
		wantDesired(t, withTrans, 1, srcAssoc, 0, true, "", "关联第一行|译文一", "关联第二行|译文二")
		if withTrans.digest == first.digest {
			t.Fatal("翻译读到了，摘要没变")
		}

		sleepUntil(e, 7*time.Second)
		e.fs.put(auxRoma, []byte(testRomaQRC))
		sleepUntil(e, 10*time.Second)
		full := latestResult(t, r)
		wantDesired(t, full, 1, srcAssoc, 0, false, "", "关联第一行|译文一", "关联第二行|译文二")
		if full.lines[0].RomaText == "" || full.digest == withTrans.digest {
			t.Fatalf("音译读到了：首行音译 %q、摘要变化 %v，want 有音译、摘要变化", full.lines[0].RomaText, full.digest != withTrans.digest)
		}
		sleepUntil(e, 30*time.Second)
		wantTimes(t, "读原文", e.fs.readsOf(lyrOK), secs(0, 4, 9))
		noAssocResult(t, r)
	})
}

// 翻译、音译不可用（解析不出行）：只少该轨，不待重试；文件不变即不再重读（同一文件作两轨，只在第 0 秒各读一次），每拍
// 照常 stat 看守。
// 变异自证（实测）：不可用也报暂缺，结果带待重试而红。
func TestAssocDesiredAuxTrackUnusableNotPending(t *testing.T) {
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Aux"), lyric: lyrOK, byUser: true}))
	e.fs.put(testIndexPath, indexDat(lyrOK, lyrBad, lyrBad))
	e.fs.put(lyrOK, []byte(testAssocLRC))
	e.fs.put(lyrBad, []byte("plain text without timestamps"))
	e.mem.play("一", midURL("003Aux"), "003Aux")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songMid: "003Aux", name: "一"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 0, false, "", assocLines...)
		sleepUntil(e, 30*time.Second)
		wantTimes(t, "读原文", e.fs.readsOf(lyrOK), secs(0))
		wantTimes(t, "读不可用的翻译音译", e.fs.readsOf(lyrBad), secs(0, 0))
		if n := len(e.fs.statsOf(lyrBad)); n < 30 {
			t.Fatalf("30 秒内 stat 不可用的翻译音译 %d 次，want 每拍一次", n)
		}
		noAssocResult(t, r)
	})
}

// 本地文件（编号缺位）：认定且文件可用 → 本地文件、条目的偏移、采纳的路径；认定而文件不可用 → 无、不待重试；
// 认定不成（无人打开，或打开的是已不存在的那条）→ 无。
// 有编号（第 5 版，设计 §6.8 的表）：认定且文件可用 → 本地文件，表里有该曲的在线条目（mid 键 003ByUserOk）时同样以本地为准；
// 认定而文件不可用 → 在线、偏移 0、不待重试；认定不成 → 按在线一侧（songid 1 没有候选，为在线、偏移 0）。
// 变异自证（实测）：不存在的音频文件也注册，认定了已删除的那条而红；本地读不成时有编号也给无，有编号的「坏词歌」一例红。
func TestAssocDesiredLocal(t *testing.T) {
	e := desiredEnv()
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		e.rm.setFn(openBy([]string{localOK}, testQQPID))
		r.Submit(songKey{gen: 1, name: "本地歌"})
		wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, localOK, assocLines...)

		e.rm.setFn(openBy([]string{localBad}, testQQPID))
		r.Submit(songKey{gen: 2, name: "坏词歌"})
		wantDesired(t, latestResult(t, r), 2, srcNone, 0, false, "")

		e.rm.setFn(nil)
		r.Submit(songKey{gen: 3, name: "没打开"})
		wantDesired(t, latestResult(t, r), 3, srcNone, 0, false, "")

		e.rm.setFn(openBy([]string{localGone}, testQQPID))
		r.Submit(songKey{gen: 4, name: "已删除"})
		wantDesired(t, latestResult(t, r), 4, srcNone, 0, false, "")

		// 有编号
		e.rm.setFn(openBy([]string{localOK}, testQQPID))
		r.Submit(songKey{gen: 11, songID: 1, name: "本地歌"})
		wantDesired(t, latestResult(t, r), 11, srcLocal, -1000, false, localOK, assocLines...)
		r.Submit(songKey{gen: 12, songMid: "003ByUserOk", name: "本地歌"})
		wantDesired(t, latestResult(t, r), 12, srcLocal, -1000, false, localOK, assocLines...)

		e.rm.setFn(openBy([]string{localBad}, testQQPID))
		r.Submit(songKey{gen: 13, songID: 1, name: "坏词歌"})
		wantDesired(t, latestResult(t, r), 13, srcOnline, 0, false, "")

		e.rm.setFn(nil)
		r.Submit(songKey{gen: 14, songID: 1, name: "没打开"})
		wantDesired(t, latestResult(t, r), 14, srcOnline, 0, false, "")

		e.rm.setFn(openBy([]string{localGone}, testQQPID))
		r.Submit(songKey{gen: 15, songID: 1, name: "已删除"})
		wantDesired(t, latestResult(t, r), 15, srcOnline, 0, false, "")
		for _, p := range e.rm.registered() {
			if strings.EqualFold(p, localGone) {
				t.Fatalf("注册了已不存在的音频文件 %s", p)
			}
		}
	})
}

// 本地文件已认定而歌词暂缺：编号缺位为无、有编号为在线（偏移 0），都待重试；第 2 秒文件出现，稳定一拍之后（第 4 秒）读到即换成
// 本地文件，认定只做一次。
func TestAssocDesiredLocalMissingFileRetries(t *testing.T) {
	for _, c := range []struct {
		name string
		key  songKey
		src  lyricSource
	}{
		{"编号缺位", songKey{gen: 5, name: "缺词歌"}, srcNone},
		{"有编号", songKey{gen: 5, songID: 1, name: "缺词歌"}, srcOnline},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := desiredEnv()
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				e.rm.setFn(openBy([]string{localMiss}, testQQPID))
				r.Submit(c.key)
				wantDesired(t, latestResult(t, r), 5, c.src, 0, true, "")
				sleepUntil(e, 2*time.Second)
				e.fs.put(lyrLocalMissing, []byte(testAssocLRC2))
				sleepUntil(e, 5*time.Second)
				wantDesired(t, latestResult(t, r), 5, srcLocal, 200, false, localMiss, assocLines2...)
				wantTimes(t, "读暂缺的歌词", e.fs.readsOf(lyrLocalMissing), secs(0, 4))
				for _, at := range e.rm.callTimes() {
					if at != 0 {
						t.Fatalf("认定之后又在第 %v 查了占用", at)
					}
				}
			})
		})
	}
}

// 内容摘要：相同输入（同一首歌再提交一次）相同；偏移、原文、翻译任一变化即变；来源不同即不同；在线来源不含关联文件的
// 内容（无条目与关联不可用两种在线、偏移 0 的摘要相同）。
// 变异自证（实测）：摘要不含偏移，改偏移一例红；摘要不含翻译文件的内容，改翻译一例红。
func TestAssocDesiredDigest(t *testing.T) {
	e := desiredEnv()
	e.mem.putRecord(fakeAlbum, fakeSinger, "四", midURL("003Unusable"), "003Unusable")
	e.mem.play("三", midURL("003ByUserOk"), "003ByUserOk")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		key := songKey{gen: 1, songMid: "003ByUserOk", name: "三"}
		r.Submit(key)
		d1 := latestResult(t, r).digest
		key.gen = 2
		r.Submit(key)
		if d := latestResult(t, r).digest; d != d1 {
			t.Fatal("同一首歌再提交一次，摘要变了")
		}

		e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003ByUserOk"), lyric: lyrOK, byUser: true, offset: 600}))
		sleepUntil(e, 3*time.Second)
		got := latestResult(t, r)
		wantDesired(t, got, 2, srcAssoc, 600, false, "", "关联第一行|译文一", "关联第二行|译文二")
		if got.digest == d1 {
			t.Fatal("偏移变了，摘要没变")
		}
		d2 := got.digest

		e.fs.put(lyrTrans, []byte("[00:01.00]新译文一\n[00:03.00]新译文二\n"))
		key.gen = 3
		r.Submit(key)
		if got := latestResult(t, r); got.digest == d2 || got.lines[0].SubText != "新译文一" {
			t.Fatalf("翻译变了：摘要变化 %v、首行翻译 %q，want 摘要变化、新译文一", got.digest != d2, got.lines[0].SubText)
		}

		e.fs.put(lyrOK, []byte(testAssocLRC2))
		key.gen = 4
		r.Submit(key)
		d4 := latestResult(t, r).digest
		if d4 == d2 {
			t.Fatal("原文变了，摘要没变")
		}

		r.Submit(songKey{gen: 5, songID: 1})
		online := latestResult(t, r).digest
		r.Submit(songKey{gen: 6, name: "没打开"})
		none := latestResult(t, r).digest
		if online == none || online == d4 {
			t.Fatal("来源不同，摘要相同")
		}
		e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Unusable"), lyric: lyrBad, byUser: true, offset: 400}))
		sleepUntil(e, 6*time.Second)
		e.mem.setPlaying(fakeAlbum, fakeSinger, "四")
		r.Submit(songKey{gen: 7, songMid: "003Unusable", name: "四"})
		if got := latestResult(t, r); got.source != srcOnline || got.digest != online {
			t.Fatalf("关联不可用时的在线结果 %+v，want 与无条目时摘要相同", got)
		}
	})
}
