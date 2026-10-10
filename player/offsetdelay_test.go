package player

// 本文件只测 offset 的方向（issue #54）：它是延迟量，正值 = 延后，负值 = 提前。符号只在两处生效——
// AdjustLyricPlayTime 把它加到歌词时间轴上，LyricMatchTime 把它从播放位置里减掉——两处必须相反。
//
// 「各播放器都经由这两个函数、不自行对 offset 做加减」由 offsetsign_lint_test.go 守。

import (
	"math"
	"testing"
)

// 播出时间 = 时间戳 + offset，下限 0。
//
// 变异自证（隔离的 worktree，实测）：AdjustLyricPlayTime 的符号改回 timestamp - offsetSec → 除 offset
// 为 0 的两例外均红；去掉下限 → 「提前量大于时间戳」「提前量恰等于时间戳的下一个可表示值」两例红。
func TestAdjustLyricPlayTimeIsDelay(t *testing.T) {
	cases := []struct {
		name              string
		timestamp, offset float32
		want              float32
	}{
		{"正值延后", 10, 0.5, 10.5},
		{"负值提前", 10, -0.5, 9.5},
		{"零不变", 10, 0, 10},
		{"时间戳为 0、零 offset", 0, 0, 0},
		{"时间戳为 0、正值延后", 0, 0.2, 0.2},
		{"提前量大于时间戳，钳到 0", 0.2, -0.5, 0},
		{"提前量恰等于时间戳", 0.5, -0.5, 0},
		{"提前量恰等于时间戳的下一个可表示值", 0.5, -math.Nextafter32(0.5, 1), 0},
		{"延后不设上限", 200, 3600, 3800},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AdjustLyricPlayTime(c.timestamp, c.offset); got != c.want {
				t.Errorf("AdjustLyricPlayTime(%v, %v) = %v，期望 %v", c.timestamp, c.offset, got, c.want)
			}
		})
	}
}

// 行匹配用的时刻 = 位置 - offset：延后 0.5s 时，位置走到 10.5s 才够得着时间戳 10s 的行。不设下限，
// 负值照原样返回（任何行的时间戳都不小于 0，匹配不到任何行）。
//
// 变异自证（隔离的 worktree，实测）：LyricMatchTime 的符号改回 position + offsetSec → 除 offset 为 0
// 的一例外均红；applemusic 的 sessionoffset_test.go 同红。
func TestLyricMatchTimeSubtractsOffset(t *testing.T) {
	cases := []struct {
		name             string
		position, offset float32
		want             float32
	}{
		{"正值延后：位置要多走 offset", 10.5, 0.5, 10},
		{"负值提前：位置少走 offset", 9.5, -0.5, 10},
		{"零不变", 10, 0, 10},
		{"延后量大于位置，结果为负、不钳", 0.2, 0.5, float32(0.2) - float32(0.5)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LyricMatchTime(c.position, c.offset); got != c.want {
				t.Errorf("LyricMatchTime(%v, %v) = %v，期望 %v", c.position, c.offset, got, c.want)
			}
		})
	}
}

// 一行的 lyric_update 在位置走到它的 play_time 时发出：行匹配的判据「LyricMatchTime(位置) >= 时间戳」
// 在位置等于 play_time 的前后翻转。取的数都能被 float32 精确表示，不受舍入影响。
//
// 变异自证（隔离的 worktree，实测）：两个函数任一个的符号改回 → 红（两个同时改回即旧语义，自洽，不红；
// 那种情形由上面两条各自拦）。
func TestLyricMatchAgreesWithPlayTime(t *testing.T) {
	const lineTime = float32(64)
	for _, offset := range []float32{0.5, -0.5, 0, 16, -16} {
		playTime := AdjustLyricPlayTime(lineTime, offset)
		for _, c := range []struct {
			position float32
			matched  bool
		}{
			{playTime - 0.25, false},
			{playTime, true},
			{playTime + 0.25, true},
		} {
			if got := LyricMatchTime(c.position, offset) >= lineTime; got != c.matched {
				t.Errorf("offset=%v：位置 %v 时行（时间戳 %v、play_time %v）匹配 = %v，期望 %v",
					offset, c.position, lineTime, playTime, got, c.matched)
			}
		}
	}
}

// 对外事件的取值逐位不变：旧语义下配置值为 A 毫秒（提前量）时的结果，与新语义下配置值为 -A 毫秒时相同。
// 旧算式照 3.x 的写法复述在这里：play_time = max(0, t - A)，行匹配用的时刻 = 位置 + A。毫秒到秒的换算
// 与各播放器相同（float32(毫秒) / 1000）。A 取默认值 200、它的相反数、0、大于时间戳的值与模板里各播放器的值。
//
// 变异自证（隔离的 worktree，实测）：AdjustLyricPlayTime 或 LyricMatchTime 的符号改回 → 红。
func TestOffsetDelayKeepsEventValuesBitIdentical(t *testing.T) {
	oldPlayTime := func(timestamp, advanceSec float32) float32 {
		adjusted := timestamp - advanceSec
		if adjusted < 0 {
			return 0
		}
		return adjusted
	}
	oldMatchTime := func(position, advanceSec float32) float32 { return position + advanceSec }

	timestamps := []float32{0, 0.01, 0.2, 0.33, 1, 4.75, 10.92, 14.58, 169.999, 170, 3599.99}
	for _, oldMs := range []int{200, -200, 0, 5000, 500, 400, 430, 340, 1, -1} {
		oldSec, newSec := float32(oldMs)/1000, float32(-oldMs)/1000
		for _, ts := range timestamps {
			want, got := oldPlayTime(ts, oldSec), AdjustLyricPlayTime(ts, newSec)
			if math.Float32bits(got) != math.Float32bits(want) {
				t.Errorf("旧 offset=%dms、时间戳 %v：play_time 旧 %v(%#x)，新 %v(%#x)",
					oldMs, ts, want, math.Float32bits(want), got, math.Float32bits(got))
			}
			want, got = oldMatchTime(ts, oldSec), LyricMatchTime(ts, newSec)
			if math.Float32bits(got) != math.Float32bits(want) {
				t.Errorf("旧 offset=%dms、位置 %v：行匹配用的时刻 旧 %v(%#x)，新 %v(%#x)",
					oldMs, ts, want, math.Float32bits(want), got, math.Float32bits(got))
			}
		}
	}
}
