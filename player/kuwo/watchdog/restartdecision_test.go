package watchdog

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/launch"
)

var errNotReady = errors.New("not ready")

type fakeOps struct {
	i                                     instance
	scanErr, verifyErr, stopErr, startErr error
	readyErr                              error // nil once started; error otherwise
	ready                                 bool
	scans, verifies, stops, starts        int
	args                                  []string
	afterScan                             func(*fakeOps)
	afterStop                             func()
}

func newFake() *fakeOps {
	return &fakeOps{i: instance{PID: 123, Exe: `C:\Kuwo\9.6.0.0_W1\bin\kwmusic.exe`, Args: []string{"/autorun", "a b"}, Created: 456}, readyErr: errNotReady}
}

func (f *fakeOps) manager() *manager {
	return &manager{ops: operations{
		scan: func(context.Context) (instance, error) {
			f.scans++
			if f.afterScan != nil {
				f.afterScan(f)
			}
			if f.scanErr != nil {
				return instance{}, f.scanErr
			}
			if f.stops > 0 {
				if f.starts > 0 {
					// The instance the restart launched.
					return instance{PID: f.i.PID + 1, Exe: f.i.Exe, Args: f.args, Created: f.i.Created + 1}, nil
				}
				return instance{}, ErrNotRunning
			}
			return f.i, nil
		},
		ready: func(context.Context) error {
			if f.ready || f.starts > 0 {
				return nil
			}
			return f.readyErr
		},
		verify: func(string) error { f.verifies++; return f.verifyErr },
		stop: func(context.Context, instance) error {
			f.stops++
			if f.afterStop != nil {
				f.afterStop()
			}
			return f.stopErr
		},
		start: func(_ string, args []string) error {
			f.starts++
			f.args = append([]string(nil), args...)
			return f.startErr
		},
		wait: func(context.Context, time.Duration) error { return nil },
	}}
}

func TestRestartRetainsArgumentsAndStopsOnlyOnce(t *testing.T) {
	f := newFake()
	m := f.manager()
	if err := m.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.stops != 1 || f.starts != 1 || f.verifies != 2 || !reflect.DeepEqual(f.args, []string{"/autorun", "a b", DebugFlag}) {
		t.Fatalf("unexpected operations %+v", f)
	}
	// The relaunched instance is left alone. Newly opened instances without the
	// flag are covered in restartperprocess_test.go.
	if err := m.ensure(context.Background()); err != nil || f.stops != 1 || f.starts != 1 {
		t.Fatalf("second ensure: %v %+v", err, f)
	}
}

func TestRestartFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeOps)
		want  error
	}{
		{"not running", func(f *fakeOps) { f.scanErr = ErrNotRunning }, ErrNotRunning},
		{"ambiguous", func(f *fakeOps) { f.scanErr = ErrAmbiguous }, ErrAmbiguous},
		// An unverified build is reported as not-authorized (manual launch), not
		// as a version rejection; no stop/start is attempted.
		{"unverified build", func(f *fakeOps) { f.verifyErr = ErrVersionUnsupported }, ErrRestartNotAuthorized},
		{"already debug", func(f *fakeOps) { f.i.Args = []string{DebugFlag} }, ErrPortNotReady},
		{"foreign debug port", func(f *fakeOps) { f.i.Args = []string{"--remote-debugging-port=9222"} }, ErrPortNotReady},
		{"pid reused", func(f *fakeOps) {
			f.afterScan = func(f *fakeOps) {
				if f.scans == 2 {
					f.i.Created++
				}
			}
		}, ErrAmbiguous},
		{"changed path", func(f *fakeOps) {
			f.afterScan = func(f *fakeOps) {
				if f.scans == 2 {
					f.i.Exe = `C:\other\kwmusic.exe`
				}
			}
		}, ErrAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			tc.setup(f)
			err := f.manager().ensure(context.Background())
			if !errors.Is(err, tc.want) || f.stops != 0 || f.starts != 0 {
				t.Fatalf("err=%v stops=%d starts=%d", err, f.stops, f.starts)
			}
		})
	}
}

// TestStoreInstallGate 钉住商店版门禁（#212）：映像路径在 WindowsApps 下或为空时返回门禁的错误，不 stop、不 start，
// 且先于 ready 的提前返回（端口已就绪时同样被拒，上层才给得出改用官网安装版的提示）。桩的 verify 一律放行，
// 拦住它们的只有门禁。
//
// 变异自证（均实测变红）：去掉门禁；门禁挪到 ready 之后（端口已就绪的两例）；门禁挪到 stop 之后。
func TestStoreInstallGate(t *testing.T) {
	const store = `C:\Program Files\WindowsApps\Kuwo.KuwoMusic_9.6.0.0_x64__abc\9.6.0.0_W1\bin\kwmusic.exe`
	for _, tc := range []struct {
		name     string
		exe      string
		ready    bool
		packaged bool // 期望 *launch.PackagedError；否则期望 launch.ErrExeUnknown
	}{
		{"商店版、端口未就绪", store, false, true},
		{"商店版、端口已就绪", store, true, true},
		{"路径为空、端口未就绪", "", false, false},
		{"路径为空、端口已就绪", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.i.Exe = tc.exe
			f.ready = tc.ready
			err := f.manager().ensure(context.Background())
			var pe *launch.PackagedError
			if tc.packaged {
				if !errors.As(err, &pe) || pe.ExePath != store {
					t.Errorf("err = %v，期望 *launch.PackagedError（%s）", err, store)
				}
			} else if !errors.Is(err, launch.ErrExeUnknown) {
				t.Errorf("err = %v，期望 launch.ErrExeUnknown", err)
			}
			if f.stops != 0 || f.starts != 0 || f.verifies != 0 {
				t.Errorf("stops=%d starts=%d verifies=%d，期望门禁之后什么都不做", f.stops, f.starts, f.verifies)
			}
		})
	}
}

func TestReadyBridgeNeverRestarts(t *testing.T) {
	f := newFake()
	f.ready = true
	f.verifyErr = ErrVersionUnsupported
	if err := f.manager().ensure(context.Background()); err != nil || f.stops != 0 || f.verifies != 0 {
		t.Fatalf("ready: %+v %v", f, err)
	}
}

func TestOccupiedPortNeverRestarts(t *testing.T) {
	f := newFake()
	m := f.manager()
	m.ops.occupied = func(context.Context) bool { return true }
	if err := m.ensure(context.Background()); !errors.Is(err, ErrPortNotReady) || f.verifies != 0 || f.stops != 0 {
		t.Fatalf("occupied port: %v %+v", err, f)
	}
}

func TestRestartRechecksNoOtherInstanceAndBinary(t *testing.T) {
	f := newFake()
	m := f.manager()
	m.ops.scan = func(context.Context) (instance, error) { f.scans++; return f.i, nil }
	if err := m.ensure(context.Background()); !errors.Is(err, ErrAmbiguous) || f.starts != 0 {
		t.Fatalf("new instance launched over: %v %+v", err, f)
	}
	f = newFake()
	m = f.manager()
	m.ops.verify = func(string) error {
		f.verifies++
		if f.verifies == 2 {
			return ErrVersionUnsupported
		}
		return nil
	}
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartNotAuthorized) || f.starts != 0 {
		t.Fatalf("updated binary launched: %v %+v", err, f)
	}
}

// A bridge that answers with an unverified build surfaces the version rejection
// even while the port is occupied; it must not be masked as cdp_disconnected,
// and no verify/stop/start is attempted.
func TestVersionUnsupportedBridgeNeverRestarts(t *testing.T) {
	f := newFake()
	f.readyErr = cdp.ErrVersionUnsupported
	m := f.manager()
	m.ops.occupied = func(context.Context) bool { return true }
	if err := m.ensure(context.Background()); !errors.Is(err, cdp.ErrVersionUnsupported) || f.verifies != 0 || f.stops != 0 || f.starts != 0 {
		t.Fatalf("version bridge: %v %+v", err, f)
	}
}

// Unknown build with the port closed: the user is told to launch manually, and
// no destructive action is taken.
func TestUnknownBuildReportsManualLaunch(t *testing.T) {
	f := newFake()
	f.verifyErr = ErrVersionUnsupported
	if err := f.manager().ensure(context.Background()); !errors.Is(err, ErrRestartNotAuthorized) || f.verifies != 1 || f.stops != 0 || f.starts != 0 {
		t.Fatalf("unknown build: %v %+v", err, f)
	}
}

func TestStartFailureConsumesRestartBudget(t *testing.T) {
	f := newFake()
	f.startErr = errors.New("start failed")
	m := f.manager()
	if err := m.ensure(context.Background()); err == nil {
		t.Fatal("start failure ignored")
	}
	f.stops = 0
	f.starts = 0
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || f.stops != 0 || f.starts != 0 {
		t.Fatalf("retry: %v %+v", err, f)
	}
}

func TestCancellationAndStopFailureNeverLaunch(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.manager().ensure(ctx); !errors.Is(err, context.Canceled) || f.scans != 0 {
		t.Fatalf("pre-cancel: %v %+v", err, f)
	}
	f = newFake()
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	f.afterStop = cancel
	if err := f.manager().ensure(ctx); !errors.Is(err, context.Canceled) || f.starts != 0 {
		t.Fatalf("post-stop cancel: %v %+v", err, f)
	}
	f = newFake()
	f.stopErr = errors.New("cannot stop")
	m := f.manager()
	if err := m.ensure(context.Background()); err == nil || f.starts != 0 {
		t.Fatalf("stop error: %v %+v", err, f)
	}
	f.stops = 0
	if err := m.ensure(context.Background()); !errors.Is(err, ErrRestartLimited) || f.stops != 0 {
		t.Fatalf("retry after failure: %v %+v", err, f)
	}
}
