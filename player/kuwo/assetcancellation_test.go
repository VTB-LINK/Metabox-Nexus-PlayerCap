package kuwo

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
)

// Exercise the real workers, queue replacement and join, without HTTP traffic.
func TestAssetWorkersCancelPreviousSongAndJoin(t *testing.T) {
	started := make(chan uint64, 8)
	var active, peak atomic.Int32
	j := newAssetJobsWithFetch(context.Background(), func(ctx context.Context, req assetRequest, artwork bool) assetResult {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started <- req.gen
		<-ctx.Done()
		return assetResult{gen: req.gen, rid: req.meta.RID, cover: artwork}
	})
	await := func(gen uint64) {
		t.Helper()
		for i := 0; i < 2; i++ {
			select {
			case got := <-started:
				if got != gen {
					t.Fatalf("started generation %d, want %d", got, gen)
				}
			case <-time.After(time.Second):
				t.Fatal("workers did not advance after cancellation")
			}
		}
	}
	j.startSong(1, cdp.Snapshot{RID: "MUSIC_1"})
	await(1)
	j.startSong(2, cdp.Snapshot{RID: "MUSIC_2"})
	await(2)
	done := make(chan struct{})
	go func() { j.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workers leaked on close")
	}
	if active.Load() != 0 || peak.Load() > 2 {
		t.Fatalf("active=%d peak=%d", active.Load(), peak.Load())
	}
	select {
	case result := <-j.results:
		t.Fatalf("canceled asset published: %+v", result)
	default:
	}
}

func TestPendingAssetsKeepLatestOnly(t *testing.T) {
	ch := make(chan assetRequest, 1)
	for gen := uint64(1); gen <= 1000; gen++ {
		enqueue(ch, assetRequest{gen: gen})
	}
	if len(ch) != 1 || (<-ch).gen != 1000 {
		t.Fatal("pending work is not bounded/latest-only")
	}
}
