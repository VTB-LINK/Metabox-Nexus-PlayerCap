package pos

import (
	"testing"
	"time"
)

// Regression: pause must not advance for 4.5 seconds then snap back to the anchor.
func TestCoreClockPauseSeekAndLoop(t *testing.T) {
	var c CoreClock
	t0 := time.Unix(1, 0)
	s := Sample{RID: "MUSIC_568213041", PositionMs: 10000, DurationMs: 154000, ObservedAt: t0}
	c.Observe(s)
	s.PositionMs = 10100
	s.ObservedAt = t0.Add(100 * time.Millisecond)
	c.Observe(s)
	if !c.Advancing(s.ObservedAt) {
		t.Fatal("moving sample not recognized")
	}
	for _, elapsed := range []time.Duration{200 * time.Millisecond, time.Second, 5 * time.Second} {
		s.ObservedAt = t0.Add(elapsed)
		c.Observe(s)
		if c.Position() != 10.1 {
			t.Fatalf("pause position drifted: %f", c.Position())
		}
	}
	if c.Advancing(s.ObservedAt) {
		t.Fatal("paused sample still advances")
	}
	s.ObservedAt = s.ObservedAt.Add(100 * time.Millisecond)
	s.PositionMs = 10200
	if c.Observe(s) {
		t.Fatal("ordinary resume marked seek")
	}
	s.ObservedAt = s.ObservedAt.Add(100 * time.Millisecond)
	s.PositionMs = 85000
	if !c.Observe(s) || c.Position() != 85 {
		t.Fatal("forward seek lost")
	}
	s.ObservedAt = s.ObservedAt.Add(100 * time.Millisecond)
	s.PositionMs = 40000
	if !c.Observe(s) || c.Position() != 40 {
		t.Fatal("backward seek lost")
	}
	s.ObservedAt = s.ObservedAt.Add(100 * time.Millisecond)
	s.PositionMs = 0
	if !c.Observe(s) || c.Position() != 0 {
		t.Fatal("loop lost")
	}
	s.RID = "MUSIC_658118183"
	s.PositionMs = 1000
	s.ObservedAt = s.ObservedAt.Add(time.Second)
	if c.Observe(s) || c.Advancing(s.ObservedAt) {
		t.Fatal("new song inherited old clock")
	}
	old := s
	old.RID = "MUSIC_568213041"
	old.PositionMs = 90000
	old.ObservedAt = old.ObservedAt.Add(-time.Second)
	c.Observe(old)
	if c.Position() != 1 {
		t.Fatal("late old-song sample reset the clock")
	}
	c.Reset()
	if c.Position() != 0 || c.Advancing(t0) {
		t.Fatal("reset retained state")
	}
}
