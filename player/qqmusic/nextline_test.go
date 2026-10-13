package qqmusic

// 本文件测 nextLine（歌词行匹配：行号只往前走，lastLineIdx 为 -1 时不限），并经 sessionreplay_test.go
// 的复述回放 issue #96 的暂停序列与它的反例。
//
// qqmusic 判暂停要等计时器停滞 pauseTimeoutSec，其间插值照走、可能推过下一行的开头；判出暂停时位置
// 退回锚点。旧写法（行号与上一次不同即发）按退回的位置补发上一行，恢复后再推回来。
//
// 变异均在本 worktree 里用编辑注入、跑完即还原，结果记在各用例的注释里（均实测）。

import (
	"math"
	"testing"
)

// 变异自证：判据改回旧写法 `idx >= 0 && idx != last`，「位置退回上一行」红；改成 `idx >= last`，
// 「没有歌词」「还没到第一行」「行首之前一点」「仍是已发出的那一行」「NaN」五例红；行首判定由 `>=` 改成
// `>`，「last 为 -1：第一行照发」「恰在行首」「往前一行」三例红（回放用例在这一处全绿）。
func TestNextLine(t *testing.T) {
	lines := linesAt(0, 10, 20, 30)
	cases := []struct {
		name    string
		lines   []lyricLine
		t       float32
		last    int
		wantIdx int
		wantOK  bool
	}{
		{"没有歌词", nil, 15, -1, -1, false},
		{"还没到第一行", linesAt(5, 10), 4.9, -1, -1, false},
		{"刚换歌、跳转或换了歌词（last 为 -1）：第一行照发", lines, 0, -1, 0, true},
		{"last 为 -1：落在中间某行也照发（跳转后行号跟着位置走）", lines, 25, -1, 2, true},
		{"恰在行首：算这一行", lines, 20, 1, 2, true},
		{"行首之前一点：仍是上一行，不发", lines, 19.999, 1, 1, false},
		{"仍是已发出的那一行：不发", lines, 25, 2, 2, false},
		{"往前一行：发", lines, 30, 2, 3, true},
		{"一拍跨过几行：发最后到达的那一行", lines, 35, 0, 3, true},
		{"位置退回上一行（暂停判出时退回锚点）：不发", lines, 19, 2, 1, false},
		{"位置退回到第一行之前：不发", lines, -1, 2, -1, false},
		{"匹配时刻是 NaN：不发", lines, float32(math.NaN()), -1, -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			idx, ok := nextLine(c.lines, c.t, c.last)
			if idx != c.wantIdx || ok != c.wantOK {
				t.Fatalf("nextLine(t=%v, last=%d) = (%d, %v)，期望 (%d, %v)", c.t, c.last, idx, ok, c.wantIdx, c.wantOK)
			}
		})
	}
}

// TestReplayIssue96Pause 回放 issue #96 录音 01-qqmusic 的暂停序列（《枝江》，时长 195s，offset -400ms，
// 每 30ms 一拍）：计时器停在 43.886s，插值在 44.549s 推过第 8 行（44.949 - 0.4），判出暂停时位置退回
// 43.886s。录音里的事件是「index 8 → 暂停时 index 7 → 恢复后 index 8」，修正后是「index 8 → 暂停 →
// 恢复 → index 9」。playback_pause 与 playback_resume 的位置照旧是计时器读数。
//
// 变异自证：nextLine 的判据改回旧写法，本例红，得到 `all lu7 lu8 pause lu7 resume lu8 lu9`，即录音里的
// 序列；改成 `idx >= last`，本例红。
func TestReplayIssue96Pause(t *testing.T) {
	songs := map[string]replaySong{"枝江": {durMs: 195000, lyrics: linesAt(
		0, 16.552, 20.534, 24.462, 27.614, 30.742, 35.209, 38.326, 44.949, 48.366, 52, 56)}}
	r := newReplay(-0.4, songs)
	r.run(0, 9300, 30, func(ms int) (string, uint32) {
		switch {
		case ms < 900:
			return "枝江", 43000 + uint32(ms)
		case ms < 4950:
			return "枝江", 43886 // 已暂停，计时器停着
		default:
			return "枝江", 43886 + uint32(ms-4950)
		}
	})
	r.expectShape(t, "all lu7 lu8 pause resume lu9")
	if e := r.find(t, "lyric_update", 1); !approx(e.pos, 44.576) {
		t.Errorf("第 8 行的位置 %.3f，期望 44.576（插值推过行首的那一拍）", e.pos)
	}
	if e := r.find(t, "playback_pause", 0); !approx(e.pos, 43.886) {
		t.Errorf("playback_pause 的位置 %.3f，期望计时器停下的读数 43.886", e.pos)
	}
	if e := r.find(t, "playback_resume", 0); !approx(e.pos, 43.916) {
		t.Errorf("playback_resume 的位置 %.3f，期望恢复后的读数 43.916", e.pos)
	}
}

// 以下反例用一首合成的歌：时长 200s，第 0～4 行起于 0、10、20、30、40s，offset 0，每 50ms 一拍。
var syntheticSongs = map[string]replaySong{"S": {durMs: 200000, lyrics: linesAt(0, 10, 20, 30, 40)}}

// TestReplayPausePositionVsLine 暂停位置相对当前行的三种情形：在当前行的行首之前（插值已推过这一行的
// 开头，即 issue 的情形）、恰在行首、在行内。三者都只发 playback_pause 与 playback_resume，不补发
// 上一行、恢复后也不重发这一行。
//
// 变异自证：nextLine 的判据改回旧写法，「行首之前」红（暂停时补发第 1 行、恢复后重发第 2 行），另两例
// 在旧写法下同样成立，是不该被改动波及的反例；改成 `idx >= last`，三例均红。
func TestReplayPausePositionVsLine(t *testing.T) {
	cases := []struct {
		name     string
		freezeMs uint32 // 计时器停下的读数；进度自 19000 起与时间同步走
		resumeAt int    // 恢复的时刻
	}{
		{"暂停位置在当前行的行首之前", 19600, 3000},
		{"暂停位置恰在行首", 20000, 3000},
		{"暂停位置在行内", 22000, 6000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReplay(0, syntheticSongs)
			freezeAt := int(c.freezeMs - 19000)
			r.run(0, c.resumeAt+11000, 50, func(ms int) (string, uint32) {
				switch {
				case ms < freezeAt:
					return "S", 19000 + uint32(ms)
				case ms < c.resumeAt:
					return "S", c.freezeMs
				default:
					return "S", c.freezeMs + uint32(ms-c.resumeAt)
				}
			})
			r.expectShape(t, "all lu1 lu2 pause resume lu3")
			if e := r.find(t, "playback_pause", 0); !approx(e.pos, float32(c.freezeMs)/1000) {
				t.Errorf("playback_pause 的位置 %.3f，期望 %.3f", e.pos, float32(c.freezeMs)/1000)
			}
		})
	}
}

// TestReplaySeekBackWhilePaused 暂停中回跳超过 2s：判为 seek，行号跟着位置回到第 0 行。行号只往前走
// 不得挡住跳转。
//
// 变异自证：注入在回放里——seek 分支不把 lastLineIdx 置 -1，本例与 TestReplaySeekOnPauseTick 红（回跳后
// 不再发任何行）；nextLine 的判据改回旧写法，本例红（判出暂停时先补发第 1 行）。
func TestReplaySeekBackWhilePaused(t *testing.T) {
	r := newReplay(0, syntheticSongs)
	r.run(0, 8000, 50, func(ms int) (string, uint32) {
		switch {
		case ms < 600:
			return "S", 19000 + uint32(ms)
		case ms < 2500:
			return "S", 19600
		default:
			return "S", 5000 + uint32(ms-2500)
		}
	})
	r.expectShape(t, "all lu1 lu2 pause resume lu0 lu1")
	if e := r.find(t, "playback_resume", 0); !approx(e.pos, 5) {
		t.Errorf("playback_resume 的位置 %.3f，期望回跳的目标 5.000", e.pos)
	}
}

// TestReplaySeekOnPauseTick 本该判出暂停的那一拍读到回跳：进度变了即重锚，这一拍不判暂停；回跳经
// seek 检测把行号退回第 1 行。只发一条 playback_resume，没有 playback_pause。
//
// 变异自证：注入在回放里——seek 分支不把 lastLineIdx 置 -1，本例红；nextLine 的判据改回旧写法本例仍绿
// （这一拍没有退回锚点），是反例。
func TestReplaySeekOnPauseTick(t *testing.T) {
	r := newReplay(0, syntheticSongs)
	r.run(0, 10000, 50, func(ms int) (string, uint32) {
		switch {
		case ms < 600:
			return "S", 19000 + uint32(ms)
		case ms < 1650: // 计时器自 600ms 起停在 19.6s，1650ms 是停滞超过 1s 的第一拍
			return "S", 19600
		default:
			return "S", 12000 + uint32(ms-1650)
		}
	})
	r.expectShape(t, "all lu1 lu2 resume lu1 lu2")
}

// TestReplaySmallDragBackWhilePaused 暂停中往回拖不足 2s：按 seek 检测的门槛不算跳转，只重锚、发
// playback_resume；行号不回退，到下一行才再发。这是行号只往前走的代价，与 applemusic 相同。
//
// 变异自证：nextLine 的判据改回旧写法，本例红（恢复后补发第 1 行、再重发第 2 行）。
func TestReplaySmallDragBackWhilePaused(t *testing.T) {
	r := newReplay(0, syntheticSongs)
	r.run(0, 14600, 50, func(ms int) (string, uint32) {
		switch {
		case ms < 2000:
			return "S", 19000 + uint32(ms)
		case ms < 4000:
			return "S", 21000
		default:
			return "S", 19500 + uint32(ms-4000)
		}
	})
	r.expectShape(t, "all lu1 lu2 pause resume lu3")
}

// TestReplayReanchorBehindLineStart 播放中计时器落后于插值、重锚把位置拉回到当前行的行首之前（不足
// 2s，不判 seek）：与暂停同一个机制，行号同样不回退。计时器在 0～500ms 停在 19.5s，550ms 读到 19.95s。
//
// 变异自证：nextLine 的判据改回旧写法，本例红（`all lu1 lu2 lu1 lu2 lu3`）。
func TestReplayReanchorBehindLineStart(t *testing.T) {
	r := newReplay(0, syntheticSongs)
	r.run(0, 10700, 50, func(ms int) (string, uint32) {
		if ms < 550 {
			return "S", 19500
		}
		return "S", 19950 + uint32(ms-550)
	})
	r.expectShape(t, "all lu1 lu2 lu3")
}

// TestReplayForwardPlayback 正常播放：每行恰发一次，依次向前。
//
// 变异自证：nextLine 改成 `idx >= last`，本例红（每拍重发当前行）。
func TestReplayForwardPlayback(t *testing.T) {
	r := newReplay(0, syntheticSongs)
	r.run(0, 45000, 50, func(ms int) (string, uint32) { return "S", uint32(ms) })
	r.expectShape(t, "all lu0 lu1 lu2 lu3 lu4")
}
