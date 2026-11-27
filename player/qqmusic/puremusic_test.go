package qqmusic

// 本文件只测 QQ 音乐的无歌词提示语适配与共用识别（#231 B4，设计 doc/design/converge-player-behavior-231-design.md §7.4、§7.5、§7.8）
// 及其与 #248 本地首轮认定未决态（waitLocal）的组合行为：
//   1. scanLRC 补充适配三段冒号时间戳 [mm:ss:xx]（如 [00:00:00]此歌曲为没有填词的纯音乐，请您欣赏），不改变普通 LRC、QRC、[kana:] 与纯文本兜底解析；
//   2. lyricState.refresh 对在线（srcOnline）与关联/本地（srcAssoc、srcLocal）全部来源统一调用 player.PureMusicHint，
//      命中后清零显示行并保留提示原文进 lyricPrint.hint；且先判定提示再判定 #248 waitLocal 未决态；
//   3. 换歌拍与补取拍两条路径经 emitNoLyrics 发出空 all_lyrics 与 index:-1 的 lyric_update，Text 原样保留提示句（不含歌名行）。
//
// 变异自证（在隔离临时副本中实测变红，详见 task-4-report.md）：
//   - 删掉 scanLRC 的三段冒号分支 → 三段冒号提示语用例因 Text=="" 而红；
//   - lyricState.refresh 去掉 pureMusicHint → 全部提示语被当作第 0 行歌词而红；
//   - emitNoLyrics 忽略 hint 传空串 "" → Text 断言全红；
//   - #248 顺序颠倒（先判 waitLocal 后做 pureMusicHint）→ #248 + B4 组合用例因首轮 undecided==false 而红。

import (
	"reflect"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

const (
	qqPureHintText     = "此歌曲为没有填词的纯音乐，请您欣赏"
	sharedPureHintText = "纯音乐，请欣赏"
)

// drainNoLyricsEvents 读取 emitNoLyrics 发出的一对事件，校验顺序与载荷形状并返回。
func drainNoLyricsEvents(t *testing.T, p *QQMusicPlayer) (*player.AllLyricsData, *player.LyricUpdate) {
	t.Helper()
	var got []player.Event
	for len(p.EventCh) > 0 {
		got = append(got, <-p.EventCh)
	}
	if len(got) != 2 {
		t.Fatalf("emitNoLyrics 应发出 2 条事件，实得 %d 条: %+v", len(got), got)
	}
	if got[0].Type != player.EventAllLyrics || got[1].Type != player.EventLyricUpdate {
		t.Fatalf("事件顺序应为 all_lyrics → lyric_update，实得 %s → %s", got[0].Type, got[1].Type)
	}
	all, ok := got[0].Data.(*player.AllLyricsData)
	if !ok {
		t.Fatalf("all_lyrics Data 类型不符: %T", got[0].Data)
	}
	lu, ok := got[1].Data.(*player.LyricUpdate)
	if !ok {
		t.Fatalf("lyric_update Data 类型不符: %T", got[1].Data)
	}
	return all, lu
}

// TestPureMusicHintFiveFormsClaimAndBackfill 覆盖设计 §7.8 五种实测明文同形写法与通用提示语，
// 分别走换歌拍与补取拍两条路径经 parseLRC → lyricState → emitNoLyrics 端到端断言。
func TestPureMusicHintFiveFormsClaimAndBackfill(t *testing.T) {
	cases := []struct {
		name     string
		rawLRC   string
		title    string
		wantHint string
	}{
		{
			name:     "三段冒号：QQ实测纯音乐提示",
			rawLRC:   "[00:00:00]" + qqPureHintText,
			title:    "Kiss the Rain - Yiruma",
			wantHint: qqPureHintText,
		},
		{
			name:     "标准LRC：QQ纯音乐提示",
			rawLRC:   "[00:00.00]" + qqPureHintText,
			title:    "Kiss the Rain - Yiruma",
			wantHint: qqPureHintText,
		},
		{
			name:     "QRC：QQ纯音乐提示",
			rawLRC:   "[0,5000]此歌曲为(0,500)没有填词的纯音乐，请您欣赏(500,4500)",
			title:    "Kiss the Rain - Yiruma",
			wantHint: qqPureHintText,
		},
		{
			name:     "无时间戳纯文本：QQ纯音乐提示",
			rawLRC:   qqPureHintText,
			title:    "Kiss the Rain - Yiruma",
			wantHint: qqPureHintText,
		},
		{
			name:     "标准LRC：仅曲名行加提示行",
			rawLRC:   "[00:00.00]Kiss the Rain\n[00:05.00]" + qqPureHintText,
			title:    "Kiss the Rain",
			wantHint: qqPureHintText,
		},
		{
			name:     "标准LRC：歌名-歌手行加提示行",
			rawLRC:   "[00:00.00]Kiss the Rain - Yiruma\n[00:05.00]" + qqPureHintText,
			title:    "Kiss the Rain - Yiruma",
			wantHint: qqPureHintText,
		},
		{
			name:     "标准LRC：歌名-歌手无空格差异加提示行",
			rawLRC:   "[00:00.00]Kiss the Rain-Yiruma\n[00:05.00]" + qqPureHintText,
			title:    "Kiss the Rain - Yiruma",
			wantHint: qqPureHintText,
		},
		{
			name:     "三段冒号：通用提示",
			rawLRC:   "[00:00:00]" + sharedPureHintText,
			title:    "Road to You - Ryan Farish",
			wantHint: sharedPureHintText,
		},
		{
			name:     "标准LRC：通用提示",
			rawLRC:   "[00:00.00]" + sharedPureHintText,
			title:    "Road to You - Ryan Farish",
			wantHint: sharedPureHintText,
		},
	}

	const (
		durMs    uint32  = 235000
		durSec   float32 = 235.0
		posSec   float32 = 12.5
		progress float32 = posSec / durSec
	)

	assertEmitted := func(t *testing.T, p *QQMusicPlayer, title, wantHint string) {
		t.Helper()
		all, lu := drainNoLyricsEvents(t, p)
		if all.Title != title || all.Count != 0 || all.Lyrics == nil || len(all.Lyrics) != 0 {
			t.Fatalf("all_lyrics 应为空非 nil 切片且 title=%q，实得 %+v", title, all)
		}
		if lu.Index != -1 || lu.Text != wantHint || lu.SubText != "" || lu.Timestamp != 0 {
			t.Fatalf("lyric_update 应为 index=-1 且 Text=%q（不含歌名行），实得 %+v", wantHint, lu)
		}
		if lu.Position != posSec || lu.Progress != progress {
			t.Fatalf("lyric_update 位置/进度不符：got (%v, %v), want (%v, %v)", lu.Position, lu.Progress, posSec, progress)
		}
	}

	for _, c := range cases {
		t.Run("换歌拍/"+c.name, func(t *testing.T) {
			parsed, _, _ := parseLRC(c.rawLRC, durMs)
			if len(parsed) == 0 {
				t.Fatalf("parseLRC(%q) 解析出 0 行，无法交给共用提示识别", c.rawLRC)
			}

			var st lyricState
			st.reset(1, false, time.Time{})
			st.setTitle(c.title)
			st.setOnlineRaw(parsed)
			lines, changed := st.refresh(true, stateOffsetSec)
			if !changed || len(lines) != 0 || st.undecided() {
				t.Fatalf("换歌拍 refresh 应归一为空行且已决：changed=%v len=%d undecided=%v", changed, len(lines), st.undecided())
			}
			if st.hint() != c.wantHint {
				t.Fatalf("lyricState.hint() = %q, want %q", st.hint(), c.wantHint)
			}
			if lyricsUndecided(lines, false, st.source(), st.undecided()) {
				t.Fatal("已决纯音乐提示的 lyricsUndecided 应为 false")
			}

			// 同一结果再次到达不重发且 hint 保持。
			if l2, ch2 := st.apply(onlineRes(1, 0), true, stateOffsetSec); ch2 || l2 != nil || st.hint() != c.wantHint {
				t.Fatalf("相同纯音乐提示不应重发：changed=%v len=%d hint=%q", ch2, len(l2), st.hint())
			}

			p := New(0, 50)
			p.emitNoLyrics(c.title, durSec, posSec, progress, st.hint())
			assertEmitted(t, p, c.title, c.wantHint)
		})

		t.Run("补取拍/"+c.name, func(t *testing.T) {
			var st lyricState
			st.reset(1, false, time.Time{})
			st.setTitle(c.title)
			// 换歌拍编号缺位：先登记「无」，补取进行中。
			st.refresh(false, stateOffsetSec)
			if st.hint() != "" {
				t.Fatalf("编号缺位登记后 hint 应为空，实得 %q", st.hint())
			}

			// 补取拍拿到编号与在线纯音乐提示。
			parsed, _, _ := parseLRC(c.rawLRC, durMs)
			st.setOnlineRaw(parsed)
			lines, changed := st.refresh(true, stateOffsetSec)
			if !changed || len(lines) != 0 || st.source() != srcOnline || st.undecided() {
				t.Fatalf("补取拍 refresh 应归一为空行且已决：changed=%v len=%d src=%v undecided=%v",
					changed, len(lines), st.source(), st.undecided())
			}
			if st.hint() != c.wantHint {
				t.Fatalf("补取拍 lyricState.hint() = %q, want %q", st.hint(), c.wantHint)
			}

			p := New(0, 50)
			p.emitNoLyrics(c.title, durSec, posSec, progress, st.hint())
			assertEmitted(t, p, c.title, c.wantHint)
		})
	}
}

// TestPureMusicHintWithAssoc248WaitLocal 验证 B4 与 #248（本地首轮认定未决态）的组合不变量：
// waitLocal == true 且在线返回三段冒号纯音乐提示时，首轮必须进入 undecided（不抢发 index:-1）：
//  1. 读取者回 srcLocal 49 行 → 直接发 49 行，hint 为空，全程 0 次 index:-1；
//  2. 读取者回 srcOnline / resUnknown / resStopped 或满 localDecideWindow 的 expireWait
//     → 解除未决并发出 index:-1，Text 仍完整保留提示原文，且不打多余来源切换日志；
//  3. 补取拍快于首轮 RM 拿到三段冒号提示 → 首拍同样未决，解除后补发带提示原文的 index:-1。
func TestPureMusicHintWithAssoc248WaitLocal(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	dl := t0.Add(localDecideWindow)
	const title = "Kiss the Rain - Yiruma"
	rawHint, _, _ := parseLRC("[00:00:00]"+qqPureHintText, 235000)
	if len(rawHint) != 1 {
		t.Fatalf("三段冒号纯音乐提示应解析为 1 行，实得 %d 行", len(rawHint))
	}

	t.Run("在线三段冒号提示未决期间，读取者回srcLocal49行直接发49行且全程0次index:-1", func(t *testing.T) {
		var st lyricState
		st.reset(1, true, dl)
		st.setTitle(title)
		st.setOnlineRaw(rawHint)

		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 0 || st.source() != srcOnline || !st.undecided() {
			t.Fatalf("首轮在线纯音乐提示在 waitLocal=true 时应进入未决态：changed=%v len=%d src=%v undecided=%v",
				changed, len(lines), st.source(), st.undecided())
		}
		if !lyricsUndecided(lines, false, st.source(), st.undecided()) {
			t.Fatal("首轮未决期间 lyricsUndecided 应为 true（不抢发 index:-1）")
		}

		// 读取者回 srcLocal 49 行：解除未决、直接发 49 行、hint 清空。
		local49 := desiredRes(1, srcLocal, -1000, makeLines248(49, "本地"), "local49")
		local49.localPath = widF
		ch := make(chan assocResult, 1)
		ch <- local49
		got, changed := takeResult(&st, ch, true, stateOffsetSec)
		if !changed || len(got) != 49 || st.source() != srcLocal || st.undecided() || st.hint() != "" {
			t.Fatalf("收到本地 49 行应解除未决并清 hint：changed=%v len=%d src=%v undecided=%v hint=%q",
				changed, len(got), st.source(), st.undecided(), st.hint())
		}
		if lyricsUndecided(got, false, st.source(), st.undecided()) {
			t.Fatal("发出本地 49 行后 lyricsUndecided 应为 false")
		}
	})

	resolveCases := []struct {
		name    string
		resolve func(t *testing.T, st *lyricState) ([]lyricLine, bool)
	}{
		{
			name: "读取者回srcOnline",
			resolve: func(t *testing.T, st *lyricState) ([]lyricLine, bool) {
				ch := make(chan assocResult, 1)
				ch <- onlineRes(1, 0)
				return takeResult(st, ch, true, stateOffsetSec)
			},
		},
		{
			name: "读取者回resUnknown",
			resolve: func(t *testing.T, st *lyricState) ([]lyricLine, bool) {
				ch := make(chan assocResult, 1)
				ch <- assocResult{gen: 1, kind: resUnknown}
				return takeResult(st, ch, true, stateOffsetSec)
			},
		},
		{
			name: "读取者回resStopped",
			resolve: func(t *testing.T, st *lyricState) ([]lyricLine, bool) {
				ch := make(chan assocResult, 1)
				ch <- stoppedRes(1)
				return takeResult(st, ch, true, stateOffsetSec)
			},
		},
		{
			name: "满1s调用expireWait",
			resolve: func(t *testing.T, st *lyricState) ([]lyricLine, bool) {
				if l, ch := st.expireWait(dl.Add(-time.Millisecond), true, stateOffsetSec); ch || l != nil || !st.undecided() {
					t.Fatalf("到期前 1ms 不应提前解除未决态：changed=%v undecided=%v", ch, st.undecided())
				}
				return st.expireWait(dl, true, stateOffsetSec)
			},
		},
	}

	for _, c := range resolveCases {
		t.Run("未决解除补发带原文的index:-1/"+c.name, func(t *testing.T) {
			logs := captureAssocLog(t)
			var st lyricState
			st.reset(1, true, dl)
			st.setTitle(title)
			st.setOnlineRaw(rawHint)
			lines, changed := st.refresh(true, stateOffsetSec)
			if !changed || len(lines) != 0 || !st.undecided() {
				t.Fatalf("首轮未决态不符：changed=%v len=%d undecided=%v", changed, len(lines), st.undecided())
			}

			got, changed := c.resolve(t, &st)
			if !changed || len(got) != 0 || st.undecided() || st.source() != srcOnline {
				t.Fatalf("%s 解除后状态不符：changed=%v len=%d undecided=%v src=%v",
					c.name, changed, len(got), st.undecided(), st.source())
			}
			if st.hint() != qqPureHintText {
				t.Fatalf("%s 解除后 hint = %q, want %q", c.name, st.hint(), qqPureHintText)
			}
			if lyricsUndecided(got, false, st.source(), st.undecided()) {
				t.Fatalf("%s 解除后 lyricsUndecided 应为 false", c.name)
			}
			if n := countLog(logs(), "*", "歌词来源改为"); n != 0 {
				t.Fatalf("%s 解除未决（srcOnline→srcOnline）不应打来源切换日志，实得 %d 条：\n%s", c.name, n, logs())
			}

			p := New(0, 50)
			p.emitNoLyrics(title, 235, 3.0, 3.0/235.0, st.hint())
			all, lu := drainNoLyricsEvents(t, p)
			if all.Count != 0 || lu.Index != -1 || lu.Text != qqPureHintText {
				t.Fatalf("%s 补发事件不符：all=%+v lu=%+v", c.name, all, lu)
			}
		})
	}

	t.Run("补取快于首轮RM拿到三段冒号提示同样受waitLocal保护", func(t *testing.T) {
		var st lyricState
		st.reset(1, true, dl)
		st.setTitle(title)
		st.refresh(false, stateOffsetSec) // 换歌无编号

		// 补取拿到编号与三段冒号提示（首轮 RM 尚未回）
		st.setOnlineRaw(rawHint)
		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 0 || !st.undecided() || st.source() != srcOnline {
			t.Fatalf("补取快于 RM 时应进入未决态：changed=%v len=%d undecided=%v src=%v",
				changed, len(lines), st.undecided(), st.source())
		}
		// 满 1s expireWait 解除后发出带提示原文的 index:-1
		got, changed := st.expireWait(dl, true, stateOffsetSec)
		if !changed || len(got) != 0 || st.undecided() || st.hint() != qqPureHintText {
			t.Fatalf("expireWait 解除后应保留提示原文：changed=%v len=%d undecided=%v hint=%q",
				changed, len(got), st.undecided(), st.hint())
		}
	})
}

// TestPureMusicHintAssocAndLocalSources 验证关联/本地歌词（srcAssoc、srcLocal）本身为纯音乐提示语时
// 同样经共用 player.PureMusicHint 归一为空行并保留原文，且切回普通歌词或换歌 reset 后 hint 正确清零。
func TestPureMusicHintAssocAndLocalSources(t *testing.T) {
	const title = "Kiss the Rain - Yiruma"

	cases := []struct {
		name     string
		src      lyricSource
		rawFile  string
		wantHint string
	}{
		{
			name:     "srcAssoc：三段冒号单行提示",
			src:      srcAssoc,
			rawFile:  "[00:00:00]" + qqPureHintText + "\n",
			wantHint: qqPureHintText,
		},
		{
			name:     "srcAssoc：歌名行加通用提示",
			src:      srcAssoc,
			rawFile:  "[00:00.00]Kiss the Rain-Yiruma\n[00:05.00]" + sharedPureHintText + "\n",
			wantHint: sharedPureHintText,
		},
		{
			name:     "srcLocal：三段冒号单行提示",
			src:      srcLocal,
			rawFile:  "[00:00:00]" + qqPureHintText + "\n",
			wantHint: qqPureHintText,
		},
		{
			name:     "srcLocal：标准LRC单行通用提示",
			src:      srcLocal,
			rawFile:  "[00:00.00]" + sharedPureHintText + "\n",
			wantHint: sharedPureHintText,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed, _, _ := parseLRCLines(c.rawFile)
			if len(parsed) == 0 {
				t.Fatalf("parseLRCLines(%q) 解析出 0 行", c.rawFile)
			}

			var st lyricState
			st.reset(1, false, time.Time{})
			st.setTitle(title)
			res := desiredRes(1, c.src, 0, parsed, c.name)
			if c.src == srcLocal {
				res.localPath = localFlac
			}

			lines, changed := st.apply(res, c.src == srcAssoc, stateOffsetSec)
			if !changed || len(lines) != 0 || st.source() != c.src || st.undecided() {
				t.Fatalf("关联/本地纯音乐提示应归一为空行且已决：changed=%v len=%d src=%v undecided=%v",
					changed, len(lines), st.source(), st.undecided())
			}
			if st.hint() != c.wantHint {
				t.Fatalf("hint = %q, want %q", st.hint(), c.wantHint)
			}

			p := New(0, 50)
			p.emitNoLyrics(title, 200, 10, 0.05, st.hint())
			all, lu := drainNoLyricsEvents(t, p)
			if all.Count != 0 || lu.Index != -1 || lu.Text != c.wantHint {
				t.Fatalf("发出事件不符：all=%+v lu=%+v", all, lu)
			}

			// 换歌 reset 后无提示零行不串用上一首 hint。
			st.reset(2, false, time.Time{})
			st.setTitle("Another Song - Artist")
			st.setOnlineRaw(nil)
			l2, ch2 := st.refresh(true, stateOffsetSec)
			if !ch2 || len(l2) != 0 || st.hint() != "" {
				t.Fatalf("换歌 reset 后 hint 应清空，实得 changed=%v len=%d hint=%q", ch2, len(l2), st.hint())
			}
		})
	}

	t.Run("同源仅提示语变化触发重发，切回普通歌词清空hint", func(t *testing.T) {
		var st lyricState
		st.reset(1, false, time.Time{})
		st.setTitle(title)
		st.setOnlineRaw(nil)
		if l, ch := st.refresh(true, stateOffsetSec); !ch || len(l) != 0 || st.hint() != "" {
			t.Fatalf("首次空行状态不符：changed=%v len=%d hint=%q", ch, len(l), st.hint())
		}
		// 变为带提示语的在线歌词：指纹（content 与 hint）变化触发重发。
		hintLines, _, _ := parseLRC("[00:00:00]"+qqPureHintText, 200000)
		st.setOnlineRaw(hintLines)
		if l, ch := st.refresh(true, stateOffsetSec); !ch || len(l) != 0 || st.hint() != qqPureHintText {
			t.Fatalf("零行变提示语应触发重发：changed=%v len=%d hint=%q", ch, len(l), st.hint())
		}
		// 切换到有词的关联歌词：hint 必须清零。
		if l, ch := st.apply(desiredRes(1, srcAssoc, 0, assocFixture("A"), "A"), true, stateOffsetSec); !ch || len(l) != 2 || st.hint() != "" {
			t.Fatalf("切换到普通歌词应清 hint：changed=%v len=%d hint=%q", ch, len(l), st.hint())
		}
	})
}

// TestPureMusicHintFalsePositivesAndParserRegression 验证形状闸门反例与解析器既有行为无回归。
func TestPureMusicHintFalsePositivesAndParserRegression(t *testing.T) {
	t.Run("两行正常歌词中含提示语不误判", func(t *testing.T) {
		raw := "[00:00.00]" + qqPureHintText + "\n[00:05.00]这其实是第一句真正的歌词\n"
		parsed, _, _ := parseLRC(raw, 200000)
		var st lyricState
		st.reset(1, false, time.Time{})
		st.setTitle("普通歌 - 歌手")
		st.setOnlineRaw(parsed)
		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 2 || st.hint() != "" {
			t.Fatalf("两行正常歌词含提示语不应误判：changed=%v len=%d hint=%q", changed, len(lines), st.hint())
		}
	})

	t.Run("歌名行与标题不同不误判", func(t *testing.T) {
		raw := "[00:00.00]Other Song - Yiruma\n[00:05.00]" + qqPureHintText + "\n"
		parsed, _, _ := parseLRC(raw, 200000)
		var st lyricState
		st.reset(1, false, time.Time{})
		st.setTitle("Kiss the Rain - Yiruma")
		st.setOnlineRaw(parsed)
		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 2 || st.hint() != "" {
			t.Fatalf("歌名行与标题不同不应误判：changed=%v len=%d hint=%q", changed, len(lines), st.hint())
		}
	})

	t.Run("三段冒号非提示普通歌词按百分秒换算", func(t *testing.T) {
		got, _, _ := parseLRC("[01:02:50]普通歌词\n[00:05:00]   \n", 200000)
		if len(got) != 1 {
			t.Fatalf("应解析出 1 行非空普通歌词，实得 %d 行: %+v", len(got), got)
		}
		if got[0].Time != 62.5 || got[0].Text != "普通歌词" {
			t.Fatalf("got = {Time:%v Text:%q}, want {Time:62.5 Text:\"普通歌词\"}", got[0].Time, got[0].Text)
		}
		var st lyricState
		st.reset(1, false, time.Time{})
		st.setTitle("某歌 - 某人")
		st.setOnlineRaw(got)
		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 1 || lines[0].Text != "普通歌词" || st.hint() != "" {
			t.Fatalf("三段冒号普通歌词不应被当作纯音乐提示：changed=%v lines=%+v hint=%q", changed, lines, st.hint())
		}
	})

	t.Run("普通LRC含超99分钟与带kana的QRC解析不变", func(t *testing.T) {
		lrcGot, name, singer := parseLRC("[ti:长混音]\n[ar:DJ]\n[02:04.45]前半\n[100:05.00]后半\n", 7200000)
		if name != "长混音" || singer != "DJ" || len(lrcGot) != 2 ||
			lrcGot[0].Time != 124.45 || lrcGot[0].Text != "前半" ||
			lrcGot[1].Time != 6005.0 || lrcGot[1].Text != "后半" {
			t.Fatalf("普通 LRC 解析回归：name=%q singer=%q lines=%+v", name, singer, lrcGot)
		}

		qrcIn := "[kana:1か(1000,200)い(1200,200)1て(1400,200)い(1600,200)]\n[1000,1000]海(1000,400)底(1400,400)\n"
		qrcGot, _, _ := parseLRC(qrcIn, 60000)
		wantFuri := player.LyricFurigana{
			{Text: "海", Ruby: "かい"},
			{Text: "底", Ruby: "てい"},
		}
		if len(qrcGot) != 1 || qrcGot[0].Text != "海底" || len(qrcGot[0].Detailed.Words) != 2 ||
			!reflect.DeepEqual(qrcGot[0].Furigana, wantFuri) {
			t.Fatalf("带 [kana:] 的 QRC 解析回归：lines=%+v", qrcGot)
		}
	})
}

// TestPureMusicHintBackfillSupplementedMetadata 回归 #229 × #231 B4 交互：
// 无编号认领时歌手尚未落位（currentTitle 冻结为 "Kiss the Rain - "），随后补取拿到带 [ar:Yiruma] 与
// "Kiss the Rain - Yiruma" 歌名行的双行纯音乐提示时，无论 song-info-keep-first 开关开启还是关闭：
//  1. 在线双行提示按补取读到的元数据标题（onlineTitle）识别，发出空 all_lyrics 与带提示原文的 index:-1；
//  2. 对外宣告的 all_lyrics.Title 仍严格保持认领时的 currentTitle（"Kiss the Rain - "）；
//  3. 若本首已采纳本地文件关联（srcLocal），setOnlineTitle 不影响本地双行提示继续按认领标题判定。
func TestPureMusicHintBackfillSupplementedMetadata(t *testing.T) {
	rawBackfill := "[ti:Kiss the Rain]\n[ar:Yiruma]\n[00:00.00]Kiss the Rain - Yiruma\n[00:05.00]" + qqPureHintText + "\n"
	lines, lrcName, lrcSinger := parseLRC(rawBackfill, 235000)
	if len(lines) != 2 || lrcName != "Kiss the Rain" || lrcSinger != "Yiruma" {
		t.Fatalf("前置解析不符：lrcName=%q lrcSinger=%q lines=%+v", lrcName, lrcSinger, lines)
	}

	for _, keepFirst := range []bool{true, false} {
		name := "keepFirst=true"
		if !keepFirst {
			name = "keepFirst=false"
		}
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { player.SetSongInfoKeepFirst(true) })
			player.SetSongInfoKeepFirst(keepFirst)

			p := New(0, 50)
			// 1. 无编号认领：歌手为空，对外 currentTitle 冻结为 "Kiss the Rain - "
			claimedName, claimedSinger := "Kiss the Rain", ""
			currentTitle := claimedName + " - " + claimedSinger
			var st lyricState
			st.reset(1, false, time.Time{})
			st.setTitle(currentTitle)
			st.refresh(false, stateOffsetSec)

			// 2. 补取拿到在线双行提示与歌手标签 Yiruma
			readName, readSinger := lrcName, lrcSinger
			if readName == "" {
				readName = claimedName
			}
			if readSinger == "" {
				readSinger = claimedSinger
			}
			gotName, gotSinger := backfillNames(claimedName, claimedSinger, claimedName, claimedSinger, lrcName, lrcSinger)
			if gotName != "Kiss the Rain" || gotSinger != "Yiruma" {
				t.Fatalf("backfillNames = (%q, %q), 期望 (\"Kiss the Rain\", \"Yiruma\")", gotName, gotSinger)
			}

			st.setOnlineTitle(readName + " - " + readSinger)
			st.setOnlineRaw(lines)
			shown, changed := st.refresh(true, stateOffsetSec)
			if !changed || len(shown) != 0 || st.hint() != qqPureHintText {
				t.Fatalf("补取双行纯音乐提示应被识别为空行并保留提示语：changed=%v shown=%+v hint=%q", changed, shown, st.hint())
			}

			// showLyrics 对外发出时仍用冻结的 currentTitle
			p.emitNoLyrics(currentTitle, 235.0, 1.5, 1.5/235.0, st.hint())
			all, lu := drainNoLyricsEvents(t, p)
			if all.Title != "Kiss the Rain - " || all.Count != 0 || len(all.Lyrics) != 0 {
				t.Fatalf("all_lyrics 应保持认领标题且为空歌词：%+v", all)
			}
			if lu.Index != -1 || lu.Text != qqPureHintText {
				t.Fatalf("lyric_update 应为 index:-1 且保留提示原文：%+v", lu)
			}
		})
	}

	t.Run("本地文件关联srcLocal双行提示不受补取setOnlineTitle覆盖", func(t *testing.T) {
		localLines, _, _ := parseLRCLines("[00:00.00]本地曲 - \n[00:05.00]" + sharedPureHintText + "\n")
		var st lyricState
		st.reset(1, false, time.Time{})
		st.setTitle("本地曲 - ")
		res := assocResult{
			gen:    1,
			kind:   resDesired,
			source: srcLocal,
			lines:  localLines,
			digest: [32]byte{1},
		}
		shown, changed := st.apply(res, false, stateOffsetSec)
		if !changed || len(shown) != 0 || st.hint() != sharedPureHintText {
			t.Fatalf("前置：本地双行提示应识别成功，实得 changed=%v len=%d hint=%q", changed, len(shown), st.hint())
		}

		// 补取随后写入不同的在线元数据标题与在线歌词，不得破坏已采纳的 srcLocal 提示判定
		st.setOnlineTitle("Online Song - Artist")
		st.setOnlineRaw(lines)
		shown2, changed2 := st.refresh(true, stateOffsetSec)
		if changed2 || shown2 != nil || st.source() != srcLocal || st.hint() != sharedPureHintText {
			t.Fatalf("补取 setOnlineTitle 不应改变已采纳的 srcLocal 提示判定：changed=%v src=%v hint=%q", changed2, st.source(), st.hint())
		}
	})
}
