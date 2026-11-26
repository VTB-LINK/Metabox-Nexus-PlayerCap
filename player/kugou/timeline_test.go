package kugou

// 本文件专测酷狗歌词时间轴修正（#251，设计 §6.5）：shiftTimeline 按酷狗侧 lyricOffsetMs（正数提前、
// 负数延后、下限钳零）同步平移各行 Time 与逐字 Detailed / RomaDetailed 的 Timestamp，不动 Duration 与
// PlayTime；cloneLines 深拷贝行与逐字/音译词切片，保证 rawLines 不受钳零污染；buildDisplayLyrics 先
// 深拷贝与 shiftTimeline、再叠加 PlayerCap 配置的 offsetSec。
//
// 变异自证（均实测变红后还原）：buildDisplayLyrics 去掉 cloneLines 直接原地修改 raw →
// TestShiftTimelineAndCloneLines/cloneLines深拷贝与大额正偏移钳零后伴唱恢复1s 红。

import (
	"math"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

func approxEq(a, b float32) bool {
	return math.Abs(float64(a-b)) < 1e-4
}

func sampleTimedLines() []klyric.Line {
	return []klyric.Line{
		{
			Index:     0,
			Time:      1.0,
			Text:      "首行",
			RomaText:  "shou hang",
			RomaWords: []string{"shou", "hang"},
			Detailed: player.LyricTextDetailed{
				Timestamp: 1.0,
				PlayTime:  1.0,
				Duration:  1.2,
				Words: []player.LyricTextDetailedWord{
					{Text: "首", Timestamp: 1.0, PlayTime: 1.0, Duration: 0.5},
					{Text: "行", Timestamp: 1.5, PlayTime: 1.5, Duration: 0.7},
				},
			},
			RomaDetailed: player.LyricTextDetailed{
				Timestamp: 1.0,
				PlayTime:  1.0,
				Duration:  1.2,
				Words: []player.LyricTextDetailedWord{
					{Text: "shou", Timestamp: 1.0, PlayTime: 1.0, Duration: 0.5},
					{Text: "hang", Timestamp: 1.5, PlayTime: 1.5, Duration: 0.7},
				},
			},
		},
		{
			Index:     1,
			Time:      5.0,
			Text:      "次行",
			RomaText:  "ci hang",
			RomaWords: []string{"ci", "hang"},
			Detailed: player.LyricTextDetailed{
				Timestamp: 5.0,
				PlayTime:  5.0,
				Duration:  1.0,
				Words: []player.LyricTextDetailedWord{
					{Text: "次", Timestamp: 5.0, PlayTime: 5.0, Duration: 0.4},
					{Text: "行", Timestamp: 5.4, PlayTime: 5.4, Duration: 0.6},
				},
			},
			RomaDetailed: player.LyricTextDetailed{
				Timestamp: 5.0,
				PlayTime:  5.0,
				Duration:  1.0,
				Words: []player.LyricTextDetailedWord{
					{Text: "ci", Timestamp: 5.0, PlayTime: 5.0, Duration: 0.4},
					{Text: "hang", Timestamp: 5.4, PlayTime: 5.4, Duration: 0.6},
				},
			},
		},
		{
			Index: 2,
			Time:  8.0,
			Text:  "纯行级",
		},
	}
}

func TestShiftTimelineAndCloneLines(t *testing.T) {
	t.Run("正偏移提前且下限钳零_不动Duration与PlayTime", func(t *testing.T) {
		lines := sampleTimedLines()
		shiftTimeline(lines, 1500)

		if !approxEq(lines[0].Time, 0) {
			t.Fatalf("首行 Time = %.3f，want 0", lines[0].Time)
		}
		if !approxEq(lines[0].Detailed.Timestamp, 0) || !approxEq(lines[0].Detailed.Words[0].Timestamp, 0) || !approxEq(lines[0].Detailed.Words[1].Timestamp, 0) {
			t.Fatalf("首行 Detailed 时间未正确钳零：%+v", lines[0].Detailed)
		}
		if !approxEq(lines[0].RomaDetailed.Timestamp, 0) || !approxEq(lines[0].RomaDetailed.Words[0].Timestamp, 0) || !approxEq(lines[0].RomaDetailed.Words[1].Timestamp, 0) {
			t.Fatalf("首行 RomaDetailed 时间未正确钳零：%+v", lines[0].RomaDetailed)
		}
		if !approxEq(lines[0].Detailed.Duration, 1.2) || !approxEq(lines[0].Detailed.PlayTime, 1.0) ||
			!approxEq(lines[0].Detailed.Words[0].Duration, 0.5) || !approxEq(lines[0].Detailed.Words[0].PlayTime, 1.0) {
			t.Fatalf("shiftTimeline 不应改动 Duration 或 PlayTime：%+v", lines[0].Detailed)
		}

		if !approxEq(lines[1].Time, 3.5) {
			t.Fatalf("次行 Time = %.3f，want 3.5", lines[1].Time)
		}
		if !approxEq(lines[1].Detailed.Timestamp, 3.5) || !approxEq(lines[1].Detailed.Words[0].Timestamp, 3.5) || !approxEq(lines[1].Detailed.Words[1].Timestamp, 3.9) {
			t.Fatalf("次行 Detailed 平移错误：%+v", lines[1].Detailed)
		}
		if !approxEq(lines[1].RomaDetailed.Timestamp, 3.5) || !approxEq(lines[1].RomaDetailed.Words[0].Timestamp, 3.5) || !approxEq(lines[1].RomaDetailed.Words[1].Timestamp, 3.9) {
			t.Fatalf("次行 RomaDetailed 平移错误：%+v", lines[1].RomaDetailed)
		}
		if !approxEq(lines[2].Time, 6.5) || lines[2].Detailed.Timestamp != 0 || lines[2].RomaDetailed.Timestamp != 0 {
			t.Fatalf("纯行级行平移错误：%+v", lines[2])
		}
	})

	t.Run("负偏移延后同步平移", func(t *testing.T) {
		lines := sampleTimedLines()
		shiftTimeline(lines, -1000)

		if !approxEq(lines[0].Time, 2.0) || !approxEq(lines[0].Detailed.Timestamp, 2.0) ||
			!approxEq(lines[0].Detailed.Words[0].Timestamp, 2.0) || !approxEq(lines[0].Detailed.Words[1].Timestamp, 2.5) {
			t.Fatalf("首行负偏移平移错误：%+v", lines[0])
		}
		if !approxEq(lines[0].RomaDetailed.Timestamp, 2.0) || !approxEq(lines[0].RomaDetailed.Words[1].Timestamp, 2.5) {
			t.Fatalf("首行 RomaDetailed 负偏移平移错误：%+v", lines[0].RomaDetailed)
		}
		if !approxEq(lines[1].Time, 6.0) || !approxEq(lines[1].Detailed.Words[1].Timestamp, 6.4) {
			t.Fatalf("次行负偏移平移错误：%+v", lines[1])
		}
	})

	t.Run("零偏移保持原样", func(t *testing.T) {
		lines := sampleTimedLines()
		shiftTimeline(lines, 0)
		if !approxEq(lines[0].Time, 1.0) || !approxEq(lines[1].Time, 5.0) {
			t.Fatalf("零偏移不应改动时间：%+v", lines)
		}
	})

	t.Run("cloneLines深拷贝与大额正偏移钳零后伴唱恢复1s", func(t *testing.T) {
		raw := sampleTimedLines()

		// 先按 +2000ms（提前 2s，首行 1.0s 钳零）与 kugou-offset -0.5s 构建显示歌词。
		clamped := buildDisplayLyrics(raw, 2000, -0.5)
		if len(clamped) != len(raw) {
			t.Fatalf("显示行数 = %d，want %d", len(clamped), len(raw))
		}
		if !approxEq(clamped[0].Time, 0) || !approxEq(clamped[0].Detailed.Timestamp, 0) || !approxEq(clamped[0].Detailed.PlayTime, 0) {
			t.Fatalf("首行 +2000ms 应钳至 0：%+v", clamped[0])
		}
		// 次行 5.0s - 2.0s = 3.0s，叠加 offsetSec = -0.5s 后 PlayTime = 2.5s。
		if !approxEq(clamped[1].Time, 3.0) || !approxEq(clamped[1].Detailed.Timestamp, 3.0) ||
			!approxEq(clamped[1].Detailed.PlayTime, 2.5) || !approxEq(clamped[1].Detailed.Words[1].Timestamp, 3.4) ||
			!approxEq(clamped[1].Detailed.Words[1].PlayTime, 2.9) || !approxEq(clamped[1].RomaDetailed.Words[1].PlayTime, 2.9) {
			t.Fatalf("次行偏移叠加错误：%+v", clamped[1])
		}

		// 篡改副本中的切片元素，确认绝不影响 rawLines。
		clamped[0].Detailed.Words[0].Text = "改写"
		clamped[0].RomaDetailed.Words[0].Text = "gaixie"
		clamped[0].RomaWords[0] = "gaixie"
		if raw[0].Time != 1.0 || raw[0].Detailed.Words[0].Text != "首" || raw[0].Detailed.Words[0].Timestamp != 1.0 ||
			raw[0].RomaDetailed.Words[0].Text != "shou" || raw[0].RomaWords[0] != "shou" {
			t.Fatalf("rawLines 被副本污染：%+v", raw[0])
		}

		// 模拟切入同名伴唱歌曲（新偏移 0ms）复用 rawLines：首行必须恢复为 1.0s。
		reused := buildDisplayLyrics(raw, 0, -0.5)
		if !approxEq(reused[0].Time, 1.0) || !approxEq(reused[0].Detailed.Timestamp, 1.0) ||
			!approxEq(reused[0].Detailed.PlayTime, 0.5) || !approxEq(reused[0].Detailed.Words[0].Timestamp, 1.0) ||
			!approxEq(reused[0].Detailed.Words[0].PlayTime, 0.5) {
			t.Fatalf("同名伴唱复用 0ms 偏移未恢复首行 1.0s：%+v", reused[0])
		}

		if got := buildDisplayLyrics(nil, 1500, -0.5); got != nil {
			t.Fatalf("空 rawLines 应返回 nil，实得 %+v", got)
		}
	})
}
