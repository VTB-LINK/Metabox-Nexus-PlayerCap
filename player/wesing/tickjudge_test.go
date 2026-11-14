package wesing

// 本文件只测 pollLyrics 每拍的位置判定（tickJudge，#177 设计 §4.5～§4.7、§4.10、§6.3）：按拍喂入直读值、时刻与
// 最小化、拖窗标志，经 emitTick 发事件，断言发了什么、在哪一拍、要不要复位行号。读数、窗口检查与歌词行不在
// 其内。#190 起判定不再向速率核对报「推进未核实」（设计 doc/design/wesing-small-seek-design.md §3.3）。

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// jt 是一拍：距起点的秒数、直读值，本拍的最小化与拖窗标志。
type jt struct {
	at, raw     float64
	min, moving bool
}

type judgeLog struct {
	events []string  // 「拍@类型@位置」，status_update 记为 status:<状态>
	resets []string  // 复位行号的拍
	fwd    int       // 判为前跳的拍数
	resume []float64 // 发 playback_resume 的拍（一拍至多一条的检查用）
}

var judgeT0 = time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)

// runJudge 依次喂入各拍，事件按拍收集。
func runJudge(ticks []jt, dur float32) judgeLog {
	p := newTestPlayer()
	p.EventCh = make(chan player.Event, 256)
	j := newTickJudge()
	var l judgeLog
	for _, tk := range ticks {
		now := judgeT0.Add(time.Duration(math.Round(tk.at * 1e9)))
		o := j.step(float32(tk.raw), now, tk.min, tk.moving, dur)
		p.emitTick(o, float32(tk.raw), "测试 - 歌手", dur)
		at := fmt.Sprintf("%.2f", tk.at)
		if o.resetLine {
			l.resets = append(l.resets, at)
		}
		if o.fwd {
			l.fwd++
		}
		for _, e := range drainEvents(p) {
			switch d := e.Data.(type) {
			case *player.StatusInfo:
				l.events = append(l.events, at+"@status:"+d.Status)
			case *player.PlaybackTimeInfo:
				l.events = append(l.events, fmt.Sprintf("%s@%s@%.2f", at, e.Type, d.Position))
				if e.Type == player.EventPlaybackResume {
					l.resume = append(l.resume, tk.at)
				}
			}
		}
	}
	return l
}

// span 从 at0 起每 step 秒一拍、到 at1 为止（含），直读值为 raw(at)。
func span(step, at0, at1 float64, raw func(at float64) float64) []jt {
	var out []jt
	for i := 0; ; i++ {
		at := at0 + float64(i)*step
		if at > at1+1e-9 {
			return out
		}
		out = append(out, jt{at: at, raw: raw(at)})
	}
}

func tag(ticks []jt, min, moving bool) []jt {
	for i := range ticks {
		ticks[i].min, ticks[i].moving = min, moving
	}
	return ticks
}

func joinJt(parts ...[]jt) []jt {
	var out []jt
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func lin(start float64) func(float64) float64 { return func(at float64) float64 { return start + at } }

func stay(v float64) func(float64) float64 { return func(float64) float64 { return v } }

// stepped 是 PCM 式的读数：按 50 ms 台阶取整。
func stepped(start float64) func(float64) float64 {
	return func(at float64) float64 { return math.Floor((start+at)/0.05+1e-6) * 0.05 }
}

// eventsOnly 去掉拍号，只看事件与位置。
func eventsOnly(evts []string) string {
	var out []string
	for _, e := range evts {
		out = append(out, e[strings.Index(e, "@")+1:])
	}
	return strings.Join(out, " ")
}

func moreThanOnePerTick(ats []float64) bool {
	for i := 1; i < len(ats); i++ {
		if ats[i] == ats[i-1] {
			return true
		}
	}
	return false
}

// 前跳（设计 §6.3 的 32～34）：播放中前跳发一条 playback_resume、复位行号；越过行首的次序由 pollLyrics 在
// emitTick 之后才推歌词行保证。
//
// 变异自证（实测）：
//   - 删去前跳判定：32、34 没有 playback_resume、33 一条都没有而红；
//   - 前跳不复位行号：32 红；
//   - 去掉最小化门控、门槛改为 1 s 等由 lyric 包的 seekfwd_test.go 与下面的用例守。
func TestTickJudgeForwardSeek(t *testing.T) {
	t.Run("32 播放中 +5 s", func(t *testing.T) {
		l := runJudge(span(0.03, 0, 10, func(at float64) float64 {
			if at >= 5-1e-9 {
				return 35 + at
			}
			return 30 + at
		}), 300)
		if got, want := strings.Join(l.events, " "), "5.01@playback_resume@40.01"; got != want {
			t.Errorf("事件 %s，want %s", got, want)
		}
		if got := strings.Join(l.resets, " "); got != "5.01" {
			t.Errorf("复位行号的拍 %s，want 5.01", got)
		}
	})

	// 33：2026-11-13 录音的拖动段（111059–112184），相邻两条事件之间按 30 ms 一拍均匀插值。前后各接 1 倍速
	// 推进，拖动开始时位置连续。逐拍超前过门槛的 18 拍各一条，每拍推进不足 2 s 的三段与停顿的一拍不发。
	t.Run("33 录音拖动段", func(t *testing.T) {
		pts := [][2]float64{{0, 52.402405}, {0.152, 78.994125}, {0.182, 79.04413}, {0.243, 101.430885}, {0.334, 120.12817},
			{0.638, 139.65646}, {0.789, 159.18474}, {0.880, 170.40312}, {1.003, 175.80457}, {1.125, 179.95952}}
		ticks := span(0.03, -1, -0.03, lin(53.402405))
		for i := 1; i < len(pts); i++ {
			a, b := pts[i-1], pts[i]
			n := int(math.Max(1, math.Round((b[0]-a[0])/0.03)))
			for k := 1; k <= n; k++ {
				f := float64(k) / float64(n)
				ticks = append(ticks, jt{at: a[0] + f*(b[0]-a[0]), raw: a[1] + f*(b[1]-a[1])})
			}
		}
		ticks = append(ticks, span(0.03, 1.155, 3, lin(179.95952-1.125))...)
		l := runJudge(ticks, 285.86087)
		if l.fwd != 18 || len(l.resume) != 18 || moreThanOnePerTick(l.resume) {
			t.Errorf("前跳 %d 拍、playback_resume %d 条，want 各 18（%v）", l.fwd, len(l.resume), l.events)
		}
		for _, at := range l.resume {
			if (at > 0.334+1e-6 && at < 0.638+1e-6) || at > 0.880+1e-6 {
				t.Errorf("每拍推进不足 2 s 的段里发了 playback_resume：%.3f", at)
			}
		}
	})

	t.Run("34 跳到末尾后停住", func(t *testing.T) {
		l := runJudge(joinJt(span(0.03, 0, 2, lin(100)), span(0.03, 2.01, 4.5, stay(285.80))), 285.86087)
		if got, want := eventsOnly(l.events), "playback_resume@285.80 status:paused playback_pause@285.80"; got != want {
			t.Errorf("事件 %s，want %s", got, want)
		}
	})
}

// 暂停与恢复（设计 §6.3 的 35～39）：暂停中拖动由恢复路径发一条，不复位行号；暂停中往回拖照旧走回跳。
//
// 变异自证（实测）：
//   - 去掉前跳的暂停门控：35、36 复位了行号而红（事件相同：跳转处理块代发了恢复）；
//   - 前跳判定挪到暂停与恢复判定之后（用判定之后的暂停状态）：35、36 红；
//   - 跳转处理块不把已判的暂停置为播放：37 红；
//   - 暂停计时改回 time.Since（不用传入的本拍时刻）的结果见 TestTickJudgeQuiet。
func TestTickJudgePauseResume(t *testing.T) {
	playStop := func(stopAt, resumeAt float64, after func(at float64) float64, end float64) []jt {
		return joinJt(span(0.03, 0, stopAt, lin(30)), span(0.03, stopAt+0.03, resumeAt-0.03, stay(30+stopAt)), span(0.03, resumeAt, end, after))
	}
	cases := []struct {
		name   string
		ticks  []jt
		want   string
		resets string
	}{
		{"35 暂停中拖到更晚 +30 s、仍暂停", playStop(2, 4.5, stay(62), 7),
			"status:paused playback_pause@32.00 status:playing playback_resume@62.00 status:paused playback_pause@62.00", ""},
		{"36 暂停 1.5 s 后同一行内前跳 5 s、接着播", playStop(2, 3.53, lin(37-3.53), 7),
			"status:paused playback_pause@32.00 status:playing playback_resume@37.00", ""},
		{"37 暂停中往回拖 5 s、仍暂停", playStop(2, 3.5, stay(27), 6),
			"status:paused playback_pause@32.00 status:playing playback_resume@27.00 status:paused playback_pause@27.00", "3.50"},
		{"38、39 停住 1 s 判暂停，恢复后 3 s 无事件", playStop(2, 3.5, lin(32-3.47), 6.5),
			"status:paused playback_pause@32.00 status:playing playback_resume@32.03", ""},
	}
	for _, c := range cases {
		l := runJudge(c.ticks, 300)
		if got := eventsOnly(l.events); got != c.want {
			t.Errorf("%s：事件 %s，want %s", c.name, got, c.want)
		}
		if got := strings.Join(l.resets, " "); got != c.resets {
			t.Errorf("%s：复位行号的拍 %q，want %q", c.name, got, c.resets)
		}
		if moreThanOnePerTick(l.resume) {
			t.Errorf("%s：同一拍多于一条 playback_resume：%v", c.name, l.events)
		}
	}
}

// 正常推进、抖动与兜底模式（设计 §6.3 的 40～50）：都不发事件，最小化期间被前跳的还原后发一条。
//
// 变异自证（实测）：
//   - 基准取 V 首次读到的时刻：43 发 playback_resume 而红；
//   - 基准时刻每拍更新：44 发 playback_resume 而红；
//   - 超前量不加经过时间：42 每拍一条而红，43、44 同样红；
//   - 去掉最小化门控：48 在标志翻转之前每拍一条而红；
//   - step 调 seekBack 时不传最小化标志：45～48、50 在最小化期间每拍一条 playback_resume 而红（#145 的刷屏）；
//   - 删去前跳判定：46、48 没有 playback_resume 而红；
//   - 暂停计时改回 time.Since（不用传入的本拍时刻）：本机墙钟晚于用例时刻，停住的第一拍就判为暂停，40、41、50
//     与 TestTickJudgeFirstChange 的 52、76 红。
func TestTickJudgeQuiet(t *testing.T) {
	jitter := span(0.03, 0, 60, stepped(30))
	for i := range jitter {
		if i%7 == 6 { // 晚一拍：读到上一拍的值
			jitter[i].raw = jitter[i-1].raw
		}
	}
	var slow []jt
	for at := 0.0; at <= 60; at += 2.03 {
		slow = append(slow, jt{at: at, raw: 30 + at})
	}
	minimized := func(restore func(at float64) float64) []jt {
		return joinJt(span(0.03, 0, 1.98, lin(30)), tag(span(0.03, 2.0, 62.0, stay(32)), true, false), span(0.03, 62.03, 64, restore))
	}
	cases := []struct {
		name  string
		ticks []jt
		dur   float32
		want  string
	}{
		{"40 60 s 正常推进，50 ms 台阶，每 7 拍晚一拍", jitter, 300, ""},
		{"41 停住 0.8 s 后接着走", joinJt(span(0.03, 0, 2, lin(30)), span(0.03, 2.03, 2.8, stay(32)), span(0.03, 2.83, 5, lin(29.2))), 300, ""},
		{"42 轮询 2000 ms（每拍 2.03 s）", slow, 300, ""},
		{"43 设计审 F1 的序列（拖窗）", tag([]jt{{at: 0, raw: 50.00}, {at: 2.10, raw: 50.05}, {at: 4.20, raw: 54.20}}, false, true), 300, ""},
		{"44 拖窗停 2.3 s 后追上", joinJt(span(0.03, 0, 2.97, lin(47)), tag(span(0.03, 3.00, 5.28, stay(50)), false, true), span(0.03, 5.31, 7, lin(47))), 300, ""},
		{"45、49 最小化 60 s，还原后与外推相近", minimized(lin(30)), 300, ""},
		{"46 最小化期间被前跳 10 s", minimized(lin(40)), 300, "62.03@playback_resume@102.03"},
		{"47 最小化期间暂停、外推跑过头 5 s", minimized(lin(25)), 300, "62.03@playback_resume@87.03"},
		{"48 最小化标志滞后：标志翻转之前读数已追上并前跳 10 s", joinJt(span(0.03, 0, 1.98, lin(30)),
			tag(span(0.03, 2.0, 60.98, stay(32)), true, false), tag(span(0.03, 61.01, 62.0, lin(40)), true, false), span(0.03, 62.03, 64, lin(40))),
			300, "62.03@playback_resume@102.03"},
		{"50 外推到总时长被钳住后还原", joinJt(span(0.03, 0, 1.98, lin(90)), tag(span(0.03, 2.0, 20.0, stay(92)), true, false), span(0.03, 20.03, 20.5, stay(100))),
			100, ""},
	}
	for _, c := range cases {
		l := runJudge(c.ticks, c.dur)
		if got := strings.Join(l.events, " "); got != c.want {
			t.Errorf("%s：事件 %q，want %q", c.name, got, c.want)
		}
	}
}

// 首拍、回跳与第一次变化（设计 §6.3 的 51、52、75、76）：第一次变化不新增前跳事件，seekBack 与恢复路径的既有
// 事件照常。
//
// 75、76 的事件断言守「第一次变化不新增前跳事件，seekBack 与恢复路径的既有事件照常」（设计审第 3 轮）。
//
// 变异自证（实测）：暂停计时改回 time.Since，52、76 红（见 TestTickJudgeQuiet）。起始读数以首读时刻作基准
// （第一个读数即 known）在这几个序列里不产生事件（首读与第一次变化只差一拍），本函数不红，由 lyric 包
// seekfwd_test.go 的 28 与 TestSeekBaseKnown 守。
func TestTickJudgeFirstChange(t *testing.T) {
	cases := []struct {
		name   string
		ticks  []jt
		want   string
		resets string
	}{
		{"51 首拍读数比宣告位置大 10 s", span(0.03, 0, 3, lin(40)), "", ""},
		{"52 重唱回 0", joinJt(span(0.03, 0, 5, lin(130)), span(0.03, 5.01, 5.49, stay(0)), span(0.03, 5.52, 8, lin(-5.49))),
			"5.01@playback_resume@0.00", "5.01"},
		{"75 第一次变化即回跳", []jt{{at: 0, raw: 30}, {at: 2, raw: 0}, {at: 4, raw: 2}, {at: 6, raw: 4}},
			"2.00@playback_resume@0.00", "2.00"},
		{"76 第一次变化发生在已判暂停之后", joinJt(span(0.03, 0, 1.98, stay(30)), span(0.03, 2.01, 4, lin(28.04))),
			"1.05@status:paused 1.05@playback_pause@30.00 2.01@status:playing 2.01@playback_resume@30.05", ""},
	}
	for _, c := range cases {
		l := runJudge(c.ticks, 300)
		if got := strings.Join(l.events, " "); got != c.want {
			t.Errorf("%s：事件 %q，want %q", c.name, got, c.want)
		}
		if got := strings.Join(l.resets, " "); got != c.resets {
			t.Errorf("%s：复位行号的拍 %q，want %q", c.name, got, c.resets)
		}
		if l.fwd != 0 {
			t.Errorf("%s：判出前跳 %d 拍，want 0", c.name, l.fwd)
		}
	}
}
