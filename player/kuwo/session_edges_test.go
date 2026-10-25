package kuwo

// Regression checks for the three session edge cases reproduced independently
// during integration review. The pre-fix isolated session failed all three:
// paused seek ended in resume, unsorted LRC selected an older line, and negative
// file offsets enlarged detailed duration. No live player or HTTP is used.
import (
	"math"
	"reflect"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

func drainEdgeEvents(p *KuwoPlayer) []player.Event {
	var events []player.Event
	for {
		select {
		case event := <-p.Events():
			events = append(events, event)
		default:
			return events
		}
	}
}

func TestSessionEdgePausedSeekEndsPaused(t *testing.T) {
	for _, initialStatus := range []int{1, 2} {
		p := New(200, 50)
		s := newSession(p)
		now := time.Unix(100, 0)
		meta := cdp.Snapshot{RID: "123", Status: initialStatus, Title: "Test"}
		s.observe(meta, pos.Sample{RID: "MUSIC_123", PositionMs: 10000, DurationMs: 60000, ObservedAt: now})
		drainEdgeEvents(p)
		meta.Status = 2
		s.observe(meta, pos.Sample{RID: "MUSIC_123", PositionMs: 30000, DurationMs: 60000, ObservedAt: now.Add(50 * time.Millisecond)})
		var clockEvents []string
		for _, event := range drainEdgeEvents(p) {
			if event.Type == player.EventPlaybackPause || event.Type == player.EventPlaybackResume {
				clockEvents = append(clockEvents, event.Type)
				info, ok := event.Data.(*player.PlaybackTimeInfo)
				if !ok || info.Position != 30 || info.Progress != 0.5 {
					t.Fatalf("initial status %d: wrong seek anchor: %+v", initialStatus, event)
				}
			}
		}
		if !reflect.DeepEqual(clockEvents, []string{player.EventPlaybackResume, player.EventPlaybackPause}) {
			t.Fatalf("initial status %d: paused seek event order %v", initialStatus, clockEvents)
		}
		if s.playing {
			t.Fatal("paused seek changed local playback state")
		}
		// A stable next tick is silent: the correction cannot rely on a later
		// transition to restore pause after the seek tick.
		s.observe(meta, pos.Sample{RID: "MUSIC_123", PositionMs: 30000, DurationMs: 60000, ObservedAt: now.Add(100 * time.Millisecond)})
		if events := drainEdgeEvents(p); len(events) != 0 {
			t.Fatalf("stable paused tick emitted %+v", events)
		}
	}
}

func TestSessionEdgeUnsortedLinesMatchChronologically(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	now := time.Unix(100, 0)
	s.observe(cdp.Snapshot{RID: "123", Status: 1, Title: "Test"}, pos.Sample{RID: "MUSIC_123", PositionMs: 25000, DurationMs: 60000, ObservedAt: now})
	drainEdgeEvents(p)
	s.lines = toLyricLines(lyric.ParseLRC("[00:20.000]later\n[00:10.000]earlier\n[00:20.000]later duplicate"), 0)
	if len(s.lines) != 3 || s.lines[0].Text != "earlier" || s.lines[1].Text != "later" || s.lines[2].Text != "later duplicate" {
		t.Fatalf("not a stable chronological sort: %+v", s.lines)
	}
	for i, line := range s.lines {
		if line.Index != i {
			t.Fatalf("stale index after sort: %+v", s.lines)
		}
	}
	s.emitLine(now)
	found := false
	for _, event := range drainEdgeEvents(p) {
		if event.Type == player.EventLyricUpdate {
			found = true
			update, ok := event.Data.(*player.LyricUpdate)
			if !ok || update.Text != "later duplicate" || update.Index != 2 || update.Timestamp != 20 || update.Position != 25 {
				t.Fatalf("25s selected wrong line: %+v", event.Data)
			}
		}
	}
	if !found {
		t.Fatal("missing lyric update")
	}
}

func TestSessionEdgeNegativeFileOffsetKeepsDetailedDuration(t *testing.T) {
	lines := toLyricLines(lyric.ParseLRC("[kuwo:067]\n[offset:-500]\n[00:00.000]<50,-50>title"), 0.2)
	if len(lines) != 1 || len(lines[0].TextDetailed.Words) != 1 {
		t.Fatalf("missing detailed timing: %+v", lines)
	}
	line := lines[0]
	word := line.TextDetailed.Words[0]
	if line.Timestamp != -0.5 || line.TextDetailed.Timestamp != -0.5 || word.Timestamp != -0.5 {
		t.Fatalf("source file offset was lost: %+v", line)
	}
	if math.Abs(float64(line.TextDetailed.Duration)-0.01) > 0.000001 || math.Abs(float64(word.Duration)-0.01) > 0.000001 {
		t.Fatalf("10ms source span duration changed: %+v", line)
	}
	if line.PlayTime != 0 || line.TextDetailed.PlayTime != 0 || word.PlayTime != 0 {
		t.Fatalf("negative display times were not clamped by common offset helper: %+v", line)
	}
}
