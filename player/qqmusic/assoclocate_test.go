package qqmusic

// 本文件只测读取者定位关联表（设计 §6.2）：缓存目录取自 QQ 配置里的 CachePath.ini；读不出、解不开、没有该节点或
// 不是绝对路径时退回 <配置目录>\QQMusicCache\，同一原因每首歌只打一条 Detail；关联表不存在时每 10 秒重新定位；
// 配置目录本身不是绝对路径时停用。

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func secs(ss ...float64) []time.Duration {
	out := []time.Duration{}
	for _, s := range ss {
		out = append(out, time.Duration(s*float64(time.Second)))
	}
	return out
}

// wantTimes 断言 got 与 want 相同；两者都为空视为相同。
func wantTimes(t *testing.T, what string, got, want []time.Duration) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s 的时刻 %v，want %v", what, got, want)
	}
}

// 配置给出的缓存目录即用：读该目录下的关联表，不读默认目录，不打退回的 Detail。
func TestAssocLocateUsesConfiguredCacheDir(t *testing.T) {
	e := offsetEnv()
	e.fs.put(testTablePath, offsetTable(-500))
	e.fs.put(testDefaultTablePath, offsetTable(300))
	logs := captureAssocLog(t)
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, -500, false, "", assocLines...)
		sleepUntil(e, 25*time.Second)
		wantTimes(t, "读配置", e.fs.readsOf(testConfigPath), secs(0))
		wantTimes(t, "读配置里的关联表", e.fs.readsOf(testTablePath), secs(0))
		wantTimes(t, "读默认目录的关联表", e.fs.readsOf(testDefaultTablePath), nil)
	})
	if n := countLog(logs(), "+", `QQMusicCache`); n != 0 {
		t.Fatalf("打了 %d 条退回默认目录的 Detail，want 0\n%s", n, logs())
	}
}

// 配置不存在、解不开、没有缓存目录节点、路径不是绝对路径：都退回 <配置目录>\QQMusicCache\，只打一条 Detail。
// 变异自证（实测）：退回目录改为配置目录本身，各例读不到默认目录的关联表而红。
func TestAssocLocateFallsBackToDefaultCacheDir(t *testing.T) {
	for name, config := range map[string][]byte{
		"配置不存在":    nil,
		"解不开":      bytes.Repeat([]byte{0x11}, 64),
		"没有缓存目录节点": testDat(testXMLDecl + `<Config><Domain DomainName="Other.ini" Path="D:\QQTestCache\"/></Config>`),
		"相对路径":     configDat(`QQTestCache\`),
	} {
		t.Run(name, func(t *testing.T) {
			e := offsetEnv()
			if config == nil {
				e.fs.remove(testConfigPath)
			} else {
				e.fs.put(testConfigPath, config)
			}
			e.fs.put(testTablePath, offsetTable(-500))
			e.fs.put(testDefaultTablePath, offsetTable(300))
			logs := captureAssocLog(t)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, songID: 100, name: "百"})
				wantDesired(t, latestResult(t, r), 1, srcAssoc, 300, false, "", assocLines...)
				sleepUntil(e, 25*time.Second)
				wantTimes(t, "读配置里的目录（不该读）", e.fs.readsOf(testTablePath), nil)
			})
			if n := countLog(logs(), "+", testQQConfigDir+`\QQMusicCache`); n != 1 {
				t.Fatalf("退回默认目录的 Detail %d 条，want 1\n%s", n, logs())
			}
		})
	}
}

// 关联表不存在：每 10 秒重新定位一次（读配置），同一原因的退回 Detail 只打一条；结果为未知。配置改指另一个目录、关联表
// 在那里出现后，下一次定位即找到，稳定 1 秒后解析，此后不再定位。
// 变异自证（实测）：重新定位的间隔改为 5 秒，读配置的时刻不符而红；关联表存在时也重新定位，第 40 秒仍读配置而红。
func TestAssocRelocatesEvery10sWhileTableMissing(t *testing.T) {
	e := offsetEnv()
	e.fs.remove(testConfigPath)
	logs := captureAssocLog(t)
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: 100, name: "百"})
		if got := latestResult(t, r); got.kind != resUnknown || got.gen != 1 {
			t.Fatalf("关联表不存在时结果 %+v，want gen 1 的未知", got)
		}
		sleepUntil(e, 25*time.Second)
		wantTimes(t, "读配置", e.fs.readsOf(testConfigPath), secs(0, 10, 20))
		noAssocResult(t, r)

		e.fs.put(testConfigPath, configDat(`E:\Moved\`))
		moved := `E:\Moved\QQMusicLyricNew\LyricAssociation.dat`
		e.fs.put(moved, offsetTable(700))
		sleepUntil(e, 45*time.Second)
		wantTimes(t, "读配置", e.fs.readsOf(testConfigPath), secs(0, 10, 20, 30))
		wantTimes(t, "读新目录的关联表", e.fs.readsOf(moved), secs(32))
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 700, false, "", assocLines...)
	})
	if n := countLog(logs(), "+", testQQConfigDir+`\QQMusicCache`); n != 1 {
		t.Fatalf("退回默认目录的 Detail %d 条，want 1（同一原因、同一首歌）\n%s", n, logs())
	}
}

// defaultAssocDeps：配置目录为 %APPDATA%\Tencent\QQMusic，进程号、版本与读记录的来源原样带上（nil 即不读），各项依赖都有。
// 只构造，不读任何文件。
func TestDefaultAssocDeps(t *testing.T) {
	appdata, err := os.UserConfigDir()
	if err != nil {
		t.Skip("取不到用户配置目录")
	}
	rec := (&fakeMem{}).source()
	for _, want := range []*recordSource{nil, rec} {
		d := defaultAssocDeps(77, "22.82.0.0", want)
		if d.configDir != filepath.Join(appdata, "Tencent", "QQMusic") || d.qqPID != 77 || d.qqVersion != "22.82.0.0" || d.record != want {
			t.Fatalf("configDir %q、qqPID %d、qqVersion %q、record %p（want %p）", d.configDir, d.qqPID, d.qqVersion, d.record, want)
		}
		if d.read == nil || d.stat == nil || d.holders == nil || d.resolveID == nil || d.resolveMid == nil || d.now == nil {
			t.Fatal("有依赖为 nil")
		}
	}
}

// 配置目录不是绝对路径（取不到 %APPDATA%）：不读任何文件，停用，发一条停用结果后退出。
func TestAssocStopsWithoutConfigDir(t *testing.T) {
	for _, dir := range []string{"", `Tencent\QQMusic`} {
		e := newReaderEnv()
		e.configDir = dir
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			if got := latestResult(t, r); got.kind != resStopped || got.gen != 0 {
				t.Fatalf("配置目录 %q：结果 %+v，want gen 0 的停用", dir, got)
			}
			if n := assocReaders.Load(); n != 0 {
				t.Fatalf("配置目录 %q：停用之后存活的读取者 %d 个，want 0", dir, n)
			}
			r.Submit(songKey{gen: 1, songID: 100, name: "百"})
			sleepUntil(e, time.Minute)
			noAssocResult(t, r)
			if n := e.fs.readCount(); n != 0 {
				t.Fatalf("配置目录 %q：读了 %d 次文件，want 0", dir, n)
			}
		})
	}
}
