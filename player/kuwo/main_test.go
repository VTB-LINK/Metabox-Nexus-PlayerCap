package kuwo

import (
	"context"
	"errors"
	"os"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

// TestMain keeps this package's tests off the network (#167). Every session started
// by runSession fetches lyrics and covers through assetFetch, which by default calls
// Kuwo's servers in mainland China, and runSession waits for in-flight fetches before
// it returns. From the CI runners those requests can stall for seconds (the cover
// download does not honour cancellation), so timing assertions such as
// TestPersistentSettlingFailsClosed failed at random. The stub answers at once: the
// song has no lyrics (no retry) and the cover is unavailable.
// TestLivePipeline restores the real fetch for itself.
func TestMain(m *testing.M) {
	assetFetch = offlineAssetFetch
	os.Exit(m.Run())
}

var errCoverOffline = errors.New("cover fetch disabled in unit tests")

func offlineAssetFetch(_ context.Context, req assetRequest, artwork bool) assetResult {
	r := assetResult{gen: req.gen, rid: pos.RIDWithPrefix(req.meta.RID), cover: artwork}
	if artwork {
		r.err = errCoverOffline
	} else {
		r.err = lyric.ErrNoLyrics
	}
	return r
}

// The stub must stay in place for the whole package: a test that reaches the real
// fetch would talk to Kuwo's servers again.
// 变异自证：TestMain 里去掉 assetFetch = offlineAssetFetch，本用例变红。
func TestSessionsDoNotFetchFromNetwork(t *testing.T) {
	jobs := newAssetJobs(context.Background())
	defer jobs.close()
	r := jobs.fetch(context.Background(), assetRequest{gen: 1}, true)
	if !errors.Is(r.err, errCoverOffline) {
		t.Fatalf("sessions in unit tests must use the offline fetch, got err %v", r.err)
	}
}
