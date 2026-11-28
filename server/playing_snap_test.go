package server

import (
	"bytes"
	"log"
	"sync"
	"testing"
	"time"
)

type blockingLogWriter struct {
	match       []byte
	enteredOnce sync.Once
	entered     chan struct{}
	releaseOnce sync.Once
	release     chan struct{}
}

func newBlockingLogWriter(match string) *blockingLogWriter {
	return &blockingLogWriter{
		match:   []byte(match),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingLogWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, w.match) {
		w.enteredOnce.Do(func() {
			close(w.entered)
		})
		<-w.release
	}
	return len(p), nil
}

func (w *blockingLogWriter) unblock() {
	w.releaseOnce.Do(func() {
		close(w.release)
	})
}

// TestIsPlayerPlayingLockFreeDuringSweepIdleHideLogBlock 断言 sweepIdleHide 持 s.mu.Lock()
// 且在 serverLog.Info 写日志处阻塞期间，IsPlayerPlaying 仍能无锁在短超时内立刻返回准确播放态，
// 且状态变更后快照与 GetPlayerStatus 保持同步。
func TestIsPlayerPlayingLockFreeDuringSweepIdleHideLogBlock(t *testing.T) {
	srv := NewServer([]string{"wesing", "kugou"})

	if srv.IsPlayerPlaying("wesing") || srv.IsPlayerPlaying("kugou") || srv.IsPlayerPlaying("unknown") {
		t.Fatal("初始未上报状态前，所有播放器的 IsPlayerPlaying 均应为 false")
	}

	srv.UpdatePlayerState(statusEvt("wesing", "playing"))
	srv.UpdatePlayerState(statusEvt("kugou", "paused"))
	if !srv.IsPlayerPlaying("wesing") {
		t.Fatal("wesing 置为 playing 后 IsPlayerPlaying 应为 true")
	}
	if srv.IsPlayerPlaying("kugou") {
		t.Fatal("kugou 置为 paused 后 IsPlayerPlaying 应为 false")
	}

	srv.SetIdleHideResolver(func(string) int { return 5 })

	origOut := log.Default().Writer()
	bw := newBlockingLogWriter("per-player [")
	log.SetOutput(bw)

	sweepDone := make(chan struct{})
	t.Cleanup(func() {
		bw.unblock()
		<-sweepDone
		log.SetOutput(origOut)
	})

	go func() {
		defer close(sweepDone)
		srv.sweepIdleHide(time.Now().Add(10 * time.Second))
	}()

	select {
	case <-bw.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("sweepIdleHide 未在预期时间内触发 serverLog.Info 阻塞点")
	}

	type queryRes struct {
		wesing bool
		kugou  bool
	}
	resCh := make(chan queryRes, 1)
	go func() {
		resCh <- queryRes{
			wesing: srv.IsPlayerPlaying("wesing"),
			kugou:  srv.IsPlayerPlaying("kugou"),
		}
	}()

	select {
	case got := <-resCh:
		if !got.wesing || got.kugou {
			t.Fatalf("s.mu 被持锁阻塞期间 IsPlayerPlaying 返回错误：wesing=%v kugou=%v，期望 true/false", got.wesing, got.kugou)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("sweepIdleHide 持 s.mu.Lock() 写日志阻塞期间，IsPlayerPlaying 在 200ms 内未返回（疑似仍依赖 s.mu）")
	}

	bw.unblock()
	<-sweepDone
	log.SetOutput(origOut)

	// 验证状态变更后无锁快照同步更新，且原有 GetPlayerStatus 行为不变。
	srv.UpdatePlayerState(statusEvt("wesing", "paused"))
	if srv.IsPlayerPlaying("wesing") {
		t.Fatal("wesing 转为 paused 后 IsPlayerPlaying 应为 false")
	}
	if got := srv.GetPlayerStatus("wesing"); got != "paused" {
		t.Fatalf("GetPlayerStatus(wesing) = %q，期望 paused", got)
	}

	srv.UpdatePlayerState(statusEvt("kugou", "playing"))
	if !srv.IsPlayerPlaying("kugou") {
		t.Fatal("kugou 转为 playing 后 IsPlayerPlaying 应为 true")
	}
	if got := srv.GetPlayerStatus("kugou"); got != "playing" {
		t.Fatalf("GetPlayerStatus(kugou) = %q，期望 playing", got)
	}

	srv.UpdatePlayerState(statusEvt("kugou", "waiting_song"))
	if srv.IsPlayerPlaying("kugou") {
		t.Fatal("kugou 转为 waiting_song 后 IsPlayerPlaying 应为 false")
	}
	if got := srv.GetPlayerStatus("kugou"); got != "waiting_song" {
		t.Fatalf("GetPlayerStatus(kugou) = %q，期望 waiting_song", got)
	}

	// 动态新播放器同样应维护快照。
	srv.UpdatePlayerState(statusEvt("spotify", "playing"))
	if !srv.IsPlayerPlaying("spotify") {
		t.Fatal("动态新播放器 spotify 置为 playing 后 IsPlayerPlaying 应为 true")
	}
	srv.UpdatePlayerState(statusEvt("spotify", "paused"))
	if srv.IsPlayerPlaying("spotify") {
		t.Fatal("动态新播放器 spotify 置为 paused 后 IsPlayerPlaying 应为 false")
	}
	if got := srv.GetPlayerStatus("unknown"); got != "offline" {
		t.Fatalf("GetPlayerStatus(unknown) = %q，期望 offline", got)
	}
}
