package kuwo

import (
	"context"
	"errors"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

// The liveness watch is driven with synthetic time so the threshold scenarios
// run instantly. Covers the design §5 liveness list.
func TestLivenessWatch(t *testing.T) {
	const thr = 20 * time.Second
	defer func(v time.Duration) { livenessThreshold = v }(livenessThreshold)
	livenessThreshold = thr
	base := time.Unix(0, 0)
	const pos0 = int32(1000)

	// playing for the threshold with the position frozen trips it.
	t.Run("frozen while playing trips at threshold", func(t *testing.T) {
		var w livenessWatch
		if w.stalled(true, pos0, base) {
			t.Fatal("tripped on the first sample")
		}
		if w.stalled(true, pos0, base.Add(thr-time.Millisecond)) {
			t.Fatal("tripped before the threshold")
		}
		if !w.stalled(true, pos0, base.Add(thr)) {
			t.Fatal("did not trip at the threshold")
		}
	})

	// A pause zeros the accumulator, so two sub-threshold spans never add up.
	t.Run("pause zeros accumulation", func(t *testing.T) {
		var w livenessWatch
		w.stalled(true, pos0, base)
		if w.stalled(true, pos0, base.Add(15*time.Second)) {
			t.Fatal("tripped during first play span")
		}
		if w.stalled(false, pos0, base.Add(16*time.Second)) { // paused
			t.Fatal("tripped while paused")
		}
		if w.stalled(true, pos0, base.Add(31*time.Second)) {
			t.Fatal("pause did not zero the accumulator")
		}
	})

	// The poll interval that ends at the first playing sample after a pause was
	// spent paused. With slow polling it must not be counted as playing time.
	// 变异自证：去掉 prevPlaying 判定则 t=20s 提前触发，本用例变红（实测）。
	t.Run("resume does not count the paused gap", func(t *testing.T) {
		var w livenessWatch
		w.stalled(false, pos0, base)  // paused
		for s := 2; s <= 20; s += 2 { // 2s polling; playing from t=2s
			if w.stalled(true, pos0, base.Add(time.Duration(s)*time.Second)) {
				t.Fatalf("tripped at t=%ds with only %ds of playing time", s, s-2)
			}
		}
		if !w.stalled(true, pos0, base.Add(22*time.Second)) {
			t.Fatal("did not trip after a full threshold of playing time")
		}
	})

	// Once the position moves it never trips, even after a long later stall.
	t.Run("movement disarms for the song", func(t *testing.T) {
		var w livenessWatch
		w.stalled(true, pos0, base)
		if w.stalled(true, pos0+10, base.Add(time.Second)) {
			t.Fatal("tripped on a moving sample")
		}
		if w.stalled(true, pos0+10, base.Add(90*time.Second)) {
			t.Fatal("tripped after the position had moved earlier")
		}
	})

	// Not playing long enough does not trip.
	t.Run("short playback never trips", func(t *testing.T) {
		var w livenessWatch
		w.stalled(true, pos0, base)
		if w.stalled(true, pos0, base.Add(19*time.Second)) {
			t.Fatal("tripped below threshold")
		}
	})

	// Normal advancing playback: every sample moves, so it is disarmed at once.
	t.Run("advancing playback never trips", func(t *testing.T) {
		var w livenessWatch
		p := pos0
		for i := 0; i < 500; i++ {
			p += 50
			if w.stalled(true, p, base.Add(time.Duration(i)*time.Second)) {
				t.Fatalf("advancing playback tripped at step %d", i)
			}
		}
	})
}

// A stalled clock ends the session through the disconnect path, never the
// version-unsupported path.
func TestClockStalledMapsToDisconnect(t *testing.T) {
	if reason, _ := statusFor(pos.ErrClockStalled); reason != player.ReasonCDPDisconnected {
		t.Fatalf("stalled clock reason=%q", reason)
	}
	if reason, _ := statusFor(pos.ErrUnsupportedCore); reason == player.ReasonCDPDisconnected {
		t.Fatal("unsupported core must not be a disconnect")
	}
}

type frozenReader struct {
	closes int
	sample pos.Sample
}

func (r *frozenReader) Read(string) (pos.Sample, error) { return r.sample, nil }
func (r *frozenReader) Close()                          { r.closes++ }

// 变异自证（隔离副本，实测）：删掉 runSession 里 s.stall.stalled 的那段 → 会话不再
// 结束，runSession 一直循环直到 ctx 超时，本用例红。
func TestRunSessionFailsClosedOnStalledClock(t *testing.T) {
	defer func(v time.Duration) { livenessThreshold = v }(livenessThreshold)
	livenessThreshold = time.Nanosecond // trip on the second playing sample
	p := New(0, 50)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader := &frozenReader{sample: pos.Sample{RID: "MUSIC_1", PositionMs: 1000, DurationMs: 60000, ObservedAt: time.Now()}}
	err := p.runSession(ctx, snapshotFunc(func(context.Context) (cdp.Snapshot, error) {
		return cdp.Snapshot{RID: "MUSIC_1", Status: 1, Title: "t"}, nil
	}), func() (positionSource, error) { return reader, nil })
	if !errors.Is(err, pos.ErrClockStalled) {
		t.Fatalf("stalled clock did not fail closed: %v", err)
	}
	if reader.closes == 0 {
		t.Fatal("reader not closed on session end")
	}
}
