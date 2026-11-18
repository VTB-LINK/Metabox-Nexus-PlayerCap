package assoc

// 本文件只测确认之后读取者的停用：读库的结构错误（不支持、损坏，含 -wal 与表结构不认得）即停用并上报一次，
// 此后对每个 hash 回停用、不再读库；读取者内部 panic 转为停用并上报，goroutine 退出、计数归零，不外泄。
// 暂时失败不停用、不上报。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

func TestReaderUnsupportedDBDisables(t *testing.T) {
	cases := map[string]error{
		"-wal（路径大小写不同）": fmt.Errorf("%w: WAL file c:\\kugou8\\KGMusicV3.db-wal exists", kgdb.ErrUnsupportedDB),
		"损坏":            fmt.Errorf("page 7: %w", kgdb.ErrCorrupt),
		"表结构不认得":        errSchema(errors.New("no column UpdateTime")),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			// 上报的错误文本去掉数据目录（裁定 V）；本例的错误文本不含歌词目录与主目录。
			wantErr := strings.ReplaceAll(err.Error(), `c:\kugou8`, "<data>")
			reports := stubReports(t)
			f := &fakeDeps{candsFn: func(int, string) ([]candidate, error) { return nil, err }}
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash("A")
				wantKind(t, latest(t, r), "A", KindDisabled)
				time.Sleep(time.Minute)
				noResult(t, r)
				r.SetHash("B")
				wantKind(t, latest(t, r), "B", KindDisabled)
				wantParses(t, f, "A@0s")

				got := reports()
				if len(got) != 1 || got[0].key != "kugou.lyricdb_unsupported" ||
					got[0].tags["kugou.version"] != okConfirmed.Version ||
					got[0].extra["error"] != wantErr {
					t.Fatalf("上报 %+v，want 一次 kugou.lyricdb_unsupported，带版本标签与去掉路径的错误文本 %q", got, wantErr)
				}
			})
		})
	}
}

// 反例：暂时失败、确认未就绪与不支持都不经这条上报（确认的不支持只停用，不上报读库不支持）。
func TestReaderNoReportWithoutDBError(t *testing.T) {
	cases := map[string]*fakeDeps{
		"暂时失败":  {candsFn: func(int, string) ([]candidate, error) { return nil, errTransientStub }},
		"确认未就绪": {confirmFn: func(int) (watchdog.Confirmed, error) { return watchdog.Confirmed{}, errNotReadyStub }},
		"确认不支持": {confirmFn: func(int) (watchdog.Confirmed, error) { return watchdog.Confirmed{}, errUnsupportedStub }},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			reports := stubReports(t)
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash("A")
				time.Sleep(time.Minute)
				if got := reports(); len(got) != 0 {
					t.Fatalf("上报 %+v，want 没有", got)
				}
			})
		})
	}
}

func TestReaderPanicDisables(t *testing.T) {
	nilMapWrite := func() { var m map[string]int; m["x"] = 1 }
	cases := []struct {
		name      string
		f         *fakeDeps
		hash      string // 空即 panic 发生在启动时的确认里，不写入 hash
		wantPanic string // 上报的 panic 文本（裁定 V：去掉数据目录、歌词目录）
	}{
		{"取候选", &fakeDeps{candsFn: func(int, string) ([]candidate, error) {
			panic(`open C:\KUGOU8\KGMusicV3.db failed`)
		}}, "A", `open <data>\KGMusicV3.db failed`},
		{"读歌词文件", &fakeDeps{readFn: func(time.Duration, string) ([]byte, error) {
			panic(errors.New(`read c:\kugou\lyric\a.lrc failed`))
		}}, "A", `read <lyric>\a.lrc failed`},
		{"写入 hash 时确认", &fakeDeps{confirmFn: func(n int) (watchdog.Confirmed, error) {
			if n == 1 {
				return watchdog.Confirmed{}, errNotReadyStub
			}
			nilMapWrite()
			return okConfirmed, nil
		}}, "A", "assignment to entry in nil map"},
		{"启动时确认", &fakeDeps{confirmFn: func(int) (watchdog.Confirmed, error) {
			nilMapWrite()
			return okConfirmed, nil
		}}, "", "assignment to entry in nil map"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reports := stubReports(t)
			runReader(t, c.f, func(t *testing.T, r *Reader) {
				if c.hash != "" {
					r.SetHash(c.hash)
				}
				wantKind(t, latest(t, r), c.hash, KindDisabled)
				synctest.Wait()
				if n := readers.Load(); n != 0 {
					t.Fatalf("panic 之后存活的读取者 %d 个，want 0", n)
				}
				r.SetHash("B") // 读取者已退出：不阻塞，也没有结果
				noResult(t, r)

				got := reports()
				if len(got) != 1 || got[0].key != "kugou.lyricdb_reader_panic" {
					t.Fatalf("上报 %+v，want 一次 kugou.lyricdb_reader_panic", got)
				}
				if p := got[0].extra["panic"]; p != c.wantPanic ||
					!strings.Contains(fmt.Sprint(got[0].extra["stack"]), "player/kugou/assoc") {
					t.Fatalf("上报 %+v，want panic 文本 %q 与调用栈", got, c.wantPanic)
				}
			})
		})
	}
}
