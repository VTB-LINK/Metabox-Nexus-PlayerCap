package kuwo

import (
	"Metabox-Nexus-PlayerCap/player"
	"testing"
	"time"
)

func TestSeekResumeEmittedOnceAndPositionHasNoOffset(t *testing.T) {
	p := New(500, 50)
	s := newSession(p)
	now := time.Now()
	observedSong(s, "MUSIC_1", 2, 10000, now)
	drainEvents(p)
	observedSong(s, "MUSIC_1", 1, 30000, now.Add(50*time.Millisecond))
	count := 0
	for _, e := range drainEvents(p) {
		if e.Type == player.EventPlaybackResume {
			count++
			v := e.Data.(*player.PlaybackTimeInfo)
			if v.Position != 30 || v.Progress != .3 {
				t.Fatalf("offset leaked into clock: %+v", v)
			}
		}
	}
	if count != 1 {
		t.Fatalf("seek+resume emitted %d resumes", count)
	}
	s.emitLine(now)
	count = 0
	for _, e := range drainEvents(p) {
		if e.Type == player.EventPositionSample {
			count++
			if e.Data.(*player.PlaybackTimeInfo).Position != 30 {
				t.Fatal("sample offset")
			}
		}
	}
	if count != 1 {
		t.Fatalf("missing position sample: %d", count)
	}
	s.emitLine(now.Add(time.Second))
	if len(drainEvents(p)) != 0 {
		t.Fatal("unthrottled sample or repeated line")
	}
	s.playing = false
	s.emitLine(now.Add(time.Minute))
	if len(drainEvents(p)) != 0 {
		t.Fatal("paused emits samples")
	}
}
