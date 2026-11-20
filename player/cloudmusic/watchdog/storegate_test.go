package watchdog

// 本文件只测 EnsureDebugMode 与 GateRunning 的商店版门禁（issue #212）：全部 cloudmusic.exe 逐个核验，任一映像路径
// 读不到或在 WindowsApps 下即不结束、不拉起，且门禁先于「已带保活参数」的早退。进程枚举、结束、拉起与等待经
// 包级变量换成桩，不碰任何真进程。

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/launch"
)

const storeExe = `C:\Program Files\WindowsApps\1F8B0F94.122165AE053F_3.1.23.0_x64__j2p0p5q0044a6\VFS\ProgramFilesX86\NetEase\CloudMusic\cloudmusic.exe`

// fakeNetEase 替换 EnsureDebugMode 用到的进程操作，记下结束与拉起。
type fakeNetEase struct {
	procs   []proc
	scanErr error
	kills   int
	starts  []string // 每次拉起的 exe 路径
	args    []string
}

func (f *fakeNetEase) install(t *testing.T) {
	t.Helper()
	oldScan, oldKill, oldDetach, oldSleep := scanFn, killFn, detachFn, sleepFn
	t.Cleanup(func() { scanFn, killFn, detachFn, sleepFn = oldScan, oldKill, oldDetach, oldSleep })
	scanFn = func() ([]proc, error) { return f.procs, f.scanErr }
	killFn = func() { f.kills++ }
	detachFn = func(exe string, args []string, _ string) (bool, error) {
		f.starts = append(f.starts, exe)
		f.args = args
		return true, nil
	}
	sleepFn = func(time.Duration) {}
}

// officialExe 返回一个不存在的官网安装版路径：ReadExeVersion 读不出版本，按 v3 走，结果不随开发机上装的网易云而变。
func officialExe(t *testing.T) string {
	return filepath.Join(t.TempDir(), "NetEase", "CloudMusic", "cloudmusic.exe")
}

// TestEnsureDebugModeRestartsOfficialInstall 是下面各条「不结束、不拉起」的对照：全部为官网安装版且缺保活参数时，
// 桩确实被调用、拉起的是读到的路径。没有这一条，桩接线失效时下面各条恒绿。
func TestEnsureDebugModeRestartsOfficialInstall(t *testing.T) {
	exe := officialExe(t)
	f := &fakeNetEase{procs: []proc{{ExePath: exe, Cmdline: `"` + exe + `"`}, {ExePath: exe, Cmdline: `"` + exe + `" --type=renderer`}}}
	f.install(t)
	restarted, err := EnsureDebugMode()
	if err != nil || !restarted {
		t.Fatalf("EnsureDebugMode = (%v, %v)，期望 (true, nil)", restarted, err)
	}
	if f.kills != 1 || !slices.Equal(f.starts, []string{exe}) || !slices.Equal(f.args, LaunchFlags) {
		t.Fatalf("kills=%d starts=%v args=%v，期望结束一次、带 LaunchFlags 拉起 %s 一次", f.kills, f.starts, f.args, exe)
	}
}

// TestEnsureDebugModeGateRefuses：任一 cloudmusic.exe 的映像路径在 WindowsApps 下或读不到，即返回门禁的错误，
// 不结束、不拉起；带着保活参数也一样（门禁先于 hasFlags 早退）。
//
// 防的缺陷与变异（均实测变红）：去掉门禁；只核验第一个进程；门禁挪到 hasFlags 早退之后（三条带保活参数的用例）；
// 门禁挪到 taskkill 之后；读不到路径时退回默认安装路径（改回旧代码的写法）。
func TestEnsureDebugModeGateRefuses(t *testing.T) {
	official := officialExe(t)
	plain := func(exe string) proc { return proc{ExePath: exe, Cmdline: `"` + exe + `"`} }
	flagged := func(exe string) proc {
		return proc{ExePath: exe, Cmdline: `"` + exe + `" ` + DebugFlag + " " + keepaliveMarker}
	}
	unreadable := proc{Cmdline: `"x" --type=renderer`}
	cases := []struct {
		name     string
		procs    []proc
		packaged bool // 期望 *PackagedError；否则期望 ErrExeUnknown
	}{
		{"只有商店版", []proc{plain(storeExe)}, true},
		{"官网版在前、商店版在后", []proc{plain(official), plain(storeExe)}, true},
		{"商店版在前、官网版在后", []proc{plain(storeExe), plain(official)}, true},
		{"商店版已带保活参数", []proc{flagged(storeExe)}, true},
		{"官网版已带保活参数、另有商店版", []proc{flagged(official), plain(storeExe)}, true},
		{"只有一个读不到路径的进程", []proc{unreadable}, false},
		{"官网版之外有一个读不到路径", []proc{plain(official), unreadable}, false},
		{"读不到路径的在前", []proc{unreadable, plain(official)}, false},
		{"读不到路径的进程带保活参数", []proc{{Cmdline: keepaliveMarker}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeNetEase{procs: c.procs}
			f.install(t)
			restarted, err := EnsureDebugMode()
			var pe *launch.PackagedError
			if c.packaged {
				if !errors.As(err, &pe) || pe.ExePath != storeExe {
					t.Errorf("err = %v，期望 *PackagedError（%s）", err, storeExe)
				}
			} else if !errors.Is(err, launch.ErrExeUnknown) {
				t.Errorf("err = %v，期望 launch.ErrExeUnknown", err)
			}
			if restarted || f.kills != 0 || len(f.starts) != 0 {
				t.Errorf("restarted=%v kills=%d starts=%v，期望不结束、不拉起", restarted, f.kills, f.starts)
			}
		})
	}
}

// TestEnsureDebugModeLeavesOthersAlone：未运行、已带保活参数的官网版、枚举出错时都不结束、不拉起。
func TestEnsureDebugModeLeavesOthersAlone(t *testing.T) {
	official := officialExe(t)
	boom := errors.New("boom")
	cases := []struct {
		name    string
		f       fakeNetEase
		wantErr error
	}{
		{"未运行", fakeNetEase{}, nil},
		{"官网版已带保活参数", fakeNetEase{procs: []proc{{ExePath: official, Cmdline: `"` + official + `" ` + keepaliveMarker}}}, nil},
		{"枚举出错", fakeNetEase{scanErr: boom}, boom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.f
			f.install(t)
			restarted, err := EnsureDebugMode()
			if !errors.Is(err, c.wantErr) || (c.wantErr == nil) != (err == nil) {
				t.Errorf("err = %v，期望 %v", err, c.wantErr)
			}
			if restarted || f.kills != 0 || len(f.starts) != 0 {
				t.Errorf("restarted=%v kills=%d starts=%v，期望不结束、不拉起", restarted, f.kills, f.starts)
			}
		})
	}
}

// 不存在的进程号（Windows 的进程号是 4 的倍数且远小于它们）：扫描层按进程号读命令行一律失败、按空处理，不读到真进程。
const (
	noPID1 uint32 = 0x7FFFFFF0
	noPID2 uint32 = 0x7FFFFFF4
	noPID3 uint32 = 0x7FFFFFF8
)

// installRunning 只换进程快照（launch.Running）的桩，扫描层 scanProcesses 照常运行；结束、拉起与等待仍是桩。
// 返回快照被查询时用的映像名。
func (f *fakeNetEase) installRunning(t *testing.T, running []launch.Proc, err error) *string {
	t.Helper()
	f.install(t)
	oldRun := runningFn
	t.Cleanup(func() { runningFn = oldRun })
	scanFn = scanProcesses
	asked := new(string)
	runningFn = func(name string) ([]launch.Proc, error) { *asked = name; return running, err }
	return asked
}

// TestScanKeepsUnreadableProcesses 从扫描层（不只是替换扫描结果）钉住：读不到映像路径的 cloudmusic.exe 也在扫描结果里，
// 由门禁按路径未知拒绝。旧扫描按 gopsutil 的进程名枚举，名字取自映像路径，这样的进程不在结果里：另有可读的官网实例时
// 只核验它就放行 taskkill，未核验的那个照样被结束；全部读不到时被当成「未运行」。第一例是对照。
//
// 防的缺陷与变异（均实测变红）：扫描跳过路径为空的进程；按别的映像名查快照；快照出错时当作未运行。
func TestScanKeepsUnreadableProcesses(t *testing.T) {
	official := officialExe(t)
	boom := errors.New("snapshot failed")
	cases := []struct {
		name    string
		running []launch.Proc
		runErr  error
		want    string // "restart" / "unknown" / "packaged" / "idle" / "error"
	}{
		{"对照：只有一个可读的官网实例，照常重启", []launch.Proc{{PID: noPID1, ExePath: official}}, nil, "restart"},
		{"可读的官网实例之外另有一个读不到的", []launch.Proc{{PID: noPID1, ExePath: official}, {PID: noPID2}}, nil, "unknown"},
		{"读不到的在前", []launch.Proc{{PID: noPID2}, {PID: noPID1, ExePath: official}}, nil, "unknown"},
		{"只有一个读不到的：不是未运行", []launch.Proc{{PID: noPID2}}, nil, "unknown"},
		{"全部读不到", []launch.Proc{{PID: noPID2}, {PID: noPID3}}, nil, "unknown"},
		{"商店版", []launch.Proc{{PID: noPID1, ExePath: official}, {PID: noPID3, ExePath: storeExe}}, nil, "packaged"},
		{"未运行", nil, nil, "idle"},
		{"快照出错", nil, boom, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeNetEase{}
			asked := f.installRunning(t, c.running, c.runErr)
			restarted, err := EnsureDebugMode()
			if *asked != TargetProcessName {
				t.Errorf("快照按 %q 查，期望 %q", *asked, TargetProcessName)
			}
			var pe *launch.PackagedError
			switch c.want {
			case "restart":
				if err != nil || !restarted || f.kills != 1 || !slices.Equal(f.starts, []string{official}) {
					t.Errorf("(%v, %v) kills=%d starts=%v，期望照常重启 %s", restarted, err, f.kills, f.starts, official)
				}
				return
			case "unknown":
				if !errors.Is(err, launch.ErrExeUnknown) {
					t.Errorf("err = %v，期望 launch.ErrExeUnknown", err)
				}
			case "packaged":
				if !errors.As(err, &pe) {
					t.Errorf("err = %v，期望 *PackagedError", err)
				}
			case "idle":
				if err != nil {
					t.Errorf("err = %v，期望 nil（未运行）", err)
				}
			case "error":
				if !errors.Is(err, boom) {
					t.Errorf("err = %v，期望快照的错误", err)
				}
			}
			if restarted || f.kills != 0 || len(f.starts) != 0 {
				t.Errorf("restarted=%v kills=%d starts=%v，期望不结束、不拉起", restarted, f.kills, f.starts)
			}
		})
	}
	t.Run("GateRunning：可读的官网实例之外另有一个读不到的", func(t *testing.T) {
		f := &fakeNetEase{}
		f.installRunning(t, []launch.Proc{{PID: noPID1, ExePath: official}, {PID: noPID2}}, nil)
		if err := GateRunning(); !errors.Is(err, launch.ErrExeUnknown) {
			t.Errorf("GateRunning = %v，期望 launch.ErrExeUnknown", err)
		}
	})
}

// TestGateRunning：供特效的独立注入与泊车用的门禁。全部为官网版才放行；有商店版、读不到路径、没有在运行的
// cloudmusic.exe、枚举出错都拒绝。
//
// 防的缺陷与变异（均实测变红）：没有在运行的进程时放行；只核验第一个进程；枚举出错时放行。
func TestGateRunning(t *testing.T) {
	official := officialExe(t)
	boom := errors.New("boom")
	cases := []struct {
		name  string
		f     fakeNetEase
		check func(error) bool
	}{
		{"全部为官网版", fakeNetEase{procs: []proc{{ExePath: official}, {ExePath: official}}}, func(err error) bool { return err == nil }},
		{"官网版之外有商店版", fakeNetEase{procs: []proc{{ExePath: official}, {ExePath: storeExe}}}, func(err error) bool { var pe *launch.PackagedError; return errors.As(err, &pe) }},
		{"官网版之外有读不到路径的", fakeNetEase{procs: []proc{{ExePath: official}, {}}}, func(err error) bool { return errors.Is(err, launch.ErrExeUnknown) }},
		{"没有在运行的", fakeNetEase{}, func(err error) bool { return errors.Is(err, launch.ErrExeUnknown) }},
		{"枚举出错", fakeNetEase{scanErr: boom}, func(err error) bool { return errors.Is(err, boom) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.f
			f.install(t)
			if err := GateRunning(); !c.check(err) {
				t.Errorf("GateRunning = %v", err)
			}
		})
	}
}
