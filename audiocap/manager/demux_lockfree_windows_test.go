//go:build windows

package manager

import (
	"bytes"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/rum"
	"Metabox-Nexus-PlayerCap/server"
)

type mgrBlockingLogWriter struct {
	match       []byte
	enteredOnce sync.Once
	entered     chan struct{}
	releaseOnce sync.Once
	release     chan struct{}
}

func newMgrBlockingLogWriter(match string) *mgrBlockingLogWriter {
	return &mgrBlockingLogWriter{
		match:   []byte(match),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *mgrBlockingLogWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, w.match) {
		w.enteredOnce.Do(func() {
			close(w.entered)
		})
		<-w.release
	}
	return len(p), nil
}

func (w *mgrBlockingLogWriter) unblock() {
	w.releaseOnce.Do(func() {
		close(w.release)
	})
}

func encodeTestFrames(playerName string, flags uint32, count int) []byte {
	var buf []byte
	for i := 0; i < count; i++ {
		buf = frame.Encode(buf, frame.Frame{
			Player: playerName,
			Epoch:  uint32(i + 1),
			Flags:  flags,
		})
	}
	return buf
}

// TestDemuxLockFreeDuringServerLogBlockAndTrackerMutexHold 验证音频 demux 转发热路径彻底脱离
// Server.mu 与 rumTracker.mu 阻塞，且首帧 ttff_ms 仍正确记录：
//  1. 真实 server.NewServer 的 sweepIdleHide 在持 Server.mu.Lock() 期间阻塞写日志，同时 manager
//     的 status 回调传入 srv.GetPlayerStatus（此时调用必阻塞），但注入 SetPlayingQuery(srv.IsPlayerPlaying)
//     后 demux 连续转发多帧完全不被阻塞；
//  2. 人为持住 m.rum.mu.Lock() 时，demux 连续转发多帧同样完全不被阻塞。
func TestDemuxLockFreeDuringServerLogBlockAndTrackerMutexHold(t *testing.T) {
	t.Run("ServerSweepIdleHideBlocksLog", func(t *testing.T) {
		col := setupManagerRUMCollector(t)

		srv := server.NewServer([]string{"wesing", "kugou"})
		srv.UpdatePlayerState(player.Event{
			PlayerName: "wesing",
			Type:       player.EventStatusUpdate,
			Data:       &player.StatusInfo{Status: "playing"},
		})
		srv.UpdatePlayerState(player.Event{
			PlayerName: "kugou",
			Type:       player.EventStatusUpdate,
			Data:       &player.StatusInfo{Status: "paused"},
		})
		srv.SetIdleHideResolver(func(string) int { return 1 })
		// 稍微留出余量，使 watchIdleHide 的首次 1s tick 即满足 >= 1s 并命中 sweepIdleHide。
		time.Sleep(25 * time.Millisecond)

		origOut := log.Default().Writer()
		bw := newMgrBlockingLogWriter("per-player [")
		log.SetOutput(bw)
		t.Cleanup(func() {
			bw.unblock()
			// 获取并释放 srv.mu，确保 sweepIdleHide 已退出临界区且后续 tick 空转不再打日志。
			srv.SetIdleHideResolver(nil)
			log.SetOutput(origOut)
		})

		readyCh := make(chan struct{})
		go func() {
			_ = srv.Start("127.0.0.1:0", readyCh)
		}()
		select {
		case <-readyCh:
		case <-time.After(3 * time.Second):
			t.Fatal("测试 server 启动超时")
		}

		select {
		case <-bw.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("watchIdleHide/sweepIdleHide 未在预期时间内触发 serverLog.Info 阻塞点")
		}

		// 确认此时真实 srv.GetPlayerStatus 的确会被 Server.mu 阻塞。
		statusDone := make(chan string, 1)
		go func() {
			statusDone <- srv.GetPlayerStatus("wesing")
		}()
		select {
		case st := <-statusDone:
			t.Fatalf("预期 sweepIdleHide 阻塞期间 srv.GetPlayerStatus 应阻塞，实际立即返回 %q", st)
		case <-time.After(40 * time.Millisecond):
		}

		const frameCount = 5
		sink := idleSink{ch: make(chan []byte, frameCount+2)}
		m := New(sink, srv.GetPlayerStatus, nil, 25, "")
		m.SetPlayingQuery(srv.IsPlayerPlaying)

		startAt := time.Now().Add(-150 * time.Millisecond)
		m.rum.onCaptureStart(1, 1, "wesing", startAt, true, true)

		hp := &helperProc{cmd: &exec.Cmd{}, exited: make(chan struct{}), gen: 1}
		stream := encodeTestFrames("wesing", frame.FlagSessionKnown|frame.FlagSessionActive, frameCount)
		go m.demux(hp, bytes.NewReader(stream))

		select {
		case <-hp.exited:
		case <-time.After(300 * time.Millisecond):
			t.Fatal("Server.mu 阻塞写日志期间，demux 在 300ms 内未完成转发（疑似触碰 Server.mu）")
		}

		if got := len(sink.ch); got != frameCount {
			t.Fatalf("sink 收到 %d 帧，期望 %d", got, frameCount)
		}

		// 解除 Server.mu 阻塞并等待上面的对照组 GetPlayerStatus 协程退出。
		bw.unblock()
		srv.SetIdleHideResolver(nil)
		select {
		case <-statusDone:
		case <-time.After(2 * time.Second):
			t.Fatal("解除日志阻塞后 srv.GetPlayerStatus 未返回")
		}

		m.rum.onCaptureStop("wesing", time.Now(), false)
		rum.Flush(2 * time.Second)

		var foundTTFF bool
		for _, ev := range col.snapshot() {
			if ev.Event == "audio_capture_session" && ev.Properties["event_kind"] == "session_end" && ev.Properties["player"] == "wesing" {
				ttff, _ := ev.Properties["ttff_ms"].(float64)
				if ttff < 100 {
					t.Fatalf("首帧 ttff_ms = %v，期望 >= 100", ttff)
				}
				foundTTFF = true
			}
		}
		if !foundTTFF {
			t.Fatalf("未收到 wesing 的 session_end 事件，实际：%#v", col.snapshot())
		}
	})

	t.Run("RumTrackerMutexHeld", func(t *testing.T) {
		col := setupManagerRUMCollector(t)

		const frameCount = 5
		sink := idleSink{ch: make(chan []byte, frameCount+2)}
		blockCh := make(chan struct{})
		t.Cleanup(func() { close(blockCh) })
		m := New(sink, func(string) string {
			<-blockCh
			return "playing"
		}, nil, 25, "")
		m.SetPlayingQuery(func(string) bool { return true })

		startAt := time.Now().Add(-120 * time.Millisecond)
		m.rum.onCaptureStart(1, 2, "wesing", startAt, true, true)

		// 人为持住 rumTracker 互斥锁，验证 demux 转发与首帧 TTFF 记录全程零锁等待。
		m.rum.mu.Lock()
		locked := true
		t.Cleanup(func() {
			if locked {
				m.rum.mu.Unlock()
			}
		})

		hp := &helperProc{cmd: &exec.Cmd{}, exited: make(chan struct{}), gen: 1}
		stream := encodeTestFrames("wesing", frame.FlagSessionKnown|frame.FlagSessionActive, frameCount)
		go m.demux(hp, bytes.NewReader(stream))

		select {
		case <-hp.exited:
		case <-time.After(300 * time.Millisecond):
			t.Fatal("m.rum.mu 被持锁期间，demux 在 300ms 内未完成转发（疑似在 onFrame 中获取 t.mu）")
		}

		if got := len(sink.ch); got != frameCount {
			t.Fatalf("sink 收到 %d 帧，期望 %d", got, frameCount)
		}

		m.rum.mu.Unlock()
		locked = false

		m.rum.onCaptureStop("wesing", time.Now(), false)
		rum.Flush(2 * time.Second)

		var foundTTFF bool
		for _, ev := range col.snapshot() {
			if ev.Event == "audio_capture_session" && ev.Properties["event_kind"] == "session_end" && ev.Properties["player"] == "wesing" {
				ttff, _ := ev.Properties["ttff_ms"].(float64)
				if ttff < 80 {
					t.Fatalf("首帧 ttff_ms = %v，期望 >= 80", ttff)
				}
				foundTTFF = true
			}
		}
		if !foundTTFF {
			t.Fatalf("未收到 wesing 的 session_end 事件，实际：%#v", col.snapshot())
		}
	})
}

// TestDemuxWithoutPlayingQueryUsesReconcileSnapshotAndNeverBlocks 验证未注入 SetPlayingQuery 时，
// demux 仅读取 reconcile 发布的不可变播放态快照，绝不调用可能阻塞的 statusFn，且状态无变化时
// reconcile 不重复分配快照。
func TestDemuxWithoutPlayingQueryUsesReconcileSnapshotAndNeverBlocks(t *testing.T) {
	t.Run("BlockingStatusFnNeverCalled", func(t *testing.T) {
		_ = setupManagerRUMCollector(t)

		const frameCount = 5
		sink := idleSink{ch: make(chan []byte, frameCount+2)}
		blockCh := make(chan struct{})
		t.Cleanup(func() { close(blockCh) })
		var statusCalls atomic.Int64

		m := New(sink, func(string) string {
			statusCalls.Add(1)
			<-blockCh
			return "playing"
		}, nil, 25, "")

		hp := &helperProc{cmd: &exec.Cmd{}, exited: make(chan struct{}), gen: 1}
		stream := encodeTestFrames("kugou", frame.FlagSessionKnown, frameCount)
		go m.demux(hp, bytes.NewReader(stream))

		select {
		case <-hp.exited:
		case <-time.After(300 * time.Millisecond):
			t.Fatal("未注入 SetPlayingQuery 且 statusFn 阻塞时，demux 在 300ms 内未完成转发")
		}

		if got := len(sink.ch); got != frameCount {
			t.Fatalf("sink 收到 %d 帧，期望 %d", got, frameCount)
		}
		if calls := statusCalls.Load(); calls != 0 {
			t.Fatalf("demux 调用了 m.status %d 次，期望 0", calls)
		}
	})

	t.Run("ReconcilePublishesImmutableSnapshot", func(t *testing.T) {
		_ = setupManagerRUMCollector(t)

		kugouStatus := "playing"
		m := New(stubSink{player: "kugou"}, func(p string) string {
			if p == "kugou" {
				return kugouStatus
			}
			return "paused"
		}, nil, 25, "")
		// 置 givenUp 阻止测试中实际拉起 helper，仍完整跑完 reconcile 播放态快照发布。
		m.givenUp = true

		m.reconcile()
		snap1 := m.reconcilePlaying.Load()
		if snap1 == nil || !(*snap1)["kugou"] || (*snap1)["wesing"] {
			t.Fatalf("首次 reconcile 后快照异常：%#v", snap1)
		}
		if !m.isPlayerPlayingNoLock("kugou") || m.isPlayerPlayingNoLock("wesing") {
			t.Fatal("isPlayerPlayingNoLock 未正确读取 reconcilePlaying 快照")
		}

		// 状态无变化时不应重新分配并发布新快照。
		m.reconcile()
		snap2 := m.reconcilePlaying.Load()
		if snap2 != snap1 {
			t.Fatal("播放态无变化时 reconcile 重新分配了 reconcilePlaying 快照")
		}

		// 状态变化后发布新快照，且旧快照保持不可变。
		kugouStatus = "paused"
		m.reconcile()
		snap3 := m.reconcilePlaying.Load()
		if snap3 == nil || snap3 == snap1 || (*snap3)["kugou"] {
			t.Fatalf("状态切为 paused 后快照未正确更新：%#v", snap3)
		}
		if !(*snap1)["kugou"] {
			t.Fatal("已发布的旧 reconcilePlaying 快照被原地修改")
		}

		// 再切回 playing 发布快照，然后将 m.status 替换为永不返回的阻塞函数，验证 demux 只读快照不阻塞。
		kugouStatus = "playing"
		m.reconcile()

		blockCh := make(chan struct{})
		t.Cleanup(func() { close(blockCh) })
		var statusCalls atomic.Int64
		m.status = func(string) string {
			statusCalls.Add(1)
			<-blockCh
			return "playing"
		}

		const frameCount = 4
		sink := idleSink{ch: make(chan []byte, frameCount+2)}
		m.sink = sink
		hp := &helperProc{cmd: &exec.Cmd{}, exited: make(chan struct{}), gen: 1}
		stream := encodeTestFrames("kugou", frame.FlagSessionKnown|frame.FlagSessionActive, frameCount)
		go m.demux(hp, bytes.NewReader(stream))

		select {
		case <-hp.exited:
		case <-time.After(300 * time.Millisecond):
			t.Fatal("使用 reconcilePlaying 快照期间 demux 被阻塞")
		}
		if got := len(sink.ch); got != frameCount {
			t.Fatalf("sink 收到 %d 帧，期望 %d", got, frameCount)
		}
		if calls := statusCalls.Load(); calls != 0 {
			t.Fatalf("demux 调用了阻塞 statusFn %d 次，期望 0", calls)
		}
		if !m.isPlayerPlayingNoLock("kugou") {
			t.Fatal("kugou 快照应保持 playing=true")
		}

		// 验证 SetPlayingQuery 优先于快照，且传 nil 时清除并回退到快照。
		m.SetPlayingQuery(func(string) bool { return false })
		if m.isPlayerPlayingNoLock("kugou") {
			t.Fatal("注入 playingFast 后应优先使用 playingFast")
		}
		m.SetPlayingQuery(nil)
		if !m.isPlayerPlayingNoLock("kugou") {
			t.Fatal("SetPlayingQuery(nil) 后应回退读取 reconcilePlaying 快照")
		}
	})
}
