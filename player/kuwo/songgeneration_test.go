package kuwo

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/cover"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

func drainEvents(p *KuwoPlayer) []player.Event {
	var out []player.Event
	for {
		select {
		case e := <-p.Events():
			out = append(out, e)
		default:
			return out
		}
	}
}

func observedSong(s *session, rid string, status int, ms int32, at time.Time) bool {
	return s.observe(cdp.Snapshot{RID: rid, Title: "song " + rid, Artist: "artist", Status: status}, pos.Sample{RID: rid, PositionMs: ms, DurationMs: 100000, ObservedAt: at})
}

// Assets must never resurrect a previous song, even when its RID is revisited.
func TestSongGenerationRejectsLateAssets(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	now := time.Now()
	observedSong(s, "MUSIC_1", 1, 1000, now)
	old := s.gen
	observedSong(s, "MUSIC_2", 1, 2000, now.Add(time.Second))
	observedSong(s, "MUSIC_1", 1, 3000, now.Add(2*time.Second))
	drainEvents(p)
	s.accept(assetResult{gen: old, rid: "MUSIC_1", cover: true, art: cover.Result{URL: "stale"}}, now)
	s.accept(assetResult{gen: s.gen, rid: "MUSIC_2", lines: []lyric.Line{{Text: "stale"}}}, now)
	if e := drainEvents(p); len(e) != 0 {
		t.Fatalf("late assets emitted: %+v", e)
	}
	s.accept(assetResult{gen: s.gen, rid: s.rid, cover: true, art: cover.Result{URL: "current", DataURI: "data:current"}}, now)
	e := drainEvents(p)
	if len(e) != 1 || e[0].Data.(*player.SongInfo).Cover != "current" {
		t.Fatalf("current cover missing: %+v", e)
	}
	gen := s.gen
	s.clear()
	drainEvents(p)
	s.accept(assetResult{gen: gen, rid: "MUSIC_1", cover: true, art: cover.Result{URL: "resurrect"}}, now)
	if s.clear() || len(drainEvents(p)) != 0 {
		t.Fatal("idle resurrected or duplicate clear")
	}
}
