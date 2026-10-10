package cloudmusic

// 本文件只测补发歌词的那一拍（runSession 的「Redux lyrics appeared later」与超时兜底）在回放里对外带什么
// 位置、发在哪：补词的阻塞之后位置取发出那一刻的读数；与校准同拍时发在对表之后，不另发通知。
// 夹具与它的边界见 domsyncreplay_test.go 的文件头：回放不经 runSession 本身，注释里「注入在回放里」的
// 变异改的是 replay 的对应步骤，runSession 的调用点靠读代码与真机。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// tickEvents 返回某一拍（按 got）发出的全部事件，含位置采样。
func (r *replay) tickEvents(ms int) (out []replayEvent) {
	for _, e := range r.events {
		if e.ms == ms {
			out = append(out, e)
		}
	}
	return out
}

// TestReplayBlockedTickEmitsLivePosition 补词阻塞的那一拍（审查给出的时间线）：对外事件与位置采样带的是
// 发出那一刻的时钟读数，不是读到 DOM 那一刻的。下游按到达的时刻落锚，带后者会多落后一个阻塞时长，直到
// 下一条带位置的事件。
//
// 一直在播，p(ms) = 8.99s + ms，时钟 = p − 0.30。上一拍 sent=1900、got=1902、秒数 10；本拍 sent=2000、
// got=2002、秒数 10（不是跳秒，不对表），时钟读数 10.692。本拍补到歌词（显示边界 5s 与 60s，下一次换行在
// 49 秒之后），读翻译歌词的调用阻塞 1 秒，事件到 3002ms 才发出，那一刻时钟读数 11.692。
//
// 变异自证（注入在回放里，均实测）：posNow 取 got 时刻的读数，本例红（all_lyrics 与 lyric_update 的位置
// 是 10.692，前端此后比后端落后 1.000s）；只有行号变化的 lyric_update 取 got 时刻的读数，本例红（同上，
// 前端的锚点是它落下的）；只有补发的 all_lyrics 取 got 时刻的读数，本例红（只有该条的位置断言红，前端的
// 锚点由其后的 lyric_update 带正）；只有位置采样取 got 时刻的读数，本例红（采样的位置 10.692）；补到的
// 歌词推迟到行匹配之后才生效，本例红（本拍没有推第 0 行）。
// 直接改 runSession——各处 posNow() 换回 clock.at(now)（全部或只换行号变化的 lyric_update 一处）、位置
// 采样传 now 与 clock.at(now)——全仓测试仍绿（实测）：调用点靠人守。
func TestReplayBlockedTickEmitsLivePosition(t *testing.T) {
	tl := timeline{p0: 8990}
	r := newReplay(tl.p(0) - 0.30)
	r.offset = 0.5
	r.tick(1900, 1902, tl.sec(1900), true)
	r.lastSample = msAt(2002).Add(-player.PositionSampleInterval) // 下一拍到点，按 got 或按发出的时刻计都到
	r.tickWith(2000, 2002, tl.sec(2000), true, tickOpt{blockMs: 1000, late: true, lines: []float32{5, 60}})

	if got := r.clock.at(msAt(2002)); !inRange(got, 10.692, 10.692) {
		t.Fatalf("读到 DOM 那一刻时钟读数 %.3f，应为 10.692：用例没有打到要测的情形", got)
	}
	evs := r.tickEvents(2002)
	want := []struct {
		typ, cause string
		line       int
	}{
		{player.EventAllLyrics, "late", -1},
		{player.EventLyricUpdate, "line", 0},
		{player.EventPositionSample, "sample", -1},
	}
	if len(evs) != len(want) {
		t.Fatalf("本拍应发 all_lyrics、第 0 行的 lyric_update 与 position_sample，实得 %v", evs)
	}
	for i, e := range evs {
		if e.typ != want[i].typ || e.cause != want[i].cause || e.line != want[i].line {
			t.Errorf("第 %d 条应为 %s（%s，行 %d），实得 %+v", i+1, want[i].typ, want[i].cause, want[i].line, e)
		}
		if e.at != 3002 || !inRange(e.pos, 11.692, 11.692) {
			t.Errorf("%s 应在 3002ms 发出、位置 11.692（发出那一刻的时钟读数），实得 %dms、%.3f", e.typ, e.at, e.pos)
		}
	}
	if len(r.adjusts) != 0 {
		t.Errorf("本拍不是跳秒，不该对表，实得 %v", r.adjusts)
	}

	// 其后 3 秒没有带位置的事件：前端一直在本拍落下的锚点上，应与后端同一条线。
	tl.play(r, 3100, 6000, replayPoll, replayRTT)
	if r.feAt != msAt(3002) {
		t.Fatalf("前端的锚点应仍是 3002ms 那一条，实得 %v（事件：%v）", r.feAt.Sub(syncT0), r.events)
	}
	if diff := r.clock.at(msAt(6000)) - r.frontPos(6000); !inRange(diff, 0, 0) {
		t.Errorf("前端比后端落后 %.3fs，应为 0（多出来的是阻塞时长）", diff)
	}
	if lag := tl.p(6000) - r.frontPos(6000); !inRange(lag, 0.30, 0.30) {
		t.Errorf("前端落后界面进度 %.3fs，应仍为 0.30s", lag)
	}
	if n := r.count("sync") + r.count("seek") + r.count("state"); n != 0 {
		t.Errorf("不该有 playback_resume / playback_pause，实得 %v", r.events)
	}
}

// TestReplayLateLyricsOnSyncTick 补发歌词与校准同拍（审查给出的时间线）：补发的事件排在对表之后，位置是
// 校准后的读数，本拍的校准量随之清零，拍末不再另发由校准触发的 playback_resume。
//
// p(ms) = ms − 9.05s（p 在 20050ms 过 11.000；时间零点在这段之前，p(0) 为负只是倒推）。上一拍 sent=20000、
// got=20002、秒数 10；本拍 sent=20100、got=20102、秒数 11，对表前时钟 10.30（落后 0.752s）：u = 0.102，
// 时钟拨到 10.70，改动 0.40s，达到通知门槛；此前没有发过通知，不受限频约束。本拍补发的是只有提示语的
// 那种：all_lyrics 加 index:-1 的 lyric_update。「补词阻塞 1 秒」一例里两条到 21102ms 才发出，位置是
// 校准后的时钟在那一刻的读数 11.70。
//
// 变异自证（注入在回放里，均实测）：补发放回对表之前，两例均红（两条的位置是对表前的 10.30 / 11.30，拍末
// 多发一条由校准触发的 playback_resume）；补发留在对表之后、位置却在对表之前取好，两例均红（位置同上，
// 不多发通知，前端比后端落后 0.40s）；补发的事件不清未告知量，两例均红（拍末多发一条 playback_resume）；
// posNow 取 got 时刻的读数，「补词阻塞 1 秒」一例红（位置 10.70）；只有提示语时不发 index:-1 的
// lyric_update，两例均红。
// 直接改 runSession——把 lateLyrics 的调用挪回对表之前——全仓测试仍绿（实测）：调用点靠人守。闭包里的
// 发出点改回 p.Emit 由 AST 门禁拦（player/cloudmusicanchor_lint_test.go，实测）。
func TestReplayLateLyricsOnSyncTick(t *testing.T) {
	cases := []struct {
		name    string
		blockMs int
		wantPos float32
	}{
		{"没有阻塞", 0, 10.70},
		{"补词阻塞 1 秒", 1000, 11.70},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tl := timeline{p0: -9050}
			r := newReplay(tl.p(0) - 0.752)
			r.tick(20_000, 20_002, tl.sec(20_000), true)
			if got := r.clock.at(msAt(20_102)); !inRange(got, 10.30, 10.30) {
				t.Fatalf("对表前时钟读数 %.3f，应为 10.30：用例没有打到要测的情形", got)
			}
			r.tickWith(20_100, 20_102, tl.sec(20_100), true, tickOpt{blockMs: c.blockMs, late: true})

			if len(r.adjusts) != 1 || r.adjusts[0].ms != 20_102 || !inRange(r.adjusts[0].delta, 0.40, 0.40) {
				t.Fatalf("应在 20102ms 向前校准 0.40s，实得 %v", r.adjusts)
			}
			evs := r.eventsAt(20_102)
			if len(evs) != 2 || evs[0].typ != player.EventAllLyrics || evs[1].typ != player.EventLyricUpdate || evs[1].line != -1 {
				t.Fatalf("本拍对外应只有 all_lyrics 与 index:-1 的 lyric_update，实得 %v", evs)
			}
			for _, e := range evs {
				if e.at != 20_102+c.blockMs || !inRange(e.pos, c.wantPos, c.wantPos) {
					t.Errorf("%s 应在 %dms 发出、位置 %.2f（对表之后的读数），实得 %dms、%.3f",
						e.typ, 20_102+c.blockMs, c.wantPos, e.at, e.pos)
				}
			}

			// 其后没有带位置的事件（无歌词的歌）：前端一直在本拍落下的锚点上。
			tl.play(r, 20_200+c.blockMs, 27_000, replayPoll, replayRTT)
			if n := r.count("sync"); n != 0 {
				t.Errorf("校准量已由补发的歌词带到，本拍及其后都不该有由校准触发的 playback_resume，实得 %v", r.syncEvents())
			}
			if late := r.adjustsAfter(20_102); len(late) != 0 {
				t.Fatalf("其后不该再校准，实得 %v：前端的断言要求后端的时间线不再变", late)
			}
			if diff := r.clock.at(msAt(27_000)) - r.frontPos(27_000); !inRange(diff, 0, 0) {
				t.Errorf("前端与后端相差 %.3fs，应在同一条（校准后的）时间线上", diff)
			}
			if lag := tl.p(27_000) - r.frontPos(27_000); !inRange(lag, domLagSec, domLagSec+replayU) {
				t.Errorf("前端落后界面进度 %.3fs，不在 [L, L+u] 之内", lag)
			}
		})
	}
}

// TestReplayLateLyricsWithoutSync 反例方向：补发歌词的那一拍没有校准、没有阻塞时，事件的位置就是那一拍的
// 时钟读数，不多发也不少发，时钟不动。
//
// p(ms) = 10s + ms，时钟 = p − 0.30。402ms、502ms 两拍秒数都是 10；502ms 那一拍补发歌词，时钟读数 10.202。
// 「补到歌词」一例的行匹配就地用上新歌词，本拍接着推第 0 行。
//
// 变异自证（注入在回放里，均实测）：补到的歌词推迟到行匹配之后才生效，「补到歌词」一例红（本拍没有推
// 第 0 行）；只有提示语时不发 index:-1 的 lyric_update，「只有提示语」一例红。
func TestReplayLateLyricsWithoutSync(t *testing.T) {
	cases := []struct {
		name   string
		lines  []float32
		causes []string // 本拍对外事件的起因，按先后
	}{
		{"只有提示语", nil, []string{"late", "late"}},
		{"补到歌词", []float32{5, 60}, []string{"late", "line"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tl := timeline{p0: 10_000}
			r := newReplay(tl.p(0) - 0.30)
			r.offset = 0.5
			r.tick(400, 402, tl.sec(400), true)
			r.tickWith(500, 502, tl.sec(500), true, tickOpt{late: true, lines: c.lines})

			evs := r.eventsAt(502)
			if len(evs) != len(c.causes) || evs[0].typ != player.EventAllLyrics || evs[1].typ != player.EventLyricUpdate {
				t.Fatalf("本拍对外应为 all_lyrics 与一条 lyric_update，实得 %v", evs)
			}
			for i, e := range evs {
				if e.cause != c.causes[i] || e.at != 502 || !inRange(e.pos, 10.202, 10.202) {
					t.Errorf("第 %d 条应为 %s、502ms 发出、位置 10.202（时钟读数），实得 %+v", i+1, c.causes[i], e)
				}
			}
			if len(r.adjusts) != 0 || r.count("sync") != 0 || r.count("seek") != 0 {
				t.Errorf("不该对表、不该发 playback_resume，实得校准 %v、事件 %v", r.adjusts, r.events)
			}
			if got := r.clock.at(msAt(502)); !inRange(got, 10.202, 10.202) {
				t.Errorf("时钟读数 %.3f，应仍为 10.202", got)
			}
			if diff := r.clock.at(msAt(900)) - r.frontPos(900); !inRange(diff, 0, 0) {
				t.Errorf("前端与后端相差 %.3fs，应为 0", diff)
			}
		})
	}
}

// TestReplayLateLyricsDuringDeferral 顺延期间补发歌词：补发的事件同样让前端落到后端的时间线上，未告知量
// 清零，限频间隔到了也不再补发通知。时间线同 TestReplayWideThenNarrowWindow：320ms 第一条通知，1105ms
// 的第二笔 0.215s 达门槛而被顺延（不清零的话在 5405ms 发出）；2005ms 那一拍补发歌词（只有提示语）。
//
// 变异自证（注入在回放里，实测）：补发的事件不清未告知量，本例红（5405ms 多发一条通知）。
func TestReplayLateLyricsDuringDeferral(t *testing.T) {
	tl := timeline{p0: 10_900}
	r := newReplay(tl.p(0) - 0.88)
	r.tick(0, 5, tl.sec(0), true)
	r.tick(100, 320, tl.sec(100), true)
	tl.play(r, 400, 2000, replayPoll, 5)
	if adj := r.adjustsAfter(0); len(adj) != 2 || adj[1].ms != 1105 || len(r.syncEvents()) != 1 {
		t.Fatalf("补发之前应有两次校准、一条通知（第二笔在顺延中），实得校准 %v、通知 %v", r.adjusts, r.syncEvents())
	}
	r.tickWith(2000, 2005, tl.sec(2000), true, tickOpt{late: true})
	tl.play(r, 2100, 9000, replayPoll, 5)

	// 补发的位置是那一刻的时钟读数：1105ms 拨到 11.70，其后走了 0.9 秒。
	evs := r.eventsAt(2005)
	if len(evs) != 2 || evs[0].cause != "late" || evs[1].cause != "late" || !inRange(evs[0].pos, 12.60, 12.60) || !inRange(evs[1].pos, 12.60, 12.60) {
		t.Fatalf("2005ms 应只发补发的两条、位置 12.60，实得 %v", evs)
	}
	if sync := r.syncEvents(); len(sync) != 1 || sync[0].ms != 320 {
		t.Errorf("顺延的那一笔已由补发的歌词带到，不该补发；应只有 320ms 的一条通知，实得 %v", sync)
	}
	if lag := tl.p(9000) - r.frontPos(9000); !inRange(lag, domLagSec, domLagSec+0.105) {
		t.Errorf("前端落后 %.3fs，不在 [L, L+u] 之内", lag)
	}
}
