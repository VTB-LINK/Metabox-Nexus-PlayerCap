package kuwo

import (
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"errors"
	"testing"
	"time"
)

func TestLyricRetryBoundAndPause(t *testing.T) {
	s := newSession(New(0, 50))
	now := time.Now()
	observedSong(s, "MUSIC_1", 1, 0, now)
	for attempt := 1; attempt <= 3; attempt++ {
		s.retryAt = time.Time{}
		s.accept(assetResult{gen: s.gen, rid: s.rid, err: errors.New("transient")}, now)
		if s.retryDue(now) {
			t.Fatal("retry has no backoff")
		}
		if s.retryDue(now.Add(5*time.Second)) != (attempt < 3) {
			t.Fatalf("attempt=%d retry=%v", attempt, s.retryAt)
		}
	}
	s.tries = 0
	s.accept(assetResult{gen: s.gen, rid: s.rid, err: lyric.ErrNoLyrics}, now)
	if s.retryDue(now.Add(time.Minute)) {
		t.Fatal("definitive no-lyrics retries")
	}
	s.retryAt = now
	s.playing = false
	if s.retryDue(now) {
		t.Fatal("paused retries")
	}
}
