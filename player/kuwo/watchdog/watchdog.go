// Package watchdog enables the tested Kuwo debug flag by restarting a verified
// running instance, at most once per process: a restart replaces the process
// with one carrying the flag, which is never restarted. Until the instance a
// restart launched is confirmed running with the flag and an answering debug
// port, no further restart is made; if it exits unconfirmed, none is made in
// this service lifetime. It does not install, patch, modify autostart, launch
// an absent player, or kill by executable name.
package watchdog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
)

const TargetProcessName = "kwmusic.exe"
const DebugPort = cdp.DebugPort
const DebugFlag = "--remote-debugging-port=9224"

const buildDirPrefix = "9.6.0.0_"

// knownBuilds maps a channel suffix (lower case) to the SHA256 of that build's
// main EXE. The restart decision is bound to one of these exact binaries, not a
// launcher sharing the filename or a directory layout alone. Adding a build is
// one line here, documented with its provenance in README and §14. It is a var
// so tests can substitute fixtures; production never mutates it.
var knownBuilds = map[string]string{
	"w1":  "43d2d98255b872178521a75b2025bf1d99a47d71eadf92fb1599e2e7cef166fd",
	"ug1": "c18942ac35180b27eb99372fd8b27c26dd76bae38015263189b4380cfac0ac6f",
}

var (
	ErrNotRunning         = i18n.Sentinel("Kuwo is not running")
	ErrVersionUnsupported = i18n.Sentinel("Kuwo version or executable is not verified")
	ErrPortNotReady       = i18n.Sentinel("Kuwo debug port is not ready yet")
	ErrAmbiguous          = i18n.Sentinel("Kuwo process identity is ambiguous")
	ErrRestartLimited     = i18n.Sentinel("The Kuwo instance of an earlier automatic restart has not been confirmed to have opened its debug port; no automatic restart until it is")
	// ErrRestartNotAuthorized means the running build is not in knownBuilds, so
	// it is not eligible for a destructive auto-restart. The user must launch it
	// with the debug port themselves; the read path still works once it is open.
	ErrRestartNotAuthorized = i18n.Sentinel("This Kuwo build is not on the verified allowlist; no automatic restart")
)

var log = logger.New("Kuwo] [Watchdog")

type instance struct {
	PID     int
	Exe     string
	Args    []string // excludes executable; decoded by CommandLineToArgvW
	Created uint64   // exact process creation FILETIME; PID reuse must not pass
}

type operations struct {
	scan     func(context.Context) (instance, error)
	ready    func(context.Context) error // nil = ready; cdp.ErrVersionUnsupported surfaces
	occupied func(context.Context) bool
	verify   func(string) error
	stop     func(context.Context, instance) error // identity check and wait on SAME handle
	start    func(string, []string) error
	wait     func(context.Context, time.Duration) error
}

type manager struct {
	mu sync.Mutex
	// limited is set when a restart begins and cleared only by confirmAfterProbe.
	// While it is set no restart is made: a launcher that drops the flag would
	// otherwise have every newly opened instance restarted in turn (#187).
	limited bool
	// launched is the first instance enumerated after the latest restart's
	// start, zero until then. It is presumed to be the one start launched:
	// startExact goes through cmd.exe and yields no PID. The presumption fails
	// only if, between start and that first successful enumeration (normally the
	// wait's first round, then every 250 ms until Kuwo appears), the launched
	// instance exits and another one is opened. If that other one carries the
	// flag it confirms the restart wrongly; the cost is one more restart of the
	// next normally opened instance at most, and if the launcher still drops
	// the flag, that restart's instance is unconfirmed and the limit stays: no
	// loop.
	launched instance
	ops      operations
}

// confirmAfterProbe runs after the port answered. before is the instance found
// before that probe. limited is cleared only when before is the launched
// instance and a fresh enumeration after the probe finds the same instance
// (sameInstance: PID, creation time, path and arguments) carrying DebugFlag; an
// instance that changed around the probe never confirms, since the port may
// have answered for the other one. It runs in the restart's own wait and in
// every later ensure, so a port that answers only after that wait still
// confirms. Any other instance, flagged or not, never does; once launched has
// exited unconfirmed, limited stays set for this service lifetime.
func (m *manager) confirmAfterProbe(ctx context.Context, before instance) {
	if !m.limited || !sameInstance(before, m.launched) {
		return
	}
	if cur, err := m.ops.scan(ctx); err == nil && sameInstance(cur, before) && slices.Contains(cur.Args, DebugFlag) {
		m.limited = false
	}
}

var defaultManager = manager{ops: operations{scan: scanProcesses, ready: debugReady, occupied: debugPortOccupied, verify: verifyExecutable, stop: stopExact, start: startExact, wait: waitContext}}

func FindMainPID() (int, error) {
	i, err := scanProcesses(context.Background())
	return i.PID, err
}

// EnsureDebugMode restarts each process at most once. Until a restart is
// confirmed (see confirmAfterProbe), including after a start failure, it makes
// no further restart. A ready bridge needs no restart.
func EnsureDebugMode(ctx context.Context) error { return defaultManager.ensure(ctx) }

func (m *manager) ensure(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	i, err := m.ops.scan(ctx)
	if err != nil {
		return err
	}
	if err := m.ops.ready(ctx); err == nil {
		m.confirmAfterProbe(ctx, i)
		return nil
	} else if errors.Is(err, cdp.ErrVersionUnsupported) {
		// The bridge answered but its build is not verified: report the version
		// rejection, do not mistake an open-but-unsupported port for "occupied".
		return err
	}
	// A foreign listener, or a not-yet-ready CEF endpoint, must not cause an
	// otherwise futile interruption of the running player.
	if m.ops.occupied != nil && m.ops.occupied(ctx) {
		return ErrPortNotReady
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, arg := range i.Args {
		if arg == DebugFlag {
			return ErrPortNotReady
		}
	}
	if m.limited {
		return ErrRestartLimited
	}
	// An unverified build is not eligible for a destructive restart: report it
	// as such so the user is told to launch with the debug port manually.
	if err = m.ops.verify(i.Exe); err != nil {
		return ErrRestartNotAuthorized
	}
	args, err := debugArgs(i.Args)
	if err != nil {
		return err
	}
	// Re-enumerate immediately before termination: an additional instance,
	// changed arguments, executable, or PID generation invalidates the plan.
	fresh, err := m.ops.scan(ctx)
	if err != nil {
		return err
	}
	if !sameInstance(i, fresh) {
		return ErrAmbiguous
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.limited, m.launched = true, instance{}
	log.Warn("Kuwo needs its debug port; restarting the verified instance once (playback may pause)")
	if err = m.ops.stop(ctx, i); err != nil {
		return i18n.Errorf("kuwo: controlled restart failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = m.ops.scan(ctx)
	if !errors.Is(err, ErrNotRunning) {
		if err != nil {
			return err
		}
		return ErrAmbiguous
	}
	// Installation could have updated while shutting down; never start a
	// different executable under the authority of the earlier fingerprint.
	if err = m.ops.verify(i.Exe); err != nil {
		return ErrRestartNotAuthorized
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = m.ops.start(i.Exe, args); err != nil {
		return i18n.Errorf("kuwo: controlled restart failed: %w", err)
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Enumerate before each probe until the launched instance is seen, so a
		// probe that succeeds can be checked against it (#187).
		if m.launched.PID == 0 {
			if cur, err := m.ops.scan(ctx); err == nil {
				m.launched = cur
			}
		}
		if err := m.ops.ready(ctx); err == nil {
			m.confirmAfterProbe(ctx, m.launched)
			return nil
		} else if errors.Is(err, cdp.ErrVersionUnsupported) {
			return err
		}
		if err = m.ops.wait(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	return ErrPortNotReady
}

func sameInstance(a, b instance) bool {
	if a.PID <= 0 || a.Created == 0 || a.PID != b.PID || a.Created != b.Created || !strings.EqualFold(filepath.Clean(a.Exe), filepath.Clean(b.Exe)) || len(a.Args) != len(b.Args) {
		return false
	}
	for n := range a.Args {
		if a.Args[n] != b.Args[n] {
			return false
		}
	}
	return true
}

func debugArgs(existing []string) ([]string, error) {
	args := append([]string(nil), existing...)
	for _, a := range args {
		// Do not silently steal a debugger from another tool/port, or rewrite
		// ambiguous split/value forms. Existing non-debug parameters survive.
		if strings.HasPrefix(a, "--remote-debugging-port") {
			return nil, ErrPortNotReady
		}
	}
	return append(args, DebugFlag), nil
}

// verifyExecutable passes only for a known build: the path is absolute, the
// filename/parent/grandparent are kwmusic.exe/bin/9.6.0.0_<s> (Windows
// case-insensitive), <s> (lower case) is in knownBuilds, and the file's SHA256
// equals that entry's hash. Everything else is ErrVersionUnsupported.
func verifyExecutable(exe string) error {
	// filepath.Clean collapses ".." lexically, which would let a path that merely
	// reduces to a valid layout through; refuse any ".." component outright.
	for _, part := range strings.FieldsFunc(exe, func(r rune) bool { return r == '\\' || r == '/' }) {
		if part == ".." {
			return ErrVersionUnsupported
		}
	}
	clean := filepath.Clean(exe)
	if !filepath.IsAbs(clean) || !strings.EqualFold(filepath.Base(clean), TargetProcessName) || !strings.EqualFold(filepath.Base(filepath.Dir(clean)), "bin") {
		return ErrVersionUnsupported
	}
	suffix, ok := buildSuffix(filepath.Base(filepath.Dir(filepath.Dir(clean))))
	if !ok {
		return ErrVersionUnsupported
	}
	want, ok := knownBuilds[suffix]
	if !ok {
		return ErrVersionUnsupported
	}
	f, err := os.Open(clean)
	if err != nil {
		return ErrVersionUnsupported
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return ErrVersionUnsupported
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return ErrVersionUnsupported
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return ErrVersionUnsupported
	}
	return nil
}

// buildSuffix extracts the lower-case channel suffix of a 9.6.0.0_<s> directory
// name, matching the prefix case-insensitively.
func buildSuffix(dir string) (string, bool) {
	if len(dir) <= len(buildDirPrefix) || !strings.EqualFold(dir[:len(buildDirPrefix)], buildDirPrefix) {
		return "", false
	}
	return strings.ToLower(dir[len(buildDirPrefix):]), true
}

// debugReady returns nil when a verified bridge is reachable, cdp.ErrVersion
// Unsupported when the port answers with an unverified build, or the connection
// error otherwise.
func debugReady(ctx context.Context) error {
	c, err := cdp.Connect(ctx)
	if err != nil {
		return err
	}
	_ = c.Close()
	return nil
}

func debugPortOccupied(ctx context.Context) bool {
	d := net.Dialer{Timeout: 250 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", "127.0.0.1:9224")
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func waitContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
