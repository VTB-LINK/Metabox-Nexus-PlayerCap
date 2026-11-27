package qqmusic

// 本文件只测同一首重开的统一跨拍守卫 reopenGuard（#245，doc/design/qqmusic-assoc-followup-design.md §3.3）：
// 规则 R1（载入边沿及占位串挂起兑现）、规则 R2（暂扣放行回零兜底，不受 !loadUsable 封锁）、规则 R3（暂停在开头 < 1.0s
// 恢复兜底）与统一跨拍去重（consumed / sawStart / consumedMs / pendingEdge 的登记与确定性复位）。
// runSession 的接线由 assocwiring_test.go 守。
//
// 变异自证（在隔离临时副本中实测）：
//   - shouldReopenByLoading 遇到占位串清空 pendingEdge → TestReopenGuardRealSequence1622 与 TestReopenGuardCounterexampleD 变体 2/3 红；
//   - onPause 在 v.pausedSeek 时也调用 → TestReopenGuardCounterexampleB 与 TestReopenGuardCounterexampleD 红；
//   - stepProgress 要求 !isPaused 才解除 consumed → TestReopenGuardCounterexampleC 红；
//   - stepProgress 删掉 >= consumedMs + resumeStepMs → TestReopenGuardCounterexampleA 继续推进子例红；
//   - R3 改回不核对 meta.Name == lastName → TestReopenGuardPlaceholderFallback（Codex P2 反例）与 TestRunSessionAssocWiring 双红；
//   - 去掉拍首 else if 挂起兑现 → TestReopenGuardPlaceholderFallback（占位串恢复同一首）与 TestRunSessionAssocWiring 双红；
//   - qqmusic.go R2 放回 !loadUsable 或漏 !reopenG.consumed → TestRunSessionAssocWiring 红。

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// guardEvent 记录复述中每一次 reopenSong 的触发拍号、触发方式与当时进度。
type guardEvent struct {
	tick       int
	byLoading  bool
	progressMs uint32
}

// reopenGuardReplay 照 runSession 复述与同一首重开、暂扣、暂停恢复相关的确定性状态机链路：
// 使用真实的 loadEdge、reopenGuard、switchHold、progressCarriedOver、midWaitGate 与 judgeTick。
type reopenGuardReplay struct {
	spec playStateSpec
	poll time.Duration
	now  time.Time
	tick int

	loadOpen     loadEdge
	reopenG      reopenGuard
	hold         switchHold
	progressWait midWaitGate
	judge        judgeState

	lastName         string
	lastMemProgress  uint32
	anchorProgressMs uint32
	anchorTime       time.Time
	isPaused         bool

	gen    uint64
	reopen uint64
	events []guardEvent

	// 仅用于变异对照或断言观测最近一拍的 verdict 与放行标记。
	lastVerdict  verdict
	lastHeld     bool
	lastReleased bool
}

func newReopenGuardReplay(poll time.Duration) *reopenGuardReplay {
	return &reopenGuardReplay{
		spec:     knownVersions["22.82"].PlayState,
		poll:     poll,
		now:      time.Date(2026, 11, 24, 16, 22, 0, 0, time.Local),
		loadOpen: newLoadEdge(poll),
	}
}

func (r *reopenGuardReplay) reopenSong(byLoading bool, progressMs uint32) {
	r.reopenG.noteReopen(progressMs)
	r.reopen++
	r.events = append(r.events, guardEvent{tick: r.tick, byLoading: byLoading, progressMs: progressMs})
}

// step 推进一拍 poll 时长，严格按 runSession 顺序执行 R1、换歌认领、暂扣、R2、judgeTick、R3、onPause 与 stepProgress。
func (r *reopenGuardReplay) step(name string, progressMs uint32, raw uint32, rawOK bool, hasIDs bool) {
	r.now = r.now.Add(r.poll)
	r.tick++
	sampleTime := r.now
	r.lastVerdict = verdict{}
	r.lastHeld, r.lastReleased = false, false

	loadUsable := r.spec.loadingUsable(r.judge.bad, r.poll)
	reopenedThisTick := false
	if edge := r.loadOpen.step(r.spec, raw, rawOK, sampleTime); r.reopenG.shouldReopenByLoading(loadUsable, edge, name, r.lastName, hasIDs) && !reopenedThisTick {
		reopenedThisTick = true
		r.reopenSong(true, progressMs)
	} else if !reopenedThisTick && r.reopenG.shouldReopenByFallback(false, name, r.lastName, hasIDs) {
		reopenedThisTick = true
		r.reopenSong(false, progressMs)
	}

	if name != r.lastName && name != "" && !strings.HasPrefix(name, "QQ音乐") {
		if progressCarriedOver(r.lastName, r.lastMemProgress, progressMs) {
			if _, keepWaiting := r.progressWait.wait(name, sampleTime, progressSettleWindow); keepWaiting {
				return
			}
		}
		r.progressWait.reset()
		r.hold.drop(sampleTime)
		r.lastName = name
		r.anchorProgressMs = progressMs
		r.anchorTime = sampleTime
		r.lastMemProgress = progressMs
		r.isPaused = false
		r.judge.resume.reset()
		r.reopenG.reset()
		r.gen++
		r.reopen = 0
	} else if name == r.lastName {
		r.progressWait.reset()
	}

	held, released := r.hold.step(r.lastMemProgress, progressMs, sampleTime)
	r.lastHeld, r.lastReleased = held, released
	if held {
		return
	}

	var changed, seek bool
	var step int64
	if progressMs != r.lastMemProgress {
		seekDetected := released && progressMs < r.lastMemProgress
		if r.lastMemProgress > 0 {
			if seekBack(r.lastMemProgress, progressMs) {
				seekDetected = true
			}
			if !seekDetected && progressMs > r.lastMemProgress {
				advance := progressMs - r.lastMemProgress
				elapsed := uint32(sampleTime.Sub(r.anchorTime).Milliseconds())
				if advance > elapsed+2000 {
					seekDetected = true
				}
			}
		}
		if seekDetected {
			if r.reopenG.shouldReopenByFallback(released && progressMs < r.lastMemProgress, name, r.lastName, hasIDs) && !reopenedThisTick {
				reopenedThisTick = true
				r.reopenSong(false, progressMs)
			}
		}
		changed, seek, step = true, seekDetected, int64(progressMs)-int64(r.lastMemProgress)
		r.anchorProgressMs = progressMs
		r.anchorTime = sampleTime
		r.lastMemProgress = progressMs
	}

	var v verdict
	r.judge, v = judgeTick(r.judge, judgeIn{
		spec: r.spec, raw: raw, rawOK: rawOK, poll: r.poll,
		paused: r.isPaused, claimed: r.lastName != "", changed: changed, seek: seek, step: step,
		idle: sampleTime.Sub(r.anchorTime), now: sampleTime,
	})
	r.lastVerdict = v
	if v.resume {
		r.isPaused = false
		r.anchorTime = sampleTime
		if r.reopenG.shouldReopenByFallback(r.anchorProgressMs < switchStartBandMs, name, r.lastName, hasIDs) && !reopenedThisTick {
			reopenedThisTick = true
			r.reopenSong(false, r.anchorProgressMs)
		}
	}
	if v.pause {
		r.isPaused = true
		r.reopenG.onPause()
	}
	r.reopenG.stepProgress(progressMs, held, r.loadOpen.loading)
}

func (r *reopenGuardReplay) claimAndPlay(name string, ms uint32) {
	r.step(name, 0, 3, true, true)
	r.step(name, ms, 3, true, true)
}

func (r *reopenGuardReplay) pauseAt(name string, ms uint32) {
	r.step(name, ms, 4, true, true)
}

// 场景 1：16:22:16～16:22:35 真实序列回归（设计 §5.2 第 2 项第 1 条）：
// 已认领曲 A → 暂停 22.57s → 占位串 "QQ音乐 听我想听" 拍进 PlayState=2 → 100ms 后歌名恢复曲 A 且 PlayState 仍为 2 →
// 当拍触发 1 次 reopenSong(true)，随后 22.73s → 0.00s 经 400ms 暂扣放行拍不重复触发第 2 次。
func TestReopenGuardRealSequence1622(t *testing.T) {
	const songA = "君の知らない物語 (Inst.)"
	r := newReopenGuardReplay(50 * time.Millisecond)
	r.claimAndPlay(songA, 22570)
	r.pauseAt(songA, 22570)
	if !r.isPaused {
		t.Fatalf("期望已判暂停")
	}

	// t=0ms、50ms：占位串拍进入载入值 2（含暂停后尾巴补写至 22730ms），挂起 pendingEdge，不触发。
	r.step("QQ音乐 听我想听", 22730, 2, true, true)
	r.step("QQ音乐 听我想听", 22730, 2, true, true)
	if len(r.events) != 0 {
		t.Fatalf("占位串期间不应触发重开，实得 %+v", r.events)
	}

	// t=100ms：歌名恢复为曲 A，PlayState 仍为 2（无新边沿），由 pendingEdge 当拍兑现 R1。
	recoverTick := r.tick + 1
	r.step(songA, 22730, 2, true, true)
	want := []guardEvent{{tick: recoverTick, byLoading: true, progressMs: 22730}}
	if !slices.Equal(r.events, want) {
		t.Fatalf("歌名恢复拍重开事件 = %+v，want %+v", r.events, want)
	}

	// t=150ms：进度归零开启暂扣；持续 400ms 后放行（PlayState=3 恢复播放）。
	r.step(songA, 0, 2, true, true)
	if !r.lastHeld {
		t.Fatalf("归零拍应开启暂扣")
	}
	released := false
	for range 10 {
		r.step(songA, 0, 3, true, true)
		if r.lastReleased {
			released = true
			break
		}
	}
	if !released {
		t.Fatalf("期望满 400ms 暂扣放行")
	}
	// 放行拍之后继续正常播放走出开头带，全程仅 1 次重开。
	r.step(songA, 500, 3, true, true)
	r.step(songA, 1200, 3, true, true)
	if !slices.Equal(r.events, want) || r.reopen != 1 {
		t.Fatalf("放行后重开事件 = %+v (reopen=%d)，want 严格 1 次 %+v", r.events, r.reopen, want)
	}
	if r.reopenG.consumed {
		t.Fatalf("播至 1.2s 离开开头带后 consumed 应已解除")
	}
}

// 场景 2：跨拍去重反例 A（设计 §5.2 第 2 项第 2 条）：
//   - A1：50ms 轮询，A@22000ms，t=0 R1 触发重开，载入结束后停在 22000ms 达 450ms（含 +116ms 尾巴补写）才归零暂扣放行 → 全程只 1 次；
//   - A2：触发后未归零而是继续播至 22500ms（>= consumedMs + resumeStepMs）→ 解除 consumed，30s 处无边沿回零重开正常触发第 2 次；
//   - A3：触发后未归零而回跳落入 >= 1.0s 的其他位置（seekBack）→ 解除 consumed，后续无边沿回零重开正常触发第 2 次。
func TestReopenGuardCounterexampleA(t *testing.T) {
	t.Run("A1_载入早于归零超过450ms不超时误清", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.claimAndPlay("曲A", 22000)

		r1Tick := r.tick + 1
		r.step("曲A", 22000, 2, true, true) // t=0：R1 触发
		want := []guardEvent{{tick: r1Tick, byLoading: true, progressMs: 22000}}
		if !slices.Equal(r.events, want) {
			t.Fatalf("R1 触发 = %+v，want %+v", r.events, want)
		}

		// 载入结束后在 22000ms 停留 450ms（9 拍 × 50ms，并在中途模拟一次 +116ms 尾巴补写），均低于 resumeStepMs。
		for i := range 9 {
			ms := uint32(22000)
			if i >= 4 {
				ms = 22116
			}
			r.step("曲A", ms, 3, true, true)
			if !r.reopenG.consumed {
				t.Fatalf("停留在旧值第 %d 拍（%dms）不应提前解除 consumed", i+1, ms)
			}
		}

		// t=500ms 归零开启暂扣，经 400ms 放行。
		r.step("曲A", 0, 3, true, true)
		if !r.lastHeld {
			t.Fatalf("归零拍应开启暂扣")
		}
		released := false
		for range 10 {
			r.step("曲A", 0, 3, true, true)
			if r.lastReleased {
				released = true
				break
			}
		}
		if !released {
			t.Fatalf("期望暂扣放行")
		}
		if !slices.Equal(r.events, want) || r.reopen != 1 {
			t.Fatalf("全程重开 = %+v (reopen=%d)，want 仅 1 次 %+v", r.events, r.reopen, want)
		}
	})

	t.Run("A2_未归零继续推进至resumeStepMs解锁后续重开", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.claimAndPlay("曲A", 22000)

		r.step("曲A", 22000, 2, true, true) // 曲中缓冲触发 R1，consumedMs=22000
		if len(r.events) != 1 || !r.reopenG.consumed {
			t.Fatalf("R1 后事件=%+v consumed=%v", r.events, r.reopenG.consumed)
		}
		// 推进 499ms（未满 resumeStepMs=500）仍保持 consumed。
		r.step("曲A", 22499, 3, true, true)
		if !r.reopenG.consumed {
			t.Fatalf("前进 499ms 未满 resumeStepMs，不应解除 consumed")
		}
		// 推进至 22500ms（恰满 consumedMs + resumeStepMs）解除 consumed。
		r.step("曲A", 22500, 3, true, true)
		if r.reopenG.consumed {
			t.Fatalf("前进至 22500ms（>= consumedMs+resumeStepMs）后应解除 consumed")
		}

		// 随后播至 30s，再无边沿回零暂扣放行，R2 正常触发第 2 次重开。
		r.step("曲A", 30000, 3, true, true)
		r.step("曲A", 0, 3, true, true)
		for range 10 {
			r.step("曲A", 0, 3, true, true)
			if r.lastReleased {
				break
			}
		}
		if len(r.events) != 2 || r.events[1].byLoading || r.reopen != 2 {
			t.Fatalf("30s 无边沿回零重开事件 = %+v (reopen=%d)，want 第 2 次由 R2 触发", r.events, r.reopen)
		}
	})

	t.Run("A3_未归零回跳落入1秒外解锁后续重开", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.claimAndPlay("曲A", 22000)

		r.step("曲A", 22000, 2, true, true) // R1 触发，consumedMs=22000
		// 未归零而是回跳至 5000ms（>= 1000ms 且 seekBack(22000, 5000) 为真）。
		r.step("曲A", 5000, 3, true, true)
		if r.reopenG.consumed {
			t.Fatalf("回跳至 5000ms 后应由 seekBack 解除 consumed")
		}
		// 随后无边沿归零暂扣放行，R2 正常触发第 2 次。
		r.step("曲A", 0, 3, true, true)
		for range 10 {
			r.step("曲A", 0, 3, true, true)
			if r.lastReleased {
				break
			}
		}
		if len(r.events) != 2 || r.events[1].byLoading || r.reopen != 2 {
			t.Fatalf("回跳解锁后无边沿重开事件 = %+v (reopen=%d)，want 2 次", r.events, r.reopen)
		}
	})
}

// 场景 3：跨拍去重反例 B（设计 §5.2 第 2 项第 3 条）：
// 暂停在 22.57s → 无边沿归零暂扣 → 400ms 放行拍仍暂停（v.pausedSeek，R2 触发 1 次）→
// 下一拍 v.resume（anchorProgressMs == 0）→ R3 不重复触发（全程 1 次）；
// 随后在 0.5s 再次点击暂停（v.pause）并再次无边沿重开 → 第 2 次重开正常触发。
func TestReopenGuardCounterexampleB(t *testing.T) {
	r := newReopenGuardReplay(50 * time.Millisecond)
	r.claimAndPlay("曲A", 22570)
	r.pauseAt("曲A", 22570)

	// 无载入边沿归零，开启暂扣，期间与放行拍均保持暂停态 raw=4。
	r.step("曲A", 0, 4, true, true)
	if !r.lastHeld {
		t.Fatalf("归零拍应开启暂扣")
	}
	var releaseTick int
	for range 10 {
		r.step("曲A", 0, 4, true, true)
		if r.lastReleased {
			releaseTick = r.tick
			break
		}
	}
	if releaseTick == 0 || !r.lastVerdict.pausedSeek || r.lastVerdict.pause {
		t.Fatalf("放行拍期望 pausedSeek=true、pause=false，实得 releaseTick=%d verdict=%+v", releaseTick, r.lastVerdict)
	}
	wantFirst := []guardEvent{{tick: releaseTick, byLoading: false, progressMs: 0}}
	if !slices.Equal(r.events, wantFirst) {
		t.Fatalf("放行拍重开 = %+v，want %+v", r.events, wantFirst)
	}

	// 下一拍变在播：产出 v.resume，anchorProgressMs == 0，R3 必须被 consumed 拦住。
	r.step("曲A", 0, 3, true, true)
	if !r.lastVerdict.resume {
		t.Fatalf("下一拍期望产出 v.resume")
	}
	if !slices.Equal(r.events, wantFirst) || r.reopen != 1 {
		t.Fatalf("v.resume 拍不应重复触发重开，实得 %+v (reopen=%d)", r.events, r.reopen)
	}

	// 随后播至 0.5s（仍在开头带内）再次点击暂停（v.pause），由 onPause 复位 consumed。
	r.step("曲A", 500, 3, true, true)
	r.step("曲A", 500, 4, true, true)
	if !r.lastVerdict.pause || r.reopenG.consumed {
		t.Fatalf("0.5s 点击暂停应产出 v.pause 并清 consumed，实得 verdict=%+v consumed=%v", r.lastVerdict, r.reopenG.consumed)
	}

	// 再次在 0.5s 无边沿重开恢复：R3 正常触发第 2 次。
	secondTick := r.tick + 1
	r.step("曲A", 500, 3, true, true)
	if !r.lastVerdict.resume {
		t.Fatalf("0.5s 恢复拍应产出 v.resume")
	}
	wantAll := []guardEvent{
		{tick: releaseTick, byLoading: false, progressMs: 0},
		{tick: secondTick, byLoading: false, progressMs: 500},
	}
	if !slices.Equal(r.events, wantAll) || r.reopen != 2 {
		t.Fatalf("0.5s 再次暂停重开后事件 = %+v (reopen=%d)，want %+v", r.events, r.reopen, wantAll)
	}
}

// 场景 4：跨拍去重反例 C（设计 §5.2 第 2 项第 4 条）：
// 暂停 A@22000ms → 拖至开头带（0ms 或 100ms）放行拍触发 R2（pausedSeek）→ 保持暂停前跳至 22000ms（离开开头带，
// stepProgress 不要求 !isPaused，解除 consumed）→ 再从另一入口无边沿重开（20ms 载入漏采，下一读数 A@0ms 在播）→
// 暂扣放行拍 R2 再次正常触发。
func TestReopenGuardCounterexampleC(t *testing.T) {
	for _, tc := range []struct {
		name           string
		startMs        uint32
		wantPausedSeek bool // lastMemProgress == 0 时 runSession 前跳检测跳过，100ms 时产出 v.pausedSeek
	}{
		{"拖至0ms放行后暂停中前跳至22s", 0, false},
		{"拖至100ms放行后暂停中前跳至22s(pausedSeek)", 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReopenGuardReplay(50 * time.Millisecond)
			r.claimAndPlay("曲A", 22000)
			r.pauseAt("曲A", 22000)

			// 暂停中拖至开头带，经 400ms 放行触发 R2（pausedSeek）。
			r.step("曲A", tc.startMs, 4, true, true)
			for range 10 {
				r.step("曲A", tc.startMs, 4, true, true)
				if r.lastReleased {
					break
				}
			}
			if len(r.events) != 1 || !r.lastVerdict.pausedSeek || !r.reopenG.consumed || !r.reopenG.sawStart {
				t.Fatalf("拖至 %dms 放行拍状态异常：events=%+v verdict=%+v guard=%+v", tc.startMs, r.events, r.lastVerdict, r.reopenG)
			}

			// 保持暂停（raw=4）前跳至 22000ms：isPaused 仍为 true，stepProgress 不要求 !isPaused，应解除 consumed。
			r.step("曲A", 22000, 4, true, true)
			if !r.isPaused || r.lastVerdict.pausedSeek != tc.wantPausedSeek {
				t.Fatalf("前跳至 22s 状态不符：isPaused=%v verdict=%+v，want pausedSeek=%v", r.isPaused, r.lastVerdict, tc.wantPausedSeek)
			}
			if r.reopenG.consumed {
				t.Fatalf("暂停中前跳离开开头带至 22s 后 consumed 应已解除，实得 guard=%+v", r.reopenG)
			}

			// 从另一入口重开，20ms 超短载入漏采，下一读数直接为 A@0ms 在播（raw=3）。
			r.step("曲A", 0, 3, true, true)
			if !r.lastHeld {
				t.Fatalf("从 22s 归零至 0ms 应开启暂扣")
			}
			var secondRelease int
			for range 10 {
				r.step("曲A", 0, 3, true, true)
				if r.lastReleased {
					secondRelease = r.tick
					break
				}
			}
			if secondRelease == 0 || len(r.events) != 2 || r.events[1].tick != secondRelease || r.events[1].byLoading || r.reopen != 2 {
				t.Fatalf("第二次无边沿重开事件 = %+v (reopen=%d, release=%d)，want 第 2 次在放行拍由 R2 触发", r.events, r.reopen, secondRelease)
			}
		})
	}
}

// 场景 5：跨拍去重反例 D 的全部 3 种变体（设计 §5.2 第 2 项第 5 条）：
// 已判暂停于 A@22000ms → 观测到 PlayState=2（变体 1：A@22000ms 当拍触发 R1；变体 2：占位串挂起、下一拍 A@22000ms 兑现 R1；
// 变体 3：占位串挂起、下一拍 A@0ms 归零同拍兑现 R1 并开启暂扣）→ 400ms 放行拍 A@0ms 暂停（R2 被拦，v.pausedSeek 不清 consumed）→
// 下一拍 A@0ms 在播（v.resume，R3 被拦）→ 三种变体全程 reopen 严格只递增 1 次。
func TestReopenGuardCounterexampleD(t *testing.T) {
	variants := []struct {
		name string
		run  func(t *testing.T, r *reopenGuardReplay) (r1Tick int)
	}{
		{
			name: "变体1_A@22000ms当拍触发R1",
			run: func(t *testing.T, r *reopenGuardReplay) int {
				r1Tick := r.tick + 1
				r.step("曲A", 22000, 2, true, true) // R1 触发
				r.step("曲A", 0, 4, true, true)     // 下一拍归零开启暂扣
				if !r.lastHeld {
					t.Fatalf("归零拍应开启暂扣")
				}
				return r1Tick
			},
		},
		{
			name: "变体2_占位串挂起下一拍A@22000ms兑现R1",
			run: func(t *testing.T, r *reopenGuardReplay) int {
				r.step("QQ音乐 听我想听", 22000, 2, true, true) // 占位串挂起 pendingEdge
				if len(r.events) != 0 {
					t.Fatalf("占位串拍不应触发")
				}
				r1Tick := r.tick + 1
				r.step("曲A", 22000, 2, true, true) // 兑现 R1
				r.step("曲A", 0, 4, true, true)     // 归零开启暂扣
				if !r.lastHeld {
					t.Fatalf("归零拍应开启暂扣")
				}
				return r1Tick
			},
		},
		{
			name: "变体3_占位串挂起下一拍A@0ms归零同拍兑现R1",
			run: func(t *testing.T, r *reopenGuardReplay) int {
				r.step("QQ音乐 听我想听", 22000, 2, true, true) // 占位串挂起 pendingEdge
				if len(r.events) != 0 {
					t.Fatalf("占位串拍不应触发")
				}
				r1Tick := r.tick + 1
				r.step("曲A", 0, 2, true, true) // 同拍兑现 R1 并开启暂扣
				if !r.lastHeld {
					t.Fatalf("同拍归零应开启暂扣")
				}
				return r1Tick
			},
		},
	}

	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			r := newReopenGuardReplay(50 * time.Millisecond)
			r.claimAndPlay("曲A", 22000)
			r.pauseAt("曲A", 22000)

			r1Tick := tc.run(t, r)
			if len(r.events) != 1 || r.events[0].tick != r1Tick || !r.events[0].byLoading {
				t.Fatalf("R1 触发异常：events=%+v，want tick=%d byLoading=true", r.events, r1Tick)
			}

			// 400ms 暂扣放行拍读数为 A@0ms 暂停：R2 被 consumed 拦，judgeTick 产出 v.pausedSeek，保留 consumed 并置 sawStart=true。
			released := false
			for range 10 {
				r.step("曲A", 0, 4, true, true)
				if r.lastReleased {
					released = true
					break
				}
			}
			if !released || !r.lastVerdict.pausedSeek || r.lastVerdict.pause {
				t.Fatalf("放行拍期望 released=true pausedSeek=true pause=false，实得 released=%v verdict=%+v", released, r.lastVerdict)
			}
			if !r.reopenG.consumed || !r.reopenG.sawStart {
				t.Fatalf("放行拍后应保留 consumed 并置 sawStart=true，实得 guard=%+v", r.reopenG)
			}

			// 下一拍读数为 A@0ms 在播：产出 v.resume，R3 被 consumed 拦。
			r.step("曲A", 0, 3, true, true)
			if !r.lastVerdict.resume {
				t.Fatalf("恢复拍期望产出 v.resume")
			}
			if len(r.events) != 1 || r.reopen != 1 {
				t.Fatalf("全程应严格只递增 1 次 reopen，实得 events=%+v reopen=%d", r.events, r.reopen)
			}
		})
	}
}

// 场景 6：20ms 超短载入漏采回归（设计 §5.2 第 2 项第 6 条）：
// 已认领曲 A，loadUsable == true，无载入边沿、22.73s → 0.00s 暂扣 400ms 后放行 → R2 仍触发 1 次 reopenSong(false)。
func TestReopenGuardShortLoadingMissed(t *testing.T) {
	r := newReopenGuardReplay(50 * time.Millisecond)
	if !r.spec.loadingUsable(false, r.poll) {
		t.Fatalf("夹具前置失败：22.82/50ms 应满足 loadUsable == true")
	}
	r.claimAndPlay("曲A", 22730)

	// 无载入边沿，直接从 22730ms 回到 0ms（raw=3）。
	r.step("曲A", 0, 3, true, true)
	if !r.lastHeld {
		t.Fatalf("22.73s → 0.00s 应开启暂扣")
	}
	var releaseTick int
	for range 10 {
		r.step("曲A", 0, 3, true, true)
		if r.lastReleased {
			releaseTick = r.tick
			break
		}
	}
	want := []guardEvent{{tick: releaseTick, byLoading: false, progressMs: 0}}
	if releaseTick == 0 || !slices.Equal(r.events, want) || r.reopen != 1 {
		t.Fatalf("loadUsable=true 时漏采超短载入放行事件 = %+v (reopen=%d)，want %+v", r.events, r.reopen, want)
	}
}

// 场景 7：开头 < 1.0s 暂停重开漏采载入 + 暂停 22.57s 原地恢复不触发 + 切往新歌 B 占位串不误触旧歌 A +
// < 1.0s 内连续第二次新 edge 解锁重开 + !hasIDs 消耗 pendingEdge 不遗留（设计 §5.2 第 2 项第 7/8 条）。
func TestReopenGuardStartResumeAndSongSwitch(t *testing.T) {
	t.Run("暂停在0.00s无边沿恢复触发R3", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.step("曲A", 0, 3, true, true)
		r.pauseAt("曲A", 0)
		resumeTick := r.tick + 1
		r.step("曲A", 0, 3, true, true)
		want := []guardEvent{{tick: resumeTick, byLoading: false, progressMs: 0}}
		if !r.lastVerdict.resume || !slices.Equal(r.events, want) {
			t.Fatalf("0.00s 恢复重开 = %+v (resume=%v)，want %+v", r.events, r.lastVerdict.resume, want)
		}
	})

	t.Run("暂停在开头带边界999ms触发1000ms不触发", func(t *testing.T) {
		for _, tc := range []struct {
			ms   uint32
			want int
		}{{999, 1}, {1000, 0}} {
			r := newReopenGuardReplay(50 * time.Millisecond)
			r.claimAndPlay("曲A", tc.ms)
			r.pauseAt("曲A", tc.ms)
			r.step("曲A", tc.ms, 3, true, true)
			if len(r.events) != tc.want {
				t.Fatalf("暂停在 %dms 原地恢复触发 %d 次，want %d", tc.ms, len(r.events), tc.want)
			}
		}
	})

	t.Run("暂停在22.57s原地恢复不触发重开", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.claimAndPlay("曲A", 22570)
		r.pauseAt("曲A", 22570)
		r.step("曲A", 22570, 3, true, true)
		if !r.lastVerdict.resume || len(r.events) != 0 {
			t.Fatalf("22.57s 原地恢复不应触发重开，实得 resume=%v events=%+v", r.lastVerdict.resume, r.events)
		}
	})

	t.Run("切往新歌B期间占位串载入边沿不误触旧歌A", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.claimAndPlay("曲A", 22570)
		// 占位串载入边沿
		r.step("QQ音乐 听我想听", 22570, 2, true, true)
		if !r.reopenG.pendingEdge || len(r.events) != 0 {
			t.Fatalf("占位串拍应挂起且不触发，实得 guard=%+v events=%+v", r.reopenG, r.events)
		}
		// 切到新歌 B 且进度已归零 → 认领新歌 B，reset() 清零 pendingEdge。
		r.step("曲B", 0, 2, true, true)
		r.step("曲B", 50, 3, true, true)
		if r.lastName != "曲B" || r.gen != 2 || r.reopenG.pendingEdge || len(r.events) != 0 {
			t.Fatalf("切到新歌 B 后状态异常：lastName=%q gen=%d guard=%+v events=%+v", r.lastName, r.gen, r.reopenG, r.events)
		}
	})

	t.Run("1秒内连续第二次新物理边沿解锁重开", func(t *testing.T) {
		for _, withPlaceholder := range []bool{false, true} {
			name := "直接同名边沿"
			if withPlaceholder {
				name = "经占位串挂起边沿"
			}
			t.Run(name, func(t *testing.T) {
				r := newReopenGuardReplay(50 * time.Millisecond)
				r.claimAndPlay("曲A", 200)
				// 第一次 R1 重开（进度仍在 < 1.0s）
				r.step("曲A", 200, 2, true, true)
				r.step("曲A", 250, 3, true, true) // 离开载入态，但进度仍 < 1.0s，consumed=true
				if len(r.events) != 1 || !r.reopenG.consumed {
					t.Fatalf("第一次重开后状态异常：events=%+v guard=%+v", r.events, r.reopenG)
				}
				// 紧接着第二次双击产生新的 non-loading → loading 物理边沿
				if withPlaceholder {
					r.step("QQ音乐 听我想听", 250, 2, true, true)
					r.step("曲A", 0, 2, true, true)
				} else {
					r.step("曲A", 0, 2, true, true)
				}
				if len(r.events) != 2 || !r.events[1].byLoading || r.reopen != 2 {
					t.Fatalf("<1s 内连续第二次新 edge 应解锁重开，实得 events=%+v reopen=%d", r.events, r.reopen)
				}
			})
		}
	})

	t.Run("编号未知时挂起边沿在恢复同名拍清零不滞后误触", func(t *testing.T) {
		var g reopenGuard
		if g.shouldReopenByLoading(true, true, "QQ音乐 听我想听", "曲A", false) {
			t.Fatalf("占位串拍不应触发")
		}
		if !g.pendingEdge {
			t.Fatalf("占位串拍应置 pendingEdge")
		}
		if g.shouldReopenByLoading(true, false, "曲A", "曲A", false) {
			t.Fatalf("!hasIDs 时不应触发")
		}
		if g.pendingEdge {
			t.Fatalf("!hasIDs 时歌名恢复为 lastName 应清掉 pendingEdge，避免滞后误触发")
		}
		if g.shouldReopenByLoading(true, false, "曲A", "曲A", true) {
			t.Fatalf("后续拿到编号但无新边沿时不应滞后误触发")
		}
	})

	t.Run("stepProgress在held或loading或pendingEdge或pendingFallback为真时不解除consumed", func(t *testing.T) {
		for _, tc := range []struct {
			name            string
			held            bool
			loading         bool
			pendingEdge     bool
			pendingFallback bool
		}{
			{"held为真", true, false, false, false},
			{"loading为真", false, true, false, false},
			{"pendingEdge为真", false, false, true, false},
			{"pendingFallback为真", false, false, false, true},
		} {
			g := reopenGuard{consumed: true, sawStart: true, consumedMs: 0, pendingEdge: tc.pendingEdge, pendingFallback: tc.pendingFallback}
			g.stepProgress(2000, tc.held, tc.loading)
			if !g.consumed {
				t.Errorf("%s：不应提前解除 consumed", tc.name)
			}
		}
	})
}

// 端到端联调（带真实 assocReader + lyricState + synctest）：16:22 真实序列下，暂停在 22.57s 后从另一入口重开同一首，
// 内存记录从 id 键（assocLines2）切换为 mid 键（assocLines），占位串拍挂起边沿在歌名恢复拍触发 1 次重开并完成重扫换词，
// 随后 400ms 暂扣放行拍不重复触发第 2 次扫描。
func TestReopenGuardRealSequenceWithAssocReader(t *testing.T) {
	e := reopenEnv(idURL(100))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		const poll = 50 * time.Millisecond
		var (
			spec             = knownVersions["22.82"].PlayState
			loadOpen         = newLoadEdge(poll)
			reopenG          reopenGuard
			hold             switchHold
			judge            judgeState
			key              songKey
			st               lyricState
			lastName         string
			lastMemProgress  uint32
			anchorProgressMs uint32
			anchorTime       time.Time
			isPaused         bool
			shown            []string
		)
		reopenSong := func(progressMs uint32) {
			reopenG.noteReopen(progressMs)
			key.reopen++
			st.reopened(key.reopen)
			r.Submit(key)
		}
		tick := func(name string, progressMs uint32, raw uint32) {
			time.Sleep(poll)
			sampleTime := time.Now()
			loadUsable := spec.loadingUsable(judge.bad, poll)
			reopenedThisTick := false
			if edge := loadOpen.step(spec, raw, true, sampleTime); reopenG.shouldReopenByLoading(loadUsable, edge, name, lastName, key.hasIDs()) && !reopenedThisTick {
				reopenedThisTick = true
				reopenSong(progressMs)
			} else if !reopenedThisTick && reopenG.shouldReopenByFallback(false, name, lastName, key.hasIDs()) {
				reopenedThisTick = true
				reopenSong(progressMs)
			}
			if name != lastName && name != "" && !strings.HasPrefix(name, "QQ音乐") {
				hold.drop(sampleTime)
				lastName = name
				anchorProgressMs = progressMs
				anchorTime = sampleTime
				lastMemProgress = progressMs
				isPaused = false
				judge.resume.reset()
				reopenG.reset()
				key = songKey{gen: key.gen + 1, songID: 100, songMid: "003M", name: name}
				st.reset(key.gen, false, time.Time{})
				r.Submit(key)
				st.setOnlineRaw(shiftFixture())
				lines, _ := st.refresh(key.hasIDs(), stateOffsetSec)
				if l, ch := takeResult(&st, r.Results(), key.hasIDs(), stateOffsetSec); ch {
					lines = l
				}
				shown = lineTexts(lines)
			}
			held, released := hold.step(lastMemProgress, progressMs, sampleTime)
			if held {
				return
			}
			var changed, seek bool
			var step int64
			if progressMs != lastMemProgress {
				seekDetected := released && progressMs < lastMemProgress
				if lastMemProgress > 0 && seekBack(lastMemProgress, progressMs) {
					seekDetected = true
				}
				if seekDetected {
					if reopenG.shouldReopenByFallback(released && progressMs < lastMemProgress, name, lastName, key.hasIDs()) && !reopenedThisTick {
						reopenedThisTick = true
						reopenSong(progressMs)
					}
				}
				changed, seek, step = true, seekDetected, int64(progressMs)-int64(lastMemProgress)
				anchorProgressMs = progressMs
				anchorTime = sampleTime
				lastMemProgress = progressMs
			}
			var v verdict
			judge, v = judgeTick(judge, judgeIn{
				spec: spec, raw: raw, rawOK: true, poll: poll,
				paused: isPaused, claimed: lastName != "", changed: changed, seek: seek, step: step,
				idle: sampleTime.Sub(anchorTime), now: sampleTime,
			})
			if v.resume {
				isPaused = false
				anchorTime = sampleTime
				if reopenG.shouldReopenByFallback(anchorProgressMs < switchStartBandMs, name, lastName, key.hasIDs()) && !reopenedThisTick {
					reopenedThisTick = true
					reopenSong(anchorProgressMs)
				}
			}
			if v.pause {
				isPaused = true
				reopenG.onPause()
			}
			reopenG.stepProgress(progressMs, held, loadOpen.loading)
			if lastName != "" {
				if l, ch := takeResult(&st, r.Results(), key.hasIDs(), stateOffsetSec); ch {
					shown = lineTexts(l)
				}
			}
		}

		// 认领「百」，初始记录为 id 键（assocLines2），随后暂停在 22.57s。
		tick("百", 0, 3)
		synctest.Wait()
		tick("百", 22570, 3)
		if !slices.Equal(shown, assocLines2) {
			t.Fatalf("重开前显示 %q，want id 键关联 %q", shown, assocLines2)
		}
		tick("百", 22570, 4) // 暂停
		if !isPaused {
			t.Fatalf("期望已判暂停")
		}
		scansBefore := len(e.mem.scanTimes())

		// 另一入口双击重开：QQ 改写记录为 mid 键，先切占位串并进载入值 2，100ms 后歌名恢复「百」且仍为 2。
		reopenRecord(e, midURL("003M"))
		tick("QQ音乐 听我想听", 22730, 2)
		tick("QQ音乐 听我想听", 22730, 2)
		tick("百", 22730, 2) // 歌名恢复拍触发唯一一次重开
		synctest.Wait()

		// 归零开启暂扣，经 400ms 放行并继续播放。
		tick("百", 0, 2)
		for range 12 {
			tick("百", 0, 3)
		}
		synctest.Wait()
		tick("百", 500, 3)

		if key.reopen != 1 {
			t.Fatalf("key.reopen = %d，want 1（放行拍不重复触发）", key.reopen)
		}
		if !slices.Equal(shown, assocLines) {
			t.Fatalf("重开后显示 %q，want mid 键关联 %q", shown, assocLines)
		}
		if got := len(e.mem.scanTimes()) - scansBefore; got != 1 {
			t.Fatalf("重开后新增扫描 %d 次，want 恰好 1 次（扫描时刻 %v）", got, e.mem.scanTimes())
		}
	})
}

// 占位串期间 R2/R3 兜底挂起与兑现回归（#245 Codex 审查 P2）：
//
//	a) 曲 A 在 0ms 暂停切往新歌 B，占位串期间先载入值 2 再在播值 3 后落到曲 B → 旧歌 A 重开 0 次；
//	b) 曲 A 在 0ms 暂停，占位串期间 20ms 短载入漏采直接读到在播值 3（R3 挂起）→ 恢复曲 A 后恰好触发 1 次 reopenSong(false, 0)；
//	c) 曲 A 在 0ms 暂停，占位串期间先载入值 2（R1 挂起）再在播值 3（R3 挂起）→ 恢复曲 A 后拍首 R1 兑现 1 次（byLoading=true），不重复；
//	d) 暂停于曲 A@22s，占位串期间归零暂扣满 400ms 放行（R2 挂起）：随后变新歌 B → 旧歌 A 重开 0 次；随后恢复曲 A → 恰好 1 次 reopenSong(false, 0)；
//	e) 占位串期间 R3 挂起后再次进入暂停（v.pause），再切回曲 A（仍暂停）→ onPause 清掉旧 pendingFallback 不误触。
func TestReopenGuardPlaceholderFallback(t *testing.T) {
	t.Run("a_Codex原反例_0ms暂停切新歌B占位串先2后3不误触旧歌A", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.step("曲A", 0, 3, true, true)
		r.pauseAt("曲A", 0)
		r.step("QQ音乐 听我想听", 0, 2, true, true)
		r.step("QQ音乐 听我想听", 0, 3, true, true)
		if len(r.events) != 0 {
			t.Fatalf("占位串期间不应重开旧歌 A，实得 %+v", r.events)
		}
		r.step("曲B", 0, 3, true, true)
		if r.lastName != "曲B" || r.gen != 2 || len(r.events) != 0 || r.reopen != 0 {
			t.Fatalf("切到新歌 B 后应 0 次误重开旧歌 A，实得 lastName=%q gen=%d reopen=%d events=%+v",
				r.lastName, r.gen, r.reopen, r.events)
		}
	})

	t.Run("b_占位串期间R3挂起后恢复同一首A恰好兑现1次", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.step("曲A", 0, 3, true, true)
		r.pauseAt("曲A", 0)
		// 20ms 超短载入漏采，占位串拍直接读到在播值 3 → R3 挂起 pendingFallback，当拍不误触。
		r.step("QQ音乐 听我想听", 0, 3, true, true)
		if !r.lastVerdict.resume || len(r.events) != 0 {
			t.Fatalf("占位串恢复拍应产出 v.resume 且暂不重开，实得 resume=%v events=%+v", r.lastVerdict.resume, r.events)
		}
		// 下一拍歌名恢复曲 A → 拍首 else if 兑现 1 次 reopenSong(false, 0)。
		recoverTick := r.tick + 1
		r.step("曲A", 0, 3, true, true)
		want := []guardEvent{{tick: recoverTick, byLoading: false, progressMs: 0}}
		if !slices.Equal(r.events, want) || r.reopen != 1 {
			t.Fatalf("恢复曲 A 后应恰好兑现 1 次 R3，实得 events=%+v reopen=%d，want %+v", r.events, r.reopen, want)
		}
	})

	t.Run("c_占位串同时有R1边沿与R3恢复后回到曲A优先兑现R1仅1次", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.step("曲A", 0, 3, true, true)
		r.pauseAt("曲A", 0)
		r.step("QQ音乐 听我想听", 0, 2, true, true)
		r.step("QQ音乐 听我想听", 0, 3, true, true)
		if len(r.events) != 0 {
			t.Fatalf("占位串期间不应提前重开，实得 %+v", r.events)
		}
		recoverTick := r.tick + 1
		r.step("曲A", 0, 3, true, true)
		want := []guardEvent{{tick: recoverTick, byLoading: true, progressMs: 0}}
		if !slices.Equal(r.events, want) || r.reopen != 1 {
			t.Fatalf("恢复曲 A 拍应优先兑现 R1 仅 1 次，实得 events=%+v reopen=%d，want %+v", r.events, r.reopen, want)
		}
	})

	t.Run("d1_占位串期间R2放行拍随后变新歌B不误触旧歌A", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.claimAndPlay("曲A", 22000)
		r.pauseAt("曲A", 22000)
		// 占位串期间归零暂扣至放行（raw=4，无载入边沿）。
		r.step("QQ音乐 听我想听", 0, 4, true, true)
		released := false
		for range 10 {
			r.step("QQ音乐 听我想听", 0, 4, true, true)
			if r.lastReleased {
				released = true
				break
			}
		}
		if !released || len(r.events) != 0 {
			t.Fatalf("占位串放行拍不应重开旧歌 A，实得 released=%v events=%+v", released, r.events)
		}
		r.step("曲B", 0, 3, true, true)
		if r.lastName != "曲B" || r.gen != 2 || len(r.events) != 0 || r.reopen != 0 {
			t.Fatalf("变新歌 B 后旧歌 A 应 0 次重开，实得 lastName=%q gen=%d reopen=%d events=%+v",
				r.lastName, r.gen, r.reopen, r.events)
		}
	})

	t.Run("d2_占位串期间R2放行拍随后恢复曲A恰好兑现1次", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.claimAndPlay("曲A", 22000)
		r.pauseAt("曲A", 22000)
		// 占位串期间归零暂扣至放行（raw=4，无载入边沿），R2 挂起 pendingFallback。
		r.step("QQ音乐 听我想听", 0, 4, true, true)
		released := false
		for range 10 {
			r.step("QQ音乐 听我想听", 0, 4, true, true)
			if r.lastReleased {
				released = true
				break
			}
		}
		if !released || len(r.events) != 0 {
			t.Fatalf("占位串放行拍不应提前重开，实得 released=%v events=%+v", released, r.events)
		}
		// 下一拍恢复曲 A 且变在播（raw=3）：拍首 else if 兑现 1 次，同拍 v.resume 的 R3 被 consumed 与 reopenedThisTick 拦住。
		recoverTick := r.tick + 1
		r.step("曲A", 0, 3, true, true)
		want := []guardEvent{{tick: recoverTick, byLoading: false, progressMs: 0}}
		if !slices.Equal(r.events, want) || r.reopen != 1 {
			t.Fatalf("恢复曲 A 拍应恰好兑现 1 次 R2，实得 events=%+v reopen=%d，want %+v", r.events, r.reopen, want)
		}
	})

	t.Run("d3_占位串期间R2放行拍随后恢复曲A仍暂停由拍首else_if兑现", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.claimAndPlay("曲A", 22000)
		r.pauseAt("曲A", 22000)
		r.step("QQ音乐 听我想听", 0, 4, true, true)
		released := false
		for range 10 {
			r.step("QQ音乐 听我想听", 0, 4, true, true)
			if r.lastReleased {
				released = true
				break
			}
		}
		if !released || len(r.events) != 0 {
			t.Fatalf("占位串放行拍不应提前重开，实得 released=%v events=%+v", released, r.events)
		}
		// 下一拍恢复曲 A 但仍暂停（raw=4，不产出 v.resume），仅靠拍首 else if 兑现 R2。
		recoverTick := r.tick + 1
		r.step("曲A", 0, 4, true, true)
		want := []guardEvent{{tick: recoverTick, byLoading: false, progressMs: 0}}
		if r.lastVerdict.resume || !slices.Equal(r.events, want) || r.reopen != 1 {
			t.Fatalf("仍暂停恢复曲 A 拍应由拍首 else if 兑现 1 次 R2，实得 resume=%v events=%+v reopen=%d",
				r.lastVerdict.resume, r.events, r.reopen)
		}
		// 再下一拍起播（v.resume）：R3 被 consumed 拦截，全程仍严格 1 次。
		r.step("曲A", 0, 3, true, true)
		if !r.lastVerdict.resume || !slices.Equal(r.events, want) || r.reopen != 1 {
			t.Fatalf("随后起播拍 R3 不应重复触发，实得 resume=%v events=%+v reopen=%d",
				r.lastVerdict.resume, r.events, r.reopen)
		}
	})

	t.Run("e_占位串R3挂起后再次进入暂停再切回曲A不误触", func(t *testing.T) {
		r := newReopenGuardReplay(50 * time.Millisecond)
		r.step("曲A", 0, 3, true, true)
		r.pauseAt("曲A", 0)
		// 占位串在播 → R3 挂起 pendingFallback。
		r.step("QQ音乐 听我想听", 0, 3, true, true)
		if !r.lastVerdict.resume || len(r.events) != 0 {
			t.Fatalf("占位串恢复拍应挂起且不触发，实得 resume=%v events=%+v", r.lastVerdict.resume, r.events)
		}
		// 再次进入暂停（v.pause）→ onPause 清掉旧 pendingFallback。
		r.step("QQ音乐 听我想听", 0, 4, true, true)
		if !r.lastVerdict.pause {
			t.Fatalf("期望产出 v.pause")
		}
		// 切回曲 A（仍暂停，无新边沿无恢复）→ 不应兑现暂停前的旧 pendingFallback。
		r.step("曲A", 0, 4, true, true)
		if len(r.events) != 0 || r.reopen != 0 {
			t.Fatalf("onPause 清掉旧 pendingFallback 后切回曲 A 不应误触，实得 events=%+v reopen=%d", r.events, r.reopen)
		}
	})
}
