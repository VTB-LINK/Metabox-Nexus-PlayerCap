package assoc

// 本文件只测读取者 goroutine 的生命周期（设计 §6.7「生命周期」）：同时至多一个；上一个阻塞在文件操作里时
// 新会话拿不到读取者，它在操作返回后退出、不再发结果、不再读后续歌词候选，之后才能再启动；各状态下取消即退出、
// 计数归零，退出后 SetHash 不阻塞。

import (
	"context"
	"io/fs"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

func TestReaderSingleInstance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g gate
		g.arm()
		defer g.release()
		blocked := &fakeDeps{candsFn: func(int, string) ([]candidate, error) {
			g.wait() // 阻塞的文件操作：不看 ctx，直到放行
			return []candidate{cacheCand}, nil
		}}
		ctx1, cancel1 := context.WithCancel(context.Background())
		defer cancel1()
		r1 := start(ctx1, blocked.deps())
		if r1 == nil {
			t.Fatal("第一个 start 返回 nil")
		}
		r1.SetHash("A")
		synctest.Wait()
		cancel1()
		synctest.Wait()

		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		if r := Start(ctx2); r != nil {
			t.Fatal("上一个读取者未退出时 Start 返回非 nil")
		}
		if r := start(ctx2, (&fakeDeps{}).deps()); r != nil {
			t.Fatal("上一个读取者未退出时 start 返回非 nil")
		}
		if n := readers.Load(); n != 1 {
			t.Fatalf("存活的读取者 %d 个，want 1", n)
		}

		g.release()
		synctest.Wait()
		if n := readers.Load(); n != 0 {
			t.Fatalf("放行之后存活的读取者 %d 个，want 0", n)
		}
		noResult(t, r1) // 已取消：解析返回后不发结果

		r3 := start(ctx2, (&fakeDeps{}).deps())
		if r3 == nil {
			t.Fatal("上一个读取者退出之后 start 返回 nil")
		}
		r3.SetHash("B")
		wantKind(t, latest(t, r3), "B", KindFound)
		cancel2()
		synctest.Wait()
		if n := readers.Load(); n != 0 {
			t.Fatalf("取消之后存活的读取者 %d 个，want 0", n)
		}
	})
}

// 第一个候选的读取阻塞期间会话取消：放行后不再读第二个候选、不发结果，goroutine 退出、计数归零。
// 第一份读取放行后无论读不到还是读到了内容，都一样。
func TestReaderCancelStopsReadingCandidates(t *testing.T) {
	for name, first := range map[string]error{"读不到": fs.ErrNotExist, "读到了": nil} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var g gate
				g.arm()
				defer g.release()
				var laterReads atomic.Int32
				f := &fakeDeps{
					candsFn: func(int, string) ([]candidate, error) { return []candidate{assocCand, cacheCand}, nil },
					readFn: func(_ time.Duration, path string) ([]byte, error) {
						if path != assocCand.Path {
							laterReads.Add(1)
							return []byte(testLRC), nil
						}
						g.wait() // 第一个候选在失联的盘上
						if first != nil {
							return nil, first
						}
						return []byte(testLRC), nil
					},
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.t0 = time.Now()
				r := start(ctx, f.deps())
				if r == nil {
					t.Fatal("start 返回 nil")
				}
				r.SetHash("A")
				synctest.Wait() // 阻塞在第一个候选的读取里
				cancel()
				synctest.Wait()
				if n := readers.Load(); n != 1 {
					t.Fatalf("读取阻塞中存活的读取者 %d 个，want 1", n)
				}

				g.release()
				synctest.Wait()
				if n := laterReads.Load(); n != 0 {
					t.Fatalf("取消之后又读了 %d 个后续候选，want 0", n)
				}
				noResult(t, r)
				if n := readers.Load(); n != 0 {
					t.Fatalf("放行之后存活的读取者 %d 个，want 0", n)
				}
			})
		})
	}
}

func TestReaderExitsOnCancel(t *testing.T) {
	notReady := func(int) (watchdog.Confirmed, error) { return watchdog.Confirmed{}, errNotReadyStub }
	unsupported := func(int) (watchdog.Confirmed, error) { return watchdog.Confirmed{}, errUnsupportedStub }
	transient := func(int, string) ([]candidate, error) { return nil, errTransientStub }
	cases := []struct {
		name string
		f    *fakeDeps
		hash string
	}{
		{"没有 hash", &fakeDeps{}, ""},
		{"未确认、等重试", &fakeDeps{confirmFn: notReady}, "A"},
		{"已确认、等下一拍", &fakeDeps{}, "A"},
		{"暂时失败、等退避", &fakeDeps{candsFn: transient}, "A"},
		{"已停用", &fakeDeps{confirmFn: unsupported}, "A"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c.f.t0 = time.Now()
				r := start(ctx, c.f.deps())
				if r == nil {
					t.Fatal("start 返回 nil")
				}
				r.SetHash(c.hash)
				time.Sleep(3 * time.Second)
				if c.hash != "" {
					latest(t, r) // 取走它的结果，免得与取消之后的检查混淆
				}
				cancel()
				synctest.Wait()
				if n := readers.Load(); n != 0 {
					t.Fatalf("取消之后存活的读取者 %d 个，want 0", n)
				}
				r.SetHash("B") // 不阻塞
				time.Sleep(time.Minute)
				noResult(t, r)
			})
		})
	}
}
