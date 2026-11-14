package sodamusic

// 本文件守汽水退出时释放 inspector 会话（issue #188，AGENTS §7.7.4）：
//   - runSession 察觉退出（inspector 宣告等待断开，或取数连续失败满 exitFailGrace）即返回 true；
//   - Start 随即关闭会话、报 standby 并清歌曲数据，冷却期内不对同一 pid 激活 inspector、不连接；
//   - 反例：托盘播放（取数照常成功，含后台节流下 1/60Hz 的采样）、短于宽限的失败、持续没有歌曲、
//     普通断开，都不按退出处理；冷却只对那一个 pid、只在察觉退出之后生效。
//
// 不碰汽水、不碰 CDP：客户端换成按会话内时刻返回读数的桩，Start 的 watchdog / cdp 入口经 seam 换成桩。
// 会话与 Start 都在 testing/synctest 的泡里跑，轮询的 time.Sleep 与 Start 的等待走假时钟，不真等。
// 读数不带歌词与封面 URL：不起借音译的联网 goroutine，封面 goroutine 发完标题即退出。
//
// 变异自证（本 worktree 里逐条注入、跑完即还原，均实测）记在各用例的注释里。

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/sodamusic/cdp"
)

// errLost 模拟连接断开：桩返回它时把自己记为已关闭，同 cdp.Client 读写失败的路径。
var errLost = errors.New("connection lost")

// errUnavailable 模拟连接还在、取数失败（窗口已关的 no-main-window、JS 被禁止执行等）。
var errUnavailable = errors.New("extraction unsuccessful: no-main-window")

// scriptClient 按会话内时刻（自连上起）返回读数。返回 cdp.ErrHostExiting 时同真客户端一样先关闭自己。
type scriptClient struct {
	script   func(e time.Duration) (*cdp.ExtractionData, error)
	t0       time.Time
	closed   bool
	closedAt time.Duration // 自连上起，第一次关闭的时刻
}

func newScriptClient(script func(time.Duration) (*cdp.ExtractionData, error)) *scriptClient {
	return &scriptClient{script: script, t0: time.Now()}
}

func (c *scriptClient) Extract() (*cdp.ExtractionData, error) {
	if c.closed {
		return nil, errLost
	}
	d, err := c.script(time.Since(c.t0))
	if errors.Is(err, cdp.ErrHostExiting) || errors.Is(err, errLost) {
		c.Close()
	}
	return d, err
}

func (c *scriptClient) IsClosed() bool { return c.closed }

func (c *scriptClient) Close() {
	if !c.closed {
		c.closed = true
		c.closedAt = time.Since(c.t0)
	}
}

// song 是《鲜花》在 progress 秒处的读数：时长 200s、无歌词、无封面 URL。
func song(progress float32, playing bool) *cdp.ExtractionData {
	return &cdp.ExtractionData{
		OK: true, IsPlaying: playing, ProgressSeconds: progress, DurationSeconds: 200,
		MediaID: "7", Name: "鲜花", Artists: []string{"回春丹"},
	}
}

// playingAt 是播放中、按 1Hz 采样的读数（progress 取整秒，同汽水的 progressSeconds）。
func playingAt(e time.Duration) *cdp.ExtractionData {
	return song(float32(e/time.Second), true)
}

// sessionLimit 是各脚本的兜底终点：到点返回 errLost 结束会话，免得实现退化成永不返回时用例挂住。
const sessionLimit = 15 * time.Minute

// runSessionScript 在 synctest 泡里以 script 跑一次 runSession，返回它的结果与返回时刻（自连上起）。
func runSessionScript(t *testing.T, script func(time.Duration) (*cdp.ExtractionData, error)) (exiting bool, at time.Duration) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		c := newScriptClient(func(e time.Duration) (*cdp.ExtractionData, error) {
			if e >= sessionLimit {
				return nil, errLost
			}
			return script(e)
		})
		exiting = p.runSession(c)
		at = time.Since(c.t0)
	})
	return exiting, at
}

// inspector 宣告进入退出前的等待：当拍返回 true，不等宽限。
//
// 变异自证：runSession 里 hostExiting 不计入 exiting（只看宽限），连接已被关掉、按普通断开返回 false，本例
// 变红；察觉退出时一律返回 false，同样变红。
func TestSessionReleasesOnWaitingForDisconnect(t *testing.T) {
	exiting, at := runSessionScript(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		if e < 3*time.Second {
			return playingAt(e), nil
		}
		return nil, cdp.ErrHostExiting
	})
	if !exiting || at != 3*time.Second {
		t.Fatalf("宣告等待断开：exiting=%v at=%v，应为 true、3s", exiting, at)
	}
}

// 取数从 3s 起一直失败、连接还在：失败满 exitFailGrace 的那一拍返回 true，不早于它。
//
// 变异自证：去掉宽限判据（exiting 只看 hostExiting），本例走到兜底终点、返回 false 变红；宽限缩短 1s
// （`>= exitFailGrace-time.Second`），本例因返回早于 3s+宽限变红。
func TestSessionReleasesAfterPersistentFailure(t *testing.T) {
	exiting, at := runSessionScript(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		if e < 3*time.Second {
			return playingAt(e), nil
		}
		return nil, errUnavailable
	})
	if want := 3*time.Second + exitFailGrace; !exiting || at != want {
		t.Fatalf("持续失败：exiting=%v at=%v，应为 true、%v", exiting, at, want)
	}
}

// 反例：多段失败各自短于宽限、之间有成功的取数，计时每次清零，不按退出处理。两段合计远超宽限。
//
// 变异自证：删掉取数成功后的 `failSince = time.Time{}`，本例在第二段失败中返回 true 变红。
func TestSessionTransientFailuresNotExit(t *testing.T) {
	short := exitFailGrace - time.Second
	windows := [][2]time.Duration{
		{5 * time.Second, 5*time.Second + short},
		{5*time.Second + short + 400*time.Millisecond, 5*time.Second + 2*short + 400*time.Millisecond},
		{40 * time.Second, 40*time.Second + short},
	}
	exiting, at := runSessionScript(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		if e >= 60*time.Second {
			return nil, errLost
		}
		for _, w := range windows {
			if e >= w[0] && e < w[1] {
				return nil, errUnavailable
			}
		}
		return playingAt(e), nil
	})
	if exiting || at != 60*time.Second {
		t.Fatalf("瞬时失败：exiting=%v at=%v，应为 false、60s（脚本断开处）", exiting, at)
	}
}

// 反例：关窗到托盘继续播放。取数照常成功，窗口最小化久了采样掉到 1/60Hz（Throttled、progress 每 60s 才变）、
// 中途暂停 3 分钟再继续，十分钟里都不按退出处理。判据只认取数失败与 inspector 的宣告，不认「采样不动」
// 「没在播放」这类会在托盘里出现的现象。
//
// 变异自证：把判据改成「连续 exitFailGrace 没有新采样即退出」（rawChanged 为假计时），本例在第一段
// 节流里返回 true 变红。
func TestSessionTrayPlaybackNotExit(t *testing.T) {
	exiting, at := runSessionScript(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		if e >= 10*time.Minute {
			return nil, errLost
		}
		d := song(float32(e/time.Minute*60), true) // 节流下每 60s 一个采样
		if e >= 4*time.Minute && e < 7*time.Minute {
			d = song(240, false)
		}
		d.Throttled = true
		d.DurationSeconds = 900
		return d, nil
	})
	if exiting || at != 10*time.Minute {
		t.Fatalf("托盘播放：exiting=%v at=%v，应为 false、10m（脚本断开处）", exiting, at)
	}
}

// 反例：已宣告的歌之后一直读到没有歌（汽水在、首页未播放）。取数是成功的，不按退出处理。
//
// 变异自证：已宣告过歌曲后读到没有歌即返回 true，本例变红。
func TestSessionNoSongNotExit(t *testing.T) {
	exiting, at := runSessionScript(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e < 3*time.Second:
			return playingAt(e), nil
		case e < 2*time.Minute:
			return &cdp.ExtractionData{OK: true}, nil
		}
		return nil, errLost
	})
	if exiting || at != 2*time.Minute {
		t.Fatalf("持续无歌：exiting=%v at=%v，应为 false、2m（脚本断开处）", exiting, at)
	}
}

// 反例：普通断开（读写失败）当拍返回 false，失败了一阵再断开也一样，交 Start 照常重连。
//
// 变异自证：出错退出时一律返回 true，本例两个子例变红（上面三个反例与 Start 的普通断开用例同样变红）。
func TestSessionPlainDisconnectNotExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failFrom time.Duration
	}{{"直接断开", 3 * time.Second}, {"失败一阵后断开", time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			exiting, at := runSessionScript(t, func(e time.Duration) (*cdp.ExtractionData, error) {
				switch {
				case e < tc.failFrom:
					return playingAt(e), nil
				case e < 3*time.Second:
					return nil, errUnavailable
				}
				return nil, errLost
			})
			if exiting || at != 3*time.Second {
				t.Fatalf("普通断开：exiting=%v at=%v，应为 false、3s", exiting, at)
			}
		})
	}
}

// ── Start ──

type attach struct {
	pid int
	at  time.Duration
}

type stamped struct {
	at   time.Duration
	desc string // status_update 记为「状态|detail|reason」，clear_song_data 记事件名
}

// startRun 是一次 Start 的记录：各次激活 inspector 的 pid 与时刻、各会话的客户端、状态类事件。
type startRun struct {
	attaches []attach
	clients  []*scriptClient
	events   []stamped
}

// runStart 在 synctest 泡里跑 Start，直到 until 之后的第一次找主进程。pidAt 给出各时刻的主进程 pid（0 为
// 未运行），session 给出第 n 个会话的脚本。时刻均自 Start 开始计。
func runStart(t *testing.T, until time.Duration, pidAt func(time.Duration) int,
	session func(n int) func(time.Duration) (*cdp.ExtractionData, error)) startRun {
	t.Helper()
	var r startRun
	of, oe, oc := findMainPID, ensureInspector, connectCDP
	defer func() { findMainPID, ensureInspector, connectCDP = of, oe, oc }()

	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		t0 := time.Now()
		stopped := false
		findMainPID = func() (int, error) {
			e := time.Since(t0)
			if e >= until {
				if !stopped {
					stopped = true
					p.Stop()
				}
				return 0, errors.New("stopped")
			}
			if pid := pidAt(e); pid != 0 {
				return pid, nil
			}
			return 0, errors.New("not running")
		}
		ensureInspector = func(pid int) error {
			r.attaches = append(r.attaches, attach{pid, time.Since(t0)})
			return nil
		}
		connectCDP = func() (extractSource, error) {
			script := session(len(r.clients))
			c := newScriptClient(func(e time.Duration) (*cdp.ExtractionData, error) {
				if e >= sessionLimit {
					return nil, errLost
				}
				return script(e)
			})
			r.clients = append(r.clients, c)
			return c, nil
		}

		done := make(chan struct{})
		got := make(chan []stamped)
		go func() {
			var evs []stamped
			for {
				select {
				case evt := <-p.EventCh:
					switch evt.Type {
					case player.EventStatusUpdate:
						st := evt.Data.(*player.StatusInfo)
						evs = append(evs, stamped{time.Since(t0), st.Status + "|" + st.Detail + "|" + st.Reason})
					case player.EventClearSongData:
						evs = append(evs, stamped{time.Since(t0), evt.Type})
					}
				case <-done:
					got <- evs
					return
				}
			}
		}()
		p.Start()
		synctest.Wait()
		close(done)
		r.events = <-got
	})
	return r
}

var (
	standbyDisconnected = "standby|" + i18n.T("Soda Music CDP disconnected") + "|" + player.ReasonCDPDisconnected
	playingSong         = "playing|回春丹 - 鲜花|"
)

func always(pid int) func(time.Duration) int { return func(time.Duration) int { return pid } }

// exitAt 是各会话的脚本：第一个会话播放到 at，此后按 how 失败；之后的会话播放 5s 后断开，好让 Start
// 走回找主进程那一步、到 until 停下。
func exitAt(at time.Duration, how error) func(n int) func(time.Duration) (*cdp.ExtractionData, error) {
	return func(n int) func(time.Duration) (*cdp.ExtractionData, error) {
		return func(e time.Duration) (*cdp.ExtractionData, error) {
			switch {
			case n == 0 && e >= at:
				return nil, how
			case n > 0 && e >= 5*time.Second:
				return nil, errLost
			}
			return playingAt(e), nil
		}
	}
}

// eventsBetween 返回 [from, to] 内的状态类事件描述。
func (r startRun) eventsBetween(from, to time.Duration) []string {
	var out []string
	for _, e := range r.events {
		if e.at >= from && e.at <= to {
			out = append(out, e.desc)
		}
	}
	return out
}

// 察觉退出 → 当拍关闭会话、报 standby 并清歌曲数据 → 同一 pid 冷却期内不激活、不连接，期满照常重连。
// 两种察觉途径（inspector 宣告、持续失败满宽限）各跑一遍。
//
// 变异自证：Start 去掉冷却判据，两个子例因第二次激活在释放后 2s 变红；runSession 察觉退出时返回 false，
// 同样变红；Start 不关闭会话（删掉 client.Close()），持续失败子例因会话未关变红（宣告子例的连接由客户端
// 自己关掉，不受影响）。
func TestStartReleasesExitingSodaAndHoldsOffSamePID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		how     error
		release time.Duration // 会话被释放的时刻
	}{
		{"inspector 宣告等待断开", cdp.ErrHostExiting, 3 * time.Second},
		{"取数持续失败", errUnavailable, 3*time.Second + exitFailGrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runStart(t, tc.release+40*time.Second, always(4242), exitAt(3*time.Second, tc.how))

			if c := r.clients[0]; !c.closed || c.closedAt != tc.release {
				t.Fatalf("第一个会话应在 %v 关闭，实为 closed=%v closedAt=%v", tc.release, c.closed, c.closedAt)
			}
			if got := r.eventsBetween(tc.release, tc.release); len(got) < 2 ||
				got[0] != standbyDisconnected || got[1] != player.EventClearSongData {
				t.Fatalf("释放当拍应先报 standby 再清歌曲数据，实为 %q", got)
			}
			if len(r.attaches) < 2 {
				t.Fatalf("冷却期满应重新激活，实际只激活 %d 次", len(r.attaches))
			}
			second := r.attaches[1]
			if second.pid != 4242 || second.at < tc.release+reattachCooldown || second.at > tc.release+reattachCooldown+5*time.Second {
				t.Fatalf("第二次激活应在释放后的冷却期满之后不久（%v～%v），实为 %+v",
					tc.release+reattachCooldown, tc.release+reattachCooldown+5*time.Second, second)
			}
		})
	}
}

// 反例：普通断开（不是察觉退出）不设冷却，照旧 2s 后重连同一 pid。
//
// 变异自证：Start 在每次会话结束都记下 releasedPID，本例因第二次激活晚于冷却期变红。
func TestStartReattachesPromptlyAfterPlainDisconnect(t *testing.T) {
	r := runStart(t, 20*time.Second, always(4242), exitAt(3*time.Second, errLost))
	if len(r.attaches) < 2 || r.attaches[1] != (attach{4242, 5 * time.Second}) {
		t.Fatalf("普通断开后应在 5s 重新激活 4242，实为 %+v", r.attaches)
	}
	if got := r.eventsBetween(3*time.Second, 3*time.Second); len(got) < 2 || got[0] != standbyDisconnected {
		t.Fatalf("断开当拍应报 standby，实为 %q", got)
	}
}

// 反例：冷却只认被释放的那个 pid。汽水退出后马上重开（新主进程），照常连新进程。
//
// 变异自证：冷却判据去掉 `pid == releasedPID`（对任何 pid 生效），本例因新 pid 的激活晚于冷却期变红。
func TestStartAttachesNewPIDDuringCooldown(t *testing.T) {
	pidAt := func(e time.Duration) int {
		if e < 4*time.Second {
			return 4242
		}
		return 5151
	}
	r := runStart(t, 20*time.Second, pidAt, exitAt(3*time.Second, cdp.ErrHostExiting))
	if len(r.attaches) < 2 || r.attaches[1] != (attach{5151, 5 * time.Second}) {
		t.Fatalf("释放 4242 后应在 5s 激活新进程 5151，实为 %+v", r.attaches)
	}
}

// 反例：连上之后一直正常播放（含托盘播放），不释放、不报 standby，只激活一次。
func TestStartKeepsHealthySession(t *testing.T) {
	r := runStart(t, 5*time.Minute, always(4242), func(n int) func(time.Duration) (*cdp.ExtractionData, error) {
		return func(e time.Duration) (*cdp.ExtractionData, error) {
			if n > 0 || e >= 4*time.Minute {
				return nil, errLost // 结束会话，好让 Start 走到 until
			}
			return playingAt(e), nil
		}
	})
	if r.attaches[0] != (attach{4242, 0}) || r.clients[0].closedAt != 4*time.Minute {
		t.Fatalf("会话应从 0s 一直持续到脚本断开的 4m，实为 attaches=%+v closedAt=%v", r.attaches, r.clients[0].closedAt)
	}
	for _, e := range r.eventsBetween(0, 4*time.Minute-time.Millisecond) {
		if e == standbyDisconnected {
			t.Fatalf("正常播放期间不应报 standby：%q", r.eventsBetween(0, 4*time.Minute))
		}
	}
	if got := r.eventsBetween(0, 0); len(got) < 3 || got[2] != playingSong {
		t.Fatalf("连上即应宣告播放中，实为 %q", got)
	}
}
