package sodamusic

// 本文件只测 Start 对商店版门禁两种错误的处置（issue #212）：微软商店版只警告一次、standby + version_unsupported、
// 按 storeRecheck 重查；读不到安装路径只警告一次、waiting_process + no_admin、循环顶部不与「未启动」来回切换。
// 两者都不连接。找主进程、开 inspector 与连接都经 seam 换成桩，在 synctest 泡里跑（等待是假时钟）。

import (
	"bytes"
	"errors"
	stdlog "log"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/launch"
)

// runGateStart 跑 Start 到 until：主进程一直是同一个 pid，开 inspector 一律返回 gateErr。返回开 inspector 的次数、
// 状态序列（「状态/reason」）与日志。
func runGateStart(t *testing.T, gateErr error, until time.Duration) (ensures int, statuses []string, logs string) {
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
			if time.Since(t0) >= until {
				p.Stop()
				return 0, errors.New("stopped")
			}
			return 4242, nil
		}
		ensureInspector = func(int) error { ensures++; return gateErr }
		connectCDP = func() (extractSource, error) {
			t.Error("门禁不过，却去连接了")
			return nil, errors.New("must not connect")
		}
		p.Start()
		for len(p.EventCh) > 0 {
			if e := <-p.EventCh; e.Type == player.EventStatusUpdate {
				s := e.Data.(*player.StatusInfo)
				statuses = append(statuses, s.Status+"/"+s.Reason)
			}
		}
	})
	return ensures, statuses, buf.String()
}

// TestStartStoreInstall
//
// 防的缺陷与变异（均实测变红）：不认商店版、落到原有的「开 inspector 失败」（每 3 秒一条警告、不报 standby）；
// 每轮都警告；重查间隔用 3 秒而不是 storeRecheck。
func TestStartStoreInstall(t *testing.T) {
	store := &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\Soda_2.1.0.0_x64__abc\SodaMusic.exe`}
	ensures, statuses, logs := runGateStart(t, store, 65*time.Second)
	if ensures != 3 {
		t.Errorf("开 inspector %d 次，期望 0s、30s、60s 各一次", ensures)
	}
	standby := strings.Count(strings.Join(statuses, ","), "standby/"+player.ReasonVersionUnsupported)
	if standby != 3 {
		t.Errorf("standby + version_unsupported 发了 %d 次，期望 3 次；状态 %v", standby, statuses)
	}
	if n := strings.Count(logs, "[!]"); n != 1 || !strings.Contains(logs, store.ExePath) {
		t.Errorf("警告打了 %d 次，期望 1 次且带映像路径；日志：%s", n, logs)
	}
}

// TestStartExeUnknown
//
// 防的缺陷与变异（均实测变红）：每轮都警告；循环顶部不看 exeUnknown、照旧声明「未启动」（状态来回切换）。
func TestStartExeUnknown(t *testing.T) {
	ensures, statuses, logs := runGateStart(t, launch.ErrExeUnknown, 10*time.Second)
	if ensures < 3 {
		t.Errorf("开 inspector %d 次，期望每 3 秒重查", ensures)
	}
	want := []string{"waiting_process/" + player.ReasonProcessNotRunning, "waiting_process/" + player.ReasonNoAdmin}
	if strings.Join(statuses, ",") != strings.Join(want, ",") {
		t.Errorf("状态序列 %v，期望 %v", statuses, want)
	}
	if n := strings.Count(logs, "[!]"); n != 1 {
		t.Errorf("警告打了 %d 次，期望 1 次；日志：%s", n, logs)
	}
}
