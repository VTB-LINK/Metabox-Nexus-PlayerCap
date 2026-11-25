package qqmusic

// 本文件只测关联歌词按文件变化重读（设计 §6.4 第 6 步、§6.6 第 4 步、§6.8「节拍」，§7「写到一半的缓存」）：所选条目的原文、
// 翻译、音译与歌词索引，不论读成没读成，每拍 stat，（大小，修改时间）与上次读时不同、且与上一拍相同才重读；文件不变不重读、
// 不解密（判为不可用的按读前 stat 缓存）；暂缺另每 5 秒兜底；路径不是绝对路径的不 stat。歌词索引的缓存另在
// associndexcache_test.go。解密只在读到内容之后做（loadLyricTrack），读的次数即解密次数的上限。

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	halfLyric = `D:\Lyrics\half_qm.qrc`
	halfTrans = `D:\Lyrics\half_qmts.qrc`
	halfRoma  = `D:\Lyrics\half_qmRoma.qrc`
)

var (
	halfKey   = songKey{gen: 1, songMid: "003Half", name: "半"}
	halfLines = []string{"第一行|", "A&BC|"} // testMainQRC 的两行
)

// halfEnv 是一首有关联的在线歌曲：mid 键条目指向 halfLyric、偏移 -500，假内存的当前记录认出这个网址。main 为 nil 时
// halfLyric 不存在。
func halfEnv(main []byte) *readerEnv {
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Half"), lyric: halfLyric, offset: -500}))
	if main != nil {
		e.fs.put(halfLyric, main)
	}
	e.mem.play("半", midURL("003Half"), "003Half")
	return e
}

// 原文第一次读到写了一半的 qrc（空文件、截在正文中间、截在 zlib 流中间）：不可用，在线、偏移 0、不待重试；100 ms 后写完，
// 关联表不变、各次扫描都认出同一网址，写完之后 2 秒内换成完整的歌词。
// 变异自证（实测）：不可用不入看守，三例到第 2.1 秒都没有新结果（停在在线）而红。
func TestAssocHalfWrittenMainSwitchesWithin2s(t *testing.T) {
	full := qrcFile(t, qrcXML(testMainQRC))
	half := len(qrcFileHeader) + 8*((len(full)-len(qrcFileHeader))/16) // 正文的前一半块：长度 − 11 是 8 的倍数
	for _, c := range []struct {
		name string
		half []byte
	}{
		{"空文件", []byte{}},
		{"截在 qrc 正文中间", full[:half+3]},
		{"截在 zlib 流中间", full[:half]},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := halfEnv(c.half)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(halfKey)
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				sleepUntil(e, 100*time.Millisecond)
				e.fs.put(halfLyric, full)
				sleepUntil(e, 2100*time.Millisecond)
				wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", halfLines...)
				wantTimes(t, "扫描当前歌曲记录", e.mem.scanTimes(), secs(0, 1, 2))
			})
		})
	}
}

// 不可用的原文不变：连续 10 拍只 stat，读一次；其间每拍都有一次代次不变的写入使读取者求结果，原文按读前 stat 直接给出
// 不可用，不读、不解密。
// 变异自证（实测）：不可用不按 stat 缓存，每次求结果都重读而红。
func TestAssocUnchangedUnusableNotReread(t *testing.T) {
	e := halfEnv([]byte("plain text without timestamps"))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(halfKey)
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		for i := 1; i <= 10; i++ {
			sleepUntil(e, time.Duration(i)*time.Second+500*time.Millisecond)
			r.Submit(halfKey)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		}
		wantTimes(t, "读原文", e.fs.readsOf(halfLyric), secs(0))
		stats := e.fs.statsOf(halfLyric)
		for i := 1; i <= 10; i++ {
			if !slices.Contains(stats, time.Duration(i)*time.Second) {
				t.Fatalf("第 %d 拍没有 stat 原文（各次 %v）", i, stats)
			}
		}
	})
}

// firstTracks 返回首行的翻译与音译；没有行时都为空。
func firstTracks(lines []lyricLine) (sub, roma string) {
	if len(lines) == 0 {
		return "", ""
	}
	return lines[0].SubText, lines[0].RomaText
}

// 写的过程中相邻两拍大小都在变：不读；稳定一拍：读一次，换成完整的歌词。
// 变异自证（实测）：去掉「与上一拍相同」的稳定要求，写的过程中每拍都重读、第 4 秒即读到写完的文件，第 4.9 秒之前多出
// 结果而红。
func TestAssocStillWritingWaitsStable(t *testing.T) {
	full := qrcFile(t, qrcXML(testMainQRC))
	e := halfEnv([]byte{})
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(halfKey)
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		for i, n := range []int{20, 40, 60} {
			sleepUntil(e, time.Duration(i)*time.Second+500*time.Millisecond)
			e.fs.put(halfLyric, full[:n])
		}
		sleepUntil(e, 3500*time.Millisecond)
		e.fs.put(halfLyric, full)
		sleepUntil(e, 4900*time.Millisecond)
		noAssocResult(t, r)
		sleepUntil(e, 5*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", halfLines...)
		sleepUntil(e, 12*time.Second)
		wantTimes(t, "读原文", e.fs.readsOf(halfLyric), secs(0, 5))
	})
}

// 翻译不可用、音译暂缺、歌词索引没读成（写到一半）、歌词索引不存在各一例：100 ms 后写完，2 秒内补上该轨，原文照常。
// 索引不存在时不待重试（见 TestAssocMissingIndexWatchedNotPending）。
// 变异自证（实测）：附加轨与索引不入看守，前三例到第 2.1 秒都没有新结果（仍缺该轨）而红；索引不存在时不入看守，
// 「索引不存在」一例同样红。
func TestAssocAuxAndIndexWatched(t *testing.T) {
	trans, roma := qrcFile(t, qrcXML(testTransLRC)), qrcFile(t, qrcXML(testRomaQRC))
	index := indexDat(halfLyric, halfTrans, halfRoma)
	for _, c := range []struct {
		name          string
		path          string
		before, after []byte // before 为 nil 即写完之前不存在
		pending       bool   // 写完之前的结果是否待重试
	}{
		{"翻译不可用", halfTrans, trans[:len(trans)-1], trans, false},
		{"音译暂缺", halfRoma, nil, roma, true},
		{"索引没读成", testIndexPath, index[:len(index)/2+1], index, true},
		{"索引不存在", testIndexPath, nil, index, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := halfEnv(qrcFile(t, qrcXML(testMainQRC)))
			for p, b := range map[string][]byte{testIndexPath: index, halfTrans: trans, halfRoma: roma} {
				if p != c.path {
					e.fs.put(p, b)
				}
			}
			if c.before != nil {
				e.fs.put(c.path, c.before)
			}
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(halfKey)
				first := latestResult(t, r)
				if sub, roma := firstTracks(first.lines); first.source != srcAssoc || first.pending != c.pending || sub != "" && roma != "" {
					t.Fatalf("写完之前的结果：来源 %d、待重试 %v、首行翻译 %q 音译 %q；want 在线关联、待重试 %v、缺一轨",
						first.source, first.pending, sub, roma, c.pending)
				}
				sleepUntil(e, 100*time.Millisecond)
				e.fs.put(c.path, c.after)
				sleepUntil(e, 2100*time.Millisecond)
				got := latestResult(t, r)
				if sub, roma := firstTracks(got.lines); got.source != srcAssoc || got.pending || sub != "第一行译文" || roma == "" {
					t.Fatalf("写完 2 秒内的结果：来源 %d、待重试 %v、首行翻译 %q 音译 %q；want 在线关联、不待重试、两轨都在",
						got.source, got.pending, sub, roma)
				}
			})
		})
	}
}

// 歌词索引不存在：没有翻译音译、不待重试，此后不出现即不再求结果、不再读它，每拍照常 stat 看守。
// 变异自证（实测）：索引不存在也置待重试，首个结果带待重试而红；不入看守，第 1～12 拍没有 stat 而红。
func TestAssocMissingIndexWatchedNotPending(t *testing.T) {
	e := halfEnv(qrcFile(t, qrcXML(testMainQRC)))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(halfKey)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", halfLines...)
		sleepUntil(e, 12*time.Second)
		noAssocResult(t, r)
		wantTimes(t, "读歌词索引", e.fs.readsOf(testIndexPath), secs(0))
		stats := e.fs.statsOf(testIndexPath)
		for i := 1; i <= 12; i++ {
			if !slices.Contains(stats, time.Duration(i)*time.Second) {
				t.Fatalf("第 %d 拍没有 stat 歌词索引（各次 %v）", i, stats)
			}
		}
	})
}

// 因合并规模跳过的轨不是文件本身不可用，不按 stat 记下（第 4 版 Task 18 代码审的反例）：
//   - 翻译与原文是同一文件、1,000 行：翻译超出合并规模而跳过；同一首连续求结果两次，两次都保留 1,000 行原文、只跳过翻译，
//     原文不因翻译被跳过而判为不可用。
//   - 原文与翻译各 1,000 行：翻译跳过；原文不变时再求结果照读翻译（照旧跳过）；原文改写成 10 行（翻译不变）之后再求结果，
//     翻译合并上。
//
// 变异自证（实测）：跳过的轨也按 stat 记为不可用（e992a27 的做法，或 loadLocalLyrics 不标 skipped），「同一文件」一例
// 第二次退回在线而红，「原文变短」一例原文变短之后翻译仍被跳过而红。
func TestAssocMergeSkipNotCachedAsUnusable(t *testing.T) {
	t.Run("同一文件", func(t *testing.T) {
		e := halfEnv(timedLRC(1000, "原文"))
		e.fs.put(testIndexPath, indexDat(halfLyric, halfLyric, ""))
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			for i, at := range []time.Duration{0, 1500 * time.Millisecond} {
				sleepUntil(e, at)
				r.Submit(halfKey)
				got := latestResult(t, r)
				if sub, _ := firstTracks(got.lines); got.source != srcAssoc || got.pending || len(got.lines) != 1000 || sub != "" {
					t.Fatalf("第 %d 次求结果：来源 %d、待重试 %v、%d 行、首行翻译 %q，want 在线关联、不待重试、1000 行且没有翻译",
						i+1, got.source, got.pending, len(got.lines), sub)
				}
			}
		})
	})
	t.Run("原文变短", func(t *testing.T) {
		e := halfEnv(timedLRC(1000, "原文"))
		e.fs.put(testIndexPath, indexDat(halfLyric, halfTrans, ""))
		e.fs.put(halfTrans, timedLRC(1000, "译文"))
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(halfKey)
			if got := latestResult(t, r); got.source != srcAssoc || len(got.lines) != 1000 || got.lines[0].SubText != "" {
				t.Fatalf("1,000 行与 1,000 行：来源 %d、%d 行，want 在线关联、1000 行且没有翻译", got.source, len(got.lines))
			}
			sleepUntil(e, 1500*time.Millisecond)
			r.Submit(halfKey)
			latestResult(t, r)

			e.fs.put(halfLyric, timedLRC(10, "原文"))
			sleepUntil(e, 2500*time.Millisecond)
			r.Submit(halfKey)
			got := latestResult(t, r)
			if sub, _ := firstTracks(got.lines); got.source != srcAssoc || len(got.lines) != 10 || sub != "译文0" {
				t.Fatalf("原文改写成 10 行之后：来源 %d、%d 行、首行翻译 %q，want 在线关联、10 行、译文0", got.source, len(got.lines), sub)
			}
			wantTimes(t, "读翻译", e.fs.readsOf(halfTrans), secs(0, 1.5, 2.5))
		})
	})
}

// 附加轨文件本身不可用（解析不出行）而不变：原文改写之后再求结果，原文照读、换上新内容，翻译仍按读前 stat 直接给出
// 不可用、不读——文件本身的不可用与原文无关。
// 变异自证（实测）：不可用的记录只在原文不变时才认（e992a27 的做法），或附加轨不认记录，原文改写之后都又读了一次翻译而红。
func TestAssocAuxUnusableIndependentOfMain(t *testing.T) {
	e := halfEnv([]byte(testAssocLRC))
	e.fs.put(testIndexPath, indexDat(halfLyric, lyrBad, ""))
	e.fs.put(lyrBad, []byte("plain text without timestamps"))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(halfKey)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", assocLines...)
		e.fs.put(halfLyric, []byte(testAssocLRC2))
		sleepUntil(e, 1500*time.Millisecond)
		r.Submit(halfKey)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", assocLines2...)
		wantTimes(t, "读原文", e.fs.readsOf(halfLyric), secs(0, 1.5))
		wantTimes(t, "读不可用的翻译", e.fs.readsOf(lyrBad), secs(0))
	})
}

// 读不到而 stat 一直不变（共享冲突使打开失败）：在线、待重试，第 5 秒兜底重读后换上。反例：同一情形改为不可用（stat
// 不变，第 3 秒内容在原长度内改成有效的歌词也不变）：不待重试，到第 12 秒都不重读。
// 变异自证（实测）：去掉 5 秒兜底，「暂缺」一例第 5 秒没有新结果（仍为在线）而红；不可用也置待重试，「不可用」一例首个
// 结果即带待重试而红。
func TestAssocMissingFallback5s(t *testing.T) {
	t.Run("暂缺", func(t *testing.T) {
		e := halfEnv(qrcFile(t, qrcXML(testMainQRC)))
		var busy atomic.Bool
		busy.Store(true)
		e.fs.readErr = func(p string) error {
			if busy.Load() && strings.EqualFold(p, halfLyric) {
				return errors.New("the process cannot access the file because it is being used by another process")
			}
			return nil
		}
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(halfKey)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, true, "")
			sleepUntil(e, 3*time.Second)
			busy.Store(false)
			sleepUntil(e, 4900*time.Millisecond)
			noAssocResult(t, r)
			sleepUntil(e, 5*time.Second)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", halfLines...)
			sleepUntil(e, 12*time.Second)
			wantTimes(t, "读原文", e.fs.readsOf(halfLyric), secs(0, 5))
		})
	})
	t.Run("不可用", func(t *testing.T) {
		bad := []byte("plain text without timestamps, padded to the length of the valid lyric ....")
		valid := []byte(testAssocLRC)
		valid = append(valid, []byte(strings.Repeat(" ", len(bad)-len(valid)))...)
		e := halfEnv(bad)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(halfKey)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, 3*time.Second)
			e.fs.putSameStat(halfLyric, valid)
			sleepUntil(e, 12*time.Second)
			noAssocResult(t, r)
			wantTimes(t, "读原文", e.fs.readsOf(halfLyric), secs(0))
		})
	})
}

// lyricUrl 是相对路径：不可用，在线、偏移 0、不待重试；从不 stat、不读它。
// 变异自证（实测）：看守不查绝对路径，每拍 stat 一次而红。
func TestAssocRelativePathNotWatched(t *testing.T) {
	const rel = `Lyrics\half_qm.qrc`
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(assocItem{song: midURL("003Half"), lyric: rel, offset: -500}))
	e.fs.put(rel, qrcFile(t, qrcXML(testMainQRC)))
	e.mem.play("半", midURL("003Half"), "003Half")
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(halfKey)
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		sleepUntil(e, 10*time.Second)
		wantTimes(t, "stat 相对路径", e.fs.statsOf(rel), nil)
		wantTimes(t, "读相对路径", e.fs.readsOf(rel), nil)
	})
}

// 读成的原文也看守（终审第 4 版第 1 条）：有效关联表指向 halfLyric，首次读到歌词 A；关联表与记录网址不变，halfLyric 被覆盖为
// 歌词 B（「搜索歌词」把所选的歌词写进该曲自己的缓存文件，路径不变）：大小与修改时间都变、只变修改时间、只变大小各一例，写完之后
// 2 秒内换成 B。其间关联表只在启动时读过一次，各次扫描都认出同一网址。
// 变异自证（实测，仓库外的副本）：只有读不成的入看守（修复之前的做法），三例到第 2.1 秒都没有新结果（停在 A）而红。
func TestAssocValidLyricOverwrittenSwitchesWithin2s(t *testing.T) {
	sameLen := "[00:01.00]关联第三行\n[00:03.00]关联第四行\n" // 与 testAssocLRC 同长
	for _, c := range []struct {
		name string
		put  func(m *memFS)
		want []string
	}{
		{"大小与修改时间都变", func(m *memFS) { m.put(halfLyric, []byte(testAssocLRC2)) }, assocLines2},
		{"只变修改时间", func(m *memFS) { m.put(halfLyric, []byte(sameLen)) }, []string{"关联第三行|", "关联第四行|"}},
		{"只变大小", func(m *memFS) { m.putKeepMod(halfLyric, []byte(testAssocLRC2)) }, assocLines2},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := halfEnv([]byte(testAssocLRC))
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(halfKey)
				wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", assocLines...)
				sleepUntil(e, 100*time.Millisecond)
				c.put(e.fs)
				sleepUntil(e, 2100*time.Millisecond)
				wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", c.want...)
				wantTimes(t, "读原文", e.fs.readsOf(halfLyric), secs(0, 2))
				wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0))
				wantTimes(t, "扫描当前歌曲记录", e.mem.scanTimes(), secs(0, 1, 2))
			})
		})
	}
}

// 读成的翻译、音译也看守：三轨与歌词索引都读成，随后翻译或音译被覆盖为另一份有效的内容，写完之后 2 秒内换上，原文照常。
// 变异自证（实测，仓库外的副本）：只有读不成的入看守，两例到第 2.1 秒都没有新结果而红。
func TestAssocValidAuxOverwrittenSwitchesWithin2s(t *testing.T) {
	for _, c := range []struct {
		name, path string
		after      string // 改写之后的明文
		sub, roma  string // 改写之后首行的翻译与音译；音译只比是否改变
	}{
		{"翻译", halfTrans, strings.ReplaceAll(testTransLRC, "译文", "新译"), "第一行新译", ""},
		{"音译", halfRoma, strings.ReplaceAll(testRomaQRC, "ichi", "ni"), "第一行译文", "changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := halfEnv(qrcFile(t, qrcXML(testMainQRC)))
			e.fs.put(testIndexPath, indexDat(halfLyric, halfTrans, halfRoma))
			e.fs.put(halfTrans, qrcFile(t, qrcXML(testTransLRC)))
			e.fs.put(halfRoma, qrcFile(t, qrcXML(testRomaQRC)))
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(halfKey)
				first := latestResult(t, r)
				sub0, roma0 := firstTracks(first.lines)
				if first.source != srcAssoc || first.pending || sub0 != "第一行译文" || roma0 == "" {
					t.Fatalf("改写之前：来源 %d、待重试 %v、首行翻译 %q 音译 %q；want 在线关联、不待重试、两轨都在", first.source, first.pending, sub0, roma0)
				}
				sleepUntil(e, 100*time.Millisecond)
				e.fs.put(c.path, qrcFile(t, qrcXML(c.after)))
				sleepUntil(e, 2100*time.Millisecond)
				got := latestResult(t, r)
				sub, roma := firstTracks(got.lines)
				if got.source != srcAssoc || got.pending || sub != c.sub || roma == "" || (c.roma != "") == (roma == roma0) {
					t.Fatalf("改写 2 秒内：来源 %d、待重试 %v、首行翻译 %q 音译 %q（改写之前 %q）；want 在线关联、翻译 %q、音译改变 %v",
						got.source, got.pending, sub, roma, roma0, c.sub, c.roma != "")
				}
				wantTimes(t, "读原文", e.fs.readsOf(halfLyric), secs(0, 2))
			})
		})
	}
}

// 读成的歌词索引也看守：索引首次读成而没有该曲的翻译（没有该曲的条目、有条目而 tsurl 为空各一例），随后 QQ 补入翻译：写完之后
// 2 秒内补上翻译，原文照常。
// 变异自证（实测，仓库外的副本）：读成的索引不入看守（修复之前的做法），两例到第 2.1 秒都没有新结果（仍缺翻译）而红。
func TestAssocIndexAddsTransWithin2s(t *testing.T) {
	for _, c := range []struct {
		name   string
		before []byte
	}{
		{"没有该曲的条目", indexDat(`D:\Lyrics\other_qm.qrc`, `D:\Lyrics\other_qmts.qrc`, "")},
		{"条目的 tsurl 为空", indexDat(halfLyric, "", "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := halfEnv([]byte(testAssocLRC))
			e.fs.put(halfTrans, []byte(testTransLRC2))
			e.fs.put(testIndexPath, c.before)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(halfKey)
				wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", assocLines...)
				sleepUntil(e, 100*time.Millisecond)
				e.fs.put(testIndexPath, indexDat(halfLyric, halfTrans, ""))
				sleepUntil(e, 2100*time.Millisecond)
				wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", "关联第一行|译文一", "关联第二行|译文二")
				wantTimes(t, "读歌词索引", e.fs.readsOf(testIndexPath), secs(0, 2))
				wantTimes(t, "读翻译", e.fs.readsOf(halfTrans), secs(2))
			})
		})
	}
}

// 三轨与歌词索引都读成且不变：此后每拍只 stat，不重读、不发结果，读计数保持为第 0 秒的一次。每拍 stat 的恰是关联表与这 4 个
// 文件各一次（看守至多 4 个）。
// 变异自证（实测，仓库外的副本）：读成的三轨不入看守，第 1 拍起只 stat 关联表与索引而红；读成的索引不入看守，第 1 拍起少 stat
// 索引而红。
func TestAssocValidFilesStatOnly(t *testing.T) {
	e := halfEnv(qrcFile(t, qrcXML(testMainQRC)))
	e.fs.put(testIndexPath, indexDat(halfLyric, halfTrans, halfRoma))
	e.fs.put(halfTrans, qrcFile(t, qrcXML(testTransLRC)))
	e.fs.put(halfRoma, qrcFile(t, qrcXML(testRomaQRC)))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(halfKey)
		if got := latestResult(t, r); got.source != srcAssoc || got.pending {
			t.Fatalf("首个结果：来源 %d、待重试 %v；want 在线关联、不待重试", got.source, got.pending)
		}
		sleepUntil(e, 10500*time.Millisecond)
		noAssocResult(t, r)
		watched := []string{halfLyric, halfTrans, halfRoma, testIndexPath}
		for _, p := range watched {
			wantTimes(t, "读 "+p, e.fs.readsOf(p), secs(0))
		}
		want := []string{}
		for _, p := range append(watched, testTablePath) {
			want = append(want, strings.ToLower(p))
		}
		slices.Sort(want)
		for i := 1; i <= 10; i++ {
			if got := e.fs.statPathsAt(time.Duration(i) * time.Second); !slices.Equal(got, want) {
				t.Fatalf("第 %d 拍 stat 了 %q，want 关联表与看守的 4 个文件各一次 %q", i, got, want)
			}
		}
	})
}

// 本地文件的关联歌词同一规则：写了一半（空文件）时为无、不待重试，写完之后 2 秒内换成本地文件的歌词。
// 变异自证（实测）：不可用不入看守，到第 2.1 秒没有新结果（停在无）而红。
func TestAssocLocalHalfWritten(t *testing.T) {
	const song, lyric = `E:\Music\半写.flac`, `E:\Music\半写.lrc`
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(assocItem{song: song, lyric: lyric, offset: -1000}))
	e.fs.put(song, []byte("audio"))
	e.fs.put(lyric, []byte{})
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		e.rm.setFn(openBy([]string{song}, testQQPID))
		r.Submit(songKey{gen: 1, name: "半写"})
		wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
		sleepUntil(e, 100*time.Millisecond)
		e.fs.put(lyric, []byte(testAssocLRC))
		sleepUntil(e, 2100*time.Millisecond)
		wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, song, assocLines...)
	})
}
