package watchdog

// 本文件只测酷狗的商店版门禁（issue #212）：gateKuGou 逐个核验全部运行中的 KuGou.exe；killAndPatch 结束酷狗之前
// 再过一次门禁；EnsurePatched 在一切操作之前先过门禁。进程快照、映像路径、kill 与 trigger 一律是桩，不碰真进程。

import (
	"errors"
	"testing"

	"Metabox-Nexus-PlayerCap/player/launch"
)

const (
	officialKuGou = `C:\Program Files\KuGou\KGMusic\KuGou.exe`
	storeKuGou    = `C:\Program Files\WindowsApps\KuGou.KuGouMusic_20.1.22.0_x64__abc\KuGou.exe`
)

// stubGateRunning 把 gateRunningFn 换成返回 err 的桩，返回它被调用的次数。
func stubGateRunning(t *testing.T, err error) *int {
	t.Helper()
	old := gateRunningFn
	t.Cleanup(func() { gateRunningFn = old })
	calls := new(int)
	gateRunningFn = func() error { *calls++; return err }
	return calls
}

// TestGateKuGou：主进程与 CEF 子进程逐个核验，任一在 WindowsApps 下或读不到路径即拒绝；快照取不到按路径未知拒绝；
// 别的进程不管。
//
// 防的缺陷与变异（均实测变红）：只核验主进程（父进程是同名进程的跳过）；跳过读不到路径的进程；映像名区分大小写；
// 快照出错时放行；不看映像名、对全部进程过门禁（「别的进程在 WindowsApps 下」被拒）。
func TestGateKuGou(t *testing.T) {
	procs := func(ps ...proc) func() ([]proc, error) { return func() ([]proc, error) { return ps, nil } }
	paths := func(m map[uint32]string) func(uint32) (string, error) {
		return func(pid uint32) (string, error) {
			if p, ok := m[pid]; ok {
				return p, nil
			}
			return "", errors.New("access denied")
		}
	}
	main := proc{PID: 10, PPID: 1, Name: "KuGou.exe"}
	child := proc{PID: 11, PPID: 10, Name: "KuGou.exe"}
	other := proc{PID: 20, PPID: 1, Name: "Calculator.exe"}
	cases := []struct {
		name     string
		snapshot func() ([]proc, error)
		image    func(uint32) (string, error)
		want     string // "ok" / "packaged" / "unknown"
	}{
		{"没有在运行的", procs(other), paths(map[uint32]string{20: `C:\Program Files\WindowsApps\Calc\Calculator.exe`}), "ok"},
		{"主进程与子进程都是官网版", procs(main, child), paths(map[uint32]string{10: officialKuGou, 11: officialKuGou}), "ok"},
		{"别的进程在 WindowsApps 下不管", procs(main, other), paths(map[uint32]string{10: officialKuGou, 20: `C:\Program Files\WindowsApps\Calc\Calculator.exe`}), "ok"},
		{"主进程是商店版", procs(main, child), paths(map[uint32]string{10: storeKuGou, 11: officialKuGou}), "packaged"},
		{"子进程是商店版", procs(main, child), paths(map[uint32]string{10: officialKuGou, 11: storeKuGou}), "packaged"},
		{"另一个主进程是商店版", procs(main, proc{PID: 30, PPID: 2, Name: "KuGou.exe"}), paths(map[uint32]string{10: officialKuGou, 30: storeKuGou}), "packaged"},
		{"映像名大小写不同的商店版", procs(main, proc{PID: 31, PPID: 2, Name: "kugou.EXE"}), paths(map[uint32]string{10: officialKuGou, 31: storeKuGou}), "packaged"},
		{"子进程读不到路径", procs(main, child), paths(map[uint32]string{10: officialKuGou}), "unknown"},
		{"主进程读不到路径", procs(main, child), paths(map[uint32]string{11: officialKuGou}), "unknown"},
		{"快照取不到", func() ([]proc, error) { return nil, errors.New("snapshot failed") }, paths(nil), "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := gateKuGou(c.snapshot, c.image)
			var pe *launch.PackagedError
			switch c.want {
			case "ok":
				if err != nil {
					t.Errorf("gateKuGou = %v，期望 nil", err)
				}
			case "packaged":
				if !errors.As(err, &pe) || pe.ExePath != storeKuGou {
					t.Errorf("gateKuGou = %v，期望 *PackagedError（%s）", err, storeKuGou)
				}
			case "unknown":
				if !errors.Is(err, launch.ErrExeUnknown) {
					t.Errorf("gateKuGou = %v，期望 launch.ErrExeUnknown", err)
				}
			}
		})
	}
}

// TestKillAndPatchGateRefuses：重新定位通过之后、结束酷狗之前再过一次门禁，不过即不结束、不触发 helper，原样返回
// 门禁的错误（不包装成 ErrInstallChanged，上层据此给出改用官网安装版的提示）。门禁通过时照常结束并触发，是对照。
//
// 防的缺陷与变异（均实测变红）：去掉 killAndPatch 里的门禁；门禁挪到 kill 之后。
func TestKillAndPatchGateRefuses(t *testing.T) {
	const lc = `C:\Program Files\KuGou\KGMusic\20.1.22.27795\libcef.dll`
	locate := func() (string, string, error) { return officialKuGou, lc, nil }
	cases := []struct {
		name      string
		gateErr   error
		wantCalls []string
	}{
		{"门禁通过：照常结束并触发", nil, []string{"kill", "trigger"}},
		{"运行中另有商店版", &launch.PackagedError{ExePath: storeKuGou}, nil},
		{"运行中另有读不到路径的", launch.ErrExeUnknown, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gates := stubGateRunning(t, c.gateErr)
			var calls []string
			err := killAndPatch(officialKuGou, lc, locate,
				func() { calls = append(calls, "kill") },
				func() error { calls = append(calls, "trigger"); return nil })
			if *gates != 1 || len(calls) != len(c.wantCalls) {
				t.Fatalf("门禁调用 %d 次，调用序列 %v；期望门禁 1 次、%v", *gates, calls, c.wantCalls)
			}
			if !errors.Is(err, c.gateErr) || (c.gateErr == nil) != (err == nil) {
				t.Errorf("err = %v，期望 %v", err, c.gateErr)
			}
			if c.gateErr != nil && errors.Is(err, ErrInstallChanged) {
				t.Errorf("门禁的错误不得包装成 ErrInstallChanged：%v", err)
			}
		})
	}
}

// TestAwaitPatchedKuGouGate：DLL 已修补、只等酷狗启动时，等到之后对全部运行中的 KuGou.exe 再过一次门禁，等来的是商店版
// 或读不到路径的实例时原样返回门禁的错误（EnsurePatched 随即返回它）。门禁的桩只在等到酷狗之后才报错，模拟「开头核验
// 时酷狗还没运行、等待期间用户启动了商店版」。等待被停止时不核验。门禁通过时返回 nil，是对照。
//
// 防的缺陷与变异（均实测变红）：等到之后不过门禁（改回直接返回 nil）；门禁挪到等待之前（只核验开头那一刻）。
func TestAwaitPatchedKuGouGate(t *testing.T) {
	store := &launch.PackagedError{ExePath: storeKuGou}
	cases := []struct {
		name     string
		waited   kugouWait
		gateErr  error // 等到酷狗之后门禁返回的错误
		wantGate int
	}{
		{"等到酷狗、门禁通过", kugouStarted, nil, 1},
		{"等来的是商店版", kugouStarted, store, 1},
		{"等来的读不到路径", kugouStarted, launch.ErrExeUnknown, 1},
		{"等待被停止", kugouWaitStopped, store, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			started := false
			old := gateRunningFn
			t.Cleanup(func() { gateRunningFn = old })
			gates := 0
			gateRunningFn = func() error {
				gates++
				if started {
					return c.gateErr
				}
				return nil
			}
			err := awaitPatchedKuGou(func() kugouWait { started = true; return c.waited })
			if gates != c.wantGate {
				t.Errorf("门禁调用 %d 次，期望 %d", gates, c.wantGate)
			}
			switch {
			case c.waited == kugouWaitStopped:
				if err == nil || errors.Is(err, store) {
					t.Errorf("err = %v，期望「stopped」", err)
				}
			case err != c.gateErr:
				t.Errorf("err = %v，期望原样返回 %v", err, c.gateErr)
			}
		})
	}
}

// TestEnsurePatchedGateFirst：门禁不过时 EnsurePatched 原样返回它的错误，先于「CDP 已可用」的探测与一切后续操作。
// 门禁桩返回错误时 EnsurePatched 立即返回，不探测本机的 12233、不找安装。
//
// 防的缺陷与变异（实测变红）：去掉开头的门禁（随后探测 12233、找安装，返回的不是门禁的错误）。
func TestEnsurePatchedGateFirst(t *testing.T) {
	for _, gateErr := range []error{&launch.PackagedError{ExePath: storeKuGou}, launch.ErrExeUnknown} {
		t.Run(gateErr.Error(), func(t *testing.T) {
			stubLocate(t, runningKuGou{}, errors.New("must not locate"), nil)
			gates := stubGateRunning(t, gateErr)
			if err := EnsurePatched(make(chan struct{})); err != gateErr || *gates != 1 {
				t.Errorf("EnsurePatched = %v（门禁调用 %d 次），期望原样返回 %v", err, *gates, gateErr)
			}
		})
	}
}
