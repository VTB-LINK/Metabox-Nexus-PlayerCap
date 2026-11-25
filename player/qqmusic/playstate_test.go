package qqmusic

// 本文件只测按 QQ 音乐自己的播放状态字段判暂停与恢复（#172，doc/design/qqmusic-start-stall-design.md 方案 D）：
// 字段描述与读取（classify、playStateAddr、readPlayState、knownVersions）、判定函数 judgeTick 与 movedWindow（§7.2），
// 经 sessionreplay_test.go 的复述回放 §7.3 的各组情形（P：§1.3 的各情形；L：暂停中补发歌词；V：20.05 的 0、1 与去抖；
// X：交叉核对；Q：轮询间隔；U：标志未知时与修复之前逐事件相同），并以源码扫描钉住 runSession 的接线（§7.4）。
//
// 回放与 runSession 调用同一个 judgeTick，判定的变异由 §7.2、§7.3 的用例抓；runSession 的接线只有文件末的源码扫描抓。
// 变异在一份分离的副本里逐条注入、跑完即还原，结果记在各用例的注释里（均实测）。「复述里」指只改
// sessionreplay_test.go 的复述。

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 字段描述与读取（§7.2）

// TestPlayStateClassify：classify 按各版本的取值集合分类，不写死 2、3、4。22.82、22.52 的 2 是载入（第 4 轮，设计 §6.1）；
// 第 5 轮的 E 类（22.60～22.81）同 22.82，A 类（22.16～22.41）0 暂停、1 在播、2 与 3 载入，21.81 未收录（零值条目）。
//
// 变异自证（均实测）：classify 写死 22.82 的取值（不看条目），20.05 各项红；22.82 的条目把 2 判为未知、把 2 判为在播（第
// 3 轮的取值）、把 4 判为在播、恒为未知、把 5 判为暂停，22.82 的相应各项红；22.81 也填上字段，零值条目一项红（第 5 轮
// 之前，22.81 当时未收录）。第 5 轮：21.81 也填上 22.16 的字段，零值条目一项红；22.26 的字段留空、22.31 的载入漏掉 3、
// 22.60 照 A 类填（暂停 0、在播 1、载入 2 与 3）、22.41 的暂停与在播取值对调，各自红。
func TestPlayStateClassify(t *testing.T) {
	type c struct {
		raw  uint32
		want playState
	}
	enumCases := []c{{2, stateLoading}, {3, statePlaying}, {4, statePaused},
		{0, stateUnknown}, {1, stateUnknown}, {5, stateUnknown}, {6, stateUnknown}, {0x304, stateUnknown},
		{0x400, stateUnknown}, {0x80000004, stateUnknown}, {0xFFFFFFFF, stateUnknown}}
	// A 类（第 5 轮）：0 暂停、1 在播、2 与 3 载入；4（E 类的暂停值）、5 与只差高位的值为未知。
	aCases := []c{{0, statePaused}, {1, statePlaying}, {2, stateLoading}, {3, stateLoading},
		{4, stateUnknown}, {5, stateUnknown}, {0x100, stateUnknown}, {0x80000000, stateUnknown}, {0xFFFFFFFF, stateUnknown}}
	cases := map[string][]c{
		"22.82": enumCases,
		"22.52": enumCases,
		// E 类（第 5 轮）同 22.82：A 类的 0、1 套不上，为未知。
		"22.81": enumCases,
		"22.76": enumCases,
		"22.71": enumCases,
		"22.60": enumCases,
		"22.41": aCases,
		"22.31": aCases,
		"22.26": aCases,
		"22.22": aCases,
		"22.16": aCases,
		"20.05": {{0, statePlaying}, {1, statePaused}, {2, stateUnknown}, {3, stateUnknown}, {4, stateUnknown},
			{0x100, stateUnknown}, {0xFFFFFFFF, stateUnknown}},
		"21.81": {{0, stateUnknown}, {1, stateUnknown}, {2, stateUnknown}, {3, stateUnknown}, {4, stateUnknown}}, // 未收录：零值条目
	}
	for ver, cs := range cases {
		for _, x := range cs {
			if got := knownVersions[ver].PlayState.classify(x.raw); got != x.want {
				t.Errorf("%s 的 %#x：classify = %d，期望 %d", ver, x.raw, got, x.want)
			}
		}
	}
}

// TestPlayStateAddr：只在版本号精确匹配时读；借用最近版本的偏移表、读不到版本号而借用最新版本的，一律不读。
//
// 变异自证（均实测）：playStateAddr 不看 fallbackTo，三个借用的例子红；21.81 也填上 22.16 的字段，「未收录」一例红；
// 22.26 的偏移写成 0xC99FA4、22.26 的字段留空，「精确匹配（22.26）」一例红。
func TestPlayStateAddr(t *testing.T) {
	const base = uintptr(0x5EC50000)
	cases := []struct {
		name              string
		version, fallback string
		offsets           dllOffsets
		want              uintptr
	}{
		{"未收录（21.81）", "21.81", "", knownVersions["21.81"], 0},
		{"精确匹配（22.82）", "22.82", "", knownVersions["22.82"], base + 0xD3BBA8},
		{"精确匹配（20.05）", "20.05", "", knownVersions["20.05"], base + 0xAF35B0},
		{"精确匹配（22.26）", "22.26", "", knownVersions["22.26"], base + 0xC99FA0},
		{"按版本号距离借用（22.83 → 22.82）", "22.83", "22.82", knownVersions["22.82"], 0},
		{"按版本号距离借用（22.27 → 22.26）", "22.27", "22.26", knownVersions["22.26"], 0},
		{"读不到版本号、借用最新版本", "unknown", "22.82", knownVersions["22.82"], 0},
	}
	for _, c := range cases {
		m := &QQMusicMem{qqmusicDllBase: base, version: c.version, fallbackTo: c.fallback, offsets: c.offsets}
		if got := m.playStateAddr(); got != c.want {
			t.Errorf("%s：playStateAddr = %#x，期望 %#x", c.name, got, c.want)
		}
	}
}

// TestReadPlayStateReportsFailure：读失败必须报告出来，不能当作取值 0（20.05 的在播值）。句柄为 0 时
// ReadProcessMemory 必失败。
//
// 变异自证（均实测，第 5 轮加版本后重跑）：readPlayState 不看 ReadProcessMemory 的返回与读到的字节数（即 ReadUint32 的
// 读法，失败时返回 (0, true)），本例红。
func TestReadPlayStateReportsFailure(t *testing.T) {
	// 22.31、22.26 的暂停值是 0：读失败若按取值分类就成了暂停。21.81 未收录（地址为 0，不读）。
	for _, ver := range []string{"20.05", "22.81", "22.31", "22.26", "21.81"} {
		m := &QQMusicMem{qqmusicDllBase: 0x5EC50000, version: ver, offsets: knownVersions[ver]}
		if v, ok := m.readPlayState(); v != 0 || ok {
			t.Errorf("%s、句柄为 0：readPlayState = (%d, %v)，期望 (0, false)", ver, v, ok)
		}
	}
}

// TestKnownVersionsPlayState：除 21.81 外的 12 个已收录版本收录了播放状态字段，且逐项等于设计 §6.1 的表；21.81 为零值。
// 加版本时照抄一整段会带上它，这条拦下。
//
// 变异自证（均实测）：22.81 也填上 22.82 的字段（第 5 轮之前）、22.82 的偏移写成 0xD3BBAC、20.05 的 MinPause 写成 400 ms、
// 22.52 的暂停取值写成 3、22.82 的 2 写回在播（第 3 轮的取值），各自红。第 5 轮：21.81 也填上 22.16 的字段、22.26 的偏移
// 写成 0xC99FA4、22.26 的字段留空、22.31 的载入漏掉 3、22.60 照 A 类填、22.41 的暂停与在播取值对调，各自红。
func TestKnownVersionsPlayState(t *testing.T) {
	want := map[string]playStateSpec{
		"22.82": {Off: 0xD3BBA8, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		"22.52": {Off: 0xCC91C0, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		"20.05": {Off: 0xAF35B0, Paused: []uint32{1}, Playing: []uint32{0}, MinPause: 300 * time.Millisecond},
		// 第 5 轮（#222、#232，设计 §1.3「其余已收录版本与 22.26」）：E 类同 22.82，A 类 0 暂停、1 在播、2 与 3 载入。
		"22.81": {Off: 0xD3BBC0, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		"22.76": {Off: 0xCEE400, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		"22.71": {Off: 0xCEC400, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		"22.60": {Off: 0xCDE1B0, Paused: []uint32{4}, Playing: []uint32{3}, Loading: []uint32{2}},
		"22.41": {Off: 0xCB2840, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		"22.31": {Off: 0xCA52C0, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		"22.26": {Off: 0xC99FA0, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		"22.22": {Off: 0xC95F90, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
		"22.16": {Off: 0xC87CF0, Paused: []uint32{0}, Playing: []uint32{1}, Loading: []uint32{2, 3}, Opening: []uint32{3}},
	}
	for ver, o := range knownVersions {
		w := want[ver] // 不在表里即零值
		if !reflect.DeepEqual(o.PlayState, w) {
			t.Errorf("%s 的 PlayState = %+v，期望 %+v", ver, o.PlayState, w)
		}
	}
	for ver := range want {
		if _, ok := knownVersions[ver]; !ok {
			t.Errorf("knownVersions 里没有 %s", ver)
		}
	}
}

// TestMovedWindow：交叉核对乙的窗口取 1 s 与两倍轮询间隔的较大者（§6.3）。
//
// 变异自证（均实测）：窗口固定 1 s，600、1000 两例红；取 2 × 轮询、不取较大者，31、500 两例红。
func TestMovedWindow(t *testing.T) {
	for _, c := range [][2]int{{31, 1000}, {500, 1000}, {600, 1200}, {1000, 2000}} {
		if got := movedWindow(time.Duration(c[0]) * time.Millisecond); got != time.Duration(c[1])*time.Millisecond {
			t.Errorf("movedWindow(%dms) = %v，期望 %dms", c[0], got, c[1])
		}
	}
}

// ---------------------------------------------------------------- judgeTick（§7.2 J 组）

var jt0 = time.Unix(1_700_000_000, 0)

func jat(ms int) time.Time { return jt0.Add(time.Duration(ms) * time.Millisecond) }

func msd(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }

// jIn 是 J 组未注明的入参：22.82 的条目、rawOK 为真、poll 31 ms、已认领、读数未变、idle 0，时刻为 t0 + now ms。
func jIn(raw uint32, now int) judgeIn {
	return judgeIn{spec: knownVersions["22.82"].PlayState, raw: raw, rawOK: true, poll: 31 * time.Millisecond,
		claimed: true, now: jat(now)}
}

// jIn2005 同 jIn，字段描述取 20.05 的条目。
func jIn2005(raw uint32, now int) judgeIn {
	in := jIn(raw, now)
	in.spec = knownVersions["20.05"].PlayState
	return in
}

// expectVerdict 核对 resume、pause、tripped，并要求 pausedSeek 为假；暂停中跳转的拍用 expectPausedSeek。
func expectVerdict(t *testing.T, what string, v verdict, resume, pause bool, tripped string) {
	t.Helper()
	if v.resume != resume || v.pause != pause || v.tripped != tripped || v.pausedSeek {
		t.Errorf("%s：resume=%v pause=%v tripped=%q pausedSeek=%v，期望 resume=%v pause=%v tripped=%q pausedSeek=false",
			what, v.resume, v.pause, v.tripped, v.pausedSeek, resume, pause, tripped)
	}
}

// expectPausedSeek 核对暂停中跳转的拍：pausedSeek 为真，不判恢复、不判暂停、不触发核对；本段暂停自跳转起重新计，movedAt 与
// resumeGate 的待确认都清零。
func expectPausedSeek(t *testing.T, what string, s judgeState, v verdict) {
	t.Helper()
	if !v.pausedSeek || v.resume || v.pause || v.tripped != "" || s.bad {
		t.Errorf("%s：pausedSeek=%v resume=%v pause=%v tripped=%q bad=%v，期望只有 pausedSeek", what, v.pausedSeek, v.resume, v.pause, v.tripped, s.bad)
	}
	if !s.movedAt.IsZero() || !s.resume.pendingAt.IsZero() {
		t.Errorf("%s：movedAt=%v resume.pendingAt=%v，期望都清零", what, s.movedAt, s.resume.pendingAt)
	}
}

// TestJudgeTick 是设计 §7.2 的 J 组（J12、J13 在第 2 轮删去，编号不复用）。
//
// 变异自证（均实测，改的是 judgeTick 或 movedWindow）：
//   - 停走判定比较改为含等号：J1（1000）红。
//   - 不看 rawOK（读失败按取值分类）：J21 红。
//   - 去抖不生效（不看 MinPause）：J16、J17 红；去抖比较改为严格大于：J16（t₀+300）红。
//   - 待定按在播处理：J19（读数未变）红；待定时仍做甲或乙：J20 红。
//   - 未满去抖结束时不报 flash、或已满的也报：J17、J18 红；恢复也去抖：J18 红。flash 改回自读到暂停值的那一拍
//     （pausedSince）起算：J17 红，回放 V2（闪 200、300）、V7、X14 红；读失败与未知值的拍也记作起点、以读失败或
//     未知值结束也报、此前没有在播读数也报：J17 红（代码审第 1、2 轮 F1）。
//   - 不看轮询间隔：J9（1001）红；轮询门槛改为严格小于 1 s：J9（1000）红。
//   - 在播时仍按停走判暂停：J4（1001）红；甲门槛改 2 s、4 s 或比较含等号：J4 红；甲不要求已认领：J6 红；
//     甲在本拍由标志判恢复时也量：J5 红。
//   - 乙窗口固定 1 s：J7（600、1000）红；取 2 × 轮询：J7（31）红；比较改严格小于：J7 各组前一个红；乙计入 seek：
//     J10 红；判暂停时不清 movedAt：J11 红（第 5 轮重测；第 4 轮 J10 也红，第 5 轮 J10 的 movedAt 由暂停中跳转的分支清）。
//   - 触发之后不改按未知：J8、J14 红。
//   - 载入（第 4 轮）：22.82 的条目把 2 判为在播，J3（raw 2 判了恢复）与 J23 的 22.82 三例红（idle 3001、5000 的拍
//     触发甲）；载入直接按未知、停走自锚点量，J23 两版三例红（idle 3001、5000 的拍当拍判暂停）；loadingSince 不随非载入
//     的拍清零，J23（三）t₀+1600 红；载入的拍做甲，J23 两版三例红（idle 3001、5000 的拍触发甲）。
//   - 暂停中跳转（第 5 轮，设计 §6.7 第 7 条）：去掉这一分支（第 4 轮的写法），J10、J24、J25 红；分支不看状态，J19（待定的
//     seek）、J24 红；状态写成暂停或待定，J19、J24 红；去掉 in.seek，J7、J8、J11、J24 红；不清 movedAt，J10、J24 红；不调
//     s.resume.reset()，J24（pendingAt）、J25（t₀+200 判了恢复）红；同时置 v.resume 或 v.pause，J10、J24、J25 红；以读成功
//     代替状态为暂停，J19、J24 红；乙计入 seek，J10、J24 红。
func TestJudgeTick(t *testing.T) {
	t.Run("J1 标志未知：停走严格超过 1 s 判暂停", func(t *testing.T) {
		for _, c := range []struct {
			idle  int
			pause bool
		}{{1000, false}, {1001, true}} {
			in := jIn(0, 0)
			in.idle = msd(c.idle)
			_, v := judgeTick(judgeState{}, in)
			expectVerdict(t, fmt.Sprintf("idle %d", c.idle), v, false, c.pause, "")
		}
	})
	t.Run("J2 暂停值：已认领当拍判暂停，未认领不判", func(t *testing.T) {
		_, v := judgeTick(judgeState{}, jIn(4, 0))
		expectVerdict(t, "已认领", v, false, true, "")
		in := jIn(4, 0)
		in.claimed = false
		_, v = judgeTick(judgeState{}, in)
		expectVerdict(t, "未认领", v, false, false, "")
	})
	t.Run("J3 在播值：已判暂停即判恢复；载入值按未知、读数未变不判", func(t *testing.T) {
		for _, c := range []struct {
			raw    uint32
			resume bool
		}{{3, true}, {2, false}} {
			in := jIn(c.raw, 0)
			in.paused = true
			_, v := judgeTick(judgeState{}, in)
			expectVerdict(t, fmt.Sprintf("raw %d", c.raw), v, c.resume, false, "")
		}
	})
	t.Run("J4 甲：报在播而停走严格超过 3 s", func(t *testing.T) {
		for _, c := range []struct {
			idle int
			trip bool
		}{{1001, false}, {3000, false}, {3001, true}} {
			in := jIn(3, 0)
			in.idle = msd(c.idle)
			s, v := judgeTick(judgeState{}, in)
			if c.trip {
				expectVerdict(t, fmt.Sprintf("idle %d", c.idle), v, false, true, "stalled")
			} else {
				expectVerdict(t, fmt.Sprintf("idle %d", c.idle), v, false, false, "")
			}
			if s.bad != c.trip {
				t.Errorf("idle %d：bad=%v，期望 %v", c.idle, s.bad, c.trip)
			}
		}
	})
	t.Run("J5 判恢复的拍不量甲", func(t *testing.T) {
		in := jIn(3, 0)
		in.paused, in.idle = true, msd(5000)
		s, v := judgeTick(judgeState{}, in)
		expectVerdict(t, "已判暂停、idle 5000", v, true, false, "")
		if s.bad {
			t.Error("判恢复的拍不该判不可信")
		}
	})
	t.Run("J6 未认领不量甲", func(t *testing.T) {
		in := jIn(3, 0)
		in.claimed, in.idle = false, 10*time.Second
		s, v := judgeTick(judgeState{}, in)
		expectVerdict(t, "未认领、idle 10 s", v, false, false, "")
		if s.bad {
			t.Error("未认领不该判不可信")
		}
	})
	t.Run("J7 乙：窗口 max(1 s, 2 × 轮询)，含端点", func(t *testing.T) {
		for _, g := range []struct{ poll, gap int }{{31, 1000}, {600, 1200}, {1000, 2000}} {
			for _, d := range []int{0, 1} {
				first := jIn(4, 0)
				first.poll, first.paused, first.changed, first.step = msd(g.poll), true, true, 116
				s, v := judgeTick(judgeState{}, first)
				expectVerdict(t, "第一次变化", v, false, false, "")
				second := first
				second.now = jat(g.gap + d)
				s, v = judgeTick(s, second)
				what := fmt.Sprintf("poll %d、相隔 %d", g.poll, g.gap+d)
				if d == 0 {
					expectVerdict(t, what, v, false, false, "moved")
					if !s.bad {
						t.Errorf("%s：应判不可信", what)
					}
				} else {
					expectVerdict(t, what, v, false, false, "")
					if s.bad || !s.movedAt.Equal(jat(g.gap+1)) {
						t.Errorf("%s：bad=%v movedAt=%v，期望不判、movedAt 换为第二次的时刻", what, s.bad, s.movedAt.Sub(jt0))
					}
				}
			}
		}
	})
	t.Run("J8 乙触发的那一拍按未知经 resumeGate", func(t *testing.T) {
		for _, c := range []struct {
			step   int64
			resume bool
		}{{600, true}, {300, false}} {
			first := jIn(4, 0)
			first.paused, first.changed, first.step = true, true, 116
			s, _ := judgeTick(judgeState{}, first)
			second := first
			second.now, second.step = jat(1000), c.step
			_, v := judgeTick(s, second)
			expectVerdict(t, fmt.Sprintf("step %d", c.step), v, c.resume, false, "moved")
		}
	})
	t.Run("J9 轮询超过 1 s 不用标志", func(t *testing.T) {
		for _, c := range []struct {
			poll  int
			pause bool
		}{{1000, true}, {1001, false}} {
			in := jIn(4, 0)
			in.poll = msd(c.poll)
			_, v := judgeTick(judgeState{}, in)
			expectVerdict(t, fmt.Sprintf("poll %d", c.poll), v, false, c.pause, "")
		}
	})
	// 第 5 轮（设计 §6.7 第 7 条）：标志仍为暂停的暂停中 seek 不判恢复也不判暂停（第 4 轮为判恢复、同一拍再判暂停）。
	t.Run("J10 暂停中 seek、标志仍为暂停：pausedSeek，不计入乙、清 movedAt", func(t *testing.T) {
		in := jIn(4, 0)
		in.paused, in.changed, in.seek, in.step = true, true, true, 70000
		s, v := judgeTick(judgeState{movedAt: jat(-500)}, in)
		expectPausedSeek(t, "seek", s, v)
	})
	t.Run("J11 判暂停清 movedAt：上一段暂停的变化不计入", func(t *testing.T) {
		s := judgeState{movedAt: jat(0)}
		in := jIn(3, 100)
		in.paused = true
		s, v := judgeTick(s, in)
		expectVerdict(t, "变 3", v, true, false, "")
		s, v = judgeTick(s, jIn(4, 200))
		expectVerdict(t, "变 4", v, false, true, "")
		if !s.movedAt.IsZero() {
			t.Errorf("判暂停那一拍 movedAt=%v，期望清零", s.movedAt.Sub(jt0))
		}
		in = jIn(4, 300)
		in.paused, in.changed, in.step = true, true, 116
		s, v = judgeTick(s, in)
		expectVerdict(t, "再暂停后的第一次变化", v, false, false, "")
		if s.bad || !s.movedAt.Equal(jat(300)) {
			t.Errorf("bad=%v movedAt=%v，期望只记时刻", s.bad, s.movedAt.Sub(jt0))
		}
	})
	t.Run("J14 已判不可信：暂停值按未知", func(t *testing.T) {
		_, v := judgeTick(judgeState{bad: true}, jIn(4, 0))
		expectVerdict(t, "bad、raw 4、idle 0", v, false, false, "")
	})
	t.Run("J15 标志未知：已判暂停经 resumeGate（同 #171）", func(t *testing.T) {
		in := jIn(0, 0)
		in.paused, in.changed, in.step = true, true, 500
		_, v := judgeTick(judgeState{}, in)
		expectVerdict(t, "step 500", v, true, false, "")
		for _, c := range []struct {
			after  int
			resume bool
		}{{1000, true}, {1001, false}} {
			in.step = 499
			s, v := judgeTick(judgeState{}, in)
			expectVerdict(t, "step 499", v, false, false, "")
			next := jIn(0, c.after)
			next.paused, next.changed, next.step = true, true, 116
			_, v = judgeTick(s, next)
			expectVerdict(t, fmt.Sprintf("499 之后 %d ms 再变", c.after), v, c.resume, false, "")
		}
	})
	t.Run("J16 20.05 去抖：暂停值持续不少于 300 ms 才判", func(t *testing.T) {
		var s judgeState
		for _, c := range []struct {
			at    int
			st    playState
			pause bool
		}{{0, stateSettling, false}, {299, stateSettling, false}, {300, statePaused, true}} {
			var v verdict
			s, v = judgeTick(s, jIn2005(1, c.at))
			expectVerdict(t, fmt.Sprintf("t₀+%d", c.at), v, false, c.pause, "")
			if v.st != c.st {
				t.Errorf("t₀+%d：st=%d，期望 %d", c.at, v.st, c.st)
			}
		}
	})
	// 上界自这段暂停值之前最近一次读到在播值的拍起算，到结束这段的在播值的拍为止；两端任一不是有效的在播读数即不报
	// （代码审第 1、2 轮 F1）。上一拍在 t₀−100 时为 350 ms，大于去抖时长 300 ms 而仍未判暂停（按拍取样的上界，不是实长）。
	// 读失败与取值集合以外的拍不算起点（暂停值可能早于它们出现，如实际 −1000～250 ms 的暂停、−50 读失败时，自 −50
	// 起算的 300 ms 不是上界）；此前一个在播读数都没有、或以读失败或未知值结束的，不报。各拍的判定照旧（这里都不判）。
	t.Run("J17 20.05 未满去抖就结束：报持续时长上界", func(t *testing.T) {
		type jt struct{ at, raw int } // raw 为 failed 即读失败
		run := []jt{{0, 1}, {200, 1}}
		seq := func(before []jt, after ...jt) []jt {
			return append(append(append([]jt{}, before...), run...), after...)
		}
		for _, c := range []struct {
			name  string
			ticks []jt
			flash int // 在 t₀+250 那一拍报出的上界，0 为整段都不报
		}{
			{"上一拍在播、在 t₀−100", seq([]jt{{-100, 0}}, jt{250, 0}), 350},
			{"上一拍在播、在 t₀−31", seq([]jt{{-31, 0}}, jt{250, 0}), 281},
			{"上一拍读失败，更早一拍在播", seq([]jt{{-200, 0}, {-50, failed}}, jt{250, 0}), 450},
			{"上一拍为取值集合以外的值，更早一拍在播", seq([]jt{{-200, 0}, {-50, 2}}, jt{250, 0}), 450},
			{"此前只有读失败", seq([]jt{{-50, failed}}, jt{250, 0}), 0},
			{"会话第一拍即为暂停值", seq(nil, jt{250, 0}), 0},
			{"以读失败结束", seq([]jt{{-31, 0}}, jt{250, failed}, jt{280, 0}), 0},
			{"以取值集合以外的值结束", seq([]jt{{-31, 0}}, jt{250, 2}, jt{280, 0}), 0},
		} {
			var s judgeState
			for _, x := range c.ticks {
				in := jIn2005(uint32(max(x.raw, 0)), x.at)
				in.rawOK = x.raw != failed
				var v verdict
				s, v = judgeTick(s, in)
				expectVerdict(t, fmt.Sprintf("%s：t₀%+d", c.name, x.at), v, false, false, "")
				want := 0
				if x.at == 250 {
					want = c.flash
				}
				if v.flash != msd(want) || want > 0 && v.flashLimit != msd(300) {
					t.Errorf("%s：t₀%+d 的 flash=%v flashLimit=%v，期望 %dms（去抖 300ms）", c.name, x.at, v.flash, v.flashLimit, want)
				}
			}
		}
	})
	t.Run("J18 20.05 已满去抖：恢复不去抖、不报 flash", func(t *testing.T) {
		var s judgeState
		s, _ = judgeTick(s, jIn2005(1, 0))
		s, v := judgeTick(s, jIn2005(1, 300))
		expectVerdict(t, "t₀+300", v, false, true, "")
		in := jIn2005(0, 2000)
		in.paused = true
		_, v = judgeTick(s, in)
		expectVerdict(t, "t₀+2000 回 0", v, true, false, "")
		if v.flash != 0 {
			t.Errorf("flash=%v，期望 0", v.flash)
		}
	})
	t.Run("J19 20.05 待定：不判恢复，seek 照旧判恢复", func(t *testing.T) {
		in := jIn2005(1, 0)
		in.paused = true
		_, v := judgeTick(judgeState{}, in)
		expectVerdict(t, "读数未变", v, false, false, "")
		in.changed, in.seek, in.step = true, true, 70000
		_, v = judgeTick(judgeState{}, in)
		expectVerdict(t, "seek", v, true, false, "")
	})
	t.Run("J20 20.05 待定：不做甲、乙", func(t *testing.T) {
		in := jIn2005(1, 0)
		in.idle = msd(5000)
		s, v := judgeTick(judgeState{}, in)
		expectVerdict(t, "未判暂停、idle 5000", v, false, false, "")
		if s.bad {
			t.Error("待定的拍不该量甲")
		}
		in = jIn2005(1, 0)
		in.paused, in.changed, in.step = true, true, 116
		s, v = judgeTick(judgeState{movedAt: jat(-500)}, in)
		expectVerdict(t, "已判暂停、非 seek 变化", v, false, false, "")
		if s.bad || !s.movedAt.Equal(jat(-500)) {
			t.Errorf("bad=%v movedAt=%v，期望不判、movedAt 不变", s.bad, s.movedAt.Sub(jt0))
		}
	})
	t.Run("J21 20.05 读失败：按未知走停走判定，不是在播", func(t *testing.T) {
		in := jIn2005(0, 0)
		in.rawOK, in.idle = false, msd(1001)
		_, v := judgeTick(judgeState{}, in)
		expectVerdict(t, "rawOK 假、idle 1001", v, false, true, "")
		if v.st != stateUnknown {
			t.Errorf("st=%d，期望未知", v.st)
		}
	})
	t.Run("J22 22.82 暂停值第一拍即判（MinPause 为 0）", func(t *testing.T) {
		_, v := judgeTick(judgeState{}, jIn(4, 0))
		expectVerdict(t, "raw 4", v, false, true, "")
	})
	// 载入值 2（第 4 轮，设计 §6.1）：按停走判，停走取 idle 与自进入载入值那一拍起的较短者，严格超过 1 s 判暂停；不做甲。
	t.Run("J23 载入值：停走自进入载入值的那一拍起量", func(t *testing.T) {
		type jt struct {
			at, raw, idle int
			pause         bool
		}
		for _, ver := range []string{"22.82", "22.52"} {
			for _, c := range []struct {
				name  string
				ticks []jt
			}{
				{"（一）首次读到 2、idle 3001", []jt{{0, 2, 3001, false}}},
				{"（二）自 t₀ 起连续为 2", []jt{{0, 2, 5000, false}, {1000, 2, 5000, false}, {1001, 2, 5000, true}}},
				{"（三）中间一拍为 3，自 t₀+600 重新起量", []jt{{0, 2, 5000, false}, {500, 3, 0, false}, {600, 2, 5000, false},
					{1600, 2, 5000, false}, {1601, 2, 5000, true}}},
			} {
				var s judgeState
				for _, x := range c.ticks {
					in := jIn(uint32(x.raw), x.at)
					in.spec, in.idle = knownVersions[ver].PlayState, msd(x.idle)
					var v verdict
					s, v = judgeTick(s, in)
					expectVerdict(t, fmt.Sprintf("%s %s：t₀+%d", ver, c.name, x.at), v, false, x.pause, "")
					if s.bad {
						t.Errorf("%s %s：t₀+%d 判了不可信", ver, c.name, x.at)
					}
				}
			}
		}
	})
	// #243：A 类的开播值 3 每拍重新起量，读到它的拍不按停走判暂停；转为 2 后自最后一拍 3 起量。暂停值照判，2 照旧。
	//
	// 变异自证（均实测）：judgeTick 不看 Opening，（一）（二）与 P22 红；开播值只把本拍停走记为 0、不重设起量（转 2 时
	// 自首拍载入值起量），（二）红。
	t.Run("J23b 开播值：读到的拍不按停走判暂停", func(t *testing.T) {
		type jt struct {
			at, raw, idle int
			pause         bool
		}
		for _, ver := range []string{"22.26", "22.16"} {
			for _, c := range []struct {
				name  string
				ticks []jt
			}{
				{"（一）连续为 3、idle 5000", []jt{{0, 3, 5000, false}, {1001, 3, 5000, false}, {3001, 3, 5000, false}}},
				{"（二）3 转 2，自最后一拍 3 起量", []jt{{0, 3, 5000, false}, {500, 3, 5000, false}, {600, 2, 5000, false},
					{1500, 2, 5000, false}, {1501, 2, 5000, true}}},
				{"（三）对照：连续为 2", []jt{{0, 2, 5000, false}, {1000, 2, 5000, false}, {1001, 2, 5000, true}}},
				{"（四）3 之后读到暂停值 0", []jt{{0, 3, 5000, false}, {31, 0, 5000, true}}},
			} {
				var s judgeState
				for _, x := range c.ticks {
					in := jIn(uint32(x.raw), x.at)
					in.spec, in.idle = knownVersions[ver].PlayState, msd(x.idle)
					var v verdict
					s, v = judgeTick(s, in)
					expectVerdict(t, fmt.Sprintf("%s %s：t₀+%d", ver, c.name, x.at), v, false, x.pause, "")
					if s.bad {
						t.Errorf("%s %s：t₀+%d 判了不可信", ver, c.name, x.at)
					}
				}
			}
		}
		// 已判暂停时读到 3、读数小步前进：照旧经 resumeGate 判恢复
		var s judgeState
		var v verdict
		for i, at := range []int{0, 300} {
			in := jIn(3, at)
			in.spec, in.paused, in.changed, in.step = knownVersions["22.26"].PlayState, true, true, 300
			s, v = judgeTick(s, in)
			expectVerdict(t, fmt.Sprintf("已判暂停、3、小步第 %d 次", i+1), v, i == 1, false, "")
		}
	})
	// 第 5 轮（设计 §6.7 第 7 条）：已判暂停、seek、状态为暂停（已满去抖）才是暂停中跳转。入参除注明外同 J10；正例的入参状态
	// 预置非零的 movedAt 与 resumeGate 待确认，查两者都清零。反例里状态为在播、载入、取值集合以外、读失败、待定、已判不可信、
	// 轮询超过 1 s 的照旧判恢复。
	t.Run("J24 暂停中 seek：只有状态为暂停才不判恢复", func(t *testing.T) {
		seekIn := func(ver string, raw uint32) judgeIn {
			in := jIn(raw, 0)
			in.spec = knownVersions[ver].PlayState
			in.paused, in.changed, in.seek, in.step = true, true, true, 70000
			return in
		}
		pre := judgeState{movedAt: jat(-500), resume: resumeGate{pendingAt: jat(-300)}}
		for _, c := range []struct {
			ver string
			raw uint32
		}{{"22.82", 4}, {"22.31", 0}, {"22.26", 0}} {
			s, v := judgeTick(pre, seekIn(c.ver, c.raw))
			expectPausedSeek(t, fmt.Sprintf("%s 的 %d", c.ver, c.raw), s, v)
		}
		notOK := seekIn("22.82", 4)
		notOK.rawOK = false
		slow := seekIn("22.82", 4)
		slow.poll = msd(1001)
		for _, c := range []struct {
			name string
			s    judgeState
			in   judgeIn
		}{
			{"22.82 的 3（在播）", judgeState{}, seekIn("22.82", 3)},
			{"22.82 的 2（载入，按未知）", judgeState{}, seekIn("22.82", 2)},
			{"22.82 的 0（取值集合以外）", judgeState{}, seekIn("22.82", 0)},
			{"rawOK 假", judgeState{}, notOK},
			{"20.05 的 1、自本拍起读到（待定）", judgeState{}, seekIn("20.05", 1)},
			{"bad 真的 22.82 的 4", judgeState{bad: true}, seekIn("22.82", 4)},
			{"poll 1001 ms 的 22.82 的 4", judgeState{}, slow},
		} {
			_, v := judgeTick(c.s, c.in)
			expectVerdict(t, c.name, v, true, false, "")
		}
		unpaused := seekIn("22.82", 4)
		unpaused.paused = false
		_, v := judgeTick(judgeState{}, unpaused)
		expectVerdict(t, "未判暂停的 22.82 的 4", v, false, true, "")
		moved := seekIn("22.82", 4)
		moved.seek, moved.step = false, 116
		s, v := judgeTick(judgeState{}, moved)
		expectVerdict(t, "已判暂停、非 seek 变化的 22.82 的 4", v, false, false, "")
		if s.bad || !s.movedAt.Equal(jat(0)) {
			t.Errorf("非 seek 变化：bad=%v movedAt=%v，期望走乙、movedAt 记为本拍", s.bad, s.movedAt.Sub(jt0))
		}
	})
	// 第 5 轮第 1 轮设计审的反例：跳转之前一次标志未知的小步留下的待确认，不得与跳转之后又一次小步凑成两次（同 #171 的
	// 1 s 窗口）。三拍依次以上一拍返回的状态调用；对照去掉跳转那一拍，t₀+200 判恢复，说明本例对待确认敏感。
	t.Run("J25 跨跳转的连续调用：跳转清 resumeGate 的待确认", func(t *testing.T) {
		small := func(now int, step int64) judgeIn {
			in := jIn(0, now)
			in.rawOK, in.paused, in.changed, in.step = false, true, true, step
			return in
		}
		jump := jIn(4, 100)
		jump.paused, jump.changed, jump.seek, jump.step = true, true, true, 120000-50100
		s, v := judgeTick(judgeState{}, small(0, 100)) // 50000 → 50100
		expectVerdict(t, "t₀ 小步", v, false, false, "")
		if !s.resume.pendingAt.Equal(jat(0)) {
			t.Errorf("t₀：resume.pendingAt=%v，期望 t₀", s.resume.pendingAt.Sub(jt0))
		}
		s, v = judgeTick(s, jump) // 50100 → 120000
		expectPausedSeek(t, "t₀+100 跳转", s, v)
		s, v = judgeTick(s, small(200, 116)) // 120000 → 120116
		expectVerdict(t, "t₀+200 小步", v, false, false, "")
		if !s.resume.pendingAt.Equal(jat(200)) {
			t.Errorf("t₀+200：resume.pendingAt=%v，期望 t₀+200", s.resume.pendingAt.Sub(jt0))
		}

		s, _ = judgeTick(judgeState{}, small(0, 100))
		_, v = judgeTick(s, small(200, 116))
		expectVerdict(t, "对照：去掉跳转那一拍，t₀+200 小步", v, true, false, "")
	})
}

// ---------------------------------------------------------------- 回放夹具的补充

// failed 在 flags 里表示自该时刻起读失败（PlayStateOK 为假）。
const failed = -1

// flags 按「自某毫秒起为某值」分段给出每拍首读的播放状态字段：flags(t₁, v₁, t₂, v₂, …)；v 为 failed 即读失败。
func flags(kv ...int) func(int) (uint32, bool) {
	return func(ms int) (uint32, bool) {
		v := failed
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i] <= ms {
				v = kv[i+1]
			}
		}
		if v < 0 {
			return 0, false
		}
		return uint32(v), true
	}
}

// stateAtMs 是 r.stateAt 在 ms 时的读数，没设即读失败：重读的播放状态字段取重读时刻的标志。
func stateAtMs(r *replay, ms int) (uint32, bool) {
	if r.stateAt == nil {
		return 0, false
	}
	return r.stateAt(ms)
}

// pr 是期望的一条暂停或恢复：typ 为 "pause" 或 "resume"，ms 为拍，pos 为位置（秒）。
type pr struct {
	typ string
	ms  int
	pos float32
}

func (p pr) String() string { return fmt.Sprintf("%s(%.3f)@%d", p.typ, p.pos, p.ms) }

// prFrom 返回不早于 ms 那一拍发出的 playback_pause 与 playback_resume。
func prFrom(r *replay, ms int) []replayEvent {
	var out []replayEvent
	for _, e := range evFrom(r, ms) {
		if e.typ == "playback_pause" || e.typ == "playback_resume" {
			out = append(out, e)
		}
	}
	return out
}

func expectPR(t *testing.T, r *replay, fromMs int, want ...pr) {
	t.Helper()
	got := prFrom(r, fromMs)
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = got[i].typ == "playback_"+want[i].typ && got[i].ms == want[i].ms && approx(got[i].pos, want[i].pos)
	}
	if !ok {
		t.Fatalf("%dms 起的暂停与恢复\n  得到 %v\n  期望 %v\n全部事件：%v", fromMs, got, want, r.evs)
	}
}

// stateCase 是一例回放：run 跑读数（含补取、重读的登记），st 给出每拍首读的标志，want 为自 from 起的暂停与恢复；
// tripAt 为判不可信的那一拍（0 即不触发）；flashes 为报出的未满去抖的暂停值；old 为不设 stateAt（修复之前）时自
// from 起暂停与恢复的形状，"-" 为不查。ver 为空即 22.82 的条目。
type stateCase struct {
	name    string
	ver     string
	from    int
	run     func(t *testing.T, r *replay)
	st      func(int) (uint32, bool)
	want    []pr
	tripAt  int
	flashes [][2]int
	old     string
}

func runStateCase(t *testing.T, c stateCase, st func(int) (uint32, bool)) *replay {
	t.Helper()
	ver := c.ver
	if ver == "" {
		ver = "22.82"
	}
	r := newReplay(-0.4, zeroSongs())
	r.spec, r.stateAt = knownVersions[ver].PlayState, st
	c.run(t, r)
	return r
}

func runTo(to int, ss ...func(t *testing.T) seg) func(t *testing.T, r *replay) {
	return func(t *testing.T, r *replay) {
		segsList := make([]seg, len(ss))
		for i, s := range ss {
			segsList[i] = s(t)
		}
		r.run(0, to, tick, segs(segsList...))
	}
}

// sg 把一段常量读数包成 runTo 的参数；pt 把 playTo 包成同样的形状。
func sg(from int, name string, base uint32, moving bool) func(*testing.T) seg {
	return func(*testing.T) seg { return seg{from, name, base, moving} }
}

func pt(name string, at uint32, T int) func(*testing.T) seg {
	return func(t *testing.T) seg { return playTo(t, name, at, T) }
}

// ---------------------------------------------------------------- 回放（§7.3 P、V、X 组）

const sT = 3100 // §7.1 的 T

// stateCases 是 §7.3 能写成一张表的各例。拍号由 firstTick 推出（拍长 31 ms，「@T+X」即不早于 T+X 的第一拍），
// 位置按 segs 的读数（走着的一段每毫秒前进 1，故拍号不在段首时位置比段首多出拍号之差）。
func stateCases() []stateCase {
	const T = sT
	ft := firstTick
	x1 := []func(*testing.T) seg{pt("A", 80000, T), sg(T, "A", 80000, false), sg(T+5000, "A", 81200, true), sg(T+8000, "A", 84200, false)}
	x5 := []func(*testing.T) seg{sg(0, "A", 50000, false), sg(300, "A", 50300, false), sg(600, "A", 50600, false),
		sg(900, "A", 50900, false), sg(1200, "A", 51200, true)}
	p8 := []func(*testing.T) seg{pt("A", 50000, T), sg(T, "A", 50000, false), sg(T+3000, "A", 120000, false)}
	p18 := []func(*testing.T) seg{pt("A", 87334, T), sg(T, "A", 87334, false), sg(T+3000, "A", 251215, false), sg(T+4400, "A", 252469, true)}
	return []stateCase{
		// ---- P：§1.3 的各情形（22.82）
		{name: "P1 换歌（29.3 s）", from: T, old: "pause resume",
			run: runTo(T+4000, pt("A", 25936, T), sg(T, "A", 0, false), sg(T+200, "B", 0, false), sg(T+1600, "B", 1277, true)),
			st:  flags(0, 3, T+200, 2, T+400, 3)},
		{name: "P2 换歌后立刻暂停、补写 +766（52.1 s）", from: T, old: "pause resume pause resume",
			run: runTo(T+10000, pt("A", 10820, T), sg(T, "", 0, false), sg(T+200, "B", 0, false), sg(T+600, "B", 209, false),
				sg(T+3600, "B", 975, false), sg(T+8600, "B", 2043, true)),
			st:   flags(0, 3, T, 2, T+400, 3, T+1400, 4, T+7400, 3),
			want: []pr{{"pause", ft(T + 1400), 0.209}, {"resume", ft(T + 7400), 0.975}}},
		{name: "P3 暂停与恢复（12.9 s）", from: T, old: "pause resume",
			run:  runTo(T+9500, pt("A", 15557, T), sg(T, "A", 15557, false), sg(T+2800, "A", 15580, false), sg(T+7000, "A", 16764, true)),
			st:   flags(0, 3, T+200, 4, T+5800, 3),
			want: []pr{{"pause", ft(T + 200), 15.557}, {"resume", ft(T + 5800), 15.58}}},
		{name: "P4 拖动（130.5 s）", from: T, old: "resume pause resume",
			run:  runTo(T+3000, pt("A", 37198, T), sg(T, "A", 81091, false), sg(T+1400, "A", 82322, true)),
			st:   flags(0, 3),
			want: []pr{{"resume", T, 81.091}}},
		{name: "P5 单曲循环回到开头", from: T, old: "resume pause resume",
			run:  runTo(T+2500, pt("A", 188000, T), sg(T, "A", 0, false), sg(T+2077, "A", 1648, true)),
			st:   flags(0, 3),
			want: []pr{{"resume", T + 403, 0}}},
		{name: "P6 误触暂停（132.5 s）", from: T, old: "pause resume",
			run:  runTo(T+3000, pt("A", 82647, T), sg(T, "A", 82972, false), sg(T+1600, "A", 83993, true)),
			st:   flags(0, 3, T, 4, T+600, 3),
			want: []pr{{"pause", T, 82.972}, {"resume", ft(T + 600), 82.972}}},
		{name: "P7 换歌后立刻暂停、读数未动（69.7 s）", from: T, old: "pause resume",
			run:  runTo(T+9000, pt("A", 10983, T), sg(T, "B", 0, false), sg(T+3200, "B", 278, false), sg(T+7800, "B", 1416, true)),
			st:   flags(0, 3, T, 2, T+200, 3, T+600, 4, T+6600, 3),
			want: []pr{{"pause", ft(T + 600), 0}, {"resume", ft(T + 6600), 0.278}}},
		// 第 5 轮（设计 §6.7 第 7 条）：标志仍为暂停，跳转那一拍只在落点补发 pause（第 4 轮为同一拍 resume、pause）。
		{name: "P8 暂停中拖动（构成 seek）", from: T, old: "pause resume pause",
			run:  runTo(T+4500, p8...),
			st:   flags(0, 3, T+100, 4),
			want: []pr{{"pause", ft(T + 100), 50}, {"pause", ft(T + 3000), 120}}},
		{name: "P8b 暂停中拖动、QQ 起播", from: T, old: "pause resume pause resume",
			run:  runTo(T+6000, append(slices.Clone(p8), sg(T+4400, "A", 121400, true))...),
			st:   flags(0, 3, T+100, 4, T+3000, 3),
			want: []pr{{"pause", ft(T + 100), 50}, {"resume", ft(T + 3000), 120}}},
		{name: "P9 暂停中不构成 seek 的拖动", from: T, old: "pause resume pause resume",
			run:  runTo(T+10000, pt("A", 50000, T), sg(T, "A", 50000, false), sg(T+5000, "A", 52000, false), sg(T+9200, "A", 53100, true)),
			st:   flags(0, 3, T+100, 4, T+8000, 3),
			want: []pr{{"pause", ft(T + 100), 50}, {"resume", ft(T + 8000), 52}}},
		{name: "P10 暂停中换歌、QQ 未起播", from: T, old: "pause pause",
			run:  runTo(T+4500, pt("A", 50000, T), sg(T, "A", 50000, false), sg(T+3000, "B", 0, false)),
			st:   flags(0, 3, T+100, 4),
			want: []pr{{"pause", ft(T + 100), 50}, {"pause", ft(T + 3000), 0}}},
		{name: "P10b 暂停中换歌、QQ 起播", from: T, old: "pause pause resume",
			run:  runTo(T+6000, pt("A", 50000, T), sg(T, "A", 50000, false), sg(T+3000, "B", 0, false), sg(T+4400, "B", 1200, true)),
			st:   flags(0, 3, T+100, 4, T+3000, 2, T+3200, 3),
			want: []pr{{"pause", ft(T + 100), 50}}},
		{name: "P11 会话开头停在暂停的歌上", from: 0, old: "pause",
			run:  runTo(1500, sg(0, "A", 50000, false)),
			st:   flags(0, 4),
			want: []pr{{"pause", 0, 50}}},
		// 首读为 3、重读（取词 300 ms 之后）读到 4，其后一直为 4：flags 在 T 之后即为 4，重读的标志取重读时刻的。
		{name: "P12 认领拍的重读带回标志", from: T, old: "pause",
			run: func(t *testing.T, r *replay) {
				st, ok := stateAtMs(r, T+300)
				r.claimRereads[T] = replayReread{fetchMs: 300, name: "B", ms: 0, state: st, stateOK: ok}
				fillRun(r, T, 300, T+1500, segs(playTo(t, "A", 80000, T), seg{T, "B", 0, false}))
			},
			st:   flags(0, 3, T+1, 4),
			want: []pr{{"pause", T, 0}}},
		// 重读歌名为空串，不采用：认领那一拍按首读的 3，不判；取词之后的拍（T+331 起，31 ms 一拍）在 T+400 之后判。
		{name: "P12b 认领拍的重读不采用", from: T, old: "pause",
			run: func(t *testing.T, r *replay) {
				st, ok := stateAtMs(r, T+300)
				r.claimRereads[T] = replayReread{fetchMs: 300, name: "", ms: 0, state: st, stateOK: ok}
				fillRun(r, T, 300, T+1500, segs(playTo(t, "A", 80000, T), seg{T, "B", 0, false}))
			},
			st:   flags(0, 3, T+400, 4),
			want: []pr{{"pause", T + 424, 0}}},
		// P 在播（读数自 1931 起随时间走，T 首读 5031），T 之后停住；补取取词 150 ms，重读 P/5031、标志 4。
		{name: "P13 补取拍的重读带回标志", from: T, old: "pause",
			run: func(t *testing.T, r *replay) {
				st, ok := stateAtMs(r, T+150)
				r.fills = []replayFill{{song: "P", from: T, ok: true, lines: coastLyrics(), fetchMs: 150, rereadName: "P",
					rereadMs: 5031, rereadState: st, rereadStateOK: ok}}
				fillRun(r, T, 150, T+1500, segs(seg{0, "P", 1931, true}, seg{T + 1, "P", 5031, false}))
			},
			st:   flags(0, 3, T+1, 4),
			want: []pr{{"pause", T, 5.031}}},
		{name: "P13b 补取拍的重读不采用", from: T, old: "pause",
			run: func(t *testing.T, r *replay) {
				st, ok := stateAtMs(r, T+150)
				r.fills = []replayFill{{song: "P", from: T, ok: true, lines: coastLyrics(), fetchMs: 150, rereadName: "",
					rereadMs: 5031, rereadState: st, rereadStateOK: ok}}
				fillRun(r, T, 150, T+1500, segs(seg{0, "P", 1931, true}, seg{T + 1, "P", 5031, false}))
			},
			st:   flags(0, 3, T+1, 4),
			want: []pr{{"pause", T + 181, 5.031}}},
		{name: "P14 #202 暂扣中按暂停", from: T, old: "resume pause",
			run:  runTo(T+2000, pt("A", 188000, T), sg(T, "A", 0, false)),
			st:   flags(0, 3, T+200, 4),
			want: []pr{{"resume", T + 403, 0}, {"pause", T + 403, 0}}},
		// 第 4 轮（设计 §6.1）：22.82 的 2 是载入，按停走判，停走自进入 2 的那一拍起量。
		// QQ 启动后载入上次的歌而未开播（§1.3）：一直为 2，自第一拍起停走超过 1 s 判暂停（同修复之前）；开播变 3 那一拍判
		// 恢复，其后的起步停顿不判。2 按在播（第 3 轮的取值）的话，3 s 时触发甲，其后按停走判。
		{name: "P15 QQ 启动后未开播", from: 0, old: "pause resume",
			run:  runTo(T+3000, sg(0, "A", 0, false), sg(T+800, "A", 719, true)),
			st:   flags(0, 2, T, 3),
			want: []pr{{"pause", 1023, 0}, {"resume", T, 0}}},
		// 记录行为（未见过，#224）：换歌那一拍起 2 持续 1.5 s 而读数不动，自认领那一拍起停走超过 1 s 判暂停，变 3 判恢复。
		{name: "P16 换歌载入 2 持续超过 1 s", from: T, old: "pause resume",
			run:  runTo(T+3500, pt("A", 80000, T), sg(T, "B", 0, false), sg(T+2400, "B", 1000, true)),
			st:   flags(0, 3, T, 2, T+1500, 3),
			want: []pr{{"pause", T + 1023, 0}, {"resume", ft(T + 1500), 0}}},
		// 设计审第 4 轮 F1 的反例：A 自认领起读数一直为 0，占位串那一拍 0→0 不暂扣、不认领、不重锚；载入自锚点量停走的话，
		// 1209 那一拍即判暂停。
		{name: "P17 起步停顿中再换歌、占位串拍为 2", from: 0, old: "pause pause resume",
			run: runTo(3500, sg(0, "A", 0, false), sg(1200, "", 0, false), sg(1400, "B", 0, false), sg(2800, "B", 1300, true)),
			st:  flags(0, 3, 1200, 2, 1400, 3)},
		// 第 5 轮：暂停中拖动的三种先后（设计 §1.3「其余已收录版本与 22.26」）。读数先跳、标志晚一拍（22.60 的 203.5 s）：跳转
		// 那一拍在落点补发 pause，标志变在播那一拍判恢复（落点）；第 4 轮为 pause、resume、pause、resume，后三条在 0.2 s 内。
		{name: "P18 暂停中拖动、读数先跳、标志晚一拍（22.60 的 203.5 s）", ver: "22.60", from: T, old: "pause resume pause resume",
			run:  runTo(T+6000, p18...),
			st:   flags(0, 3, T+100, 4, T+3200, 3),
			want: []pr{{"pause", ft(T + 100), 87.334}, {"pause", ft(T + 3000), 251.215}, {"resume", ft(T + 3200), 251.215}}},
		// A 类（暂停 0、在播 1）、标志晚两拍（22.26 的 199.1 s）。
		{name: "P19 暂停中拖动、读数先跳、标志晚两拍（22.26 的 199.1 s）", ver: "22.26", from: T, old: "pause resume pause resume",
			run:  runTo(T+6000, p18...),
			st:   flags(0, 1, T+100, 0, T+3400, 1),
			want: []pr{{"pause", ft(T + 100), 87.334}, {"pause", ft(T + 3000), 251.215}, {"resume", ft(T + 3400), 251.215}}},
		// 记录限制，不是期望正确（§6.7 第 7 条）：标志先于读数变在播（22.16 的 204.9 s），标志变的那一拍按原位置判恢复，跳转
		// 那一拍未判暂停、照旧当拍发 resume（落点）。
		{name: "P20 暂停中拖动、标志先变（22.16 的 204.9 s）", ver: "22.16", from: T, old: "pause resume",
			run: runTo(T+5000, pt("A", 50000, T), sg(T, "A", 50000, false), sg(T+3000, "A", 120000, false),
				sg(T+3800, "A", 120800, true)),
			st:   flags(0, 1, T+100, 0, T+2800, 1),
			want: []pr{{"pause", ft(T + 100), 50}, {"resume", ft(T + 2800), 50}, {"resume", ft(T + 3000), 120}}},
		// 反例：P18 的读数、标志未知（不设 stateAt），与第 4 轮、与修复之前逐条相同：停走 1 s 判暂停，跳转那一拍判恢复，
		// 其后停走 1 s 再判暂停，读数起步判恢复。
		{name: "P21 P18 的读数、标志未知", ver: "22.60", from: T, old: "pause resume pause resume",
			run: runTo(T+6000, p18...),
			want: []pr{{"pause", T + 992, 87.334}, {"resume", ft(T + 3000), 251.215}, {"pause", T + 4030, 251.215},
				{"resume", ft(T + 4400), 252.469 + float32(ft(T+4400)-T-4400)/1000}}},
		// #243：换到在线歌曲，标志停在开播值 3、读数在 0 停 1.3 s 后起步并一直走（22.26 的 282.1 s，rc.14 验收录像）：不判
		// 暂停与恢复。反例 P22b：同样的读数、标志为 2，停走 1 s 判暂停、起步判恢复，同修复之前。
		{name: "P22 换歌后停在开播值、起步缓冲 1.3 s（22.26 的 282.1 s）", ver: "22.26", from: T, old: "pause resume",
			run: runTo(T+4000, pt("A", 85737, T), sg(T, "B", 0, false), sg(T+1300, "B", 0, true)),
			st:  flags(0, 1, T, 3)},
		{name: "P22b P22 的读数、标志为 2", ver: "22.26", from: T, old: "pause resume",
			run:  runTo(T+4000, pt("A", 85737, T), sg(T, "B", 0, false), sg(T+1300, "B", 0, true)),
			st:   flags(0, 1, T, 2),
			want: []pr{{"pause", ft(T + 1001), 0}, {"resume", ft(T+1300) + tick, float32(ft(T+1300)+tick-T-1300) / 1000}}},

		// ---- V：20.05 的 0、1 与去抖
		{name: "V1 普通暂停（21.7 s）", ver: "20.05", from: T, old: "pause resume",
			run:  runTo(T+9000, pt("A", 35294, T), sg(T, "A", 35294, false), sg(T+7400, "A", 36594, true)),
			st:   flags(0, 0, T+200, 1, T+6200, 0),
			want: []pr{{"pause", T + 527, 35.294}, {"resume", ft(T + 6200), 35.294}}},
		{name: "V2 换歌那一拍闪 1（200 ms）、当拍认领", ver: "20.05", from: T, old: "pause resume",
			run:     runTo(T+3000, pt("A", 49040, T), sg(T, "B", 0, false), sg(T+1400, "B", 1300, true)),
			st:      flags(0, 0, T, 1, T+200, 0),
			flashes: [][2]int{{T + 217, 248}}},
		// 报出的是上界（自暂停值之前的上一拍 T−31 起算）：341 ms 大于去抖时长 300 ms，而这段暂停值按拍只读到 279 ms，
		// 未判暂停。V2 的三例、V7、X14 的 flash 都自上一拍起算。
		{name: "V2 换歌那一拍闪 1（300 ms）、当拍认领", ver: "20.05", from: T, old: "pause resume",
			run:     runTo(T+3000, pt("A", 49040, T), sg(T, "B", 0, false), sg(T+1400, "B", 1300, true)),
			st:      flags(0, 0, T, 1, T+300, 0),
			flashes: [][2]int{{T + 310, 341}}},
		// §6.1 的风险，记录限制，不是期望正确：T+310 仍读到 1、满 300 ms。
		{name: "V2 换歌那一拍闪 1（320 ms）、当拍认领", ver: "20.05", from: T, old: "pause resume",
			run:  runTo(T+3000, pt("A", 49040, T), sg(T, "B", 0, false), sg(T+1400, "B", 1300, true)),
			st:   flags(0, 0, T, 1, T+320, 0),
			want: []pr{{"pause", T + 310, 0}, {"resume", T + 341, 0}}},
		{name: "V3 闪烁落在 #96 等待里（51.7 s）", ver: "20.05", from: T, old: "",
			run: runTo(T+3500, pt("A", 49040, T), sg(T, "B", 49040, false), sg(T+1800, "B", 1579, true)),
			st:  flags(0, 0, T, 1, T+200, 0)},
		// 等满 2 s 按上一首读数认领（T+2015）；等待中不调判定函数，去抖自认领那一拍起算；读数换过来时按回跳再发一条
		// （§6.5 所述多出的一条带上一首位置的 resume）。
		{name: "V4 换歌后立刻暂停、等满按上一首读数认领（74.7 s）", ver: "20.05", from: T, old: "pause resume",
			run: runTo(T+8000, pt("A", 10658, T), sg(T, "B", 10658, false), sg(T+6600, "B", 1230, true)),
			st:  flags(0, 0, T+400, 1, T+5400, 0),
			want: []pr{{"pause", T + 2325, 10.658}, {"resume", ft(T + 5400), 10.658},
				{"resume", ft(T + 6600), 1.230 + float32(ft(T+6600)-T-6600)/1000}}},
		{name: "V5 拖动松手前读数停 1.4 s（109.1 s）", ver: "20.05", from: T, old: "pause resume",
			run:  runTo(T+3000, pt("A", 10518, T), sg(T, "A", 10518, false), sg(T+1400, "A", 94941, true)),
			st:   flags(0, 0),
			want: []pr{{"resume", ft(T + 1400), 94.941 + float32(ft(T+1400)-T-1400)/1000}}},
		{name: "V7 去抖跨拍累计", ver: "20.05", from: T, old: "pause",
			run:     runTo(T+2500, pt("A", 50000, T), sg(T, "A", 50000, true), sg(T+1000, "A", 51000, false)),
			st:      flags(0, 0, T, 1, T+250, 0, T+1000, 1),
			want:    []pr{{"pause", T + 1333, 51}},
			flashes: [][2]int{{T + 279, 310}}},

		// ---- X：交叉核对
		{name: "X1 甲触发：垃圾恒 3，真暂停", from: T, tripAt: T + 2976, old: "-",
			run: runTo(T+9500, x1...),
			st:  flags(0, 3),
			want: []pr{{"pause", T + 2976, 80}, {"resume", ft(T + 5000), 81.2 + float32(ft(T+5000)-T-5000)/1000},
				{"pause", T + 9052, 84.2}}},
		// 停走 2.9 s 不触发甲；T+5000 起暂停（标志 4、读数停住），仍按标志判。设计 §7.3 的输入没写读数在 T+5000 停住，
		// 照原样读数一直走、标志为 4，正是乙要拦的「报暂停却在走」（实测 T+5084 判不可信），故补上停住的一段。
		{name: "X3 甲不触发", from: T, old: "-",
			run:  runTo(T+5500, pt("A", 80000, T), sg(T, "A", 80000, false), sg(T+2900, "A", 82800, true), sg(T+5000, "A", 84900, false)),
			st:   flags(0, 3, T+5000, 4),
			want: []pr{{"pause", ft(T + 5000), 84.9}}},
		{name: "X4 甲与补写", from: T, tripAt: T + 4929, old: "-",
			run:  runTo(T+5500, pt("A", 80000, T), sg(T, "A", 80000, false), sg(T+1905, "A", 80116, false)),
			st:   flags(0, 3),
			want: []pr{{"pause", T + 4929, 80.116}}},
		{name: "X5 乙触发：垃圾恒 4，QQ 在播", from: 0, tripAt: 620, old: "-",
			run:  runTo(3000, x5...),
			st:   flags(0, 4),
			want: []pr{{"pause", 0, 50}, {"resume", 930, 50.9}}},
		{name: "X6 乙不触发：翻转后多一步，1.6 s 后补写", from: T, old: "-",
			run:  runTo(T+6500, pt("A", 80000, T), sg(T, "A", 80000, false), sg(T+62, "A", 80325, false), sg(T+1662, "A", 80441, false)),
			st:   flags(0, 3, T+31, 4, T+6000, 3),
			want: []pr{{"pause", T + 31, 80}, {"resume", ft(T + 6000), 80.441}}},
		// 第 5 轮：两次暂停中跳转各在落点补发一条 pause（第 4 轮为每次跳转各一对 resume、pause）。
		{name: "X8 seek 不计入乙", from: T, old: "-",
			run:  runTo(T+4500, append(slices.Clone(p8), sg(T+3500, "A", 30000, false))...),
			st:   flags(0, 3, T+100, 4),
			want: []pr{{"pause", ft(T + 100), 50}, {"pause", ft(T + 3000), 120}, {"pause", ft(T + 3500), 30}}},
		// X8 的两次 seek 之间有判暂停清 movedAt（第 5 轮为暂停中跳转清 movedAt），乙计入 seek 时也拦不住（实测）；这一例在 seek
		// 之前 0.5 s 补写一次。第 5 轮跳转那一拍只补发 pause（第 4 轮为 resume、pause）。
		{name: "X8b 补写之后 0.5 s 内暂停中拖动：seek 不计入乙", from: T, old: "-",
			run:  runTo(T+4500, pt("A", 50000, T), sg(T, "A", 50000, false), sg(T+2500, "A", 50116, false), sg(T+3000, "A", 120000, false)),
			st:   flags(0, 3, T+100, 4),
			want: []pr{{"pause", ft(T + 100), 50}, {"pause", ft(T + 3000), 120}}},
		{name: "X9 未认领不核对甲", from: 0, old: "-",
			run: runTo(7000, sg(0, "", 0, false), sg(5000, "A", 0, false), sg(6400, "A", 1200, true)),
			st:  flags(0, 3)},
		{name: "X10 上一段暂停的变化不计入", from: T, old: "-",
			run:  runTo(T+3500, pt("A", 80000, T), sg(T, "A", 80000, false), sg(T+2000, "A", 80116, false), sg(T+2800, "A", 80441, false)),
			st:   flags(0, 3, T+31, 4, T+2100, 3, T+2400, 4),
			want: []pr{{"pause", T + 31, 80}, {"resume", ft(T + 2100), 80.116}, {"pause", ft(T + 2400), 80.116}}},
		// X1 之后接 #172 录音的换歌形状：判不可信之后同修复之前（停走 1 s 判暂停、首步判恢复）。
		{name: "X11 判不可信之后同修复之前", from: T + 12000, tripAt: T + 2976, old: "pause resume",
			run: runTo(T+16000, append(slices.Clone(x1), sg(T+12000, "A", 0, false), sg(T+12061, "B", 0, false),
				sg(T+14018, "B", 1671, true))...),
			st:   flags(0, 3),
			want: []pr{{"pause", T + 13113, 0}, {"resume", ft(T + 14018), 1.671 + float32(ft(T+14018)-T-14018)/1000}}},
		// 记录限制，不是期望正确（§6.3、§10）：标志卡在在播值而每次停走都不超过 3 s，三次暂停都漏判且不触发。
		{name: "X12 恒为在播 + 反复短暂停", from: T, old: "pause resume pause resume pause resume",
			run: runTo(T+23000, pt("A", 80000, T), sg(T, "A", 80000, false), sg(T+1600, "A", 81021, true),
				sg(T+10000, "A", 89421, false), sg(T+11600, "A", 90442, true), sg(T+20000, "A", 98842, false),
				sg(T+21600, "A", 99863, true)),
			st: flags(0, 3)},
		{name: "X13 去抖与乙：垃圾恒为 1（20.05），QQ 在播", ver: "20.05", from: 0, tripAt: 930, old: "-",
			run:  runTo(3000, x5...),
			st:   flags(0, 1),
			want: []pr{{"pause", 310, 50.3}, {"resume", ft(1200), 51.209}}},
		{name: "X14 去抖与停走：20.05 起步停顿中闪 1", ver: "20.05", from: T, old: "pause resume",
			run:     runTo(T+3000, pt("A", 80000, T), sg(T, "B", 0, false), sg(T+1400, "B", 1300, true)),
			st:      flags(0, 0, T+1100, 1, T+1300, 0),
			flashes: [][2]int{{T + 1302, 217}}},
	}
}

// TestReplayPlayStateCases 跑 stateCases 的各例：自 from 起的暂停与恢复（拍与位置）、判不可信的拍、报出的未满去抖
// 的暂停值；并以同一读数、不设 stateAt 核对修复之前的形状（old）。
//
// 变异自证（均实测，下列为各条红的用例中属于本表的部分）：
//   - 判定函数层：在播时仍按停走判暂停，P1、P4、P5、P8b、P10b、P15～P17 等红；暂停时仍经 resumeGate，P2、P9 红；22.82
//     的条目把 4 判为在播或恒为未知，P2、P3、P6～P14 红（恒为未知另有 P15～P17）；22.82 的条目把 2 判为在播（第 3 轮的
//     取值），P15、P16 红；载入直接按未知、停走自锚点量，P17 红（1209 判暂停）；classify 写死 22.82 的取值，V1、V2、
//     X13 红；去抖不生效，V2（闪 200、300）、X14 红；待定按未知处理，X14 红；恢复也去抖，V1 红；甲门槛改 2 s，X3 红；
//     甲门槛改 4 s，X1、X4、X11 红（含等号只有 X2 红，见 TestReplayPlayStateStallBoundary）；甲不要求已认领，X9 红；
//     甲在本拍由标志判恢复时也量，P3 红；乙窗口取 2 × 轮询，X5 红；乙计入 seek，X8b 红（X8 不红：两次 seek 之间已清
//     movedAt，第 4 轮由判暂停、第 5 轮由暂停中跳转的分支清）；判暂停时不清 movedAt，X10 红；乙改用 resumeGate 的判据
//     （单步不小于 500 ms 即触发），P2、P9 红；触发之后不改按未知，X1、X11 红。
//   - 复述里：不写回跨拍状态（每拍以零值调用 judgeTick），V1、V7、X5、X13 红；判恢复不重锚 anchorTime，P2、P3、P7、
//     P9、V1、V4、X6 红（暂停期间的停走算进锚点，恢复后下一拍即触发甲、多出一条 pause）；认领处的重读不换标志，P12 红；
//     补取处的重读不换标志，P13 红；补发不看 isPaused，P13、P13b 红（播放中补取多出一条 pause）。
//   - 暂停中跳转（第 5 轮）：去掉这一分支，P8、P18、P19、X8、X8b 红；分支不看状态，P8b、P21 的期望红，P8、P18～P20、V4、
//     V5 的 old 一栏红（标志未知时暂停中 seek 也不判恢复），pausetail_test.go 与 switchzero_test.go 里暂停中回跳的用例同红；
//     去掉 in.seek，P 组除 P1、P4、P5、P15～P17、P21 之外各例与 V1、V4、V7、X3、X5、X6、X8、X8b、X10、X13 红；同时置
//     v.resume 或 v.pause，P8、P18、P19、X8、X8b 红；以读成功代替状态为暂停，P8b 红；乙计入 seek，仍是 X8b 红；判暂停时
//     不清 movedAt，X10 红（重测）。复述里变化分支照旧在暂停中发 resume、或漏掉 pausedSeek 的补发，P8、P18、P19、X8、X8b
//     红；判恢复仍按 !seek 决定是否发（暂停中跳转而判恢复时漏发 resume），P8b、P21 的期望红，P8、P18～P20、V4、V5 的 old
//     一栏红。
func TestReplayPlayStateCases(t *testing.T) {
	for _, c := range stateCases() {
		t.Run(c.name, func(t *testing.T) {
			r := runStateCase(t, c, c.st)
			expectPR(t, r, c.from, c.want...)
			wantBad := -1
			if c.tripAt != 0 {
				wantBad = c.tripAt
			}
			if r.badAt != wantBad {
				t.Errorf("判不可信的拍 %d，期望 %d", r.badAt, wantBad)
			}
			if !reflect.DeepEqual(r.flashes, c.flashes) {
				t.Errorf("未满去抖的暂停值 %v，期望 %v", r.flashes, c.flashes)
			}
			if c.old != "-" {
				r0 := runStateCase(t, c, nil)
				if got := shapeOf(prFrom(r0, c.from)); got != c.old {
					t.Errorf("不设 stateAt（修复之前）时 %dms 起的暂停与恢复 %q，期望 %q；明细：%v", c.from, got, c.old, prFrom(r0, c.from))
				}
			}
		})
	}
}

// TestReplayPlayStateResumeReanchors：P3 由标志判恢复时重锚 anchorTime（§6.2）。读数自 T+7000 起走，第 4 行（19.2 s，
// 匹配时刻含 offset 0.4 s）在 T+9036 之后的第一拍；不重锚的话插值自 T+2821 的补写起算，约 T+6020 就推到第 4 行。
//
// 变异自证（实测）：复述里判恢复不重锚 anchorTime，本例不红：恢复之后的下一拍停走自补写起已超过 3 s，先触发甲、判
// 暂停，第 4 行仍在读数起步之后；这条变异由 TestReplayPlayStateCases 的 P3 等例抓（多出一条 pause）。本例钉住的是设计
// §7.3 P3 写明的行号时刻。
func TestReplayPlayStateResumeReanchors(t *testing.T) {
	var p3 stateCase
	for _, c := range stateCases() {
		if strings.HasPrefix(c.name, "P3 ") {
			p3 = c
		}
	}
	r := runStateCase(t, p3, p3.st)
	for _, e := range r.evs {
		if e.typ == "lyric_update" && e.idx == 4 {
			if e.ms != firstTick(sT+9036) {
				t.Fatalf("第 4 行在 %dms，期望 %dms；明细：%v", e.ms, firstTick(sT+9036), r.evs)
			}
			return
		}
	}
	t.Fatalf("没有第 4 行；明细：%v", r.evs)
}

// TestReplayPlayStateMovedBoundary 是 X7：已判暂停（标志恒 4），两次非 seek 的变化相隔 1000 ms 时第二次那一拍判
// 不可信，1001 ms 不判。
//
// 变异自证（均实测）：乙窗口比较改严格小于，1000 一例红；窗口取 2 × 轮询，1000 一例红。
func TestReplayPlayStateMovedBoundary(t *testing.T) {
	for _, c := range []struct {
		gap int
		bad int
	}{{1000, 2000}, {1001, -1}} {
		t.Run(fmt.Sprintf("相隔 %dms", c.gap), func(t *testing.T) {
			r := newStateReplay("22.82", flags(0, 4))
			r.run(0, 500, tick, segs(seg{0, "A", 50000, false}))
			r.tick(1000, "A", 50100)
			r.tick(1000+c.gap, "A", 50200)
			if r.badAt != c.bad {
				t.Fatalf("判不可信的拍 %d，期望 %d", r.badAt, c.bad)
			}
		})
	}
}

// TestReplayPlayStateStallBoundary 是 X2：拍长 50 ms、T = 5000，标志恒 3，最后一次变化在 T−50。停走恰 3000 ms 的拍
// 不判；单独一拍于 +3001 判暂停（位置为锚点）并判不可信。
//
// 变异自证（均实测）：甲门槛比较含等号，本例红（+3000 那一拍即判）；甲门槛改 4 s，红（+3001 不判）。
func TestReplayPlayStateStallBoundary(t *testing.T) {
	const T = 5000
	r := newStateReplay("22.82", flags(0, 3))
	r.poll = 50 * time.Millisecond
	at := segs(seg{0, "A", 80000 - (T - 50), true}, seg{T, "A", 80000, false})
	r.run(0, T+2950, 50, at)
	if len(prFrom(r, 0)) != 0 || r.badAt != -1 {
		t.Fatalf("停走恰 3000ms：事件 %v、判不可信的拍 %d，期望都没有", prFrom(r, 0), r.badAt)
	}
	r.tick(T+2951, "A", 80000)
	expectPR(t, r, 0, pr{"pause", T + 2951, 80})
	if r.badAt != T+2951 {
		t.Fatalf("判不可信的拍 %d，期望 %d", r.badAt, T+2951)
	}
}

func newStateReplay(ver string, st func(int) (uint32, bool)) *replay {
	r := newReplay(-0.4, zeroSongs())
	r.spec, r.stateAt = knownVersions[ver].PlayState, st
	return r
}

// ---------------------------------------------------------------- 回放：暂停中补发歌词（§7.3 L 组）

// TestReplayPlayStateRepauseAfterLyrics：已判暂停时补取发出 all_lyrics（有歌词与无歌词两条路径），紧接着、在取封面
// 之前补发一条同位置的 playback_pause（§6.2）。歌 P（待补取）在 T 认领，读数 0 停住；补取登记在 T+500，取词 100 ms，
// 重读 P/0。
//
// 变异自证（均实测，均为复述里）：不补发，L1～L4、L6、L7 红；补发不看 isPaused，P13（TestReplayPlayStateCases）与
// switchzero_test.go 里播放中补取的各例红；补发排在取封面之后，L7 红。runSession 里的补发由文件末的源码扫描（g）守。
func TestReplayPlayStateRepauseAfterLyrics(t *testing.T) {
	const T = sT
	at := segs(playTo(t, "A", 80000, T), seg{T, "P", 0, false})
	fill := func(lines []lyricLine, ok bool, state uint32, stateOK bool, from, coverMs int) replayFill {
		return replayFill{song: "P", from: from, ok: ok, lines: lines, fetchMs: 100, rereadName: "P", rereadMs: 0,
			rereadState: state, rereadStateOK: stateOK, coverMs: coverMs}
	}
	fillAt := firstTick(T + 500)

	t.Run("L1 补取到歌词时已按标志暂停", func(t *testing.T) {
		r := newStateReplay("22.82", flags(0, 3, T+100, 4))
		r.fills = []replayFill{fill(coastLyrics(), true, 4, true, T+500, 0)}
		fillRun(r, fillAt, 100, T+3500, at)
		evs := expectFrom(t, r, T, "all pause all pause lu0")
		expectAt(t, evs[1], "playback_pause", "P", firstTick(T+100), 0)
		expectAt(t, evs[2], "all_lyrics", "P", fillAt, 0)
		expectAt(t, evs[3], "playback_pause", "P", fillAt, 0)
	})
	t.Run("L2 无歌词路径", func(t *testing.T) {
		r := newStateReplay("22.82", flags(0, 3, T+100, 4))
		r.fills = []replayFill{fill([]lyricLine{}, true, 4, true, T+500, 0)}
		fillRun(r, fillAt, 100, T+3500, at)
		evs := expectFrom(t, r, T, "all pause all lu-1 pause")
		expectAt(t, evs[4], "playback_pause", "P", fillAt, 0)
	})
	t.Run("L3 补取那一拍标志变 3：补发之后照常判恢复", func(t *testing.T) {
		r := newStateReplay("22.82", flags(0, 3, T+100, 4, T+500, 3))
		r.fills = []replayFill{fill(coastLyrics(), true, 3, true, T+500, 0)}
		fillRun(r, fillAt, 100, T+3500, at)
		evs := expectFrom(t, r, T, "all pause all pause resume lu0")
		expectAt(t, evs[4], "playback_resume", "P", fillAt, 0)
	})
	t.Run("L4 标志未知、停走判定已判暂停之后补取到歌词", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.fills = []replayFill{fill(coastLyrics(), true, 0, false, T+1500, 0)}
		fillRun(r, firstTick(T+1500), 100, T+3500, at)
		evs := expectFrom(t, r, T, "all pause all pause lu0")
		expectAt(t, evs[3], "playback_pause", "P", firstTick(T+1500), 0)
	})
	t.Run("L5 暂停中补取失败", func(t *testing.T) {
		r := newStateReplay("22.82", flags(0, 3, T+100, 4))
		r.fills = []replayFill{fill(coastLyrics(), false, 4, true, T+500, 0)}
		fillRun(r, fillAt, 100, T+3500, at)
		expectFrom(t, r, T, "all pause")
	})
	t.Run("L6 补发之后读数补写：无事件", func(t *testing.T) {
		r := newStateReplay("22.82", flags(0, 3, T+100, 4))
		r.fills = []replayFill{fill(coastLyrics(), true, 4, true, T+500, 0)}
		fillRun(r, fillAt, 100, T+3500, segs(playTo(t, "A", 80000, T), seg{T, "P", 0, false}, seg{T + 2900, "P", 116, false}))
		expectFrom(t, r, T, "all pause all pause lu0")
		if r.badAt != -1 {
			t.Errorf("判不可信的拍 %d，期望没有", r.badAt)
		}
	})
	t.Run("L7 补发早于取封面", func(t *testing.T) {
		r := newStateReplay("22.82", flags(0, 3, T+100, 4))
		r.fills = []replayFill{fill(coastLyrics(), true, 4, true, T+500, 5000)}
		fillRun(r, fillAt, 100+5000, T+8000, at)
		evs := expectFrom(t, r, T, "all pause all pause lu0")
		if evs[3].at != evs[2].at || evs[4].at != evs[2].at+5000 {
			t.Fatalf("补取的 all 在 %dms、补发的 pause 在 %dms、其后的行在 %dms：补发应与 all 同一墙钟、早于取封面的 5000ms",
				evs[2].at, evs[3].at, evs[4].at)
		}
	})
}

// ---------------------------------------------------------------- 回放：轮询间隔（§7.3 Q 组）

// TestReplayPlayStatePollInterval 是 Q 组：乙的窗口随轮询放大（max(1 s, 2 × 轮询)），轮询超过 1 s 不用标志。
//
// 变异自证（均实测）：乙窗口固定 1 s，Q2 红（Q1 的两次变化恰隔 1000 ms，固定窗口也拦得住）；不看轮询间隔（恒用标志），
// Q5 红；轮询门槛改严格小于 1 s，Q1、Q2、Q3 红（1000 ms 轮询不再用标志）；乙改为一次变化即触发，Q3 红。
func TestReplayPlayStatePollInterval(t *testing.T) {
	moving := segs(seg{0, "A", 50000, true})
	slow := func(st func(int) (uint32, bool), poll int) *replay {
		r := newStateReplay("22.82", st)
		r.poll = msd(poll)
		return r
	}
	t.Run("Q1 轮询 1000 ms，垃圾恒 4", func(t *testing.T) {
		r := slow(flags(0, 4), 1000)
		r.run(0, 6000, 1000, moving)
		expectPR(t, r, 0, pr{"pause", 0, 50}, pr{"resume", 2000, 52})
		if r.badAt != 2000 {
			t.Errorf("判不可信的拍 %d，期望 2000", r.badAt)
		}
	})
	t.Run("Q2 同 Q1，第二次变化的拍延迟", func(t *testing.T) {
		for _, c := range []struct{ late, bad int }{{2900, 2900}, {3100, 4100}} {
			r := slow(flags(0, 4), 1000)
			r.run(0, 1000, 1000, moving)
			r.run(c.late, c.late+3000, 1000, moving)
			if r.badAt != c.bad {
				t.Errorf("第二次变化在 %dms：判不可信的拍 %d，期望 %d", c.late, r.badAt, c.bad)
			}
		}
	})
	t.Run("Q3 轮询 1000 ms，真标志，真暂停一次补写", func(t *testing.T) {
		r := slow(flags(0, 3, 5000, 4, 10000, 3), 1000)
		r.run(0, 13000, 1000, segs(seg{0, "A", 50000, true}, seg{5000, "A", 55000, false}, seg{7400, "A", 55116, false},
			seg{11000, "A", 56200, true}))
		expectPR(t, r, 0, pr{"pause", 5000, 55}, pr{"resume", 10000, 55.116})
		if r.badAt != -1 {
			t.Errorf("判不可信的拍 %d，期望没有", r.badAt)
		}
	})
	t.Run("Q4 轮询 1000 ms，换歌起步停顿", func(t *testing.T) {
		const T = sT
		r := slow(flags(0, 3, T+200, 2, T+400, 3), 1000)
		r.run(0, T+4000, 1000, segs(playTo(t, "A", 25936, T), seg{T, "A", 0, false}, seg{T + 200, "B", 0, false}, seg{T + 1600, "B", 1277, true}))
		expectPR(t, r, 0)
	})
	t.Run("Q5 轮询 1500、2000 ms：同不设 stateAt", func(t *testing.T) {
		const T = sT
		inputs := []struct {
			name string
			to   int
			at   func(int) (string, uint32)
			st   func(int) (uint32, bool)
		}{
			{"P1", T + 4000, segs(playTo(t, "A", 25936, T), seg{T, "A", 0, false}, seg{T + 200, "B", 0, false},
				seg{T + 1600, "B", 1277, true}), flags(0, 3, T+200, 2, T+400, 3)},
			{"P3", T + 9500, segs(playTo(t, "A", 15557, T), seg{T, "A", 15557, false}, seg{T + 2800, "A", 15580, false},
				seg{T + 7000, "A", 16764, true}), flags(0, 3, T+200, 4, T+5800, 3)},
			{"X5", 3000, segs(seg{0, "A", 50000, false}, seg{300, "A", 50300, false}, seg{600, "A", 50600, false},
				seg{900, "A", 50900, false}, seg{1200, "A", 51200, true}), flags(0, 4)},
		}
		for _, in := range inputs {
			for _, step := range []int{1500, 2000} {
				var evs [2][]replayEvent
				for i, st := range []func(int) (uint32, bool){in.st, nil} {
					r := newStateReplay("22.82", st)
					r.poll = msd(step)
					r.run(0, in.to, step, in.at)
					evs[i] = r.evs
				}
				if !slices.Equal(evs[0], evs[1]) {
					t.Errorf("%s、拍长 %d：设 stateAt\n  %v\n不设\n  %v", in.name, step, evs[0], evs[1])
				}
			}
		}
	})
	t.Run("Q6 默认轮询，垃圾恒 4，一拍阻塞", func(t *testing.T) {
		at := segs(seg{0, "A", 50000, false}, seg{300, "A", 50300, false}, seg{600, "A", 50600, false},
			seg{900, "A", 50900, false}, seg{1200, "A", 51200, true})
		r := newStateReplay("22.82", flags(0, 4))
		r.run(0, 310, tick, at)
		r.tick(1600, "A", 50600)
		if r.badAt != -1 {
			t.Fatalf("1600 那一拍距上一次变化 1290ms，不应判不可信")
		}
		r.run(1631, 3000, tick, at)
		if r.badAt != 1631 {
			t.Fatalf("判不可信的拍 %d，期望 1631", r.badAt)
		}
	})
}

// ---------------------------------------------------------------- 回放：标志未知与修复之前逐事件相同（§7.3 U 组）

// TestReplayPlayStateUnknownSameAsBefore 是 U2（含 V6）：P 组的读数配读失败与该例条目里不是暂停、在播的常值标志（22.82 与
// E 类为 0、1、2、5、6，A 类为 2、4～6；载入值自第一拍起一直不变时停走即自锚点量；A 类的 3 是开播值，不按停走判暂停，
// 不在此列，#243），V 组的读数配读失败与常值 2、7（20.05
// 的条目），事件序列（含拍与位置）与不设 stateAt 时逐条相同。第 5 轮的 P8、P18 在其中：标志不可用时暂停中跳转照旧判恢复。
//
// 变异自证（均实测）：不看 rawOK，V 组配读失败时红（20.05 读失败当作在播值 0）；22.82 的条目把 5 判为暂停，P 组配 5
// 红；22.82 的条目把 2 判为在播（第 3 轮的取值），P 组配 2 各例红。第 5 轮：暂停中跳转的分支以读成功代替状态为暂停，本例
// 红；分支不看状态，本例不红（两边同样不判恢复，比的基准也变了），由 P21 与各例的 old 一栏抓。
func TestReplayPlayStateUnknownSameAsBefore(t *testing.T) {
	for _, c := range stateCases() {
		var consts []int
		switch {
		case strings.HasPrefix(c.name, "P"):
			spec := knownVersions[cmp.Or(c.ver, "22.82")].PlayState
			consts = []int{failed}
			for k := range 7 {
				if st := spec.classify(uint32(k)); (st == stateUnknown || st == stateLoading) && !slices.Contains(spec.Opening, uint32(k)) {
					consts = append(consts, k)
				}
			}
		case strings.HasPrefix(c.name, "V"):
			consts = []int{failed, 2, 7}
		default:
			continue
		}
		r0 := runStateCase(t, c, nil)
		for _, k := range consts {
			if r := runStateCase(t, c, flags(0, k)); !slices.Equal(r.evs, r0.evs) {
				t.Errorf("%s 配常值 %d：\n  得到 %v\n  不设 stateAt %v", c.name, k, r.evs, r0.evs)
			}
		}
	}
}

// TestReplayPlayStateRecordedSwitch 是 U3：#172 录音的换歌形状（c0e7784 版 R1）。不设 stateAt 时为 pause(0)、resume，
// 钉住无偏移版本的现行行为；配 [0:3, T+61:2, T+261:3] 时为无。
func TestReplayPlayStateRecordedSwitch(t *testing.T) {
	const T = sT
	at := segs(playTo(t, "A", 80000, T), seg{T, "A", 0, false}, seg{T + 61, "B", 0, false}, seg{T + 2018, "B", 1671, true})
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T+3000, tick, at)
	expectPR(t, r, T, pr{"pause", T + 1085, 0}, pr{"resume", firstTick(T + 2018), 1.671 + float32(firstTick(T+2018)-T-2018)/1000})
	r = newStateReplay("22.82", flags(0, 3, T+61, 2, T+261, 3))
	r.run(0, T+3000, tick, at)
	expectPR(t, r, T)
}

// TestReplayPlayStatePauseTail 是 U4：#171 的 tailPause1 暂停段配 4、其余配 3：补写不判恢复，暂停与恢复在标志翻转的
// 拍；不设 stateAt 时同 pausetail_test.go。
//
// 变异自证（均实测）：暂停时仍经 resumeGate，本例照绿（补写 +116 不足 500 ms，resumeGate 也只记待确认）；乙改为
// 一次变化即触发，本例红（补写那一拍判不可信，恢复推到读数起步）。
func TestReplayPlayStatePauseTail(t *testing.T) {
	r := newReplay(0, tailSongs)
	r.spec, r.stateAt = knownVersions["22.82"].PlayState, flags(0, 3, 1500, 4, 5500, 3)
	r.run(0, 8000, 30, sampled("Y", tailPause1...))
	r.expectShape(t, "all lu0 pause resume")
	expectPR(t, r, 0, pr{"pause", 1500, 55.363}, pr{"resume", 5520, 55.479})
	r = newReplay(0, tailSongs)
	r.run(0, 8000, 30, sampled("Y", tailPause1...))
	expectPR(t, r, 0, pr{"pause", 2460, 55.363}, pr{"resume", 6390, 56.616})
}

// ---------------------------------------------------------------- 接线门禁（§7.4）

// playStateWiringProblems 按设计 §7.4 检查 src（qqmusic.go 的源码）里 runSession 的接线形状，返回发现的问题，每条以
// 对应的条目字母开头（「a: …」）。runSession 要真进程、没有离线夹具；判定本身由回放与 judgeTick 的用例守，这里只查
// 接线。找不到 runSession 或任一锚点时自报失效。
func playStateWiringProblems(src string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "qqmusic.go", src, parser.ParseComments)
	if err != nil {
		return []string{"解析失败: " + err.Error()}
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == "runSession" {
			fn = d
		}
	}
	if fn == nil {
		return []string{"没找到 runSession —— 门禁失效"}
	}
	var loop *ast.ForStmt
	for _, s := range fn.Body.List {
		if l, ok := s.(*ast.ForStmt); ok && l.Cond == nil {
			loop = l
		}
	}
	if loop == nil {
		return []string{"没找到 runSession 的轮询循环 —— 门禁失效"}
	}

	callName := func(c *ast.CallExpr) string {
		switch fun := c.Fun.(type) {
		case *ast.Ident:
			return fun.Name
		case *ast.SelectorExpr:
			return fun.Sel.Name
		}
		return ""
	}
	callsIn := func(n ast.Node, match func(*ast.CallExpr) bool) []*ast.CallExpr {
		var out []*ast.CallExpr
		ast.Inspect(n, func(x ast.Node) bool {
			if c, ok := x.(*ast.CallExpr); ok && match(c) {
				out = append(out, c)
			}
			return true
		})
		return out
	}
	funIs := func(s string) func(*ast.CallExpr) bool {
		return func(c *ast.CallExpr) bool { return types.ExprString(c.Fun) == s }
	}
	within := func(n ast.Node, b *ast.BlockStmt) bool { return b != nil && n.Pos() >= b.Pos() && n.End() <= b.End() }
	// isPauseEmit 认直接发 playback_pause 的调用（Emit、TryEmit 等，首个实参为该事件）；暂停中补发经 p.emitFrozenPause，另认。
	isPauseEmit := func(c *ast.CallExpr) bool {
		return len(c.Args) > 0 && types.ExprString(c.Args[0]) == "player.EventPlaybackPause"
	}
	isFrozenPause := funIs("p.emitFrozenPause")

	// 锚点
	var switchIf, fillIf, changeIf, pauseIf, resumeIf, trippedIf, flashIf *ast.IfStmt
	for _, s := range loop.Body.List {
		ifs, ok := s.(*ast.IfStmt)
		if !ok {
			continue
		}
		cond := types.ExprString(ifs.Cond)
		switch {
		case switchIf == nil && len(callsIn(ifs, funIs("progressCarriedOver"))) > 0:
			switchIf = ifs
		case fillIf == nil && strings.Contains(cond, "lyricsPending"):
			fillIf = ifs
		case cond == "meta.ProgressMs != lastMemProgress":
			changeIf = ifs
		case cond == "v.pause":
			pauseIf = ifs
		case cond == "v.resume":
			resumeIf = ifs
		case cond == `v.tripped != ""`:
			trippedIf = ifs
		case cond == "v.flash > 0":
			flashIf = ifs
		}
	}
	interp := token.NoPos
	for _, cg := range f.Comments {
		if cg.Pos() > loop.Pos() && cg.End() < loop.End() && strings.Contains(cg.Text(), "--- 本地时钟插值 ---") {
			interp = cg.Pos()
		}
	}
	var probs []string
	for name, missing := range map[string]bool{"换歌分支": switchIf == nil, "补取块": fillIf == nil, "变化分支": changeIf == nil,
		"if v.pause": pauseIf == nil, "if v.resume": resumeIf == nil, `if v.tripped != ""`: trippedIf == nil,
		"if v.flash > 0": flashIf == nil, "// --- 本地时钟插值 ---": interp == token.NoPos} {
		if missing {
			probs = append(probs, "没找到"+name+" —— 门禁失效")
		}
	}
	if len(probs) > 0 {
		return probs
	}

	// a：恰一处 judgeTick，位于变化分支之后、插值之前，入参依次取自规定的表达式。
	jcalls := callsIn(fn, funIs("judgeTick"))
	var jcall *ast.CallExpr
	if len(jcalls) != 1 {
		probs = append(probs, fmt.Sprintf("a: runSession 里应恰有一处 judgeTick 调用，实得 %d", len(jcalls)))
	} else {
		jcall = jcalls[0]
		if !(jcall.Pos() > changeIf.End() && jcall.End() < interp) {
			probs = append(probs, "a: judgeTick 调用应在变化分支之后、// --- 本地时钟插值 --- 之前")
		}
		wantArgs := []string{"spec: mem.Offsets().PlayState", "raw: meta.PlayState", "rawOK: meta.PlayStateOK",
			"poll: pollInterval", "paused: isPaused", `claimed: lastName != ""`, "changed: changed", "seek: seek",
			"step: step", "idle: sampleTime.Sub(anchorTime)", "now: sampleTime"}
		var got []string
		if len(jcall.Args) == 2 && types.ExprString(jcall.Args[0]) == "judge" {
			if lit, ok := jcall.Args[1].(*ast.CompositeLit); ok && types.ExprString(lit.Type) == "judgeIn" {
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						got = append(got, types.ExprString(kv.Key)+": "+types.ExprString(kv.Value))
					} else {
						got = append(got, types.ExprString(e))
					}
				}
			}
		}
		if !slices.Equal(got, wantArgs) {
			probs = append(probs, fmt.Sprintf("a: judgeTick 的入参应为 judge, judgeIn{%s}，实得 %s",
				strings.Join(wantArgs, ", "), types.ExprString(jcall)))
		}
	}

	// j：调用是 judge, v = judgeTick(…)（=），judge 是 runSession 顶层声明的那个，循环体里没有别的 judge 声明。
	if jcall != nil {
		var as *ast.AssignStmt
		ast.Inspect(loop.Body, func(x ast.Node) bool {
			if a, ok := x.(*ast.AssignStmt); ok && len(a.Rhs) == 1 && a.Rhs[0] == jcall {
				as = a
			}
			return true
		})
		switch {
		case as == nil:
			probs = append(probs, "j: judgeTick 的结果应以赋值语句 judge, v = judgeTick(…) 接收")
		case as.Tok != token.ASSIGN:
			probs = append(probs, "j: 应为 judge, v = judgeTick(…)（=），不是 "+as.Tok.String())
		case len(as.Lhs) != 2 || types.ExprString(as.Lhs[0]) != "judge" || types.ExprString(as.Lhs[1]) != "v":
			probs = append(probs, "j: 左边应为 judge, v")
		}
	}
	topJudge := false
	for _, s := range fn.Body.List {
		if d, ok := s.(*ast.DeclStmt); ok {
			if g, ok := d.Decl.(*ast.GenDecl); ok && g.Tok == token.VAR {
				for _, sp := range g.Specs {
					for _, n := range sp.(*ast.ValueSpec).Names {
						topJudge = topJudge || n.Name == "judge"
					}
				}
			}
		}
	}
	if !topJudge {
		probs = append(probs, "j: runSession 顶层没有 var judge 声明")
	}
	ast.Inspect(loop.Body, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, l := range x.Lhs {
					if types.ExprString(l) == "judge" {
						probs = append(probs, "j: 轮询循环体里另声明了 judge（:=）")
					}
				}
			}
		case *ast.ValueSpec:
			for _, n := range x.Names {
				if n.Name == "judge" {
					probs = append(probs, "j: 轮询循环体里另声明了 judge（var）")
				}
			}
		}
		return true
	})

	// b：判定、去抖与两条核对不在 runSession 里另写。
	forbidden := map[string]bool{"classify": true, "observe": true, "pauseTimeout": true, "playStateStallLimit": true,
		"movedWindow": true, "MinPause": true}
	ast.Inspect(fn.Body, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && forbidden[id.Name] {
			probs = append(probs, fmt.Sprintf("b: runSession 里出现了 %s（第 %d 行）", id.Name, fset.Position(id.Pos()).Line))
		}
		return true
	})

	// c：isPaused 只在 v.pause（置真）、v.resume 与换歌认领（置假）里赋值。
	ast.Inspect(fn.Body, func(x ast.Node) bool {
		as, ok := x.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, l := range as.Lhs {
			if types.ExprString(l) != "isPaused" {
				continue
			}
			rhs := ""
			if len(as.Rhs) == len(as.Lhs) {
				rhs = types.ExprString(as.Rhs[i])
			}
			ok := rhs == "true" && within(as, pauseIf.Body) ||
				rhs == "false" && (within(as, resumeIf.Body) || within(as, switchIf.Body))
			if !ok {
				probs = append(probs, fmt.Sprintf("c: isPaused 在 v.pause、v.resume 与换歌认领之外被赋值（第 %d 行）",
					fset.Position(as.Pos()).Line))
			}
		}
		return true
	})

	// d：playback_pause 只在 v.pause 体里直接发出。暂停中补发只经 p.emitFrozenPause（#218 变基后补取与播放中换词共用一处，
	// 入队失败每拍再试），补发处至多一处（没有时由 g 报出）：条件为 isPaused、体内调用它的 if（在 showLyrics 里）；另可在
	// 暂停中跳转的补发处（if v.pausedSeek，第 5 轮，形状由 m 查）与入队失败的再试处（if repausePending && isPaused）调用。
	// 补发处连同它所在的块记下，g 要求发出歌词的调用也在这个块里（同一条路径上）。
	type repauseAt struct {
		ifs    *ast.IfStmt
		parent *ast.BlockStmt
	}
	var repause []repauseAt
	var retryIfs, pausedSeekIfs []*ast.IfStmt
	ast.Inspect(loop.Body, func(x ast.Node) bool {
		if ifs, ok := x.(*ast.IfStmt); ok && types.ExprString(ifs.Cond) == "v.pausedSeek" {
			pausedSeekIfs = append(pausedSeekIfs, ifs)
		}
		return true
	})
	ast.Inspect(fn.Body, func(x ast.Node) bool {
		if b, ok := x.(*ast.BlockStmt); ok {
			for _, s := range b.List {
				ifs, ok := s.(*ast.IfStmt)
				if !ok || len(callsIn(ifs.Body, isFrozenPause)) == 0 {
					continue
				}
				switch types.ExprString(ifs.Cond) {
				case "isPaused":
					repause = append(repause, repauseAt{ifs, b})
				case "repausePending && isPaused":
					retryIfs = append(retryIfs, ifs)
				}
			}
		}
		return true
	})
	if len(repause) > 1 {
		probs = append(probs, fmt.Sprintf("d: 暂停中补发应只有一处 if isPaused { … p.emitFrozenPause(…) … }，实得 %d 处", len(repause)))
	}
	for _, c := range callsIn(fn.Body, isPauseEmit) {
		if !within(c, pauseIf.Body) {
			probs = append(probs, fmt.Sprintf("d: playback_pause 在 v.pause 体之外直接发出（第 %d 行），暂停中补发应经 emitFrozenPause",
				fset.Position(c.Pos()).Line))
		}
	}
	for _, c := range callsIn(fn.Body, isFrozenPause) {
		ok := false
		for _, rp := range repause {
			ok = ok || within(c, rp.ifs.Body)
		}
		for _, ifs := range append(slices.Clone(retryIfs), pausedSeekIfs...) {
			ok = ok || within(c, ifs.Body)
		}
		if !ok {
			probs = append(probs, fmt.Sprintf("d: emitFrozenPause 在补发处、暂停中跳转与入队失败的再试处之外调用（第 %d 行）", fset.Position(c.Pos()).Line))
		}
	}

	// e：判恢复重锚 anchorTime。
	reanchor := false
	ast.Inspect(resumeIf.Body, func(x ast.Node) bool {
		if as, ok := x.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 &&
			types.ExprString(as.Lhs[0]) == "anchorTime" && types.ExprString(as.Rhs[0]) == "sampleTime" {
			reanchor = true
		}
		return true
	})
	if !reanchor {
		probs = append(probs, "e: if v.resume 体里应有 anchorTime = sampleTime")
	}

	// f：两处重读的采用分支都把 meta.PlayState、meta.PlayStateOK 换成 fresh 的。
	var rereadIfs []*ast.IfStmt
	ast.Inspect(fn.Body, func(x ast.Node) bool {
		if ifs, ok := x.(*ast.IfStmt); ok {
			if c, ok := ifs.Cond.(*ast.CallExpr); ok && callName(c) == "rereadSameSong" {
				rereadIfs = append(rereadIfs, ifs)
			}
		}
		return true
	})
	if len(rereadIfs) != 2 {
		probs = append(probs, fmt.Sprintf("f: 应有两处以 rereadSameSong 为条件的重读，实得 %d", len(rereadIfs)))
	}
	for _, ifs := range rereadIfs {
		pairs := map[string]bool{}
		ast.Inspect(ifs.Body, func(x ast.Node) bool {
			if as, ok := x.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
				for i := range as.Lhs {
					pairs[types.ExprString(as.Lhs[i])+"="+types.ExprString(as.Rhs[i])] = true
				}
			}
			return true
		})
		for _, w := range []string{"meta.PlayState=fresh.PlayState", "meta.PlayStateOK=fresh.PlayStateOK"} {
			if !pairs[w] {
				probs = append(probs, fmt.Sprintf("f: 第 %d 行的重读采用时没有 %s", fset.Position(ifs.Pos()).Line, w))
			}
		}
	}

	// g：换歌分支之外每处发出 all_lyrics（补取块与播放中换词都经 showLyrics，#218）之后、按源码次序的下一处同步请求之前，
	// 同一条路径上（补发所在的块包含发出处）有 if isPaused { … p.emitFrozenPause(…) … }。showLyrics 定义在轮询循环之前，
	// 它里面的补发在源码上即早于补取块其后的取封面地址；补取块须经 showLyrics 或直接发出歌词，否则自报失效。
	syncCalls := callsIn(fn.Body, func(c *ast.CallExpr) bool {
		switch callName(c) {
		case "fetchLRC", "fetchCoverURL", "FindSongMid", "ReadAllMetadata":
			return true
		}
		return false
	})
	isLyricsEmit := func(c *ast.CallExpr) bool {
		fun := types.ExprString(c.Fun)
		return fun == "p.emitNoLyrics" || fun == "p.Emit" && len(c.Args) > 0 && types.ExprString(c.Args[0]) == "player.EventAllLyrics"
	}
	emits := callsIn(fn.Body, func(c *ast.CallExpr) bool { return isLyricsEmit(c) && !within(c, switchIf.Body) })
	if len(emits) == 0 || len(callsIn(fillIf, isLyricsEmit))+len(callsIn(fillIf, funIs("showLyrics"))) == 0 {
		probs = append(probs, "g: 换歌分支之外或补取块里没找到发出 all_lyrics 的调用 —— 门禁失效")
	}
	for _, e := range emits {
		next := token.NoPos
		for _, s := range syncCalls {
			if s.Pos() > e.End() && (next == token.NoPos || s.Pos() < next) {
				next = s.Pos()
			}
		}
		ok := false
		for _, rp := range repause {
			ok = ok || rp.ifs.Pos() > e.End() && (next == token.NoPos || rp.ifs.End() < next) && within(e, rp.parent)
		}
		if !ok {
			probs = append(probs, fmt.Sprintf("g: 第 %d 行发出歌词之后、下一处同步请求之前，同一条路径上没有 if isPaused 经 emitFrozenPause 补发",
				fset.Position(e.Pos()).Line))
		}
	}

	// h：换歌认领复位 judge.resume。
	if len(callsIn(switchIf, funIs("judge.resume.reset"))) == 0 {
		probs = append(probs, "h: 换歌认领里没有 judge.resume.reset()")
	}

	// i：核对触发 Warn 并上报；未满去抖的暂停值记 Detail。
	if len(callsIn(trippedIf.Body, funIs("log.Warn"))) == 0 || len(callsIn(trippedIf.Body, funIs("telemetry.ReportOnce"))) == 0 {
		probs = append(probs, `i: if v.tripped != "" 体里应有 log.Warn 与 telemetry.ReportOnce`)
	}
	if len(callsIn(flashIf.Body, funIs("log.Detail"))) == 0 {
		probs = append(probs, "i: if v.flash > 0 体里应有 log.Detail")
	}

	// k（#227，设计 §6.10）：补取块恰有一处 switch backfill.next(time.Now(), isPaused)，没有 default；取词与扫描只在只列
	// backfillRetry 的子句里；lyricsPending = false 在补取块里只在 backfillGiveUp 子句与取词成功处；截止与节流不在
	// runSession 里另写（不出现 lyricRetryInterval、pendingDeadline、lastRetry，不访问 backfill 的字段，lyricRetryWindow
	// 只作放弃时日志的参数：log.Warn，来源为本地文件关联的歌词时 log.Detail，#218）。
	inside := func(n, m ast.Node) bool { return n.Pos() >= m.Pos() && n.End() <= m.End() }
	inCase := func(n ast.Node, cc *ast.CaseClause) bool { return cc != nil && inside(n, cc) }
	var bswitch []*ast.SwitchStmt
	ast.Inspect(fn.Body, func(x ast.Node) bool {
		if sw, ok := x.(*ast.SwitchStmt); ok && sw.Tag != nil && types.ExprString(sw.Tag) == "backfill.next(time.Now(), isPaused)" {
			bswitch = append(bswitch, sw)
		}
		return true
	})
	var retryCC, giveUpCC *ast.CaseClause
	if len(bswitch) != 1 || !within(bswitch[0], fillIf.Body) {
		probs = append(probs, fmt.Sprintf("k: 补取块里应恰有一处 switch backfill.next(time.Now(), isPaused)，runSession 里实得 %d 处", len(bswitch)))
	} else {
		for _, s := range bswitch[0].Body.List {
			cc := s.(*ast.CaseClause)
			var vals []string
			for _, e := range cc.List {
				vals = append(vals, types.ExprString(e))
			}
			switch {
			case cc.List == nil:
				probs = append(probs, "k: switch backfill.next 不应有 default 子句")
			case slices.Equal(vals, []string{"backfillRetry"}):
				retryCC = cc
			case slices.Equal(vals, []string{"backfillGiveUp"}):
				giveUpCC = cc
			case slices.Contains(vals, "backfillRetry"):
				probs = append(probs, "k: 重试的子句应只列 backfillRetry，实得 case "+strings.Join(vals, ", "))
			}
		}
		if retryCC == nil || giveUpCC == nil {
			probs = append(probs, "k: switch backfill.next 里应有只列 backfillRetry、只列 backfillGiveUp 的子句各一")
		}
	}
	for _, c := range callsIn(fillIf, func(c *ast.CallExpr) bool {
		switch callName(c) {
		case "FindSongMid", "FindCookie", "fetchLRC", "fetchCoverURL":
			return true
		}
		return false
	}) {
		if !inCase(c, retryCC) {
			probs = append(probs, fmt.Sprintf("k: 第 %d 行的 %s 不在 case backfillRetry 子句里", fset.Position(c.Pos()).Line, callName(c)))
		}
	}
	var fetchOK []*ast.IfStmt // 取词成功处：case backfillRetry 里条件为 ferr == nil 的 if
	if retryCC != nil {
		ast.Inspect(retryCC, func(x ast.Node) bool {
			if ifs, ok := x.(*ast.IfStmt); ok && types.ExprString(ifs.Cond) == "ferr == nil" {
				fetchOK = append(fetchOK, ifs)
			}
			return true
		})
	}
	ast.Inspect(fillIf, func(x ast.Node) bool {
		as, ok := x.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 || types.ExprString(as.Lhs[0]) != "lyricsPending" || types.ExprString(as.Rhs[0]) != "false" {
			return true
		}
		ok = inCase(as, giveUpCC)
		for _, ifs := range fetchOK {
			ok = ok || within(as, ifs.Body)
		}
		if !ok {
			probs = append(probs, fmt.Sprintf("k: 第 %d 行的 lyricsPending = false 不在 case backfillGiveUp 子句或取词成功处", fset.Position(as.Pos()).Line))
		}
		return true
	})
	warns := append(callsIn(fn.Body, funIs("log.Warn")), callsIn(fn.Body, funIs("log.Detail"))...)
	ast.Inspect(fn.Body, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.Ident:
			switch x.Name {
			case "lyricRetryInterval", "pendingDeadline", "lastRetry":
				probs = append(probs, fmt.Sprintf("k: runSession 里出现了 %s（第 %d 行）", x.Name, fset.Position(x.Pos()).Line))
			case "lyricRetryWindow":
				ok := false
				for _, w := range warns {
					ok = ok || inside(x, w)
				}
				if !ok {
					probs = append(probs, fmt.Sprintf("k: lyricRetryWindow 只应作 log.Warn 或 log.Detail 的参数（第 %d 行）", fset.Position(x.Pos()).Line))
				}
			}
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "backfill" && x.Sel.Name != "arm" && x.Sel.Name != "next" {
				probs = append(probs, fmt.Sprintf("k: runSession 访问了 backfill.%s（第 %d 行）", x.Sel.Name, fset.Position(x.Pos()).Line))
			}
		}
		return true
	})

	// l（#227）：判恢复时补取未了结即重设窗口（if v.resume 体里 if lyricsPending { … backfill.arm(sampleTime) … }）；
	// 换歌认领里 ID 缺位处（置 lyricsPending = true 的块）开窗。
	armIn := func(n ast.Node, arg string) bool {
		for _, c := range callsIn(n, funIs("backfill.arm")) {
			if arg == "" || len(c.Args) == 1 && types.ExprString(c.Args[0]) == arg {
				return true
			}
		}
		return false
	}
	resumeArm := false
	ast.Inspect(resumeIf.Body, func(x ast.Node) bool {
		if ifs, ok := x.(*ast.IfStmt); ok && types.ExprString(ifs.Cond) == "lyricsPending" && armIn(ifs.Body, "sampleTime") {
			resumeArm = true
		}
		return true
	})
	if !resumeArm {
		probs = append(probs, "l: if v.resume 体里应有 if lyricsPending { backfill.arm(sampleTime) }")
	}
	claimArm := false
	ast.Inspect(switchIf, func(x ast.Node) bool {
		if b, ok := x.(*ast.BlockStmt); ok {
			for _, s := range b.List {
				if as, ok := s.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 &&
					types.ExprString(as.Lhs[0]) == "lyricsPending" && types.ExprString(as.Rhs[0]) == "true" && armIn(b, "") {
					claimArm = true
				}
			}
		}
		return true
	})
	if !claimArm {
		probs = append(probs, "l: 换歌认领里置 lyricsPending = true 的块应调用 backfill.arm(…)")
	}

	// m（第 5 轮，设计 §6.7 第 7 条）：变化分支 if seekDetected 体里，发 playback_resume 的调用与 resumeEmitted = true 各恰一处，
	// 都在条件恰为 !isPaused 的 if 体里（跳转之前已判暂停的由判定决定）；轮询循环体里恰有一处 if v.pausedSeek，在循环体顶层、
	// if v.pause 之后、入队失败的再试之前，体内先 repausePos = float32(anchorProgressMs) / 1000.0、后
	// repausePending = !p.emitFrozenPause(repausePos, currentDurationSec)，不发 status_update、不直接发 playback_pause、不改 isPaused。
	var seekIf *ast.IfStmt
	ast.Inspect(changeIf.Body, func(x ast.Node) bool {
		if ifs, ok := x.(*ast.IfStmt); ok && types.ExprString(ifs.Cond) == "seekDetected" {
			seekIf = ifs
		}
		return true
	})
	assignsIn := func(n ast.Node, lhs, rhs string) []*ast.AssignStmt {
		var out []*ast.AssignStmt
		ast.Inspect(n, func(x ast.Node) bool {
			if as, ok := x.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 && types.ExprString(as.Lhs[0]) == lhs &&
				(rhs == "" || types.ExprString(as.Rhs[0]) == rhs) {
				out = append(out, as)
			}
			return true
		})
		return out
	}
	if seekIf == nil {
		probs = append(probs, "m: 变化分支里没找到 if seekDetected")
	} else {
		var notPaused []*ast.IfStmt
		ast.Inspect(seekIf.Body, func(x ast.Node) bool {
			if ifs, ok := x.(*ast.IfStmt); ok && types.ExprString(ifs.Cond) == "!isPaused" {
				notPaused = append(notPaused, ifs)
			}
			return true
		})
		guarded := func(n ast.Node) bool {
			for _, ifs := range notPaused {
				if within(n, ifs.Body) {
					return true
				}
			}
			return false
		}
		resumes := callsIn(seekIf.Body, func(c *ast.CallExpr) bool {
			return len(c.Args) > 0 && types.ExprString(c.Args[0]) == "player.EventPlaybackResume"
		})
		marks := assignsIn(seekIf.Body, "resumeEmitted", "true")
		if len(resumes) != 1 || len(marks) != 1 || !guarded(resumes[0]) || !guarded(marks[0]) {
			probs = append(probs, fmt.Sprintf("m: if seekDetected 体里发 playback_resume 与 resumeEmitted = true 应各恰一处、都在 if !isPaused 体里"+
				"（实得 %d 处、%d 处）", len(resumes), len(marks)))
		}
	}
	pauseAt, seekAt, retryAt := -1, -1, -1
	for i, s := range loop.Body.List {
		if ifs, ok := s.(*ast.IfStmt); ok {
			switch types.ExprString(ifs.Cond) {
			case "v.pause":
				pauseAt = i
			case "v.pausedSeek":
				seekAt = i
			case "repausePending && isPaused":
				retryAt = i
			}
		}
	}
	if len(pausedSeekIfs) != 1 || seekAt < 0 || !(pauseAt < seekAt && seekAt < retryAt) {
		probs = append(probs, fmt.Sprintf("m: 轮询循环体顶层应恰有一处 if v.pausedSeek，位于 if v.pause 之后、if repausePending && isPaused 之前"+
			"（循环体里实得 %d 处）", len(pausedSeekIfs)))
	}
	for _, ifs := range pausedSeekIfs {
		pos := assignsIn(ifs.Body, "repausePos", "float32(anchorProgressMs) / 1000.0")
		emit := assignsIn(ifs.Body, "repausePending", "!p.emitFrozenPause(repausePos, currentDurationSec)")
		if len(pos) != 1 || len(emit) != 1 || pos[0].Pos() > emit[0].Pos() {
			probs = append(probs, "m: if v.pausedSeek 体里应先 repausePos = float32(anchorProgressMs) / 1000.0，"+
				"后 repausePending = !p.emitFrozenPause(repausePos, currentDurationSec)")
		}
		statusUpdate := false
		ast.Inspect(ifs.Body, func(x ast.Node) bool {
			if e, ok := x.(*ast.SelectorExpr); ok && types.ExprString(e) == "player.EventStatusUpdate" {
				statusUpdate = true
			}
			return true
		})
		if statusUpdate {
			probs = append(probs, "m: if v.pausedSeek 体里不应发 status_update（状态没有变）")
		}
		if len(callsIn(ifs.Body, isPauseEmit)) > 0 {
			probs = append(probs, "m: if v.pausedSeek 体里不应直接发 playback_pause（经 emitFrozenPause，入队失败每拍再试）")
		}
		if len(assignsIn(ifs.Body, "isPaused", "")) > 0 {
			probs = append(probs, "m: if v.pausedSeek 体里不应改 isPaused（仍在暂停中）")
		}
	}
	return probs
}

// TestRunSessionPlayStateWiring 钉住 runSession 里 #172 的接线形状（§7.4 a～j）与 #227 补取窗口的接线（k、l）。门禁
// 自身的变异见下一条。
//
// 变异自证（均实测，改的是 qqmusic.go，在分离的副本里）：下一条自测的各种改写里能编译的二十三种逐条写进 qqmusic.go，
// 每种都只有本例（与下一条里依赖同一处原文的自测）红，回放与 judgeTick 的用例全绿——接线层只有这里守得住；其中
// judge, v := judgeTick(…) 能编译（v 已声明、judge 在循环体里新建并被 judge.movedAt 用到），去抖与核对随之失效。
// k、l 的九种同样逐条写进 qqmusic.go（重试体写成 default、case backfillRetry, backfillHold、backfillHold 子句放弃、
// 重试体另按 backfill.lastRetry 节流、另声明 pendingDeadline 并在补取块里比较、放弃子句里另按 lyricRetryWindow 比较、
// 认领处直接改 backfill.deadline、判恢复不调 arm、认领处不调 arm），每种都只有本例（与依赖同一处原文的自测）红，
// 回放、judgeTick 与 backfillWindow 的用例全绿。
//
// #218 变基后（暂停中补发并为 showLyrics 里经 emitFrozenPause 的一处）另写进 qqmusic.go 的八种（均实测）：去掉补发、
// 补发挪到补取块取封面之后、补发改用 Emit 直接发、补发条件改为 true、补发只放在有歌词一支、播放中换词绕过 showLyrics
// 直接发 all_lyrics，本例与 assocwiring_test.go 的接线门禁都红；补取块另保留变基前的 if isPaused { p.Emit(…) }（两处
// 补发）、放弃子句另按 lyricRetryWindow 比较，只有本例红。回放（含 L 组）与 judgeTick 的用例全绿。
//
// 第 5 轮（暂停中跳转，m）另写进 qqmusic.go 的十种（均实测）：seek 的 playback_resume 不收进 if !isPaused（第 4 轮的写法）、
// 收了发出而漏收 resumeEmitted = true、块里多发 status_update、补发位置取 meta.ProgressMs 或 lastMemProgress、块挪到
// if v.pause 之前或入队失败的再试之后，本例以 m 报出；块里加 isPaused = true，m 与 c 报出；块里改用 p.Emit 直接发
// playback_pause，m 与 d 报出；删去 if v.pausedSeek 块，m 报出；后两种 assocwiring_test.go 的赋值计数也红。回放与
// judgeTick 的用例全绿（下一条里依赖同一处原文的自测随之自报失效）。
func TestRunSessionPlayStateWiring(t *testing.T) {
	if probs := playStateWiringProblems(readQQMusicSource(t)); len(probs) > 0 {
		t.Fatalf("runSession 的 #172 接线不符：\n  %s", strings.Join(probs, "\n  "))
	}
}

// TestPlayStateWiringCatchesKnownHoles 以改写过的源码确认门禁拦得住设计 §7.6 接线层的每一行：每种改写都要由对应的条目
// 报出问题。改写找不到原文时自报失效（源码变了，这条自测要跟着改）。
//
// 变异自证（均实测，改的是上面的 playStateWiringProblems）：a～l 任一条不报，该条的各种改写红；g 只比位置、不要求补发
// 所在的块包含发出处，「拆开两条路径时漏掉无歌词一条」红；k 不查 lyricRetryWindow 只作 Warn 的参数、不查
// lyricRetryInterval / pendingDeadline / lastRetry、不查 lyricsPending = false 的位置、不查访问 backfill 的字段，依次
// 「补取块另按 lyricRetryWindow 比较」「认领处另记截止」「backfillHold 子句放弃」「认领处直接改窗口的截止」红。
// #218 变基后的 d、g、k（均实测）：g 不要求补发所在的块包含发出处，「拆开两条路径时漏掉无歌词一条」红；g 退回只扫补取块
// 里的直接发出，上一条用例红（补取块改经 showLyrics，自报失效）；d 不数补发处、只认 p.Emit、不查 emitFrozenPause 的调用处，
// 依次「补取块另加一处补发」「补发改用 TryEmit 直接发」「在补发处与再试处之外经 emitFrozenPause 发」红；k 不放行 log.Detail，
// 上一条用例红（放弃时来源为本地文件的 Detail 带 lyricRetryWindow）。
// 第 5 轮的 m 与 d（均实测）：m 不查 if !isPaused 的包裹，四条 seek 的改写红；不查 if v.pausedSeek 的位置，两条挪动红；不查
// 体内补发的形状，「位置取 meta.ProgressMs / lastMemProgress」「先补发、后记位置」红；认不出 status_update、不查 isPaused
// 的赋值、不查体内直接发 playback_pause，依次「多发 status_update」「加 isPaused = true」「补发之外另直接发」红；d 不放行
// if v.pausedSeek 体，上一条用例红。
func TestPlayStateWiringCatchesKnownHoles(t *testing.T) {
	src := readQQMusicSource(t)
	const (
		callHead   = "\t\tjudge, v = judgeTick("
		changeHead = "\t\t// --- 快速计时器锚点更新 + seek 检测 ---\n"
		interpHead = "\t\t// --- 本地时钟插值 ---\n"
		reread     = "meta.PlayState, meta.PlayStateOK = fresh.PlayState, fresh.PlayStateOK"
		// 暂停中补发：#218 变基后只在 showLyrics 里一处，补取与播放中换词共用
		repause    = "\t\tif isPaused {\n\t\t\trepausePos = pos\n\t\t\trepausePending = !p.emitFrozenPause(repausePos, currentDurationSec)\n\t\t}\n"
		repauseOne = "\t\t\trepausePos = pos\n\t\t\trepausePending = !p.emitFrozenPause(repausePos, currentDurationSec)\n"
		coverLine  = "\t\t\t\t\t\tcoverURL, det := fetchCoverURL(sid, smid)\n"
		allEmitEnd = "\t\t\t\tCount: len(items), Lyrics: items,\n\t\t\t})\n" // showLyrics 里有歌词一支的 all_lyrics
		fillShow   = "\t\t\t\t\t\tshowLyrics(shown, float32(meta.ProgressMs)/1000.0, sampleTime)\n"
		fillRaw    = "\t\t\t\t\t\tlyricSt.setOnlineRaw(lines)\n"
		tickShow   = "\t\t\t\tshowLyrics(lines, float32(anchorProgressMs)/1000.0, anchorTime)\n"
		sampleHead = "\t\t// 位置采样"
		resumeHead = "\t\t\tisPaused = false\n\t\t\trepausePending = false\n\t\t\tanchorTime = sampleTime\n" // 只在 if v.resume 体里是这个次序
		// #227 的补取窗口（k、l）
		backfillHead = "\t\t\tswitch backfill.next(time.Now(), isPaused) {\n"
		retryCase    = "\t\t\tcase backfillRetry:\n"
		giveUpCase   = "\t\t\tcase backfillGiveUp:\n"
		retryBody    = "\t\t\t\tretryCount++\n"
		claimArm     = "\t\t\t\tbackfill.arm(time.Now())\n"
		resumeArm    = "backfill.arm(sampleTime)"
		// 第 5 轮的暂停中跳转（m）
		seekGuard   = "\t\t\t\tif !isPaused {\n"
		seekResume  = seekGuard + "\t\t\t\t\tposSec := float32(meta.ProgressMs) / 1000.0\n\t\t\t\t\tp.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{\n\t\t\t\t\t\tPosition: posSec, Progress: player.ClampProgress(posSec, currentDurationSec),\n\t\t\t\t\t})\n\t\t\t\t\tresumeEmitted = true\n\t\t\t\t}\n"
		seekRound4  = "\t\t\t\tposSec := float32(meta.ProgressMs) / 1000.0\n\t\t\t\tp.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{\n\t\t\t\t\tPosition: posSec, Progress: player.ClampProgress(posSec, currentDurationSec),\n\t\t\t\t})\n\t\t\t\tresumeEmitted = true\n"
		seekMark    = "\t\t\t\t\tresumeEmitted = true\n\t\t\t\t}\n"
		pausedHead  = "\t\tif v.pausedSeek {\n"
		pauseHead   = "\t\tif v.pause {\n"
		pausedPos   = "\t\t\trepausePos = float32(anchorProgressMs) / 1000.0\n"
		pausedEmit  = "\t\t\trepausePending = !p.emitFrozenPause(repausePos, currentDurationSec)\n"
		pausedBody  = pausedPos + pausedEmit
		retryRepeat = "\t\tif repausePending && isPaused {\n\t\t\trepausePending = !p.emitFrozenPause(repausePos, currentDurationSec)\n\t\t}\n"
	)
	// pausedBlock 取出 if v.pausedSeek 整块（自 pausedHead 到其后第一个 "\t\t}\n"），找不到为空；movePaused 把它挪到 at 之前
	// 或之后，找不到块或 at 时不改写。
	pausedBlock := func(s string) string {
		i := strings.Index(s, pausedHead)
		if i < 0 {
			return ""
		}
		return s[i : i+strings.Index(s[i:], "\t\t}\n")+len("\t\t}\n")]
	}
	movePaused := func(at string, after bool) func(string) string {
		return func(orig string) string {
			b := pausedBlock(orig)
			s := strings.Replace(orig, b, "", 1)
			if b == "" || !strings.Contains(s, at) {
				return orig
			}
			if after {
				return strings.Replace(s, at, at+b, 1)
			}
			return strings.Replace(s, at, b+at, 1)
		}
	}
	// cutCall 取出 judgeTick 的调用语句（自 callHead 到其后第一个 "})\n"）。
	cutCall := func(s string) (stmt, rest string) {
		i := strings.Index(s, callHead)
		j := strings.Index(s[i:], "})\n") + i + len("})\n")
		return s[i:j], s[:i] + s[j:]
	}
	moveCall := func(before string) func(string) string {
		return func(s string) string {
			stmt, rest := cutCall(s)
			return strings.Replace(rest, before, stmt+before, 1)
		}
	}
	replace := func(old, new string) func(string) string {
		return func(s string) string { return strings.Replace(s, old, new, 1) }
	}
	insertBefore := func(at, text string) func(string) string {
		return func(s string) string { return strings.Replace(s, at, text+at, 1) }
	}
	insertAfter := func(at, text string) func(string) string {
		return func(s string) string { return strings.Replace(s, at, at+text, 1) }
	}
	moveRepause := func(to func(string) string) func(string) string {
		return func(s string) string { return to(strings.Replace(s, repause, "", 1)) }
	}
	cases := []struct {
		name   string
		want   string // 应由哪一条报出（问题的前缀）
		needle string // 改写依赖的原文
		mutate func(string) string
	}{
		{"judgeTick 挪到变化分支之前", "a:", callHead, moveCall(changeHead)},
		{"judgeTick 挪到插值之后", "a:", callHead, moveCall(sampleHead)},
		{"spec 传某一版的常量", "a:", "spec: mem.Offsets().PlayState", replace("spec: mem.Offsets().PlayState", `spec: knownVersions["22.82"].PlayState`)},
		{"rawOK 恒真", "a:", "rawOK: meta.PlayStateOK", replace("rawOK: meta.PlayStateOK", "rawOK: true")},
		{"poll 传常量", "a:", "poll: pollInterval", replace("poll: pollInterval", "poll: 30 * time.Millisecond")},
		{"idle 量到 time.Now()", "a:", "idle: sampleTime.Sub(anchorTime)", replace("idle: sampleTime.Sub(anchorTime)", "idle: time.Since(anchorTime)")},
		{"paused 恒假", "a:", "paused: isPaused", replace("paused: isPaused", "paused: false")},
		{"raw 取重读之前另存的首读", "a:", "raw: meta.PlayState,", replace("raw: meta.PlayState,", "raw: firstRaw,")},
		{"写成 judge, v := judgeTick(…)", "j:", callHead, replace(callHead, "\t\tjudge, v := judgeTick(")},
		{"循环体里另声明 judge", "j:", "\t\tvar v verdict\n", insertBefore("\t\tvar v verdict\n", "\t\tvar judge judgeState\n")},
		{"另写停走判暂停的 if", "b:", interpHead, insertBefore(interpHead,
			"\t\tif sampleTime.Sub(anchorTime) > pauseTimeout && !isPaused && lastName != \"\" {\n\t\t\tisPaused = true\n\t\t}\n")},
		{"直接调 resume.observe", "b:", interpHead, insertBefore(interpHead, "\t\t_ = judge.resume.observe(step, sampleTime, time.Second)\n")},
		{"直接调 classify", "b:", interpHead, insertBefore(interpHead, "\t\t_ = mem.Offsets().PlayState.classify(meta.PlayState)\n")},
		{"isPaused 在别处被赋值", "c:", "\t\t\t\t\tresumeEmitted = true\n", insertAfter("\t\t\t\t\tresumeEmitted = true\n", "\t\t\t\t\tisPaused = false\n")},
		{"补取块另发 playback_pause", "d:", fillRaw, insertAfter(fillRaw, "\t\t\t\t\t\tp.Emit(player.EventPlaybackPause, nil)\n")},
		{"补发改用 Emit 直接发（不再试）", "d:", repauseOne,
			replace(repauseOne, "\t\t\trepausePos = pos\n\t\t\tp.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: pos, Progress: prog})\n")},
		{"补发改用 TryEmit 直接发", "d:", repauseOne,
			replace(repauseOne, "\t\t\trepausePos = pos\n\t\t\trepausePending = !p.TryEmit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: pos})\n")},
		{"补取块另加一处补发", "d:", fillShow, insertAfter(fillShow, "\t\t\t\t\t\tif isPaused {\n\t\t\t\t\t\t\t_ = p.emitFrozenPause(0, currentDurationSec)\n\t\t\t\t\t\t}\n")},
		{"在补发处与再试处之外经 emitFrozenPause 发", "d:", interpHead, insertBefore(interpHead, "\t\t_ = p.emitFrozenPause(0, currentDurationSec)\n")},
		{"判恢复不重锚 anchorTime", "e:", resumeHead, replace(resumeHead, "\t\t\tisPaused = false\n\t\t\trepausePending = false\n")},
		{"认领处的重读不换标志", "f:", reread, replace(reread, "_ = fresh")},
		{"补取处的重读不换标志", "f:", reread, func(s string) string {
			i := strings.LastIndex(s, reread)
			return s[:i] + "_ = fresh" + s[i+len(reread):]
		}},
		{"重读只换 PlayState、不换 PlayStateOK", "f:", reread, replace(reread, "meta.PlayState = fresh.PlayState")},
		{"去掉补发", "g:", repause, replace(repause, "")},
		{"补发挪到 fetchCoverURL 之后", "g:", repause, moveRepause(insertAfter(coverLine, repause))},
		{"补发挪到行匹配之后", "g:", repause, moveRepause(insertBefore(sampleHead, repause))},
		{"补发条件去掉 isPaused", "g:", repause, replace(repause, strings.Replace(repause, "if isPaused {", "if true {", 1))},
		{"拆开两条路径时漏掉无歌词一条", "g:", allEmitEnd, moveRepause(insertAfter(allEmitEnd, repause))},
		{"补取块绕过 showLyrics 直接发歌词", "g:", fillShow, replace(fillShow,
			"\t\t\t\t\t\tp.Emit(player.EventAllLyrics, &player.AllLyricsData{Title: currentTitle, Position: 0})\n")},
		{"播放中换词绕过 showLyrics 直接发歌词", "g:", tickShow, replace(tickShow, "\t\t\t\tp.emitNoLyrics(currentTitle, currentDurationSec, 0, 0)\n")},
		{"补取块不再发歌词（锚点）", "g:", fillShow, replace(fillShow, "")},
		{"认领不复位 judge.resume", "h:", "judge.resume.reset()", replace("judge.resume.reset()", "_ = judge")},
		{"核对触发不 Warn", "i:", `log.Warn("QQ Music reports`, func(s string) string {
			return strings.ReplaceAll(s, `log.Warn("QQ Music reports`, `log.Info("QQ Music reports`)
		}},
		{"核对触发不上报", "i:", "telemetry.ReportOnce(\"qqmusic.play_state_mismatch\"",
			replace("telemetry.ReportOnce(\"qqmusic.play_state_mismatch\"", "reportOnceDisabled(\"qqmusic.play_state_mismatch\"")},
		{"未满去抖的暂停值不记 Detail", "i:", `log.Detail("QQ Music play state held`, replace(`log.Detail("QQ Music play state held`, `log.Info("QQ Music play state held`)},
		{"补取块不经 switch backfill.next", "k:", backfillHead, replace(backfillHead, "\t\t\tswitch step := backfill.next(time.Now(), isPaused); step {\n")},
		{"重试体写成 default 子句", "k:", retryCase, replace(retryCase, "\t\t\tdefault:\n")},
		{"backfillHold 也进入重试体", "k:", retryCase, replace(retryCase, "\t\t\tcase backfillRetry, backfillHold:\n")},
		{"backfillHold 子句放弃（修复之前的行为）", "k:", retryCase, insertBefore(retryCase, "\t\t\tcase backfillHold:\n\t\t\t\tlyricsPending = false\n")},
		{"重试体另按 lastRetry 节流", "k:", retryBody, insertBefore(retryBody,
			"\t\t\t\tif time.Since(backfill.lastRetry) < lyricRetryInterval {\n\t\t\t\t\tbreak\n\t\t\t\t}\n")},
		{"认领处另记截止", "k:", claimArm, insertAfter(claimArm, "\t\t\t\tpendingDeadline = time.Now().Add(8 * time.Second)\n")},
		{"认领处直接改窗口的截止", "k:", claimArm, insertAfter(claimArm, "\t\t\t\tbackfill.deadline = backfill.deadline.Add(8 * time.Second)\n")},
		{"补取块另按 lyricRetryWindow 比较", "k:", giveUpCase, insertAfter(giveUpCase,
			"\t\t\t\tif time.Since(sampleTime) < lyricRetryWindow {\n\t\t\t\t\tbreak\n\t\t\t\t}\n")},
		{"判恢复不重设补取窗口", "l:", resumeArm, replace(resumeArm, "_ = sampleTime")},
		{"认领处不开补取窗口", "l:", claimArm, replace(claimArm, "\t\t\t\t_ = time.Now()\n")},
		{"seek 的 playback_resume 不收进 if !isPaused（第 4 轮的写法）", "m:", seekResume, replace(seekResume, seekRound4)},
		{"收进的 if 条件改为 isPaused", "m:", seekGuard, replace(seekGuard, "\t\t\t\tif isPaused {\n")},
		{"收进的 if 条件改为 true", "m:", seekGuard, replace(seekGuard, "\t\t\t\tif true {\n")},
		{"收了发出、漏收 resumeEmitted = true", "m:", seekMark, replace(seekMark, "\t\t\t\t}\n\t\t\t\tresumeEmitted = true\n")},
		{"删去 if v.pausedSeek 块", "m:", pausedHead, func(s string) string {
			if b := pausedBlock(s); b != "" {
				return strings.Replace(s, b, "", 1)
			}
			return s
		}},
		{"if v.pausedSeek 挪到 if v.pause 之前", "m:", pausedHead, movePaused(pauseHead, false)},
		{"if v.pausedSeek 挪到入队失败的再试之后", "m:", retryRepeat, movePaused(retryRepeat, true)},
		{"if v.pausedSeek 体内直接发 playback_pause（m）", "m:", pausedBody,
			replace(pausedBody, pausedPos+"\t\t\tp.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: repausePos})\n")},
		{"if v.pausedSeek 体内直接发 playback_pause（d）", "d:", pausedBody,
			replace(pausedBody, pausedPos+"\t\t\tp.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: repausePos})\n")},
		{"if v.pausedSeek 体内补发之外另直接发 playback_pause", "m:", pausedBody,
			insertAfter(pausedBody, "\t\t\tp.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: repausePos})\n")},
		{"if v.pausedSeek 体内多发 status_update", "m:", pausedBody,
			insertAfter(pausedBody, "\t\t\tp.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: \"paused\", Detail: currentTitle})\n")},
		{"if v.pausedSeek 体内加 isPaused = true", "m:", pausedBody, insertAfter(pausedBody, "\t\t\tisPaused = true\n")},
		{"补发位置取 meta.ProgressMs", "m:", pausedPos, replace(pausedPos, "\t\t\trepausePos = float32(meta.ProgressMs) / 1000.0\n")},
		{"补发位置取 lastMemProgress", "m:", pausedPos, replace(pausedPos, "\t\t\trepausePos = float32(lastMemProgress) / 1000.0\n")},
		{"先补发、后记位置", "m:", pausedBody, replace(pausedBody, pausedEmit+pausedPos)},
		{"去掉变化分支（锚点）", "没找到", "if meta.ProgressMs != lastMemProgress {", replace("if meta.ProgressMs != lastMemProgress {", "if meta.ProgressMs != lastMemProgress+0 {")},
		{"去掉插值注释（锚点）", "没找到", interpHead, replace(interpHead, "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(src, c.needle) {
				t.Fatalf("qqmusic.go 里找不到改写依赖的原文 %q —— 本条自测失效", c.needle)
			}
			mutated := c.mutate(src)
			if mutated == src {
				t.Fatalf("改写没有生效 —— 本条自测失效")
			}
			probs := playStateWiringProblems(mutated)
			for _, p := range probs {
				if strings.HasPrefix(p, c.want) {
					return
				}
			}
			t.Fatalf("门禁没有以 %q 拦下这处改写；报出 %v", c.want, probs)
		})
	}
}
