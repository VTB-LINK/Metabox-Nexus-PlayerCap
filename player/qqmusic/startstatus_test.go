package qqmusic

// 本文件经 connectQQMusic / qqProcessRunning 桩与 QQMusicMem 只读测试钩子离线跑 Start、attachQQMusic 与
// runSession，只守非歌名状态层次、attach 错误分类、Stop 立即打断等待与连上无歌如实上报（#231，设计
// doc/design/converge-player-behavior-231-design.md §3.2、§3.3、§4.3 第 1～2 条、§4.4 qqmusic 行）：
//   - 进程未运行 → waiting_process（从未见过它运行为 process_not_running，见过为 process_exited）；
//   - 进程在运行而拒绝访问 → standby / no_admin（并置 seen）；
//   - 进程在运行而模块未就绪 → standby / cdp_disconnected（并置 seen）；
//   - 会话结束后按进程是否仍在运行分流报 waiting_process / process_exited 或 standby / cdp_disconnected，
//     且进程退出后持续未运行保持 process_exited 不退回 process_not_running；
//   - 非歌名状态只在变化时发，且同拍紧跟 clear_song_data；
//   - Stop() 立即打断外层 2s 等待；
//   - 连上后尚未认领过歌曲且读到空歌名或 "QQ音乐" 前缀占位串 → 先发 waiting_song / no_song、紧跟 1 次
//     clear_song_data，后续无歌拍不重发；认领真歌后中途读到空或占位串保持现行不报。
//
// Start 与 runSession 在 testing/synctest 的泡里跑，等待全部走假时钟，零外部进程与网络访问。

import (
	"errors"
	"fmt"
	"slices"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
)

func msDur(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }

type startScript struct {
	until   time.Duration
	connect func(now time.Duration) (*QQMusicMem, error)
	running func(now time.Duration) bool
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
	connectAt []time.Duration
	returned  bool
}

func runStartScript(t *testing.T, sc startScript) startRun {
	t.Helper()
	origConnect, origRunning := connectQQMusic, qqProcessRunning
	prevReaders := assocReaders.Swap(1) // 离线测试不启动后台磁盘/RM 读取者，避免触碰真实配置目录与泡外 goroutine
	defer func() {
		connectQQMusic, qqProcessRunning = origConnect, origRunning
		assocReaders.Store(prevReaders)
	}()

	var r startRun
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 50)
		t0 := time.Now()
		now := func() time.Duration { return time.Since(t0) }

		connectQQMusic = func() (*QQMusicMem, error) {
			r.connectAt = append(r.connectAt, now())
			return sc.connect(now())
		}
		qqProcessRunning = func() bool {
			if sc.running == nil {
				return false
			}
			return sc.running(now())
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
			if now() >= sc.until {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		p.Stop()
		<-done
		r.drain(p, now())
	})
	return r
}

func (r *startRun) drain(p *QQMusicPlayer, now time.Duration) {
	for {
		select {
		case e := <-p.EventCh:
			switch e.Type {
			case player.EventStatusUpdate:
				r.events = append(r.events, timedEvent{at: now, typ: e.Type, s: e.Data.(*player.StatusInfo)})
			case player.EventClearSongData:
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

func statusAt(at time.Duration, status, reason, detail string) string {
	return timedEvent{at: at, typ: player.EventStatusUpdate, s: &player.StatusInfo{Status: status, Reason: reason, Detail: detail}}.String()
}

func durMs(ms ...int) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, m := range ms {
		out[i] = msDur(m)
	}
	return out
}

// TestStartStatus 守 QQ 音乐外层循环的非歌名状态层次、seen 保持与去重。
//
// 变异自证（在隔离临时副本中逐项实测变红）：
//   - no_admin 改报 waiting_process（ProcessStatus(false, …)）→「拒绝访问报no_admin，随后进程退出保持process_exited」红；
//   - errQQMusicAccessDenied 分支改回硬编码 running=true →「拒绝访问后进程已退出不报standby直接报process_exited」红；
//   - errQQMusicModuleNotReady 分支改回硬编码 running=true →「附加期间进程已退出（ERROR_INVALID_PARAMETER）不报standby直接报process_exited」红；
//   - 漏维护 seen（default 分支改传 seen=false）→「拒绝访问报no_admin…」「模块未就绪→进程退出→重启连上→退出」
//     「连上会话后进程已退出，持续未运行保持process_exited」「连上会话返回但进程仍在运行报cdp_disconnected，随后退出报process_exited」四例均红；
//   - 连上无歌漏发 NoSongStatus 或颠倒先清后发状态 →「模块未就绪→进程退出→重启连上→退出」与 TestRunSessionConnectNoSong 全四例均红。
func TestStartStatus(t *testing.T) {
	d := qqMusicDetails()
	noAdminDetail := i18n.T("Cannot read the QQ Music process (QQ Music may be running as administrator)")
	noSongDetail := i18n.T("QQ Music has no current song")

	cases := []struct {
		name      string
		sc        startScript
		running   func(now time.Duration) bool // 断言每条状态与当时进程在不在一致
		statuses  []string
		connectAt []time.Duration
	}{
		{
			name: "从未运行持续多轮只发一条",
			sc: startScript{
				until:   7 * time.Second,
				connect: func(time.Duration) (*QQMusicMem, error) { return nil, errQQMusicNotRunning },
			},
			running: func(time.Duration) bool { return false },
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
			},
			connectAt: durMs(0, 2000, 4000, 6000),
		},
		{
			// 0s、2s 拒绝访问（seen 置真，只发 1 条 standby/no_admin）；3s 进程退出，4s、6s 返回未运行
			// → 4s 改报 process_exited，6s 仍保持 process_exited 不退回 process_not_running。
			name: "拒绝访问报no_admin，随后进程退出保持process_exited",
			sc: startScript{
				until: 7 * time.Second,
				connect: func(now time.Duration) (*QQMusicMem, error) {
					if now < 3*time.Second {
						return nil, errQQMusicAccessDenied
					}
					return nil, errQQMusicNotRunning
				},
				running: func(now time.Duration) bool { return now < 3*time.Second },
			},
			running: func(now time.Duration) bool { return now < 3*time.Second },
			statuses: []string{
				statusAt(0, "standby", player.ReasonNoAdmin, noAdminDetail),
				statusAt(4*time.Second, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			connectAt: durMs(0, 2000, 4000, 6000),
		},
		{
			// 0s、2s 模块未就绪（seen 置真，报 standby/cdp_disconnected）；3s 进程退出，4s 报 process_exited；
			// 6s 重启连上（首拍 6.05s 无歌报 waiting_song/no_song，第 2 拍 6.1s 进程退出返回外层报 process_exited）；
			// 8.1s 仍未运行保持 process_exited。
			name: "模块未就绪→进程退出→重启连上→退出",
			sc: startScript{
				until: 10 * time.Second,
				connect: func(now time.Duration) (*QQMusicMem, error) {
					switch {
					case now < 3*time.Second:
						return nil, errQQMusicModuleNotReady
					case now < 6*time.Second:
						return nil, errQQMusicNotRunning
					case now == 6*time.Second:
						ticks := 0
						return &QQMusicMem{
							pid:     4321,
							version: "22.82",
							checkValidFn: func() bool {
								ticks++
								return ticks == 1
							},
							readMetaFn: func() (*SongMetadata, error) {
								return &SongMetadata{Name: ""}, nil
							},
						}, nil
					default:
						return nil, errQQMusicNotRunning
					}
				},
				running: func(now time.Duration) bool {
					return now < 3*time.Second || (now >= 6*time.Second && now < msDur(6100))
				},
			},
			running: func(now time.Duration) bool {
				return now < 3*time.Second || (now >= 6*time.Second && now < msDur(6100))
			},
			statuses: []string{
				statusAt(0, "standby", player.ReasonCDPDisconnected, d.Disconnected),
				statusAt(4*time.Second, "waiting_process", player.ReasonProcessExited, d.Exited),
				statusAt(msDur(6050), "waiting_song", player.ReasonNoSong, noSongDetail),
				statusAt(msDur(6100), "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			connectAt: durMs(0, 2000, 4000, 6000, 8100),
		},
		{
			// 0s 未运行 → 2s 连上（零值 QQMusicMem 的 CheckValid 在 2.05s 返回 false 退出会话且进程已退出）
			// → 报 process_exited；4.05s、6.05s 仍未运行保持 process_exited 不退回 process_not_running。
			name: "连上会话后进程已退出，持续未运行保持process_exited",
			sc: startScript{
				until: 7 * time.Second,
				connect: func(now time.Duration) (*QQMusicMem, error) {
					if now == 2*time.Second {
						return &QQMusicMem{pid: 1001, version: "22.82"}, nil
					}
					return nil, errQQMusicNotRunning
				},
				running: func(time.Duration) bool { return false },
			},
			running: func(time.Duration) bool { return false },
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
				statusAt(msDur(2050), "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			connectAt: durMs(0, 2000, 4050, 6050),
		},
		{
			// 0s 连上会话 → 50ms 会话返回但进程仍在运行 → 报 standby/cdp_disconnected + clear_song_data；
			// 2.05s 进程仍在但模块未就绪（维持 standby/cdp_disconnected 不重发）；3s 进程退出，4.05s 报 process_exited。
			name: "连上会话返回但进程仍在运行报cdp_disconnected，随后退出报process_exited",
			sc: startScript{
				until: 6 * time.Second,
				connect: func(now time.Duration) (*QQMusicMem, error) {
					switch {
					case now == 0:
						return &QQMusicMem{pid: 2002, version: "22.82"}, nil
					case now < 3*time.Second:
						return nil, errQQMusicModuleNotReady
					default:
						return nil, errQQMusicNotRunning
					}
				},
				running: func(now time.Duration) bool { return now < 3*time.Second },
			},
			running: func(now time.Duration) bool { return now < 3*time.Second },
			statuses: []string{
				statusAt(msDur(50), "standby", player.ReasonCDPDisconnected, d.Disconnected),
				statusAt(msDur(4050), "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			connectAt: durMs(0, 2050, 4050),
		},
		{
			// 0s 枚举到唯一 PID 101 后该进程在 OpenProcess 前退出（返回 ERROR_INVALID_PARAMETER → attachQQMusic 返
			// errQQMusicModuleNotReady），此时复核进程存在性为 false → 立即报 waiting_process/process_exited，全程 0 条
			// standby，且后续未运行轮次保持 process_exited 不退回 process_not_running。
			name: "附加期间进程已退出（ERROR_INVALID_PARAMETER）不报standby直接报process_exited",
			sc: startScript{
				until: 5 * time.Second,
				connect: func(now time.Duration) (*QQMusicMem, error) {
					if now == 0 {
						ops := attachOps{
							openProcess: func(uint32) (windows.Handle, error) {
								return 0, windows.ERROR_INVALID_PARAMETER
							},
							closeHandle:   func(windows.Handle) error { return nil },
							probeModules:  func(windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) { return 0, 0, 0, 0, false },
							detectVersion: func(*QQMusicMem) {},
						}
						return attachQQMusic([]uint32{101}, ops)
					}
					return nil, errQQMusicNotRunning
				},
				running: func(time.Duration) bool { return false },
			},
			running: func(time.Duration) bool { return false },
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			connectAt: durMs(0, 2000, 4000),
		},
		{
			// 0s 附加遇到 ERROR_ACCESS_DENIED 但复核时进程恰已退出（qqProcessRunning() == false）
			// → 立即报 waiting_process/process_exited，全程 0 条 standby，后续保持 process_exited。
			name: "拒绝访问后进程已退出不报standby直接报process_exited",
			sc: startScript{
				until: 5 * time.Second,
				connect: func(now time.Duration) (*QQMusicMem, error) {
					if now == 0 {
						ops := attachOps{
							openProcess: func(uint32) (windows.Handle, error) {
								return 0, windows.ERROR_ACCESS_DENIED
							},
							closeHandle:   func(windows.Handle) error { return nil },
							probeModules:  func(windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) { return 0, 0, 0, 0, false },
							detectVersion: func(*QQMusicMem) {},
						}
						return attachQQMusic([]uint32{101}, ops)
					}
					return nil, errQQMusicNotRunning
				},
				running: func(time.Duration) bool { return false },
			},
			running: func(time.Duration) bool { return false },
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessExited, d.Exited),
			},
			connectAt: durMs(0, 2000, 4000),
		},
		{
			name: "其他connect错误按未运行处理",
			sc: startScript{
				until:   3 * time.Second,
				connect: func(time.Duration) (*QQMusicMem, error) { return nil, errors.New("snapshot failed") },
			},
			running: func(time.Duration) bool { return false },
			statuses: []string{
				statusAt(0, "waiting_process", player.ReasonProcessNotRunning, d.NotRunning),
			},
			connectAt: durMs(0, 2000),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runStartScript(t, c.sc)
			if r.returned {
				t.Errorf("Start 在 Stop 之前返回了")
			}
			until := c.sc.until
			for i, e := range r.events {
				if e.s == nil || e.at >= until {
					continue
				}
				if e.s.Reason == "" {
					t.Errorf("非歌名状态没有 reason：%v", e)
				}
				if e.s.Status == "standby" && !c.running(e.at) {
					t.Errorf("QQ 音乐不在运行而报 standby：%v", e)
				}
				if e.s.Status == "waiting_process" && c.running(e.at) {
					t.Errorf("QQ 音乐在运行而报 waiting_process：%v", e)
				}
				if e.s.Status == "waiting_process" || e.s.Status == "standby" || e.s.Status == "waiting_song" {
					if i+1 >= len(r.events) || r.events[i+1] != (timedEvent{at: e.at, typ: player.EventClearSongData}) {
						t.Errorf("%v 之后应在同一时刻紧跟 clear_song_data；事件 %v", e, r.events[i:min(i+3, len(r.events))])
					}
				}
			}
			if got := r.statusesBefore(until); !slices.Equal(got, c.statuses) {
				t.Errorf("状态序列\n got  %v\n want %v", got, c.statuses)
			}
			if !slices.Equal(r.connectAt, c.connectAt) {
				t.Errorf("connectQQMusic 调用于 %v，期望 %v", r.connectAt, c.connectAt)
			}
		})
	}
}

// TestStartStopInterruptsSleep 验证 Stop() 在外层 2s 等待期间可立即打断并返回，不卡满 2s。
func TestStartStopInterruptsSleep(t *testing.T) {
	origConnect, origRunning := connectQQMusic, qqProcessRunning
	prevReaders := assocReaders.Swap(1)
	defer func() {
		connectQQMusic, qqProcessRunning = origConnect, origRunning
		assocReaders.Store(prevReaders)
	}()

	t.Run("连接失败等待期间Stop立即返回", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			connectQQMusic = func() (*QQMusicMem, error) { return nil, errQQMusicNotRunning }
			p := New(0, 50)
			done := make(chan struct{})
			go func() { defer close(done); p.Start() }()

			synctest.Wait()
			stoppedAt := time.Now()
			p.Stop()
			<-done
			if elapsed := time.Since(stoppedAt); elapsed >= 500*time.Millisecond {
				t.Errorf("Stop() 后等待了 %v 才退出，应立即打断 2s 等待", elapsed)
			}
		})
	})

	t.Run("会话结束等待期间Stop立即返回", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			connectQQMusic = func() (*QQMusicMem, error) { return &QQMusicMem{pid: 1001, version: "22.82"}, nil }
			qqProcessRunning = func() bool { return false }
			p := New(0, 50)
			done := make(chan struct{})
			go func() { defer close(done); p.Start() }()

			// 推进 60ms 让 runSession 完成第一拍 50ms 休眠并退出回到外层 2s 等待
			time.Sleep(60 * time.Millisecond)
			synctest.Wait()
			stoppedAt := time.Now()
			p.Stop()
			<-done
			if elapsed := time.Since(stoppedAt); elapsed >= 500*time.Millisecond {
				t.Errorf("会话结束后 Stop() 等待了 %v 才退出，应立即打断 2s 等待", elapsed)
			}
		})
	})
}

// TestAttachQQMusicErrorClassification 验证 attachQQMusic 对拒绝访问与模块未就绪的精确分类。
func TestAttachQQMusicErrorClassification(t *testing.T) {
	t.Run("多PID存在ERROR_ACCESS_DENIED且无一附加返回errQQMusicAccessDenied", func(t *testing.T) {
		closeCount := map[windows.Handle]int{}
		ops := attachOps{
			openProcess: func(pid uint32) (windows.Handle, error) {
				if pid == 101 {
					return 0, windows.ERROR_ACCESS_DENIED
				}
				return 0x1002, nil
			},
			closeHandle: func(h windows.Handle) error {
				closeCount[h]++
				return nil
			},
			probeModules: func(windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) {
				return 0, 0, 0, 0, false
			},
			detectVersion: func(*QQMusicMem) {},
		}
		mem, err := attachQQMusic([]uint32{101, 102}, ops)
		if mem != nil {
			t.Fatalf("期望 mem 为 nil，实得 %+v", mem)
		}
		if !errors.Is(err, errQQMusicAccessDenied) {
			t.Fatalf("err = %v，期望 errors.Is(err, errQQMusicAccessDenied)", err)
		}
		if closeCount[0x1002] != 1 {
			t.Errorf("未命中句柄 0x1002 关闭次数 = %d，期望 1", closeCount[0x1002])
		}
	})

	t.Run("syscall.Errno(5)同样识别为errQQMusicAccessDenied", func(t *testing.T) {
		ops := attachOps{
			openProcess:   func(uint32) (windows.Handle, error) { return 0, syscall.Errno(5) },
			closeHandle:   func(windows.Handle) error { return nil },
			probeModules:  func(windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) { return 0, 0, 0, 0, false },
			detectVersion: func(*QQMusicMem) {},
		}
		mem, err := attachQQMusic([]uint32{101}, ops)
		if mem != nil || !errors.Is(err, errQQMusicAccessDenied) {
			t.Fatalf("实得 mem=%v err=%v，期望 (nil, errQQMusicAccessDenied)", mem, err)
		}
	})

	t.Run("部分PID被拒但另一个PID成功附加到QQMusic.dll则成功返回mem", func(t *testing.T) {
		ops := attachOps{
			openProcess: func(pid uint32) (windows.Handle, error) {
				if pid == 201 {
					return 0, windows.ERROR_ACCESS_DENIED
				}
				return 0x2002, nil
			},
			closeHandle: func(windows.Handle) error { return nil },
			probeModules: func(windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) {
				return 0x6000_0000, 0, 0, 0, true
			},
			detectVersion: func(m *QQMusicMem) { m.version = "22.82" },
		}
		mem, err := attachQQMusic([]uint32{201, 202}, ops)
		if err != nil || mem == nil {
			t.Fatalf("期望成功附加 PID 202，实得 mem=%v err=%v", mem, err)
		}
		if mem.pid != 202 || mem.hProcess != 0x2002 || mem.qqmusicDllBase != 0x6000_0000 {
			t.Errorf("返回实例字段不符: %+v", mem)
		}
	})

	t.Run("全部PID可打开但均无QQMusic.dll返回errQQMusicModuleNotReady", func(t *testing.T) {
		closeCount := map[windows.Handle]int{}
		ops := attachOps{
			openProcess: func(pid uint32) (windows.Handle, error) {
				return windows.Handle(0x3000 + pid), nil
			},
			closeHandle: func(h windows.Handle) error {
				closeCount[h]++
				return nil
			},
			probeModules: func(windows.Handle) (uintptr, uintptr, uint32, uintptr, bool) {
				return 0, 0, 0, 0x7600_0000, false
			},
			detectVersion: func(*QQMusicMem) {},
		}
		mem, err := attachQQMusic([]uint32{1, 2}, ops)
		if mem != nil {
			t.Fatalf("期望 mem 为 nil，实得 %+v", mem)
		}
		if !errors.Is(err, errQQMusicModuleNotReady) {
			t.Fatalf("err = %v，期望 errors.Is(err, errQQMusicModuleNotReady)", err)
		}
		if closeCount[0x3001] != 1 || closeCount[0x3002] != 1 {
			t.Errorf("已开句柄应各关闭一次，实得 %v", closeCount)
		}
	})
}

// TestRunSessionConnectNoSong 守 QQ 音乐连上无歌（B2 连上部分）的真实 runSession 行为：
//   - 尚未认领过歌曲（lastName == ""）且读到空串或 "QQ音乐" 前缀占位串 → 先发 waiting_song/no_song，
//     紧随发 1 次 clear_song_data；连续多拍仍为空/占位串不重发状态也不重发 clear_song_data；
//   - 认领真歌（lastName != ""）后中途读到空串或占位串 → 保持现行不报 waiting_song、不发 clear_song_data；
//   - 通道仅余 1 槽时首拍状态入队、清空失败，次拍不重发状态并按序补齐 1 次 clear_song_data。
//
// 变异自证（在隔离临时副本中逐项实测变红）：
//   - 连上无歌漏发 NoSongStatus → 全四例均红；
//   - 连上无歌颠倒先发 clear_song_data 后发 NoSongStatus → 全四例均红；
//   - 去掉 lastName == "" 守卫（误报中途无歌）→「连上无歌→认领真歌→中途读到空或占位串保持现行不报waiting_song也不清屏」红。
func TestRunSessionConnectNoSong(t *testing.T) {
	noSongDetail := i18n.T("QQ Music has no current song")

	runTicks := func(t *testing.T, metas []*SongMetadata, prefill int) []player.Event {
		t.Helper()
		prevReaders := assocReaders.Swap(1)
		defer assocReaders.Store(prevReaders)

		var got []player.Event
		synctest.Test(t, func(t *testing.T) {
			p := New(0, 50)
			for range prefill {
				p.EventCh <- player.Event{Type: "filler"}
			}
			idx := 0
			mem := &QQMusicMem{
				pid:     5001,
				version: "22.82",
				checkValidFn: func() bool {
					return idx < len(metas)
				},
				readMetaFn: func() (*SongMetadata, error) {
					m := metas[idx]
					idx++
					return m, nil
				},
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.runSession(mem, 0)
			}()

			// 若预填了占位事件，先推进 1 拍（50ms）让首拍状态尝试入队并占满最后 1 槽，再排空占位与首拍事件
			if prefill > 0 {
				time.Sleep(60 * time.Millisecond)
				synctest.Wait()
				for len(p.EventCh) > 0 {
					e := <-p.EventCh
					if e.Type != "filler" {
						got = append(got, e)
					}
				}
			}

			<-done
			for len(p.EventCh) > 0 {
				e := <-p.EventCh
				if e.Type != "filler" {
					got = append(got, e)
				}
			}
		})
		return got
	}

	t.Run("首拍空串且连续多拍为空或占位串只发一次状态加一次清除", func(t *testing.T) {
		evs := runTicks(t, []*SongMetadata{
			{Name: ""},
			{Name: ""},
			{Name: "QQ音乐"},
			{Name: "QQ音乐 听我想听"},
		}, 0)
		if len(evs) != 2 {
			t.Fatalf("应恰发 2 条事件（1 状态 + 1 清除），实得 %d 条: %+v", len(evs), evs)
		}
		if evs[0].Type != player.EventStatusUpdate || evs[1].Type != player.EventClearSongData {
			t.Fatalf("顺序应为 status_update → clear_song_data，实得 %s → %s", evs[0].Type, evs[1].Type)
		}
		si := evs[0].Data.(*player.StatusInfo)
		if si.Status != "waiting_song" || si.Reason != player.ReasonNoSong || si.Detail != noSongDetail {
			t.Errorf("状态内容不符: %+v", si)
		}
	})

	t.Run("首拍即为QQ音乐占位串报waiting_song并清除一次", func(t *testing.T) {
		evs := runTicks(t, []*SongMetadata{
			{Name: "QQ音乐 听我想听"},
			{Name: "QQ音乐"},
		}, 0)
		if len(evs) != 2 {
			t.Fatalf("应恰发 2 条事件（1 状态 + 1 清除），实得 %d 条: %+v", len(evs), evs)
		}
		if evs[0].Type != player.EventStatusUpdate || evs[1].Type != player.EventClearSongData {
			t.Fatalf("顺序应为 status_update → clear_song_data，实得 %s → %s", evs[0].Type, evs[1].Type)
		}
		si := evs[0].Data.(*player.StatusInfo)
		if si.Status != "waiting_song" || si.Reason != player.ReasonNoSong || si.Detail != noSongDetail {
			t.Errorf("状态内容不符: %+v", si)
		}
	})

	t.Run("连上无歌→认领真歌→中途读到空或占位串保持现行不报waiting_song也不清屏", func(t *testing.T) {
		evs := runTicks(t, []*SongMetadata{
			{Name: ""},
			{Name: "晴天", Singer: "周杰伦", DurationMs: 269000, ProgressMs: 1000},
			{Name: ""},
			{Name: "QQ音乐"},
			{Name: "QQ音乐 听我想听"},
		}, 0)

		var statuses []*player.StatusInfo
		clearCount := 0
		playingIdx := -1
		for i, e := range evs {
			switch e.Type {
			case player.EventStatusUpdate:
				si := e.Data.(*player.StatusInfo)
				statuses = append(statuses, si)
				if si.Status == "playing" {
					playingIdx = i
				}
			case player.EventClearSongData:
				clearCount++
				if playingIdx >= 0 && i > playingIdx {
					t.Errorf("认领真歌之后不应发 clear_song_data（第 %d 条）", i)
				}
			}
		}
		if len(statuses) != 2 {
			t.Fatalf("应恰发 2 条 status_update（waiting_song → playing），实得 %d 条: %+v", len(statuses), statuses)
		}
		if statuses[0].Status != "waiting_song" || statuses[0].Reason != player.ReasonNoSong {
			t.Errorf("第 1 条状态应为 waiting_song/no_song，实得 %+v", statuses[0])
		}
		if statuses[1].Status != "playing" || statuses[1].Detail != "晴天 - 周杰伦" {
			t.Errorf("第 2 条状态应为 playing/晴天 - 周杰伦，实得 %+v", statuses[1])
		}
		if clearCount != 1 {
			t.Errorf("全程应只发 1 次 clear_song_data（连上无歌首拍），实得 %d 次", clearCount)
		}
		if evs[0].Type != player.EventStatusUpdate || evs[1].Type != player.EventClearSongData {
			t.Errorf("首拍应先发 waiting_song 再紧跟 clear_song_data，实得 %s → %s", evs[0].Type, evs[1].Type)
		}
	})

	t.Run("通道仅余1槽时次拍按序补齐clear_song_data且不重发状态", func(t *testing.T) {
		// BaseEmitter.EventCh 容量 128；预填 127 条，首拍 status 占最后 1 槽、clear 丢弃；排空后次拍补齐 1 次 clear。
		evs := runTicks(t, []*SongMetadata{
			{Name: ""},
			{Name: ""},
			{Name: "QQ音乐"},
		}, 127)
		if len(evs) != 2 {
			t.Fatalf("应恰得 2 条事件（首拍 1 状态 + 次拍补齐 1 清除），实得 %d 条: %+v", len(evs), evs)
		}
		if evs[0].Type != player.EventStatusUpdate || evs[1].Type != player.EventClearSongData {
			t.Fatalf("顺序应为 status_update → clear_song_data，实得 %s → %s", evs[0].Type, evs[1].Type)
		}
		si := evs[0].Data.(*player.StatusInfo)
		if si.Status != "waiting_song" || si.Reason != player.ReasonNoSong || si.Detail != noSongDetail {
			t.Errorf("状态内容不符: %+v", si)
		}
	})
}
