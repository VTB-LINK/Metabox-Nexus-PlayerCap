package player

// 本文件只测 LyricPlayTime —— play_time 的唯一公式。
//
// 它守的契约：play_time 是「这一行何时该播出」（displayStart - offset），**不是**「当前播放
// 到哪」。四家播放器曾各写各的：cloudmusic 走公式，qqmusic/wesing/kugou 直接发实时插值位置。
// 两者在常规行上只差一个轮询滞后（毫秒级），长期看不出差别——正因如此，这里必须把语义钉死，
// 否则下次又会有人「顺手」把它改回实时位置而所有测试照样绿。

import "testing"

// 基本契约：无逐字时 play_time = timestamp - offset（用户给的定义原文：
// timestamp=170.0、offset=500ms → play_time=169.5）。
func TestLyricPlayTimeIsTimestampMinusOffset(t *testing.T) {
	got := LyricPlayTime(170.0, LyricTextDetailed{}, 0.5)
	if got != 169.5 {
		t.Fatalf("LyricPlayTime(170.0, {}, 0.5) = %v, want 169.5", got)
	}
}

// 逐字**晚于**行时间戳（网易云 YRC 的常态，实测晚 90~840ms）→ 仍以行时间戳为准，
// 绝不能改用逐字时间，否则整首歌的行都会延后上屏。
func TestLyricPlayTimeIgnoresLaterDetailed(t *testing.T) {
	detailed := LyricTextDetailed{
		Timestamp: 5.04, // 晚于行的 4.75
		Words:     []LyricTextDetailedWord{{Timestamp: 5.04, Duration: 0.66, Text: "You"}},
	}
	got := LyricPlayTime(4.75, detailed, 0.5)
	if got != 4.25 {
		t.Fatalf("LyricPlayTime = %v, want 4.25（逐字晚于行时应以行时间戳为准）", got)
	}
}

// 逐字**早于**行时间戳 → 取逐字起点，让行在第一个字发声时上屏。
// 取自实测：网易云 "A-A-All the details..."，LRC 标 14.58s 而 YRC 首字 10.92s。
//
// 变异自证：把 LyricDisplayStart 的 `detailed.Timestamp < lineTime` 判断删掉，本例变红。
func TestLyricPlayTimeUsesEarlierDetailedStart(t *testing.T) {
	detailed := LyricTextDetailed{
		Timestamp: 10.92,
		Words:     []LyricTextDetailedWord{{Timestamp: 10.92, Duration: 0.03, Text: "A"}},
	}
	got := LyricPlayTime(14.58, detailed, 0.5)
	if want := float32(10.92 - 0.5); got != want {
		t.Fatalf("LyricPlayTime = %v, want %v（逐字早于行时应提前到第一个字发声的时刻）", got, want)
	}
}

// 空 Words 的 detailed 不算逐字：即使 Timestamp 更早也不得生效，
// 否则零值结构体会把 play_time 拽到 0。
func TestLyricPlayTimeIgnoresDetailedWithoutWords(t *testing.T) {
	detailed := LyricTextDetailed{Timestamp: 0} // 无 Words
	got := LyricPlayTime(14.58, detailed, 0.5)
	if want := float32(14.58 - 0.5); got != want {
		t.Fatalf("LyricPlayTime = %v, want %v；空 Words 不构成逐字", got, want)
	}
}

// offset 大于时间戳时钳到 0，不得为负。
func TestLyricPlayTimeNeverNegative(t *testing.T) {
	if got := LyricPlayTime(0.2, LyricTextDetailed{}, 0.5); got != 0 {
		t.Fatalf("LyricPlayTime = %v, want 0", got)
	}
}

// 承重不变量：play_time / position / progress 是三个**独立**的量，绝不能互相串。
//
// 用例刻意让三者的正确答案互不相同、也不等于任何单一输入：
//
//	play_time = 170.0 - 0.5      = 169.5   （歌词时间轴，本行何时上屏）
//	position  = 100.0            = 100.0   （实时时钟，整曲播到哪，playPos 原值、不减 offset）
//	progress  = 100.0 / 200.0    = 0.5     （position / 总时长）
//
// 于是「把实时位置塞进 play_time」（旧的 qqmusic/wesing/kugou 写法，得 100.0）、
// 「用 timestamp 算 progress」（得 0.85）、或漏掉 position（得 0）都会立刻变红。真机上这些
// 错法只差一个轮询滞后、毫秒级，肉眼与线上数据都看不出来——这正是需要合成用例把它钉死的原因。
//
// 变异自证：把 BuildLyricUpdate 里的 PlayTime 改成 playPos、删掉 Position: playPos、
// 或把 Progress 改成 ClampProgress(lineTime, duration)，本例分别变红。
func TestBuildLyricUpdateKeepsPlayTimeAndProgressIndependent(t *testing.T) {
	u := BuildLyricUpdate(3, 170.0, "text", "sub", "roma", LyricTextDetailed{}, 0.5, 100.0, 200.0)

	if u.PlayTime != 169.5 {
		t.Fatalf("PlayTime = %v, want 169.5；play_time 是「本行何时该播出」"+
			"(timestamp-offset)，不是当前播放位置(playPos=100)", u.PlayTime)
	}
	if u.Position != 100.0 {
		t.Fatalf("Position = %v, want 100.0；position 是整曲实时播放位置(playPos)，"+
			"与 play_time(169.5) 语义相反、不减 offset", u.Position)
	}
	if u.Progress != 0.5 {
		t.Fatalf("Progress = %v, want 0.5；progress 是「整首播到哪」(position/duration)，"+
			"不该用 timestamp 算(那会得 0.85)", u.Progress)
	}
	if u.Timestamp != 170.0 {
		t.Fatalf("Timestamp = %v, want 170.0；timestamp 必须原样透传平台值", u.Timestamp)
	}
	if u.Text != "text" || u.SubText != "sub" {
		t.Fatalf("Text/SubText 透传错误: %q / %q", u.Text, u.SubText)
	}
	// roma_text（音译）与 sub_text（翻译）是两条独立的轨，各自原样透传、互不串。
	if u.RomaText != "roma" {
		t.Fatalf("RomaText = %q, want \"roma\"；音译必须原样透传、且不与 sub_text 混用", u.RomaText)
	}
}

// lyric_update 与 all_lyrics 里同一行的 play_time 必须逐字节一致（含逐字早于行的情形）。
func TestBuildLyricUpdateAgreesWithBuildLyricLine(t *testing.T) {
	detailed := LyricTextDetailed{
		Timestamp: 10.92,
		Words:     []LyricTextDetailedWord{{Timestamp: 10.92, Text: "A"}},
	}
	line := BuildLyricLine(1, 14.58, "t", "s", "", detailed, 0.5)
	upd := BuildLyricUpdate(1, 14.58, "t", "s", "", detailed, 0.5, 99.0, 200.0)
	if line.PlayTime != upd.PlayTime {
		t.Fatalf("all_lyrics 的 play_time (%v) != lyric_update 的 play_time (%v)",
			line.PlayTime, upd.PlayTime)
	}
}

// 承重不变量：同一行经 BuildLyricLine（all_lyrics 用）与经 LyricPlayTime（lyric_update 用）
// 必须得到**逐字节相同**的 play_time。这两条路分叉，就意味着同一行在两个事件里报两个时间。
//
// 变异自证：把 BuildLyricLine 里的 LyricPlayTime 换回 AdjustLyricPlayTime(lineTime, offset)
// （即绕过 displayStart），本例变红。
func TestBuildLyricLineAndLyricPlayTimeAgree(t *testing.T) {
	cases := []struct {
		name     string
		lineTime float32
		detailed LyricTextDetailed
	}{
		{"无逐字", 170.0, LyricTextDetailed{}},
		{"逐字晚于行", 4.75, LyricTextDetailed{
			Timestamp: 5.04,
			Words:     []LyricTextDetailedWord{{Timestamp: 5.04, Text: "You"}},
		}},
		{"逐字早于行", 14.58, LyricTextDetailed{
			Timestamp: 10.92,
			Words:     []LyricTextDetailedWord{{Timestamp: 10.92, Text: "A"}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			built := BuildLyricLine(0, c.lineTime, "t", "s", "", c.detailed, 0.5)
			direct := LyricPlayTime(c.lineTime, c.detailed, 0.5)
			if built.PlayTime != direct {
				t.Fatalf("all_lyrics 的 play_time (%v) != lyric_update 的 play_time (%v)；"+
					"同一行在两个事件里报了两个时间", built.PlayTime, direct)
			}
			// timestamp 必须始终是平台原始值，不受 offset/displayStart 影响
			if built.Timestamp != c.lineTime {
				t.Fatalf("Timestamp = %v, want %v；timestamp 必须原样透传平台值",
					built.Timestamp, c.lineTime)
			}
		})
	}
}
