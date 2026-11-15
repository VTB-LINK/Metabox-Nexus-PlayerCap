package qqmusic

// 本文件是 runSession 一拍的复述，供 issue #96、#171、#202 的回放用例使用（nextline_test.go、carriedover_test.go、
// pausetail_test.go、switchzero_test.go）。
//
// 复述换歌认领（含等进度从上一首换过来、认领时取词之后的重读）、换歌前进度归零的暂扣（#202）、补取（取词、
// 重读与发出；暂扣放行的那一拍不补取）、回跳检测、暂停检测与暂停中的恢复判定、本地时钟插值与歌词行匹配，顺序与判据照 runSession；
// 不发状态与位置采样，换歌时的歌词与时长直接取自 songs，取词的结果与重读的读数由用例登记（fills、
// claimRereads）。取词阻塞期间 runSession 不出拍，用例分段调用 run 跳过取词耗时。
// 轮询循环要真进程，没有离线夹具：这份复述经 progressCarriedOver、midWaitGate、resumeGate、nextLine、
// switchHold、seekBack 与 rereadSameSong，改它们本身会让回放变红；直接改 runSession 的调用点（比如换歌分支不等、
// 行匹配不经 nextLine）不会，那一层靠读代码与真机，#202 的几处接线另由 switchzero_test.go 的源码扫描守形状。

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
	// pending：认领时歌词未到（歌曲 ID 未落位），认领只发空的 all_lyrics、不发 index -1，歌词由登记的补取
	// 送来；同 runSession 认领时的 err 一支（qqmusic.go 的 errSongMidUnavailable 路径）。
	pending bool
}

type replayEvent struct {
	typ  string // all_lyrics / lyric_update / playback_pause / playback_resume
	song string
	idx  int     // lyric_update 的行号；其余事件为 -1
	pos  float32 // 事件的 position（秒）
	ms   int     // 发出它的那一拍（会话开始后的毫秒，拍起点）
	at   int     // 发出它的墙钟时刻（会话开始后的毫秒）：本拍做过阻塞的取词时晚于 ms
}

func (e replayEvent) String() string {
	if e.typ == "lyric_update" {
		return fmt.Sprintf("%s[%s] idx=%d @%.3f (%dms, at %dms)", e.typ, e.song, e.idx, e.pos, e.ms, e.at)
	}
	return fmt.Sprintf("%s[%s] @%.3f (%dms, at %dms)", e.typ, e.song, e.pos, e.ms, e.at)
}

// replayFill 是登记的一次补取（runSession 的补取块）：song 是这首歌的歌名，自 from 起第一个走到补取块、且已认领
// 的是这首歌的拍取词；取词耗时 fetchMs，成功（ok）即送来 lines；取词返回后重读到 rereadName 与 rereadMs。
type replayFill struct {
	song       string
	from       int
	ok         bool
	lines      []lyricLine
	fetchMs    int
	rereadName string
	rereadMs   uint32
}

// replayReread 是认领那一拍取词之后的重读：取词耗时 fetchMs，重读到 name 与 ms。
type replayReread struct {
	fetchMs int
	name    string
	ms      uint32
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
	resume      resumeGate
	anchorMs    uint32
	anchorTime  time.Time
	lastMem     uint32

	progressWait midWaitGate
	hold         switchHold

	fills        []replayFill
	claimRereads map[int]replayReread

	tickMs int
	wall   time.Time // 此刻的墙钟（runSession 的 time.Now()）：拍起点，取词之后晚于读数时刻
	evs    []replayEvent
}

func newReplay(offsetSec float32, songs map[string]replaySong) *replay {
	return &replay{t0: time.Unix(1_700_000_000, 0), offsetSec: offsetSec, songs: songs, lastLineIdx: -1,
		claimRereads: map[int]replayReread{}}
}

func (r *replay) emit(typ string, idx int, pos float32) {
	r.evs = append(r.evs, replayEvent{typ: typ, song: r.lastName, idx: idx, pos: pos, ms: r.tickMs,
		at: int(r.wall.Sub(r.t0) / time.Millisecond)})
}

// takeFill 取出第一条属于当前已认领的歌、已到期的补取登记。
func (r *replay) takeFill(ms int) (replayFill, bool) {
	for i, f := range r.fills {
		if f.song == r.lastName && f.from <= ms {
			r.fills = append(r.fills[:i:i], r.fills[i+1:]...)
			return f, true
		}
	}
	return replayFill{}, false
}

// dropFills 作废属于 song 的补取登记：认领别的歌时，上一首的补取随之了结（runSession 认领时重置 lyricsPending）。
func (r *replay) dropFills(song string) {
	kept := r.fills[:0]
	for _, f := range r.fills {
		if f.song != song {
			kept = append(kept, f)
		}
	}
	r.fills = kept
}

// tick 是会话开始后第 ms 毫秒的一拍：读到的歌名为 name、进度为 progMs。
func (r *replay) tick(ms int, name string, progMs uint32) {
	r.tickMs = ms
	now := r.t0.Add(time.Duration(ms) * time.Millisecond) // 本拍读数的时刻（runSession 的 sampleTime）
	r.wall = now

	if name != r.lastName && name != "" {
		if progressCarriedOver(r.lastName, r.lastMem, progMs) {
			if _, keepWaiting := r.progressWait.wait(name, now, progressSettleWindow); keepWaiting {
				return
			}
		}
		r.progressWait.reset()
		progMs, now = r.claim(ms, name, progMs, now)
	} else if name == r.lastName {
		r.progressWait.reset()
	}

	held, released := r.hold.step(r.lastMem, progMs, now)
	if held {
		return
	}

	// 补取：放行拍不补取；取词之后重读，读到的仍是这首歌、且相对 lastMem 不构成暂扣的触发才采用；取词成功即发出。
	if !released && name == r.lastName && name != "" {
		if f, ok := r.takeFill(ms); ok {
			rereadAt := now.Add(time.Duration(f.fetchMs) * time.Millisecond)
			r.wall = rereadAt
			if rereadSameSong(&SongMetadata{Name: f.rereadName, ProgressMs: f.rereadMs}, nil, r.lastName, r.lastMem) {
				progMs, now = f.rereadMs, rereadAt
			}
			if f.ok {
				r.cur, r.lastLineIdx = f.lines, -1
				r.emit("all_lyrics", -1, player.LivePosition(float32(progMs)/1000, now, r.wall, !r.isPaused, r.durSec))
			}
		}
	}

	if progMs != r.lastMem {
		seek := released && progMs < r.lastMem
		if r.lastMem > 0 {
			if seekBack(r.lastMem, progMs) {
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
		if r.isPaused && (seek || r.resume.observe(int64(progMs)-int64(r.lastMem), now, replayPauseTimeoutSec*time.Second)) {
			r.isPaused = false
			r.resume.reset()
			if !resumed {
				r.emit("playback_resume", -1, pos)
			}
		}
		r.anchorMs, r.anchorTime, r.lastMem = progMs, now, progMs
	}

	if now.Sub(r.anchorTime).Seconds() > replayPauseTimeoutSec && !r.isPaused && r.lastName != "" {
		r.isPaused = true
		r.resume.reset()
		r.emit("playback_pause", -1, float32(r.anchorMs)/1000)
	}

	progressSec := player.LivePosition(float32(r.anchorMs)/1000, r.anchorTime, r.wall, !r.isPaused, r.durSec)
	if idx, ok := nextLine(r.cur, player.LyricMatchTime(progressSec, r.offsetSec), r.lastLineIdx); ok {
		r.lastLineIdx = idx
		r.emit("lyric_update", idx, progressSec)
	}
}

// claim 是换歌分支：认领 name，以本拍读数落锚，发 all_lyrics；没有歌词的歌另发 index -1 的 lyric_update。
// 登记了认领时的重读（claimRereads）的，照 runSession 先置 lastMem、再经 rereadSameSong 决定是否采用；返回本拍
// 其后用的读数与读数时刻，墙钟推到取词之后（r.wall）。
func (r *replay) claim(ms int, name string, progMs uint32, now time.Time) (uint32, time.Time) {
	s := r.songs[name]
	r.hold.drop(now)
	r.dropFills(r.lastName)
	r.lastName, r.lastLineIdx = name, -1
	r.anchorMs, r.anchorTime, r.lastMem, r.isPaused = progMs, now, progMs, false
	r.resume.reset()
	r.cur, r.durSec = s.lyrics, float32(s.durMs)/1000
	if s.pending {
		r.cur = nil
	}
	if rr, ok := r.claimRereads[ms]; ok && !s.pending {
		rereadAt := now.Add(time.Duration(rr.fetchMs) * time.Millisecond)
		r.wall = rereadAt
		if rereadSameSong(&SongMetadata{Name: rr.name, ProgressMs: rr.ms}, nil, name, r.lastMem) {
			progMs, now = rr.ms, rereadAt
		}
	}
	pos := player.LivePosition(float32(progMs)/1000, now, r.wall, true, r.durSec)
	r.emit("all_lyrics", -1, pos)
	if len(r.cur) == 0 && !s.pending {
		r.emit("lyric_update", -1, pos)
	}
	return progMs, now
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
	return shapeOf(r.evs)
}

func shapeOf(evs []replayEvent) string {
	parts := make([]string, len(evs))
	for i, e := range evs {
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
