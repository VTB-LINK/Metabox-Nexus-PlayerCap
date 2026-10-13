package qqmusic

// 本文件是 runSession 一拍的复述，供 issue #96 的回放用例使用（nextline_test.go、carriedover_test.go）。
//
// 只复述换歌认领（含等进度从上一首换过来）、回跳检测、暂停检测、本地时钟插值与歌词行匹配，顺序与判据
// 照 runSession；不取词、不重读、不发状态与位置采样，换歌时的歌词与时长直接取自 songs。轮询循环要真
// 进程，没有离线夹具：这份复述经 progressCarriedOver、midWaitGate 与 nextLine，改它们本身会让回放变红；
// 直接改 runSession 的调用点（比如换歌分支不等、行匹配不经 nextLine）不会，那一层靠读代码与真机。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// replayPauseTimeoutSec 同 runSession 的 pauseTimeoutSec。
const replayPauseTimeoutSec = 1.0

type replaySong struct {
	durMs  uint32
	lyrics []lyricLine
}

type replayEvent struct {
	typ  string // all_lyrics / lyric_update / playback_pause / playback_resume
	song string
	idx  int     // lyric_update 的行号；其余事件为 -1
	pos  float32 // 事件的 position（秒）
}

func (e replayEvent) String() string {
	if e.typ == "lyric_update" {
		return fmt.Sprintf("%s[%s] idx=%d @%.3f", e.typ, e.song, e.idx, e.pos)
	}
	return fmt.Sprintf("%s[%s] @%.3f", e.typ, e.song, e.pos)
}

type replay struct {
	t0        time.Time
	offsetSec float32
	songs     map[string]replaySong

	lastName    string
	cur         []lyricLine
	durSec      float32
	lastLineIdx int
	isPaused    bool
	anchorMs    uint32
	anchorTime  time.Time
	lastMem     uint32

	progressWait midWaitGate

	evs []replayEvent
}

func newReplay(offsetSec float32, songs map[string]replaySong) *replay {
	return &replay{t0: time.Unix(1_700_000_000, 0), offsetSec: offsetSec, songs: songs, lastLineIdx: -1}
}

func (r *replay) emit(typ string, idx int, pos float32) {
	r.evs = append(r.evs, replayEvent{typ: typ, song: r.lastName, idx: idx, pos: pos})
}

// tick 是会话开始后第 ms 毫秒的一拍：读到的歌名为 name、进度为 progMs。
func (r *replay) tick(ms int, name string, progMs uint32) {
	now := r.t0.Add(time.Duration(ms) * time.Millisecond)

	if name != r.lastName && name != "" {
		if progressCarriedOver(r.lastName, r.lastMem, progMs) {
			if _, keepWaiting := r.progressWait.wait(name, now, progressSettleWindow); keepWaiting {
				return
			}
		}
		r.progressWait.reset()
		r.claim(name, progMs, now)
	} else if name == r.lastName {
		r.progressWait.reset()
	}

	if progMs != r.lastMem {
		seek := false
		if r.lastMem > 0 {
			if progMs+2000 < r.lastMem {
				seek = true
			}
			if !seek && progMs > r.lastMem && progMs-r.lastMem > uint32(now.Sub(r.anchorTime).Milliseconds())+2000 {
				seek = true
			}
		}
		pos := float32(progMs) / 1000
		resumed := false
		if seek {
			r.lastLineIdx = -1
			r.emit("playback_resume", -1, pos)
			resumed = true
		}
		if r.isPaused {
			r.isPaused = false
			if !resumed {
				r.emit("playback_resume", -1, pos)
			}
		}
		r.anchorMs, r.anchorTime, r.lastMem = progMs, now, progMs
	}

	if now.Sub(r.anchorTime).Seconds() > replayPauseTimeoutSec && !r.isPaused && r.lastName != "" {
		r.isPaused = true
		r.emit("playback_pause", -1, float32(r.anchorMs)/1000)
	}

	progressSec := player.LivePosition(float32(r.anchorMs)/1000, r.anchorTime, now, !r.isPaused, r.durSec)
	if idx, ok := nextLine(r.cur, player.LyricMatchTime(progressSec, r.offsetSec), r.lastLineIdx); ok {
		r.lastLineIdx = idx
		r.emit("lyric_update", idx, progressSec)
	}
}

// claim 是换歌分支：认领 name，以本拍读数落锚，发 all_lyrics；没有歌词的歌另发 index -1 的 lyric_update。
func (r *replay) claim(name string, progMs uint32, now time.Time) {
	s := r.songs[name]
	r.lastName, r.lastLineIdx = name, -1
	r.anchorMs, r.anchorTime, r.lastMem, r.isPaused = progMs, now, progMs, false
	r.cur, r.durSec = s.lyrics, float32(s.durMs)/1000
	pos := player.LivePosition(float32(progMs)/1000, now, now, true, r.durSec)
	r.emit("all_lyrics", -1, pos)
	if len(s.lyrics) == 0 {
		r.emit("lyric_update", -1, pos)
	}
}

// run 从 fromMs 起每 stepMs 一拍、到 toMs 为止（含），每拍的歌名与进度由 at 给出。
func (r *replay) run(fromMs, toMs, stepMs int, at func(ms int) (string, uint32)) {
	for ms := fromMs; ms <= toMs; ms += stepMs {
		name, prog := at(ms)
		r.tick(ms, name, prog)
	}
}

// shape 把事件序列写成「类型:行号」的串，便于整段比对；行号只对 lyric_update 写出。
func (r *replay) shape() string {
	parts := make([]string, len(r.evs))
	for i, e := range r.evs {
		switch e.typ {
		case "lyric_update":
			parts[i] = fmt.Sprintf("lu%d", e.idx)
		case "all_lyrics":
			parts[i] = "all"
		case "playback_pause":
			parts[i] = "pause"
		case "playback_resume":
			parts[i] = "resume"
		}
	}
	return strings.Join(parts, " ")
}

func (r *replay) expectShape(t *testing.T, want string) {
	t.Helper()
	if got := r.shape(); got != want {
		t.Fatalf("事件序列\n  得到 %s\n  期望 %s\n明细：%v", got, want, r.evs)
	}
}

// find 返回第 n 个（从 0 起）类型为 typ 的事件。
func (r *replay) find(t *testing.T, typ string, n int) replayEvent {
	t.Helper()
	for _, e := range r.evs {
		if e.typ == typ {
			if n == 0 {
				return e
			}
			n--
		}
	}
	t.Fatalf("没有第 %d 个 %s；明细：%v", n, typ, r.evs)
	return replayEvent{}
}

func linesAt(times ...float32) []lyricLine {
	out := make([]lyricLine, len(times))
	for i, ts := range times {
		out[i] = lyricLine{Index: i, Time: ts}
	}
	return out
}

func approx(a, b float32) bool {
	d := a - b
	return d < 0.0005 && d > -0.0005
}
