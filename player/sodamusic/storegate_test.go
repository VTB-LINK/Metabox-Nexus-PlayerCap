package sodamusic

// 本文件只测 Start 对开 inspector 失败的处置（issue #212、#231）：微软商店版只警告一次、standby + version_unsupported、
// 按 storeRecheck 重查，其间每 recheckStep 判一次进程，退出不等满重查间隔；读不到安装路径只警告一次、standby + no_admin；
// 其余错误报 standby + cdp_disconnected。三者都不连接，状态只在变化时发。另测汽水在运行而主进程的命令行读不到（#231 代码审）：
// 按在运行报 standby + no_admin、只警告一次、不开 inspector。找主进程、开 inspector 与连接都经 seam 换成桩，在 synctest 泡里跑
// （等待是假时钟）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/launch"
	"Metabox-Nexus-PlayerCap/player/sodamusic/watchdog"
)

// runGateStart 跑 Start 到 until：汽水在 [0, exitAt) 内运行、之后不在运行；其中 [0, unreadableUntil) 内主进程的命令行读不到，
// 此后主进程是同一个 pid；开 inspector 一律返回 gateErr。返回开 inspector 的各次时刻、状态序列（「时刻 状态/reason」）与日志。
func runGateStart(t *testing.T, gateErr error, unreadableUntil, exitAt, until time.Duration) (ensures []time.Duration, statuses []string, logs string) {
	t.Helper()
	of, oe, oc := findMainPID, ensureInspector, connectCDP
	defer func() { findMainPID, ensureInspector, connectCDP = of, oe, oc }()

	var buf bytes.Buffer
	w := stdlog.Writer()
	stdlog.SetOutput(&buf)
	defer stdlog.SetOutput(w)

	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		t0 := time.Now()
		findMainPID = func() (int, error) {
			switch e := time.Since(t0); {
			case e >= until:
				p.Stop()
				return 0, watchdog.ErrNotRunning
			case e >= exitAt:
				return 0, watchdog.ErrNotRunning
			case e < unreadableUntil:
				return 0, watchdog.ErrMainUnreadable
			}
			return 4242, nil
		}
		ensureInspector = func(int) error { ensures = append(ensures, time.Since(t0)); return gateErr }
		connectCDP = func(context.Context) (extractSource, error) {
			t.Error("门禁不过，却去连接了")
			return nil, errors.New("must not connect")
		}
		done := make(chan struct{})
		go func() { defer close(done); p.Start() }()
		for {
			synctest.Wait()
			for len(p.EventCh) > 0 {
				// until 那一次找主进程报错（桩借它 Stop），其后的状态不计。
				if e := <-p.EventCh; e.Type == player.EventStatusUpdate && time.Since(t0) < until {
					s := e.Data.(*player.StatusInfo)
					statuses = append(statuses, fmt.Sprintf("%v %s/%s", time.Since(t0), s.Status, s.Reason))
				}
			}
			select {
			case <-done:
				return
			default:
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return ensures, statuses, buf.String()
}

// TestStartStoreInstall：0s 判出商店版，30s 重查仍是；41s 关掉，重查等待在 42s 的那一步即察觉、改报 process_exited。
//
// 防的缺陷与变异（均实测变红）：不认商店版、落到原有的「开 inspector 失败」（每 3 秒一条警告、报 cdp_disconnected）；
// 每轮都警告；重查间隔用 3 秒而不是 storeRecheck；状态改回 p.Emit（每次重查重发一条）；重查等待改回 sleepOrStop(storeRecheck)
// （60s 才察觉退出）。
func TestStartStoreInstall(t *testing.T) {
	store := &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\Soda_2.1.0.0_x64__abc\SodaMusic.exe`}
	ensures, statuses, logs := runGateStart(t, store, 0, 41*time.Second, 49*time.Second)
	if want := []time.Duration{0, 30 * time.Second}; fmt.Sprint(ensures) != fmt.Sprint(want) {
		t.Errorf("开 inspector 于 %v，期望 %v", ensures, want)
	}
	want := []string{"0s standby/" + player.ReasonVersionUnsupported, "42s waiting_process/" + player.ReasonProcessExited}
	if strings.Join(statuses, ",") != strings.Join(want, ",") {
		t.Errorf("状态序列 %v，期望 %v", statuses, want)
	}
	if n := strings.Count(logs, "[!]"); n != 1 || !strings.Contains(logs, store.ExePath) {
		t.Errorf("警告打了 %d 次，期望 1 次且带映像路径；日志：%s", n, logs)
	}
}

// TestStartExeUnknown
//
// 防的缺陷与变异（均实测变红）：每轮都警告；按旧写法报 waiting_process / no_admin（汽水在运行）。
func TestStartExeUnknown(t *testing.T) {
	ensures, statuses, logs := runGateStart(t, launch.ErrExeUnknown, 0, time.Hour, 10*time.Second)
	if len(ensures) < 3 {
		t.Errorf("开 inspector %d 次，期望每 3 秒重查", len(ensures))
	}
	want := []string{"0s standby/" + player.ReasonNoAdmin}
	if strings.Join(statuses, ",") != strings.Join(want, ",") {
		t.Errorf("状态序列 %v，期望 %v", statuses, want)
	}
	if n := strings.Count(logs, "[!]"); n != 1 {
		t.Errorf("警告打了 %d 次，期望 1 次；日志：%s", n, logs)
	}
}

// TestStartInspectorFailure：开 inspector 的其他错误（命名映射打不开等）按在运行报 standby / cdp_disconnected，只发一条；
// 汽水退出即改报 process_exited。
//
// 变异自证（实测变红）：开 inspector 失败按不在运行合成（报 waiting_process）。
func TestStartInspectorFailure(t *testing.T) {
	_, statuses, _ := runGateStart(t, errors.New("合成的映射打不开"), 0, 7*time.Second, 11*time.Second)
	want := []string{"0s standby/" + player.ReasonCDPDisconnected, "9s waiting_process/" + player.ReasonProcessExited}
	if strings.Join(statuses, ",") != strings.Join(want, ",") {
		t.Errorf("状态序列 %v，期望 %v", statuses, want)
	}
}

// TestStartMainUnreadable：汽水在运行而主进程的命令行读不到（多因汽水以管理员身份运行，#231 代码审）：按在运行报 standby /
// no_admin（计入 player_running），只警告一次，不开 inspector；6 s 起读得到了，照常开 inspector（本例开不了，报
// cdp_disconnected）；10 s 退出，12 s 那一轮报 process_exited。
//
// 变异自证（均实测变红）：读不到命令行按不在运行合成（0 s 报 waiting_process）；读不到命令行每轮都警告（Warn 条数）；
// FindMainPID 的结论里只认 err == nil 为在运行（同第一条）。
func TestStartMainUnreadable(t *testing.T) {
	ensures, statuses, logs := runGateStart(t, errors.New("合成的映射打不开"), 6*time.Second, 10*time.Second, 14*time.Second)
	want := []string{
		"0s standby/" + player.ReasonNoAdmin,
		"6s standby/" + player.ReasonCDPDisconnected,
		"12s waiting_process/" + player.ReasonProcessExited,
	}
	if strings.Join(statuses, ",") != strings.Join(want, ",") {
		t.Errorf("状态序列 %v，期望 %v", statuses, want)
	}
	if want := []time.Duration{6 * time.Second, 9 * time.Second}; fmt.Sprint(ensures) != fmt.Sprint(want) {
		t.Errorf("开 inspector 于 %v，期望读得到命令行之后才开：%v", ensures, want)
	}
	if n := strings.Count(logs, "[!]"); n != 3 {
		t.Errorf("警告打了 %d 次，期望读不到命令行 1 次、开 inspector 失败 2 次；日志：%s", n, logs)
	}
}

// TestStartStopInterruptsConnect：连接挂住（9229 端口接受连接而不完成响应）时，Stop 即中止这次连接，Start 随即返回（连接失败
// 后的等待可被停止打断）。连接的桩一直挂到 ctx 结束。
//
// 变异自证（均实测变红）：连接改传 context.Background()（泡内死锁）；停止时不取消 ctx（同上）。
func TestStartStopInterruptsConnect(t *testing.T) {
	of, oe, oc := findMainPID, ensureInspector, connectCDP
	defer func() { findMainPID, ensureInspector, connectCDP = of, oe, oc }()

	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		findMainPID = func() (int, error) { return 4242, nil }
		ensureInspector = func(int) error { return nil }
		connects := 0
		connectCDP = func(ctx context.Context) (extractSource, error) {
			connects++
			<-ctx.Done()
			return nil, ctx.Err()
		}
		done := make(chan struct{})
		go func() { defer close(done); p.Start() }()
		time.Sleep(3 * time.Second)
		t0 := time.Now()
		p.Stop()
		<-done
		if el := time.Since(t0); el != 0 || connects != 1 {
			t.Errorf("Stop 之后 %v 返回、连接 %d 次，期望中止挂住的那次连接、随即返回、只连 1 次", el, connects)
		}
	})
}
