package lyric

// 本文件只测 syncWindow.observe：CKSongsGraph 的解码位置是否与播放时间逐秒同步推进（设计 §4.2）。
// 逐拍喂入 (播放时间, 解码位置) 序列，看每一拍的判定。

import (
	"strings"
	"testing"
)

// syncTick 是一拍：播放时间 t 与解码位置 pos（秒）；bad 为真表示这一拍读不出或首 dword 不再是虚表。
type syncTick struct {
	t, pos float64
	bad    bool
}

// verdicts 依次喂入 ticks，返回每一拍的判定：'S' 为同步，'.' 为不同步。
func verdicts(ticks []syncTick, total, lastLyric float64) string {
	var w syncWindow
	var sb strings.Builder
	for _, tk := range ticks {
		if w.observe(!tk.bad, tk.t, tk.pos, total, lastLyric) {
			sb.WriteByte('S')
		} else {
			sb.WriteByte('.')
		}
	}
	return sb.String()
}

// at 按播放时间序列 ts 生成各拍，解码位置为 pos(t)。
func at(ts []float64, pos func(t float64) float64) []syncTick {
	out := make([]syncTick, len(ts))
	for i, t := range ts {
		out[i] = syncTick{t: t, pos: pos(t)}
	}
	return out
}

// seq 是从 t0 起、每拍推进 step 的 n 个播放时间。
func seq(t0, step float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = t0 + float64(i)*step
	}
	return out
}

// steps 是从 0 起、依次推进 ds 的播放时间。
func steps(ds ...float64) []float64 {
	out := []float64{0}
	for _, d := range ds {
		out = append(out, out[len(out)-1]+d)
	}
	return out
}

func offset(d float64) func(float64) float64 { return func(t float64) float64 { return t + d } }
func rate(r float64) func(float64) float64   { return func(t float64) float64 { return t * r } }

const rate48k = 192000.0 / 176400 // 48 kHz 的字节数按 44.1 kHz 换算：快 8.8%

// 设计 §6 列出的各条序列。total、lastLyric 不特别说明时取 200、150，不影响判定。
//
// 变异自证（在隔离克隆里逐条改过、确认红，再还原）：
//   - 极差改成只比较首尾两个采样（锚点与当前）：「首个采样读偏、抵消 8.8% 漂移」红。
//   - 去掉 Δt 下限（只要 dt <= 1.5）：「Δt 0.49 s」「同步之后暂停」「回退清空窗口」红。
//   - 去掉 Δt 上限：「Δt 1.51 s」「前跳、来源正常」红。
//   - syncMinSamples 改成 5：「窗口 5 个与 6 个采样」红。
//   - 覆盖推进的判断改成不含 5 s：「推进 5.0 s」「正常推进」等十余例红；syncSpan 改成 4.9：「推进 4.9 s」红。
//   - |d| 上限改成不含 1：「|d| = 1.0」「d = −1.0」红；去掉 |d| 判断：「|d| = 1.01」「d = −1.01」红
//     （「停住的残留对象」此时仍判不成，由极差挡住）。
//   - 极差判断改成不含 0.25：「极差 0.25 s」红；syncMaxSpread 改成 0.3：「极差 0.26 s」与两条「速率 5.1%」红。
//   - 丢旧采样时不看「丢弃后仍覆盖 5 s」：「Δt 0.5 s」红（TestSyncWindowCapsAt16 也红）。
//   - 去掉 pos <= total：「pos > total」「total = 0」红；去掉 lastLyric <= total、total <= 7200：各自那一例红。
func TestSyncWindowSequences(t *testing.T) {
	stale := func(float64) float64 { return 30 } // 停在 30 s 的残留对象
	misread := func(first float64, rest func(float64) float64) func(float64) float64 {
		return func(t float64) float64 {
			if t == 1 {
				return first
			}
			return rest(t)
		}
	}
	cases := []struct {
		name             string
		ticks            []syncTick
		total, lastLyric float64
		want             string
	}{
		{"正常推进（解码位置超前 0.05 s）", at(seq(1, 1, 8), offset(0.05)), 200, 150, ".....SSS"},

		{"停住的残留对象：播放经过它时", at(seq(25, 1, 11), stale), 200, 150, "..........."},

		{"速率 192000/176400", at(seq(0, 1, 21), rate(rate48k)), 200, 150, "....................."},
		{"速率偏快 4.9%", at(seq(0, 1, 11), rate(1.049)), 200, 150, ".....SSSSSS"},
		{"速率偏快 5.1%", at(seq(0, 1, 16), rate(1.051)), 200, 150, "................"},
		{"速率偏慢 4.9%", at(seq(0, 1, 11), rate(0.951)), 200, 150, ".....SSSSSS"},
		{"速率偏慢 5.1%", at(seq(0, 1, 16), rate(0.949)), 200, 150, "................"},

		{"首个采样读偏 0.35 s、其后正常", at(seq(1, 1, 7), misread(1.4, offset(0.05))), 200, 150, "......S"},
		// 48 kHz 来源，首个采样读偏得恰好让首尾两个采样的偏差相等（6 s 时漂移 0.53 s）。
		{"首个采样读偏、抵消 8.8% 漂移", at(seq(1, 1, 12), misread(1+6*(rate48k-1), rate(rate48k))), 200, 150, "............"},

		// 首拍 (t=1, pos=1.5) 之后前跳到 t=6：两个采样跨 5 s、偏差几乎相等，但相邻推进 5 s，窗口清空。
		{"首拍读偏后前跳、48 kHz 来源", append([]syncTick{{t: 1, pos: 1.5}}, at(seq(6, 1, 10), rate(rate48k))...), 200, 150, "..........."},
		{"前跳、来源正常：跳后重新攒满即同步", append([]syncTick{{t: 1, pos: 1.03}}, at(seq(6, 1, 7), offset(0.03))...), 200, 150, "......SS"},

		{"Δt 0.49 s", at(steps(1, 1, 1, 1, 0.49, 1), offset(0.05)), 200, 150, "......."},
		{"Δt 0.5 s", at(steps(1, 1, 1, 1, 0.5, 1), offset(0.05)), 200, 150, "......S"},
		{"Δt 1.5 s", at(steps(1, 1, 1, 1, 1.5, 1), offset(0.05)), 200, 150, ".....SS"},
		{"Δt 1.51 s", at(steps(1, 1, 1, 1, 1.51, 1), offset(0.05)), 200, 150, "......."},

		// 每拍推进 1.25 s：5 个采样已跨 5 s，仍要第 6 个。
		{"窗口 5 个与 6 个采样", at(seq(0, 1.25, 6), offset(0.05)), 200, 150, ".....S"},

		{"暂停中", at([]float64{10, 10, 10, 10, 10, 10, 10, 10}, offset(0.05)), 200, 150, "........"},
		{"同步之后暂停", at([]float64{1, 2, 3, 4, 5, 6, 6, 6, 6}, offset(0.05)), 200, 150, ".....S..."},
		{"回退清空窗口", at([]float64{1, 2, 3, 4, 5, 2, 3, 4, 5, 6, 7, 8}, offset(0.05)), 200, 150, "..........SS"},

		{"|d| = 1.0", at(seq(1, 1, 6), offset(1.0)), 200, 150, ".....S"},
		{"|d| = 1.01", at(seq(1, 1, 6), offset(1.01)), 200, 150, "......"},
		{"d = −1.0", at(seq(2, 1, 6), offset(-1.0)), 200, 150, ".....S"},
		{"d = −1.01", at(seq(2, 1, 6), offset(-1.01)), 200, 150, "......"},

		{"极差 0.25 s", at(seq(1, 1, 6), misread(1.25, offset(0))), 200, 150, ".....S"},
		{"极差 0.26 s", at(seq(1, 1, 6), misread(1.26, offset(0))), 200, 150, "......"},

		{"推进 4.9 s", at(steps(1, 1, 1, 1, 0.9), offset(0.05)), 200, 150, "......"},
		{"推进 5.0 s", at(steps(1, 1, 1, 1, 1), offset(0.05)), 200, 150, ".....S"},

		// 解码位置在 t=6 时为 6.5。
		{"pos = total", at(seq(1, 1, 6), offset(0.5)), 6.5, 0, ".....S"},
		{"pos > total", at(seq(1, 1, 6), offset(0.5)), 6.4, 0, "......"},
		{"total = lastLyric", at(seq(1, 1, 6), offset(0.05)), 200, 200, ".....S"},
		{"total < lastLyric", at(seq(1, 1, 6), offset(0.05)), 200, 200.5, "......"},
		{"total = 0", at(seq(1, 1, 6), offset(0.05)), 0, 0, "......"},
		{"total = 7200 s", at(seq(1, 1, 6), offset(0.05)), 7200, 150, ".....S"},
		{"total = 7200.1 s", at(seq(1, 1, 6), offset(0.05)), 7200.1, 150, "......"},
	}
	for _, c := range cases {
		if got := verdicts(c.ticks, c.total, c.lastLyric); got != c.want {
			t.Errorf("%s：判定 %s，want %s", c.name, got, c.want)
		}
	}
}

// 中途一拍读不出（或首 dword 不再是虚表）：窗口清空，之后从头攒满 6 个采样、5 s 推进。
//
// 变异自证：observe 不看 ok（读失败的那一拍照常进窗口），第 6 拍就判为同步而红。
func TestSyncWindowClearsOnBadRead(t *testing.T) {
	ticks := at(seq(1, 1, 10), offset(0.05))
	ticks[3].bad = true // t=4
	if got, want := verdicts(ticks, 200, 150), ".........S"; got != want {
		t.Errorf("中途读失败：判定 %s，want %s", got, want)
	}
}

// 窗口至多 16 个采样，超出时丢最旧的。Δt ≥ 0.5 s 时窗口至多 11 个采样（覆盖 5 s 之后就开始丢），
// 这个上限经 observe 的正常输入到不了，只能直接摆出 16 个采样的窗口来测。
//
// 变异自证：去掉上限，窗口留下 17 个而红；改成丢最新的，最旧的那个还在而红。
func TestSyncWindowCapsAt16(t *testing.T) {
	var w syncWindow
	for i := range syncMaxSamples {
		w.s = append(w.s, syncSample{t: float64(i) * 0.1})
	}
	w.observe(true, 2.5, 2.5, 200, 150)
	if len(w.s) != syncMaxSamples || w.s[0].t != 0.1 || w.s[len(w.s)-1].t != 2.5 {
		t.Fatalf("窗口 %d 个采样，首 %.2f、末 %.2f；want 16 个，首 0.10、末 2.50", len(w.s), w.s[0].t, w.s[len(w.s)-1].t)
	}
}
