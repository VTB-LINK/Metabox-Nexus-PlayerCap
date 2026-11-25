package qqmusic

// 本文件只测读取者何时重读关联表、读坏了怎么办（设计 §6.3 第 4、5 步，§6.8「节拍」）：（大小，修改时间）变化后稳定
// 1 秒才重读；写了一半（截断、末块被改）为暂时性失败、结果为未知、下次成功即恢复；暂时性失败按 1、2、4、8 秒退避后
// 保持 8 秒，成功或文件变化即复位；两次读到同一份坏文件、（大小，修改时间）未变且错误相同才判为不支持并停用。

import (
	"errors"
	"testing"
	"time"
)

// offsetLyric 是 offsetTable 那条条目所指的歌词。选中的条目不论 byUser 都读其歌词（设计 §6.4 第 6 步），读得到即在线关联、
// 条目的偏移：各例以偏移标出读的是哪一份关联表。
const offsetLyric = `D:\x.qrc`

func offsetTable(offset int) []byte {
	return tableDat(assocItem{song: idURL(100), lyric: offsetLyric, offset: offset})
}

// offsetEnv 是放好 offsetLyric（内容为 testAssocLRC）的读取者环境，假内存里当前歌曲是「百」，记录为 songid 100 的 id 键网址。
func offsetEnv() *readerEnv {
	e := newReaderEnv()
	e.mem.play("百", idURL(100), "003Song")
	e.fs.put(offsetLyric, []byte(testAssocLRC))
	return e
}

// 文件在 2.5 秒变化：第 3 秒的节拍看到变化，第 4 秒仍未变才重读；在 5.5、6.5 秒接连变化时等到 8 秒；不变不重读。
// 变异自证（实测）：去掉「与上一秒相同」的条件，第 3 秒即重读而红。
func TestAssocTableRereadAfterStable(t *testing.T) {
	e := offsetEnv()
	e.fs.put(testTablePath, offsetTable(100))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 100, false, "", assocLines...)

		sleepUntil(e, 2500*time.Millisecond)
		e.fs.put(testTablePath, offsetTable(200))
		sleepUntil(e, 3900*time.Millisecond)
		noAssocResult(t, r)
		sleepUntil(e, 4*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 200, false, "", assocLines...)

		sleepUntil(e, 5500*time.Millisecond)
		e.fs.put(testTablePath, offsetTable(300))
		sleepUntil(e, 6500*time.Millisecond)
		e.fs.put(testTablePath, offsetTable(400))
		sleepUntil(e, 20*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 400, false, "", assocLines...)
		wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 4, 8))
	})
}

// 解析成功之后关联表不见了（如 QQ 清了缓存）：当前歌与之后的新歌结果都为未知，不再按旧表给；原样回来（大小与修改时间
// 都不变）后稳定 1 秒即重读。
// 变异自证（实测）：不见了仍按旧表，不发未知而红；作废时不清解析记录，原样回来后不重读而红。
func TestAssocTableVanishedIsUnknown(t *testing.T) {
	e := offsetEnv()
	e.fs.put(testTablePath, offsetTable(100))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 100, false, "", assocLines...)
		sleepUntil(e, 1500*time.Millisecond)
		f := e.fs.hide(testTablePath)
		sleepUntil(e, 2*time.Second)
		if got := latestResult(t, r); got.kind != resUnknown || got.gen != 1 {
			t.Fatalf("关联表不见了：结果 %+v，want gen 1 的未知", got)
		}
		r.Submit(songKey{gen: 2, songID: 100, name: "百"})
		if got := latestResult(t, r); got.kind != resUnknown || got.gen != 2 {
			t.Fatalf("关联表不见了之后的新歌：结果 %+v，want gen 2 的未知", got)
		}
		sleepUntil(e, 5500*time.Millisecond)
		e.fs.unhide(testTablePath, f)
		sleepUntil(e, 7*time.Second)
		wantDesired(t, latestResult(t, r), 2, srcAssoc, 100, false, "", assocLines...)
		wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 7))
	})
}

// 截断（长度不是 8 的倍数、末 7 字节不为 0）与末块被改：结果为未知；写完之后的下一次读取成功即恢复。
// 变异自证（实测）：结构错误按停用处理（不经两次确认），三例都在第一次读坏时即停用而红。
func TestAssocTableTornIsTransient(t *testing.T) {
	good := offsetTable(222)
	tampered := append([]byte(nil), good...)
	tampered[len(tampered)-1] ^= 0x40
	for name, bad := range map[string][]byte{
		"截断 3 字节": good[:len(good)-3],
		"截断一块":    good[:len(good)-8],
		"末块被改":    tampered,
	} {
		t.Run(name, func(t *testing.T) {
			e := offsetEnv()
			e.fs.put(testTablePath, offsetTable(100))
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, songID: 100, name: "百"})
				wantDesired(t, latestResult(t, r), 1, srcAssoc, 100, false, "", assocLines...)
				sleepUntil(e, 1500*time.Millisecond)
				e.fs.put(testTablePath, bad)
				sleepUntil(e, 3*time.Second)
				if got := latestResult(t, r); got.kind != resUnknown || got.gen != 1 {
					t.Fatalf("读到写了一半的关联表：结果 %+v，want gen 1 的未知", got)
				}
				r.Submit(songKey{gen: 2, songID: 100, name: "百"})
				if got := latestResult(t, r); got.kind != resUnknown || got.gen != 2 {
					t.Fatalf("暂时性失败期间的新歌：结果 %+v，want gen 2 的未知", got)
				}
				sleepUntil(e, 3500*time.Millisecond)
				e.fs.put(testTablePath, good)
				sleepUntil(e, 4*time.Second)
				wantDesired(t, latestResult(t, r), 2, srcAssoc, 222, false, "", assocLines...)
				wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 3, 4))
			})
		})
	}
}

// 读取出错（不是结构错误）：每次都为暂时性，退避 1、2、4、8 秒后保持 8 秒，文件未变也不停用；文件变化稳定后立即重读并
// 复位退避；一次成功之后再失败从 1 秒起。
// 变异自证（实测）：退避不封顶，第 23 秒没有重读而红；文件变化不复位退避，第 27 秒没有重读而红。
func TestAssocTableTransientBackoff(t *testing.T) {
	e := offsetEnv()
	e.fs.put(testTablePath, offsetTable(100))
	busy := errors.New("the process cannot access the file")
	failTable := func(p string) error {
		if p == testTablePath {
			return busy
		}
		return nil
	}
	e.fs.readErr = failTable
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		sleepUntil(e, 24500*time.Millisecond)
		wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 1, 3, 7, 15, 23))
		e.fs.put(testTablePath, offsetTable(200))
		sleepUntil(e, 40*time.Second)
		wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 1, 3, 7, 15, 23, 26, 27, 29, 33))
		if got := latestResult(t, r); got.kind != resUnknown || got.gen != 1 {
			t.Fatalf("一直读不出：结果 %+v，want gen 1 的未知", got)
		}

		e.fs.setHooks(nil, nil)
		sleepUntil(e, 41*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 200, false, "", assocLines...)

		e.fs.setHooks(nil, failTable)
		e.fs.put(testTablePath, offsetTable(300))
		sleepUntil(e, 47*time.Second)
		wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 1, 3, 7, 15, 23, 26, 27, 29, 33, 41, 43, 44, 46))
	})
}

// 两次读到同一份坏文件、（大小，修改时间）未变、错误相同：停用，发停用结果，上报一次，goroutine 退出。
// 反例：两次之间修改时间变了（内容与错误都相同，QQ 又写了一遍），或错误不同（大小与修改时间相同、内容改写），都还是
// 暂时性，下一次相同才停用。
// 变异自证（实测）：不比较（大小，修改时间），「修改时间变了」一例在第 1 秒即停用而红；不比较错误文本，「错误不同」一例在
// 第 1 秒即停用而红。
func TestAssocTableUnsupportedAfterTwoSameBadReads(t *testing.T) {
	badA := testDat(testXMLDecl + "<AssociateInfoA></AssociateInfoA>")
	badB := testDat(testXMLDecl + "<AssociateInfoB></AssociateInfoB>")
	truncated := offsetTable(100)
	truncated = truncated[:len(truncated)-3]
	for _, c := range []struct {
		name     string
		first    []byte
		at05     func(m *memFS) // 第 0.5 秒对文件做的改动，nil 即不动
		stopAt   time.Duration
		wantRead []time.Duration
	}{
		{"同一份坏文件", truncated, nil, time.Second, secs(0, 1)},
		{"修改时间变了", truncated, func(m *memFS) { m.put(testTablePath, truncated) }, 3 * time.Second, secs(0, 1, 3)},
		{"错误不同", badA, func(m *memFS) { m.putSameStat(testTablePath, badB) }, 3 * time.Second, secs(0, 1, 3)},
	} {
		t.Run(c.name, func(t *testing.T) {
			reports := stubAssocReports(t)
			logs := captureAssocLog(t)
			e := newReaderEnv()
			e.fs.put(testTablePath, c.first)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 9, songID: 100, name: "百"})
				if got := latestResult(t, r); got.kind != resUnknown {
					t.Fatalf("第一次读坏：结果 %+v，want 未知", got)
				}
				if c.at05 != nil {
					sleepUntil(e, 500*time.Millisecond)
					c.at05(e.fs)
				}
				sleepUntil(e, c.stopAt-time.Millisecond)
				if n := assocReaders.Load(); n != 1 {
					t.Fatalf("第 %v 之前读取者已退出", c.stopAt)
				}
				sleepUntil(e, c.stopAt)
				if got := latestResult(t, r); got.kind != resStopped || got.gen != 9 {
					t.Fatalf("结果 %+v，want gen 9 的停用", got)
				}
				if n := assocReaders.Load(); n != 0 {
					t.Fatalf("停用之后存活的读取者 %d 个，want 0", n)
				}
				r.Submit(songKey{gen: 10, songID: 100, name: "百"})
				sleepUntil(e, time.Minute)
				noAssocResult(t, r)
				wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), c.wantRead)
			})
			got := reports()
			if len(got) != 1 || got[0].key != "qqmusic.assoc_unsupported" || got[0].tags["qqmusic.version"] != testQQVersion {
				t.Fatalf("上报 %+v，want 一次 qqmusic.assoc_unsupported，带 QQ 版本", got)
			}
			if n := countLog(logs(), "!", ""); n != 1 {
				t.Fatalf("Warn %d 条，want 1\n%s", n, logs())
			}
		})
	}
}
