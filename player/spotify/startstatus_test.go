package spotify

// 本文件经桩离线跑 Start 的外层循环与 runSession，只守非歌名状态（#231，设计 doc/design/converge-player-behavior-231-design.md
// §3.2、§3.3、4.3、4.4 spotify 阶段一）：每轮按 EnsureDebugMode 的结论声明当前层次，ErrNotRunning 报 waiting_process（见过它
// 运行为 process_exited，否则 process_not_running），其余结论报 standby 加原因；版本不支持与商店版按 30 秒重探，其间每 3 秒
// 判进程；非歌名状态只在变化时发，且同拍紧跟 clear_song_data。连上之后本会话还没宣告过歌时，没有曲目报 waiting_song，
// 曲目在而读不到歌名报 standby / metadata_unavailable；宣告过歌之后歌名变空不发事件。
//
// Start 在 testing/synctest 的泡里跑，等待是假时钟：EnsureDebugMode、找主进程、连接 CDP 换成按假时钟取值的桩，注册表的
// 自启参数补丁换成空操作，spotify_cli 一律找不到（不调用它、不取词）。连上时给出按假时钟取快照的客户端桩（fakeClient），
// 到期即断开。测试 goroutine 每 100 ms 取走已发出的事件，按取到的时刻记；各桩的取值、Start 的等待与 runSession 的轮询
// （200 ms）都落在 100 ms 的整数倍上，记下的时刻即发出的时刻。
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
	"Metabox-Nexus-PlayerCap/player/launch"
	"Metabox-Nexus-PlayerCap/player/spotify/cdp"
	"Metabox-Nexus-PlayerCap/player/spotify/watchdog"
)

func secDur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// startScript 是一次 Start 的剧本，各桩按自 Start 开始计的假时钟 now 取值。
type startScript struct {
	until   float64                             // 到这一刻（秒）Stop；只比较此前发出的状态
	running func(now time.Duration) bool        // 找主进程是否不返回 ErrNotRunning
	ensure  func(now time.Duration) error       // Spotify 在运行时 EnsureDebugMode 的结论；nil 即恒为 nil（不在运行时一律 ErrNotRunning）
	connect func(now time.Duration) *fakeClient // 连上时给出的客户端；返回 nil 或 connect 为 nil 即连接失败
}

// startRun 是一次 Start 的记录。
type startRun struct {
	events    []timedEvent    // 状态与 clear_song_data，按发出的先后
	ensureAt  []time.Duration // ensureDebugMode 各次调用的时刻
	connectAt []time.Duration // connectCDP 各次调用的时刻
	returned  bool            // Start 在 Stop 之前返回
	logs      string
}

type timedEvent struct {
	at time.Duration
	s  *player.StatusInfo // nil 即 clear_song_data
}

func (e timedEvent) String() string {
	if e.s == nil {
		return fmt.Sprintf("%v clear", e.at)
	}
	return fmt.Sprintf("%v %s/%s/%s", e.at, e.s.Status, e.s.Reason, e.s.Detail)
}

// fakeClient 是按假时钟取快照的 CDP 客户端桩。snap 给出各时刻的快照；到 end 秒起连接断开：Snapshot 报错、IsClosed 为真。
// 取词一律取不到，不发网络请求。
type fakeClient struct {
	snap   func(now time.Duration) *cdp.Snapshot
	end    float64
	now    func() time.Duration // 由 runStartScript 填
	closed bool
}

func (c *fakeClient) Snapshot() (*cdp.Snapshot, error) {
	if c.now() >= secDur(c.end) {
		c.closed = true
		return nil, errors.New("websocket: close 1006 (abnormal closure)")
	}
	return c.snap(c.now()), nil
}
func (c *fakeClient) IsClosed() bool      { return c.closed }
func (c *fakeClient) Close()              { c.closed = true }
func (c *fakeClient) Token() string       { return "" }
func (c *fakeClient) AppPlatform() string { return "" }
func (c *fakeClient) AppVersion() string  { return "" }
func (c *fakeClient) FetchLyrics(string, string) (string, error) {
	return "", errors.New("lyrics unavailable in test")
}
func (c *fakeClient) FetchViaPageCache(string) (string, error) {
	return "", errors.New("lyrics unavailable in test")
}

// 合成的快照。named 是正在播放《合成歌名》的一拍；noTrack 是没有曲目（首页、刚启动：没有进度条，图标为播放三角）；
// unnamed 是曲目在而歌名读不到（时长有效、在播放）。
func named(time.Duration) *cdp.Snapshot {
	return &cdp.Snapshot{OK: true, Title: "合成歌名", Artists: []string{"合成歌手"}, DurationMs: 200000, PositionMs: 42000, State: "playing"}
}
func noTrack(time.Duration) *cdp.Snapshot { return &cdp.Snapshot{OK: true, State: "paused"} }
func unnamed(now time.Duration) *cdp.Snapshot {
	return &cdp.Snapshot{OK: true, DurationMs: 200000, PositionMs: 42000 + float64(now/time.Second*1000), State: "playing"}
}

const songTitle = "合成歌名 - 合成歌手"

// sequence 把 a、b 两段快照拼成一段：from 秒之前取 a，之后取 b。
func sequence(a func(time.Duration) *cdp.Snapshot, from float64, b func(time.Duration) *cdp.Snapshot) func(time.Duration) *cdp.Snapshot {
	return func(now time.Duration) *cdp.Snapshot {
		if now < secDur(from) {
			return a(now)
		}
		return b(now)
	}
}

// between 返回在 [from, to) 秒之间为真的 running 桩。
func between(from, to float64) func(time.Duration) bool {
	return func(now time.Duration) bool { return now >= secDur(from) && now < secDur(to) }
}

func always(b bool) func(time.Duration) bool { return func(time.Duration) bool { return b } }

// constErr 返回恒给出 err 的 ensure 桩（只在 Spotify 运行时被问到）。
func constErr(err error) func(time.Duration) error { return func(time.Duration) error { return err } }

// connectOnce 返回只在给定各秒的那一次连接给出客户端的 connect 桩；其余各次连接失败。
func connectOnce(clients map[float64]func() *fakeClient) func(time.Duration) *fakeClient {
	return func(now time.Duration) *fakeClient {
		for sec, mk := range clients {
			if now == secDur(sec) {
				return mk()
			}
		}
		return nil
	}
}

// runStartScript 按剧本跑 Start 到 sc.until 再 Stop，返回记录。
func runStartScript(t *testing.T, sc startScript) startRun {
	t.Helper()
	op, oe, of, oc, ol := patchAutoStart, ensureDebugMode, findMainPID, connectCDP, findCLI
	defer func() { patchAutoStart, ensureDebugMode, findMainPID, connectCDP, findCLI = op, oe, of, oc, ol }()

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
		findCLI = func() (string, error) { return "", errors.New("spotify_cli.exe not found") }
		findMainPID = func() (int, error) {
			if sc.running(now()) {
				return 4242, nil
			}
			return 0, watchdog.ErrNotRunning
		}
		ensureDebugMode = func() error {
			r.ensureAt = append(r.ensureAt, now())
			switch {
			case !sc.running(now()):
				return watchdog.ErrNotRunning
			case sc.ensure != nil:
				return sc.ensure(now())
			}
			return nil
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
			if now() >= secDur(sc.until) {
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

// drain 取走通道里已有的事件，记下状态与 clear_song_data。
func (r *startRun) drain(p *SpotifyPlayer, now time.Duration) {
	for {
		select {
		case e := <-p.EventCh:
			switch e.Type {
			case player.EventStatusUpdate:
				r.events = append(r.events, timedEvent{now, e.Data.(*player.StatusInfo)})
			case player.EventClearSongData:
				r.events = append(r.events, timedEvent{now, nil})
			}
		default:
			return
		}
	}
}

// statusesBefore 返回 until 之前发出的状态。
func (r startRun) statusesBefore(until time.Duration) []string {
	var out []string
	for _, e := range r.events {
		if e.s != nil && e.at < until {
			out = append(out, e.String())
		}
	}
	return out
}

// at 构造期望的一条状态。
func at(sec float64, status, reason, detail string) string {
	return timedEvent{secDur(sec), &player.StatusInfo{Status: status, Reason: reason, Detail: detail}}.String()
}

func secs(ss ...float64) []time.Duration {
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		out[i] = secDur(s)
	}
	return out
}

const (
	waitingProcess = "waiting_process"
	standby        = "standby"
	waitingSong    = "waiting_song"
)

// 合成的数据：商店版路径不是真实安装。
var errStore = &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\合成.Spotify_1.0.0.0_x86__0000000000000\Spotify.exe`}

// TestStartStatus：外层循环与连上之后第一拍的非歌名状态序列。所有子例共守的反例：Start 在 Stop 之前返回；非歌名状态没有
// reason；Spotify 不在运行而报 standby；在运行而报 waiting_process；非歌名状态之后同一时刻没有紧跟 clear_song_data。
//
// 变异自证（均实测变红，括号里是变红的子例）：
//   - 外层循环的状态改回 p.Emit 发（十个子例，只有一轮外层循环的五个不红；另有 player/waitingstatus_lint_test.go 的门禁红）。
//   - 版本不支持、商店版的等待改回 sleepOrStop(unsupportedRecheck)（「版本不支持…」「商店版…」）。
//   - 不维护 seen（「未运行 → 端口未就绪 → 退出…」「版本不支持…」「商店版…」「读不到路径而 Spotify 随后退出」「会话结束而进程已退出」）。
//   - 端口未就绪按不在运行合成（「未运行 → 端口未就绪 → 退出…」）；watchdog 的其他错误按不在运行合成（「watchdog 的其他错误…」）。
//   - 读不到路径按不在运行合成（两个「读不到路径…」）。
//   - 会话结束无条件报 standby（「会话结束而进程已退出」）。
//   - 会话结束先清空后状态（「会话结束而进程已退出」「会话结束而进程仍在运行」「宣告过歌、断开后重连…」）。
//   - 连上时 unnamed 改报 waiting_song（「曲目在而歌名读不到…」「图标认不出、位置推进…」）；没有曲目改报 metadata_unavailable
//     （「连上没有曲目」、两个「图标认不出…」「会话结束而进程已退出」「宣告过歌、断开后重连…」）。
//   - runSession 去掉连上时的声明、照旧静默（「连上没有曲目」「曲目在而歌名读不到…」、两个「图标认不出…」「会话结束而进程已退出」
//     「宣告过歌、断开后重连…」）；声明之后不发 clear_song_data（同这六个子例）。
//   - 会话第一个位置样本也算推进；分类改回用宣告的播放态（首个样本算推进的那一份）（两者都是两个「图标认不出…」，#231 代码审）；
//     classifyTrack 去掉播放态（「图标认不出、位置推进…」）。
//   - 宣告过歌之后歌名变空也按连上时声明（「宣告过歌之后歌名变空」）。
//   - 版本不支持的警告每轮都打（「版本不支持…」的 Warn 条数）。
func TestStartStatus(t *testing.T) {
	d := spotifyDetails()
	vuDetail := i18n.T("Spotify version unsupported (remote debugging unavailable)")
	storeDetail := i18n.T("Spotify Microsoft Store version unsupported (requires the official installer version)")
	noAdminDetail := i18n.T("Cannot read the Spotify installation path (Spotify may be running as administrator)")
	noTrackDetail := i18n.T("Spotify has no current track")
	unnamedDetail := i18n.T("Cannot read the Spotify track title")

	upDown := func(now time.Duration) bool { return between(3, 8)(now) || now >= 12*time.Second }

	cases := []struct {
		name      string
		sc        startScript
		statuses  []string
		ensureAt  []time.Duration // nil 即不查
		connectAt []time.Duration // nil 即不查
		warns     int             // 日志里 Warn 的条数；-1 即不查
		quietFrom float64         // >0 时：这一刻起到 until 不应再有状态或 clear_song_data
	}{
		{
			name:      "从未运行",
			sc:        startScript{until: 10, running: always(false)},
			statuses:  []string{at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning)},
			ensureAt:  secs(0, 3, 6, 9),
			connectAt: secs(),
			warns:     0,
		},
		{
			// 3 s 启动、端口一直未就绪（1 s 退避），8 s 退出；12 s 再启动，端口就绪而连不上（2 s 退避）。
			name: "未运行 → 端口未就绪 → 退出 → 再启动而连不上",
			sc: startScript{until: 17, running: upDown, ensure: func(now time.Duration) error {
				if now < 12*time.Second {
					return watchdog.ErrPortNotReady
				}
				return nil
			}},
			statuses: []string{
				at(0, waitingProcess, player.ReasonProcessNotRunning, d.NotRunning),
				at(3, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(8, waitingProcess, player.ReasonProcessExited, d.Exited),
				at(14, standby, player.ReasonCDPDisconnected, d.Disconnected),
			},
			ensureAt:  secs(0, 3, 4, 5, 6, 7, 8, 11, 14, 16),
			connectAt: secs(14, 16),
			warns:     2, // 两次连接失败
		},
		{
			// 0 s 判出版本不支持，30、60、90 s 三次重探仍是；101 s 关掉，重探等待在 102 s 的那一步即察觉。
			name:      "版本不支持持续 3 次重探只发一条，等待中进程消失",
			sc:        startScript{until: 109, running: between(0, 101), ensure: constErr(watchdog.ErrVersionUnsupported)},
			statuses:  []string{at(0, standby, player.ReasonVersionUnsupported, vuDetail), at(102, waitingProcess, player.ReasonProcessExited, d.Exited)},
			ensureAt:  secs(0, 30, 60, 90, 102, 105, 108),
			connectAt: secs(),
			warns:     1,
		},
		{
			name:      "商店版，等待中进程消失",
			sc:        startScript{until: 49, running: between(0, 41), ensure: constErr(errStore)},
			statuses:  []string{at(0, standby, player.ReasonVersionUnsupported, storeDetail), at(42, waitingProcess, player.ReasonProcessExited, d.Exited)},
			ensureAt:  secs(0, 30, 42, 45, 48),
			connectAt: secs(),
			warns:     1,
		},
		{
			// 9 s 起路径读得到了：照常去连，连不上报 cdp_disconnected。
			name: "读不到路径报 no_admin，路径读到后恢复",
			sc: startScript{until: 12, running: always(true), ensure: func(now time.Duration) error {
				if now < 9*time.Second {
					return launch.ErrExeUnknown
				}
				return nil
			}},
			statuses:  []string{at(0, standby, player.ReasonNoAdmin, noAdminDetail), at(9, standby, player.ReasonCDPDisconnected, d.Disconnected)},
			ensureAt:  secs(0, 3, 6, 9, 11),
			connectAt: secs(9, 11),
			warns:     3, // 读不到路径一次、两次连接失败
		},
		{
			name:      "读不到路径而 Spotify 随后退出",
			sc:        startScript{until: 8, running: between(0, 5), ensure: constErr(launch.ErrExeUnknown)},
			statuses:  []string{at(0, standby, player.ReasonNoAdmin, noAdminDetail), at(6, waitingProcess, player.ReasonProcessExited, d.Exited)},
			ensureAt:  secs(0, 3, 6),
			connectAt: secs(),
			warns:     1,
		},
		{
			name:      "watchdog 的其他错误按在运行报 standby",
			sc:        startScript{until: 8, running: always(true), ensure: constErr(errors.New("合成的 watchdog 错误"))},
			statuses:  []string{at(0, standby, player.ReasonCDPDisconnected, d.Disconnected)},
			ensureAt:  secs(0, 3, 6),
			connectAt: secs(),
			warns:     0,
		},
		{
			name: "连上没有曲目",
			sc: startScript{until: 3, running: always(true), connect: connectOnce(map[float64]func() *fakeClient{
				0: func() *fakeClient { return &fakeClient{snap: noTrack, end: 1e9} },
			})},
			statuses: []string{at(0, standby, player.ReasonCDPDisconnected, d.Disconnected), at(0, waitingSong, player.ReasonNoSong, noTrackDetail)},
			warns:    -1,
		},
		{
			// 曲目在（时长有效、在播放）而歌名读不到；2 s 起歌名读到了，照常宣告。
			name: "曲目在而歌名读不到，随后歌名出现",
			sc: startScript{until: 4, running: always(true), connect: connectOnce(map[float64]func() *fakeClient{
				0: func() *fakeClient { return &fakeClient{snap: sequence(unnamed, 2, named), end: 1e9} },
			})},
			statuses: []string{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, standby, player.ReasonMetadataUnavailable, unnamedDetail),
				at(2, "playing", "", songTitle),
			},
			warns: -1,
		},
		{
			// 图标认不出、没有进度条（时长 0），位置每秒推进：第一拍只有一个位置样本，还看不出在走，按没有曲目报；1 s 时
			// 位置变了，即曲目在。
			name: "图标认不出、位置推进：曲目在而歌名读不到",
			sc: startScript{until: 4, running: always(true), connect: connectOnce(map[float64]func() *fakeClient{
				0: func() *fakeClient {
					return &fakeClient{snap: func(now time.Duration) *cdp.Snapshot {
						return &cdp.Snapshot{OK: true, PositionMs: float64(now / time.Second * 1000), State: "unknown"}
					}, end: 1e9}
				},
			})},
			statuses: []string{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, waitingSong, player.ReasonNoSong, noTrackDetail),
				at(1, standby, player.ReasonMetadataUnavailable, unnamedDetail),
			},
			warns: -1,
		},
		{
			// 审查给的反例：图标认不出、时长与位置一直为 0。会话第一个位置样本不是推进的证据，自始至终按没有曲目报，
			// 不报 metadata_unavailable。
			name: "图标认不出、位置不动：没有曲目",
			sc: startScript{until: 4, running: always(true), connect: connectOnce(map[float64]func() *fakeClient{
				0: func() *fakeClient {
					return &fakeClient{snap: func(time.Duration) *cdp.Snapshot { return &cdp.Snapshot{OK: true, State: "unknown"} }, end: 1e9}
				},
			})},
			statuses: []string{at(0, standby, player.ReasonCDPDisconnected, d.Disconnected), at(0, waitingSong, player.ReasonNoSong, noTrackDetail)},
			warns:    -1,
		},
		{
			// 宣告过歌之后 2～4 s 歌名读不到（曲目仍在），4 s 起同一首的歌名又读到了：其间不发状态、不清空，之后也不重新宣告。
			name: "宣告过歌之后歌名变空",
			sc: startScript{until: 6, running: always(true), connect: connectOnce(map[float64]func() *fakeClient{
				0: func() *fakeClient {
					return &fakeClient{snap: sequence(named, 2, sequence(unnamed, 4, named)), end: 1e9}
				},
			})},
			statuses:  []string{at(0, standby, player.ReasonCDPDisconnected, d.Disconnected), at(0, "playing", "", songTitle)},
			warns:     -1,
			quietFrom: 0.1,
		},
		{
			// 0 s 连上（连上之前先报 standby），一直没有曲目；Spotify 在 10 s 退出，连接随之断开。
			name: "会话结束而进程已退出",
			sc: startScript{until: 13, running: between(0, 10), connect: connectOnce(map[float64]func() *fakeClient{
				0: func() *fakeClient { return &fakeClient{snap: noTrack, end: 10} },
			})},
			statuses: []string{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, waitingSong, player.ReasonNoSong, noTrackDetail),
				at(10, waitingProcess, player.ReasonProcessExited, d.Exited),
			},
			ensureAt:  secs(0, 12),
			connectAt: secs(0),
			warns:     -1,
		},
		{
			name: "会话结束而进程仍在运行",
			sc: startScript{until: 8, running: always(true), connect: connectOnce(map[float64]func() *fakeClient{
				0: func() *fakeClient { return &fakeClient{snap: named, end: 5} },
			})},
			statuses: []string{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, "playing", "", songTitle),
				at(5, standby, player.ReasonCDPDisconnected, d.Disconnected),
			},
			ensureAt:  secs(0, 7),
			connectAt: secs(0, 7),
			warns:     -1,
		},
		{
			// 先宣告一首歌，5 s 连接断开；7 s 重连，此后一直没有曲目（设计 4.5：断开时画面按决定保留，重连后没有歌须清屏）。
			name: "宣告过歌、断开后重连而一直没有曲目",
			sc: startScript{until: 9, running: always(true), connect: connectOnce(map[float64]func() *fakeClient{
				0: func() *fakeClient { return &fakeClient{snap: named, end: 5} },
				7: func() *fakeClient { return &fakeClient{snap: noTrack, end: 1e9} },
			})},
			statuses: []string{
				at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(0, "playing", "", songTitle),
				at(5, standby, player.ReasonCDPDisconnected, d.Disconnected),
				at(7, waitingSong, player.ReasonNoSong, noTrackDetail),
			},
			ensureAt:  secs(0, 7),
			connectAt: secs(0, 7),
			warns:     -1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runStartScript(t, c.sc)
			if r.returned {
				t.Errorf("Start 在 Stop 之前返回了")
			}
			until := secDur(c.sc.until)
			for i, e := range r.events {
				if e.s == nil || e.at >= until || e.s.Status == "playing" || e.s.Status == "paused" {
					continue
				}
				switch {
				case e.s.Reason == "":
					t.Errorf("非歌名状态没有 reason：%v", e)
				case e.s.Status == standby && !c.sc.running(e.at):
					t.Errorf("Spotify 不在运行而报 standby：%v", e)
				case e.s.Status == waitingProcess && c.sc.running(e.at):
					t.Errorf("Spotify 在运行而报 waiting_process：%v", e)
				}
				if i+1 >= len(r.events) || r.events[i+1] != (timedEvent{e.at, nil}) {
					t.Errorf("%v 之后应在同一时刻紧跟 clear_song_data；事件 %v", e, r.events[i:min(i+3, len(r.events))])
				}
			}
			if got := r.statusesBefore(until); !slices.Equal(got, c.statuses) {
				t.Errorf("状态序列\n got  %v\n want %v", got, c.statuses)
			}
			if c.ensureAt != nil && !slices.Equal(r.ensureAt, c.ensureAt) {
				t.Errorf("EnsureDebugMode 调用于 %v，期望 %v", r.ensureAt, c.ensureAt)
			}
			if c.connectAt != nil && !slices.Equal(r.connectAt, c.connectAt) {
				t.Errorf("连接 CDP 于 %v，期望 %v", r.connectAt, c.connectAt)
			}
			if n := strings.Count(r.logs, "[!]"); c.warns >= 0 && n != c.warns {
				t.Errorf("Warn 打了 %d 条，期望 %d 条；日志：%s", n, c.warns, r.logs)
			}
			if c.quietFrom > 0 {
				for _, e := range r.events {
					if e.at >= secDur(c.quietFrom) && e.at < until {
						t.Errorf("%v 起不应再有状态或 clear_song_data，实有 %v", secDur(c.quietFrom), e)
					}
				}
			}
		})
	}
}

// TestStartStopInterruptsConnect：连接挂住（9223 端口接受连接而不完成响应）时，Stop 即中止这次连接，Start 随即返回（连接失败
// 后的等待可被停止打断）。连接的桩一直挂到 ctx 结束。
//
// 变异自证（均实测变红）：连接改传 context.Background()（泡内死锁）；停止时不取消 ctx（同上）。
func TestStartStopInterruptsConnect(t *testing.T) {
	op, oe, of, oc, ol := patchAutoStart, ensureDebugMode, findMainPID, connectCDP, findCLI
	defer func() { patchAutoStart, ensureDebugMode, findMainPID, connectCDP, findCLI = op, oe, of, oc, ol }()

	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		patchAutoStart = func() {}
		ensureDebugMode = func() error { return nil }
		findMainPID = func() (int, error) { return 4242, nil }
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
		if el := time.Since(t0); el != 0 || connects != 1 {
			t.Errorf("Stop 之后 %v 返回、连接 %d 次，期望中止挂住的那次连接、随即返回、只连 1 次", el, connects)
		}
	})
}
