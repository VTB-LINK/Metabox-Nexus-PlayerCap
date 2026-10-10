package applemusic

// 本文件只测位置结构的发现与替换：认得多快（附加时没在放、暂停中启动后点了别的歌、确认后的结构停住
// 而另一处在走、读不出后重新找到），以及不该认的不认（刚启动时的进度条、没有歌名时在走的东西）。

import (
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// waitSongInfo 逐秒推进，返回出现 song_info_update 用了几秒；limit 秒内没出现返回 -1。
func waitSongInfo(h *harness, limit int) int {
	from := h.mark()
	for i := 1; i <= limit; i++ {
		h.run(time.Second)
		if count(h.since(from), player.EventSongInfoUpdate) > 0 {
			return i
		}
	}
	return -1
}

// coldStartWorst 空闲附加（真结构是 [0][0][0]、没有存储）一百多秒后放第一首，存储比时长早 lead 出现。
// 把「开播落在周期全扫的哪个相位」扫一遍（每 100ms 一档，覆盖 15s），返回从时长写入（开播）到宣告的
// 最坏值。
func coldStartWorst(t *testing.T, lead time.Duration) time.Duration {
	t.Helper()
	var worst time.Duration
	for phase := time.Duration(0); phase < 15*time.Second; phase += step {
		h := newHarness(t)
		h.f.twins[0x2000] = mem.Twin{Dur: 180, Pos: 0} // 形态相同的静止结构
		h.setTwin(0, 0, false)
		h.run(100*time.Second + phase)
		h.f.songs = np("第一首")
		h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(210.5, "第一行"), Count: 1}}
		h.run(lead)
		h.setTwin(210, 0, true)
		d := waitAnnounce(h, 70*time.Second)
		if d < 0 {
			t.Fatalf("空闲 %v 后放第一首（存储早 %v），70s 内没认出", 100*time.Second+phase, lead)
		}
		worst = max(worst, d)
	}
	return worst
}

// 附加时 Apple Music 还没放过歌（真结构是 [0][0][0]，进不了候选表，内存里也没有「正在播放」存储）：
// 过了一百多秒才放第一首。没见到存储时周期全扫只找存储、封顶 5s（idleScanSteps）；认出存储后立即补一次
// 收候选的全扫，再加判活（约 2s）即认出。离线回放量得从开播到宣告的最坏值：存储与时长同时出现 7.0s，
// 存储早 1s 出现（真机手动点播的常态）6.0s；改动前（候选扫描封顶 15s、每次都收候选）同一回放是 17.0s
// 与 16.0s。假内存的全扫不耗时、位置每秒整一格；真机端到端 2026-10-09 量到一次约 4s。
//
// 变异自证：给 idleScanSteps 末尾加两档 5s、15s（封顶改回 15s），最坏值回到 17.0s 与 16.0s 而红；把
// applySnapshot 里的 `s.restartTwinScan(now, !collected)` 改成 `s.restartTwinScan(now, false)`（认出
// 存储后不立即补扫，等 2s），最坏值各多出 1.8s 而红。
func TestDiscoveryFirstSongAfterIdleAttach(t *testing.T) {
	for _, tc := range []struct {
		lead, limit time.Duration
	}{
		{0, 7 * time.Second},
		{time.Second, 6 * time.Second},
	} {
		worst := coldStartWorst(t, tc.lead)
		t.Logf("存储比时长早 %v：从开播到宣告最坏 %v", tc.lead, worst)
		if worst > tc.limit {
			t.Errorf("存储比时长早 %v：从开播到宣告最坏 %v，应 ≤ %v", tc.lead, worst, tc.limit)
		}
	}
}

// 空闲附加期间（没有存储、没有歌）：除本会话第一次外，每次全扫都不收候选——没见到存储时
// watchCandidates 本就不认任何候选，而收候选的全扫更慢（耗时见 idleScanSteps）。认出存储的那次全扫之后
// 立即补一次收候选的。
//
// 变异自证：把 maybeScan 里未确认位置结构时的 `collect = s.store.Sig != nil || s.lastScanEnd.IsZero()`
// 改回 `collect = true`（每次都收），空闲期间的全扫都在收候选而红。
func TestDiscoveryIdleScansSkipCandidates(t *testing.T) {
	h := newHarness(t)
	h.f.twins[0x2000] = mem.Twin{Dur: 180, Pos: 0}
	h.setTwin(0, 0, false)
	h.run(60 * time.Second)
	if len(h.f.wants) < 10 || !h.f.wants[0] {
		t.Fatalf("前置：60s 内应有十几次全扫，且第一次收候选，got %v", h.f.wants)
	}
	if slices.Contains(h.f.wants[1:], true) {
		t.Fatalf("没见到存储时，第一次之后的全扫不应收候选，got %v", h.f.wants)
	}

	idle := len(h.f.wants)
	h.f.songs = np("第一首")
	h.setTwin(210, 0, true)
	if d := waitAnnounce(h, 30*time.Second); d < 0 {
		t.Fatal("放第一首后 30s 内应认出")
	}
	after := h.f.wants[idle:]
	if len(after) < 2 || after[0] || !after[1] || h.scanAt[idle+1].Sub(h.scanAt[idle]) > minScanGap+2*step {
		t.Fatalf("认出存储的那次全扫不收候选，随后应立即（%v 内）补一次收候选的，got %v，两次相隔 %v",
			minScanGap+2*step, after, h.scanAt[idle+1].Sub(h.scanAt[idle]))
	}
}

// 附加时歌已在放：本会话第一次全扫就收候选，认出时间不因「没见到存储不收候选」变慢（改动前后都是
// 2.8s，离线回放）。
//
// 变异自证：把 maybeScan 里的 `|| s.lastScanEnd.IsZero()` 去掉（第一次全扫也要先见到存储才收候选），
// 第一次全扫不收候选而红。本回放里认出时间在该变异下仍是 2.8s（补扫只晚 0.3s，判活落在同样的两跳上），
// 时间那一条断言守的是别的改动把它拖慢。
func TestDiscoveryAttachWhilePlayingCollectsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("附加时在放")
	h.setTwin(240, 37, true)
	d := waitAnnounce(h, 30*time.Second)
	t.Logf("附加时歌已在放：%v 认出", d)
	if d < 0 || d > 2800*time.Millisecond {
		t.Fatalf("附加时歌已在放，%v 才认出（-1 = 30s 内没认出），应 ≤ 2.8s", d)
	}
	if !h.f.wants[0] {
		t.Fatal("本会话第一次全扫应收候选")
	}
}

// 存储出现了、时长却迟迟不写入（暂停中点了歌，几十秒后才按播放）：见到存储后按 twinScanSteps 继续收
// 候选（立即补扫，然后 2/3/5/5/15s），按下播放后在封顶档加判活之内认出。
//
// 变异自证：把 twinScanTable 改成一律返回 idleScanSteps（见到存储也封顶 5s），间隔成了 2/3/5/5/5 而红；
// 把 maybeScan 里未确认位置结构时的 collect 改成只在第一次全扫收（去掉 `s.store.Sig != nil ||`），见到
// 存储后的全扫都不收候选而红；给 twinScanSteps 末尾加一档 60s，按下播放后 40s 内认不出而红。
func TestDiscoveryStoreSeenLongBeforePlay(t *testing.T) {
	h := newHarness(t)
	h.setTwin(0, 0, false)
	h.run(30 * time.Second)
	idle := len(h.f.wants)

	h.f.songs = np("点了没放")
	h.run(40 * time.Second)
	seen := h.f.wants[idle:]
	if len(seen) < 2 || seen[0] || slices.Contains(seen[1:], false) {
		t.Fatalf("认出存储之后的全扫都应收候选，got %v", seen)
	}
	// 认出存储后的节奏：立即补扫，然后 2/3/5/5/15s。
	var gaps []time.Duration
	for i := idle + 2; i < len(h.scanAt); i++ {
		gaps = append(gaps, h.scanAt[i].Sub(h.scanAt[i-1]).Round(time.Second))
	}
	if want := []time.Duration{2 * time.Second, 3 * time.Second, 5 * time.Second, 5 * time.Second, 15 * time.Second}; !slices.Equal(gaps[:min(len(gaps), 5)], want) {
		t.Fatalf("见到存储后收候选的间隔应为 %v，got %v", want, gaps)
	}

	h.setTwin(210, 0, true)
	d := waitAnnounce(h, 40*time.Second)
	t.Logf("见到存储 40s 后按下播放：%v 认出", d)
	if d < 0 || d > 19*time.Second {
		t.Fatalf("按下播放后 %v 才认出（-1 = 40s 内没认出），应 ≤ 19s", d)
	}
}

// 认出存储后立即补的那次全扫，时长还是 0（真结构还进不了候选表）：下一档（2s 后）再扫到并认出，不必
// 等到后面的档。
//
// 变异自证：把 restartTwinScan 里的 `s.twinScan.reset(now)` 改成只置 `s.twinScan.next = now`（不回到
// 第一档），补扫之后接着的是封顶的 15s 一档，开播后 15.7s 才认出而红。
func TestDiscoverySupplementScanBeforeDuration(t *testing.T) {
	h := newHarness(t)
	h.setTwin(0, 0, false)
	h.run(30 * time.Second)
	idle := len(h.f.wants)
	h.f.songs = np("慢一点开播")
	for i := 0; i < 80 && len(h.f.wants) < idle+2; i++ {
		h.run(step)
	}
	if got := h.f.wants[idle:]; len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("前置：应先有一次不收候选的全扫认出存储、再补一次收候选的，got %v", got)
	}
	h.run(500 * time.Millisecond)
	h.setTwin(210, 0, true) // 补扫之后 0.5s 时长才写入
	d := waitAnnounce(h, 30*time.Second)
	t.Logf("补扫之后 0.5s 开播：%v 认出", d)
	if d < 0 || d > 5*time.Second {
		t.Fatalf("补扫时时长还是 0，开播后 %v 才认出（-1 = 30s 内没认出），应 ≤ 5s", d)
	}
}

// checkSupplement 断言下标 i 的全扫（不收候选、认出了存储）之后立即补了一次收候选的，其后按 2/3/5/5/15s
// （twinScanSteps）收候选。
func checkSupplement(t *testing.T, h *harness, i int, what string) {
	t.Helper()
	if len(h.f.wants) < i+2 || h.f.wants[i] || !h.f.wants[i+1] || h.scanAt[i+1].Sub(h.scanAt[i]) > minScanGap+2*step {
		t.Fatalf("%s：认出存储的那次全扫不收候选，随后应立即（%v 内）补一次收候选的，got %v", what, minScanGap+2*step, h.f.wants[min(i, len(h.f.wants)):])
	}
	want := []time.Duration{2 * time.Second, 3 * time.Second, 5 * time.Second, 5 * time.Second, 15 * time.Second}
	var gaps []time.Duration
	for j := i + 2; j < len(h.scanAt) && len(gaps) < len(want); j++ {
		gaps = append(gaps, h.scanAt[j].Sub(h.scanAt[j-1]).Round(time.Second))
	}
	if !slices.Equal(gaps, want) || slices.Contains(h.f.wants[i+1:], false) {
		t.Fatalf("%s：补扫之后应按 %v 收候选，got 间隔 %v 收候选 %v", what, want, gaps, h.f.wants[i+1:])
	}
}

// 存储的出现与消失相隔不短于 storeFlapQuiet 时，处理与没有抖动保护时相同：头一回认出后立即补一次收候选的
// 全扫、之后按 twinScanSteps；消失后回到 idleScanSteps 的第一档；隔够了再回来，照样立即补扫。相隔多久按
// 认出这两次翻转的全扫算，不按存储实际消失了多久：消失 7s 回来的，认出它的那次全扫距认出消失的那次正好
// 10s，也在安静期之外。
//
// 变异自证（隔离副本，实测）：把 applySnapshot 里的 flapping 改成 `flapping := flip`（凡翻转都当抖动），
// 头一回认出存储后不补扫而红；把 storeFlapQuiet 改成 20s，消失 12s 后回来不补扫而红。
func TestDiscoveryStoreFlipOutsideQuietPeriod(t *testing.T) {
	for _, away := range []time.Duration{12 * time.Second, 7 * time.Second} {
		h := newHarness(t)
		h.setTwin(0, 0, false)
		h.run(30 * time.Second)
		first := len(h.f.wants)
		h.f.songs = np("点了没放")
		h.run(40 * time.Second)
		checkSupplement(t, h, first, "头一回认出存储")

		gone := len(h.f.wants)
		h.f.songs = nil
		h.run(away)
		back := len(h.f.wants)
		if got := h.f.wants[gone:]; len(got) < 3 || !got[0] || slices.Contains(got[1:], true) || h.scanAt[gone+1].Sub(h.scanAt[gone]) > idleScanSteps[0]+2*step {
			t.Fatalf("消失 %v：盯梢发现后的那次全扫照收候选，之后回到 idleScanSteps 第一档、不收候选，got %v，头两次相隔 %v",
				away, got, h.scanAt[gone+1].Sub(h.scanAt[gone]))
		}
		h.f.songs = np("点了没放")
		h.run(40 * time.Second)
		if d := h.scanAt[back].Sub(h.scanAt[gone]); d < 10*time.Second {
			t.Fatalf("前置：消失 %v 后回来，认出回来与认出消失的两次全扫应相隔不短于 10s，got %v", away, d)
		}
		checkSupplement(t, h, back, "消失 "+away.String()+" 后回来")
	}
}

// 存储消失后不足 storeFlapQuiet 就回来（按认出这两次翻转的全扫算）：不立即补扫，下一次全扫在原先排定的
// 时刻发生并收候选；此后开播照样认出。离线回放（测试日志）：消失 1s 后回来，认出它的那次全扫之后 3s 才再
// 全扫，回来的同时开播 5.6s 认出；消失 4s 后回来，5s 后才再全扫，回来的同时开播 7.7s 认出；回来后 0~20s
// 内开播（每 0.5s 一档）最坏都是 16.6s，落在 twinScanSteps 的 15s 一档上，与存储一直在、时长迟迟不写入时
// 相同。没有抖动保护时两例都是 300ms 后补扫、3.9s 认出，最坏 16.8s——多出的几秒是抖动保护的代价。
//
// 变异自证（隔离副本，实测）：把 applySnapshot 里的 flapping 改成恒假，两例都在 300ms 后补扫而红；把抖动
// 分支改成 `s.restartTwinScan(now, false)`（回到第一档），两例都在 2s 后再扫而红。
func TestDiscoveryStoreBackWithinQuietPeriod(t *testing.T) {
	for _, tc := range []struct {
		away, next time.Duration // 消失多久；认出回来的那次全扫之后，原先排定的下一次在多久之后
	}{
		{time.Second, idleScanSteps[1]},
		{4 * time.Second, idleScanSteps[2]},
	} {
		var worst, atOnce, gap time.Duration
		for playAfter := time.Duration(0); playAfter <= 20*time.Second; playAfter += 500 * time.Millisecond {
			h := newHarness(t)
			h.setTwin(0, 0, false)
			h.f.songs = np("点了没放")
			h.run(40 * time.Second)
			h.f.songs = nil
			h.run(tc.away)
			back := len(h.f.wants)
			h.f.songs = np("点了没放")
			h.run(playAfter)
			h.setTwin(210, 0, true)
			d := waitAnnounce(h, 60*time.Second)
			if d < 0 {
				t.Fatalf("消失 %v 后回来、再过 %v 开播，60s 内没认出", tc.away, playAfter)
			}
			if got := h.f.wants[back:]; len(got) < 2 || got[0] || !got[1] {
				t.Fatalf("消失 %v 后回来：认出它的那次全扫不收候选，下一次应收，got %v", tc.away, got)
			}
			gap = h.scanAt[back+1].Sub(h.scanAt[back]) // 各档 playAfter 下相同：开播不影响这两次全扫
			if worst = max(worst, d); playAfter == 0 {
				atOnce = d
			}
		}
		t.Logf("消失 %v 后回来：认出后 %v 才再全扫；回来的同时开播 %v 认出，回来后 0~20s 开播最坏 %v", tc.away, gap, atOnce, worst)
		if gap < tc.next || gap > tc.next+2*step {
			t.Errorf("消失 %v 后回来：不应立即补扫，下一次全扫应在原先排定的 %v 之后，got %v", tc.away, tc.next, gap)
		}
		if atOnce > 8*time.Second || worst > 17*time.Second {
			t.Errorf("消失 %v 后回来：回来的同时开播 %v 认出（应 ≤ 8s），回来后 0~20s 开播最坏 %v（应 ≤ 17s）", tc.away, atOnce, worst)
		}
	}
}

// 存储还在、只是换了歌名（用户点了另一首）不是「出现 / 消失」：哪怕距存储头一回出现不足 storeFlapQuiet，
// 也照旧把候选扫描重排到开头几档（另有 tagScanAfter 后补记歌词归属的一次）。
//
// 变异自证（隔离副本，实测）：把 applySnapshot 里的 flip 改成 `changed || gone`（换歌名也算翻转），换歌名
// 后不重排、沿原先的档位走（其后 0.7、4.3、9.3、24.3s）而红。
func TestDiscoveryTitleChangeIsNotStoreFlap(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(0, 0, false)
	h.run(5500 * time.Millisecond) // 全扫在 0、2、5s；存储头一回出现在 0s
	changed := len(h.f.wants)
	h.f.songs = np("B")
	h.run(40 * time.Second)
	var at []time.Duration
	for i := changed + 1; i < len(h.scanAt); i++ {
		at = append(at, h.scanAt[i].Sub(h.scanAt[changed]))
	}
	want := []time.Duration{tagScanAfter, 2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second}
	if !slices.Equal(at, want) {
		t.Fatalf("换歌名后的全扫应在其后 %v（补记歌词归属的一次，然后 twinScanSteps 从第一档起），got %v", want, at)
	}
}

// flapPlayWorst 存储按 hideCollect 抖动一分多钟，随后（calm > 0 时先彻底消失 calm 之久）稳定下来、lead 之后
// 时长写入（开播）。把「抖动停在周期全扫的哪个相位」扫一遍（每 100ms 一档，覆盖 30s），返回从开播到宣告的
// 最坏值。
func flapPlayWorst(t *testing.T, calm, lead time.Duration) time.Duration {
	t.Helper()
	var worst time.Duration
	for phase := time.Duration(0); phase < 30*time.Second; phase += step {
		h, m := newFlapHarness(t, hideCollect)
		h.f.twins[0x2000] = mem.Twin{Dur: 180, Pos: 0} // 形态相同的静止结构
		h.setTwin(0, 0, false)
		h.run(60*time.Second + phase)
		if calm > 0 {
			h.f.songs = nil
			h.run(calm)
			h.f.songs = np("第一首")
		}
		m.hide = nil
		h.run(lead)
		h.setTwin(210, 0, true)
		d := waitAnnounce(h, 70*time.Second)
		if d < 0 {
			t.Fatalf("抖动 %v、消失 %v 后存储稳定下来，%v 后开播，70s 内没认出", 60*time.Second+phase, calm, lead)
		}
		worst = max(worst, d)
	}
	return worst
}

// 存储抖动过后歌真的开播，照样认出（离线回放，测试日志）：
//
//   - 抖动停止、存储消失 20s（超过 storeFlapQuiet）后放第一首：与空闲附加后放第一首相同
//     （TestDiscoveryFirstSongAfterIdleAttach），最坏 7.0s（存储与时长同时出现）、6.0s（存储早 1s 出现）。
//   - 抖动中存储稳定下来并开播：认出存储的那次全扫不补扫，等下一档收候选的，最坏 11.8s（同时开播）；稳定
//     1s 后才写入时长的，可能刚错过一次收候选的全扫、落到 twinScanSteps 的 15s 一档上，最坏 16.8s，与存储
//     一直在、时长迟迟不写入时同一量级（TestDiscoveryStoreSeenLongBeforePlay，≤19s）。没有抖动保护时这两例
//     是 4.0s 与 3.8s，靠的正是要去掉的那种约每 1.15s 一次的连扫。
//
// 变异自证（隔离副本，实测）：把 applySnapshot 里的 flapping 改成 `flapping := flip`（凡翻转都当抖动），
// 前两例 11.8s、10.8s 而红；去掉 retableTwinScan 里「不晚于新表封顶」的那一段，第三例 16.9s 而红；给
// twinScanSteps 末尾加一档 60s，第四例 61.8s 而红。
func TestDiscoverySongAfterStoreFlapping(t *testing.T) {
	for _, tc := range []struct {
		name              string
		calm, lead, limit time.Duration
	}{
		{"抖动停止、存储消失 20s 后放第一首，存储与时长同时出现", 20 * time.Second, 0, 7 * time.Second},
		{"抖动停止、存储消失 20s 后放第一首，存储早 1s 出现", 20 * time.Second, time.Second, 6 * time.Second},
		{"抖动中存储稳定下来，同时开播", 0, 0, 12 * time.Second},
		{"抖动中存储稳定下来，1s 后开播", 0, time.Second, 17 * time.Second},
	} {
		worst := flapPlayWorst(t, tc.calm, tc.lead)
		t.Logf("%s：从开播到宣告最坏 %v", tc.name, worst)
		if worst > tc.limit {
			t.Errorf("%s：从开播到宣告最坏 %v，应 ≤ %v", tc.name, worst, tc.limit)
		}
	}
}

// 不收候选的全扫不得动候选观察表：表里已在观察的候选（真机四五百个）都留着，存储出现后还靠它们认出
// 位置结构。
//
// 变异自证：把 maybeScan 里的 `if collect { s.setCandidates(snap, end) }` 改成无条件调用，不收候选的
// 全扫拿空的候选列表去并，观察表被清空而红。
func TestDiscoveryIdleScanKeepsCandidates(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 100; i++ { // 一百个形态相同的静止结构
		h.f.twins[uintptr(0x10000+i*0x20)] = mem.Twin{Dur: float64(100 + i), Pos: 0}
	}
	h.setTwin(180, 5, true) // 真结构在走，但没有存储：不认
	h.run(time.Second)
	watched := len(h.s.cands)
	if watched != 101 {
		t.Fatalf("前置：第一次全扫应把 101 个候选都收进观察表，got %d", watched)
	}
	h.run(20 * time.Second)
	if len(h.f.wants) < 4 || slices.Contains(h.f.wants[1:], true) {
		t.Fatalf("前置：其后应有几次不收候选的全扫，got %v", h.f.wants)
	}
	if len(h.s.cands) != watched || h.s.twin != 0 {
		t.Fatalf("不收候选的全扫之后观察表应仍是 %d 个、位置结构仍未确认，got %d 个 twin=%#x", watched, len(h.s.cands), h.s.twin)
	}
	h.f.songs = np("歌名来了")
	d := waitAnnounce(h, 20*time.Second)
	t.Logf("存储出现后 %v 宣告", d)
	if d < 0 || d > 6*time.Second {
		t.Fatalf("存储出现后 %v 才宣告（-1 = 20s 内没有），应 ≤ 6s", d)
	}
}

// 暂停中启动（不宣告），随后没按播放、直接点了另一首：真结构就在候选表里，时长先归零再写新值，
// 不必等下一次候选扫描，走满两格即认出。
//
// 认得快靠两条路，各自都够：候选留在表里跟着它的时长变（cand.observe），以及存储换了就把候选扫描
// 重排到开头几档（applySnapshot）。
//
// 变异自证：两条一起断——watchCandidates 里 `t.Dur != c.dur` 时把候选删掉，并删掉 applySnapshot 里
// 存储换了时的 `s.restartTwinScan(now, !collected)`——本例要等封顶档的候选扫描而红。只断其中
// 一条本例仍绿（实测都是 3s 认出）。
func TestDiscoveryPausedStartThenDifferentSong(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(240, 37, false)
	h.run(10 * time.Second)

	h.setTwin(0, 0, false)
	h.f.songs = np("B")
	h.run(1300 * time.Millisecond)
	h.setTwin(260, 0, true)
	if sec := waitSongInfo(h, 30); sec < 0 || sec > 4 {
		t.Fatalf("暂停中启动后点另一首，%ds 才认出（-1 = 30s 内没认出），应 ≤ 4s", sec)
	}
	for _, si := range songInfos(h.evs) {
		if si.Name != "B" {
			t.Fatalf("不应宣告 B 以外的歌，got %q", si.Name)
		}
	}
}

// 加载中（时长为 0）恰逢一次候选扫描：已在观察的真结构此刻不在这次扫到的候选里，不能因此丢掉。
//
// 变异自证：把 setCandidates 改回只保留这次扫到的地址（不把已在观察的并进来），并删掉 applySnapshot 里
// 存储换了时的 `s.restartTwinScan(now, !collected)`，真结构在加载中被丢、本例要等封顶档的候选
// 扫描而红。只做前一样本例仍绿（实测 3s 认出）：存储换了会把候选扫描重排到开头几档，补得上。
func TestDiscoveryCandidateKeptWhileLoading(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(240, 37, false)
	h.run(4500 * time.Millisecond) // 候选扫描在 0、2、5s……停在 5s 那次之前

	h.setTwin(0, 0, false) // 切歌，加载中；5s 那次候选扫描落在这段里
	h.f.songs = np("B")
	h.run(2 * time.Second)
	h.setTwin(260, 0, true)
	if sec := waitSongInfo(h, 30); sec < 0 || sec > 4 {
		t.Fatalf("加载中赶上候选扫描后开播，%ds 才认出（-1 = 30s 内没认出），应 ≤ 4s", sec)
	}
}

// Apple Music 刚启动的头一两秒：一个 [100][进度][进度] 的进度条在走（2026-10-08 真机，旧实现把它认成
// 「时长 100s」的位置结构，之后真开始放歌要等它停住 20 秒才换过去）。它走得比每秒一格快，不认；
// 随后真的放歌，照常认出真结构。真机没量过它到底多快，三种走法都摆上。
//
// 变异自证：把 cand.observe 里 `gap >= liveMinGap` 的条件去掉（挨得再近的前进也累计），「每 300ms 一格」
// 一例被认成位置结构、宣告出时长 100 而红；把「一下跳了好几格就清零」那个 case 删掉，「每秒跳三格」
// 一例被认而红。「每 100ms 一格」两条都拦得住，单删一条仍绿。
func TestDiscoveryStartupProgressBarNotConfirmed(t *testing.T) {
	const bar uintptr = 0x3000
	var h *harness
	for _, c := range []struct {
		name string
		pos  func(i int) float64 // 第 i 个 100ms 时的读数
	}{
		{"每 100ms 一格", func(i int) float64 { return float64(i) }},
		{"每 300ms 一格", func(i int) float64 { return float64(i / 3) }},
		{"每秒跳三格", func(i int) float64 { return float64(3 * (i / 10)) }},
	} {
		h = newHarness(t)
		h.f.songs = np("启动前放的那首") // 即便内存里有歌名，也不能认它
		h.f.twins[bar] = mem.Twin{Dur: 100, Pos: 0}
		for i := 1; i <= 40; i++ {
			h.f.twins[bar] = mem.Twin{Dur: 100, Pos: c.pos(i)}
			h.run(step)
		}
		h.run(5 * time.Second)
		if h.s.twin != 0 || count(h.evs, player.EventSongInfoUpdate) != 0 {
			t.Fatalf("%s：进度条不应被认成位置结构（twin=%#x，song_info=%d）", c.name, h.s.twin, count(h.evs, player.EventSongInfoUpdate))
		}
	}

	h.f.songs = np("真的开始放了")
	h.setTwin(269, 0, true)
	if sec := waitSongInfo(h, 30); sec < 0 || sec > 8 {
		t.Fatalf("随后真的放歌，%ds 才认出（-1 = 30s 内没认出），应 ≤ 8s", sec)
	}
	if al := allLyrics(h.evs); len(al) == 0 || al[0].Duration != 269 {
		t.Fatalf("应按真结构的时长 269 宣告，got %+v", al)
	}
}

// 有东西像歌曲那样每秒走一格，但内存里没有任何「正在播放」存储：没有歌名可宣告，不认它。
// 存储出现后才认。
//
// 变异自证：删掉 watchCandidates 里 `s.store.Sig == nil` 时不认的那一段，本例在没有存储时就确认了
// 位置结构而红。
func TestDiscoveryNoStoreNoConfirm(t *testing.T) {
	h := newHarness(t)
	h.setTwin(180, 5, true)
	h.run(8 * time.Second)
	if h.s.twin != 0 {
		t.Fatalf("没有「正在播放」存储时不应确认位置结构（twin=%#x）", h.s.twin)
	}
	h.f.songs = np("歌名来了")
	if sec := waitSongInfo(h, 10); sec < 0 || sec > 3 {
		t.Fatalf("存储出现后 %ds 才宣告（-1 = 10s 内没有），应 ≤ 3s", sec)
	}
}

// 已确认的位置结构停住（例如锁在了一份不再更新的副本上），而另一处同时长的结构在走：停住 staleAfter
// 之后应找出在走的那个、改用它，并按恢复处理（审查 P1-8：旧实现整首判暂停且永不自愈）。
//
// 变异自证：删掉 tick 里对 checkStale 的调用，本例一直停在暂停而红。
func TestDiscoveryStaleTwinReplaced(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("锁死")
	h.setTwin(200, 30, true)
	h.run(5 * time.Second)

	// realTwin 停住；另一处同时长的结构从同一位置接着走。
	pos := h.f.twins[realTwin].Pos
	h.setTwin(200, pos, false)
	const other uintptr = 0x5000
	from := h.mark()
	for i := 1; i <= 60; i++ {
		h.f.twins[other] = mem.Twin{Dur: 200, Pos: pos + float64(i)}
		h.run(time.Second)
		if h.s.twin == other {
			break
		}
	}
	if h.s.twin != other {
		t.Fatalf("60s 内没有改用在走的结构（仍是 %#x）", h.s.twin)
	}
	evs := h.since(from)
	if st := statuses(evs); len(st) == 0 || st[len(st)-1] != "playing" {
		t.Fatalf("改用后应恢复 playing，got %v", st)
	}
	if count(evs, player.EventSongInfoUpdate) != 0 {
		t.Fatal("同一首歌换了位置结构，不应重新宣告")
	}
}

// linesAfterResume 返回 evs 里第一条 playback_resume 之前、之后各推了哪些歌词行。
func linesAfterResume(evs []player.Event) (before, after []int) {
	for i, e := range evs {
		if e.Type == player.EventPlaybackResume {
			return lineIdxs(evs[:i]), lineIdxs(evs[i:])
		}
	}
	return lineIdxs(evs), nil
}

// 同一首歌找回位置结构（读不出后重新发现、或停住后换成另一处在走的）时，位置可能在已推的行之前：
// 行号要复位，否则 nextLine 一直拒发靠前的行，歌词停住直到播放位置越过原先那一行。
//
// 变异自证：删掉 watchCandidates 同曲目分支里的 `s.lastLineIdx = -1`，前两例找回后一行都不推而红；
// 把这句挪到 watchCandidates 开头（每次复读都复位），三例都把同一行重推了多次而红（第三例只有这一种
// 变异拦得住：它守的是「不多推」）。
func TestDiscoveryRefoundTwinResetsLyricLine(t *testing.T) {
	// start 放到 62s 附近（已推到 60s 起的那一行），返回会话与此刻的读数。
	start := func(t *testing.T, starts ...float64) (*harness, float64) {
		h := newHarness(t)
		h.f.songs = np("找回")
		h.f.docs = []mem.TTMLDoc{{Text: ttmlAt(200.4, starts...), Count: 1}}
		h.setTwin(200, 50, true)
		h.run(14 * time.Second)
		if got := lineIdxs(h.evs); len(got) == 0 || got[len(got)-1] != 2 {
			t.Fatalf("前置：应已推到第 2 行，got %v", got)
		}
		return h, h.f.twins[realTwin].Pos
	}
	lose := func(h *harness) { // 位置结构读不出，超过 twinFailFor 后作废
		delete(h.f.twins, realTwin)
		h.play = false
		h.run(1500 * time.Millisecond)
		if h.s.twin != 0 {
			t.Fatal("前置：位置结构应已作废")
		}
	}

	t.Run("读不出后重新发现，位置更靠前", func(t *testing.T) {
		h, _ := start(t, 10, 30, 60)
		lose(h)
		from := h.mark()
		h.setTwin(200, 22, true)
		h.run(50 * time.Second)
		before, after := linesAfterResume(h.since(from))
		if len(before) != 0 || !slices.Equal(after, []int{0, 1, 2}) {
			t.Fatalf("找回后应在 playback_resume 之后依次推第 0、1、2 行，got 之前 %v 之后 %v", before, after)
		}
	})
	t.Run("停住后换成另一处在走的，位置更靠前", func(t *testing.T) {
		h, pos := start(t, 10, 50, 60)
		h.setTwin(200, pos, false) // realTwin 停住
		const other uintptr = 0x5000
		from := h.mark()
		for i := 0; i < 45; i++ { // 另一处同时长的结构从 22s 起走；停满 staleAfter 后才会去找它
			h.f.twins[other] = mem.Twin{Dur: 200, Pos: 22 + float64(i)}
			h.run(time.Second)
		}
		if h.s.twin != other {
			t.Fatalf("前置：45s 内应改用在走的结构（仍是 %#x）", h.s.twin)
		}
		before, after := linesAfterResume(h.since(from))
		if len(before) != 0 || !slices.Equal(after, []int{0, 1, 2}) {
			t.Fatalf("改用后应在 playback_resume 之后依次推第 0、1、2 行，got 之前 %v 之后 %v", before, after)
		}
	})
	t.Run("找回时位置没变，当前行至多重推一次", func(t *testing.T) {
		h, pos := start(t, 10, 30, 60, 75)
		lose(h)
		from := h.mark()
		h.setTwin(200, pos+2, true)
		h.run(20 * time.Second)
		before, after := linesAfterResume(h.since(from))
		if len(before) != 0 || !(slices.Equal(after, []int{2, 3}) || slices.Equal(after, []int{3})) {
			t.Fatalf("找回后当前行（2）至多重推一次，随后推第 3 行，got 之前 %v 之后 %v", before, after)
		}
	})
}

// 已确认的位置结构读不出之后：存储仍在就继续收候选；存储也没了（回到没有歌的状态）则只找存储、不收
// 候选，间隔立刻回到 idleScanSteps 的封顶之内；存储回来后立即补一次收候选的，位置结构在走就按恢复接上。
//
// 变异自证：把 applySnapshot 里存储消失时的 `s.restartTwinScan(now, false)` 删掉，存储没了之后仍等着
// 收候选时排下的那一档（15s），40s 里只扫了 3 次而红。
func TestDiscoveryTwinAndStoreLost(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("丢失")
	h.setTwin(200, 30, true)
	h.run(40 * time.Second)

	saved := h.f.twins[realTwin]
	delete(h.f.twins, realTwin)
	h.play = false
	dropped := len(h.f.wants)
	h.run(20 * time.Second)
	if got := h.f.wants[dropped:]; h.s.twin != 0 || len(got) < 4 || slices.Contains(got, false) {
		t.Fatalf("位置结构作废而存储仍在时，每次全扫都应收候选，got twin=%#x %v", h.s.twin, got)
	}

	h.f.songs = nil // 存储也没了
	h.run(2 * time.Second)
	if h.s.store.Sig != nil {
		t.Fatal("前置：盯梢应已发现存储消失")
	}
	lost := len(h.f.wants)
	h.run(40 * time.Second)
	if got := h.f.wants[lost:]; len(got) < 6 || slices.Contains(got, true) {
		t.Fatalf("存储也没了之后的全扫应只找存储、不收候选，got %v", got)
	}
	for i := lost; i < len(h.scanAt); i++ {
		if gap := h.scanAt[i].Sub(h.scanAt[i-1]); gap > 5500*time.Millisecond {
			t.Fatalf("没有存储时相邻两次全扫相隔 %v，应不超过 idleScanSteps 的封顶 5s", gap)
		}
	}

	back, from := len(h.f.wants), h.mark()
	h.f.songs = np("丢失")
	h.setTwin(200, saved.Pos+40, true)
	h.run(12 * time.Second)
	got := h.f.wants[back:]
	if len(got) < 2 || got[0] || !got[1] || h.scanAt[back+1].Sub(h.scanAt[back]) > minScanGap+2*step {
		t.Fatalf("存储回来后应立即补一次收候选的全扫，got %v", got)
	}
	evs := h.since(from)
	if st := statuses(evs); h.s.twin != realTwin || len(st) == 0 || st[len(st)-1] != "playing" || count(evs, player.EventSongInfoUpdate) != 0 {
		t.Fatalf("12s 内应重新发现位置结构并按恢复报 playing、不重新宣告，got twin=%#x status=%v song_info=%d",
			h.s.twin, st, count(evs, player.EventSongInfoUpdate))
	}
}

// 已确认的位置结构连续读不出（被释放、地址失效）：twinFailFor 后作废，按暂停报（没有可靠位置源
// 不报 playing），重新发现后接上、按恢复报 playing。
//
// 变异自证：把 twinFailFor 改成极大值（永不作废），读不出期间不报暂停而红。
func TestDiscoveryTwinLostThenRediscovered(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("丢失")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(200.4, "第一行"), Count: 1}}
	h.setTwin(200, 30, true)
	h.run(5 * time.Second)

	from := h.mark()
	saved := h.f.twins[realTwin]
	delete(h.f.twins, realTwin)
	h.play = false
	h.run(1500 * time.Millisecond)
	evs := h.since(from)
	if count(evs, player.EventPlaybackPause) != 1 || !slices.Contains(statuses(evs), "paused") {
		t.Fatalf("位置结构读不出 %v 后应作废并报暂停，got pause=%d status=%v", twinFailFor, count(evs, player.EventPlaybackPause), statuses(evs))
	}
	if h.s.twin != 0 {
		t.Fatal("位置结构应已作废")
	}

	from = h.mark()
	h.setTwin(200, saved.Pos+2, true)
	h.run(15 * time.Second)
	evs = h.since(from)
	if h.s.twin != realTwin {
		t.Fatal("15s 内应重新发现位置结构")
	}
	if st := statuses(evs); len(st) == 0 || st[len(st)-1] != "playing" || count(evs, player.EventSongInfoUpdate) != 0 {
		t.Fatalf("重新发现后应按恢复报 playing、不重新宣告，got status=%v song_info=%d", st, count(evs, player.EventSongInfoUpdate))
	}
}
