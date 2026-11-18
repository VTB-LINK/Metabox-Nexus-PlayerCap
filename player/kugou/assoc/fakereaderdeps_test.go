package assoc

// 本文件是读取者各用例共用的桩与辅助函数，不含用例。依赖全部为桩，不碰本机酷狗的进程、注册表与文件；
// 桩记下各次调用相对读取者启动的时长（testing/synctest 泡内的假时钟）与参数。

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

// okConfirmed 是桩确认通过时的结果；wantDBPath 是它对应的库路径。
var okConfirmed = watchdog.Confirmed{PID: 1, Version: "20.1.22.27795", DataDir: `C:\KuGou8`, LyricDir: `C:\KuGou\Lyric`}

const wantDBPath = `C:\KuGou8\KGMusicV3.db`

// cacheCand 是相对歌词目录的缓存候选，拼上 okConfirmed.LyricDir 即 cachePath；assocCand 是绝对路径的关联候选。
var (
	cacheCand = candidate{Path: "a.lrc", Source: SourceCache, UpdateTime: 1}
	assocCand = candidate{Path: `D:\assoc.lrc`, Source: SourceAssociation, UpdateTime: 2}
)

const cachePath = `C:\KuGou\Lyric\a.lrc`

var (
	errNotReadyStub    = fmt.Errorf("%w: KuGou.exe main process not found", watchdog.ErrConfirmNotReady)
	errUnsupportedStub = fmt.Errorf("%w: KuGou version 10.0 is not supported", watchdog.ErrConfirmUnsupported)
	errTransientStub   = fmt.Errorf("%w: journal file exists", errTransient)
)

// fakeDeps 是读取者的桩依赖。各 Fn 为 nil 时：确认恒通过（okConfirmed）；候选恒为 [cacheCand]；库文件的
// （大小, 修改时间）恒定；只有 cachePath 读得到（内容 testLRC），其余路径不存在。colFn 非空时取代 candsFn，
// 给出完整的 collect 结局；candsFn 只给候选，其余为零值（没有关联键、没有 P）。
type fakeDeps struct {
	confirmFn func(n int) (watchdog.Confirmed, error)             // 第 n 次确认（从 1 起）
	candsFn   func(n int, hash string) ([]candidate, error)       // 第 n 次取候选（从 1 起）
	colFn     func(n int, hash string) (collected, error)         // 同上，给出完整结局
	statFn    func(at time.Duration) (int64, time.Time, error)    // at 为调用时刻
	readFn    func(at time.Duration, path string) ([]byte, error) // at 为调用时刻

	mu       sync.Mutex
	t0       time.Time
	confirms []time.Duration
	parses   []string // 各次取候选的「hash@时刻」
	dbPaths  []string // 各次取候选的库路径
}

func (f *fakeDeps) since() time.Duration { return time.Since(f.t0) }

func (f *fakeDeps) deps() deps {
	return deps{
		confirm: func() (watchdog.Confirmed, error) {
			f.mu.Lock()
			f.confirms = append(f.confirms, f.since())
			n, fn := len(f.confirms), f.confirmFn
			f.mu.Unlock()
			if fn == nil {
				return okConfirmed, nil
			}
			return fn(n)
		},
		candidates: func(_ context.Context, dbPath, hash string) (collected, error) {
			f.mu.Lock()
			f.parses = append(f.parses, fmt.Sprintf("%s@%v", hash, f.since()))
			f.dbPaths = append(f.dbPaths, dbPath)
			n, fn, colFn := len(f.parses), f.candsFn, f.colFn
			f.mu.Unlock()
			switch {
			case colFn != nil:
				return colFn(n, hash)
			case fn == nil:
				return collected{cands: []candidate{cacheCand}}, nil
			}
			cands, err := fn(n, hash)
			return collected{cands: cands}, err
		},
		readFile: func(path string) ([]byte, error) {
			if f.readFn != nil {
				return f.readFn(f.since(), path)
			}
			if path == cachePath {
				return []byte(testLRC), nil
			}
			return nil, fs.ErrNotExist
		},
		stat: func(string) (int64, time.Time, error) {
			if f.statFn != nil {
				return f.statFn(f.since())
			}
			return 1 << 20, time.Unix(1000, 0), nil
		},
	}
}

// gate 让桩阻塞到用例放行。须在泡内 arm：泡外建的通道上的阻塞不算 durably blocked，synctest.Wait 等不到它。
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func (g *gate) arm()     { g.ch = make(chan struct{}) }
func (g *gate) wait()    { <-g.ch }
func (g *gate) release() { g.once.Do(func() { close(g.ch) }) }

// runReader 在 synctest 的泡里以 f 的桩启动一个读取者，等它做完启动时的那次确认，再执行 body；body 返回后取消，
// 断言读取者 goroutine 已退出、计数归零。
func runReader(t *testing.T, f *fakeDeps, body func(t *testing.T, r *Reader)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.t0 = time.Now()
		r := start(ctx, f.deps())
		if r == nil {
			t.Fatal("start 返回 nil，want 一个读取者")
		}
		synctest.Wait()
		body(t, r)
		cancel()
		synctest.Wait()
		if n := readers.Load(); n != 0 {
			t.Fatalf("取消之后存活的读取者 %d 个，want 0", n)
		}
	})
}

// latest 等读取者停下，取走结果通道里的那一条；通道为空即失败。
func latest(t *testing.T, r *Reader) Result {
	t.Helper()
	synctest.Wait()
	select {
	case res := <-r.Results():
		return res
	default:
		t.Fatal("结果通道为空，want 一条结果")
		return Result{}
	}
}

// noResult 等读取者停下，断言结果通道为空。
func noResult(t *testing.T, r *Reader) {
	t.Helper()
	synctest.Wait()
	select {
	case res := <-r.Results():
		t.Fatalf("意外的结果 %+v，want 通道为空", res)
	default:
	}
}

// wantKind 断言结果的 hash 与种类；种类为 KindFound 时另断言来源、Retry 与行数（testLRC 两行）。
func wantKind(t *testing.T, got Result, hash string, kind Kind) {
	t.Helper()
	if got.Hash != hash || got.Kind != kind {
		t.Fatalf("结果 %+v，want Hash %q、Kind %d", got, hash, kind)
	}
	if kind != KindFound && (got.Lines != nil || got.Source != 0 || got.Retry) {
		t.Fatalf("结果 %+v，want 不带行、来源与 Retry", got)
	}
}

// wantParses 等读取者停下，断言取候选的调用序列（「hash@时刻」）。
func wantParses(t *testing.T, f *fakeDeps, want ...string) {
	t.Helper()
	synctest.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.parses, want) {
		t.Fatalf("取候选\n got  %q\n want %q", f.parses, want)
	}
}

// confirmCalls 等读取者停下，返回各次确认的时刻。
func confirmCalls(f *fakeDeps) []time.Duration {
	synctest.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.confirms)
}

// report 是一次 reportOnce 调用。
type report struct {
	key   string
	tags  map[string]string
	extra map[string]any
}

// stubReports 把 reportOnce 换成记录调用的桩，用例结束时还原。须在启动读取者之前调用。
func stubReports(t *testing.T) func() []report {
	t.Helper()
	var mu sync.Mutex
	var got []report
	orig := reportOnce
	reportOnce = func(key, _ string, tags map[string]string, extra map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, report{key, tags, extra})
	}
	t.Cleanup(func() { reportOnce = orig })
	return func() []report {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}
