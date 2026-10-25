package kuwo

// These tests keep Kuwo's delayed translation rows attached to the preceding
// original, without shifting its word clock or leaking translations on a switch.
import (
	"math"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

const bilingualFixture = "[ml:1.0]\n[kuwo:054]\n[offset:100]\n" +
	"[00:01.000]<400,-400>原文一\n" +
	"[00:02.000]<0,0>译文一\n" +
	"[00:02.000]<800,-800>原文二\n" +
	"[00:03.000]<0,0>译文二\n[00:03.000]\n"

func TestTranslationOutputAndClock(t *testing.T) {
	lines := toLyricLines(lyric.ParseLRC(bilingualFixture), 0.2)
	if len(lines) != 2 {
		t.Fatalf("translation became original rows: %+v", lines)
	}
	if lines[0].SubText != "译文一" || lines[1].SubText != "译文二" || lines[0].Text != "原文一" || lines[1].Text != "原文二" {
		t.Fatalf("incorrect translation pairing: %+v", lines)
	}
	if lines[0].Timestamp != 1.1 || math.Abs(float64(lines[0].PlayTime)-1.3) > 1e-6 || len(lines[0].TextDetailed.Words) != 1 || lines[0].TextDetailed.Words[0].Duration != 0.1 {
		t.Fatalf("original word clock changed: %+v", lines[0])
	}
	p := New(200, 50)
	s := newSession(p)
	now := time.Unix(100, 0)
	s.observe(cdp.Snapshot{RID: "123", Status: 1}, pos.Sample{RID: "MUSIC_123", PositionMs: 1500, DurationMs: 60000, ObservedAt: now})
	drainEdgeEvents(p)
	s.lines = lines
	s.emitLine(now)
	found := false
	for _, e := range drainEdgeEvents(p) {
		if e.Type == player.EventLyricUpdate {
			u := e.Data.(*player.LyricUpdate)
			if u.SubText != "译文一" || u.Text != "原文一" {
				t.Fatalf("current-line translation lost: %+v", u)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing current-line event")
	}
	s.observe(cdp.Snapshot{RID: "456", Status: 1}, pos.Sample{RID: "MUSIC_456", PositionMs: 0, DurationMs: 60000, ObservedAt: now.Add(time.Second)})
	if len(s.lines) != 0 {
		t.Fatal("old translation survived track switch")
	}
}
