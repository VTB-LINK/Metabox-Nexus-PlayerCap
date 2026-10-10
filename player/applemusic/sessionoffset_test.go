package applemusic

// 本文件只测会话里 offset 的方向（issue #54）：offset 是延迟量，一行的 lyric_update 在整曲位置走到
// 「时间戳 + offset」时发出，它的 play_time 也是这个值。
//
// 六家播放器里只有本包有离线的会话夹具。其余五家对 player.LyricMatchTime 的调用由
// player/offsetsign_lint_test.go 查有没有，传入的位置对不对查不了。

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// 三行歌词起点 10 / 40 / 200s，从 20s 起播。延后 5s 时第 1 行在位置 45s 发出，提前 5s 时在 35s 发出；
// 发出时的 position 是整曲位置本身、不含 offset，play_time 是 时间戳 + offset。位置读数是整秒采样、
// 每 100ms 一轮，发出时刻相对 play_time 的滞后实测 0.05s，这里留到 0.5s。
//
// 变异自证（隔离的 worktree，实测）：emitLyricLine 的行匹配改回 `interp+s.offsetSec`，或
// player.LyricMatchTime 的符号改回 → 延后与提前两例红（发出位置与 play_time 相差 10s）；
// lyric_update 的 position 传成 player.LyricMatchTime 的结果 → 延后与提前两例红。
func TestSessionLyricLineFollowsOffsetDelay(t *testing.T) {
	const lineTime, slack = float32(40), float32(0.5)
	for _, c := range []struct {
		name      string
		offsetSec float32
	}{
		{"延后 5s", 5},
		{"提前 5s", -5},
		{"不偏移", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.s.offsetSec = c.offsetSec
			h.f.songs = np("偏移")
			h.f.docs = []mem.TTMLDoc{{Text: ttmlAt(300.4, 10, 40, 200), Count: 1}}
			h.setTwin(300, 20, true)
			h.run(40 * time.Second)

			var line *player.LyricUpdate
			for _, e := range h.evs {
				if d, ok := e.Data.(*player.LyricUpdate); ok && d.Index == 1 {
					line = d
					break
				}
			}
			if line == nil {
				t.Fatalf("没有推出第 1 行，推出的行：%v", lineIdxs(h.evs))
			}
			wantPlayTime := lineTime + c.offsetSec
			if line.Timestamp != lineTime || line.PlayTime != wantPlayTime {
				t.Errorf("timestamp=%v play_time=%v，期望 %v 与 %v", line.Timestamp, line.PlayTime, lineTime, wantPlayTime)
			}
			if lag := line.Position - wantPlayTime; lag < 0 || lag > slack {
				t.Errorf("第 1 行在位置 %v 发出，应在 play_time %v 之后 %v 以内", line.Position, wantPlayTime, slack)
			}
			t.Logf("offset=%vs：第 1 行在位置 %v 发出，play_time %v", c.offsetSec, line.Position, line.PlayTime)
		})
	}
}
