package kuwo

import (
	"context"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/cover"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
	"Metabox-Nexus-PlayerCap/telemetry"
)

type assetRequest struct {
	ctx  context.Context
	gen  uint64
	meta cdp.Snapshot
}
type assetResult struct {
	gen   uint64
	rid   string
	cover bool
	art   cover.Result
	lines []lyric.Line
	err   error
}

// Exactly two workers per session, with latest-only pending slots. Rapid seeks
// or song changes cannot accumulate unbounded download goroutines.
type assetJobs struct {
	ctx            context.Context
	cancel         context.CancelFunc
	songCtx        context.Context
	songCancel     context.CancelFunc
	lyrics, covers chan assetRequest
	results        chan assetResult
	wg             sync.WaitGroup
	fetch          func(context.Context, assetRequest, bool) assetResult
}

func newAssetJobs(parent context.Context) *assetJobs {
	return newAssetJobsWithFetch(parent, fetchAsset)
}

func newAssetJobsWithFetch(parent context.Context, fetch func(context.Context, assetRequest, bool) assetResult) *assetJobs {
	ctx, cancel := context.WithCancel(parent)
	j := &assetJobs{ctx: ctx, cancel: cancel, lyrics: make(chan assetRequest, 1), covers: make(chan assetRequest, 1), results: make(chan assetResult, 4), fetch: fetch}
	j.wg.Add(2)
	go j.worker(false)
	go j.worker(true)
	return j
}

func (j *assetJobs) close() { j.cancelSong(); j.cancel(); j.wg.Wait() }
func (j *assetJobs) cancelSong() {
	if j.songCancel != nil {
		j.songCancel()
		j.songCancel = nil
	}
}
func enqueue(ch chan assetRequest, r assetRequest) {
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- r:
	default:
	}
}
func (j *assetJobs) startSong(gen uint64, meta cdp.Snapshot) {
	j.cancelSong()
	j.songCtx, j.songCancel = context.WithCancel(j.ctx)
	r := assetRequest{ctx: j.songCtx, gen: gen, meta: meta}
	enqueue(j.lyrics, r)
	enqueue(j.covers, r)
}
func (j *assetJobs) retryLyrics(gen uint64, meta cdp.Snapshot) {
	if j.songCtx != nil {
		enqueue(j.lyrics, assetRequest{ctx: j.songCtx, gen: gen, meta: meta})
	}
}
func (j *assetJobs) worker(artwork bool) {
	defer telemetry.Guard()
	defer j.wg.Done()
	ch := j.lyrics
	if artwork {
		ch = j.covers
	}
	for {
		select {
		case <-j.ctx.Done():
			return
		case req := <-ch:
			if req.ctx.Err() != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(req.ctx, 5*time.Second)
			r := j.fetch(ctx, req, artwork)
			cancel()
			if req.ctx.Err() != nil {
				continue
			}
			select {
			case j.results <- r:
			case <-req.ctx.Done():
			case <-j.ctx.Done():
				return
			}
		}
	}
}

func fetchAsset(ctx context.Context, req assetRequest, artwork bool) assetResult {
	r := assetResult{gen: req.gen, rid: pos.RIDWithPrefix(req.meta.RID), cover: artwork}
	if artwork {
		r.art, r.err = cover.Fetch(ctx, req.meta.RID, 5*time.Second)
	} else {
		var response lyric.Response
		response, r.err = lyric.NewClient().Fetch(ctx, lyric.Request{RID: req.meta.RID, Song: req.meta.Title, Artist: req.meta.Artist})
		if r.err == nil {
			r.lines = lyric.ParseLRC(response.LRC)
		}
	}
	return r
}
