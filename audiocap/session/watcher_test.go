package session

import (
	"testing"
	"testing/synctest"
	"time"
)

// 本文件只测 Watcher 的编排：槽位集合的登记与摘除、空集合不探测、停止，以及在途探测只写它拷出的槽位。
// 时间与阻塞用 testing/synctest 的虚拟时钟：synctest.Test 在函数返回后等泡内全部 goroutine 退出，Run 若停
// 不下来即报死锁。

// gatedEnum 每次 enumerate 先阻塞在 gate 上，放行后依次返回 results 中的下一组（用完后重复最后一组）。
type gatedEnum struct {
	gate    chan struct{}
	results [][]endpoint
	calls   int
}

func (g *gatedEnum) enumerate() ([]endpoint, error) {
	<-g.gate
	i := min(g.calls, len(g.results)-1)
	g.calls++
	return g.results[i], nil
}

// 槽位按 capture 分配：对 PID A 的探测阻塞在枚举里时，同一播放器 stop（摘除 A 的槽位）、再 capture PID B
// （登记新槽位）；放行后这一拍只写 A 的槽位（A 有 Active 会话，发布 (1,1)），B 在自己的首次有效探测之前保持
// (0,0)；B 自己的那一拍里只有 B 的 Active 会话，B 为 (1,1)。若 B 被算进 A 的那一拍，会得到相反的 (1,0)。
//
// 变异自证（实测）：probe 在枚举之后改为重新取 pending()（不用开拍时拷出的槽位）→ B 在首次探测前被 A 的那一拍
// 写成 (1,0)，红。
func TestInFlightProbeOnlyWritesItsSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pidA, pidB = 100, 200
		enum := &gatedEnum{gate: make(chan struct{}), results: [][]endpoint{
			{{sessions: []sessionInfo{active(pidA)}}},
			{{sessions: []sessionInfo{active(pidB)}}},
		}}
		w := NewWatcher(snapshotOf(), func(string) {})
		a := w.Add(pidA)
		done := make(chan struct{})
		go func() {
			w.Run(enum)
			close(done)
		}()
		synctest.Wait() // Run 已拷出 [a]，阻塞在第一次枚举里

		w.Remove(a) // stop
		b := w.Add(pidB)
		enum.gate <- struct{}{}
		synctest.Wait() // 第一拍写完，Run 在拍间等待
		if f := b.Flags(time.Now()); f != 0 {
			t.Fatalf("B 在首次探测前: flags = %#x, 期望 0", f)
		}
		if f := a.Flags(time.Now()); f != both {
			t.Fatalf("A 的探测应写回 A 的槽位: flags = %#x, 期望 %#x", f, both)
		}

		time.Sleep(probeInterval)
		synctest.Wait() // 第二拍已拷出 [b]，阻塞在枚举里
		if f := b.Flags(time.Now()); f != 0 {
			t.Fatalf("B 的首次探测完成前: flags = %#x, 期望 0", f)
		}
		enum.gate <- struct{}{}
		synctest.Wait()
		if f := b.Flags(time.Now()); f != both {
			t.Fatalf("B 的首次探测之后: flags = %#x, 期望 %#x", f, both)
		}
		w.Stop()
		<-done
	})
}

// 集合为空时不探测、阻塞等待；登记即唤醒并立即探测；摘空后不再探测；Stop 后 Run 返回，泡内无残留 goroutine。
//
// 变异自证（实测）：Run 在空集合时也调 probe → 首段的枚举次数不为 0，红；空集合分支只等 wake、不等 stop →
// Stop 后 Run 不返回，红（synctest 报死锁）。
func TestWatcherIdleWhenEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		enum := &fakeEnum{}
		w := NewWatcher(snapshotOf(), func(string) {})
		done := make(chan struct{})
		go func() {
			w.Run(enum)
			close(done)
		}()
		time.Sleep(10 * probeInterval)
		synctest.Wait()
		if n := enum.calls.Load(); n != 0 {
			t.Fatalf("空集合时枚举了 %d 次, 期望 0", n)
		}

		s := w.Add(target)
		synctest.Wait()
		if n := enum.calls.Load(); n != 1 {
			t.Fatalf("登记后枚举 %d 次, 期望立即 1 次", n)
		}
		w.Remove(s)
		time.Sleep(10 * probeInterval)
		synctest.Wait()
		if n := enum.calls.Load(); n != 1 {
			t.Fatalf("摘空后又枚举到 %d 次, 期望仍为 1", n)
		}

		w.Stop()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("集合为空时 Stop 后 Run 未返回")
		}
	})
}

// 拍间等待时 Stop 立即返回，不等满间隔。
//
// 变异自证（实测）：拍间等待只等计时器、不等 stop → Stop 后 Run 未返回，红。
func TestWatcherStopBetweenTicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := NewWatcher(snapshotOf(), func(string) {})
		w.Add(target)
		done := make(chan struct{})
		go func() {
			w.Run(&fakeEnum{})
			close(done)
		}()
		synctest.Wait() // 已探测一拍，在拍间等待
		w.Stop()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("拍间 Stop 后 Run 未返回")
		}
		w.Stop() // 可重复调用
	})
}
