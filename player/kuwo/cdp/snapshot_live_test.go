package cdp

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveSnapshot only reads an already enabled Kuwo bridge. It never invokes
// watchdog, navigates, changes playback, or enables the debug port itself.
func TestLiveSnapshot(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("set PLAYERCAP_LIVE=1 for the read-only Kuwo CDP check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.RID != "" && s.Title == "" {
		t.Fatalf("RID without title: %+v", s)
	}
	t.Logf("RID=%s status=%d title=%q artist=%q album=%q", s.RID, s.Status, s.Title, s.Artist, s.Album)
}
