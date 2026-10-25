package kuwo

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// Opt-in complete capture pipeline. Requires an already-enabled CDP instance
// playing a song with word lyrics; never restarts or controls the music player.
func TestLivePipeline(t *testing.T) {
	if os.Getenv("PLAYERCAP_KUWO_PIPELINE_LIVE") != "1" {
		t.Skip("PLAYERCAP_KUWO_PIPELINE_LIVE=1 required")
	}
	duration := 12 * time.Second
	if raw := os.Getenv("PLAYERCAP_KUWO_LIVE_DURATION"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil || v < time.Second || v > time.Minute {
			t.Fatal("live duration must be 1s..1m")
		}
		duration = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	p := New(200, 50)
	done := make(chan error, 1)
	go func() { done <- p.connectSession(ctx) }()
	var title, art bool
	words, samples, pauses, resumes := 0, 0, 0, 0
	check := func(e player.Event) {
		switch e.Type {
		case player.EventSongInfoUpdate:
			v := e.Data.(*player.SongInfo)
			title = title || v.Name != ""
			art = art || len(v.CoverBase64) > 100
			t.Logf("song=%q cover_bytes=%d", v.Title, len(v.CoverBase64))
		case player.EventAllLyrics:
			v := e.Data.(*player.AllLyricsData)
			n := 0
			for _, l := range v.Lyrics {
				n += len(l.TextDetailed.Words)
			}
			if n > words {
				words = n
			}
			t.Logf("lyrics=%d words=%d position=%.3f duration=%.3f", v.Count, n, v.Position, v.Duration)
		case player.EventStatusUpdate:
			t.Logf("status=%+v", e.Data)
		case player.EventPositionSample:
			samples++
			t.Logf("sample=%+v", e.Data)
		case player.EventPlaybackPause:
			pauses++
			t.Logf("pause=%+v", e.Data)
		case player.EventPlaybackResume:
			resumes++
			t.Logf("resume/seek=%+v", e.Data)
		}
	}
	for {
		select {
		case e := <-p.Events():
			check(e)
		case err := <-done:
			for _, e := range drainEvents(p) {
				check(e)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("pipeline exited: %v", err)
			}
			t.Logf("title=%v cover=%v words=%d samples=%d pauses=%d resumes=%d", title, art, words, samples, pauses, resumes)
			if !title || !art || words == 0 || samples == 0 {
				t.Fatal("incomplete live pipeline; requires playing a word-lyric song")
			}
			return
		}
	}
}
