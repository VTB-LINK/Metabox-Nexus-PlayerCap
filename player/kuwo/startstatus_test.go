package kuwo

// 本文件经桩离线跑 Start 的外层循环与 connectSession/runSession，只守非歌名状态层次、seen 维护与「状态在前、清空在后」
// （#231，设计 doc/design/converge-player-behavior-231-design.md §3.2、§3.3、§4.4、§4.5）：
//   - ErrNotRunning 报 waiting_process（从未见过运行为 process_not_running，见过为 process_exited 并保持）；
//   - 商店版、读不到路径、版本/核心不支持、未授权重启、端口未就绪及其他连接错误均按进程在运行报 standby 加各自 reason，
//     并置 seen=true；
//   - 每轮先 status_update 后 clear_song_data，同状态由 EmitStatusIfChanged 去重不重发，同 reason 告警不重复打；
//   - 会话运行中断开时 defer 先发 standby / cdp_disconnected 后发 clear_song_data；下一轮外层见到进程退出改报
//     waiting_process / process_exited；重连后没有歌立即先发 waiting_song / no_song 后发 clear_song_data。
//
// Start 在 testing/synctest 的泡里跑，等待是假时钟；ensureDebugMode、findMainPID、connectCDP、openPosition 换成桩。
//
// 变异自证（实测）记在各用例注释里。

import (
	"bytes"
	"context"
	"fmt"
	stdlog "log"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
	"Metabox-Nexus-PlayerCap/player/kuwo/watchdog"
	"Metabox-Nexus-PlayerCap/player/launch"

	"golang.org/x/sys/windows"
)

func secDur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

type startScript struct {
	until   float64
	running func(now time.Duration) bool           // 进程是否在运行（用于反例断言及驱动默认行为）
	ensure  func(now time.Duration) error          // 进程在运行时 ensureDebugMode 的返回值；nil 即返回 nil
	connect func(now time.Duration) *fakeCDPClient // connectCDP 返回的桩；nil 或返回 nil 即返回 cdp.ErrUnavailable
	openErr func(now time.Duration) error          // openPosition 的错误；nil 即给出可用位置桩
}

type timedEvent struct {
	at time.Duration
	s  *player.StatusInfo // nil 表示 clear_song_data
}

func (e timedEvent) String() string {
	if e.s == nil {
		return fmt.Sprintf("%v clear", e.at)
	}
	return fmt.Sprintf("%v %s/%s/%s", e.at, e.s.Status, e.s.Reason, e.s.Detail)
}

type startRun struct {
	events   []timedEvent
	ensureAt []time.Duration
	returned bool
	logs     string
}

type fakeCDPClient struct {
	snap  func(now time.Duration) (cdp.Snapshot, error)
	end   float64
	now   func() time.Duration
	build string
}

func (c *fakeCDPClient) Snapshot(ctx context.Context) (cdp.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return cdp.Snapshot{}, err
	}
	if c.now() >= secDur(c.end) {
		return cdp.Snapshot{}, cdp.ErrUnavailable
	}
	return c.snap(c.now())
}
func (c *fakeCDPClient) Build() string { return c.build }
func (c *fakeCDPClient) Close() error  { return nil }

type fakePosReader struct {
	now func() time.Duration
	pos int32
}

func (r *fakePosReader) Read(expected string, _ bool) (pos.Sample, error) {
	r.pos += 50
	return pos.Sample{
		RID:        pos.RIDWithPrefix(expected),
		PositionMs: r.pos,
		CoreMs:     r.pos,
		DurationMs: 200000,
		ObservedAt: time.Now(),
	}, nil
}
func (r *fakePosReader) Close() {}

func runStartScript(t *testing.T, sc startScript) startRun {
	t.Helper()
	oe, of, oc, oo := ensureDebugMode, findMainPID, connectCDP, openPosition
	defer func() { ensureDebugMode, findMainPID, connectCDP, openPosition = oe, of, oc, oo }()

	var buf bytes.Buffer
	w := stdlog.Writer()
	stdlog.SetOutput(&buf)
	defer stdlog.SetOutput(w)

	var r startRun
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 50)
		t0 := time.Now()
		now := func() time.Duration { return time.Since(t0) }

		ensureDebugMode = func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			r.ensureAt = append(r.ensureAt, now())
			if !sc.running(now()) {
				return watchdog.ErrNotRunning
			}
			if sc.ensure != nil {
				return sc.ensure(now())
			}
			return nil
		}
		findMainPID = func() (int, error) {
			if !sc.running(now()) {
				return 0, watchdog.ErrNotRunning
			}
			return 4242, nil
		}
		connectCDP = func(ctx context.Context) (cdpSession, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if sc.connect != nil {
				if c := sc.connect(now()); c != nil {
					c.now = now
					if c.build == "" {
						c.build = "W1"
					}
					return c, nil
				}
			}
			return nil, cdp.ErrUnavailable
		}
		openPosition = func(int, string, *bool, int) (positionSource, error) {
			if sc.openErr != nil {
				if err := sc.openErr(now()); err != nil {
					return nil, err
				}
			}
			return &fakePosReader{now: now}, nil
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

func (r *startRun) drain(p *KuwoPlayer, now time.Duration) {
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

func (r startRun) statusesBefore(until time.Duration) []string {
	var out []string
	for _, e := range r.events {
		if e.s != nil && e.at < until {
			out = append(out, e.String())
		}
	}
	return out
}

func statusAt(sec float64, status, reason, detail string) string {
	return timedEvent{secDur(sec), &player.StatusInfo{Status: status, Reason: reason, Detail: detail}}.String()
}

func between(from, to float64) func(time.Duration) bool {
	return func(now time.Duration) bool { return now >= secDur(from) && now < secDur(to) }
}

func always(b bool) func(time.Duration) bool { return func(time.Duration) bool { return b } }

func constErr(err error) func(time.Duration) error { return func(time.Duration) error { return err } }

var errStore = &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\合成.Kuwo_1.0.0.0_x86__0000000000000\kwmusic.exe`}

// TestStartStatus 守 kuwo 外层循环与会话衔接的非歌名状态层次、seen 维护、去重及「状态在前、清空在后」。
//
// 变异自证（均实测变红）：
//   - Start 改为先 clear_song_data 后状态（七个经 Start 发非歌名状态的子例均红）；
//   - Start 与 connectSession 不维护 seen（六个退出子例报 process_not_running 而红）；
//   - runSession defer 改回 waiting_process（「会话无歌 → 断开且进程退出 → 外层改报 process_exited」
//     「会话中宣告歌曲 → 断开 → 重连无歌」两例红；defer 改回先清空后状态另由 nosong_test.go、
//     coreopenfailure_test.go、sessionfailure_test.go 实测守红）。
func TestStartStatus(t *testing.T) {
	d := kuwoDetails()
	storeDetail := i18n.T("Kuwo Microsoft Store version unsupported (requires the official installer version)")
	noAdminDetail := i18n.T("Cannot read the Kuwo installation path (Kuwo may be running as administrator)")
	notAuthDetail := i18n.T("Kuwo must be started with debug port 9224; automatic restart is limited to verified builds, please launch Kuwo manually with --remote-debugging-port=9224")
	unsupDetail := i18n.T("This Kuwo playback core is not supported")
	noSongDetail := i18n.T("Kuwo has no current song")

	playingSnap := func(time.Duration) (cdp.Snapshot, error) {
		return cdp.Snapshot{RID: "MUSIC_1", Title: "合成歌名", Artist: "合成歌手", Status: 1}, nil
	}
	noSongSnap := func(time.Duration) (cdp.Snapshot, error) {
		return cdp.Snapshot{Status: 3}, nil
	}

	cases := []struct {
		name     string
		sc       startScript
		statuses []string
		warns    int    // -1 不查
		logHas   string // 非空时断言日志包含此子串
	}{
		{
			name:     "从未运行：只发一条 process_not_running 并去重",
			sc:       startScript{until: 10, running: always(false)},
			statuses: []string{statusAt(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning)},
			warns:    1,
		},
		{
			// 0s 未运行 → 3s 商店版受阻（seen=true）→ 9s 退出后报 process_exited 并保持到结束。
			name: "未运行 → 商店版受阻 → 退出后保持 process_exited",
			sc: startScript{
				until:   16,
				running: between(3, 8),
				ensure:  constErr(errStore),
			},
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
				statusAt(3, "standby", player.ReasonVersionUnsupported, storeDetail),
				statusAt(9, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			warns: 3, // 未运行一次、商店版一次、退出一次
		},
		{
			name: "运行中端口未就绪 → 退出后报 process_exited",
			sc: startScript{
				until:   10,
				running: between(0, 5),
				ensure:  constErr(watchdog.ErrPortNotReady),
			},
			statuses: []string{
				statusAt(0, "standby", player.ReasonCDPDisconnected, d.Disconnected),
				statusAt(6, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			warns: 2,
		},
		{
			name: "运行中未授权自动重启 → 退出后报 process_exited",
			sc: startScript{
				until:   10,
				running: between(0, 5),
				ensure:  constErr(watchdog.ErrRestartNotAuthorized),
			},
			statuses: []string{
				statusAt(0, "standby", player.ReasonCDPDisconnected, notAuthDetail),
				statusAt(6, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			warns: 2,
		},
		{
			name: "运行中读不到路径报 no_admin → 退出后报 process_exited",
			sc: startScript{
				until:   10,
				running: between(0, 5),
				ensure:  constErr(launch.ErrExeUnknown),
			},
			statuses: []string{
				statusAt(0, "standby", player.ReasonNoAdmin, noAdminDetail),
				statusAt(6, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			warns: 2,
		},
		{
			// watchdog.EnsureDebugMode 对真实权限拒绝（windows.ERROR_ACCESS_DENIED）包装为 launch.ErrExeUnknown，
			// Start 须产出 standby / no_admin 及权限说明日志，进程退出后报 process_exited。
			name: "运行中进程读取权限拒绝（包装ErrExeUnknown）报 no_admin → 退出后报 process_exited",
			sc: startScript{
				until:   10,
				running: between(0, 5),
				ensure:  constErr(fmt.Errorf("%w: %w", launch.ErrExeUnknown, windows.ERROR_ACCESS_DENIED)),
			},
			statuses: []string{
				statusAt(0, "standby", player.ReasonNoAdmin, noAdminDetail),
				statusAt(6, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			warns:  2,
			logHas: i18n.T("Cannot read the installation path of Kuwo; it was not restarted automatically. Run Kuwo with the same privileges as this program"),
		},
		{
			name: "运行中桥版本不支持报 version_unsupported → 退出后报 process_exited",
			sc: startScript{
				until:   10,
				running: between(0, 5),
				ensure:  constErr(cdp.ErrVersionUnsupported),
			},
			statuses: []string{
				statusAt(0, "standby", player.ReasonVersionUnsupported, unsupDetail),
				statusAt(6, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			warns: 2,
		},
		{
			// 0s 连上且无歌 → 2s 断开同时进程已退出：runSession defer 先报 standby/cdp_disconnected，
			// 3s 后（5s）外层循环检出进程不在运行，报 waiting_process/process_exited 并保持。
			name: "会话无歌 → 断开且进程退出 → 外层改报 process_exited",
			sc: startScript{
				until:   9,
				running: between(0, 2),
				connect: func(now time.Duration) *fakeCDPClient {
					if now == 0 {
						return &fakeCDPClient{snap: noSongSnap, end: 2}
					}
					return nil
				},
			},
			statuses: []string{
				statusAt(0, "waiting_song", player.ReasonNoSong, noSongDetail),
				statusAt(2, "standby", player.ReasonCDPDisconnected, d.Disconnected),
				statusAt(5, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			warns: -1,
		},
		{
			// 0s 连上宣告歌曲 → 2s 连接断开而进程仍在（standby/cdp_disconnected）→ 5s 重连且无歌（waiting_song/no_song）。
			name: "会话中宣告歌曲 → 断开 → 重连无歌",
			sc: startScript{
				until:   7,
				running: always(true),
				connect: func(now time.Duration) *fakeCDPClient {
					switch now {
					case 0:
						return &fakeCDPClient{snap: playingSnap, end: 2}
					case 5 * time.Second:
						return &fakeCDPClient{snap: noSongSnap, end: 1e9}
					}
					return nil
				},
			},
			statuses: []string{
				statusAt(0, "playing", "", "合成歌名 - 合成歌手"),
				statusAt(2, "standby", player.ReasonCDPDisconnected, d.Disconnected),
				statusAt(5, "waiting_song", player.ReasonNoSong, noSongDetail),
			},
			warns: -1,
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
				if e.s.Reason == "" {
					t.Errorf("非歌名状态没有 reason：%v", e)
				}
				if e.s.Status == "waiting_process" && c.sc.running(e.at) {
					t.Errorf("酷我在运行而报 waiting_process：%v", e)
				}
				// 状态在前、清空在后：每条非歌名状态之后须在同一时刻紧跟 clear_song_data。
				if i+1 >= len(r.events) || r.events[i+1] != (timedEvent{e.at, nil}) {
					t.Errorf("%v 之后应在同一时刻紧跟 clear_song_data；事件 %v", e, r.events[i:min(i+3, len(r.events))])
				}
			}
			if got := r.statusesBefore(until); !slices.Equal(got, c.statuses) {
				t.Errorf("状态序列\n got  %v\n want %v", got, c.statuses)
			}
			if c.warns >= 0 {
				if n := strings.Count(r.logs, "[!]"); n != c.warns {
					t.Errorf("Warn 打了 %d 条，期望 %d 条；日志：%s", n, c.warns, r.logs)
				}
			}
			if c.logHas != "" && !strings.Contains(r.logs, c.logHas) {
				t.Errorf("日志缺少 %q；实际日志：%s", c.logHas, r.logs)
			}
		})
	}
}

// TestStartStopInterruptsConnect：连接挂住时 Stop 立即取消 ctx，Start 随即返回。
func TestStartStopInterruptsConnect(t *testing.T) {
	oe, of, oc := ensureDebugMode, findMainPID, connectCDP
	defer func() { ensureDebugMode, findMainPID, connectCDP = oe, of, oc }()

	synctest.Test(t, func(t *testing.T) {
		p := New(0, 50)
		ensureDebugMode = func(context.Context) error { return nil }
		findMainPID = func() (int, error) { return 4242, nil }
		connects := 0
		connectCDP = func(ctx context.Context) (cdpSession, error) {
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
			t.Errorf("Stop 之后 %v 返回、连接 %d 次，期望中止挂住的连接、随即返回、只连 1 次", el, connects)
		}
	})
}
