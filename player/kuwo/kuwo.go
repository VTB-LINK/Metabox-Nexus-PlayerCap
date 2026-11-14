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

// noSongTolerance is how long the bridge may report no song, once a song has
// been published, before the session clears it and reports waiting_song. On a
// switch the bridge reports no song for a moment before the next song (#182:
// 0.57 s and 1.10 s from waiting_song to the next song, 2026-11-13). A package
// var so tests can shrink it.
var noSongTolerance = 2 * time.Second

// snapshotNow stamps when a bridge snapshot was answered, the clock the #185 hold
// confirms against together with the core sample's ObservedAt. A package var so
// tests can run the hold on their fake's clock, deterministically.
var snapshotNow = time.Now

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
	// Read takes one sample. playing (meta.Status == 1) feeds the corrected
	// source's #135 transient filter; the bare core reader ignores it.
	Read(rid string, playing bool) (pos.Sample, error)
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
	// Per connection: the first seek-lag drift is logged as a warning.
	warned := false
	return p.runSession(ctx, client, func() (positionSource, error) {
		reader, err := pos.OpenCore(pid)
		if err != nil {
			// Do not box a nil *CoreReader into a non-nil positionSource.
			return nil, err
		}
		timestamp, slot := reader.Build()
		log.Detail("Kuwo core bound: build %s, PlayerCore timestamp 0x%08X, read slot %d", client.Build(), timestamp, slot)
		return withLag(reader, func() (lagReader, string, error) {
			lag, err := pos.OpenLag(pid)
			if err != nil {
				// Likewise for a nil *LagReader.
				return nil, "", err
			}
			return lag, lag.Build(), nil
		}, &warned, p.pollMs), nil
	})
}

// switchWatch returns the reader to keep and watch through a switch (#198): a
// corrected source while a song is published. A bare core reader has no output
// to watch.
func switchWatch(reader positionSource, s *session) (*correctedSource, bool) {
	c, ok := reader.(*correctedSource)
	return c, ok && s.rid != ""
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
	// noSongAt times the bridge reporting no song, a third clock for the same reason.
	// snapAt is when the current snapshot was answered, for the #185 hold.
	var nextMeta, failedAt, snapFailedAt, noSongAt, snapAt time.Time
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
					if now.Sub(snapFailedAt) < transientTolerance {
						// The next song's output may open and start playing while the
						// bridge is settling: watch it (#198 A-1).
						if c, ok := switchWatch(reader, s); ok {
							c.observeOutput()
						}
						if wait(ctx, EffectivePollInterval(p.pollMs)) {
							continue
						}
					}
				}
				return err
			}
			snapAt = snapshotNow()
			if !snapFailedAt.IsZero() && !noSongAt.IsZero() {
				// The no-song clock is paused through a settling window: it counts
				// the time the bridge reports no song, not the time its state is
				// changing, so no song, settling, no song again is not cleared early
				// (#182). A window is cut off once a failed snapshot finds it past
				// transientTolerance, but its length also includes the snapshot calls
				// themselves (up to the CDP call timeout), so the time the previous song
				// is kept has no strict bound when the bridge keeps flipping.
				noSongAt = noSongAt.Add(now.Sub(snapFailedAt))
			}
			snapFailedAt = time.Time{}
			if snap.Status != 1 && snap.Status != 2 && snap.Status != 3 {
				return cdp.ErrUnavailable
			}
			nextMeta = time.Now().Add(200 * time.Millisecond)
		}
		if snap.RID == "" || snap.Status == 3 {
			// A published song is kept through a switch's no-song moment (#182): no
			// event of it goes out, and a song reported within noSongTolerance is
			// handled by observe as a switch, or as the same song going on. Its asset
			// jobs are kept for the latter; startSong cancels them for the former.
			// With no song published, waiting_song goes out at once. A held step back
			// (#185) ends here unpublished.
			s.dropHold(now)
			if noSongAt.IsZero() {
				noSongAt = now
			}
			if (s.rid == "" || now.Sub(noSongAt) >= noSongTolerance) && s.clear() {
				jobs.cancelSong()
			}
			// Through a switch's no-song moment the corrected source is kept and
			// watches the output, so the next song's opening segment is seen from its
			// start and its transient filtered (#198,
			// doc/design/kuwo-switch-start-transient-design.md §5.1 A-1). Once the
			// session has cleared (no song beyond noSongTolerance) or before any song
			// was published, the reader is closed at once, as before, and the next
			// song opens a new one with a fresh filter and corrector; so is a bare core
			// reader, so that the next song retries OpenLag (lagsource.go).
			if c, ok := switchWatch(reader, s); ok {
				c.observeOutput()
			} else if reader != nil {
				reader.Close()
				reader = nil
			}
			failedAt = time.Time{}
		} else {
			noSongAt = time.Time{}
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
				sample, err = reader.Read(snap.RID, snap.Status == 1)
			}
			if err != nil {
				// Reject a torn/song-transition sample immediately. A persistent
				// source failure clears the public state instead of leaving playing.
				// A held step back (#185) ends unpublished.
				s.dropHold(now)
				if failedAt.IsZero() {
					failedAt = now
				}
				nextMeta = time.Time{}
				if now.Sub(failedAt) >= transientTolerance {
					return err
				}
			} else {
				failedAt = time.Time{}
				if s.hold(snap, snapAt, sample) {
					// A step back to the start is held until the bridge confirms the
					// song (#185): refresh the snapshot every tick meanwhile.
					nextMeta = time.Time{}
				} else if s.observe(snap, sample) {
					jobs.startSong(s.gen, snap)
				}
				// Liveness: playing long enough that a live core would have moved,
				// yet the position never did. Fail closed via the disconnect path.
				// It watches the raw core position (CoreMs), before the #135 transient
				// filter and the #131 correction: the filter may pin the position at the
				// base and a change of the correction moves the public position, neither
				// of which is core movement (design §3.2).
				if s.stall.stalled(snap.Status == 1, sample.CoreMs, now) {
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
				// Retry a dropped playback_pause (#134), independent of observe's
				// event branch, which does not fire on a tick with no change.
				s.resendPauseIfPending()
			}
		}
		if !wait(ctx, EffectivePollInterval(p.pollMs)) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}
