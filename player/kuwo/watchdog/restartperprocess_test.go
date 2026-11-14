package watchdog

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
)

// This file covers #187: the restart budget is per Kuwo process. A user who
// closes the restarted Kuwo and opens it again normally (without the flag) gets
// one more restart, but only after the instance the previous restart launched
// was confirmed running with the flag and an answering port, so a launcher that
// drops the flag cannot cause a restart loop.
//
// Mutation check (by hand; each fails at least the tests named):
//   - pre-#187 watchdog.go: TestNormallyReopenedKuwoIsRestartedAgain,
//     TestRepeatedNormalReopensRestartEachOnce,
//     TestUnverifiedBuildIsNotRestartedAfterConfirmedRestart.
//   - never set limited: TestSameProcessIsNotRestartedTwice, every case of
//     TestUnconfirmedRestartBlocksLaterRestarts, the tests on instances other
//     than the launched one and TestMisidentifiedLaunchCostsAtMostOneMoreRestart;
//     set it only after a successful stop: the stop cases.
//   - clear it once start succeeds: all unconfirmed cases but start and stop,
//     the other-instance tests, TestMisidentifiedLaunchCostsAtMostOneMoreRestart.
//   - confirm without the flag check: "port answered without the flag".
//   - no confirmation in later ensures: TestLateReadyLaunchedInstanceIsConfirmedLater.
//   - confirm by the snapshot taken before the probe, or without checking that
//     the instance after the probe is the one before it: both
//     TestInstanceSwapped* tests (the latter also
//     TestOtherFlaggedInstanceDoesNotConfirmRestart).
//   - not check the instance before the probe against the launched one:
//     TestLaunchedInstanceExitedUnconfirmedKeepsLimit, TestReusedPIDDoesNotConfirmRestart;
//     compare it by PID only: TestReusedPIDDoesNotConfirmRestart.
//   - record the launched instance after the probe instead of before it:
//     TestLateReadyLaunchedInstanceIsConfirmedLater,
//     TestOtherFlaggedInstanceDoesNotConfirmRestart,
//     TestInstanceSwappedDuringWaitProbeDoesNotConfirm.
//   - not reset it per restart: TestRepeatedNormalReopensRestartEachOnce.
//
// Dropping the limited gate in confirmAfterProbe fails nothing: it only saves an
// enumeration.

const worldExe = `C:\Kuwo\9.6.0.0_W1\bin\kwmusic.exe`

var errGaveUp = errors.New("gave up waiting for the port")

// world models the single Kuwo process the watchdog sees; cur is nil while Kuwo
// is not running. Every process, opened by the user or by a restart, gets a new
// PID and creation time. The faults marked "launched" apply only to the
// instance the latest restart started.
type world struct {
	cur      *instance
	seq      int
	launched int             // PID of the instance the latest restart started
	exes     map[string]bool // executables that fail verification

	stripFlag  bool  // the launcher drops DebugFlag from what it starts
	portDown   bool  // no port answers, flag or not
	portAlways bool  // launched: a verified bridge answers without the flag
	readyErr   error // launched: ready fails with this
	identErr   error // launched: scans fail with this
	startErr   error
	stopErr    error // the process survives the stop

	onWait  func(*world) // runs on every post-start wait, before it gives up
	onReady func(*world) // runs at the start of every port probe
	onStart func(*world) // runs after start opened the launched instance

	stopped []instance // the instances stop was called on
	starts  int
	waits   int
}

func (w *world) isLaunched() bool { return w.cur != nil && w.cur.PID == w.launched }

func (w *world) open(exe string, args ...string) instance {
	w.seq++
	i := instance{PID: 1000 + w.seq, Exe: exe, Args: args, Created: uint64(50000 + w.seq)}
	w.cur = &i
	return i
}

func (w *world) close() { w.cur = nil }

func (w *world) manager() *manager {
	return &manager{ops: operations{
		scan: func(context.Context) (instance, error) {
			if w.cur == nil {
				return instance{}, ErrNotRunning
			}
			if w.identErr != nil && w.isLaunched() {
				return instance{}, w.identErr
			}
			return *w.cur, nil
		},
		ready: func(context.Context) error {
			if w.onReady != nil {
				w.onReady(w)
			}
			if w.cur == nil || w.portDown {
				return errNotReady
			}
			if w.readyErr != nil && w.isLaunched() {
				return w.readyErr
			}
			if w.portAlways && w.isLaunched() || slices.Contains(w.cur.Args, DebugFlag) {
				return nil
			}
			return errNotReady
		},
		verify: func(exe string) error {
			if w.exes[exe] {
				return ErrVersionUnsupported
			}
			return nil
		},
		stop: func(_ context.Context, i instance) error {
			w.stopped = append(w.stopped, i)
			if w.stopErr != nil {
				return w.stopErr
			}
			w.cur = nil
			return nil
		},
		start: func(exe string, args []string) error {
			w.starts++
			if w.startErr != nil {
				return w.startErr
			}
			if w.stripFlag {
				args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == DebugFlag })
			}
			w.launched = w.open(exe, args...).PID
			if w.onStart != nil {
				w.onStart(w)
			}
			return nil
		},
		// The post-start wait gives up after a few rounds instead of spinning
		// until the real 25 s deadline.
		wait: func(context.Context, time.Duration) error {
			if w.onWait != nil {
				w.onWait(w)
			}
			w.waits++
			if w.waits > 3 {
				return errGaveUp
			}
			return nil
		},
	}}
}

// restartOnce opens Kuwo normally and asserts that one ensure restarts exactly
// that process and leaves the relaunched one running with the flag.
func restartOnce(t *testing.T, w *world, m *manager) {
	t.Helper()
	opened := w.open(worldExe, "/autorun")
	before := len(w.stopped)
	if err := m.ensure(context.Background()); err != nil {
		t.Fatalf("restart of a newly opened instance: %v", err)
	}
	if len(w.stopped) != before+1 || !sameInstance(w.stopped[before], opened) {
		t.Fatalf("stopped %+v, want exactly %+v", w.stopped[before:], opened)
	}
	if !w.isLaunched() || !slices.Contains(w.cur.Args, DebugFlag) {
		t.Fatalf("relaunched instance %+v", w.cur)
	}
}

// The issue's sequence: restart, the user closes Kuwo, opens it again normally.
// The new process is restarted once and collection can go on (ensure returns
// nil with the port ready). Reverting #187 fails it with ErrRestartLimited.
func TestNormallyReopenedKuwoIsRestartedAgain(t *testing.T) {
	w := &world{}
	m := w.manager()
	restartOnce(t, w, m)
	w.close()
	restartOnce(t, w, m)
	// The relaunched instance is ready; collection continues without a stop.
	for range 3 {
		if err := m.ensure(context.Background()); err != nil || len(w.stopped) != 2 {
			t.Fatalf("after the second restart: %v, %d stops", err, len(w.stopped))
		}
	}
}

// Closing and normally reopening many times restarts each new process exactly
// once; repeated ensures in between never stop anything else, so there is no
// loop. Every stopped instance is one the user opened, none is one a restart
// launched.
func TestRepeatedNormalReopensRestartEachOnce(t *testing.T) {
	w := &world{}
	m := w.manager()
	const cycles = 5
	for range cycles {
		restartOnce(t, w, m)
		for range 4 {
			if err := m.ensure(context.Background()); err != nil {
				t.Fatalf("ensure on the relaunched instance: %v", err)
			}
		}
		w.close()
		if err := m.ensure(context.Background()); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("closed: %v", err)
		}
	}
	if len(w.stopped) != cycles || w.starts != cycles {
		t.Fatalf("%d stops, %d starts, want %d each", len(w.stopped), w.starts, cycles)
	}
	seen := map[int]bool{}
	for _, s := range w.stopped {
		if slices.Contains(s.Args, DebugFlag) || seen[s.PID] {
			t.Fatalf("stopped a relaunched or already stopped instance: %+v", w.stopped)
		}
		seen[s.PID] = true
	}
}

// The instance a restart launched is never restarted, even when its port stops
// answering while it keeps running: it carries the flag, so the watchdog waits.
func TestRelaunchedInstanceIsNeverRestarted(t *testing.T) {
	w := &world{}
	m := w.manager()
	restartOnce(t, w, m)
	w.portDown = true
	for range 3 {
		if err := m.ensure(context.Background()); !errors.Is(err, ErrPortNotReady) || len(w.stopped) != 1 {
			t.Fatalf("relaunched instance without a port: %v, %d stops", err, len(w.stopped))
		}
	}
}

// A process the restart could not stop is the same process; it is not stopped a
// second time, and neither is anything opened after it.
func TestSameProcessIsNotRestartedTwice(t *testing.T) {
	w := &world{stopErr: errors.New("access denied")}
	m := w.manager()
	survivor := w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); err == nil || len(w.stopped) != 1 {
		t.Fatalf("stop failure: %v, %d stops", err, len(w.stopped))
	}
	for range 3 {
		if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || len(w.stopped) != 1 {
			t.Fatalf("same process again: %v, %d stops", err, len(w.stopped))
		}
	}
	if !sameInstance(*w.cur, survivor) {
		t.Fatal("the survivor changed")
	}
	w.stopErr = nil
	w.close()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || len(w.stopped) != 1 {
		t.Fatalf("new process after a failed stop: %v, %d stops", err, len(w.stopped))
	}
}

// A first restart that was not confirmed to bring up the debug port blocks every
// later restart: the next normally opened process is reported ErrRestartLimited
// and nothing is stopped. Each case also holds when it happens to a second
// restart, after a confirmed first one.
func TestUnconfirmedRestartBlocksLaterRestarts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*world)
	}{
		// The launcher dropped the flag: the port never comes up.
		{"flag stripped", func(w *world) { w.stripFlag = true }},
		{"start failed", func(w *world) { w.startErr = errors.New("start failed") }},
		{"stop failed", func(w *world) { w.stopErr = errors.New("access denied") }},
		{"port never answers", func(w *world) { w.portDown = true }},
		// The port opened with an unverified bridge: no capture is possible, a
		// further restart would only interrupt playback again.
		{"bridge version rejected", func(w *world) { w.readyErr = cdp.ErrVersionUnsupported }},
		// The port answered, but the running instance does not carry the flag.
		{"port answered without the flag", func(w *world) { w.stripFlag = true; w.portAlways = true }},
		// The port answered, but the running instance could not be identified.
		{"instance not identified", func(w *world) { w.identErr = ErrAmbiguous }},
	} {
		for _, confirmedFirst := range []bool{false, true} {
			name := tc.name
			if confirmedFirst {
				name += " on the second restart"
			}
			t.Run(name, func(t *testing.T) {
				w := &world{}
				m := w.manager()
				if confirmedFirst {
					restartOnce(t, w, m)
					w.close()
				}
				tc.setup(w)
				w.open(worldExe, "/autorun")
				_ = m.ensure(context.Background())
				stops, starts := len(w.stopped), w.starts
				// Restore a healthy world: only the earlier failure may block now.
				*w = world{seq: w.seq, stopped: w.stopped, starts: w.starts}
				w.open(worldExe, "/autorun")
				for range 3 {
					if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) {
						t.Fatalf("later normal open: %v", err)
					}
				}
				if len(w.stopped) != stops || w.starts != starts {
					t.Fatalf("restarted after an unconfirmed restart: %d stops, %d starts", len(w.stopped), w.starts)
				}
			})
		}
	}
}

// After a confirmed restart, an unverified build opened later is not restarted:
// the allowlist still applies to every process.
func TestUnverifiedBuildIsNotRestartedAfterConfirmedRestart(t *testing.T) {
	w := &world{exes: map[string]bool{`C:\Kuwo\9.6.0.0_X9\bin\kwmusic.exe`: true}}
	m := w.manager()
	restartOnce(t, w, m)
	w.close()
	w.open(`C:\Kuwo\9.6.0.0_X9\bin\kwmusic.exe`, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartNotAuthorized) || len(w.stopped) != 1 || w.starts != 1 {
		t.Fatalf("unverified build: %v, %d stops, %d starts", err, len(w.stopped), w.starts)
	}
	// It also does not use up anything: the next verified process is restarted.
	w.close()
	restartOnce(t, w, m)
}

// After a confirmed restart, a Kuwo the user launched with the flag is waited
// for, not restarted, while its port is still coming up.
func TestUserLaunchedWithFlagIsNotRestarted(t *testing.T) {
	w := &world{}
	m := w.manager()
	restartOnce(t, w, m)
	w.close()
	w.open(worldExe, "/autorun", DebugFlag)
	w.portDown = true
	if err := m.ensure(context.Background()); !errors.Is(err, ErrPortNotReady) || len(w.stopped) != 1 {
		t.Fatalf("user launched with the flag: %v, %d stops", err, len(w.stopped))
	}
	w.portDown = false
	if err := m.ensure(context.Background()); err != nil || len(w.stopped) != 1 {
		t.Fatalf("user launched with the flag, port up: %v, %d stops", err, len(w.stopped))
	}
}

// The relaunched instance carries the flag but its port only answers after the
// restart stopped waiting. The next ensure that finds that same instance with
// the port up confirms the restart, so a later normally opened Kuwo is restarted
// once (#187, review 187-late-ready).
func TestLateReadyLaunchedInstanceIsConfirmedLater(t *testing.T) {
	w := &world{portDown: true}
	m := w.manager()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, errGaveUp) || len(w.stopped) != 1 {
		t.Fatalf("restart with a slow port: %v, %d stops", err, len(w.stopped))
	}
	launched := *w.cur
	w.portDown = false
	if err := m.ensure(context.Background()); err != nil || !sameInstance(*w.cur, launched) {
		t.Fatalf("launched instance, port up: %v", err)
	}
	w.close()
	restartOnce(t, w, m)
}

// The restart launched A without the flag; while it waited, A was closed and the
// user started B with the flag, whose port answers. B is used for capture but
// is not the launched instance, so it does not confirm the restart: after B, a
// normally opened C is not restarted (#187, review 187-instance-confirmation).
func TestOtherFlaggedInstanceDoesNotConfirmRestart(t *testing.T) {
	var b instance
	w := &world{stripFlag: true}
	w.onWait = func(w *world) {
		if b.PID == 0 {
			w.close()
			b = w.open(worldExe, "/autorun", DebugFlag)
		}
	}
	m := w.manager()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); err != nil || !sameInstance(*w.cur, b) {
		t.Fatalf("user's flagged instance during the wait: %v, running %+v", err, w.cur)
	}
	w.close()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || len(w.stopped) != 1 {
		t.Fatalf("normal open after an unconfirmed restart: %v, %d stops", err, len(w.stopped))
	}
}

// The launched instance exited before its port ever answered. Nothing after it
// can confirm the restart, not even a flagged instance the user starts with a
// working port; later normally opened instances get ErrRestartLimited.
func TestLaunchedInstanceExitedUnconfirmedKeepsLimit(t *testing.T) {
	w := &world{portDown: true}
	m := w.manager()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, errGaveUp) {
		t.Fatalf("restart with a dead port: %v", err)
	}
	w.close()
	w.portDown = false
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || len(w.stopped) != 1 {
		t.Fatalf("normal open: %v, %d stops", err, len(w.stopped))
	}
	w.close()
	w.open(worldExe, "/autorun", DebugFlag)
	if err := m.ensure(context.Background()); err != nil {
		t.Fatalf("user's flagged instance: %v", err)
	}
	w.close()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || len(w.stopped) != 1 {
		t.Fatalf("normal open after the user's instance: %v, %d stops", err, len(w.stopped))
	}
}

// A process reusing the launched instance's PID, with another creation time, is
// not the launched instance: even with the flag and a working port it does not
// confirm the restart.
func TestReusedPIDDoesNotConfirmRestart(t *testing.T) {
	w := &world{portDown: true}
	m := w.manager()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, errGaveUp) {
		t.Fatalf("restart with a dead port: %v", err)
	}
	launched := *w.cur
	w.close()
	w.portDown = false
	w.cur = &instance{PID: launched.PID, Exe: launched.Exe, Args: slices.Clone(launched.Args), Created: launched.Created + 1}
	if err := m.ensure(context.Background()); err != nil || len(w.stopped) != 1 {
		t.Fatalf("PID reused with the flag: %v, %d stops", err, len(w.stopped))
	}
	w.close()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || len(w.stopped) != 1 {
		t.Fatalf("normal open after a reused PID: %v, %d stops", err, len(w.stopped))
	}
}

// The launched instance A is recorded and its port is late. A later ensure finds
// A, but while the port is probed A exits and the user starts B with the flag,
// whose port answers. The probe succeeded for B, not A: the restart is not
// confirmed, and a normally opened C afterwards is not restarted (review: the
// later confirmation used the snapshot taken before the probe).
func TestInstanceSwappedDuringLaterProbeDoesNotConfirm(t *testing.T) {
	w := &world{portDown: true}
	m := w.manager()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, errGaveUp) {
		t.Fatalf("restart with a slow port: %v", err)
	}
	launched := *w.cur
	var b instance
	w.onReady = func(w *world) {
		if b.PID == 0 {
			w.close()
			b = w.open(worldExe, "/autorun", DebugFlag)
			w.portDown = false
		}
	}
	if err := m.ensure(context.Background()); err != nil || !sameInstance(*w.cur, b) || sameInstance(b, launched) {
		t.Fatalf("probe answered by the user's instance: %v, running %+v", err, w.cur)
	}
	w.onReady = nil
	w.close()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || len(w.stopped) != 1 {
		t.Fatalf("normal open after the swap: %v, %d stops", err, len(w.stopped))
	}
}

// The same swap inside the restart's own wait: the launched instance A has been
// seen, then during a probe A exits and the user's flagged B answers. Not a
// confirmation either.
func TestInstanceSwappedDuringWaitProbeDoesNotConfirm(t *testing.T) {
	w := &world{portDown: true}
	m := w.manager()
	probes := 0
	var b instance
	w.onReady = func(w *world) {
		// Probe 1 is ensure's own, before the restart; probe 2 is the wait's
		// first, after the launched instance has been enumerated.
		if probes++; probes == 2 {
			w.close()
			b = w.open(worldExe, "/autorun", DebugFlag)
			w.portDown = false
		}
	}
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); err != nil || b.PID == 0 || !sameInstance(*w.cur, b) {
		t.Fatalf("probe answered by the user's instance: %v, running %+v", err, w.cur)
	}
	w.onReady = nil
	w.close()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || len(w.stopped) != 1 {
		t.Fatalf("normal open after the swap: %v, %d stops", err, len(w.stopped))
	}
}

// The accepted limit of taking the first instance seen after the start as the
// launched one: the launcher drops the flag, the launched A exits before it is
// first enumerated, and the user has just started B with the flag. B is taken
// as launched and confirms the restart. The cost is bounded: the next normally
// opened instance is restarted once more, that restart launches an instance
// without the flag again, the limit stays, and nothing after it is restarted.
func TestMisidentifiedLaunchCostsAtMostOneMoreRestart(t *testing.T) {
	w := &world{stripFlag: true}
	w.onStart = func(w *world) {
		w.onStart = nil
		w.close()
		w.open(worldExe, "/autorun", DebugFlag)
	}
	m := w.manager()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); err != nil {
		t.Fatalf("misidentified restart: %v", err)
	}
	w.close()
	w.open(worldExe, "/autorun")
	if err := m.ensure(context.Background()); !errors.Is(err, errGaveUp) || len(w.stopped) != 2 {
		t.Fatalf("the one extra restart: %v, %d stops", err, len(w.stopped))
	}
	for range 5 {
		w.close()
		w.open(worldExe, "/autorun")
		if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) {
			t.Fatalf("normal open after the extra restart: %v", err)
		}
	}
	if len(w.stopped) != 2 || w.starts != 2 {
		t.Fatalf("%d stops, %d starts, want 2 each", len(w.stopped), w.starts)
	}
}
