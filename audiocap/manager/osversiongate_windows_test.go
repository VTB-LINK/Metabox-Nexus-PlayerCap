//go:build windows

package manager

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateSink 记录 Run 是否进入 reconcile（reconcile 首先查 AnySubscribers）与 idleKeepalive 推出的空闲帧数
// （root 有订阅、无活跃播放器时每拍推一帧 player="" 的空闲帧）。AnySubscribers 报告无订阅，reconcile 随即
// 返回，不起 helper。
type gateSink struct {
	once       sync.Once
	reconciled chan struct{} // AnySubscribers 首次被调用时关闭
	idleFrames atomic.Int32
}

func (s *gateSink) BroadcastAudioFrame(player string, _ []byte) {
	if player == "" {
		s.idleFrames.Add(1)
	}
}
func (s *gateSink) HasSubscribers(string) bool { return false }
func (s *gateSink) HasRootSubscriber() bool    { return true }
func (s *gateSink) ActivePlayer() string       { return "" }
func (s *gateSink) AnySubscribers() bool {
	s.once.Do(func() { close(s.reconciled) })
	return false
}

// fakeOSVersion 把 osVersion 固定为给定版本，Cleanup 时还原。调用 Run 的用例都要先调它，否则结果随宿主
// 系统版本而变。在起 Run 之前调用：Cleanup 后进先出，还原排在其后注册的「等 Run 返回」之后。
func fakeOSVersion(t *testing.T, major, minor, build uint32) {
	t.Helper()
	orig := osVersion
	osVersion = func() (uint32, uint32, uint32) { return major, minor, build }
	t.Cleanup(func() { osVersion = orig })
}

// runWithOSVersion 注入系统版本并起 Run。Cleanup 中 Stop，断言 Run 随之返回。
func runWithOSVersion(t *testing.T, major, minor, build uint32) *gateSink {
	t.Helper()
	fakeOSVersion(t, major, minor, build)
	sink := &gateSink{reconciled: make(chan struct{})}
	m := New(sink, nil, nil, 25, os.Args[0])
	done := make(chan struct{})
	t.Cleanup(func() {
		m.Stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Stop 后 Run 10s 内未返回")
		}
	})
	m.Wake() // Run 若进入管理循环，第一轮 reconcile 立即执行
	go func() {
		defer close(done)
		m.Run()
	}()
	return sink
}

// TestRunSkipsCaptureBelowMinBuild 断言系统低于 19041 时 Run 不进入 reconcile（不会起 helper），idleKeepalive
// 照常推空闲帧。先等到首个空闲帧：idleKeepalive 与管理循环同在孤儿清扫之后启动，而管理循环有 Wake 在先、
// 首拍即 reconcile，空闲帧要等 100ms 一拍，故缺了版本判定时，首个空闲帧到达之前 reconcile 已经发生。
func TestRunSkipsCaptureBelowMinBuild(t *testing.T) {
	sink := runWithOSVersion(t, 10, 0, 18363)

	deadline := time.Now().Add(10 * time.Second)
	for sink.idleFrames.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("系统低于 19041 时 10s 内没有空闲帧，idleKeepalive 未运行")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-sink.reconciled:
		t.Fatal("系统低于 19041 时 Run 不应进入 reconcile")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestRunReconcilesAtMinBuild 是上例的对照：系统恰为 19041 时 Run 进入 reconcile。
func TestRunReconcilesAtMinBuild(t *testing.T) {
	sink := runWithOSVersion(t, 10, 0, 19041)

	select {
	case <-sink.reconciled:
	case <-time.After(10 * time.Second):
		t.Fatal("系统为 19041 时 Run 10s 内未进入 reconcile")
	}
}
