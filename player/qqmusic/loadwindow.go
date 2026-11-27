package qqmusic

// 本文件是会话一侧的改名证据、载入段记录与同一首重开守卫（#218 第 5 版，doc/design/qqmusic-lyric-association-design.md
// §6.5 第 5 步、§6.8；#245，doc/design/qqmusic-assoc-followup-design.md §3.3）：loadEdge（qqmusic.go）的 step 记下窗口与
// 载入段，换歌认领时 claim 求证据、excludeFor 定出交给读取者的 excludeLocal；reopenGuard 统一协调同一首重开的 R1/R2/R3
// 触发与跨拍去重。只由轮询线程调用，只做内存比较，没有 IO、没有等待。

import (
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

// renameEvidence 是一次换歌认领求得的「同一次播放被 QQ 改名」的证据。
type renameEvidence struct {
	ok     bool
	reason string        // 不成立的原因，英文原文，注入日志时经 i18n.T；成立时为空
	maxGap time.Duration // 窗口里相邻两次读成的读数的最长间隔
}

// loadSegment 是一段载入值的四个时刻与标记（设计 §6.8「载入段的记录」）。
type loadSegment struct {
	pre, first, last, post time.Time // t₀（零值即没有）、t₁、t₂、t₃
	fail, gap              bool      // t₀～t₁ 或 t₂～t₃ 之间有读失败、有采样缺口
	failInside, gapInside  bool      // t₁～t₂ 之间有读失败、有采样缺口
}

// newLoadEdge 按会话的轮询间隔建出 loadEdge，载入段结束时打一条 Detail。
func newLoadEdge(poll time.Duration) loadEdge {
	return loadEdge{poll: poll, onSegment: logLoadSegment}
}

// missed 补记一拍读不成：每拍开头整拍元数据读失败而 continue 的那一拍不调用 step，这一拍没有看播放状态。不改 lastAt。
func (e *loadEdge) missed() {
	e.sawFail, e.failSince = true, true
}

// claim 在换歌分支认领时、写入当前歌曲之前调用，每次认领恰一次（设计 §6.5 第 5 步、§6.8「改名证据」）：求自上一次认领以来
// 的窗口是否构成改名证据，随后清零窗口。usable 是本拍的 loadUsable，spec 是本版本的播放状态表项（门槛 G 只出自它），poll 是会话
// 的轮询间隔。按下列次序取第一个成立的原因，都不成立即成立：载入信号不可用；本版本的 G 为 0；轮询间隔不小于 G；见到载入边沿；
// 有读不成的拍；有采样缺口。lastAt、loading 与载入段的状态不清：认领那一拍的读数到认领后第一次读数的间隔计入下一次认领。
func (e *loadEdge) claim(usable bool, spec playStateSpec, poll time.Duration) renameEvidence {
	ev := renameEvidence{maxGap: e.maxGap}
	switch {
	case !usable:
		ev.reason = "loading signal unusable"
	case spec.LoadingMinGap <= 0:
		ev.reason = "no loading sample threshold for this QQ Music version"
	case poll >= spec.LoadingMinGap:
		ev.reason = "poll interval not shorter than the loading sample threshold"
	case e.sawEdge:
		ev.reason = "loading state seen"
	case e.sawFail:
		ev.reason = "unreadable play state"
	case e.sawGap:
		ev.reason = "sampling gap"
	default:
		ev.ok = true
	}
	e.sawEdge, e.sawFail, e.sawGap, e.maxGap = false, false, false, 0
	return ev
}

// excludeFor 返回换歌认领交给读取者的 excludeLocal：证据成立即为空（上一首的文件照常作候选），否则为上一首采纳过的本地文件。
func excludeFor(prevLocal string, ev renameEvidence) string {
	if ev.ok {
		return ""
	}
	return prevLocal
}

// logRenameEvidence 在上一首有采纳的本地文件时打一条 Detail（设计 §6.9）：证据成立与否、不成立的原因与窗口里的最长读数间隔。
func logRenameEvidence(prevLocal string, ev renameEvidence) {
	switch {
	case prevLocal == "":
	case ev.ok:
		log.Detail("Song changed; keeping the previous local file as a candidate (same playback renamed by QQ Music); longest gap between play-state reads %v", ev.maxGap)
	default:
		log.Detail("Song changed; excluding the previous local file: %s; longest gap between play-state reads %v", i18n.T(ev.reason), ev.maxGap)
	}
}

// lower 是持续时长的保守下界 t₂ − t₁（只有一个载入样本时为 0）；t₁～t₂ 之间有读失败或采样缺口时那里可能藏着一段非载入，
// 不可用。
func (s loadSegment) lower() (time.Duration, bool) {
	if s.failInside || s.gapInside {
		return 0, false
	}
	return s.last.Sub(s.first), true
}

// upper 是持续时长的保守上界 t₃ − t₀；没有 t₀（会话第一次读数即为载入值）时不可用。
func (s loadSegment) upper() (time.Duration, bool) {
	if s.pre.IsZero() {
		return 0, false
	}
	return s.post.Sub(s.pre), true
}

// logLoadSegment 在一段载入值结束时打一条 Detail（设计 §6.9），供复核门槛 G：四个时刻、持续时长的保守范围与读失败、采样缺口的标记。
func logLoadSegment(s loadSegment) {
	at := func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("15:04:05.000")
	}
	bound := func(d time.Duration, ok bool) string {
		if !ok {
			return i18n.T("unavailable")
		}
		return d.Round(time.Millisecond).String()
	}
	var marks string
	for _, m := range []struct {
		on   bool
		text string
	}{
		{s.failInside, i18n.T("; unreadable play state between the loading samples")},
		{s.gapInside, i18n.T("; sampling gap between the loading samples")},
		{s.fail, i18n.T("; unreadable play state next to the segment")},
		{s.gap, i18n.T("; sampling gap next to the segment")},
	} {
		if m.on {
			marks += m.text
		}
	}
	log.Detail("QQ Music loading segment: non-loading %s, loading %s to %s, non-loading %s; at least %s, at most %s%s",
		at(s.pre), at(s.first), at(s.last), at(s.post), bound(s.lower()), bound(s.upper()), marks)
}

// reopenGuard 是同一首重开的统一跨拍守卫（#245，doc/design/qqmusic-assoc-followup-design.md §3.3）：
// 协调载入边沿及占位串挂起（R1）、暂扣放行回零（R2）与暂停在开头恢复（R3）三条触发规则，以确定性的进度与暂停状态
// 变化而非时间超时划定跨拍去重边界。只由轮询线程调用，零值立即可用。
type reopenGuard struct {
	pendingEdge     bool   // 已认领歌曲期间见到了载入边沿，等待歌名恢复为 lastName 兑现
	pendingFallback bool   // 已认领歌曲期间在非 lastName 拍见到了无边沿兜底信号（R2/R3），等待歌名恢复为 lastName 兑现
	consumed        bool   // 当前这一次重开操作已触发过 reopenSong（R1/R2/R3 统一登记）
	sawStart        bool   // 自 consumed 置真以来已观测到进度落入开头带 (< switchStartBandMs)
	consumedMs      uint32 // consumed 置真时的进度读数（用于区分「重开前停在旧值等归零」与「曲中缓冲后继续前进」）
}

// noteReopen 仅在 reopenSong 闭包内统一调用一次：登记本次重开已消费，并清掉尚未兑现的旧挂起边沿与兜底意图，
// 防止 R1/R2/R3 任一触发后因滞后歌名恢复再次触发重开。
func (g *reopenGuard) noteReopen(progressMs uint32) {
	g.consumed = true
	g.sawStart = progressMs < switchStartBandMs
	g.consumedMs = progressMs
	g.pendingEdge = false
	g.pendingFallback = false
}

// reset 在换歌认领时调用：清零上一首残留的挂起边沿、挂起兜底意图与跨拍去重状态。
func (g *reopenGuard) reset() {
	*g = reopenGuard{}
}

// onPause 仅在暂停与恢复判定处 if v.pause 为真时调用（绝不在 v.pausedSeek 或 hold.step 开启暂扣时调用）：
// 新进入已判暂停即清零跨拍消费状态与暂停前的旧兜底挂起（pendingFallback），保留 pendingEdge 不动
// （暂停前刚见到的占位串载入边沿仍待歌名恢复兑现）。
func (g *reopenGuard) onPause() {
	g.consumed = false
	g.sawStart = false
	g.consumedMs = 0
	g.pendingFallback = false
}

// stepProgress 每拍在暂停与恢复判定之后调用（不要求 !isPaused，播放走出或暂停中跳转离开开头带均成立）：
// 观测进度进入开头带，并在确定性离开开头带或自触发点继续前进/非归零回跳时解除 consumed。
func (g *reopenGuard) stepProgress(progressMs uint32, held, loading bool) {
	if !g.consumed {
		return
	}
	if progressMs < switchStartBandMs {
		g.sawStart = true
		return
	}
	if held || loading || g.pendingEdge || g.pendingFallback {
		return
	}
	if g.sawStart ||
		uint64(progressMs) >= uint64(g.consumedMs)+resumeStepMs ||
		seekBack(g.consumedMs, progressMs) {
		g.consumed = false
		g.sawStart = false
		g.consumedMs = 0
	}
}

// shouldReopenByLoading 按规则 R1 判定是否由载入边沿触发同一首重开（设计 §3.3）：
// 观测到新物理载入边沿即挂起 pendingEdge 并解锁 consumed（即使仍在 < 1.0s 内连续第二次双击重开）；
// 若当拍为占位串（空串或 "QQ音乐..."）或其他非空真歌名则保持挂起，待歌名恢复为 lastName 时兑现（若认领新歌则由 reset 清零）；
// 歌名恢复为 lastName 时消耗 pendingEdge，仅在 !consumed && hasIDs 时返回真。
func (g *reopenGuard) shouldReopenByLoading(usable, edge bool, name, lastName string, hasIDs bool) bool {
	if !usable || lastName == "" {
		g.pendingEdge = false
		return false
	}
	if edge {
		g.pendingEdge = true
		g.consumed = false
		g.sawStart = false
		g.consumedMs = 0
	}
	if g.pendingEdge && name == lastName {
		g.pendingEdge = false
		return !g.consumed && hasIDs
	}
	return false
}

// shouldReopenByFallback 按规则 R2/R3 判定是否由无边沿兜底信号触发同一首重开（设计 §3.3，#245）：
// 当拍歌名等于 lastName 且未消费时立即兑现；若当拍歌名为占位串或其他非 lastName（例如切往新歌 B 的过渡占位串拍），
// 则仅挂起 pendingFallback 不立即触发，待后续歌名恢复为 lastName 时由拍首兑现，若最终认领新歌则由 reset() 丢弃。
func (g *reopenGuard) shouldReopenByFallback(triggered bool, name, lastName string, hasIDs bool) bool {
	if lastName == "" {
		g.pendingFallback = false
		return false
	}
	if triggered && !g.consumed {
		g.pendingFallback = true
	}
	if g.pendingFallback && name == lastName {
		g.pendingFallback = false
		return !g.consumed && hasIDs
	}
	return false
}
