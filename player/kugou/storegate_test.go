package kugou

// 本文件只测 Start 对商店版门禁两种错误的处置（issue #212）：微软商店版不进终态、按 storeRecheck 重查而不退出循环
// （用户改装官网版后不必重启 PlayerCap），警告只打一次；读不到安装路径只警告一次。两者都不去等 CDP。状态按酷狗是否
// 在运行合成（#231）：商店版在运行时为 standby + version_unsupported、不在运行时为 waiting_process 加写明商店版的
// detail；读不到安装路径在运行时为 standby + no_admin。修补检查、判进程与 CDP 的门禁、连接都经 seam 换成桩，不弹 UAC、
// 不碰真进程；Start 在 synctest 的泡里跑，夹具见 startstatus_test.go。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	"Metabox-Nexus-PlayerCap/player/launch"
)

var storeErr = &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\KuGou.KuGouMusic_20.1.22.0_x64__abc\KuGou.exe`}

// TestStartStoreInstallKeepsLooping：商店版按 storeRecheck 重查、不退出循环，警告只打一次；酷狗没打开时报 waiting_process、
// detail 写明商店版不支持，打开后下一轮改报 standby + version_unsupported，关掉后改报 process_exited。
//
// 防的缺陷与变异（均实测变红）：商店版的分支改回 return（Start 在 Stop 之前返回）；每次重查都警告；状态不经
// isKuGouRunning、一律报 standby；重查等待直接 sleepOrStop(storeRecheck)；seen 不维护（关掉后报 process_not_running）。
// 落到 classifyEnsureErr 照常等 CDP 的写法由「不去连接」一条断言守（未实测）。
func TestStartStoreInstallKeepsLooping(t *testing.T) {
	sc := startScript{until: 99, running: between(41, 71), ensure: failWith(storeErr)}
	r := runStartScript(t, sc)
	d := kugouDetails()
	notRunning := i18n.T("KuGou not running; the Microsoft Store version is unsupported (requires the official installer version)")
	r.check(t, sc, []timedStatus{
		at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
		at(0, waitingProcess, player.ReasonProcessNotRunning, notRunning),
		at(42, standby, player.ReasonVersionUnsupported, i18n.T("KuGou Microsoft Store version unsupported (requires the official installer version)")),
		at(72, waitingProcess, player.ReasonProcessExited, notRunning),
	}, secs(0, 30, 60, 90), []time.Duration{})
	if len(r.connectAt) != 0 {
		t.Errorf("商店版却去连接了 CDP：%v", r.connectAt)
	}
	if n := strings.Count(r.logs, "[!]"); n != 1 {
		t.Errorf("警告打了 %d 次，期望 1 次；日志：%s", n, r.logs)
	}
	if !strings.Contains(r.logs, storeErr.ExePath) || !strings.Contains(r.logs, installerURL) {
		t.Errorf("警告里应有映像路径与安装包直链；日志：%s", r.logs)
	}
}

// stubCDPGate 换上 waitForCDP 的门禁与连接的桩，判进程恒为不在运行，返回连接被调用的次数。
func stubCDPGate(t *testing.T, gate func() error, connect func() (*cdp.Client, error)) *int {
	t.Helper()
	oldGate, oldConnect, oldRunning := gateRunning, connectCDP, isKuGouRunning
	t.Cleanup(func() { gateRunning, connectCDP, isKuGouRunning = oldGate, oldConnect, oldRunning })
	connects := new(int)
	gateRunning = gate
	connectCDP = func(context.Context) (*cdp.Client, error) { *connects++; return connect() }
	isKuGouRunning = func() bool { return false }
	return connects
}

// TestWaitForCDPGate：每次连接之前都过商店版门禁，不过即原样返回门禁的错误、不连接。门禁通过时照常连接，是对照
// （连接的桩失败并 Stop，waitForCDP 随即因酷狗未运行或收到停止信号返回）。
//
// 防的缺陷与变异（均实测变红）：去掉 waitForCDP 里的门禁（#231 改写后重测）；门禁挪到连接之后（#212 时实测）。
func TestWaitForCDPGate(t *testing.T) {
	for _, gateErr := range []error{storeErr, launch.ErrExeUnknown} {
		t.Run(gateErr.Error(), func(t *testing.T) {
			p := New(0, 30)
			connects := stubCDPGate(t, func() error { return gateErr }, func() (*cdp.Client, error) {
				t.Error("门禁不过，却去连接了")
				return nil, errors.New("must not connect")
			})
			if client, err := p.waitForCDP(); client != nil || err != gateErr || *connects != 0 {
				t.Errorf("waitForCDP = (%v, %v)、连接 %d 次，期望原样返回门禁的错误、不连接", client, err, *connects)
			}
		})
	}
	t.Run("门禁通过：照常连接", func(t *testing.T) {
		p := New(0, 30)
		connects := stubCDPGate(t, func() error { return nil }, func() (*cdp.Client, error) {
			p.Stop()
			return nil, errors.New("refused")
		})
		if client, err := p.waitForCDP(); client != nil || err == nil || *connects != 1 {
			t.Errorf("waitForCDP = (%v, %v)、连接 %d 次，期望连接 1 次后返回错误", client, err, *connects)
		}
	})
}

// TestStartRoutesCDPGateErr：修补检查放行之后，等 CDP 期间门禁不过（用户启动了商店版）：Start 不连接、不打「CDP 未就绪」、
// 不等，回到循环顶部由修补检查报出同一错误、进入商店版分支（standby + version_unsupported，按 storeRecheck 重查）。第二次起
// 修补检查的桩返回商店版，模拟它开头的门禁。
//
// 防的缺陷与变异（均实测变红）：waitForCDP 的门禁错误照旧按「CDP 未就绪」处理（打警告、等 2 秒）；去掉 waitForCDP 里的门禁；
// 商店版的分支改回 return。
func TestStartRoutesCDPGateErr(t *testing.T) {
	sc := startScript{
		until:   9,
		running: always(true),
		ensure: func(_ time.Duration, n int) error {
			if n == 1 {
				return nil
			}
			return storeErr
		},
		gate: func() error { return storeErr },
	}
	r := runStartScript(t, sc)
	r.check(t, sc, []timedStatus{
		at(0, standby, player.ReasonCDPDisconnected, kugouDetails().Disconnected),
		at(0, standby, player.ReasonVersionUnsupported, i18n.T("KuGou Microsoft Store version unsupported (requires the official installer version)")),
	}, secs(0, 0), []time.Duration{})
	if len(r.connectAt) != 0 {
		t.Errorf("门禁不过却去连接了 CDP：%v", r.connectAt)
	}
	if n := strings.Count(r.logs, "[!]"); n != 1 || !strings.Contains(r.logs, storeErr.ExePath) {
		t.Errorf("警告打了 %d 次，期望只有商店版那 1 条；日志：%s", n, r.logs)
	}
}

// TestStartExeUnknownWarnsOnce：读不到安装路径时只警告一次，每轮重走修补检查、不去等 CDP；酷狗在运行时报 standby + no_admin，
// 不在运行时按普通的未运行（这一情形没有专门的 detail），状态不在两条之间来回切换。
//
// 防的缺陷与变异（均实测变红）：每轮都警告；读不到路径不记为受阻（在运行时报 cdp_disconnected，且不再警告）；状态不经
// isKuGouRunning（不在运行时报 standby）；重查等待直接 sleepOrStop(storeRecheck)。
func TestStartExeUnknownWarnsOnce(t *testing.T) {
	d := kugouDetails()
	cases := []struct {
		name     string
		running  bool
		statuses []timedStatus
	}{
		{"酷狗在运行", true, []timedStatus{
			at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
			at(0, standby, player.ReasonNoAdmin, i18n.T("Cannot read the KuGou installation path (KuGou may be running as administrator)")),
		}},
		{"酷狗不在运行", false, []timedStatus{
			at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := startScript{until: 9, running: always(c.running), ensure: failWith(launch.ErrExeUnknown)}
			r := runStartScript(t, sc)
			r.check(t, sc, c.statuses, secs(0, 2, 4, 6, 8), []time.Duration{})
			if len(r.connectAt) != 0 {
				t.Errorf("读不到安装路径却去连接了 CDP：%v", r.connectAt)
			}
			if n := strings.Count(r.logs, "[!]"); n != 1 {
				t.Errorf("警告打了 %d 次，期望 1 次；日志：%s", n, r.logs)
			}
		})
	}
}
