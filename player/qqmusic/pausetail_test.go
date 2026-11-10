package qqmusic

// 本文件测已判暂停时的恢复判定 resumeGate（#171，doc/design/qqmusic-pause-tail-design.md），并经
// sessionreplay_test.go 的复述回放实测读数与设计 §3.2 各情形。
//
// QQ 音乐暂停后进度先停 1.85–3.0s，再补写一次 +93～116ms 才停住；旧规则（暂停中读数一变即判恢复）把补写
// 判成恢复，1s 后再判暂停。
//
// 变异自证（均实测）：回放夹具改回旧规则，TestReplayPauseTailMeasured 三例与 TestReplayPauseTailCases 七例全红，
// 除「补写之后 1s 内采到小步」一例是恢复位置落在补写读数上之外，得到的序列都含「pause resume pause」；observe 的 `step >= resumeStepMs` 改成 `>`，「首步恰为 500」红；窗口
// 判据 `<=` 改成 `<`，「恰 1s」红。runSession 在判出暂停或认领新歌时不清空待确认，全包仍绿：等价变异——
// 待确认时刻不晚于最后一次变化，新的暂停要再停满 pauseTimeoutSec 才判出，其后的变化必已过窗。

import (
	"testing"
	"time"
)

func TestResumeGate(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	type obs struct {
		step int64
		ms   int
		want bool
	}
	cases := []struct {
		name string
		obs  []obs
	}{
		{"首步恰为 500：立即恢复", []obs{{500, 0, true}}},
		{"首步 499：待确认，不恢复", []obs{{499, 0, false}}},
		{"实测补写 +116：待确认", []obs{{116, 0, false}}},
		{"实测恢复首步 +859：立即恢复", []obs{{859, 0, true}}},
		{"小步之后 1s 内再变：恢复", []obs{{116, 0, false}, {232, 330, true}}},
		{"小步之后恰 1s 再变：恢复（窗口含端点）", []obs{{116, 0, false}, {232, 1000, true}}},
		{"小步之后 1001ms 再变：仍待确认", []obs{{116, 0, false}, {232, 1001, false}}},
		{"过窗的待确认作废，以新一次为准", []obs{{116, 0, false}, {232, 1200, false}, {325, 1530, true}}},
		{"回退的小步：待确认", []obs{{-1500, 0, false}}},
		{"回退之后 1s 内再变：恢复", []obs{{-1500, 0, false}, {50, 50, true}}},
		{"待确认之后来大步：恢复", []obs{{93, 0, false}, {1045, 3084, true}}},
		{"判出恢复即清空：其后的小步重新待确认", []obs{{116, 0, false}, {232, 300, true}, {116, 500, false}}},
		{"大步判恢复也清空", []obs{{116, 0, false}, {1137, 3000, true}, {116, 3300, false}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var g resumeGate
			for i, o := range c.obs {
				if got := g.observe(o.step, at(o.ms), time.Second); got != o.want {
					t.Fatalf("第 %d 次 observe(step=%d, t=%dms) = %v，期望 %v", i, o.step, o.ms, got, o.want)
				}
			}
		})
	}

	t.Run("reset 清空待确认", func(t *testing.T) {
		var g resumeGate
		g.observe(116, at(0), time.Second)
		g.reset()
		if g.observe(232, at(300), time.Second) {
			t.Fatal("reset 之后的小步被判为恢复")
		}
	})
}

// sampled 把实测的变化点（会话内毫秒、读数）变成 run 的取值函数：返回不晚于 ms 的最后一个读数。
func sampled(name string, pts ...[2]int) func(ms int) (string, uint32) {
	return func(ms int) (string, uint32) {
		v := uint32(pts[0][1])
		for _, p := range pts {
			if p[0] > ms {
				break
			}
			v = uint32(p[1])
		}
		return name, v
	}
}

var tailSongs = map[string]replaySong{"Y": {durMs: 288613, lyrics: linesAt(0)}}

// 实测第 1 次暂停（02:58:23.945 起，《ユメセカイ》）：稳定步进到 55363，停 1905ms 后补写 +116，
// 3042ms 后恢复首步 +1137，其后恢复稳定步进。
var tailPause1 = [][2]int{
	{0, 53923}, {277, 54202}, {549, 54480}, {879, 54805}, {1207, 55130}, {1428, 55363},
	{3333, 55479}, // 补写
	{6375, 56616}, {6914, 57127}, {7381, 57615}, {7671, 57894},
}

func TestReplayPauseTailMeasured(t *testing.T) {
	t.Run("第 1 次暂停：补写 +116 后恢复首步 +1137", func(t *testing.T) {
		r := newReplay(0, tailSongs)
		r.run(0, 8000, 30, sampled("Y", tailPause1...))
		r.expectShape(t, "all lu0 pause resume")
		if e := r.find(t, "playback_pause", 0); !approx(e.pos, 55.363) {
			t.Errorf("playback_pause 的位置 %.3f，期望补写之前的读数 55.363", e.pos)
		}
		if e := r.find(t, "playback_resume", 0); !approx(e.pos, 56.616) {
			t.Errorf("playback_resume 的位置 %.3f，期望恢复首步的读数 56.616", e.pos)
		}
	})

	// 实测第 4 次暂停（02:58:56.978 起，《Change of Coast》）：补写 +93 在停 3014ms 处，恢复首步 +1045。
	t.Run("第 4 次暂停：补写 +93 后恢复首步 +1045", func(t *testing.T) {
		r := newReplay(0, tailSongs)
		r.run(0, 9000, 30, sampled("Y",
			[2]int{0, 51898}, [2]int{326, 52223}, [2]int{657, 52572}, [2]int{877, 52781}, [2]int{1202, 53106},
			[2]int{1532, 53431}, [2]int{1751, 53663},
			[2]int{4766, 53756}, // 补写
			[2]int{7850, 54801}, [2]int{8096, 55033}, [2]int{8421, 55358}))
		r.expectShape(t, "all lu0 pause resume")
		if e := r.find(t, "playback_resume", 0); !approx(e.pos, 54.801) {
			t.Errorf("playback_resume 的位置 %.3f，期望 54.801", e.pos)
		}
	})

	t.Run("补写之后一直停着：只判一次暂停", func(t *testing.T) {
		r := newReplay(0, tailSongs)
		r.run(0, 9000, 30, sampled("Y", tailPause1[:7]...))
		r.expectShape(t, "all lu0 pause")
	})
}

// playing 生成自 fromMs 起每 330ms 前进 325ms 的读数，直到 toMs（实测稳定步进的典型值）。
func playing(fromMs, fromProg, toMs int) [][2]int {
	var out [][2]int
	for ms, p := fromMs, fromProg; ms <= toMs; ms, p = ms+330, p+325 {
		out = append(out, [2]int{ms, p})
	}
	return out
}

// 设计 §3.2 各情形，以第 1 次暂停的前 7 个读数为底（55363 停住，3333ms 处补写到 55479），只换补写之后的读数。
func TestReplayPauseTailCases(t *testing.T) {
	base := tailPause1[:7]
	cases := []struct {
		name    string
		after   [][2]int
		want    string
		wantPos float32 // 第一个 playback_resume 的位置；0 为不查
	}{
		{"恢复首步 <500（未实测），330ms 后第二步：在第二步判恢复",
			append([][2]int{{6375, 55711}}, playing(6705, 56036, 9000)...), "all lu0 pause resume", 56.036},
		{"补写之后 1s 内采到小步：与补写构成两步，在该步判恢复",
			append([][2]int{{4000, 55711}}, playing(4330, 56036, 9000)...), "all lu0 pause resume", 55.711},
		{"补写之后超过 1s 才采到小步：该步待确认，下一步判恢复",
			append([][2]int{{4600, 55711}}, playing(4900, 56036, 9000)...), "all lu0 pause resume", 56.036},
		{"补写之后单次小步、其后不再变：保持暂停",
			[][2]int{{5000, 55711}}, "all lu0 pause", 0},
		{"暂停中回退 1.5s（不构成 seek）、其后不再变：保持暂停",
			[][2]int{{5000, 53979}}, "all lu0 pause", 0},
		{"暂停中单次 ≥500 的前进（快速恢复又暂停）：判恢复，1s 后再判暂停（同现行）",
			[][2]int{{5000, 56338}}, "all lu0 pause resume pause", 56.338},
		{"暂停中回跳 3s（seek）：立即判恢复，行号随位置重发（同现行）",
			playing(5000, 52479, 9000), "all lu0 pause resume lu0", 52.479},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReplay(0, tailSongs)
			r.run(0, 9000, 30, sampled("Y", append(append([][2]int{}, base...), c.after...)...))
			r.expectShape(t, c.want)
			if c.wantPos != 0 {
				if e := r.find(t, "playback_resume", 0); !approx(e.pos, c.wantPos) {
					t.Errorf("playback_resume 的位置 %.3f，期望 %.3f", e.pos, c.wantPos)
				}
			}
		})
	}
}

// TestReplayStablePlaybackNoPause 实测稳定播放的间隔（211–393ms）与恢复后第二步的最长间隔 699ms 都不足 1s，
// 不判暂停。
func TestReplayStablePlaybackNoPause(t *testing.T) {
	r := newReplay(0, tailSongs)
	r.run(0, 3000, 30, sampled("Y",
		[2]int{0, 62666}, [2]int{699, 63363}, [2]int{1092, 63619}, [2]int{1303, 63828}, [2]int{1660, 64185},
		[2]int{1993, 64510}, [2]int{2326, 64835}, [2]int{2660, 65160}, [2]int{2993, 65485}))
	r.expectShape(t, "all lu0")
}
