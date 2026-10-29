package player

// 本文件只测 BaseEmitter.TryEmit：与 Emit 相同，但如实返回事件是否入队，通道满时不阻塞。
// kuwo 的 emitPos 据它判断前端模型能否落锚（issue #131）。

import (
	"testing"
	"time"
)

// 变异自证（均实测）：TryEmit 恒返回 true 即红；改成阻塞发送（不走 select default）即红（超时）。
func TestTryEmitReportsEnqueue(t *testing.T) {
	b := NewBaseEmitter("test")
	for i := 0; i < 128; i++ {
		b.Emit(EventLyricIdle, nil)
	}
	if n := len(b.EventCh); n != 128 {
		t.Fatalf("填充后通道长度 %d，期望 128", n)
	}

	done := make(chan bool, 1)
	go func() { done <- b.TryEmit(EventPlaybackResume, &PlaybackTimeInfo{Position: 1}) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("通道满时 TryEmit 返回 true")
		}
	case <-time.After(time.Second):
		t.Fatal("通道满时 TryEmit 阻塞")
	}
	if n := len(b.EventCh); n != 128 {
		t.Fatalf("丢弃之后通道长度 %d，期望仍为 128", n)
	}

	<-b.EventCh
	info := &PlaybackTimeInfo{Position: 2}
	if !b.TryEmit(EventPlaybackResume, info) {
		t.Fatal("通道有空位时 TryEmit 返回 false")
	}
	evts := drain(&b)
	if len(evts) != 128 {
		t.Fatalf("通道里有 %d 条，期望 128", len(evts))
	}
	last := evts[len(evts)-1]
	if last.Type != EventPlaybackResume || last.Data != info || last.PlayerName != "test" {
		t.Fatalf("入队的事件不对：%+v", last)
	}
}
