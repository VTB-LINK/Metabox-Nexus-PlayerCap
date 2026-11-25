package qqmusic

// 本文件只测读取者对歌词索引的缓存（设计 §6.6 第 4 步，§7「大曲库」歌词索引一条的缓存部分）：按读之前的（大小，修改
// 时间）缓存解析成功的映射与稳定的结构错误，未变不重读；第一次的结构错误与 IO 错误不缓存，按暂缺重读。
// 按变化重读的看守另在 assocrereadchange_test.go。

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// indexCacheEnv 是 halfEnv 加上歌词索引 index 与它所指的翻译、音译文件（halfTrans、halfRoma）。index 为 nil 时索引不存在。
func indexCacheEnv(t *testing.T, index []byte) *readerEnv {
	e := halfEnv(qrcFile(t, qrcXML(testMainQRC)))
	if index != nil {
		e.fs.put(testIndexPath, index)
	}
	e.fs.put(halfTrans, qrcFile(t, qrcXML(testTransLRC)))
	e.fs.put(halfRoma, qrcFile(t, qrcXML(testRomaQRC)))
	return e
}

// genKey 是 halfKey 换成代次 gen，即同一首歌又一次换歌写入。
func genKey(gen uint64) songKey {
	k := halfKey
	k.gen = gen
	return k
}

// wantTracks 断言结果为在线关联、待重试如 pending，首行带翻译与音译与否如 withTracks。
func wantTracks(t *testing.T, what string, got assocResult, pending, withTracks bool) {
	t.Helper()
	sub, roma := firstTracks(got.lines)
	if got.source != srcAssoc || got.pending != pending || (sub == "第一行译文" && roma != "") != withTracks || !withTracks && (sub != "" || roma != "") {
		t.Fatalf("%s：来源 %d、待重试 %v、首行翻译 %q 音译 %q；want 在线关联、待重试 %v、带翻译音译 %v",
			what, got.source, got.pending, sub, roma, pending, withTracks)
	}
}

// 索引不变：连续求结果 10 次（同一首换歌 10 次）只读一次；变了即重读，翻译随之换成新索引所指的那份。
// 变异自证（实测）：不缓存，每次求结果都读而红。
func TestIndexCacheParsesOnce(t *testing.T) {
	const otherTrans = `D:\Lyrics\half_other_qmts.lrc`
	e := indexCacheEnv(t, indexDat(halfLyric, halfTrans, halfRoma))
	e.fs.put(otherTrans, []byte("[00:01.00]另一译文\n"))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		for g := uint64(1); g <= 10; g++ {
			r.Submit(genKey(g))
			wantTracks(t, "索引不变时", latestResult(t, r), false, true)
		}
		wantTimes(t, "读歌词索引", e.fs.readsOf(testIndexPath), secs(0))

		sleepUntil(e, 500*time.Millisecond)
		e.fs.put(testIndexPath, indexDat(halfLyric, otherTrans, ""))
		r.Submit(genKey(11))
		if sub, roma := firstTracks(latestResult(t, r).lines); sub != "另一译文" || roma != "" {
			t.Fatalf("索引变了之后首行翻译 %q 音译 %q，want 另一译文、没有音译", sub, roma)
		}
		wantTimes(t, "读歌词索引", e.fs.readsOf(testIndexPath), secs(0, 0.5))
	})
}

// 同一份坏索引（结构错误）：第一次不缓存、按暂缺，第 5 秒兜底照读；两次读之前的 stat 相同、同一结构错误即缓存为稳定，
// 此后不读、不待重试，换歌也不读；仍看守，写成有效的索引之后照常读到翻译与音译。反例：第二次读之前索引已改写成另一份坏
// 索引（stat 不同），不算稳定，仍待重试，第三次读到同一份才缓存。
// 变异自证（实测）：第一次结构错误即缓存，两例首个结果都不待重试而红；稳定之后仍置待重试，两例读到同一份两次之后仍待重试
// 而红。
func TestIndexCacheStableStructuralError(t *testing.T) {
	good := indexDat(halfLyric, halfTrans, halfRoma)
	bad := good[:len(good)/2+1]
	t.Run("同一份", func(t *testing.T) {
		e := indexCacheEnv(t, bad)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(genKey(1))
			wantTracks(t, "第一次结构错误", latestResult(t, r), true, false)
			sleepUntil(e, 5*time.Second)
			wantTracks(t, "第二次同一结构错误", latestResult(t, r), false, false)
			for g := uint64(2); g <= 3; g++ {
				sleepUntil(e, time.Duration(4+g)*time.Second)
				r.Submit(genKey(g))
				wantTracks(t, "稳定之后换歌", latestResult(t, r), false, false)
			}
			sleepUntil(e, 20*time.Second)
			wantTimes(t, "读歌词索引", e.fs.readsOf(testIndexPath), secs(0, 5))

			sleepUntil(e, 20500*time.Millisecond)
			e.fs.put(testIndexPath, good)
			sleepUntil(e, 22*time.Second)
			wantTracks(t, "写成有效的索引之后", latestResult(t, r), false, true)
			wantTimes(t, "读歌词索引", e.fs.readsOf(testIndexPath), secs(0, 5, 22))
		})
	})
	t.Run("两次 stat 不同", func(t *testing.T) {
		e := indexCacheEnv(t, bad)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(genKey(1))
			wantTracks(t, "第一次结构错误", latestResult(t, r), true, false)
			sleepUntil(e, 3*time.Second)
			e.fs.put(testIndexPath, good[:len(good)/2+3])
			sleepUntil(e, 5*time.Second)
			wantTracks(t, "改写成另一份坏索引之后", latestResult(t, r), true, false)
			sleepUntil(e, 10*time.Second)
			wantTracks(t, "同一份读到两次", latestResult(t, r), false, false)
			sleepUntil(e, 30*time.Second)
			wantTimes(t, "读歌词索引", e.fs.readsOf(testIndexPath), secs(0, 5, 10))
		})
	})
}

// IO 错误不缓存（第 4 版第 1 轮设计审的反例）：索引的大小与修改时间一直不变（不再 put），读取桩第一次返回共享冲突、
// 第二次返回有效的索引。第 5 秒兜底，或第 1 秒换歌，都再次实际读取，翻译与音译恢复。
// 变异自证（实测）：IO 错误也按 stat 缓存，两例再次求结果都不读索引、缺翻译音译而红。
func TestIndexCacheIOErrorNotCached(t *testing.T) {
	for _, c := range []struct {
		name string
		at   time.Duration
	}{{"5 秒兜底", 5 * time.Second}, {"换歌", time.Second}} {
		t.Run(c.name, func(t *testing.T) {
			e := indexCacheEnv(t, indexDat(halfLyric, halfTrans, halfRoma))
			var failed atomic.Bool
			e.fs.readErr = func(p string) error {
				if strings.EqualFold(p, testIndexPath) && failed.CompareAndSwap(false, true) {
					return errors.New("the process cannot access the file because it is being used by another process")
				}
				return nil
			}
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(genKey(1))
				wantTracks(t, "索引读失败", latestResult(t, r), true, false)
				sleepUntil(e, c.at)
				if c.name == "换歌" {
					r.Submit(genKey(2))
				}
				wantTracks(t, "再次读取之后", latestResult(t, r), false, true)
				wantTimes(t, "读歌词索引", e.fs.readsOf(testIndexPath), []time.Duration{0, c.at})
			})
		})
	}
}
