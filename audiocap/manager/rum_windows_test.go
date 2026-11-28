//go:build windows

package manager

import (
	"io"
	"math"
	"os"
	"os/exec"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/rum"
)

// TestTeardownHelperWaitsForEOFSummaryBeforeKill 端到端覆盖 R1：
// 模拟最后订阅断开走 Manager.teardownHelper()，helper 收到 stdin EOF 后在 150ms 宽限内写出 [mbx-rum] capture_summary 并退出；
// 验证 teardownHelper 不会抢在摘要之前强杀，最终 session_end 携带完整非零指标且 stats_incomplete=false。
func TestTeardownHelperWaitsForEOFSummaryBeforeKill(t *testing.T) {
	col := setupManagerRUMCollector(t)

	stdinR, stdinW := io.Pipe()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		stdinR.Close()
		stdinW.Close()
		stderrR.Close()
		stderrW.Close()
	})

	m := &Manager{
		dispatched:   map[string]uint32{"wesing": 1001},
		dispatchedAt: map[string]time.Time{"wesing": time.Now().Add(-5 * time.Second)},
		lastFrame:    map[string]time.Time{},
		lastForward:  map[string]time.Time{},
		stderrOut:    io.Discard,
		helperGen:    1,
	}
	hp := &helperProc{
		stdin:      stdinW,
		exited:     make(chan struct{}),
		start:      time.Now().Add(-5 * time.Second),
		stderrTail: &tailBuffer{},
		gen:        1,
		jobOK:      true,
	}
	m.helper = hp
	m.rum.summaryGrace = 250 * time.Millisecond

	sid := m.rum.allocSessionID()
	m.rum.onCaptureStart(hp.gen, sid, "wesing", time.Now().Add(-5*time.Second), true, true)
	m.rum.onFrame("wesing", frame.FlagSessionKnown|frame.FlagSessionActive, time.Now().Add(-4900*time.Millisecond), func(string) bool { return true })

	go m.pumpHelperStderr(stderrR, hp.stderrTail, hp.gen)

	// 假 helper：收到 stdin EOF 后等 30ms（模拟 captureLoop 退出并排空 diagLoop），写出最终 capture_summary 后退出。
	go func() {
		_, _ = io.Copy(io.Discard, stdinR)
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(stderrW, "[mbx-rum] kind=capture_summary player=wesing sid=1 norm_frames=100 capped_frames=25 discontinuities=4 frames_dropped=6\n")
		stderrW.Close()
		close(hp.exited)
	}()

	m.teardownHelper()

	// 等待独立 stderr 泵把摘要送达并触发上报
	deadline := time.Now().Add(2 * time.Second)
	var got map[string]any
	for time.Now().Before(deadline) {
		rum.Flush(500 * time.Millisecond)
		for _, ev := range col.snapshot() {
			if ev.Event == "audio_capture_session" && ev.Properties["event_kind"] == "session_end" && ev.Properties["player"] == "wesing" {
				got = ev.Properties
				break
			}
		}
		if got != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatalf("teardownHelper 后未收到 wesing session_end 事件，实际：%#v", col.snapshot())
	}
	ratio, _ := got["gain_capped_ratio"].(float64)
	if math.Abs(ratio-0.25) > 1e-6 || got["discontinuity_count"] != float64(4) || got["frames_dropped"] != float64(6) || got["stats_incomplete"] != false {
		t.Errorf("teardownHelper 优雅退出未拿到完整摘要：%#v", got)
	}
}

// TestTeardownHelperKillsProcessWhenDemuxPanickedWithoutWait 覆盖 R8：
// 当 job == 0 且 demux 因 panic 本地恢复提前关闭 hp.exited（未走到 cmd.Wait，waitDone == false）时，
// teardownHelper 仍须调用 Process.Kill() 终止残留 helper 进程。
func TestTeardownHelperKillsProcessWhenDemuxPanickedWithoutWait(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), fakeHelperEnv+"=sleep")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	// 模拟 demux panic 本地恢复：hp.exited 已提前关闭，但 waitDone 为 false 且无 Job Object
	hp := &helperProc{
		cmd:    cmd,
		stdin:  stdin,
		job:    0,
		exited: closedChan(),
		start:  time.Now(),
		gen:    1,
	}
	m := &Manager{
		helper:       hp,
		dispatched:   map[string]uint32{},
		dispatchedAt: map[string]time.Time{},
		lastFrame:    map[string]time.Time{},
		lastForward:  map[string]time.Time{},
	}

	m.teardownHelper()

	waited := make(chan error, 1)
	go func() {
		waited <- cmd.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("job==0 且 demux 未走到 cmd.Wait 时，teardownHelper 漏掉了 Process.Kill()，残留进程仍在运行")
	}
}
