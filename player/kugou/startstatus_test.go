package kugou

// 本文件经桩离线跑 Start 的外层循环，只守非歌名状态与修补检查的去向（#231，设计
// doc/design/converge-player-behavior-231-design.md §3.2、§5）：酷狗是否在运行每轮先判；修补受阻（版本不支持、在运行而
// 找不到安装、提权已放弃）不进终态、按 storeRecheck 重做修补检查，不在运行时报 waiting_process、受阻的原因写进 detail，
// 在运行时报 standby 加原因；提权被拒 3 次之后只做不提权的检查。商店版与读不到安装路径两种门禁错误见 storegate_test.go，
// 复用本文件的夹具。
//
// Start 在 testing/synctest 的泡里跑，等待是假时钟：修补检查（ensurePatched、ensurePatchedNoElevate）、判进程
// （isKuGouRunning）、等 CDP 的门禁与连接（gateRunning、connectCDP）都换成按假时钟取值的桩，连接一律失败。测试 goroutine
// 每 100 ms 取走已发出的事件，状态按取到的时刻记；各桩的取值与 Start 的等待都落在 100 ms 的整数倍上，记下的时刻即发出的时刻。
//
// 变异自证（在临时副本里逐条注入、跑完即还原，均实测）记在各用例的注释里。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

// startScript 是一次 Start 的剧本，各桩按自 Start 开始计的假时钟 now 取值。
type startScript struct {
	until   float64                              // 到这一刻（秒）Stop
	running func(now time.Duration) bool         // isKuGouRunning
	ensure  func(now time.Duration, n int) error // ensurePatched 的第 n 次（从 1 起）调用；nil 即不得调用
	noElev  func(now time.Duration, n int) error // ensurePatchedNoElevate，同上
	gate    func() error                         // gateRunning；nil 即放行
}

// startRun 是一次 Start 的记录。
type startRun struct {
	statuses  []timedStatus
	ensureAt  []time.Duration // ensurePatched 各次调用的时刻
	noElevAt  []time.Duration // ensurePatchedNoElevate 各次调用的时刻
	connectAt []time.Duration // connectCDP 各次调用的时刻
	returned  bool            // Start 在 Stop 之前返回
	logs      string
}

type timedStatus struct {
	at time.Duration
	s  player.StatusInfo
}

func (ts timedStatus) String() string {
	return fmt.Sprintf("%v %s/%s/%s", ts.at, ts.s.Status, ts.s.Reason, ts.s.Detail)
}

// at 构造期望的一条状态。
func at(sec float64, status, reason, detail string) timedStatus {
	return timedStatus{time.Duration(sec * float64(time.Second)), player.StatusInfo{Status: status, Reason: reason, Detail: detail}}
}

// secs 把秒数列成时刻。
func secs(ss ...float64) []time.Duration {
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		out[i] = time.Duration(s * float64(time.Second))
	}
	return out
}

// every 列出 from 起每隔 step 秒、不超过 to 的时刻。
func every(from, to, step float64) []time.Duration {
	var ss []float64
	for s := from; s <= to; s += step {
		ss = append(ss, s)
	}
	return secs(ss...)
}

// runStartScript 按剧本跑 Start 到 sc.until 再 Stop，返回记录。
func runStartScript(t *testing.T, sc startScript) startRun {
	t.Helper()
	oe, on, or, og, oc := ensurePatched, ensurePatchedNoElevate, isKuGouRunning, gateRunning, connectCDP
	defer func() {
		ensurePatched, ensurePatchedNoElevate, isKuGouRunning, gateRunning, connectCDP = oe, on, or, og, oc
	}()

	var buf bytes.Buffer
	w := stdlog.Writer()
	stdlog.SetOutput(&buf)
	defer stdlog.SetOutput(w)

	var r startRun
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		t0 := time.Now()
		now := func() time.Duration { return time.Since(t0) }
		isKuGouRunning = func() bool { return sc.running(now()) }
		ensurePatched = func(<-chan struct{}) error {
			r.ensureAt = append(r.ensureAt, now())
			if sc.ensure == nil {
				t.Errorf("%v：不应调用 ensurePatched", now())
				return errors.New("unexpected ensurePatched")
			}
			return sc.ensure(now(), len(r.ensureAt))
		}
		ensurePatchedNoElevate = func(<-chan struct{}) error {
			r.noElevAt = append(r.noElevAt, now())
			if sc.noElev == nil {
				t.Errorf("%v：不应调用 ensurePatchedNoElevate", now())
				return errors.New("unexpected ensurePatchedNoElevate")
			}
			return sc.noElev(now(), len(r.noElevAt))
		}
		gateRunning = func() error {
			if sc.gate == nil {
				return nil
			}
			return sc.gate()
		}
		connectCDP = func(context.Context) (*cdp.Client, error) {
			r.connectAt = append(r.connectAt, now())
			return nil, errors.New("connection refused")
		}

		done := make(chan struct{})
		go func() { defer close(done); p.Start() }()
		for {
			synctest.Wait()
			r.drain(p, now())
			select {
			case <-done:
				r.returned = true
				return
			default:
			}
			if now() >= time.Duration(sc.until*float64(time.Second)) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		p.Stop()
		<-done
		r.drain(p, now())
	})
	r.logs = buf.String()
	return r
}

// drain 取走通道里已有的事件，记下其中的状态。
func (r *startRun) drain(p *KuGouPlayer, now time.Duration) {
	for {
		select {
		case e := <-p.EventCh:
			if e.Type == player.EventStatusUpdate {
				r.statuses = append(r.statuses, timedStatus{now, *e.Data.(*player.StatusInfo)})
			}
		default:
			return
		}
	}
}

// check 核对状态序列与各桩的调用时刻（want 为 nil 的不核对），并守所有剧本共有的反例：Start 在 Stop 之前返回、报 error、
// reason 为空、酷狗不在运行而报 standby、在运行而报 waiting_process。
func (r startRun) check(t *testing.T, sc startScript, statuses []timedStatus, ensureAt, noElevAt []time.Duration) {
	t.Helper()
	if r.returned {
		t.Errorf("Start 在 Stop 之前返回了")
	}
	for _, s := range r.statuses {
		switch {
		case s.s.Status == "error":
			t.Errorf("报了终态 error：%v", s)
		case s.s.Reason == "":
			t.Errorf("非歌名状态没有 reason：%v", s)
		case s.s.Status == "standby" && !sc.running(s.at):
			t.Errorf("酷狗不在运行而报 standby：%v", s)
		case s.s.Status == "waiting_process" && sc.running(s.at):
			t.Errorf("酷狗在运行而报 waiting_process：%v", s)
		}
	}
	if !slices.Equal(r.statuses, statuses) {
		t.Errorf("状态序列\n got  %v\n want %v", r.statuses, statuses)
	}
	if ensureAt != nil && !slices.Equal(r.ensureAt, ensureAt) {
		t.Errorf("ensurePatched 调用于 %v，期望 %v", r.ensureAt, ensureAt)
	}
	if noElevAt != nil && !slices.Equal(r.noElevAt, noElevAt) {
		t.Errorf("ensurePatchedNoElevate 调用于 %v，期望 %v", r.noElevAt, noElevAt)
	}
}

// between 返回在 [from, to) 秒之间为真的 running 桩。
func between(from, to float64) func(time.Duration) bool {
	return func(now time.Duration) bool {
		return now >= time.Duration(from*float64(time.Second)) && now < time.Duration(to*float64(time.Second))
	}
}

func always(b bool) func(time.Duration) bool { return func(time.Duration) bool { return b } }

func failWith(err error) func(time.Duration, int) error {
	return func(time.Duration, int) error { return err }
}

// 生产里各错误的包装形态（watchdog.ensurePatched 的返回）。
var (
	errVersion   = fmt.Errorf("%w: libcef.dll of 20.9.1 does not match known offsets; refusing blind patch", watchdog.ErrVersionUnsupported)
	errElevation = fmt.Errorf("%w (user may have denied UAC): canceled", watchdog.ErrElevationFailed)
	errOther     = errors.New("check patch status: open libcef.dll: access denied")
)

const (
	waitingProcess = "waiting_process"
	standby        = "standby"
)

// TestStartVersionUnsupportedFollowsProcess：版本不支持而酷狗没打开时报 waiting_process / process_not_running，detail 写明
// 启动后也读不了，持续期间只发一条；酷狗打开后下一轮（2 s 内）改报 standby / version_unsupported，关掉后下一轮改报
// waiting_process / process_exited。修补检查每 30 秒重做一次，进程状态不随它推迟。
//
// 变异自证（均红）：版本不支持的分支改回 return（Start 在 Stop 之前返回）；状态不经 isKuGouRunning、一律按在运行合成
// （0 s 报 standby）；重查等待不判进程、直接 sleepOrStop(storeRecheck)（打开在 120 s 才报）；seen 不维护（关掉后报
// process_not_running）；每次重查都打「自动修复失败」。
func TestStartVersionUnsupportedFollowsProcess(t *testing.T) {
	sc := startScript{
		until:   259,
		running: between(101, 201),
		ensure:  failWith(errVersion),
	}
	r := runStartScript(t, sc)
	d := kugouDetails()
	notRunning := i18n.T("KuGou not running; this libcef.dll version is unsupported and cannot be read after it starts")
	r.check(t, sc, []timedStatus{
		at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
		at(0, waitingProcess, player.ReasonProcessNotRunning, notRunning),
		at(102, standby, player.ReasonVersionUnsupported, i18n.T("KuGou libcef.dll version unsupported")),
		at(202, waitingProcess, player.ReasonProcessExited, notRunning),
	}, every(0, 240, 30), []time.Duration{})
	if n := strings.Count(r.logs, "[✗]"); n != 1 {
		t.Errorf("「自动修复失败」打了 %d 次，期望重查时不重复打；日志：%s", n, r.logs)
	}
	if len(r.connectAt) != 0 {
		t.Errorf("版本不支持却去连接了 CDP：%v", r.connectAt)
	}
}

// TestStartElevationGiveUp：UAC 被拒按 30 s、2 min 退避，第 3 次被拒即放弃提权，之后只调 ensurePatchedNoElevate、每 30 秒一次，
// ensurePatched 停在 3 次；酷狗没打开时报 waiting_process，detail 写明重启 PlayerCap 后重新请求，打开后报 standby / no_admin；
// 不提权的检查返回 nil（DLL 已由别的方式修补）时照常去等 CDP。
//
// 变异自证（均红）：放弃提权后仍调 ensurePatched（ensurePatched 多出调用）；放弃提权的分支改回 return；状态不经
// isKuGouRunning；重查等待直接 sleepOrStop(storeRecheck)；waitForCDP 里不报 standby（480 s 处缺 cdp_disconnected）。
func TestStartElevationGiveUp(t *testing.T) {
	sc := startScript{
		until:   499,
		running: between(465, 1e9),
		ensure:  failWith(errElevation),
		noElev: func(_ time.Duration, n int) error {
			if n <= 10 {
				return watchdog.ErrElevationNeeded
			}
			return nil
		},
	}
	r := runStartScript(t, sc)
	d := kugouDetails()
	r.check(t, sc, []timedStatus{
		at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
		at(150, waitingProcess, player.ReasonProcessNotRunning, i18n.T("KuGou not running; administrator rights not obtained, restart PlayerCap to request again")),
		at(466, standby, player.ReasonNoAdmin, i18n.T("Administrator rights not obtained; restart PlayerCap to request again")),
		at(480, standby, player.ReasonCDPDisconnected, d.Disconnected),
	}, secs(0, 30, 150), every(180, 480, 30))
	if len(r.connectAt) == 0 || r.connectAt[0] != 480*time.Second {
		t.Errorf("连接 CDP 于 %v，期望不提权的检查返回 nil 的 480 s 起照常去等", r.connectAt)
	}
	if n := strings.Count(r.logs, "[✗]"); n != 1 {
		t.Errorf("放弃提权的错误打了 %d 次，期望 1 次；日志：%s", n, r.logs)
	}
}

// TestStartBlockedWhileRunning：酷狗在运行时版本不支持、连续 5 轮找不到安装、提权已放弃，各报一条 standby 加对应 reason；
// 其间关掉酷狗，下一轮（2 s 内、不等 30 s 重查）改报 waiting_process / process_exited，detail 为该情形不在运行时的那条
// （找不到安装没有专门的一条，按普通的已退出）。第一轮修补检查之前酷狗在运行、还没有结论，报 standby / cdp_disconnected。
//
// 变异自证（均红）：重查等待直接 sleepOrStop(storeRecheck)（三个子例的退出都晚于 2 s 才报）；状态不经 isKuGouRunning
// （退出后仍报 standby）；seen 不维护（三个子例退出后都报 process_not_running）；ensureStop 分支改回 return（前两个
// 子例）；放弃提权的分支改回 return、放弃提权后仍调 ensurePatched（第三个子例）。
func TestStartBlockedWhileRunning(t *testing.T) {
	d := kugouDetails()
	cases := []struct {
		name     string
		sc       startScript
		statuses []timedStatus
		ensureAt []time.Duration
		noElevAt []time.Duration
	}{
		{
			name: "版本不支持",
			sc:   startScript{until: 59, running: between(0, 41), ensure: failWith(errVersion)},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, standby, player.ReasonVersionUnsupported, i18n.T("KuGou libcef.dll version unsupported")),
				at(42, waitingProcess, player.ReasonProcessExited, i18n.T("KuGou not running; this libcef.dll version is unsupported and cannot be read after it starts")),
			},
			ensureAt: secs(0, 30),
			noElevAt: []time.Duration{},
		},
		{
			// 第 5 轮（8 s）才受阻；38 s 重查仍找不到；61 s 关掉，68 s 重查时不在运行，按等它启动、每 2 s 重试。
			name: "找不到安装满 5 轮",
			sc:   startScript{until: 79, running: between(0, 61), ensure: failWith(watchdog.ErrInstallNotFound)},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(8, standby, player.ReasonInstallNotFound, i18n.T("KuGou installation not found")),
				at(62, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt: secs(0, 2, 4, 6, 8, 38, 68, 70, 72, 74, 76, 78),
			noElevAt: []time.Duration{},
		},
		{
			name: "提权已放弃",
			sc: startScript{until: 219, running: between(0, 201), ensure: failWith(errElevation),
				noElev: failWith(watchdog.ErrElevationNeeded)},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(150, standby, player.ReasonNoAdmin, i18n.T("Administrator rights not obtained; restart PlayerCap to request again")),
				at(202, waitingProcess, player.ReasonProcessExited, i18n.T("KuGou not running; administrator rights not obtained, restart PlayerCap to request again")),
			},
			ensureAt: secs(0, 30, 150),
			noElevAt: secs(180, 210),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runStartScript(t, c.sc)
			r.check(t, c.sc, c.statuses, c.ensureAt, c.noElevAt)
			if len(r.connectAt) != 0 {
				t.Errorf("受阻期间去连接了 CDP：%v", r.connectAt)
			}
		})
	}
}

// TestStartVersionUnsupportedRecovers：版本不支持期间换成受支持的版本（修补检查改为返回 nil），下一次重查即恢复去等 CDP。
//
// 变异自证（均红）：版本不支持的分支改回 return（不再重查）；waitForCDP 里不报 standby（60 s 处缺 cdp_disconnected）。
func TestStartVersionUnsupportedRecovers(t *testing.T) {
	sc := startScript{
		until:   69,
		running: always(true),
		ensure: func(now time.Duration, _ int) error {
			if now < 45*time.Second {
				return errVersion
			}
			return nil
		},
	}
	r := runStartScript(t, sc)
	d := kugouDetails()
	r.check(t, sc, []timedStatus{
		at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
		at(0, standby, player.ReasonVersionUnsupported, i18n.T("KuGou libcef.dll version unsupported")),
		at(60, standby, player.ReasonCDPDisconnected, d.Disconnected),
	}, secs(0, 30, 60), []time.Duration{})
	if want := every(60, 68, 2); !slices.Equal(r.connectAt, want) {
		t.Errorf("连接 CDP 于 %v，期望 %v", r.connectAt, want)
	}
}

// TestStartCDPNotReady：修补检查放行之后等 CDP。酷狗在运行而连不上报 standby / cdp_disconnected（不再与「未运行」合在
// 一条 waiting_process 里），等满 90 s 仍连不上照常重走修补检查、状态不变；酷狗从未运行报 process_not_running；连上过
// 之后退出报 process_exited。修补检查一直等到酷狗启动才返回时，等 CDP 期间即改报 standby，不停在循环顶部报的未运行。
//
// 变异自证（均红）：waitForCDP 里不报 standby（「等到酷狗启动」5 s 处缺 standby）；修补检查返回 nil 时不记 seen（「放行
// 而随即不在」报 process_not_running）；状态不经 isKuGouRunning；重查等待直接 sleepOrStop(storeRecheck)。只让判进程时
// 不记 seen 本例不红（修补检查返回 nil 时已记上），由 TestStartVersionUnsupportedFollowsProcess、TestStartBlockedWhileRunning
// 与 storegate_test.go 的 TestStartStoreInstallKeepsLooping 守。
func TestStartCDPNotReady(t *testing.T) {
	d := kugouDetails()
	cases := []struct {
		name     string
		sc       startScript
		statuses []timedStatus
		ensureAt []time.Duration
	}{
		{
			name:     "酷狗在运行而 CDP 一直未就绪",
			sc:       startScript{until: 101, running: always(true), ensure: failWith(nil)},
			statuses: []timedStatus{at(0, standby, player.ReasonCDPDisconnected, d.Disconnected)},
			ensureAt: secs(0, 94),
		},
		{
			// 修补检查报别的错误（照常等 CDP）：等 CDP 一见酷狗不在即返回，2 s 后重来。
			name:     "酷狗从未运行",
			sc:       startScript{until: 9, running: always(false), ensure: failWith(errOther)},
			statuses: []timedStatus{at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning)},
			ensureAt: secs(0, 2, 4, 6, 8),
		},
		{
			name: "连上过之后退出",
			sc:   startScript{until: 39, running: between(0, 31), ensure: failWith(nil)},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(32, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt: secs(0, 34, 36, 38),
		},
		{
			// 第一次修补检查等到 4.5 s 酷狗启动、5 s 才返回 nil（其间 2 s、4 s 的判进程都还没见到它）；11 s 酷狗退出。
			name: "修补检查等到酷狗启动",
			sc: startScript{until: 18, running: between(4.5, 11), ensure: func(_ time.Duration, n int) error {
				if n == 1 {
					time.Sleep(5 * time.Second)
					return nil
				}
				return errOther
			}},
			statuses: []timedStatus{
				at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
				at(5, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(11, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt: secs(0, 13, 15, 17),
		},
		{
			// 修补检查返回 nil 而随即判得酷狗不在（打完补丁结束酷狗、重新拉起的进程还没出现）：酷狗运行过，报 process_exited。
			name: "修补检查放行而酷狗随即不在",
			sc: startScript{until: 5, running: always(false), ensure: func(_ time.Duration, n int) error {
				if n == 1 {
					return nil
				}
				return errOther
			}},
			statuses: []timedStatus{
				at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
				at(0, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt: secs(0, 2, 4),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runStartScript(t, c.sc)
			r.check(t, c.sc, c.statuses, c.ensureAt, []time.Duration{})
			if len(r.connectAt) == 0 {
				t.Errorf("没有去等 CDP")
			}
		})
	}
}

// TestStartDeclaresDuringRepairCheck：修补检查长时间不返回（DLL 已修补时等酷狗启动，要修补时等 UAC 与酷狗启动）期间，
// 照常每 2 s 判进程、按修补检查之前的结论报状态：已报 standby 之后酷狗退出，2 s 内改报 waiting_process / process_exited，
// 再打开 2 s 内改回 standby；修补检查之前若已受阻，detail 用受阻那一条；检查期间见到的运行计入 seen。修补检查返回之后
// 照常由循环报。
//
// 变异自证（均红）：修补检查期间不报状态（三个子例）；检查期间按 30 s 判进程（三个子例）；检查期间不用受阻原因
// （「之前已受阻」）；检查期间判进程不记 seen（「检查期间打开又退出」报 process_not_running）；修补检查返回后不停止它
// （各 Start 用例的泡内留下 goroutine，-race 报数据竞争）。
func TestStartDeclaresDuringRepairCheck(t *testing.T) {
	d := kugouDetails()
	versionNotRunning := i18n.T("KuGou not running; this libcef.dll version is unsupported and cannot be read after it starts")
	cases := []struct {
		name      string
		sc        startScript
		statuses  []timedStatus
		ensureAt  []time.Duration
		connectAt []time.Duration
	}{
		{
			// 第一次修补检查从 0 s 等到 19 s 才返回 nil；酷狗 5 s 退出、15 s 再打开。
			name: "没有受阻",
			sc: startScript{
				until:   24,
				running: func(now time.Duration) bool { return now < 5*time.Second || now >= 15*time.Second },
				ensure: func(_ time.Duration, n int) error {
					if n == 1 {
						time.Sleep(19 * time.Second)
					}
					return nil
				},
			},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(6, waitingProcess, player.ReasonProcessExited, d.Exited),
				at(16, standby, player.ReasonCDPDisconnected, d.Disconnected),
			},
			ensureAt:  secs(0),
			connectAt: every(19, 23, 2),
		},
		{
			// 0 s 判出版本不支持；30 s 重查时那次修补检查等到 49 s 才返回 nil；酷狗 35 s 退出。
			name: "之前已受阻",
			sc: startScript{
				until:   58,
				running: between(0, 35),
				ensure: func(_ time.Duration, n int) error {
					switch n {
					case 1:
						return errVersion
					case 2:
						time.Sleep(19 * time.Second)
						return nil
					}
					return errOther
				},
			},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, standby, player.ReasonVersionUnsupported, i18n.T("KuGou libcef.dll version unsupported")),
				at(36, waitingProcess, player.ReasonProcessExited, versionNotRunning),
				at(49, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt:  secs(0, 30, 51, 53, 55, 57),
			connectAt: secs(49, 51, 53, 55, 57),
		},
		{
			// 循环顶部判为未运行；修补检查从 0 s 等到 11 s，其间酷狗 3 s 打开、7 s 退出，只有检查期间的判进程见过它。
			name: "检查期间打开又退出",
			sc: startScript{
				until:   16,
				running: between(3, 7),
				ensure: func(_ time.Duration, n int) error {
					if n == 1 {
						time.Sleep(11 * time.Second)
					}
					return errOther
				},
			},
			statuses: []timedStatus{
				at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
				at(4, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(8, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt:  secs(0, 13, 15),
			connectAt: secs(11, 13, 15),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runStartScript(t, c.sc)
			r.check(t, c.sc, c.statuses, c.ensureAt, []time.Duration{})
			if !slices.Equal(r.connectAt, c.connectAt) {
				t.Errorf("连接 CDP 于 %v，期望 %v", r.connectAt, c.connectAt)
			}
		})
	}
}
