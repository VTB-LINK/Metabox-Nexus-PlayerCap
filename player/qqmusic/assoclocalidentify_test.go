package qqmusic

// 本文件只测读取者何时做本地文件认定（设计 §6.5）：不论有无 songid、songmid 都做（第 5 版）；换歌后第 0、1、2、4、8 秒各查
// 一次，认定即停，查满即停到换歌或关联表变化；Restart Manager 报致命错误后本会话不再查（Warn 一次），其余错误照常补查；
// 占用者按 QQ 的进程号过滤；上一首采纳过的文件不注册。

import (
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
)

const (
	localOne = `E:\Music\一.flac`
	localTwo = `E:\Music\二.flac`
)

// localEnv 的关联表有两个本地条目（都是存在的普通文件），各关联一份歌词。
func localEnv() *readerEnv {
	e := newReaderEnv()
	e.fs.put(testTablePath, tableDat(
		assocItem{song: localOne, lyric: lyrLocal, offset: 100},
		assocItem{song: localTwo, lyric: lyrOK, offset: 200},
	))
	e.fs.put(lyrLocal, []byte(testAssocLRC))
	e.fs.put(lyrOK, []byte(testAssocLRC2))
	e.fs.put(localOne, []byte("audio"))
	e.fs.put(localTwo, []byte("audio"))
	return e
}

// 一直没有被 QQ 打开：第 0、1、2、4、8 秒各查一次（每次一个会话注册两个候选），之后不再查；换歌后重新从第 0 秒起。
// 变异自证（实测）：查满之后不停，第 16 秒仍查而红；换歌不复位，第 60 秒之后不查而红。
func TestAssocLocalIdentifySchedule(t *testing.T) {
	e := localEnv()
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, name: "一"})
		wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
		sleepUntil(e, 60*time.Second)
		wantTimes(t, "本地认定", e.rm.callTimes(), secs(0, 1, 2, 4, 8))
		noAssocResult(t, r)
		r.Submit(songKey{gen: 2, name: "二"})
		sleepUntil(e, 120*time.Second)
		wantTimes(t, "本地认定", e.rm.callTimes(), secs(0, 1, 2, 4, 8, 60, 61, 62, 64, 68))
	})
}

// 第 3.5 秒 QQ 才打开文件：第 4 秒那次认定成功（含对分的查询），随即给出本地文件，此后不再查。
func TestAssocLocalIdentifyLateOpen(t *testing.T) {
	e := localEnv()
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, name: "二"})
		wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
		sleepUntil(e, 3500*time.Millisecond)
		e.rm.setFn(openBy([]string{localTwo}, testQQPID))
		sleepUntil(e, 4*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcLocal, 200, false, localTwo, assocLines2...)
		sleepUntil(e, 30*time.Second)
		for _, at := range e.rm.callTimes() {
			if at > 4*time.Second {
				t.Fatalf("认定之后又在第 %v 查了占用", at)
			}
		}
	})
}

// 查满之后关联表变化：从第 0 秒重新查起。
func TestAssocLocalIdentifyRestartsOnTableChange(t *testing.T) {
	e := localEnv()
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, name: "一"})
		sleepUntil(e, 20500*time.Millisecond)
		e.rm.setFn(openBy([]string{localOne}, testQQPID))
		e.fs.put(testTablePath, tableDat(
			assocItem{song: localOne, lyric: lyrLocal, offset: 150},
			assocItem{song: localTwo, lyric: lyrOK, offset: 200},
		))
		sleepUntil(e, 22*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcLocal, 150, false, localOne, assocLines...)
	})
}

// Restart Manager 报 ERROR_SEM_TIMEOUT、ERROR_WRITE_FAULT、ERROR_MAX_SESSIONS_REACHED（经 %w 包装）：本会话不再查，换歌也不查，
// Warn 一次。
// 变异自证（实测）：致命错误按其余错误处理，第 1 秒仍查而红。
func TestAssocLocalIdentifyFatalStopsForSession(t *testing.T) {
	for _, code := range []syscall.Errno{121, 29, 353} {
		t.Run(code.Error(), func(t *testing.T) {
			logs := captureAssocLog(t)
			e := localEnv()
			e.rm.fn = func([]string) ([]uint32, error) { return nil, i18n.Errorf("RmStartSession failed: %w", code) }
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, name: "一"})
				wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
				sleepUntil(e, 20*time.Second)
				r.Submit(songKey{gen: 2, name: "二"})
				wantDesired(t, latestResult(t, r), 2, srcNone, 0, false, "")
				sleepUntil(e, 60*time.Second)
				wantTimes(t, "本地认定", e.rm.callTimes(), secs(0))
			})
			if n := countLog(logs(), "!", ""); n != 1 {
				t.Fatalf("Warn %d 条，want 1\n%s", n, logs())
			}
		})
	}
}

// 其余错误（ERROR_ACCESS_DENIED）：照常第 1、2、4、8 秒补查，不 Warn。
func TestAssocLocalIdentifyOtherErrorsRetry(t *testing.T) {
	logs := captureAssocLog(t)
	e := localEnv()
	e.rm.fn = func([]string) ([]uint32, error) {
		return nil, i18n.Errorf("RmGetList failed: %w", windows.ERROR_ACCESS_DENIED)
	}
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, name: "一"})
		sleepUntil(e, 30*time.Second)
		wantTimes(t, "本地认定", e.rm.callTimes(), secs(0, 1, 2, 4, 8))
	})
	if n := countLog(logs(), "!", ""); n != 0 {
		t.Fatalf("Warn %d 条，want 0\n%s", n, logs())
	}
}

// 有 songid 或 songmid 时同样做本地认定（设计 §6.5 第 6 步，第 5 版：不论有无编号）：只有 songid、只有 songmid、两者都有各一例，
// 表里没有这首的在线条目。第 0、1、2 秒各查一次没打开、为在线；第 3.5 秒 QQ 打开 localOne，第 4 秒那次认定（一次注册两个候选、
// 对分两次，共 3 个会话），随即为本地文件，此后不再查（第 8 秒那次不做）。第 4 版的反例：这三例都不查、一直为在线。
// 变异自证（实测）：localDue 留着 w.online()，三例都不查而红。
func TestAssocLocalIdentifyWithIDs(t *testing.T) {
	for _, c := range []struct {
		name string
		key  songKey
	}{
		{"只有 songid", songKey{gen: 1, songID: 5, name: "一"}},
		{"只有 songmid", songKey{gen: 1, songMid: "001X", name: "一"}},
		{"两者都有", songKey{gen: 1, songID: 5, songMid: "001X", name: "一"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := localEnv()
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(c.key)
				wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
				sleepUntil(e, 3500*time.Millisecond)
				noAssocResult(t, r)
				e.rm.setFn(openBy([]string{localOne}, testQQPID))
				sleepUntil(e, 4*time.Second)
				wantDesired(t, latestResult(t, r), 1, srcLocal, 100, false, localOne, assocLines...)
				sleepUntil(e, 30*time.Second)
				wantTimes(t, "本地认定", e.rm.callTimes(), secs(0, 1, 2, 4, 4, 4))
				noAssocResult(t, r)
			})
		})
	}
}

// 文件只被别的进程（网易云）打开不算；QQ 也打开之后才认定。上一首采纳过的文件即使被 QQ 打开也不注册。
// 变异自证（实测）：不传排除的路径，第二首认定了上一首的文件而红。
func TestAssocLocalIdentifyFiltersByPIDAndExclude(t *testing.T) {
	const otherPID = 999
	e := localEnv()
	e.rm.fn = openBy([]string{localOne}, otherPID)
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, name: "一"})
		wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
		sleepUntil(e, 1500*time.Millisecond)
		e.rm.setFn(openBy([]string{localOne}, otherPID, testQQPID))
		sleepUntil(e, 2*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcLocal, 100, false, localOne, assocLines...)

		before := len(e.rm.registered())
		r.Submit(songKey{gen: 2, name: "一", excludeLocal: `e:\music\一.FLAC`})
		wantDesired(t, latestResult(t, r), 2, srcNone, 0, false, "")
		sleepUntil(e, 30*time.Second)
		after := e.rm.registered()[before:]
		if len(after) == 0 {
			t.Fatal("排除上一首之后没有再查")
		}
		for _, p := range after {
			if p == localOne {
				t.Fatalf("排除的上一首文件仍被注册：%s", p)
			}
		}
	})
}
