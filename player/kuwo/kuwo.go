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
	"Metabox-Nexus-PlayerCap/player/launch"
	"Metabox-Nexus-PlayerCap/telemetry"
)

const PlayerName = "kuwo"

func init() { config.RegisterPlayer(PlayerName) }

var log = logger.New("Kuwo")

// downloadURL is Kuwo's official download page; its Windows button served the
// 9.6.0.0_W1 installer (2026-11-20). The installer link itself carries an
// expiring signature, so the page is the stable address to give.
const downloadURL = "https://www.kuwo.cn/down"

type KuwoPlayer struct {
	player.BaseEmitter
	offsetMs, pollMs int
	seen             bool // 本次服务期间是否曾见过酷我进程在运行（仅 Start goroutine 读写）
	stopOnce         sync.Once
	rumGate          player.SessionRUMGate[*kuwoSessionRUM]
	testBeforeAttach func() // 仅供单测注入 Register 锁外发 attached 前的并发交错屏障
	attachMetaMu     sync.Mutex
	attachMeta       kuwoAttachMeta
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

func kuwoDetails() player.ProcessDetails {
	return player.ProcessDetails{
		NotRunning:   i18n.T("Kuwo is not running"),
		Exited:       i18n.T("Kuwo has exited"),
		Disconnected: i18n.T("Kuwo connection lost; retrying"),
	}
}

// statusFor 把会话错误映射为状态（设计 §3.3）：ErrNotRunning 为进程不在运行，其余为在运行而读不了。
// 它作为辅助函数只用作 EmitStatusIfChanged 的直接实参（满足 waitingstatus_lint_test.go 规则 2）。
func statusFor(err error, seen bool) *player.StatusInfo {
	switch {
	case errors.Is(err, watchdog.ErrNotRunning):
		return player.ProcessStatus(false, seen, kuwoDetails(), player.Blocked{})
	case errors.As(err, new(*launch.PackagedError)): // 商店版门禁（#212）
		return player.ProcessStatus(true, true, kuwoDetails(), player.Blocked{
			Reason: player.ReasonVersionUnsupported,
			Detail: i18n.T("Kuwo Microsoft Store version unsupported (requires the official installer version)"),
		})
	case errors.Is(err, launch.ErrExeUnknown):
		return player.ProcessStatus(true, true, kuwoDetails(), player.Blocked{
			Reason: player.ReasonNoAdmin,
			Detail: i18n.T("Cannot read the Kuwo installation path (Kuwo may be running as administrator)"),
		})
	case errors.Is(err, pos.ErrUnsupportedCore) || errors.Is(err, cdp.ErrVersionUnsupported):
		return player.ProcessStatus(true, true, kuwoDetails(), player.Blocked{
			Reason: player.ReasonVersionUnsupported,
			Detail: i18n.T("This Kuwo playback core is not supported"),
		})
	case errors.Is(err, watchdog.ErrRestartNotAuthorized):
		return player.ProcessStatus(true, true, kuwoDetails(), player.Blocked{
			Reason: player.ReasonCDPDisconnected,
			Detail: i18n.T("Kuwo must be started with debug port 9224; automatic restart is limited to verified builds, please launch Kuwo manually with --remote-debugging-port=9224"),
		})
	default:
		return player.ProcessStatus(true, true, kuwoDetails(), player.Blocked{})
	}
}

// errLogReason 返回 Start 告警去重用的原因分类键，与 statusFor 的非退出 reason 一致，
// 避免把 statusFor 的结果存入局部变量而违反 waitingstatus_lint_test.go 规则 2。
func errLogReason(err error) string {
	switch {
	case errors.Is(err, watchdog.ErrNotRunning):
		return player.ReasonProcessNotRunning
	case errors.As(err, new(*launch.PackagedError)):
		return player.ReasonVersionUnsupported
	case errors.Is(err, launch.ErrExeUnknown):
		return player.ReasonNoAdmin
	case errors.Is(err, pos.ErrUnsupportedCore) || errors.Is(err, cdp.ErrVersionUnsupported):
		return player.ReasonVersionUnsupported
	default:
		return player.ReasonCDPDisconnected
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
	var bt kuwoBlockTracker
	for ctx.Err() == nil {
		err := ensureDebugMode(ctx)
		if err == nil {
			p.seen = true
			err = p.connectSessionWithBT(ctx, &bt)
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil || !errors.Is(err, watchdog.ErrNotRunning) {
			p.seen = true
		}
		switch {
		case errors.Is(err, watchdog.ErrNotRunning):
			bt.onNotRunning()
		case errors.As(err, new(*launch.PackagedError)):
			bt.onBlocked("store_install")
		case errors.Is(err, launch.ErrExeUnknown):
			bt.onBlocked("no_admin")
		case errors.Is(err, pos.ErrUnsupportedCore):
			bt.onBlocked("kuwo_unsupported_core")
		case errors.Is(err, cdp.ErrVersionUnsupported) || errors.Is(err, watchdog.ErrVersionUnsupported):
			bt.onBlocked("version_unsupported")
		case errors.Is(err, watchdog.ErrAmbiguous):
			bt.onBlocked("kuwo_ambiguous_proc")
		case err != nil:
			bt.onBlocked("cdp_port_not_ready")
		}
		p.EmitStatusIfChanged(statusFor(err, p.seen))
		p.Emit(player.EventClearSongData, nil)
		reason := errLogReason(err)
		if reason != lastReason && err != nil {
			var packaged *launch.PackagedError
			switch {
			case errors.As(err, &packaged):
				log.Warn("Kuwo is the Microsoft Store version (%s) and cannot be started with the debug flag; skipped Kuwo capture. To use it, uninstall it and install the official installer version: %s", packaged.ExePath, downloadURL)
				telemetry.ReportOnce("kuwo.store_install", "酷我为微软商店版，无法带调试参数启动", nil, map[string]any{"exe_path": packaged.ExePath})
			case errors.Is(err, launch.ErrExeUnknown):
				log.Warn("Cannot read the installation path of Kuwo; it was not restarted automatically. Run Kuwo with the same privileges as this program")
			default:
				log.Warn("Kuwo capture unavailable: %v", err)
			}
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
type cdpSession interface {
	snapshotSource
	Build() string
	Close() error
}
type positionSource interface {
	// Read takes one sample. playing (meta.Status == 1) feeds the corrected
	// source's #135 transient filter; the bare core reader ignores it.
	Read(rid string, playing bool) (pos.Sample, error)
	Close()
}

type lagUnsupportedSource struct {
	positionSource
}

// 以下为离线测试 Start / connectSession 用的包级 seam，生产恒指向真实实现。
var (
	ensureDebugMode = watchdog.EnsureDebugMode
	findMainPID     = watchdog.FindMainPID
	connectCDP      = func(ctx context.Context) (cdpSession, error) {
		c, err := cdp.Connect(ctx)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	openPosition = func(pid int, build string, warned *bool, pollMs int) (positionSource, error) {
		reader, err := pos.OpenCore(pid)
		if err != nil {
			// Do not box a nil *CoreReader into a non-nil positionSource.
			return nil, err
		}
		timestamp, slot := reader.Build()
		log.Detail("Kuwo core bound: build %s, PlayerCore timestamp 0x%08X, read slot %d", build, timestamp, slot)
		lagUnsupported := false
		src := withLagNotifying(reader, func() (lagReader, string, error) {
			lag, err := pos.OpenLag(pid)
			if err != nil {
				// Likewise for a nil *LagReader.
				return nil, "", err
			}
			return lag, lag.Build(), nil
		}, warned, pollMs, func() { lagUnsupported = true })
		if lagUnsupported {
			return lagUnsupportedSource{positionSource: src}, nil
		}
		return src, nil
	}
)

func (p *KuwoPlayer) connectSession(ctx context.Context) error {
	return p.connectSessionWithBT(ctx, nil)
}

func (p *KuwoPlayer) connectSessionWithBT(ctx context.Context, bt *kuwoBlockTracker) error {
	pid, err := findMainPID()
	if err != nil {
		return err
	}
	p.seen = true
	connectStart := time.Now()
	client, err := connectCDP(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	if bt != nil {
		bt.onRecoveredExcept("kuwo_unsupported_core")
	}
	p.setSessionAttachMeta(time.Since(connectStart).Milliseconds(), client.Build())
	// Per connection: the first seek-lag drift is logged as a warning.
	warned := false
	return p.runSessionWithBT(ctx, client, func() (positionSource, error) {
		return openPosition(pid, client.Build(), &warned, p.pollMs)
	}, bt)
}

// switchWatch returns the reader to keep and watch through a switch (#198): a
// corrected source while a song is published. A bare core reader has no output
// to watch.
func switchWatch(reader positionSource, s *session) (*correctedSource, bool) {
	c, ok := reader.(*correctedSource)
	return c, ok && s.rid != ""
}

func (p *KuwoPlayer) runSession(ctx context.Context, client snapshotSource, open func() (positionSource, error)) error {
	return p.runSessionWithBT(ctx, client, open, nil)
}

func (p *KuwoPlayer) runSessionWithBT(ctx context.Context, client snapshotSource, open func() (positionSource, error), bt *kuwoBlockTracker) (retErr error) {
	meta := p.takeSessionAttachMeta()
	rumSess := p.startSessionRUM(meta.build, meta.attachMs)
	defer func() {
		if rumSess == nil {
			return
		}
		cause := "cdp_closed"
		switch {
		case ctx.Err() != nil:
			cause = "stopped"
		case errors.Is(retErr, pos.ErrClockStalled):
			cause = "clock_stalled_20s"
		case errors.Is(retErr, pos.ErrUnsupportedCore):
			cause = "unsupported_core"
		default:
			if _, err := findMainPID(); errors.Is(err, watchdog.ErrNotRunning) {
				cause = "process_exited"
			}
		}
		p.endSessionRUM(rumSess, cause)
	}()

	s := newSession(p)
	s.rum = rumSess
	jobs := newAssetJobs(ctx)
	defer jobs.close()
	defer func() {
		// 状态先于清空发出（设计 §4.4、§4.5），且在等封面任务结束前清掉对外态；
		// 连接断开报 standby / cdp_disconnected，进程是否还在由下一轮外层循环判。
		p.EmitStatusIfChanged(player.ProcessStatus(true, true, kuwoDetails(), player.Blocked{}))
		p.Emit(player.EventClearSongData, nil)
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
					if u, ok := candidate.(lagUnsupportedSource); ok {
						rumSess.onCompat("unknown_audio_plugin_lag_off", "")
						candidate = u.positionSource
					}
					reader = candidate
					if reader == nil {
						err = cdp.ErrUnavailable
					}
				}
			}
			var sample pos.Sample
			if err == nil {
				sample, err = reader.Read(snap.RID, snap.Status == 1)
				if err == nil {
					if bt != nil {
						bt.onRecovered()
					}
					if cm, ok := reader.(interface{ clockMode() string }); ok {
						if m := cm.clockMode(); m != "" {
							s.clockModeHint = m
							rumSess.updateClockMode(s.gen, m)
						}
					}
				}
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
					rumSess.onCompat("clock_stalled_20s", "")
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
