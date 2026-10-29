//go:build windows

package pos

import (
	"os"
	"testing"
	"time"
)

// Opt-in read-only check; does not start, pause, seek or restart the player.
func TestLiveCoreClock(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("PLAYERCAP_LIVE=1 required")
	}
	r, err := OpenCore(int(findKuwo(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first, err := r.Read("", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RID=%s position=%dms duration=%dms", first.RID, first.PositionMs, first.DurationMs)
	var clock CoreClock
	clock.Observe(first)
	prev := first
	changes := 0
	maxGap := time.Duration(0)
	lastChange := first.ObservedAt
	sawSeek := false
	for n := 0; n < 100; n++ {
		time.Sleep(50 * time.Millisecond)
		s, err := r.Read(first.RID, false)
		if err != nil {
			t.Fatal(err)
		}
		if clock.Observe(s) {
			sawSeek = true
			t.Logf("seek/loop %d -> %d", prev.PositionMs, s.PositionMs)
		}
		if s.PositionMs != prev.PositionMs {
			changes++
			gap := s.ObservedAt.Sub(lastChange)
			if gap > maxGap {
				maxGap = gap
			}
			lastChange = s.ObservedAt
		}
		prev = s
	}
	t.Logf("samples=100 changes=%d maxChangeGap=%s position=%dms delta=%dms wall=%s", changes, maxGap, prev.PositionMs, prev.PositionMs-first.PositionMs, prev.ObservedAt.Sub(first.ObservedAt))
	if changes < 20 {
		t.Fatalf("source not high-frequency (or player paused): changes=%d", changes)
	}
	if !sawSeek {
		drift := time.Duration(prev.PositionMs-first.PositionMs)*time.Millisecond - prev.ObservedAt.Sub(first.ObservedAt)
		if drift < -200*time.Millisecond || drift > 200*time.Millisecond {
			t.Fatalf("steady playback is not 1:1 (or was paused): delta-wall=%s", drift)
		}
	} else {
		t.Log("seek/loop observed; steady rate assertion not applicable")
	}
}
