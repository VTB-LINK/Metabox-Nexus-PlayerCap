package qqmusic

// 本文件只测读取者 goroutine 的生命周期（设计 §6.8「生命周期」）：同时至多一个；上一个阻塞在读文件里时新会话拿不到
// 读取者、也不读任何文件，它在读取返回后退出、不发结果，之后才能再启动（QQ 重启的情形：新读取者按新进程号过滤占用者）；
// 各状态下取消即退出、计数归零，退出后 Submit 不阻塞；编号兜底请求阻塞中取消，返回后不再求结果、不再请求；内部 panic 转为停用并上报一次（用户主目录脱敏），不外泄；
// 读当前歌曲记录的句柄在读取者退出时、或读取者没起来时恰关一次。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// 变异自证（实测）：不做 CAS，上一个读取者未退出时启动了新的而红；认定写死旧进程号，认定了旧 QQ 打开的文件而红。
func TestAssocReaderSingleInstance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g gate
		g.arm()
		defer g.release()
		old := localEnv()
		old.pid = 100
		old.fs.onRead = func(p string) {
			if p == testTablePath {
				g.wait() // 阻塞的读取：不看 ctx，直到放行
			}
		}
		old.setT0(time.Now())
		ctx1, cancel1 := context.WithCancel(context.Background())
		defer cancel1()
		r1, ok := startAssocReader(ctx1, old.deps())
		if !ok {
			t.Fatal("第一个 startAssocReader 返回假")
		}
		r1.Submit(songKey{gen: 1, name: "一"})
		synctest.Wait()
		cancel1()
		synctest.Wait()

		e := localEnv()
		e.pid = 200
		e.setT0(time.Now())
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		if r, ok := startAssocReader(ctx2, e.deps()); ok || r != nil {
			t.Fatal("上一个读取者未退出时启动了新的读取者")
		}
		if n := assocReaders.Load(); n != 1 {
			t.Fatalf("存活的读取者 %d 个，want 1", n)
		}
		if n := e.fs.readCount(); n != 0 {
			t.Fatalf("未启动的读取者读了 %d 次文件，want 0", n)
		}

		g.release()
		synctest.Wait()
		if n := assocReaders.Load(); n != 0 {
			t.Fatalf("放行之后存活的读取者 %d 个，want 0", n)
		}
		noAssocResult(t, r1)

		e.rm.fn = func(paths []string) ([]uint32, error) {
			var pids []uint32
			if slices.Contains(paths, localOne) {
				pids = append(pids, 100) // 旧的 QQ 进程号
			}
			if slices.Contains(paths, localTwo) {
				pids = append(pids, 200)
			}
			return pids, nil
		}
		r2, ok := startAssocReader(ctx2, e.deps())
		if !ok {
			t.Fatal("上一个读取者退出之后 startAssocReader 返回假")
		}
		r2.Submit(songKey{gen: 1, name: "二"})
		wantDesired(t, latestResult(t, r2), 1, srcLocal, 200, false, localTwo, assocLines2...)
		cancel2()
		synctest.Wait()
		if n := assocReaders.Load(); n != 0 {
			t.Fatalf("取消之后存活的读取者 %d 个，want 0", n)
		}
	})
}

// 读歌词索引阻塞期间会话取消：放行后不再读关联歌词的原文与翻译（下一个文件可能在失联的盘上），不发结果，goroutine 退出。
// 变异自证（实测）：读之前不查 ctx，原文仍被读而红。
func TestAssocReaderCancelStopsFurtherReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g gate
		g.arm()
		defer g.release()
		e := desiredEnv()
		e.mem.play("三", midURL("003ByUserOk"), "003ByUserOk")
		e.fs.onRead = func(p string) {
			if p == testIndexPath {
				g.wait()
			}
		}
		e.setT0(time.Now())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r, ok := startAssocReader(ctx, e.deps())
		if !ok {
			t.Fatal("startAssocReader 返回假")
		}
		r.Submit(songKey{gen: 1, songMid: "003ByUserOk", name: "三"})
		synctest.Wait()
		cancel()
		synctest.Wait()
		if n := assocReaders.Load(); n != 1 {
			t.Fatalf("读取阻塞中存活的读取者 %d 个，want 1", n)
		}
		g.release()
		synctest.Wait()
		wantTimes(t, "读原文", e.fs.readsOf(lyrOK), nil)
		wantTimes(t, "读翻译", e.fs.readsOf(lyrTrans), nil)
		noAssocResult(t, r)
		if n := assocReaders.Load(); n != 0 {
			t.Fatalf("放行之后存活的读取者 %d 个，want 0", n)
		}
	})
}

// 编号兜底请求阻塞期间会话取消（设计 §6.8「生命周期」）：桩放行、返回 MidA 之后不再求结果（不开始扫描、不发结果）、不再请求，
// goroutine 退出。
// 变异自证（实测）：兜底返回之后不查 ctx，放行后开始了一次扫描而红。
func TestAssocReaderCancelStopsFill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g gate
		g.arm()
		defer g.release()
		e := fillEnv()
		e.byID.setFn(func(uint32) (string, error) {
			g.wait()
			return "MidA", nil
		})
		e.setT0(time.Now())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r, ok := startAssocReader(ctx, e.deps())
		if !ok {
			t.Fatal("startAssocReader 返回假")
		}
		r.Submit(withDetail(fillKey, failedDetail))
		wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
		wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0))
		cancel()
		synctest.Wait()
		if n := assocReaders.Load(); n != 1 {
			t.Fatalf("兜底请求阻塞中存活的读取者 %d 个，want 1", n)
		}
		g.release()
		synctest.Wait()
		time.Sleep(time.Minute)
		noAssocResult(t, r)
		wantTimes(t, "兜底请求", e.byID.callTimes(), secs(0))
		wantTimes(t, "扫描", e.mem.scanTimes(), nil)
		if n := assocReaders.Load(); n != 0 {
			t.Fatalf("放行之后存活的读取者 %d 个，want 0", n)
		}
	})
}

// 本地认定中会话取消（阻塞在第一次占用查询里，或阻塞在第一个候选的 stat 里）：放行后不再查占用、不再 stat 任何文件，
// 不发结果，goroutine 退出。
// 变异自证（实测）：占用查询前不查 ctx，放行后对分又查了占用而红；候选 stat 前不查 ctx，放行后又 stat 了第二个候选而红。
func TestAssocReaderCancelStopsLocalIdentify(t *testing.T) {
	for _, blockIn := range []string{"占用查询", "候选 stat"} {
		t.Run(blockIn, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var g gate
				g.arm()
				defer g.release()
				e := localEnv()
				if blockIn == "占用查询" {
					e.rm.fn = func(paths []string) ([]uint32, error) {
						if len(paths) == 2 {
							g.wait() // 第一次查询注册两个候选；放行后报 QQ 占用，未取消时会接着对分
						}
						return []uint32{testQQPID}, nil
					}
				} else {
					e.fs.onStat = func(p string) {
						if p == localOne {
							g.wait()
						}
					}
				}
				e.setT0(time.Now())
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r, ok := startAssocReader(ctx, e.deps())
				if !ok {
					t.Fatal("startAssocReader 返回假")
				}
				r.Submit(songKey{gen: 1, name: "一"})
				synctest.Wait()
				cancel()
				rmBefore, statBefore := len(e.rm.callTimes()), e.fs.statCount()
				g.release()
				synctest.Wait()
				if n := len(e.rm.callTimes()); n != rmBefore {
					t.Fatalf("取消之后又查了 %d 次占用，want 0", n-rmBefore)
				}
				if n := e.fs.statCount(); n != statBefore {
					t.Fatalf("取消之后又 stat 了 %d 次，want 0", n-statBefore)
				}
				noAssocResult(t, r)
				if n := assocReaders.Load(); n != 0 {
					t.Fatalf("放行之后存活的读取者 %d 个，want 0", n)
				}
			})
		})
	}
}

// 各状态下取消：goroutine 退出、计数归零；退出之后 Submit 不阻塞、不再有结果。
// 变异自证（实测）：等待时不选 ctx.Done，取消之后计数仍为 1 而红。
func TestAssocReaderExitsOnCancel(t *testing.T) {
	busy := errors.New("the process cannot access the file")
	missingLyrics := func() *readerEnv {
		e := desiredEnv()
		e.mem.play("一", midURL("003Missing"), "003Missing")
		return e
	}
	recordSchedule := func() *readerEnv {
		e := newReaderEnv()
		e.fs.put(testTablePath, tableDat(assocItem{song: idURL(1), lyric: lyrOK, byUser: true}))
		e.mem.setPlaying(fakeAlbum, fakeSinger, "一")
		return e
	}
	cases := []struct {
		name string
		env  func() *readerEnv
		key  songKey
	}{
		{"没有歌", localEnv, songKey{}},
		{"关联表不存在", newReaderEnv, songKey{gen: 1, songID: 1}},
		{"暂时性失败退避中", func() *readerEnv {
			e := localEnv()
			e.fs.readErr = func(p string) error {
				if p == testTablePath {
					return busy
				}
				return nil
			}
			return e
		}, songKey{gen: 1, songID: 1}},
		{"本地认定补查中", localEnv, songKey{gen: 1, name: "一"}},
		{"歌词暂缺待重试", missingLyrics, songKey{gen: 1, songMid: "003Missing", name: "一"}},
		{"当前歌曲记录的日程中", recordSchedule, songKey{gen: 1, songID: 1, name: "一"}},
		{"编号兜底退避中", fillEnv, songKey{gen: 1, songID: 100, name: "百", detail: songDetail{state: detailFailed}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := c.env()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				e.setT0(time.Now())
				r, ok := startAssocReader(ctx, e.deps())
				if !ok {
					t.Fatal("startAssocReader 返回假")
				}
				if c.key.gen != 0 {
					r.Submit(c.key)
				}
				time.Sleep(3 * time.Second)
				synctest.Wait()
				select {
				case <-r.Results():
				default:
				}
				cancel()
				synctest.Wait()
				if n := assocReaders.Load(); n != 0 {
					t.Fatalf("取消之后存活的读取者 %d 个，want 0", n)
				}
				r.Submit(songKey{gen: 9, songID: 1})
				time.Sleep(time.Minute)
				noAssocResult(t, r)
			})
		})
	}
}

// 读关联歌词阻塞期间会话写入了下一首或重开了这一首，随后 panic：停用结果带最新写入的代次（gen 与 reopen），只发一次。
// 变异自证（实测）：停用时不取尚未取走的歌，两例都带（3，0）而红；停用结果不带 reopen，重开一例带（3，0）而红。
func TestAssocStopCarriesLatestGen(t *testing.T) {
	for _, next := range []songKey{{gen: 4, songID: 1}, {gen: 3, reopen: 1, songMid: "003ByUserOk", name: "三"}} {
		t.Run(fmt.Sprintf("（%d，%d）", next.gen, next.reopen), func(t *testing.T) {
			stubAssocReports(t)
			e := desiredEnv()
			e.mem.play("三", midURL("003ByUserOk"), "003ByUserOk")
			var g gate
			e.beforeStart = g.arm
			e.fs.onRead = func(p string) {
				if p == lyrOK {
					g.wait()
					panic("injected")
				}
			}
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				defer g.release()
				r.Submit(songKey{gen: 3, songMid: "003ByUserOk", name: "三"})
				synctest.Wait()
				r.Submit(next)
				g.release()
				if got := latestResult(t, r); got.kind != resStopped || got.gen != next.gen || got.reopen != next.reopen {
					t.Fatalf("结果 %+v，want（%d，%d）的停用", got, next.gen, next.reopen)
				}
				sleepUntil(e, time.Minute)
				noAssocResult(t, r)
			})
		})
	}
}

// 读当前歌曲记录的句柄恰关一次：读取者因取消、停用或 panic 退出时由它关闭（计数归零时已关）；上一个读取者未退出、
// startAssocReader 返回假时当即关闭，不等上一个退出；没有句柄（record 为 nil）时不 panic。
// 变异自证（实测）：读取者退出时不关，四例都红；不启动时不关，不启动一例红；退出时关两次，四例都红。
func TestAssocReaderClosesRecordHandle(t *testing.T) {
	for _, c := range []struct {
		name string
		env  func() *readerEnv
	}{
		{"取消", localEnv},
		{"停用", func() *readerEnv { e := newReaderEnv(); e.configDir = ""; return e }},
		{"panic", func() *readerEnv {
			e := localEnv()
			e.fs.onRead = func(p string) {
				if p == testTablePath {
					panic("injected")
				}
			}
			return e
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubAssocReports(t)
			e := c.env()
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				if c.name == "取消" && e.mem.closeCount() != 0 {
					t.Fatalf("读取者还在，句柄已关 %d 次", e.mem.closeCount())
				}
			})
			if n := e.mem.closeCount(); n != 1 {
				t.Fatalf("读取者退出之后句柄关闭 %d 次，want 1", n)
			}
		})
	}
	t.Run("不启动", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var g gate
			g.arm()
			defer g.release()
			old := localEnv()
			old.fs.onRead = func(p string) {
				if p == testTablePath {
					g.wait()
				}
			}
			ctx1, cancel1 := context.WithCancel(context.Background())
			defer cancel1()
			if _, ok := startAssocReader(ctx1, old.deps()); !ok {
				t.Fatal("第一个 startAssocReader 返回假")
			}
			synctest.Wait()
			e := localEnv()
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			if _, ok := startAssocReader(ctx2, e.deps()); ok {
				t.Fatal("上一个读取者未退出时启动了新的读取者")
			}
			if n, m := e.mem.closeCount(), old.mem.closeCount(); n != 1 || m != 0 {
				t.Fatalf("没起来的读取者的句柄关闭 %d 次、上一个的 %d 次，want 1、0", n, m)
			}
			e.noRecord = true
			if _, ok := startAssocReader(ctx2, e.deps()); ok {
				t.Fatal("上一个读取者未退出时启动了新的读取者")
			}
			cancel1()
			g.release()
			synctest.Wait()
			if n, m := e.mem.closeCount(), old.mem.closeCount(); n != 1 || m != 1 {
				t.Fatalf("上一个读取者退出之后：没起来的关闭 %d 次、上一个的 %d 次，want 各 1", n, m)
			}
		})
	})
}

// 读取者内部 panic（启动时读关联表、或给歌曲读关联歌词时）：发停用结果（gen 为当时的歌，没有歌时为 0），上报一次
// qqmusic.assoc_reader_panic，panic 文本与栈里的用户主目录（不分大小写，\ 与 / 两种分隔符）换成 <home>；goroutine 退出、
// 不外泄，之后 Submit 不阻塞。栈里的源文件路径在主目录之下时（如本机的工作区）才验得到栈的脱敏。
// 变异自证（实测）：不脱敏，上报文本含主目录而红；只认 \ 分隔符，本机上栈里的 C:/Users/… 未脱敏而红；去掉 recover，
// 测试进程随 panic 退出。
func TestAssocReaderPanicStops(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("取不到用户主目录")
	}
	for _, c := range []struct {
		name    string
		panicOn string
		key     songKey
	}{
		{"启动时读关联表", testTablePath, songKey{}},
		{"读关联歌词", lyrOK, songKey{gen: 3, songMid: "003ByUserOk", name: "三"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			reports := stubAssocReports(t)
			e := desiredEnv()
			e.mem.play("三", midURL("003ByUserOk"), "003ByUserOk")
			e.fs.onRead = func(p string) {
				if p == c.panicOn {
					panic("cannot read " + strings.ToUpper(home) + `\AppData\x.qrc`)
				}
			}
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				if c.key.gen != 0 {
					r.Submit(c.key)
				}
				if got := latestResult(t, r); got.kind != resStopped || got.gen != c.key.gen {
					t.Fatalf("结果 %+v，want gen %d 的停用", got, c.key.gen)
				}
				if n := assocReaders.Load(); n != 0 {
					t.Fatalf("panic 之后存活的读取者 %d 个，want 0", n)
				}
				r.Submit(songKey{gen: 4, songID: 1})
				sleepUntil(e, time.Minute)
				noAssocResult(t, r)
			})
			got := reports()
			if len(got) != 1 || got[0].key != "qqmusic.assoc_reader_panic" || got[0].tags["qqmusic.version"] != testQQVersion {
				t.Fatalf("上报 %+v，want 一次 qqmusic.assoc_reader_panic，带 QQ 版本", got)
			}
			homeRe := regexp.MustCompile("(?i)" + strings.ReplaceAll(regexp.QuoteMeta(filepath.ToSlash(home)), "/", `[\\/]`))
			text, _ := got[0].extra["panic"].(string)
			stack, _ := got[0].extra["stack"].(string)
			if !strings.Contains(text, `<home>\AppData\x.qrc`) || homeRe.MatchString(text) || stack == "" || homeRe.MatchString(stack) {
				t.Fatalf("上报的 panic %q（栈 %d 字节，含主目录 %v），want 主目录换成 <home>", text, len(stack), homeRe.MatchString(stack))
			}
		})
	}
}
