package wesing

// 本文件经 findProcess / openProc / closeProc / enumModules / getPlayState 桩离线跑 Start、waitForProcess 与
// runSession，只守非歌名状态层次与连上第一拍（窗口已关或加载中）的状态（#231，设计
// doc/design/converge-player-behavior-231-design.md §3.2、§3.3、4.3 第 2 条、4.4 wesing 行）：
//   - findProcess 失败 → waiting_process（从未见它运行为 process_not_running，运行过为 process_exited）；
//   - 找到进程而 openProc 失败 → standby / no_admin（并置 seen）；
//   - 等待期间每轮声明状态，经 EmitStatusIfChanged 去重，同拍紧跟 clear_song_data；
//   - 连上时窗口已关（第一拍即 PhaseStandby）→ 报 waiting_song / window_not_open + clear_song_data，且不发 lyric_idle；
//   - 连上时即处于加载中（第一拍即 PhaseLoading）→ 报 waiting_song / no_song 脱离未运行，不发 clear_song_data 也不发 lyric_idle；
//   - 会话结束：进程已退出报 waiting_process / process_exited，仍在运行（EnumModules 失败）报 standby / no_admin。
//
// Start 在 testing/synctest 的泡里跑，等待是假时钟。

import (
	"errors"
	"fmt"
	"slices"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

func secDur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

type startScript struct {
	until     float64
	find      func(now time.Duration) (uint32, error)
	open      func(now time.Duration, pid uint32) (syscall.Handle, error)
	modules   func(now time.Duration, pid uint32) ([]proc.Module, error)
	playState func(now time.Duration, pid uint32) proc.PlayState
}

type timedEvent struct {
	at  time.Duration
	typ string
	s   *player.StatusInfo // 非 nil 即 status_update
}

func (e timedEvent) String() string {
	if e.s != nil {
		return fmt.Sprintf("%v %s/%s/%s", e.at, e.s.Status, e.s.Reason, e.s.Detail)
	}
	return fmt.Sprintf("%v %s", e.at, e.typ)
}

type startRun struct {
	events    []timedEvent
	lyricIdle int
	returned  bool
}

func runStartScript(t *testing.T, sc startScript) startRun {
	t.Helper()
	of, oo, oc, oe, og := findProcess, openProc, closeProc, enumModules, getPlayState
	defer func() { findProcess, openProc, closeProc, enumModules, getPlayState = of, oo, oc, oe, og }()

	var r startRun
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 30)
		t0 := time.Now()
		now := func() time.Duration { return time.Since(t0) }

		findProcess = func(string) (uint32, error) {
			return sc.find(now())
		}
		openProc = func(pid uint32) (syscall.Handle, error) {
			if sc.open == nil {
				return 0, errors.New("access denied")
			}
			return sc.open(now(), pid)
		}
		closeProc = func(syscall.Handle) {}
		enumModules = func(pid uint32) ([]proc.Module, error) {
			if sc.modules == nil {
				return []proc.Module{{Name: "KSongsLyric.dll", Base: 0x1000, Size: 0x1000}}, nil
			}
			return sc.modules(now(), pid)
		}
		getPlayState = func(pid uint32) proc.PlayState {
			if sc.playState == nil {
				return proc.PlayState{Phase: proc.PhaseStandby}
			}
			return sc.playState(now(), pid)
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
	return r
}

func (r *startRun) drain(p *WesingPlayer, now time.Duration) {
	for {
		select {
		case e := <-p.EventCh:
			switch e.Type {
			case player.EventStatusUpdate:
				r.events = append(r.events, timedEvent{at: now, typ: e.Type, s: e.Data.(*player.StatusInfo)})
			case player.EventClearSongData:
				r.events = append(r.events, timedEvent{at: now, typ: e.Type})
			case player.EventLyricIdle:
				r.lyricIdle++
				r.events = append(r.events, timedEvent{at: now, typ: e.Type})
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

func at(sec float64, status, reason, detail string) string {
	return timedEvent{at: secDur(sec), typ: player.EventStatusUpdate, s: &player.StatusInfo{Status: status, Reason: reason, Detail: detail}}.String()
}

var (
	errNotFound = errors.New("Process not found: WeSing.exe")
	errOpenFail = errors.New("OpenProcess failed: Access is denied.")
	errModFail  = errors.New("CreateToolhelp32Snapshot(MODULE) failed: Access is denied.")
)

// TestStartStatus 守全民K歌外层循环与连上第一拍（窗口已关或加载中）的非歌名状态。
//
// 变异自证（均实测变红）：
//   - lastPhase 初值改回零值（PhaseStandby）：「连上时窗口已关即报且不发lyric_idle」「未运行→打不开→连上且窗口已关→进程退出」
//     「连上时窗口已关→加载中→再关窗」三例均红（连上时不报 window_not_open）；
//   - idle 初值改回零值（sent: false）：上述三例与「启动时已在加载中→关窗进入待机」均因发出 lyric_idle 而红；
//   - openProc 失败按未运行报：「找到而打不开报no_admin，随后进程退出」「未运行→打不开→连上且窗口已关→进程退出」两例均红；
//   - 删掉 PhaseLoading 的 lastPhase == PlayPhase(-1) 发射分支：「未运行→连上即加载中」「启动时已在加载中→关窗进入待机」两例均红；
//   - PhaseLoading 去掉 lastPhase == PlayPhase(-1) 条件改为无条件发 no_song：「连上时窗口已关→加载中→再关窗」红；
//   - PhaseLoading 发射分支附带发 clear_song_data：「未运行→连上即加载中」「启动时已在加载中→关窗进入待机」两例均红。
func TestStartStatus(t *testing.T) {
	d := wesingDetails()
	noAdminDetail := i18n.T("Cannot read the WeSing process (WeSing may be running as administrator)")
	winClosedDetail := i18n.T("WeSing window not open")
	loadingDetail := i18n.T("WeSing loading song")

	cases := []struct {
		name     string
		sc       startScript
		running  func(now time.Duration) bool
		statuses []string
	}{
		{
			name: "从未运行",
			sc: startScript{
				until: 7,
				find:  func(time.Duration) (uint32, error) { return 0, errNotFound },
			},
			running: func(time.Duration) bool { return false },
			statuses: []string{
				at(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
			},
		},
		{
			// 0s、2s、4s 找到进程但打不开（no_admin，同时置 seen）；5s 进程退出，6s 查不到 → 报 process_exited。
			name: "找到而打不开报no_admin，随后进程退出",
			sc: startScript{
				until: 9,
				find: func(now time.Duration) (uint32, error) {
					if now < 5*time.Second {
						return 1001, nil
					}
					return 0, errNotFound
				},
				open: func(time.Duration, uint32) (syscall.Handle, error) { return 0, errOpenFail },
			},
			running: func(now time.Duration) bool { return now < 5*time.Second },
			statuses: []string{
				at(0, "standby", player.ReasonNoAdmin, noAdminDetail),
				at(6, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
		},
		{
			// 连上时窗口已关：立即报 waiting_song/window_not_open 且不发 lyric_idle，不先闪 waiting_process，持续待机不重发。
			name: "连上时窗口已关即报且不发lyric_idle",
			sc: startScript{
				until:     4,
				find:      func(time.Duration) (uint32, error) { return 1001, nil },
				open:      func(time.Duration, uint32) (syscall.Handle, error) { return syscall.Handle(1), nil },
				playState: func(time.Duration, uint32) proc.PlayState { return proc.PlayState{Phase: proc.PhaseStandby} },
			},
			running: func(time.Duration) bool { return true },
			statuses: []string{
				at(0, "waiting_song", player.ReasonWindowNotOpen, winClosedDetail),
			},
		},
		{
			// 0s 未运行 → 2s 打不开（no_admin）→ 4s 打开成功且窗口已关（window_not_open，不发 lyric_idle）
			// → 7s 进程退出（process_exited）→ 9s、11s 仍未运行，保持 process_exited。
			name: "未运行→打不开→连上且窗口已关→进程退出",
			sc: startScript{
				until: 12,
				find: func(now time.Duration) (uint32, error) {
					if now >= 2*time.Second && now < 7*time.Second {
						return 1001, nil
					}
					return 0, errNotFound
				},
				open: func(now time.Duration, _ uint32) (syscall.Handle, error) {
					if now < 4*time.Second {
						return 0, errOpenFail
					}
					return syscall.Handle(1), nil
				},
				playState: func(time.Duration, uint32) proc.PlayState { return proc.PlayState{Phase: proc.PhaseStandby} },
			},
			running: func(now time.Duration) bool { return now >= 2*time.Second && now < 7*time.Second },
			statuses: []string{
				at(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
				at(2, "standby", player.ReasonNoAdmin, noAdminDetail),
				at(4, "waiting_song", player.ReasonWindowNotOpen, winClosedDetail),
				at(7, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
		},
		{
			// 0s 窗口已关 → 1s 进入加载中 → 1.5s 关窗回到待机：状态由 EmitStatusIfChanged 去重，未宣告过歌仍不发 lyric_idle。
			name: "连上时窗口已关→加载中→再关窗",
			sc: startScript{
				until: 3,
				find:  func(time.Duration) (uint32, error) { return 1001, nil },
				open:  func(time.Duration, uint32) (syscall.Handle, error) { return syscall.Handle(1), nil },
				playState: func(now time.Duration, _ uint32) proc.PlayState {
					if now >= time.Second && now < 1500*time.Millisecond {
						return proc.PlayState{Phase: proc.PhaseLoading, SongTitle: "加载中曲目"}
					}
					return proc.PlayState{Phase: proc.PhaseStandby}
				},
			},
			running: func(time.Duration) bool { return true },
			statuses: []string{
				at(0, "waiting_song", player.ReasonWindowNotOpen, winClosedDetail),
			},
		},
		{
			// 2s 前未运行，2s 起连上并持续 PhaseLoading 至 8s：0s 报 process_not_running，2s 立刻报 waiting_song/no_song
			// 脱离未运行，后续不重发、不发 clear_song_data 也不发 lyric_idle。
			name: "未运行→连上即加载中",
			sc: startScript{
				until: 8,
				find: func(now time.Duration) (uint32, error) {
					if now < 2*time.Second {
						return 0, errNotFound
					}
					return 1001, nil
				},
				open: func(time.Duration, uint32) (syscall.Handle, error) { return syscall.Handle(1), nil },
				playState: func(time.Duration, uint32) proc.PlayState {
					return proc.PlayState{Phase: proc.PhaseLoading, SongTitle: "加载中曲目"}
				},
			},
			running: func(now time.Duration) bool { return now >= 2*time.Second },
			statuses: []string{
				at(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
				at(2, "waiting_song", player.ReasonNoSong, loadingDetail),
			},
		},
		{
			// 0s 启动时已在加载中（含 SongTitle 为空与非空两拍）→ 2s 关窗进入待机：0s 立刻报 waiting_song/no_song
			// （不发 clear_song_data），2s 转报 waiting_song/window_not_open 并紧跟 clear_song_data，全程不发 lyric_idle。
			name: "启动时已在加载中→关窗进入待机",
			sc: startScript{
				until: 4,
				find:  func(time.Duration) (uint32, error) { return 1001, nil },
				open:  func(time.Duration, uint32) (syscall.Handle, error) { return syscall.Handle(1), nil },
				playState: func(now time.Duration, _ uint32) proc.PlayState {
					switch {
					case now < time.Second:
						return proc.PlayState{Phase: proc.PhaseLoading}
					case now < 2*time.Second:
						return proc.PlayState{Phase: proc.PhaseLoading, SongTitle: "加载中曲目"}
					default:
						return proc.PlayState{Phase: proc.PhaseStandby}
					}
				},
			},
			running: func(time.Duration) bool { return true },
			statuses: []string{
				at(0, "waiting_song", player.ReasonNoSong, loadingDetail),
				at(2, "waiting_song", player.ReasonWindowNotOpen, winClosedDetail),
			},
		},
		{
			// 会话中 EnumModules 失败而进程仍在运行 → 报 standby/no_admin（与 openProc 失败一致）。
			name: "EnumModules失败而进程仍在运行报no_admin",
			sc: startScript{
				until:   5,
				find:    func(time.Duration) (uint32, error) { return 1001, nil },
				open:    func(time.Duration, uint32) (syscall.Handle, error) { return syscall.Handle(1), nil },
				modules: func(time.Duration, uint32) ([]proc.Module, error) { return nil, errModFail },
			},
			running: func(time.Duration) bool { return true },
			statuses: []string{
				at(0, "standby", player.ReasonNoAdmin, noAdminDetail),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runStartScript(t, c.sc)
			if r.returned {
				t.Errorf("Start 在 Stop 之前返回了")
			}
			if r.lyricIdle != 0 {
				t.Errorf("未宣告过歌不应发 lyric_idle，got %d 次", r.lyricIdle)
			}
			until := secDur(c.sc.until)
			inLoadingNoSong := false
			for i, e := range r.events {
				if e.at >= until {
					continue
				}
				if e.s == nil {
					if inLoadingNoSong && e.typ == player.EventClearSongData {
						t.Errorf("加载中的 waiting_song/no_song 期间不应发 clear_song_data：%v", e)
					}
					continue
				}
				if e.s.Reason == "" {
					t.Errorf("非歌名状态没有 reason：%v", e)
				}
				if e.s.Status == "standby" && !c.running(e.at) {
					t.Errorf("全民K歌不在运行而报 standby：%v", e)
				}
				if e.s.Status == "waiting_process" && c.running(e.at) {
					t.Errorf("全民K歌在运行而报 waiting_process：%v", e)
				}
				if e.s.Status == "waiting_song" && e.s.Reason == player.ReasonNoSong {
					inLoadingNoSong = true
					if i+1 < len(r.events) && r.events[i+1] == (timedEvent{at: e.at, typ: player.EventClearSongData}) {
						t.Errorf("%v 不应紧跟 clear_song_data；事件 %v", e, r.events[i:min(i+3, len(r.events))])
					}
				} else {
					inLoadingNoSong = false
					if i+1 >= len(r.events) || r.events[i+1] != (timedEvent{at: e.at, typ: player.EventClearSongData}) {
						t.Errorf("%v 之后应在同一时刻紧跟 clear_song_data；事件 %v", e, r.events[i:min(i+3, len(r.events))])
					}
				}
			}
			if got := r.statusesBefore(until); !slices.Equal(got, c.statuses) {
				t.Errorf("状态序列\n got  %v\n want %v", got, c.statuses)
			}
		})
	}
}
