package assoc

// 本文件只测读取者的确认闸门（设计 §6.2「时机」，裁定 U）：读取者启动即确认一次；未通过时每次写入 hash 与
// 每 10 秒（不论有没有 hash）各再确认一次，未就绪累计 30 次即停用；不支持立即停用；确认通过之前对每个写入的
// hash 回未确认、不读库，没有 hash 时不发结果（停用除外）；停用后不再确认、不读库；确认通过后对当前 hash
// 解析，没有 hash 则只记下确认结果，之后写入 hash 立即解析、不再确认；库路径与歌词目录取自确认结果。

import (
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

func notReadyUntil(n int) func(int) (watchdog.Confirmed, error) {
	return func(i int) (watchdog.Confirmed, error) {
		if i <= n {
			return watchdog.Confirmed{}, errNotReadyStub
		}
		return okConfirmed, nil
	}
}

func wantConfirms(t *testing.T, f *fakeDeps, want ...time.Duration) {
	t.Helper()
	if got := confirmCalls(f); !slices.Equal(got, want) {
		t.Fatalf("确认时刻 %v，want %v", got, want)
	}
}

func TestReaderConfirmsAtStart(t *testing.T) {
	f := &fakeDeps{}
	runReader(t, f, func(t *testing.T, r *Reader) {
		wantConfirms(t, f, 0) // 没写入 hash 也确认
		noResult(t, r)        // 没有 hash：只记下确认结果，不发结果
		time.Sleep(time.Hour)
		noResult(t, r)
		wantConfirms(t, f, 0)
		wantParses(t, f)

		r.SetHash("A") // 已确认：立即解析，不再确认
		res := latest(t, r)
		wantKind(t, res, "A", KindFound)
		if res.Source != SourceCache || len(res.Lines) != 2 || res.Retry {
			t.Fatalf("结果 %+v，want 相对路径拼上确认所得的歌词目录读到的缓存歌词", res)
		}
		wantConfirms(t, f, 0)
		wantParses(t, f, "A@1h0m0s")
		if !slices.Equal(f.dbPaths, []string{wantDBPath}) {
			t.Fatalf("库路径 %q，want %q", f.dbPaths, wantDBPath)
		}
	})
}

func TestReaderStartNotReadyRetries(t *testing.T) {
	f := &fakeDeps{confirmFn: notReadyUntil(1)}
	runReader(t, f, func(t *testing.T, r *Reader) {
		noResult(t, r) // 没有 hash：未就绪不发未确认
		time.Sleep(10*time.Second - time.Millisecond)
		wantConfirms(t, f, 0)
		time.Sleep(time.Millisecond)
		wantConfirms(t, f, 0, 10*time.Second)
		noResult(t, r) // 通过了，但没有 hash：不解析、不发结果
		wantParses(t, f)

		r.SetHash("A")
		wantKind(t, latest(t, r), "A", KindFound)
		wantConfirms(t, f, 0, 10*time.Second)
	})
}

func TestReaderNotConfirmedThenConfirmed(t *testing.T) {
	f := &fakeDeps{confirmFn: notReadyUntil(3)} // 启动、A、B 三次未就绪，第 4 次通过
	runReader(t, f, func(t *testing.T, r *Reader) {
		r.SetHash("A")
		wantKind(t, latest(t, r), "A", KindNotConfirmed)
		r.SetHash("B") // 每次写入 hash 都立即再试一次
		wantKind(t, latest(t, r), "B", KindNotConfirmed)
		wantParses(t, f)

		time.Sleep(10*time.Second - time.Millisecond)
		noResult(t, r)
		time.Sleep(time.Millisecond)
		wantKind(t, latest(t, r), "B", KindFound) // 通过即对当前 hash 解析
		wantConfirms(t, f, 0, 0, 0, 10*time.Second)
		wantParses(t, f, "B@10s")

		r.SetHash("C") // 已确认：不再确认，直接解析
		wantKind(t, latest(t, r), "C", KindFound)
		wantConfirms(t, f, 0, 0, 0, 10*time.Second)
	})
}

func TestReaderGivesUpAfter30(t *testing.T) {
	notReady := func(int) (watchdog.Confirmed, error) { return watchdog.Confirmed{}, errNotReadyStub }
	t.Run("有 hash", func(t *testing.T) {
		f := &fakeDeps{confirmFn: notReady}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A") // 启动一次、写入 A 一次，之后每 10 秒一次
			wantKind(t, latest(t, r), "A", KindNotConfirmed)

			time.Sleep(280*time.Second - time.Millisecond)
			noResult(t, r) // 定时重试未就绪不发结果
			if n := len(confirmCalls(f)); n != 29 {
				t.Fatalf("279.999 s 时确认 %d 次，want 29", n)
			}
			time.Sleep(time.Millisecond)
			wantKind(t, latest(t, r), "A", KindDisabled)
			want := []time.Duration{0}
			for i := range 29 {
				want = append(want, time.Duration(i)*10*time.Second)
			}
			wantConfirms(t, f, want...)

			time.Sleep(time.Hour)
			noResult(t, r)
			r.SetHash("B")
			wantKind(t, latest(t, r), "B", KindDisabled)
			if n := len(confirmCalls(f)); n != 30 {
				t.Fatalf("停用后确认 %d 次，want 仍为 30", n)
			}
			wantParses(t, f)
		})
	})
	t.Run("没有 hash 同样累计", func(t *testing.T) {
		f := &fakeDeps{confirmFn: notReady}
		runReader(t, f, func(t *testing.T, r *Reader) {
			time.Sleep(290*time.Second - time.Millisecond)
			noResult(t, r)
			time.Sleep(time.Millisecond)
			wantKind(t, latest(t, r), "", KindDisabled) // Hash 为空，会话按 hash 不符丢弃
			var want []time.Duration
			for i := range 30 {
				want = append(want, time.Duration(i)*10*time.Second)
			}
			wantConfirms(t, f, want...)
			r.SetHash("A")
			wantKind(t, latest(t, r), "A", KindDisabled)
			if n := len(confirmCalls(f)); n != 30 {
				t.Fatalf("停用后确认 %d 次，want 仍为 30", n)
			}
		})
	})
}

func TestReaderUnsupportedStopsImmediately(t *testing.T) {
	unsupported := func(int) (watchdog.Confirmed, error) { return watchdog.Confirmed{}, errUnsupportedStub }
	t.Run("启动即不支持", func(t *testing.T) {
		f := &fakeDeps{confirmFn: unsupported}
		runReader(t, f, func(t *testing.T, r *Reader) {
			wantKind(t, latest(t, r), "", KindDisabled)
			time.Sleep(time.Hour)
			noResult(t, r)
			r.SetHash("A")
			wantKind(t, latest(t, r), "A", KindDisabled)
			wantConfirms(t, f, 0)
			wantParses(t, f)
		})
	})
	t.Run("写入 hash 时不支持", func(t *testing.T) {
		f := &fakeDeps{confirmFn: func(n int) (watchdog.Confirmed, error) {
			if n == 1 {
				return watchdog.Confirmed{}, errNotReadyStub
			}
			return unsupported(n)
		}}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			wantKind(t, latest(t, r), "A", KindDisabled)
			time.Sleep(time.Hour)
			noResult(t, r)
			r.SetHash("B")
			wantKind(t, latest(t, r), "B", KindDisabled)
			wantConfirms(t, f, 0, 0)
			wantParses(t, f)
		})
	})
}

// 反例：确认返回两种 sentinel 以外的错误时按未就绪处理（重试、计数），不停用。
func TestReaderUnknownConfirmErrorRetries(t *testing.T) {
	f := &fakeDeps{confirmFn: func(n int) (watchdog.Confirmed, error) {
		if n <= 2 { // 启动与写入 A
			return watchdog.Confirmed{}, errTransientStub
		}
		return okConfirmed, nil
	}}
	runReader(t, f, func(t *testing.T, r *Reader) {
		r.SetHash("A")
		wantKind(t, latest(t, r), "A", KindNotConfirmed)
		time.Sleep(10 * time.Second)
		wantKind(t, latest(t, r), "A", KindFound)
	})
}
