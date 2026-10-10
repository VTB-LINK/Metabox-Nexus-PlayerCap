package applemusic

// 本文件只测全扫的节流（审查 P1-1）：旧实现在没有歌、没有歌词或定位不到位置时约每 1.2s 全扫一次，
// 审查方真机单核 CPU 约 100%。现在每种需求都有节流，稳态播放零全扫。

import (
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// scanGaps 返回相邻两次全扫的间隔。
func scanGaps(h *harness) []time.Duration {
	var gaps []time.Duration
	for i := 1; i < len(h.scanAt); i++ {
		gaps = append(gaps, h.scanAt[i].Sub(h.scanAt[i-1]))
	}
	return gaps
}

// Apple Music 开着但没放过歌（没有存储，只有静止的同形态结构）：周期全扫只找存储，间隔 2/3/5s、封顶 5s
// （idleScanSteps），除本会话第一次外都不收候选，不出现连扫。10 分钟 121 次（测试日志），每次比收候选的
// 全扫快（耗时见 idleScanSteps）；旧实现按审查的估算约每 1.2s 一次、每次都收候选，约 500 次。
//
// 变异自证：把 backoff.advance 改成固定 1s 间隔，次数远超 125 而红；把 maybeScan 里未确认位置结构时的
// `collect = s.store.Sig != nil || s.lastScanEnd.IsZero()` 改回 `collect = true`，每次都收候选而红。
func TestScanBudgetNoSong(t *testing.T) {
	h := newHarness(t)
	h.f.twins[0x2000] = mem.Twin{Dur: 180, Pos: 0}
	h.f.twins[0x3000] = mem.Twin{Dur: 95, Pos: 0}
	h.run(10 * time.Minute)
	t.Logf("10 分钟全扫 %d 次", h.f.scans)
	if h.f.scans > 125 {
		t.Fatalf("10 分钟没有歌时全扫 %d 次，应 ≤ 125", h.f.scans)
	}
	if !h.f.wants[0] || slices.Contains(h.f.wants[1:], true) {
		t.Fatalf("没见到存储时只有第一次全扫收候选，got %v", h.f.wants)
	}
	for i, gap := range scanGaps(h) {
		want := 5 * time.Second
		if i < 2 {
			want = idleScanSteps[i]
		}
		if gap < want {
			t.Fatalf("第 %d、%d 次全扫相隔 %v，应不小于 %v", i+1, i+2, gap, want)
		}
	}
}

// 暂停中附加（有存储、同形态的结构都不走）：见到存储后每次全扫都收候选，间隔 2/3/5/5s、封顶 15s
// （twinScanSteps，取舍见其注释）。10 分钟 43 次（测试日志）。
//
// 变异自证：把 twinScanTable 改成一律返回 idleScanSteps（见到存储也封顶 5s），10 分钟全扫一百多次而红。
func TestScanBudgetPausedAttach(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("暂停中附加")
	h.f.twins[0x2000] = mem.Twin{Dur: 180, Pos: 0}
	h.setTwin(240, 37, false)
	h.run(10 * time.Minute)
	t.Logf("10 分钟全扫 %d 次", h.f.scans)
	if h.f.scans > 45 {
		t.Fatalf("暂停中附加 10 分钟全扫 %d 次，应 ≤ 45", h.f.scans)
	}
	if slices.Contains(h.f.wants, false) {
		t.Fatalf("见到存储、尚未确认位置结构时每次全扫都应收候选，got %v", h.f.wants)
	}
	for i, gap := range scanGaps(h) {
		want := 15 * time.Second
		if i < len(twinScanSteps) {
			want = twinScanSteps[i]
		}
		if gap < want || gap > want+2*step {
			t.Fatalf("第 %d、%d 次全扫相隔 %v，应为 %v", i+1, i+2, gap, want)
		}
	}
}

// 存储在相邻两次全扫之间反复出现与消失、位置结构始终未确认（PR #60 修复轮复审的回放）：周期全扫照样走到
// 封顶，不比没有歌时（TestScanBudgetNoSong，121 次）更密。10 分钟里（测试日志）：
//
//   - 不收候选的全扫认出、收候选的认不出：123 次，只有头一回认出存储后补的那一次与前一次相隔不足 2s。
//     没有抖动保护时 521 次：认出 → 立即补扫 → 没认出 → 2s 后再扫，约每 1.15s 一次。
//   - 逢偶数次认不出（第一次由收候选的全扫认出）：121 次，没有连扫。没有抖动保护时同样 521 次。
//   - 收候选的认出、不收候选的认不出：这种交替到不了——见到存储后每次全扫都收候选，存储不再「消失」，按
//     twinScanSteps 走，43 次。
//
// 每一轮还核对间隔表与状态相符：没见到存储用 idleScanSteps，见到了用 twinScanSteps。
//
// 变异自证（隔离副本，实测）：把 applySnapshot 里的 flapping 改成恒假（去掉抖动保护），前两例 521 次而红；
// 改成 `flapping := flip`（凡翻转都当抖动），第一例开头不补扫而红；去掉 retableTwinScan 里「不晚于新表
// 封顶」的那一段，前两例仍是 121、120 次，但约每 30s 又清零补扫一轮（不足 2s 的连扫 20、19 次）而红；去掉
// retableTwinScan 里换表的那一句，前两例间隔表与状态不符而红；把抖动分支改成
// `s.restartTwinScan(now, false)`（不补扫但回到第一档），前两例 301、300 次而红。第三例在以上变异下都
// 不红；把 maybeScan 里未确认位置结构时的 collect 改成只在第一次全扫收（去掉 `s.store.Sig != nil ||`），它
// 121 次而红。
func TestScanBudgetStoreFlapping(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hide  func(wantTwins bool, nth int) bool
		quick int // 相隔不足 2s 的连扫次数：只有头一回认出存储后补的那一次
		max   int // 全扫次数上限
	}{
		{"不收候选的认出、收候选的认不出", hideCollect, 1, 125},
		{"逢偶数次认不出", func(_ bool, nth int) bool { return nth%2 == 0 }, 0, 125},
		{"收候选的认出、不收候选的认不出", func(wantTwins bool, _ int) bool { return !wantTwins }, 0, 45},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newFlapHarness(t, tc.hide)
			h.f.twins[0x2000] = mem.Twin{Dur: 180, Pos: 0}
			h.f.twins[0x3000] = mem.Twin{Dur: 95, Pos: 0}
			for end := h.now.Add(10 * time.Minute); h.now.Before(end); {
				h.run(step)
				if idle := h.s.store.Sig == nil; idle != (len(h.s.twinScan.steps) == len(idleScanSteps)) {
					t.Fatalf("第 %d 次全扫后间隔表与状态不符：见到存储=%v，表=%v", h.f.scans, !idle, h.s.twinScan.steps)
				}
			}
			quick, last := 0, 0
			for i, gap := range scanGaps(h) {
				if gap < 2*time.Second {
					quick, last = quick+1, i+2
				}
			}
			t.Logf("10 分钟全扫 %d 次，其中与前一次相隔不足 2s 的 %d 次", h.f.scans, quick)
			if h.f.scans > tc.max {
				t.Fatalf("存储时有时无的 10 分钟里全扫 %d 次，应 ≤ %d", h.f.scans, tc.max)
			}
			if quick != tc.quick || last > 3 {
				t.Fatalf("与前一次相隔不足 2s 的全扫 %d 次、最后一次是第 %d 次，应为 %d 次且只在开头（认出存储后补的那一次）", quick, last, tc.quick)
			}
			if h.s.twin != 0 || len(songInfos(h.evs)) != 0 || slices.Contains(statuses(h.evs), "playing") {
				t.Fatalf("没有在走的候选，不应确认位置结构、宣告歌曲或报 playing，got twin=%#x status=%v", h.s.twin, statuses(h.evs))
			}
		})
	}
}

// 有歌、有歌词、有封面、位置已确认：开头几秒取齐之后不再全扫。
//
// 变异自证：把 setCover 里的 `s.needCover = false` 去掉（封面到了也照旧按 coverTries 扫），本例 15s
// 之后还有一次（20s 那档）而红。
func TestScanBudgetSteadyPlayback(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("稳态")
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(400.4, "第一行"), Count: 1}}
	h.setTwin(400, 10, true)
	h.run(15 * time.Second)
	before := h.f.scans
	h.run(3 * time.Minute)
	if n := h.f.scans - before; n != 0 {
		t.Fatalf("稳态播放 3 分钟内全扫 %d 次，应为 0", n)
	}
}

// 有歌没歌词：宽限期（前 15s）内约每 1.2s 找一次，之后退避，最后一档 60s 一次。
//
// 变异自证：把 scheduleSongScan 里 `since < lyricGrace` 的条件去掉（一直按 lyricGapFast 找），后 5 分钟
// 全扫两百多次而红。
func TestScanBudgetNoLyrics(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("无词")
	h.setTwin(400, 10, true)
	h.run(30 * time.Second)
	early := h.f.scans
	h.run(5 * time.Minute)
	t.Logf("前 30s 全扫 %d 次，之后 5 分钟 %d 次", early, h.f.scans-early)
	if early > 25 {
		t.Fatalf("前 30s 全扫 %d 次，应 ≤ 25", early)
	}
	if n := h.f.scans - early; n < 4 || n > 7 {
		t.Fatalf("没歌词时后 5 分钟内全扫 %d 次，应为每分钟约一次（4~7）", n)
	}
}

// 存储一直在变（抖动）：因它触发的全扫间隔翻倍、封顶 5s，不会跟着连扫。
//
// 变异自证：把 maybeScan 里 dirtyGap 翻倍的那一段删掉（恒为 minScanGap），一分钟里全扫一百多次而红。
func TestScanBudgetFlappingStore(t *testing.T) {
	h := newHarness(t)
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(400.4, "第一行"), Count: 1}}
	h.f.songs = np("抖动")
	h.setTwin(400, 10, true)
	h.run(10 * time.Second)
	before := h.f.scans
	for i := 0; i < 600; i++ { // 一分钟里每 100ms 换一次封面：每次都是一份新存储
		h.f.songs = []mem.NowPlaying{store("抖动", string(rune('a'+i%26)))}
		h.run(step)
	}
	if n := h.f.scans - before; n > 25 {
		t.Fatalf("存储抖动的一分钟里全扫 %d 次，应 ≤ 25", n)
	}
}
