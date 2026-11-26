package cloudmusic

// 本文件经桩离线跑 Start 的外层循环，只守非歌名状态（#231，设计 doc/design/converge-player-behavior-231-design.md §3.2、
// §3.3 cloudmusicv3 行）：每轮在 EnsureDebugMode 之后判一次网易云是否在运行，不在运行报 waiting_process（见过它运行为
// process_exited，否则 process_not_running），在运行而读不了报 standby 加原因；v2 与商店版按 30 秒重查，其间每 2 秒判进程；
// 非歌名状态只在变化时发。连上之后经 runSession 守连上即报没有歌（先状态、后 clear_song_data）与会话结束处的状态。
//
// Start 在 testing/synctest 的泡里跑，等待是假时钟：EnsureDebugMode、判进程、连接 CDP 都换成按假时钟取值的桩，注册表的
// 自启参数补丁换成空操作。连接一般失败；连上时给出按假时钟取快照的客户端桩（fakeClient），到期即断开，runSession 照常
// 轮询、照常结束。测试 goroutine 每 100 ms 取走已发出的事件，按取到的时刻记；各桩的取值、Start 的等待与 runSession 的
// 轮询都落在 100 ms 的整数倍上，记下的时刻即发出的时刻。
//
// 变异自证（在临时副本里逐条注入、跑完即还原，均实测）记在用例的注释里。

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
	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/watchdog"
	"Metabox-Nexus-PlayerCap/player/launch"
)

// startScript 是一次 Start 的剧本，各桩按自 Start 开始计的假时钟 now 取值。
type startScript struct {
	until   float64                                      // 到这一刻（秒）Stop
	running func(now time.Duration) bool                 // isRunning
	ensure  func(now time.Duration, n int) (bool, error) // ensureDebugMode 的第 n 次（从 1 起）调用；nil 即恒为 (false, nil)
	connect func(now time.Duration) *fakeClient          // connectCDP 连上时给出的客户端；返回 nil 或 connect 为 nil 即连接失败
}

// startRun 是一次 Start 的记录。
type startRun struct {
	statuses  []timedStatus
	events    []timedEvent    // 全部事件，按发出的先后
	ensureAt  []time.Duration // ensureDebugMode 各次调用的时刻
	connectAt []time.Duration // connectCDP 各次调用的时刻
	returned  bool            // Start 在 Stop 之前返回
	logs      string
}

type timedEvent struct {
	at  time.Duration
	typ string
}

// fakeClient 是按假时钟取快照的 CDP 客户端桩。snap 给出 Extract 的快照；到 end 秒起连接断开：Extract 报错、IsClosed 为真。
// 取词与封面一律取不到，不发网络请求。
type fakeClient struct {
	snap   func() *cdp.ExtractionData
	end    float64
	now    func() time.Duration // 由 runStartScript 填
	closed bool
}

func (c *fakeClient) Extract() (*cdp.ExtractionData, error) {
	if c.now() >= time.Duration(c.end*float64(time.Second)) {
		c.closed = true
		return nil, errors.New("websocket: close 1006 (abnormal closure)")
	}
	return c.snap(), nil
}
func (c *fakeClient) IsClosed() bool                 { return c.closed }
func (c *fakeClient) ForceFetchLyricsInRedux() error { return nil }
func (c *fakeClient) FetchLyricsViaCDP(string) (*cdp.LyricFetchResult, error) {
	return nil, errors.New("lyrics unavailable in test")
}
func (c *fakeClient) FetchCoverViaCDP(string) (string, error) { return "", nil }
func (c *fakeClient) ExtractTlyricLines() map[int]string      { return nil }

// noSong 是连上而没有当前歌曲的快照：CurPlaying 为空。DomTimeSec 为 -1 即界面进度不可用（不做 seek 检测与对表）。
func noSong() *cdp.ExtractionData { return &cdp.ExtractionData{DomTimeSec: -1} }

// playingSong 是正在播放一首合成歌曲的快照；Redux ID 非空，换歌时不按歌名搜索。
func playingSong() *cdp.ExtractionData {
	d := newSnapshot("合成歌名", "合成歌手")
	d.CurPlaying.ID = "1"
	d.PlayingState = 2
	d.DomTimeSec = -1
	return d
}

// pausedSong 是暂停中的同一首歌。
func pausedSong() *cdp.ExtractionData {
	d := playingSong()
	d.PlayingState = 1
	return d
}

const songTitle = "合成歌名 - 合成歌手"

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

// between 返回在 [from, to) 秒之间为真的 running 桩。
func between(from, to float64) func(time.Duration) bool {
	return func(now time.Duration) bool {
		return now >= time.Duration(from*float64(time.Second)) && now < time.Duration(to*float64(time.Second))
	}
}

func always(b bool) func(time.Duration) bool { return func(time.Duration) bool { return b } }

// whileRunning 返回只在网易云运行时给出 err 的 ensure 桩：EnsureDebugMode 只在有进程时判出版本、商店版与读不到路径。
func whileRunning(running func(time.Duration) bool, err error) func(time.Duration, int) (bool, error) {
	return func(now time.Duration, _ int) (bool, error) {
		if running(now) {
			return false, err
		}
		return false, nil
	}
}

// runStartScript 按剧本跑 Start 到 sc.until 再 Stop，返回记录。
func runStartScript(t *testing.T, sc startScript) startRun {
	t.Helper()
	op, oe, or, oc := patchAutoStart, ensureDebugMode, isRunning, connectCDP
	defer func() { patchAutoStart, ensureDebugMode, isRunning, connectCDP = op, oe, or, oc }()

	var buf bytes.Buffer
	w := stdlog.Writer()
	stdlog.SetOutput(&buf)
	defer stdlog.SetOutput(w)

	var r startRun
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		t0 := time.Now()
		now := func() time.Duration { return time.Since(t0) }

		patchAutoStart = func() {}
		isRunning = func() bool { return sc.running(now()) }
		ensureDebugMode = func() (bool, error) {
			r.ensureAt = append(r.ensureAt, now())
			if sc.ensure == nil {
				return false, nil
			}
			return sc.ensure(now(), len(r.ensureAt))
		}
		connectCDP = func(context.Context) (sessionClient, error) {
			r.connectAt = append(r.connectAt, now())
			if sc.connect != nil {
				if c := sc.connect(now()); c != nil {
					c.now = now
					return c, nil
				}
			}
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

// drain 取走通道里已有的事件，记下其类型与其中的状态。
func (r *startRun) drain(p *CloudMusicPlayer, now time.Duration) {
	for {
		select {
		case e := <-p.EventCh:
			r.events = append(r.events, timedEvent{now, e.Type})
			if e.Type == player.EventStatusUpdate {
				r.statuses = append(r.statuses, timedStatus{now, *e.Data.(*player.StatusInfo)})
			}
		default:
			return
		}
	}
}

const (
	waitingProcess = "waiting_process"
	standby        = "standby"
	waitingSong    = "waiting_song"
)

// 合成的数据：版本号与商店版路径都不是真实安装。
var (
	errV2    = &watchdog.UnsupportedVersionError{Version: "2.0.0.1", ExePath: `C:\合成\CloudMusic\cloudmusic.exe`}
	errStore = &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\合成.CloudMusic_1.0.0.0_x64__0000000000000\cloudmusic.exe`}
)

// TestStartStatus：外层循环按进程层次报的非歌名状态序列。所有子例共守的反例：Start 在 Stop 之前返回；非歌名状态没有 reason；
// 网易云不在运行而报 standby；在运行而报 waiting_process（含读不到路径报 waiting_process / no_admin 的旧写法）。
//
// 变异自证（均实测变红，括号里是变红的子例）：
//   - v2 分支改回 p.Emit（「v2 …」，另有 player/waitingstatus_lint_test.go 的门禁红）；商店版分支同改（「商店版」）。
//   - v2 的等待改回 time.Sleep(unsupportedRecheck)（「v2 …」）；商店版同改（「商店版」）。
//   - 会话结束无条件报 standby（「会话结束而进程已退出」）。
//   - 会话结束改为 clear_song_data 在前、状态在后（两个「会话结束…」与「宣告过歌、断开后重连…」）。
//   - runSession 去掉连上时的没有歌判断（「会话结束而进程已退出」「宣告过歌、断开后重连…」）。
//   - 没有歌时不发 clear_song_data、或先清空后状态（同上两个子例）。
//   - 换歌拍改回只在 PlayingState 为 2 时发状态（「暂停中连上」）。
//   - 不维护 seen（「未运行 → 启动 → 退出 → 再启动」「运行中而 CDP 连不上，之后退出」「v2 …」「商店版」「读不到路径而网易云随后退出」
//     「会话结束而进程已退出」）。
//   - 去掉不在运行时的分支、照常去连 CDP（「从未运行」等七个子例）。
//   - 连 CDP 之前不报 standby（九个子例）；改为只在连接失败时报（「带参数重启」「会话结束而进程已退出」「暂停中连上」
//     「宣告过歌、断开后重连…」：连上之前、等 CEF 期间都该报）。
//   - 读不到路径时按不在运行合成（两个「读不到路径…」）。
//   - v2、商店版、读不到路径的警告每轮都打（「v2 …」「商店版」、两个「读不到路径…」）。
func TestStartStatus(t *testing.T) {
	d := cloudmusicDetails()
	v2Detail := fmt.Sprintf(i18n.T("NetEase Cloud Music v%s unsupported (requires v3+)"), errV2.Version)
	storeDetail := i18n.T("NetEase Cloud Music Microsoft Store version unsupported (requires the official installer version)")
	noAdminDetail := i18n.T("Cannot read the NetEase Cloud Music installation path (NetEase Cloud Music may be running as administrator)")
	noSongDetail := i18n.T("NetEase Cloud Music has no current song")

	upDown := func(now time.Duration) bool { // 3 s 启动、7 s 退出、11 s 再启动
		return between(3, 7)(now) || now >= 11*time.Second
	}
	v2Running := between(0, 101)
	storeRunning := between(0, 41)
	exitedRunning := between(0, 5)

	cases := []struct {
		name       string
		sc         startScript
		statuses   []timedStatus
		ensureAt   []time.Duration // ensureDebugMode 各次调用的时刻
		connectAt  []time.Duration // connectCDP 各次调用的时刻
		warns      int             // 日志里 Warn 的条数
		clearAfter []float64       // 这些时刻（秒）发出的第一条状态之后须在同一时刻紧跟 clear_song_data
	}{
		{
			name:      "从未运行",
			sc:        startScript{until: 9, running: always(false)},
			statuses:  []timedStatus{at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning)},
			ensureAt:  every(0, 8, 2),
			connectAt: []time.Duration{},
			warns:     0,
		},
		{
			name: "未运行 → 启动 → 退出 → 再启动",
			sc:   startScript{until: 15, running: upDown},
			statuses: []timedStatus{
				at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
				at(4, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(8, waitingProcess, player.ReasonProcessExited, d.Exited),
				at(12, standby, player.ReasonCDPDisconnected, d.Disconnected),
			},
			ensureAt:  every(0, 14, 2),
			connectAt: secs(4, 6, 12, 14),
			warns:     0,
		},
		{
			name: "运行中而 CDP 连不上，之后退出",
			sc:   startScript{until: 15, running: between(0, 11)},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(12, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt:  every(0, 14, 2),
			connectAt: every(0, 10, 2),
			warns:     0,
		},
		{
			// 0 s 判出 v2，30、60、90 s 三次重查仍是 v2；101 s 关掉，重查等待在 102 s 的那一步即察觉。
			name: "v2 持续 3 次重查只发一条，等待中进程消失",
			sc:   startScript{until: 109, running: v2Running, ensure: whileRunning(v2Running, errV2)},
			statuses: []timedStatus{
				at(0, standby, player.ReasonVersionUnsupported, v2Detail),
				at(102, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt:  secs(0, 30, 60, 90, 102, 104, 106, 108),
			connectAt: []time.Duration{},
			warns:     1,
		},
		{
			name: "商店版",
			sc:   startScript{until: 49, running: storeRunning, ensure: whileRunning(storeRunning, errStore)},
			statuses: []timedStatus{
				at(0, standby, player.ReasonVersionUnsupported, storeDetail),
				at(42, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt:  secs(0, 30, 42, 44, 46, 48),
			connectAt: []time.Duration{},
			warns:     1,
		},
		{
			// 9 s 之后路径读得到了：照常去连，连不上报 cdp_disconnected。
			name: "读不到路径报 no_admin，路径读到后恢复",
			sc: startScript{until: 15, running: always(true), ensure: func(now time.Duration, _ int) (bool, error) {
				if now < 9*time.Second {
					return false, launch.ErrExeUnknown
				}
				return false, nil
			}},
			statuses: []timedStatus{
				at(0, standby, player.ReasonNoAdmin, noAdminDetail),
				at(10, standby, player.ReasonCDPDisconnected, d.Disconnected),
			},
			ensureAt:  every(0, 14, 2),
			connectAt: secs(10, 12, 14),
			warns:     1,
		},
		{
			name: "读不到路径而网易云随后退出",
			sc:   startScript{until: 9, running: exitedRunning, ensure: whileRunning(exitedRunning, launch.ErrExeUnknown)},
			statuses: []timedStatus{
				at(0, standby, player.ReasonNoAdmin, noAdminDetail),
				at(6, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt:  every(0, 8, 2),
			connectAt: []time.Duration{},
			warns:     1,
		},
		{
			// 第一轮带参数重启了网易云：等 5 s 再连，其间已报 standby；之后照常每 2 s 重连。
			name: "带参数重启",
			sc: startScript{until: 8, running: always(true), ensure: func(_ time.Duration, n int) (bool, error) {
				return n == 1, nil
			}},
			statuses:  []timedStatus{at(0, standby, player.ReasonCDPDisconnected, d.Disconnected)},
			ensureAt:  secs(0, 7),
			connectAt: secs(5, 7),
			warns:     0,
		},
		{
			// 0 s 连上（连上之前先报 standby），一直没有歌；网易云在 10 s 退出，连接随之断开。
			name: "会话结束而进程已退出",
			sc: startScript{until: 15, running: between(0, 10), connect: func(now time.Duration) *fakeClient {
				if now == 0 {
					return &fakeClient{snap: noSong, end: 10}
				}
				return nil
			}},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0.1, waitingSong, player.ReasonNoSong, noSongDetail),
				at(10, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt:   secs(0, 10, 12, 14),
			connectAt:  secs(0),
			warns:      1, // 连接断开时的取数错误
			clearAfter: []float64{0.1, 10},
		},
		{
			// 4 s 才连上（此前报 standby），在播一首歌；10 s 连接断开而网易云仍在运行。
			name: "会话结束而进程仍在运行",
			sc: startScript{until: 15, running: always(true), connect: func(now time.Duration) *fakeClient {
				if now == 4*time.Second {
					return &fakeClient{snap: playingSong, end: 10}
				}
				return nil
			}},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(4.4, "playing", "", songTitle),
				at(10, standby, player.ReasonCDPDisconnected, d.Disconnected),
			},
			ensureAt:   secs(0, 2, 4, 10, 12, 14),
			connectAt:  secs(0, 2, 4, 10, 12, 14),
			warns:      2, // 取词失败、连接断开时的取数错误
			clearAfter: []float64{10},
		},
		{
			// 网易云暂停中连上：换歌拍按播放态报 paused（设计 4.3 第 2 条）。
			name: "暂停中连上",
			sc: startScript{until: 3, running: always(true), connect: func(now time.Duration) *fakeClient {
				if now == 0 {
					return &fakeClient{snap: pausedSong, end: 1e9}
				}
				return nil
			}},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0.4, "paused", "", songTitle),
				at(3, standby, player.ReasonCDPDisconnected, d.Disconnected),
			},
			ensureAt:   secs(0),
			connectAt:  secs(0),
			warns:      1, // 取词失败
			clearAfter: []float64{3},
		},
		{
			// 先宣告一首歌，5 s 连接断开；随即重连，此后一直没有歌（设计 4.5：断开时画面按决定保留，重连后真停播须清屏）。
			name: "宣告过歌、断开后重连而一直没有歌",
			sc: startScript{until: 9, running: always(true), connect: func(now time.Duration) *fakeClient {
				switch now {
				case 0:
					return &fakeClient{snap: playingSong, end: 5}
				case 5 * time.Second:
					return &fakeClient{snap: noSong, end: 1e9}
				}
				return nil
			}},
			statuses: []timedStatus{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0.4, "playing", "", songTitle),
				at(5, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(5.1, waitingSong, player.ReasonNoSong, noSongDetail),
				at(9, standby, player.ReasonCDPDisconnected, d.Disconnected),
			},
			ensureAt:   secs(0, 5),
			connectAt:  secs(0, 5),
			warns:      2, // 取词失败、第一次连接断开时的取数错误
			clearAfter: []float64{5, 5.1, 9},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runStartScript(t, c.sc)
			if r.returned {
				t.Errorf("Start 在 Stop 之前返回了")
			}
			for _, s := range r.statuses {
				switch {
				case s.s.Status == "playing" || s.s.Status == "paused":
				case s.s.Reason == "":
					t.Errorf("非歌名状态没有 reason：%v", s)
				case s.s.Status == standby && !c.sc.running(s.at):
					t.Errorf("网易云不在运行而报 standby：%v", s)
				case s.s.Status == waitingProcess && c.sc.running(s.at):
					t.Errorf("网易云在运行而报 waiting_process：%v", s)
				}
			}
			if !slices.Equal(r.statuses, c.statuses) {
				t.Errorf("状态序列\n got  %v\n want %v", r.statuses, c.statuses)
			}
			if !slices.Equal(r.ensureAt, c.ensureAt) {
				t.Errorf("EnsureDebugMode 调用于 %v，期望 %v", r.ensureAt, c.ensureAt)
			}
			if !slices.Equal(r.connectAt, c.connectAt) {
				t.Errorf("连接 CDP 于 %v，期望 %v", r.connectAt, c.connectAt)
			}
			if n := strings.Count(r.logs, "[!]"); n != c.warns {
				t.Errorf("Warn 打了 %d 条，期望 %d 条；日志：%s", n, c.warns, r.logs)
			}
			for _, sec := range c.clearAfter {
				when := time.Duration(sec * float64(time.Second))
				i := slices.IndexFunc(r.events, func(e timedEvent) bool { return e.at == when && e.typ == player.EventStatusUpdate })
				if i < 0 || i+1 >= len(r.events) || r.events[i+1] != (timedEvent{when, player.EventClearSongData}) {
					t.Errorf("%v 的状态之后应在同一时刻紧跟 clear_song_data；事件 %v", when, r.events[max(i-2, 0):min(i+4, len(r.events))])
				}
			}
		})
	}
}

// TestStartStopInterruptsConnect：连接挂住（9222 端口接受连接而不完成响应）时，Stop 即中止这次连接；Start 照旧等完连接失败后的
// 2 s，回到循环顶部见到停止即返回。连接的桩一直挂到 ctx 结束。
//
// 变异自证（均实测变红）：连接改传 context.Background()（泡内死锁）；停止时不取消 ctx（同上）。
func TestStartStopInterruptsConnect(t *testing.T) {
	op, oe, or, oc := patchAutoStart, ensureDebugMode, isRunning, connectCDP
	defer func() { patchAutoStart, ensureDebugMode, isRunning, connectCDP = op, oe, or, oc }()

	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		patchAutoStart = func() {}
		ensureDebugMode = func() (bool, error) { return false, nil }
		isRunning = func() bool { return true }
		connects := 0
		connectCDP = func(ctx context.Context) (sessionClient, error) {
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
		if el := time.Since(t0); el != 2*time.Second || connects != 1 {
			t.Errorf("Stop 之后 %v 返回、连接 %d 次，期望中止挂住的那次连接、2 s 后返回、只连 1 次", el, connects)
		}
	})
}
