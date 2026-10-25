// Package kuwo combines the CEF metadata bridge with the read-only playback-core
// clock. Unsupported clocks fail closed; no four-second snapshot extrapolation.
package kuwo

import (
	"context"
	"errors"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
	"Metabox-Nexus-PlayerCap/player/kuwo/watchdog"
	"Metabox-Nexus-PlayerCap/telemetry"
)

const PlayerName = "kuwo"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("Kuwo")

type KuwoPlayer struct {
	player.BaseEmitter
	offsetMs, pollMs int
	stopOnce         sync.Once
}

func New(offsetMs, pollMs int) *KuwoPlayer {
	return &KuwoPlayer{BaseEmitter: player.NewBaseEmitter(PlayerName), offsetMs: offsetMs, pollMs: pollMs}
}

func (p *KuwoPlayer) Stop() { p.stopOnce.Do(p.BaseEmitter.Stop) }

// transientTolerance is how long a settling bridge snapshot, or a core sample that
// disagrees with it, is retried before the session fails closed. A package var so
// tests can shrink it.
var transientTolerance = time.Second

func EffectivePollInterval(pollMs int) time.Duration {
	if pollMs < 50 {
		pollMs = 50
	}
	return time.Duration(pollMs) * time.Millisecond
}

// statusFor maps a session error to its public reason and (English) detail, per
// the build-matching design §3.4. An unverified build (cdp/pos version errors)
// reports version_unsupported; a build ineligible for auto-restart reports
// cdp_disconnected with an actionable manual-launch message, not a version
// rejection; everything else is a transient disconnect.
func statusFor(err error) (reason, detail string) {
	switch {
	case errors.Is(err, watchdog.ErrNotRunning):
		return player.ReasonProcessNotRunning, "Kuwo is not running"
	case errors.Is(err, pos.ErrUnsupportedCore) || errors.Is(err, cdp.ErrVersionUnsupported):
		return player.ReasonVersionUnsupported, "This Kuwo playback core is not supported"
	case errors.Is(err, watchdog.ErrRestartNotAuthorized):
		return player.ReasonCDPDisconnected, "Kuwo must be started with debug port 9224; automatic restart is limited to verified builds, please launch Kuwo manually with --remote-debugging-port=9224"
	default:
		return player.ReasonCDPDisconnected, "Kuwo connection lost; retrying"
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *KuwoPlayer) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer telemetry.Guard()
		select {
		case <-p.StopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	lastReason := ""
	for ctx.Err() == nil {
		err := watchdog.EnsureDebugMode(ctx)
		if err == nil {
			err = p.connectSession(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		reason, detail := statusFor(err)
		p.Emit(player.EventClearSongData, nil)
		p.EmitStatusIfChanged(&player.StatusInfo{Status: "waiting_process", Detail: i18n.T(detail), Reason: reason})
		if reason != lastReason && err != nil {
			log.Warn("Kuwo capture unavailable: %v", err)
		}
		lastReason = reason
		if !wait(ctx, 3*time.Second) {
			return
		}
	}
}

type snapshotSource interface {
	Snapshot(context.Context) (cdp.Snapshot, error)
}
type positionSource interface {
	Read(string) (pos.Sample, error)
	Close()
}

func (p *KuwoPlayer) connectSession(ctx context.Context) error {
	pid, err := watchdog.FindMainPID()
	if err != nil {
		return err
	}
	client, err := cdp.Connect(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	return p.runSession(ctx, client, func() (positionSource, error) {
		reader, err := pos.OpenCore(pid)
		if err != nil {
			// Do not box a nil *CoreReader into a non-nil positionSource.
			return nil, err
		}
		timestamp, slot := reader.Build()
		log.Detail("Kuwo core bound: build %s, PlayerCore timestamp 0x%08X, read slot %d", client.Build(), timestamp, slot)
		return reader, nil
	})
}

func (p *KuwoPlayer) runSession(ctx context.Context, client snapshotSource, open func() (positionSource, error)) error {
	s := newSession(p)
	jobs := newAssetJobs(ctx)
	defer jobs.close()
	defer func() {
		// Clear BEFORE waiting for bounded in-flight artwork normalization.
		p.Emit(player.EventClearSongData, nil)
		p.EmitStatusIfChanged(&player.StatusInfo{Status: "waiting_process", Detail: i18n.T("Kuwo connection lost; retrying"), Reason: player.ReasonCDPDisconnected})
	}()
	var reader positionSource
	defer func() {
		if reader != nil {
			reader.Close()
		}
	}()
	var snap cdp.Snapshot
	// failedAt times core-sample failures; snapFailedAt times bridge snapshots that
	// are still settling. They are separate clocks so a settling bridge (a few
	// hundred ms on every song switch) does not spend the core's reconciliation budget.
	var nextMeta, failedAt, snapFailedAt time.Time
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(nextMeta) {
			var err error
			snap, err = client.Snapshot(ctx)
			if err != nil {
				if errors.Is(err, cdp.ErrSongChanged) {
					if snapFailedAt.IsZero() {
						snapFailedAt = now
					}
					if now.Sub(snapFailedAt) < transientTolerance && wait(ctx, EffectivePollInterval(p.pollMs)) {
						continue
					}
				}
				return err
			}
			snapFailedAt = time.Time{}
			if snap.Status != 1 && snap.Status != 2 && snap.Status != 3 {
				return cdp.ErrUnavailable
			}
			nextMeta = time.Now().Add(200 * time.Millisecond)
		}
		if snap.RID == "" || snap.Status == 3 {
			if s.clear() {
				jobs.cancelSong()
			}
			if reader != nil {
				reader.Close()
				reader = nil
			}
			failedAt = time.Time{}
		} else {
			var err error
			if reader == nil {
				var candidate positionSource
				candidate, err = open()
				// A failed open owns no usable reader. In particular, retaining
				// a typed nil here would skip retry and panic in Read or Close.
				if err == nil {
					reader = candidate
					if reader == nil {
						err = cdp.ErrUnavailable
					}
				}
			}
			var sample pos.Sample
			if err == nil {
				sample, err = reader.Read(snap.RID)
			}
			if err != nil {
				// Reject a torn/song-transition sample immediately. A persistent
				// source failure clears the public state instead of leaving playing.
				if failedAt.IsZero() {
					failedAt = now
				}
				nextMeta = time.Time{}
				if now.Sub(failedAt) >= transientTolerance {
					return err
				}
			} else {
				failedAt = time.Time{}
				changed := s.observe(snap, sample)
				if changed {
					jobs.startSong(s.gen, snap)
				}
				// Liveness: playing long enough that a live core would have moved,
				// yet the position never did. Fail closed via the disconnect path.
				if s.stall.stalled(snap.Status == 1, sample.PositionMs, now) {
					log.Warn("Kuwo is playing but the core position has not advanced for %s; failing closed", livenessThreshold)
					return pos.ErrClockStalled
				}
				for {
					select {
					case result := <-jobs.results:
						s.accept(result, now)
					default:
						goto drained
					}
				}
			drained:
				if s.retryDue(now) {
					jobs.retryLyrics(s.gen, snap)
					s.retryAt = time.Time{}
				}
				s.emitLine(now)
			}
		}
		if !wait(ctx, EffectivePollInterval(p.pollMs)) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}
