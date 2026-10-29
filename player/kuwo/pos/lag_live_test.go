//go:build windows

package pos

import (
	"os"
	"testing"
	"time"
)

// Opt-in read-only check of the seek-lag reader against a playing kwmusic;
// does not start, pause, seek or restart the player.
func TestLiveLag(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("PLAYERCAP_LIVE=1 required")
	}
	r, err := OpenLag(int(findKuwo(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	t.Logf("build %s", r.Build())
	valid, failed := 0, 0
	var last LagSample
	for n := 0; n < 100; n++ {
		if n > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		s, _, err := r.Read()
		if err != nil {
			if failed++; failed == 1 {
				t.Logf("sample %d: first read error: %v", n, err)
			}
			continue
		}
		if !s.Valid {
			continue
		}
		valid++
		if s.Key != last.Key || s.EUs != last.EUs {
			t.Logf("sample %d: e=%.1fms D=%.1fms segment ctx=%#x ring=%#x base=%dms written=%d",
				n, float64(s.EUs)/1000, float64(s.DUs)/1000, s.Key.Ctx, s.Key.Ring, s.Key.Base, s.Written)
		}
		last = s
	}
	t.Logf("samples=100 valid=%d readErrors=%d", valid, failed)
	if valid == 0 {
		t.Fatal("no valid sample (or the player is not playing)")
	}
}
