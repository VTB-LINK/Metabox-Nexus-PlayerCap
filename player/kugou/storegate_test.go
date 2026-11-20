package kugou

// 本文件只测 Start 对商店版门禁两种错误的处置（issue #212）：微软商店版不进终态、按 storeRecheck 重查而不退出循环
// （用户改装官网版后不必重启 PlayerCap），警告只打一次、状态为 standby + version_unsupported；读不到安装路径
// 只警告一次、状态为 waiting_process + no_admin。两者都不去等 CDP。EnsurePatched 经 seam 换成桩，不弹 UAC、
// 不碰真进程。

import (
	"bytes"
	"errors"
	stdlog "log"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	"Metabox-Nexus-PlayerCap/player/launch"
)

// runStart 换上 EnsurePatched 的桩（第 n 次调用返回 errs[n-1]，最后一次调用之前 Stop），跑 Start 到返回，
// 返回桩被调用的次数、发出的状态与日志。
func runStart(t *testing.T, errs ...error) (calls int, statuses []*player.StatusInfo, logs string) {
	t.Helper()
	oldEnsure, oldRecheck := ensurePatched, storeRecheck
	t.Cleanup(func() { ensurePatched, storeRecheck = oldEnsure, oldRecheck })
	storeRecheck = time.Millisecond

	var buf bytes.Buffer
	w := stdlog.Writer()
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(w) })

	p := New(0, 30)
	ensurePatched = func(<-chan struct{}) error {
		calls++
		if calls == len(errs) {
			p.Stop()
		}
		return errs[calls-1]
	}
	done := make(chan struct{})
	go func() { p.Start(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Start 没有在 Stop 之后返回")
	}
	for len(p.EventCh) > 0 {
		if e := <-p.EventCh; e.Type == player.EventStatusUpdate {
			statuses = append(statuses, e.Data.(*player.StatusInfo))
		}
	}
	return calls, statuses, buf.String()
}

// TestStartStoreInstallKeepsLooping
//
// 防的缺陷与变异（均实测变红）：商店版沿用「版本不支持即终态、退出循环」（不再调用 EnsurePatched）；每轮都警告。
// 落到 classifyEnsureErr 照常等 CDP 的变异未实测：那会连本机酷狗的调试端口。
func TestStartStoreInstallKeepsLooping(t *testing.T) {
	store := &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\KuGou.KuGouMusic_20.1.22.0_x64__abc\KuGou.exe`}
	calls, statuses, logs := runStart(t, store, store, store)
	if calls != 3 {
		t.Fatalf("EnsurePatched 调用 %d 次，期望 3 次（商店版不退出循环）", calls)
	}
	standby := 0
	for _, s := range statuses {
		if s.Status == "error" {
			t.Errorf("商店版发出了终态 error：%+v", s)
		}
		if s.Status == "standby" && s.Reason == player.ReasonVersionUnsupported {
			standby++
		}
	}
	// 第三轮的桩先 Stop 再返回，Start 在出错分支开头见到停止信号即返回，不再发状态。
	if standby != 2 {
		t.Errorf("standby + version_unsupported 发了 %d 次，期望前两轮各一次；状态 %+v", standby, statuses)
	}
	if n := strings.Count(logs, "[!]"); n != 1 {
		t.Errorf("警告打了 %d 次，期望 1 次；日志：%s", n, logs)
	}
	if !strings.Contains(logs, store.ExePath) || !strings.Contains(logs, installerURL) {
		t.Errorf("警告里应有映像路径与安装包直链；日志：%s", logs)
	}
}

// stubCDPGate 换上 waitForCDP 的门禁与连接的桩，返回连接被调用的次数。
func stubCDPGate(t *testing.T, gate func() error, connect func() (*cdp.Client, error)) *int {
	t.Helper()
	oldGate, oldConnect := gateRunning, connectCDP
	t.Cleanup(func() { gateRunning, connectCDP = oldGate, oldConnect })
	connects := new(int)
	gateRunning = gate
	connectCDP = func() (*cdp.Client, error) { *connects++; return connect() }
	return connects
}

// TestWaitForCDPGate：每次连接之前都过商店版门禁，不过即原样返回门禁的错误、不连接。门禁通过时照常连接，是对照
// （连接的桩失败并 Stop，waitForCDP 随即因酷狗未运行或收到停止信号返回）。
//
// 防的缺陷与变异（均实测变红）：去掉 waitForCDP 里的门禁；门禁挪到连接之后。
func TestWaitForCDPGate(t *testing.T) {
	store := &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\KuGou.KuGouMusic_20.1.22.0_x64__abc\KuGou.exe`}
	for _, gateErr := range []error{store, launch.ErrExeUnknown} {
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

// TestStartRoutesCDPGateErr：EnsurePatched 放行之后，等 CDP 期间门禁不过（用户启动了商店版）：Start 不连接、不打
// 「CDP 未就绪」，回到循环顶部由 EnsurePatched 报出同一错误、进入商店版分支（standby + version_unsupported，按
// storeRecheck 重查）。第二次起 EnsurePatched 的桩返回商店版，模拟它开头的门禁。
//
// 防的缺陷与变异（均实测变红）：waitForCDP 的门禁错误照旧按「CDP 未就绪」处理（打警告、等 2 秒）。
func TestStartRoutesCDPGateErr(t *testing.T) {
	store := &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\KuGou.KuGouMusic_20.1.22.0_x64__abc\KuGou.exe`}
	connects := stubCDPGate(t, func() error { return store }, func() (*cdp.Client, error) {
		t.Error("门禁不过，却去连接了")
		return nil, errors.New("must not connect")
	})
	start := time.Now()
	calls, statuses, logs := runStart(t, nil, store, store)
	if calls != 3 || *connects != 0 {
		t.Fatalf("EnsurePatched 调用 %d 次、连接 %d 次，期望 3 次、0 次", calls, *connects)
	}
	standby := 0
	for _, s := range statuses {
		if s.Status == "standby" && s.Reason == player.ReasonVersionUnsupported {
			standby++
		}
	}
	if standby != 1 {
		t.Errorf("standby + version_unsupported 发了 %d 次，期望 1 次；状态 %+v", standby, statuses)
	}
	if n := strings.Count(logs, "[!]"); n != 1 || !strings.Contains(logs, store.ExePath) {
		t.Errorf("警告打了 %d 次，期望只有商店版那 1 条；日志：%s", n, logs)
	}
	if d := time.Since(start); d >= 2*time.Second {
		t.Errorf("Start 用了 %v，门禁错误不应按「CDP 未就绪」等 2 秒", d)
	}
}

// TestStartExeUnknownWarnsOnce：读不到安装路径时只警告一次，状态为 waiting_process + no_admin，且之后循环顶部声明的
// 仍是这一条（EmitStatusIfChanged 去重，不与「未启动」每轮来回切换）。
//
// 防的缺陷与变异（均实测变红）：每轮都警告；循环顶部不看 exeUnknown、照旧声明「未启动」（状态来回切换）。
func TestStartExeUnknownWarnsOnce(t *testing.T) {
	// 三轮：第三轮的桩先 Stop，Start 在出错分支开头返回，前两轮走到读不到路径的处置（每轮等 2 秒）。
	calls, statuses, logs := runStart(t, launch.ErrExeUnknown, launch.ErrExeUnknown, launch.ErrExeUnknown)
	if calls != 3 {
		t.Fatalf("EnsurePatched 调用 %d 次，期望 3 次", calls)
	}
	var reasons []string
	for _, s := range statuses {
		reasons = append(reasons, s.Status+"/"+s.Reason)
	}
	want := []string{"waiting_process/" + player.ReasonProcessNotRunning, "waiting_process/" + player.ReasonNoAdmin}
	if strings.Join(reasons, ",") != strings.Join(want, ",") {
		t.Errorf("状态序列 %v，期望 %v", reasons, want)
	}
	if n := strings.Count(logs, "[!]"); n != 1 {
		t.Errorf("警告打了 %d 次，期望 1 次；日志：%s", n, logs)
	}
}
