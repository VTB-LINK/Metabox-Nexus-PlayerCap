package applemusic

// 本文件只测会话的状态与播放事件：没有位置源不报 playing（审查 P1-5）、清屏后同名歌重新宣告
// （审查 P1-6）、拖动只发一次 playback_resume（暂停中：审查 P1-7；播放中：读数会闪）、加载中不挂旧歌、
// 新歌起手即报 playing。

import (
	"math"
	"slices"
	"sort"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// 附加后先报「没有歌」：外层此前报的是「未启动」，附加后若一直没有歌也不能沿用它（审查 P2）。
//
// 变异自证：删掉 newSession 里的 s.showNoSong()，一条事件都没有而红。
func TestSessionStartsWithNoSong(t *testing.T) {
	h := newHarness(t)
	h.run(10 * time.Second)
	if len(h.evs) == 0 {
		t.Fatal("附加后应先报 waiting_song，却一条事件都没有")
	}
	st, ok := h.evs[0].Data.(*player.StatusInfo)
	if !ok || st.Status != "waiting_song" || st.Reason != player.ReasonNoSong {
		t.Fatalf("首个事件应为 waiting_song/no_song，got %+v", h.evs[0].Data)
	}
	if count(h.evs, player.EventSongInfoUpdate) != 0 {
		t.Fatal("没有歌时不应宣告歌曲")
	}
}

// 暂停中启动（有标题、没有在走的位置结构）：不宣告、不报 playing，一直是「未在播放」——属实，且没有
// 可靠位置源时绝不能报 playing（审查 P1-5）。恢复播放后认出位置结构，一次宣告（带时长与歌词）
// 并报 playing。
//
// 变异自证：把 watchCandidates 里判活的条件 `c.observe(t, now)` 换成 `t.Dur > 0`（形态对就认），
// 第一段出现 song_info_update 而红。
func TestSessionPausedStartNotAnnounced(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("暂停中启动")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(240.5, "第一行"), Count: 1}}
	h.f.twins[0x2000] = mem.Twin{Dur: 180, Pos: 0} // 形态相同的静止结构
	h.setTwin(240, 37, false)
	h.run(30 * time.Second)
	if n := count(h.evs, player.EventSongInfoUpdate); n != 0 {
		t.Fatalf("暂停中启动不应宣告，got %d 次 song_info_update", n)
	}
	if st := statuses(h.evs); len(st) != 1 || st[0] != "waiting_song" {
		t.Fatalf("应一直是 waiting_song，got %v", st)
	}

	from := h.mark()
	h.setTwin(240, 37, true)
	h.run(5 * time.Second)
	evs := h.since(from)
	if count(evs, player.EventSongInfoUpdate) != 1 {
		t.Fatalf("恢复后应宣告一次，got %d", count(evs, player.EventSongInfoUpdate))
	}
	if st := statuses(evs); len(st) == 0 || st[len(st)-1] != "playing" || slices.Contains(st, "paused") {
		t.Fatalf("恢复播放后应直接报 playing，got %v", st)
	}
	if al := allLyrics(evs); len(al) != 1 || al[0].Duration != 240 || al[0].Count == 0 {
		t.Fatalf("应只发一条带时长与歌词的 all_lyrics，got %+v", al)
	}
}

// 加载超过 loadingGrace 即清屏（waiting_song + clear_song_data）；之后同名同歌手再出现必须重新宣告。
// 加载不到 loadingGrace 不清屏：真机换歌加载 1.3~4.4s 都常见，3s 的旧门槛会让普通的切歌先清屏
// 再宣告（根路径上还会被别的播放器插进来一下）。
//
// 变异自证：把 clearSong 里 `s.song = songState{}` 删掉（只清屏不复位），song.dur 仍 > 0，加载期间每轮
// 重进 clearSong，清屏段的 clear_song_data 计数不为 1 而红；把 loadingGrace 改回 3s，「4.4s 不清屏」
// 一段出现 clear_song_data 而红。
func TestSessionClearThenSameSongReannounced(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("同名")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	from := h.mark()
	h.setTwin(0, 0, false) // 加载中
	h.run(4400 * time.Millisecond)
	if n := count(h.since(from), player.EventClearSongData); n != 0 {
		t.Fatalf("加载 4.4s（真机常见）不应清屏，got %d 次 clear_song_data", n)
	}
	h.run(2 * time.Second)
	evs := h.since(from)
	if count(evs, player.EventClearSongData) != 1 || !slices.Contains(statuses(evs), "waiting_song") {
		t.Fatalf("加载超过 %v 应清屏并报 waiting_song，got clear=%d status=%v", loadingGrace, count(evs, player.EventClearSongData), statuses(evs))
	}

	from = h.mark()
	h.setTwin(200, 0, true)
	h.run(5 * time.Second)
	evs = h.since(from)
	if count(evs, player.EventSongInfoUpdate) != 1 || count(evs, player.EventAllLyrics) == 0 {
		t.Fatalf("同名歌再出现应重新宣告，song_info=%d all_lyrics=%d",
			count(evs, player.EventSongInfoUpdate), count(evs, player.EventAllLyrics))
	}
	if st := statuses(evs); len(st) == 0 || st[len(st)-1] != "playing" {
		t.Fatalf("应回到 playing，got %v", st)
	}
}

// 快速换歌（归零不到 loadingGrace）不清屏，直接换成新歌。
//
// 变异自证：把 loadingGrace 改成 1s，归零 1.3s 就清屏而红。
func TestSessionQuickSwitchNoClear(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("一")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	from := h.mark()
	h.setTwin(0, 0, false)
	h.f.songs = np("二")
	h.run(1300 * time.Millisecond)
	h.setTwin(260, 0, true)
	h.run(3 * time.Second)
	evs := h.since(from)
	if count(evs, player.EventClearSongData) != 0 {
		t.Fatal("快速换歌不应清屏")
	}
	var names []string
	for _, si := range songInfos(evs) {
		names = append(names, si.Name)
	}
	if len(names) != 1 || names[0] != "二" {
		t.Fatalf("应宣告新歌「二」一次，got %v", names)
	}
}

// 暂停着切到另一首、过一阵才按播放：时长一写入就是开播（真机：暂停中切歌时时长一直是 0，按了播放
// 才写入），新歌宣告时直接报 playing，不能先报 paused 再等位置走两格才改口——路由只认 playing，
// 先报 paused 的那两三秒里根输出不会切过来。
//
// 变异自证：把 beginSong 里起手播放态改回沿用上一首的（上一首已暂停则 clock.reset(..., false, ...)），
// 新歌先报 paused 而红。
func TestSessionNewSongAfterPauseStartsPlaying(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("上一首")
	h.setTwin(200, 10, true)
	h.run(12 * time.Second)
	pos := h.f.twins[realTwin].Pos
	h.setTwin(200, pos, false) // 暂停
	h.run(3 * time.Second)
	if st := statuses(h.evs); st[len(st)-1] != "paused" {
		t.Fatalf("前置：应已判暂停，got %v", st)
	}

	from := h.mark()
	h.setTwin(0, 0, false) // 暂停中切歌：时长归零并停在 0
	h.f.songs = np("下一首")
	h.run(4 * time.Second)
	h.setTwin(260, 0, true) // 按了播放
	h.run(1 * time.Second)
	evs := h.since(from)
	if si := songInfos(evs); len(si) != 1 || si[0].Name != "下一首" {
		t.Fatalf("按播放后 1s 内应宣告新歌，got %d 条", len(si))
	}
	if st := statuses(evs); len(st) != 1 || st[0] != "playing" {
		t.Fatalf("新歌应直接报 playing，got %v", st)
	}
}

// 播放 → 暂停 → 暂停中往后拖 40s → 恢复：拖动时不发 playback_resume，恢复时只发一次。
//
// 变异自证：把 onTwin 的 evPausedSeek 分支改成也发 playback_resume，拖动那段出现 resume 而红。
func TestSessionSeekWhilePausedSingleResume(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("拖动")
	h.setTwin(300, 20, true)
	h.run(12 * time.Second) // 过了开播放宽期，暂停按常规门槛判

	from := h.mark()
	pos := h.f.twins[realTwin].Pos
	h.setTwin(300, pos, false) // 暂停
	h.run(3 * time.Second)
	evs := h.since(from)
	if count(evs, player.EventPlaybackPause) != 1 || !slices.Contains(statuses(evs), "paused") {
		t.Fatalf("应报一次暂停，got pause=%d status=%v", count(evs, player.EventPlaybackPause), statuses(evs))
	}

	from = h.mark()
	h.setTwin(300, pos+40, false) // 暂停中拖动
	h.run(3 * time.Second)
	if evs := h.since(from); count(evs, player.EventPlaybackResume) != 0 || slices.Contains(statuses(evs), "playing") {
		t.Fatalf("暂停中拖动不应发 resume 或 playing，got resume=%d status=%v", count(evs, player.EventPlaybackResume), statuses(evs))
	}

	from = h.mark()
	h.setTwin(300, pos+40, true) // 恢复
	h.run(4 * time.Second)
	evs = h.since(from)
	if n := count(evs, player.EventPlaybackResume); n != 1 {
		t.Fatalf("恢复时应只发一次 playback_resume，got %d", n)
	}
	if st := statuses(evs); len(st) != 1 || st[0] != "playing" {
		t.Fatalf("恢复时应只报一次 playing，got %v", st)
	}
}

// pausedAt212 给出一个放着三行歌词（起点 10 / 60 / 200s）、已在 212s 附近暂停的会话，返回暂停处的读数。
func pausedAt212(t *testing.T) (*harness, float64) {
	t.Helper()
	h := newHarness(t)
	h.f.songs = np("拖动")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlAt(300.4, 10, 60, 200), Count: 1}}
	h.setTwin(300, 201, true)
	h.run(12 * time.Second)
	pos := h.f.twins[realTwin].Pos
	h.setTwin(300, pos, false) // 暂停
	h.run(3 * time.Second)
	if st := statuses(h.evs); st[len(st)-1] != "paused" {
		t.Fatalf("前置：应已判暂停，got %v", st)
	}
	if got := lineIdxs(h.evs); len(got) != 1 || got[0] != 2 {
		t.Fatalf("前置：应已推出第 2 行，got %v", got)
	}
	return h, pos
}

// 暂停中拖动进度条（2026-10-08 真机：暂停在 212s，拖到约 63s）：读数先闪一下目标值又弹回原值、约半秒后
// 才真正过去。旧实现暂停中任何变化都立即重锚并推歌词行，一次拖动依次推出目标行、原位置的行、目标行。
// 现在只在真正过去后推一次目标位置的那一行；全程不发 playback_resume，之后按播放只发一条。
//
// 变异自证：把 clock.observe 暂停分支改回「变化即重锚」（不经 holdJump），闪现与弹回各推一行（1、2）
// 而红。
func TestSessionPausedSeekFlickerSingleLine(t *testing.T) {
	h, pos := pausedAt212(t)

	from := h.mark()
	h.setTwin(300, 63, false) // 闪一轮
	h.run(step)
	h.setTwin(300, pos, false) // 弹回原值
	h.run(500 * time.Millisecond)
	if got := lineIdxs(h.since(from)); len(got) != 0 {
		t.Fatalf("闪现与弹回不应推歌词行，got %v", got)
	}
	h.setTwin(300, 63, false) // 真正过去
	h.run(2 * time.Second)
	evs := h.since(from)
	if got := lineIdxs(evs); len(got) != 1 || got[0] != 1 {
		t.Fatalf("应只推一次目标位置的第 1 行，got %v", got)
	}
	if count(evs, player.EventPlaybackResume) != 0 || len(statuses(evs)) != 0 {
		t.Fatalf("暂停中拖动不应发 playback_resume 或 status_update，got resume=%d status=%v", count(evs, player.EventPlaybackResume), statuses(evs))
	}

	from = h.mark()
	h.setTwin(300, 63, true) // 按播放
	h.run(4 * time.Second)
	evs = h.since(from)
	if rs := resumes(evs); len(rs) != 1 || rs[0].Position < 63 || rs[0].Position > 67 {
		t.Fatalf("恢复应只发一条 playback_resume、位置在新位置附近，got %+v", rs)
	}
	if st := statuses(evs); len(st) != 1 || st[0] != "playing" {
		t.Fatalf("恢复时应只报一次 playing，got %v", st)
	}
}

// 暂停中真拖动（读数直接变成目标值并保持）：持续满 jumpHold 的那次读数确认，推一条目标位置的歌词行；
// 连着拖到另一处再推一条；拖完 1s 后按播放，恰一条 playback_resume、位置在新位置附近。
//
// 变异自证：把 onTwin 的 evPausedSeek 分支里的 `s.lastLineIdx = -1` 去掉，往回拖之后不推歌词行而红；把
// clock.holdJump 末尾的持续判定改成恒真，持续 0.1s 时就推出第 1 行而红。
func TestSessionPausedSeekPushesLineOnce(t *testing.T) {
	h, _ := pausedAt212(t)

	from := h.mark()
	h.setTwin(300, 63, false)
	h.run(3 * step) // 读到三次，持续 0.2s
	if got := lineIdxs(h.since(from)); len(got) != 0 {
		t.Fatalf("目标读数持续不足 jumpHold 先不认，got 歌词行 %v", got)
	}
	h.run(step)
	if got := lineIdxs(h.since(from)); len(got) != 1 || got[0] != 1 {
		t.Fatalf("持续满 jumpHold 的那次读数应确认并推出第 1 行，got %v", got)
	}
	h.setTwin(300, 15, false) // 接着拖到另一处
	h.run(time.Second)
	h.setTwin(300, 15, true) // 立刻按播放
	h.run(4 * time.Second)
	evs := h.since(from)
	if got := lineIdxs(evs); len(got) != 2 || got[1] != 0 {
		t.Fatalf("两次拖动应各推一行（1、0），got %v", got)
	}
	if rs := resumes(evs); len(rs) != 1 || rs[0].Position < 15 || rs[0].Position > 19 {
		t.Fatalf("应恰有一条 playback_resume、位置在新位置附近，got %+v", rs)
	}
	if st := statuses(evs); len(st) != 1 || st[0] != "playing" {
		t.Fatalf("应只报一次 playing，got %v", st)
	}
}

// pausedSeekCase 是 seekSamplePaused 里的一次暂停中拖动：读数依次是 T 闪现 → 原值 → V 闪现 → T 约 1.1s →
// V 约 245ms → V−1 约 512ms → V，此后约每 1024ms 进一格（已在播放）。
type pausedSeekCase struct {
	flash, settle        int     // 目标值第一次闪现、读数最后落到 V 的时刻
	orig, target, landed float64 // 原位置、目标 T、落点 V
}

var pausedSeekCases = []pausedSeekCase{
	{534545, 537113, 24, 130, 120},
	{544623, 548194, 123, 205, 200},
	{559480, 562055, 207, 20, 17},
}

// pausedBeforeSeek 给出一个放到原位置后已暂停 4s（已判出暂停）的会话，歌词三行：原位置、落点 V、目标 T
// 各占一行。此后回放用的毫秒数以拖动开始（目标值第一次闪现）为 0：返回它对应的时刻、下一次该读的
// 毫秒数，以及 T、V 所在的行号。
func pausedBeforeSeek(t *testing.T, tc pausedSeekCase, phase int) (h *harness, origin time.Time, next, tLine, vLine int) {
	t.Helper()
	const pauseFor = 4000
	starts := []float64{tc.orig - 5, tc.landed - 2, tc.target - 2}
	sort.Float64s(starts)
	h = newHarness(t)
	h.f.songs = np("拖动")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlAt(256.4, starts...), Count: 1}}
	from := -pauseFor - 13000 + phase // 先正常播放 13s
	origin = h.now.Add(-time.Duration(from) * time.Millisecond)
	next = h.replay(origin, 256, from, 0, 50, func(ms int) float64 {
		if ms < -pauseFor { // 每 1024ms 一格，在 -pauseFor 进到原位置后停住
			return tc.orig - float64((-pauseFor-ms+1023)/1024)
		}
		return tc.orig
	})
	if st := statuses(h.evs); len(st) == 0 || st[len(st)-1] != "paused" {
		t.Fatalf("%d 相位 %d 前置：应已判暂停，got %v", tc.flash, phase, st)
	}
	return h, origin, next, sort.SearchFloat64s(starts, tc.target-2), sort.SearchFloat64s(starts, tc.landed-2)
}

// 暂停中拖动，按 2026-10-09 真机采样的三次回放、拖完停在 V：全程不发 playback_resume、不发
// status_update。3s 后按播放：恰一条 playback_resume，位置在 V 附近。
//
// 歌词行只记录现状（已知现象，不处理）：一次拖动推出两条 lyric_update，先是 T 所在的行，再是 V 所在的
// 行。T 的读数停了约 1.1s，长于 jumpHold，分不出它是过渡；之后 V 只停约 245ms、不认，V−1 停约 512ms、
// 认（推出 V 所在的行），再进到 V 是「一跳」、不重推。
//
// 变异自证：把 onTwin 的 evPausedSeek 分支改成也发 playback_resume，拖动那段出现 playback_resume 而红；
// 把 jumpHold 改回 40ms，V 与 V−1 各确认一次，V 所在的行被推两次而红。
func TestSessionPausedSeekSample(t *testing.T) {
	for _, tc := range pausedSeekCases {
		for phase := 0; phase < 50; phase += 7 {
			h, origin, next, tLine, vLine := pausedBeforeSeek(t, tc, phase)
			playAt := tc.settle - tc.flash + 3000
			pos := func(ms int) float64 {
				if ms < playAt {
					return sampleAt(seekSamplePaused, min(tc.flash+ms, tc.settle))
				}
				return tc.landed + math.Floor(float64(ms-playAt+1024-450)/1024) // 按了播放：0.45s 后第一跳，之后每 1024ms 一格
			}

			mark := h.mark()
			next = h.replay(origin, 256, next, playAt, 50, pos)
			evs := h.since(mark)
			if n, st := count(evs, player.EventPlaybackResume), statuses(evs); n != 0 || len(st) != 0 {
				t.Fatalf("%d 相位 %d：暂停中拖动不应发 playback_resume 或 status_update，got resume=%d status=%v", tc.flash, phase, n, st)
			}
			if got := lineIdxs(evs); len(got) != 2 || got[0] != tLine || got[1] != vLine {
				t.Fatalf("%d 相位 %d：现状是先推 T 所在的第 %d 行、再推 V 所在的第 %d 行，got %v", tc.flash, phase, tLine, vLine, got)
			}

			mark = h.mark()
			h.replay(origin, 256, next, playAt+4000, 50, pos)
			evs = h.since(mark)
			if rs := resumes(evs); len(rs) != 1 || float64(rs[0].Position) < tc.landed || float64(rs[0].Position) > tc.landed+4 {
				t.Fatalf("%d 相位 %d：按播放应只发一条 playback_resume、位置在 V=%.0f 附近，got %v", tc.flash, phase, tc.landed, positions(rs))
			}
			if st := statuses(evs); len(st) != 1 || st[0] != "playing" {
				t.Fatalf("%d 相位 %d：按播放应只报一次 playing，got %v", tc.flash, phase, st)
			}
		}
	}
}

// positions 返回各条 playback_resume 的位置（报错时打印用）。
func positions(rs []*player.PlaybackTimeInfo) []float32 {
	out := make([]float32, len(rs))
	for i, r := range rs {
		out[i] = r.Position
	}
	return out
}

// resumeAfter 从 next 起按 50ms 轮询回放到 endMs，返回第一条 playback_resume 是在读第几毫秒时发出的
// （没有则为 -1）与全部事件。
func resumeAfter(h *harness, origin time.Time, dur float64, next, endMs int, pos func(ms int) float64) (at int, evs []player.Event) {
	mark := h.mark()
	at = -1
	for ms := next; ms < endMs; ms += 50 {
		h.replay(origin, dur, ms, ms+1, 50, pos)
		if at < 0 && count(h.since(mark), player.EventPlaybackResume) > 0 {
			at = ms
		}
	}
	return at, h.since(mark)
}

// 暂停 → 拖动 → 立刻按播放：恰一条 playback_resume、一次 playing。暂停中要连着两跳才判恢复（见
// resumeConfirm），所以它在恢复后的第二跳发出。
//
// 「采样」三例照 seekSamplePaused 回放（那三次拖动之后都接着播放了；采样没有记下按播放的时刻）：读数
// 最后落到 V 是第一跳，约 1.02s 后的 V+1 是第二跳，playback_resume 在这时发出，距目标值第一次闪现
// 3.6~4.6s。
//
// 「读数直接落到目标」两例是构造的：拖动的同时按下播放，读数直接变成目标值并保持，delta 之后进第一格、
// 此后每 1024ms 一格。进第一格晚于 jumpHold（0.45s）：拖动先确认，playback_resume 在按下播放后约 1.5s。
// 进第一格早于 jumpHold（0.25s）：这一格并入待确认的目标（holdJump 允许目标随播放推进），不算恢复的
// 第一跳，playback_resume 晚一格、在约 2.3s；jumpHold 为 40ms 时，进第一格落在拖动后 50ms 之内才如此。
//
// 变异自证：把 onTwin 的 evPausedSeek 分支改成也发 playback_resume，拖动确认时各多发一条而红；把
// clock.observe 暂停分支里「等下一跳确认」去掉（第一跳就判恢复），playback_resume 早一跳、在读数落到 V
// 时就发出而红；把 jumpHold 改回 40ms，「0.25s 后进第一格」一例在 1.3s 就发出而红。
func TestSessionPausedSeekThenPlayAtOnce(t *testing.T) {
	for _, tc := range pausedSeekCases {
		h, origin, next, _, _ := pausedBeforeSeek(t, tc, 0)
		pos := func(ms int) float64 { return sampleAt(seekSamplePaused, tc.flash+ms) }
		at, evs := resumeAfter(h, origin, 256, next, tc.settle-tc.flash+3500, pos)
		rs := resumes(evs)
		if len(rs) != 1 || float64(rs[0].Position) < tc.landed+1 || float64(rs[0].Position) > tc.landed+2 {
			t.Fatalf("采样 %d：应恰有一条 playback_resume、位置在 V+1=%.0f 那一格，got %v", tc.flash, tc.landed+1, positions(rs))
		}
		if st := statuses(evs); len(st) != 1 || st[0] != "playing" {
			t.Fatalf("采样 %d：应只报一次 playing，got %v", tc.flash, st)
		}
		if late := at - (tc.settle - tc.flash); late < 1024 || late > 1024+50 {
			t.Fatalf("采样 %d：playback_resume 在读数落到 V 之后 %dms 发出，应在 V+1 那一跳（约 1024ms）", tc.flash, late)
		}
		t.Logf("采样 %d：playback_resume 距读数落到 V %dms，距目标值第一次闪现 %dms", tc.flash, at-(tc.settle-tc.flash), at)
	}

	for _, tc := range []struct {
		name          string
		delta, wantAt int // 按下播放（同时拖动）到进第一格、到发出 playback_resume 的毫秒数
	}{
		{"读数直接落到目标，0.45s 后进第一格", 450, 450 + 1024},
		{"读数直接落到目标，0.25s 后进第一格", 250, 250 + 2*1024},
	} {
		h, _ := pausedAt212(t)
		pos := func(ms int) float64 { return 63 + math.Floor(float64(ms-tc.delta+1024)/1024) }
		at, evs := resumeAfter(h, h.now, 300, 0, 4000, pos)
		rs := resumes(evs)
		if len(rs) != 1 || rs[0].Position < 63 || rs[0].Position > 67 {
			t.Fatalf("%s：应恰有一条 playback_resume、位置在新位置附近，got %v", tc.name, positions(rs))
		}
		if st := statuses(evs); len(st) != 1 || st[0] != "playing" {
			t.Fatalf("%s：应只报一次 playing，got %v", tc.name, st)
		}
		if at < tc.wantAt || at >= tc.wantAt+50 {
			t.Fatalf("%s：playback_resume 在按下播放后 %dms 发出，应在 %d~%dms", tc.name, at, tc.wantAt, tc.wantAt+50)
		}
		t.Logf("%s：playback_resume 在按下播放后 %dms 发出", tc.name, at)
	}
}

// 播放中拖动进度条：读数先闪一下目标值又弹回，约半秒后才真正变过去（见 TestClockSeekFlickerIgnored）。
// 整次拖动只发一条 playback_resume，位置在目标附近。
//
// 变异自证：把 clock.observe 里待确认那一段删掉（第一次读到就落锚、报 evSeek），本例发出三条
// playback_resume（285 → 24 → 285）而红。
func TestSessionSeekFlickerSingleResume(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("拖动")
	h.setTwin(300, 20, true)
	h.run(5 * time.Second)

	from := h.mark()
	h.play = false // 之后逐步摆读数
	cur := h.f.twins[realTwin].Pos
	for _, pos := range []float64{285, cur, cur, cur, cur, cur, 285, 285} {
		h.f.twins[realTwin] = mem.Twin{Dur: 300, Pos: pos}
		h.run(step)
	}
	h.setTwin(300, 285, true)
	h.run(3 * time.Second)

	var resumes []*player.PlaybackTimeInfo
	for _, e := range h.since(from) {
		if e.Type == player.EventPlaybackResume {
			resumes = append(resumes, e.Data.(*player.PlaybackTimeInfo))
		}
	}
	if len(resumes) != 1 || resumes[0].Position < 285 || resumes[0].Position > 286 {
		t.Fatalf("一次拖动应只发一条 playback_resume、位置在 285~286，got %d 条 %+v", len(resumes), resumes)
	}
	if st := statuses(h.since(from)); len(st) != 0 {
		t.Fatalf("播放中拖动不改变播放态，不应发 status_update，got %v", st)
	}
}

// 2026-10-09 真机采样的 12 次播放中拖动（seekSamplePlaying）按 50ms 轮询整段回放：每次拖动恰好一条
// playback_resume、位置在目标附近（只闪了一下就被下一次拖动取代的那次不发），歌词行只推目标位置的那
// 一行、不回到原位置，状态始终是 playing。
//
// 同日真机（jumpHold 还是 40ms）：一次拖动发出 playback_resume 52.35 → 160.35 → 52.35，其间 lyric_update
// 的行号 6 → 23 → 6；另一次 238.35 → 221.35 → 238.35，行号 57 → 52、53 → 57。目标值闪现约 220ms 的拖动
// （采样里是 494701、502380 两次）就是这样：闪现、弹回、落定各被认了一次。
//
// 变异自证：把 jumpHold 改回 40ms，那两次拖动各发出三条 playback_resume（共 15 条）、歌词行在目标行与
// 原位置的行之间来回而红。
func TestSessionSeekSampleSingleResume(t *testing.T) {
	tl := seekSamplePlaying
	var landed []sampleDrag
	var starts []float64 // 每个目标各占一行歌词，起点在目标前 3s
	for _, d := range sampleDrags {
		if d.land != 0 {
			landed = append(landed, d)
			starts = append(starts, d.to-3)
		}
	}
	sort.Float64s(starts)
	for phase := 0; phase < 50; phase += 7 {
		h := newHarness(t)
		h.f.songs = np("拖动")
		h.f.docs = []mem.TTMLDoc{{Text: ttmlAt(256.4, starts...), Count: 1}}
		from := tl[0].ms - 13000 + phase // 先正常播放 13s：认出位置结构、宣告，并过了开播放宽期
		origin := h.now.Add(-time.Duration(from) * time.Millisecond)
		pos := func(ms int) float64 { return sampleAt(tl, ms) }
		next := h.replay(origin, 256, from, sampleDrags[0].flash, 50, pos)
		if st, got := statuses(h.evs), lineIdxs(h.evs); len(st) == 0 || st[len(st)-1] != "playing" || len(got) != 1 || got[0] != 0 {
			t.Fatalf("相位 %d 前置：应在播放、已推出第 0 行，got status=%v 歌词行=%v", phase, st, got)
		}

		mark := h.mark()
		h.replay(origin, 256, next, tl[len(tl)-1].ms, 50, pos)
		evs := h.since(mark)
		rs, lines := resumes(evs), lineIdxs(evs)
		if len(rs) != len(landed) || len(lines) != len(landed) {
			t.Fatalf("相位 %d：%d 次落定的拖动应各有一条 playback_resume、各推一行歌词，got 位置 %v、歌词行 %v", phase, len(landed), positions(rs), lines)
		}
		for i, d := range landed {
			if p := float64(rs[i].Position); p < d.to-1 || p > d.to+1 {
				t.Errorf("相位 %d：%d 那次拖动的 playback_resume 位置=%.2f，应在目标 %.0f 附近", phase, d.flash, p, d.to)
			}
			if want := sort.SearchFloat64s(starts, d.to-3); lines[i] != want {
				t.Errorf("相位 %d：%d 那次拖动推出第 %d 行，应是目标所在的第 %d 行", phase, d.flash, lines[i], want)
			}
		}
		if n, st := count(evs, player.EventPlaybackPause), statuses(evs); n != 0 || len(st) != 0 {
			t.Errorf("相位 %d：状态应始终是 playing（不发 playback_pause、不发 status_update），got playback_pause=%d status=%v", phase, n, st)
		}
	}
}

// 播放中拖动一次进度条，跳转确认后读数的下一跳晚于 pauseThreshold 才来（2026-10-08 真机：根 WS 依次
// 收到 playback_resume 55.35 → 约 2s 后 playback_pause 56 + paused → 约 1s 后 playback_resume 57.5 +
// playing，音频与波形都没断）。全程只该有跳转那一条 playback_resume。
//
// 变异自证：去掉 clock.observe 跳转确认处的 `c.relaxAt = now`，跳转后 1.7s 处发出 playback_pause 与
// paused、随后又一条 playback_resume 与 playing 而红。
func TestSessionSeekThenSlowFirstFlipNoPause(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("拖动")
	h.setTwin(300, 20, true)
	h.run(12 * time.Second) // 过了开播放宽期

	from := h.mark()
	h.setTwin(300, 55, false) // 拖到 55：读数变成目标值后停 2.5s
	h.run(2500 * time.Millisecond)
	h.setTwin(300, 56, true) // 之后每秒一跳
	h.run(6 * time.Second)
	evs := h.since(from)
	if rs := resumes(evs); len(rs) != 1 || rs[0].Position < 55 || rs[0].Position > 56 {
		t.Fatalf("应只有跳转那一条 playback_resume、位置在 55~56，got %d 条 %+v", len(rs), rs)
	}
	if n, st := count(evs, player.EventPlaybackPause), statuses(evs); n != 0 || len(st) != 0 {
		t.Fatalf("跳转后不应报暂停或任何 status_update，got playback_pause=%d status=%v", n, st)
	}
}

// 连着拖三次（间隔 2s、1s，其间读数停着）：每次一条 playback_resume，状态始终是 playing。误判成暂停后，
// 接着的拖动都会被当成暂停中拖动、不发 playback_resume，状态在 paused 里停留数秒（2026-10-08 真机）。
//
// 变异自证：同上一条的变异，第一次拖动后 1.7s 处报了暂停，之后两次拖动不发 playback_resume 而红。
func TestSessionRepeatedSeeksStayPlaying(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("拖动")
	h.setTwin(300, 20, true)
	h.run(12 * time.Second)

	from := h.mark()
	for i, pos := range []float64{100, 200, 50} {
		h.setTwin(300, pos, false)
		h.run([]time.Duration{2 * time.Second, time.Second, 2 * time.Second}[i])
	}
	h.setTwin(300, 51, true)
	h.run(5 * time.Second)
	evs := h.since(from)
	rs := resumes(evs)
	if len(rs) != 3 {
		t.Fatalf("三次拖动应各发一条 playback_resume，got %d 条", len(rs))
	}
	for i, want := range []float32{100, 200, 50} {
		if rs[i].Position < want || rs[i].Position > want+1 {
			t.Fatalf("第 %d 条 playback_resume 位置=%.2f，应在 %.0f~%.0f", i+1, rs[i].Position, want, want+1)
		}
	}
	if n, st := count(evs, player.EventPlaybackPause), statuses(evs); n != 0 || len(st) != 0 {
		t.Fatalf("状态应始终是 playing（不发 playback_pause、不发 status_update），got playback_pause=%d status=%v", n, st)
	}
}

// 读到一次目标值（待确认）之后没有真的跳过去，而是暂停了、或换了歌：待确认的目标不残留，不发
// playback_resume；之后的暂停、恢复与新歌照常。
//
// 变异自证：把 clock.observe 里待确认那一步去掉（播放分支偏出范围即落锚、报 evSeek），两例都在读到
// 285 的那一轮发出 playback_resume 而红。
func TestSessionPendingSeekThenPauseOrSongChange(t *testing.T) {
	t.Run("转入暂停", func(t *testing.T) {
		h := newHarness(t)
		h.f.songs = np("拖动")
		h.setTwin(300, 20, true)
		h.run(12 * time.Second)

		from := h.mark()
		pos := h.f.twins[realTwin].Pos
		h.setTwin(300, 285, false) // 闪一下目标值
		h.run(step)
		h.setTwin(300, pos, false) // 弹回原位，并且停住了（暂停）
		h.run(3 * time.Second)
		evs := h.since(from)
		if count(evs, player.EventPlaybackResume) != 0 || count(evs, player.EventPlaybackPause) != 1 {
			t.Fatalf("应只报一次暂停、不发 playback_resume，got resume=%d pause=%d", count(evs, player.EventPlaybackResume), count(evs, player.EventPlaybackPause))
		}

		from = h.mark()
		h.setTwin(300, pos, true) // 在原位恢复
		h.run(4 * time.Second)
		if rs := resumes(h.since(from)); len(rs) != 1 || float64(rs[0].Position) < pos || float64(rs[0].Position) > pos+4 {
			t.Fatalf("恢复应只发一条 playback_resume、位置在原位附近（%.0f 起），got %+v", pos, rs)
		}
	})
	t.Run("换歌", func(t *testing.T) {
		h := newHarness(t)
		h.f.songs = np("A")
		h.setTwin(300, 20, true)
		h.run(12 * time.Second)

		from := h.mark()
		h.setTwin(300, 285, false) // 闪一下目标值
		h.run(step)
		skipTo(h, store("B", "封面:B"), 260)
		h.run(5 * time.Second)
		evs := h.since(from)
		if count(evs, player.EventPlaybackResume) != 0 || count(evs, player.EventPlaybackPause) != 0 {
			t.Fatalf("换歌不应发 playback_resume / playback_pause，got resume=%d pause=%d", count(evs, player.EventPlaybackResume), count(evs, player.EventPlaybackPause))
		}
		if si := songInfos(evs); len(si) != 1 || si[0].Name != "B" {
			t.Fatalf("应宣告 B 一次，got %d 条", len(si))
		}
		if st := statuses(evs); len(st) != 1 || st[0] != "playing" {
			t.Fatalf("新歌应只报一次 playing，got %v", st)
		}
	})
}

// 快速连切：B 刚放 1.5s 就切到下一首，加载中（时长为 0）内存里是「正在连接…」。加载期间不得发任何
// 事件，更不能把「正在连接…」配上一首的时长宣告出去（复查实测旧实现会，P2 那条就是它）。加载中存储
// 换了会触发全扫，但那只用来记歌词归属。
//
// 变异自证：把 maybeScan 里两处 songReady 的 `!s.loading` 都去掉，加载那段宣告出「正在连接…」而红。
func TestSessionNoEventsWhileLoading(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	h.setTwin(0, 0, false)
	h.run(600 * time.Millisecond)
	h.f.songs = np("B")
	h.setTwin(260, 0, true)
	h.run(1500 * time.Millisecond)

	from, scans := h.mark(), h.f.scans
	h.setTwin(0, 0, false)
	h.f.songs = np("正在连接…")
	h.run(2 * time.Second) // 短于 loadingGrace，不清屏
	if evs := h.since(from); len(evs) != 0 {
		t.Fatalf("加载中不应发任何事件，got %d 条（首条 %s）", len(evs), evs[0].Type)
	}
	if h.f.scans == scans {
		t.Fatal("加载中存储换了应触发全扫（记歌词归属用）")
	}

	from = h.mark()
	h.f.songs = np("C")
	h.setTwin(300, 0, true)
	h.run(3 * time.Second)
	for _, si := range songInfos(h.since(from)) {
		if si.Name != "C" {
			t.Fatalf("加载结束后应宣告 C，got %q", si.Name)
		}
	}
	if count(h.since(from), player.EventSongInfoUpdate) != 1 {
		t.Fatal("加载结束后应宣告 C 一次")
	}
}

// 换歌后一直取不到标题：上一首的信息不能一直挂着，titleGiveUp（10s）一到就清屏，不拖到下一次扫描。
//
// 变异自证：删掉 maybeScan 里 titleGiveUp 的那一段，一直不清屏而红。（只把它挪到全扫之后本例仍绿：
// 迟迟宣告不了时候选扫描在换歌后 5、7、10s 各来一次，10s 那次正好赶上——每轮都判是不依赖这个巧合。）
func TestSessionTitleGiveUpOnTime(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)
	h.setTwin(0, 0, false)
	h.f.songs = nil
	h.run(600 * time.Millisecond)
	h.setTwin(260, 0, true)

	from := h.mark()
	h.run(9500 * time.Millisecond)
	if n := count(h.since(from), player.EventClearSongData); n != 0 {
		t.Fatalf("10s 之前不应清屏，got %d", n)
	}
	h.run(1500 * time.Millisecond)
	evs := h.since(from)
	if count(evs, player.EventClearSongData) != 1 || !slices.Contains(statuses(evs), "waiting_song") {
		t.Fatalf("约 10s 时应清屏并报 waiting_song，got clear=%d status=%v", count(evs, player.EventClearSongData), statuses(evs))
	}
}

// 时长相同的下一首、没看到归零（真机自然播完衔接时归零只有约 57ms，轮询看不到）：位置结构这头只看到
// 位置跳回 0，靠盯「正在播放」存储认出是新歌。新歌不得沿用上一首的歌词——上一首自己的歌词文档
// 早就记在它名下。
//
// 变异自证：删掉 tick 里对 watchStore 的调用、并删掉 onTwin 里跳回开头时置 storeDirty 的那句，B 不被
// 宣告而红；把 applyMeta 里「确认窗口之外」也改成转移旧标题名下的文档，B 配上「A 的歌词」而红。
func TestSessionSameDurationChangeDetectedByStore(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(200, 30, true)
	h.run(5 * time.Second)
	// A 的歌词在它开播之后才到（真机常态）：是「这首歌期间第一次露面的文档」，换歌后不能跟着标题转走。
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(200.4, "A 的歌词"), Count: 3}}
	h.run(15 * time.Second)
	if al := allLyrics(h.evs); len(al) == 0 || al[len(al)-1].Count == 0 {
		t.Fatalf("前置：A 应配上自己的歌词，got %+v", al)
	}

	from := h.mark()
	h.f.songs = np("B")
	h.setTwin(200, 0, true)
	h.run(3 * time.Second)
	evs := h.since(from)
	if si := songInfos(evs); len(si) != 1 || si[0].Name != "B" {
		t.Fatalf("应宣告 B 一次，got %d 条", len(si))
	}
	for _, al := range allLyrics(evs) {
		if al.Count != 0 {
			t.Fatalf("B 不应配上 A 的歌词，got %q", al.Lyrics[0].Text)
		}
	}
}
