package spotify

// 本文件测「页面读数 → 真实播放位置」这一步（truePositionSec 与 posConverter）。
//
// 校正量的实测依据写在 spotify.go 的 progressLeadSec 注释里；这里的用例钉住：
//  1. truePositionSec：播放中减掉超前量、暂停时原样、减成负数取 0。
//     变异自证：把 playing 判据删掉（两种状态都减）→ 暂停那条红；把减改成加 → 两条红；去掉下限 → 开播首帧那条红。
//  2. posConverter 与 convertTick：双向沿用读数写出时的播放态口径（暂停无超前延续到恢复后首次刷出新读数；
//     图标可信的播放超前延续到暂停后首次回落或拖动）、!(hasLed && hasUnled) 互斥不变量、iconKnown 门控、
//     换歌与跨歌残留夹零复位、非法读数（NaN / 负值 / 超长）与短曲目曲末 / 迟到 duration 下保持口径。
//  3. 录音回归（#241 / #254）：串联 posConverter → clockRaw → clock.observe → clockEmits → liveAt + LyricMatchTime。
//
// 变异自证（在隔离副本里逐条注入，均实测变红）：
//   - M1（c.hasUnled && rawSec == c.unledRaw 命中一次即清 hasUnled）→
//     TestPosConverter/C3_multi_tick_stale_after_resume、C7_invalid_tick_preserves_unled、C10_late_duration_keeps_unled
//     与 TestResumePositionRecording241/late_duration_no_false_seek 红；
//   - M2a（去掉 convertTick 中换歌前 if isNewSong { c.reset() }）→
//     TestPosConverter/C6_song_change_resets_unled（0.8→0.8 与 2.5→2.5）红；
//   - M2b（去掉 convertTick 中夹零分支的 c.reset()）→
//     TestPosConverter/C6_stale_clamped_resets 红；
//   - M3a（在 convert 的 !(rawSec >= 0)、!playing 超 1.05*durationSec 或 playing 超 1.05*durationSec 分支分别改写/清 hasUnled）→
//     TestPosConverter/C7_invalid_tick_preserves_unled 红；
//   - M3b（在 convert 中把超限检查提前到 c.hasUnled && rawSec == c.unledRaw 之前并清 hasUnled）→
//     TestPosConverter/C7_invalid_tick_preserves_unled、C10_late_duration_keeps_unled
//     与 TestResumePositionRecording241/late_duration_no_false_seek 红；
//   - M4（convert 退回直接调 truePositionSec(rawSec, playing)）→
//     TestResumePositionRecording241 全部 3 个子测试及 TestPosConverter 的 C1/C2/C3/C4/C5/C7/C8/C10 红；
//   - M5（去掉 !playing 分支中的 if iconKnown && c.hasLed && rawSec == c.ledRaw）→
//     TestPosConverter 的 P3/P4/P5/P7/P8/P12 与 TestPausePositionRecording254 全部 3 个子测试红；
//   - M6（命中 hasLed 一次即清 c.hasLed = false）→
//     TestPosConverter/P3_multi_tick_stale_after_pause、P8_invalid_tick_preserves_led 红；
//   - M7a（命中 hasLed 时误置 c.hasUnled = true，不改 unledRaw）→
//     TestPosConverter 的 P3/P4/P5/P7/P8/P12 与 TestPausePositionRecording254 全部 3 个子测试互斥不变量红；
//   - M7b（命中 hasLed 时同时误写 c.hasUnled = true; c.unledRaw = rawSec）→
//     P4 输出（step 2 返回 100.0）与上述互斥不变量同红；
//   - M8（去掉 hasLed 的 iconKnown 门控：全删、仅删 !playing 侧或仅删 playing 侧）→
//     TestPosConverter/C8_P11_icon_unknown_stall_fallback 红；
//   - M9a / M9b（convertTick 换歌前或夹零分支漏清 hasLed）→
//     TestPosConverter/P9_song_change_resets_led、P10_stale_clamped_resets_led 各自变红；
//   - M10a / M10b / M10c（!(rawSec >= 0)、!playing 超限、playing 超限分支清除 hasLed）→
//     TestPosConverter/P8_invalid_tick_preserves_led 各自变红；
//   - M10d（把 !playing 超限检查提前到 hasLed 命中之前）→
//     TestPosConverter/P12_short_track_end_pause_before_overlimit 红。

import (
	"fmt"
	"math"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/spotify/lyric"
)

func TestTruePositionAppliesLeadOnlyWhilePlaying(t *testing.T) {
	cases := []struct {
		raw     float32
		playing bool
		want    float32
	}{
		{120.0, true, 119.0},   // 播放中：读数超前 1s
		{120.0, false, 120.0},  // 暂停：读数就是位置，原样
		{0.5, true, 0.0},       // 刚开播：读数还没有超前 1s，取 0 而不是负数
		{1.0, true, 0.0},       // 边界：正好等于超前量
		{1.2, true, 0.200000},  // 略过边界
		{178.63, true, 177.63}, // 真机读数样例（/spotify/lyric_update 实测过 178.64）
	}
	for _, c := range cases {
		got := truePositionSec(c.raw, c.playing)
		if diff := got - c.want; diff > 0.001 || diff < -0.001 {
			t.Errorf("truePositionSec(%.3f, %v) = %.3f，期望 %.3f", c.raw, c.playing, got, c.want)
		}
	}
}

func assertPosNear(t *testing.T, label string, got, want float32) {
	t.Helper()
	if math.IsNaN(float64(want)) {
		if !math.IsNaN(float64(got)) {
			t.Errorf("%s = %.5f，期望 NaN", label, got)
		}
		return
	}
	if math.IsNaN(float64(got)) || math.Abs(float64(got-want)) > 0.001 {
		t.Errorf("%s = %.5f，期望 %.5f（容差 0.001）", label, got, want)
	}
}

func assertConverterInvariant(t *testing.T, label string, c posConverter) {
	t.Helper()
	if c.hasLed && c.hasUnled {
		t.Errorf("%s 破坏互斥不变量: hasLed=%v (ledRaw=%.3f) && hasUnled=%v (unledRaw=%.3f)",
			label, c.hasLed, c.ledRaw, c.hasUnled, c.unledRaw)
	}
}

// TestPosConverter 覆盖 posConverter.convert 与 convertTick 的双向状态迁移、换歌复位、夹零复位与异常读数保护。
func TestPosConverter(t *testing.T) {
	type convStep struct {
		raw       float32
		dur       float32
		playing   bool
		iconKnown bool
		want      float32
	}
	runSteps := func(t *testing.T, steps []convStep) {
		t.Helper()
		var conv posConverter
		for i, s := range steps {
			label := fmt.Sprintf("step %d: convert(%v, %.3f, playing=%v, iconKnown=%v)", i, s.raw, s.dur, s.playing, s.iconKnown)
			got := conv.convert(s.raw, s.dur, s.playing, s.iconKnown)
			assertPosNear(t, label, got, s.want)
			assertConverterInvariant(t, label, conv)
		}
	}

	t.Run("C1_attach_paused_then_resume", func(t *testing.T) {
		runSteps(t, []convStep{
			{215.512, 339.434, false, true, 215.512},
			{215.512, 339.434, true, true, 215.512},
			{216.912, 339.434, true, true, 215.912},
		})
	})

	t.Run("C2_paused_drag_then_resume", func(t *testing.T) {
		runSteps(t, []convStep{
			{68.491, 339.434, false, true, 68.491},
			{161.85966, 339.434, false, true, 161.85966},
			{161.85966, 339.434, true, true, 161.85966},
			{163.067, 339.434, true, true, 162.067},
		})
	})

	t.Run("C3_multi_tick_stale_after_resume", func(t *testing.T) {
		runSteps(t, []convStep{
			{100.0, 300, false, true, 100.0},
			{100.0, 300, true, true, 100.0},
			{100.0, 300, true, true, 100.0},
			{100.0, 300, true, true, 100.0},
			{102.0, 300, true, true, 101.0},
		})
	})

	t.Run("C4_resume_repause_resume_without_dom_refresh", func(t *testing.T) {
		runSteps(t, []convStep{
			{50.0, 300, false, true, 50.0},
			{50.0, 300, true, true, 50.0},
			{50.0, 300, false, true, 50.0},
			{50.0, 300, true, true, 50.0},
		})
	})

	t.Run("C5_resume_near_zero", func(t *testing.T) {
		runSteps(t, []convStep{
			{0.4, 300, false, true, 0.4},
			{0.4, 300, true, true, 0.4},
			{1.6, 300, true, true, 0.6},
		})
		runSteps(t, []convStep{
			{0.0, 300, false, true, 0.0},
			{0.0, 300, true, true, 0.0},
			{1.2, 300, true, true, 0.2},
		})
	})

	t.Run("C6_song_change_resets_unled", func(t *testing.T) {
		// 序列 a：曲 A 0.8s 暂停，切曲 B 首拍 0.8s 播放中应已复位并扣超前到 0.0。
		var convA posConverter
		pos, stale, clamped := convA.convertTick(0.8, 300, false, true, true, true)
		assertPosNear(t, "seqA trackA pos", pos, 0.8)
		assertConverterInvariant(t, "seqA trackA", convA)
		if stale != 0 || clamped {
			t.Fatalf("seqA trackA stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
		pos, stale, clamped = convA.convertTick(0.8, 300, true, true, true, false)
		assertPosNear(t, "seqA trackB pos", pos, 0.0)
		assertConverterInvariant(t, "seqA trackB", convA)
		if stale != 0 || clamped {
			t.Fatalf("seqA trackB stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}

		// 序列 b：曲 A 2.5s 暂停，切曲 B 首拍 2.5s 播放中应已复位并扣超前到 1.5。
		var convB posConverter
		pos, stale, clamped = convB.convertTick(2.5, 300, false, true, true, true)
		assertPosNear(t, "seqB trackA pos", pos, 2.5)
		assertConverterInvariant(t, "seqB trackA", convB)
		if stale != 0 || clamped {
			t.Fatalf("seqB trackA stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
		pos, stale, clamped = convB.convertTick(2.5, 300, true, true, true, false)
		assertPosNear(t, "seqB trackB pos", pos, 1.5)
		assertConverterInvariant(t, "seqB trackB", convB)
		if stale != 0 || clamped {
			t.Fatalf("seqB trackB stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
	})

	t.Run("C6_stale_clamped_resets", func(t *testing.T) {
		var conv posConverter
		// 曲 A（首曲不夹）
		pos, stale, clamped := conv.convertTick(180.0, 300, true, true, true, true)
		assertPosNear(t, "trackA pos", pos, 179.0)
		assertConverterInvariant(t, "trackA", conv)
		if stale != 0 || clamped {
			t.Fatalf("trackA stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
		// 换曲 B 首拍暂停态残留 200.0 > trackStartGraceSec：夹为 0 并复位无超前标记
		pos, stale, clamped = conv.convertTick(200.0, 300, false, true, true, false)
		assertPosNear(t, "trackB stale clamped pos", pos, 0.0)
		assertPosNear(t, "trackB stalePos", stale, 200.0)
		assertConverterInvariant(t, "trackB clamped", conv)
		if !clamped {
			t.Fatalf("trackB clamped=false，期望 true")
		}
		// 下一拍曲 B 播放态读到 200.0：应扣超前返回 199.0，不能残留上拍夹零前的无超前标记
		pos, stale, clamped = conv.convertTick(200.0, 300, true, true, false, false)
		assertPosNear(t, "trackB next tick pos", pos, 199.0)
		assertConverterInvariant(t, "trackB next tick", conv)
		if stale != 0 || clamped {
			t.Fatalf("trackB next tick stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
	})

	t.Run("C7_invalid_tick_preserves_unled", func(t *testing.T) {
		nan32 := float32(math.NaN())
		runSteps(t, []convStep{
			{80.0, 100, false, true, 80.0},
			{80.0, 100, true, true, 80.0},
			{nan32, 100, true, true, nan32},
			{-1.0, 100, true, true, -1.0},
			{120.0, 100, false, true, 120.0},
			{120.0, 100, true, true, 119.0},
			{80.0, 100, true, true, 80.0},
		})
	})

	t.Run("C8_P11_icon_unknown_stall_fallback", func(t *testing.T) {
		// iconKnown == false 退回 stallWindow：满 2.5s 判暂停立即释放真实位置，不锁死在 99.4。
		runSteps(t, []convStep{
			{100.4, 300, true, false, 99.4},
			{100.4, 300, false, false, 100.4},
			{100.4, 300, true, false, 100.4},
		})
		// 中途失去图标可信度（iconKnown true → false）时同样不沿用旧 hasLed。
		runSteps(t, []convStep{
			{100.4, 300, true, true, 99.4},
			{100.4, 300, false, false, 100.4},
		})
		// iconKnown == false 的播放拍须清掉此前残留的 hasLed，随后即使图标恢复也不误扣。
		runSteps(t, []convStep{
			{100.4, 300, true, true, 99.4},
			{100.4, 300, true, false, 99.4},
			{100.4, 300, false, true, 100.4},
		})
	})

	t.Run("C9_immediate_refresh_on_resume_tick", func(t *testing.T) {
		runSteps(t, []convStep{
			{100.0, 300, false, true, 100.0},
			{101.05, 300, true, true, 100.05},
		})
	})

	t.Run("C10_late_duration_keeps_unled", func(t *testing.T) {
		runSteps(t, []convStep{
			{105.5, 0, false, true, 105.5},
			{105.5, 100, true, true, 105.5},
			{105.5, 100, true, true, 105.5},
		})
	})

	t.Run("P3_multi_tick_stale_after_pause", func(t *testing.T) {
		runSteps(t, []convStep{
			{100.0, 300, true, true, 99.0},
			{100.0, 300, false, true, 99.0},
			{100.0, 300, false, true, 99.0},
			{100.0, 300, false, true, 99.0},
			{99.3, 300, false, true, 99.3},
		})
	})

	t.Run("P4_pause_then_resume_without_dom_refresh", func(t *testing.T) {
		runSteps(t, []convStep{
			{100.0, 300, true, true, 99.0},
			{100.0, 300, false, true, 99.0},
			{100.0, 300, true, true, 99.0},
		})
	})

	t.Run("P5_paused_manual_seek_forward_and_backward", func(t *testing.T) {
		runSteps(t, []convStep{
			{100.0, 300, true, true, 99.0},
			{100.0, 300, false, true, 99.0},
			{145.5, 300, false, true, 145.5},
			{40.2, 300, false, true, 40.2},
			{40.2, 300, true, true, 40.2},
		})
	})

	t.Run("P6_immediate_rollback_on_pause_tick", func(t *testing.T) {
		runSteps(t, []convStep{
			{100.0, 300, true, true, 99.0},
			{99.4, 300, false, true, 99.4},
			{99.4, 300, true, true, 99.4},
		})
	})

	t.Run("P7_pause_near_zero_before_rollback", func(t *testing.T) {
		runSteps(t, []convStep{
			{0.4, 300, true, true, 0.0},
			{0.4, 300, false, true, 0.0},
			{0.15, 300, false, true, 0.15},
		})
	})

	t.Run("P8_invalid_tick_preserves_led", func(t *testing.T) {
		nan32 := float32(math.NaN())
		runSteps(t, []convStep{
			{80.0, 100, true, true, 79.0},
			{80.0, 100, false, true, 79.0},
			{nan32, 100, false, true, nan32},
			{-1.0, 100, false, true, -1.0},
			{120.0, 100, false, true, 120.0},
			{120.0, 100, true, true, 119.0},
			{80.0, 100, false, true, 79.0},
		})
	})

	t.Run("P9_song_change_resets_led", func(t *testing.T) {
		var conv posConverter
		pos, stale, clamped := conv.convertTick(2.5, 300, true, true, true, true)
		assertPosNear(t, "trackA pos", pos, 1.5)
		assertConverterInvariant(t, "trackA", conv)
		if stale != 0 || clamped {
			t.Fatalf("trackA stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
		pos, stale, clamped = conv.convertTick(2.5, 300, false, true, true, false)
		assertPosNear(t, "trackB pos", pos, 2.5)
		assertConverterInvariant(t, "trackB", conv)
		if stale != 0 || clamped {
			t.Fatalf("trackB stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
	})

	t.Run("P10_stale_clamped_resets_led", func(t *testing.T) {
		var conv posConverter
		pos, stale, clamped := conv.convertTick(180.0, 300, true, true, true, true)
		assertPosNear(t, "trackA pos", pos, 179.0)
		assertConverterInvariant(t, "trackA", conv)
		if stale != 0 || clamped {
			t.Fatalf("trackA stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
		pos, stale, clamped = conv.convertTick(200.0, 300, true, true, true, false)
		assertPosNear(t, "trackB stale clamped pos", pos, 0.0)
		assertPosNear(t, "trackB stalePos", stale, 199.0)
		assertConverterInvariant(t, "trackB clamped", conv)
		if !clamped {
			t.Fatalf("trackB clamped=false，期望 true")
		}
		pos, stale, clamped = conv.convertTick(200.0, 300, false, true, false, false)
		assertPosNear(t, "trackB next tick pos", pos, 200.0)
		assertConverterInvariant(t, "trackB next tick", conv)
		if stale != 0 || clamped {
			t.Fatalf("trackB next tick stale=%.3f clamped=%v，期望 0/false", stale, clamped)
		}
	})

	t.Run("P12_short_track_end_pause_before_overlimit", func(t *testing.T) {
		var conv posConverter
		pos1 := conv.convert(15.8, 15.0, true, true)
		assertPosNear(t, "short track playing", pos1, 14.8)
		assertConverterInvariant(t, "short track playing", conv)
		pos2 := conv.convert(15.8, 15.0, false, true)
		assertPosNear(t, "short track pause lag", pos2, 14.8)
		assertConverterInvariant(t, "short track pause lag", conv)
		if raw := clockRaw(pos2, 15.0); math.Abs(raw-14.8) > 0.001 {
			t.Fatalf("short track pause clockRaw = %.3f，期望 14.8（不被超限误拒为 -1）", raw)
		}
	})
}

// TestPausePositionRecording254 按 runSession 同一调用顺序回放 #254 实测暂停序列（sample_out.json / repeat_out.json）。
func TestPausePositionRecording254(t *testing.T) {
	const (
		dur       = float32(192.965)
		offsetSec = float32(-0.2)
	)
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	matchLine := func(lyrics []lyric.Line, interpSec float32) int {
		matchTime := player.LyricMatchTime(interpSec, offsetSec)
		for i := len(lyrics) - 1; i >= 0; i-- {
			if matchTime >= lyrics[i].Time {
				return i
			}
		}
		return -1
	}

	t.Run("P1_first_round_pause_rollback_and_resume", func(t *testing.T) {
		var posConv posConverter
		var clk clock

		// 拍 1：t0 稳态播放落锚（128.341 含 +1.0s 超前 → 127.341）
		pos := posConv.convert(128.341, dur, true, true)
		assertPosNear(t, "playing tick pos", pos, 127.341)
		assertConverterInvariant(t, "P1 tick 1", posConv)
		clk.reset()
		clk.observe(clockRaw(pos, dur), true, float64(dur), t0)
		if ev := clk.observe(clockRaw(pos, dur), true, float64(dur), t0); ev != evNone {
			t.Fatalf("落锚同拍 ev = %v，期望 evNone", ev)
		}

		// 拍 2：t0+122ms 按钮翻为 paused，进度条尚未回落仍为 128.341
		t1 := t0.Add(122 * time.Millisecond)
		pos = posConv.convert(128.341, dur, false, true)
		assertPosNear(t, "pause lag tick pos", pos, 127.341)
		assertConverterInvariant(t, "P1 tick 2", posConv)
		ev := clk.observe(clockRaw(pos, dur), false, float64(dur), t1)
		emits := clockEmits(ev, false, pos, float32(clk.liveAt(t1)))
		if len(emits) != 1 || emits[0].event != player.EventPlaybackPause || emits[0].status != "paused" {
			t.Fatalf("暂停当拍事件 = %+v，期望唯一 EventPlaybackPause + status=paused", emits)
		}
		assertPosNear(t, "playback_pause pos", emits[0].pos, 127.463)

		// 拍 3：t0+322ms 页面回落至真实暂停位置 127.117
		t2 := t0.Add(322 * time.Millisecond)
		pos = posConv.convert(127.117, dur, false, true)
		assertPosNear(t, "rollback tick pos", pos, 127.117)
		assertConverterInvariant(t, "P1 tick 3", posConv)
		if ev := clk.observe(clockRaw(pos, dur), false, float64(dur), t2); ev != evNone {
			t.Fatalf("回落拍 ev = %v，期望 evNone", ev)
		}
		assertPosNear(t, "rollback liveAt", float32(clk.liveAt(t2)), 127.117)

		// 拍 4：t0+522ms 恢复播放（首拍仍为 127.117，不倒退 1.0s）
		t3 := t0.Add(522 * time.Millisecond)
		pos = posConv.convert(127.117, dur, true, true)
		assertPosNear(t, "resume tick pos", pos, 127.117)
		assertConverterInvariant(t, "P1 tick 4", posConv)
		ev = clk.observe(clockRaw(pos, dur), true, float64(dur), t3)
		emits = clockEmits(ev, true, pos, float32(clk.liveAt(t3)))
		if len(emits) != 1 || emits[0].event != player.EventPlaybackResume || emits[0].status != "playing" {
			t.Fatalf("恢复首拍事件 = %+v，期望唯一 EventPlaybackResume + status=playing", emits)
		}
		assertPosNear(t, "playback_resume pos", emits[0].pos, 127.117)
	})

	runRepeatT1 := func(t *testing.T, nextLineTime float32) (pausePos float32, emitted []int) {
		t.Helper()
		lyrics := []lyric.Line{
			{Index: 0, Time: 125.0},
			{Index: 1, Time: nextLineTime},
		}
		var posConv posConverter
		var clk clock
		lastLineIdx := -1
		emitLineIfChanged := func(interpSec float32) {
			idx := matchLine(lyrics, interpSec)
			if idx >= 0 && idx != lastLineIdx {
				lastLineIdx = idx
				emitted = append(emitted, idx)
			}
		}

		// 拍 1：t0 稳态播放 130.987 → 129.987
		pos := posConv.convert(130.987, dur, true, true)
		assertPosNear(t, "T1 playing pos", pos, 129.987)
		assertConverterInvariant(t, "T1 tick 1", posConv)
		clk.reset()
		clk.observe(clockRaw(pos, dur), true, float64(dur), t0)
		clk.observe(clockRaw(pos, dur), true, float64(dur), t0)
		emitLineIfChanged(float32(clk.liveAt(t0)))

		// 拍 2：t0+442ms 暂停当拍 130.987 → 129.987，clk.liveAt = 130.429
		t1 := t0.Add(442 * time.Millisecond)
		pos = posConv.convert(130.987, dur, false, true)
		assertPosNear(t, "T1 pause lag pos", pos, 129.987)
		assertConverterInvariant(t, "T1 tick 2", posConv)
		ev := clk.observe(clockRaw(pos, dur), false, float64(dur), t1)
		emits := clockEmits(ev, false, pos, float32(clk.liveAt(t1)))
		if len(emits) != 1 || emits[0].event != player.EventPlaybackPause {
			t.Fatalf("T1 暂停当拍事件 = %+v，期望唯一 EventPlaybackPause", emits)
		}
		pausePos = emits[0].pos
		emitLineIfChanged(float32(clk.liveAt(t1)))

		// 拍 3：t0+642ms 页面回落至 130.297
		t2 := t0.Add(642 * time.Millisecond)
		pos = posConv.convert(130.297, dur, false, true)
		assertPosNear(t, "T1 rollback pos", pos, 130.297)
		assertConverterInvariant(t, "T1 tick 3", posConv)
		if ev := clk.observe(clockRaw(pos, dur), false, float64(dur), t2); ev != evNone {
			t.Fatalf("T1 回落拍 ev = %v，期望 evNone", ev)
		}
		emitLineIfChanged(float32(clk.liveAt(t2)))
		return pausePos, emitted
	}

	t.Run("P2_repeat_T1_eliminates_1s_lyric_flash", func(t *testing.T) {
		pausePos, emitted := runRepeatT1(t, 130.7)
		assertPosNear(t, "T1 pausePos", pausePos, 130.429)
		if len(emitted) != 1 || emitted[0] != 0 {
			t.Fatalf("T1（T_N=130.7）歌词推送序列 = %v，期望仅推送 [0]（消除整秒超前闪跳）", emitted)
		}
	})

	// 记录设计文档 §5 第 1 条接受的窄窗残余回落现状：当 T_N 落入 [130.497, 130.629) 时，
	// 墙钟外推 130.429（matchTime=130.629）仍会在暂停当拍推出行 1、回落拍 130.297（matchTime=130.497）退回行 0。
	t.Run("P2_repeat_T1_narrow_residual_boundary", func(t *testing.T) {
		pausePos, emitted := runRepeatT1(t, 130.6)
		assertPosNear(t, "T1 residual pausePos", pausePos, 130.429)
		want := []int{0, 1, 0}
		if len(emitted) != len(want) || emitted[0] != 0 || emitted[1] != 1 || emitted[2] != 0 {
			t.Fatalf("T1 窄窗残余（T_N=130.6）歌词推送序列 = %v，期望记录现状 %v", emitted, want)
		}
	})
}

// TestResumePositionRecording241 按 runSession 同一调用顺序回放 #241 真机录音序列与迟到 duration 场景。
// 变异自证（实测）：M1 与 M3b 触发 late_duration_no_false_seek 红；
// M4 触发 first_resume_at_215_512、paused_drag_then_resume_no_line_rollback、late_duration_no_false_seek 全部红。
func TestResumePositionRecording241(t *testing.T) {
	const (
		dur       = float32(339.434)
		offsetSec = float32(-0.2)
	)
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	matchLine := func(lyrics []lyric.Line, interpSec float32) int {
		matchTime := player.LyricMatchTime(interpSec, offsetSec)
		for i := len(lyrics) - 1; i >= 0; i-- {
			if matchTime >= lyrics[i].Time {
				return i
			}
		}
		return -1
	}

	t.Run("first_resume_at_215_512", func(t *testing.T) {
		var posConv posConverter
		var clk clock

		// 拍 1：t0 暂停落锚
		pos := posConv.convert(215.512, dur, false, true)
		assertConverterInvariant(t, "241_1 tick 1", posConv)
		clk.reset()
		clk.observe(clockRaw(pos, dur), false, float64(dur), t0)
		if ev := clk.observe(clockRaw(pos, dur), false, float64(dur), t0); ev != evNone {
			t.Fatalf("落锚同拍 ev = %v，期望 evNone", ev)
		}

		// 拍 2：t0+3803ms 恢复播放，页面尚未刷新读数
		t1 := t0.Add(3803 * time.Millisecond)
		pos = posConv.convert(215.512, dur, true, true)
		assertConverterInvariant(t, "241_1 tick 2", posConv)
		ev := clk.observe(clockRaw(pos, dur), true, float64(dur), t1)
		emits := clockEmits(ev, true, pos, float32(clk.liveAt(t1)))
		if len(emits) != 1 {
			t.Fatalf("恢复拍发出 %d 条事件，期望 1 条: %+v", len(emits), emits)
		}
		e := emits[0]
		if e.event != player.EventPlaybackResume || e.status != "playing" {
			t.Fatalf("恢复拍事件 = %+v，期望 EventPlaybackResume + status=playing", e)
		}
		if math.Abs(float64(e.pos-215.512)) >= 1e-3 {
			t.Fatalf("恢复拍位置 = %.5f，期望 215.512（容差 1e-3）", e.pos)
		}

		// 拍 3：t0+6131ms 页面刷出新读数 218.8673
		t2 := t0.Add(6131 * time.Millisecond)
		pos = posConv.convert(218.8673, dur, true, true)
		assertConverterInvariant(t, "241_1 tick 3", posConv)
		ev = clk.observe(clockRaw(pos, dur), true, float64(dur), t2)
		if ev != evNone {
			t.Fatalf("刷新拍 ev = %v，期望 evNone", ev)
		}
	})

	t.Run("paused_drag_then_resume_no_line_rollback", func(t *testing.T) {
		lyrics := make([]lyric.Line, 23)
		for i := 0; i < 21; i++ {
			lyrics[i] = lyric.Line{Index: i, Time: float32(i * 5)}
		}
		lyrics[21] = lyric.Line{Index: 21, Time: 157.176}
		lyrics[22] = lyric.Line{Index: 22, Time: 162.027}

		var posConv posConverter
		var clk clock
		lastLineIdx := -1
		var emitted []int
		emitLineIfChanged := func(interpSec float32) int {
			idx := matchLine(lyrics, interpSec)
			if idx >= 0 && idx != lastLineIdx {
				lastLineIdx = idx
				emitted = append(emitted, idx)
			}
			return idx
		}

		// 拍 1：t0 暂停于 68.491
		pos := posConv.convert(68.491, dur, false, true)
		assertConverterInvariant(t, "241_2 tick 1", posConv)
		clk.reset()
		clk.observe(clockRaw(pos, dur), false, float64(dur), t0)
		clk.observe(clockRaw(pos, dur), false, float64(dur), t0)
		emitLineIfChanged(float32(clk.liveAt(t0)))

		// 拍 2：t0+404ms 暂停态拖到 161.85966，应匹配并推出 idx == 22
		t1 := t0.Add(404 * time.Millisecond)
		pos = posConv.convert(161.85966, dur, false, true)
		assertConverterInvariant(t, "241_2 tick 2", posConv)
		clk.observe(clockRaw(pos, dur), false, float64(dur), t1)
		beforeCount := len(emitted)
		idx := emitLineIfChanged(float32(clk.liveAt(t1)))
		if idx != 22 || len(emitted) != beforeCount+1 || emitted[len(emitted)-1] != 22 {
			t.Fatalf("暂停拖动拍匹配 idx=%d emitted=%v，期望推出 idx=22", idx, emitted)
		}

		// 拍 3：t0+1087ms 恢复播放（161.85966 未变），t0+1094ms 取 liveAt 匹配歌词
		t2 := t0.Add(1087 * time.Millisecond)
		t2Lyric := t0.Add(1094 * time.Millisecond)
		pos = posConv.convert(161.85966, dur, true, true)
		assertConverterInvariant(t, "241_2 tick 3", posConv)
		ev := clk.observe(clockRaw(pos, dur), true, float64(dur), t2)
		emits := clockEmits(ev, true, pos, float32(clk.liveAt(t2)))
		if len(emits) != 1 || emits[0].event != player.EventPlaybackResume || emits[0].status != "playing" {
			t.Fatalf("恢复拍事件 = %+v，期望唯一 EventPlaybackResume + status=playing", emits)
		}
		if math.Abs(float64(emits[0].pos-161.85966)) >= 1e-3 {
			t.Fatalf("恢复拍位置 = %.5f，期望 161.85966（容差 1e-3）", emits[0].pos)
		}
		beforeCount = len(emitted)
		idx = emitLineIfChanged(float32(clk.liveAt(t2Lyric)))
		if idx != 22 || len(emitted) != beforeCount {
			t.Fatalf("恢复拍行号回退或重复推送: idx=%d emitted=%v（不期望推出 idx=21）", idx, emitted)
		}

		// 拍 4：t0+1292ms 页面刷出 163.067，ev == evNone，行号保持 22 不重复发出
		t3 := t0.Add(1292 * time.Millisecond)
		pos = posConv.convert(163.067, dur, true, true)
		assertConverterInvariant(t, "241_2 tick 4", posConv)
		ev = clk.observe(clockRaw(pos, dur), true, float64(dur), t3)
		if ev != evNone {
			t.Fatalf("刷新拍 ev = %v，期望 evNone", ev)
		}
		beforeCount = len(emitted)
		idx = emitLineIfChanged(float32(clk.liveAt(t3)))
		if idx != 22 || len(emitted) != beforeCount {
			t.Fatalf("刷新拍行号异常或重复推送: idx=%d emitted=%v", idx, emitted)
		}
	})

	t.Run("late_duration_no_false_seek", func(t *testing.T) {
		var posConv posConverter
		var clk clock

		// 拍 1：t0 暂停落锚，此时时长尚未到达（dur=0）
		pos1 := posConv.convert(105.5, 0, false, true)
		assertConverterInvariant(t, "241_3 tick 1", posConv)
		clk.reset()
		clk.observe(clockRaw(pos1, 0), false, 0, t0)
		clk.observe(clockRaw(pos1, 0), false, 0, t0)

		// 拍 2：t0+200ms 时长补到 100 且恢复播放，原始读数仍为 105.5（超 dur*1.05）
		t1 := t0.Add(200 * time.Millisecond)
		pos2 := posConv.convert(105.5, 100, true, true)
		assertConverterInvariant(t, "241_3 tick 2", posConv)
		raw2 := clockRaw(pos2, 100)
		if raw2 != -1 {
			t.Fatalf("第 2 拍 clockRaw = %.3f，期望 -1", raw2)
		}
		clk.observe(raw2, true, 100, t1)

		// 拍 3：t0+400ms 原始读数仍为 105.5，应继续保持无超前口径并判非法，不产生 evSeek
		t2 := t0.Add(400 * time.Millisecond)
		pos3 := posConv.convert(105.5, 100, true, true)
		assertConverterInvariant(t, "241_3 tick 3", posConv)
		raw3 := clockRaw(pos3, 100)
		if raw3 != -1 {
			t.Fatalf("第 3 拍 clockRaw = %.3f，期望 -1", raw3)
		}
		ev3 := clk.observe(raw3, true, 100, t2)
		if ev3 != evNone {
			t.Fatalf("第 3 拍 ev = %v，期望 evNone（不产生 evSeek）", ev3)
		}
	})
}
