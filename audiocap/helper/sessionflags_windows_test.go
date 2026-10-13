//go:build windows

package helper

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/dsp"
	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/audiocap/session"
)

// 本文件只测会话信号进帧：encodeFrame 与 pumpCapture 把本路槽位的结果并入帧头 flags。

const (
	sessKnown = frame.FlagSessionKnown
	sessBoth  = frame.FlagSessionKnown | frame.FlagSessionActive
)

// 槽位为 nil、从未发布、新鲜（有或无 Active 会话）、已过期时，真音频帧与保活零帧的 flags；与归一化位并存、
// 互不覆盖。
//
// 变异自证（实测）：只在真音频分支并入会话标志 → 保活帧各例红；归一化启用时以赋值覆盖 flags（不是或上）→
// 归一化启用且槽位新鲜的各例红；不并入会话标志 → 新鲜各例红。
func TestEncodeFrameSessionFlags(t *testing.T) {
	published := func(active bool, at time.Time) *session.Slot {
		s := new(session.Slot)
		s.Publish(active, at)
		return s
	}
	for _, c := range []struct {
		name string
		slot func() *session.Slot
		want uint32
	}{
		{"nil 槽位", func() *session.Slot { return nil }, 0},
		{"从未发布", func() *session.Slot { return new(session.Slot) }, 0},
		{"新鲜、有 Active 会话", func() *session.Slot { return published(true, time.Now()) }, sessBoth},
		{"新鲜、无 Active 会话", func() *session.Slot { return published(false, time.Now()) }, sessKnown},
		{"已过期", func() *session.Slot { return published(true, time.Now().Add(-time.Minute)) }, 0},
	} {
		for _, norm := range []bool{false, true} {
			for _, batch := range [][]float32{audioBatch(fftSize), nil} {
				want := c.want
				if norm {
					want |= frame.FlagNormalized
				}
				f := decodeOne(t, encodeFrame(dsp.New(fftSize), newNormalizer(norm, -14, 12, testSR), c.slot(), "kugou", 1, batch, [2]float32{}, len(batch)))
				if f.Flags != want {
					t.Errorf("%s norm=%v 保活帧=%v: flags = %#x, 期望 %#x", c.name, norm, batch == nil, f.Flags, want)
				}
			}
		}
	}
}

// pumpCapture 把传入的槽位交给每一帧：30 帧音频与 3 帧保活零帧都带上新鲜槽位的会话标志（数据形态同
// TestPumpCaptureHonorsControl）。
//
// 变异自证（实测）：pumpCapture 交给 encodeFrame 的槽位写成 nil → 红。
func TestPumpCaptureCarriesSessionFlags(t *testing.T) {
	clk := time.Unix(1000, 0)
	i := 0
	read := func(dst []float32) (int, bool, error) {
		defer func() { i++ }()
		switch {
		case i < 100:
			clk = clk.Add(10 * time.Millisecond)
			for j := 0; j < 960; j++ {
				dst[j] = 0.05
			}
			return 480, false, nil
		case i < 106:
			clk = clk.Add(20 * time.Millisecond)
			return 0, false, nil
		}
		return 0, false, errStopPump
	}
	slot := new(session.Slot)
	slot.Publish(true, time.Now())
	frames := make(chan []byte, 256)
	ctl := control{op: "capture", pid: 1, player: "qqmusic", fps: 30}
	if err := pumpCapture(read, func() time.Time { return clk }, ctl, 7, slot, make(chan struct{}), frames); err != errStopPump {
		t.Fatalf("pumpCapture 应返回读包的错误, 得 %v", err)
	}
	close(frames)
	n := 0
	for raw := range frames {
		if f := decodeOne(t, raw); f.Flags != sessBoth {
			t.Fatalf("第 %d 帧（rms=%v）: flags = %#x, 期望 %#x", n, f.RMS, f.Flags, sessBoth)
		}
		n++
	}
	if n != 33 {
		t.Fatalf("出帧数 = %d, 期望 33（30 帧音频 + 3 帧保活）", n)
	}
}
