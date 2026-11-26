package applemusic

// 本文件经 attach / reportOnce 桩离线跑 Start 的外层循环，只守非歌名状态层次与无作废不上报（#231，
// 设计 doc/design/converge-player-behavior-231-design.md §3.2、§3.3、§6.4）：
//   - attach 返回 mem.ErrAccessDenied → standby / no_admin（并记下见过进程运行）；
//   - attach 返回其余错误 → waiting_process（从未见过它运行为 process_not_running，见过为 process_exited）；
//   - 附加成功置 seen，进入 runSession（首拍报 waiting_song / no_song）；进程退出（r.Valid() 假）返回后报
//     waiting_process / process_exited，不再报 standby；
//   - 非歌名状态只在变化时发，且同拍紧跟 clear_song_data。
//
// Start 在 testing/synctest 的泡里跑，等待是假时钟；attach 成功时返回零值 &mem.Reader{}，其 Valid() 立即
// 返回假、Close() 为空操作，刚好离线走完「附加 → newSession 报 waiting_song → 进程退出 → 外层报 process_exited」。

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

func secDur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

type startScript struct {
	until  float64
	attach func(now time.Duration) (*mem.Reader, error)
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

type startRun struct {
	events   []timedEvent
	attachAt []time.Duration
	reports  int
	returned bool
}

func runStartScript(t *testing.T, sc startScript) startRun {
	t.Helper()
	origAttach, origReport := attach, reportOnce
	defer func() { attach, reportOnce = origAttach, origReport }()

	var r startRun
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 50)
		t0 := time.Now()
		now := func() time.Duration { return time.Since(t0) }

		attach = func() (*mem.Reader, error) {
			r.attachAt = append(r.attachAt, now())
			return sc.attach(now())
		}
		reportOnce = func(string, string, map[string]string, map[string]any) {
			r.reports++
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

func (r *startRun) drain(p *AppleMusicPlayer, now time.Duration) {
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

func secs(ss ...float64) []time.Duration {
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		out[i] = secDur(s)
	}
	return out
}

// TestStartStatus 守 Apple Music 外层循环的非歌名状态层次与去重。
//
// 变异自证（均实测变红）：
//   - attach 拒绝访问改报 waiting_process（ProcessStatus(false, …)）：「拒绝访问报no_admin」「拒绝访问后进程退出」
//     「未运行→拒绝访问→附加→退出→仍未运行」三例均红；
//   - twinStats.summary 在 lost 为 0 时返回非 nil：「未运行→拒绝访问→附加→退出→仍未运行」因无作废却上报遥测而红。
func TestStartStatus(t *testing.T) {
	d := appleMusicDetails()
	noAdminDetail := i18n.T("Cannot read the Apple Music process (Apple Music may be running as administrator)")
	noSongDetail := i18n.T("Apple Music not playing")

	cases := []struct {
		name     string
		sc       startScript
		running  func(now time.Duration) bool // 断言每条状态与当时进程在不在一致
		statuses []string
		attachAt []time.Duration
	}{
		{
			name:    "从未运行",
			sc:      startScript{until: 10, attach: func(time.Duration) (*mem.Reader, error) { return nil, mem.ErrNotRunning }},
			running: func(time.Duration) bool { return false },
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
			},
			attachAt: secs(0, 3, 6, 9),
		},
		{
			name:    "拒绝访问报no_admin",
			sc:      startScript{until: 10, attach: func(time.Duration) (*mem.Reader, error) { return nil, mem.ErrAccessDenied }},
			running: func(time.Duration) bool { return true },
			statuses: []string{
				statusAt(0, "standby", player.ReasonNoAdmin, noAdminDetail),
			},
			attachAt: secs(0, 3, 6, 9),
		},
		{
			// 0s、3s 拒绝访问（进程在运行，seen 置真）；5s 进程退出，6s attach 返回 ErrNotRunning → 报 process_exited。
			name: "拒绝访问后进程退出",
			sc: startScript{until: 10, attach: func(now time.Duration) (*mem.Reader, error) {
				if now < 5*time.Second {
					return nil, mem.ErrAccessDenied
				}
				return nil, mem.ErrNotRunning
			}},
			running: func(now time.Duration) bool { return now < 5*time.Second },
			statuses: []string{
				statusAt(0, "standby", player.ReasonNoAdmin, noAdminDetail),
				statusAt(6, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			attachAt: secs(0, 3, 6, 9),
		},
		{
			// 0s 未运行 → 3s 拒绝访问 → 6s 附加成功（newSession 报 waiting_song，随后 Valid() 为假退出会话报 process_exited）
			// → 8s、11s 仍未运行，保持 process_exited 不退回 process_not_running。
			name: "未运行→拒绝访问→附加→退出→仍未运行",
			sc: startScript{until: 12, attach: func(now time.Duration) (*mem.Reader, error) {
				switch {
				case now < 3*time.Second:
					return nil, mem.ErrNotRunning
				case now < 6*time.Second:
					return nil, mem.ErrAccessDenied
				case now == 6*time.Second:
					return &mem.Reader{}, nil
				default:
					return nil, mem.ErrNotRunning
				}
			}},
			running: func(now time.Duration) bool {
				return now >= 3*time.Second && now < 6*time.Second
			},
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
				statusAt(3, "standby", player.ReasonNoAdmin, noAdminDetail),
				statusAt(6, "waiting_song", player.ReasonNoSong, noSongDetail),
				statusAt(6, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			attachAt: secs(0, 3, 6, 8, 11),
		},
		{
			name:    "其他attach错误按未运行处理",
			sc:      startScript{until: 4, attach: func(time.Duration) (*mem.Reader, error) { return nil, errors.New("snapshot failed") }},
			running: func(time.Duration) bool { return false },
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
			},
			attachAt: secs(0, 3),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runStartScript(t, c.sc)
			if r.returned {
				t.Errorf("Start 在 Stop 之前返回了")
			}
			if r.reports != 0 {
				t.Errorf("没有位置结构作废的会话不应上报遥测，got %d 次", r.reports)
			}
			until := secDur(c.sc.until)
			for i, e := range r.events {
				if e.s == nil || e.at >= until {
					continue
				}
				if e.s.Reason == "" {
					t.Errorf("非歌名状态没有 reason：%v", e)
				}
				if e.s.Status == "standby" && !c.running(e.at) {
					t.Errorf("Apple Music 不在运行而报 standby：%v", e)
				}
				if e.s.Status == "waiting_process" && c.running(e.at) {
					t.Errorf("Apple Music 在运行而报 waiting_process：%v", e)
				}
				if e.s.Status == "waiting_process" || e.s.Status == "standby" {
					if i+1 >= len(r.events) || r.events[i+1] != (timedEvent{e.at, nil}) {
						t.Errorf("%v 之后应在同一时刻紧跟 clear_song_data；事件 %v", e, r.events[i:min(i+3, len(r.events))])
					}
				}
			}
			if got := r.statusesBefore(until); !slices.Equal(got, c.statuses) {
				t.Errorf("状态序列\n got  %v\n want %v", got, c.statuses)
			}
			if !slices.Equal(r.attachAt, c.attachAt) {
				t.Errorf("attach 调用于 %v，期望 %v", r.attachAt, c.attachAt)
			}
		})
	}
}
