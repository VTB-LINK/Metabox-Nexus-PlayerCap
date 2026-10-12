package spotify

import (
	"math"
	"time"
)

// clock 是「锚点采样 + 本地墙钟外推」的播放位置时钟，六家里的第四份同形实现
// （cloudmusic 的 baseRealClock、kugou 的落锚、applemusic 的 clock 是另外三份）。
//
// 为什么需要它：Spotify 页面上的位置是 **~1Hz 采样**（实测 2026-10：两次更新间隔 962~1026ms），
// 两次之间那个值一直是旧的。轮询频率再高也只是重复读同一个数，**位置精度靠本地外推**——
// 在采样变化的那一刻落锚，两次采样之间按墙钟推进。这照 sodamusic 的边沿落锚（§7.7.1）：
// 每轮都落锚会把 time.Since(anchorAt) 清零，外推形同虚设，位置与歌词行匹配一起退化成 1s 阶梯。
//
// 与外推配套的是「翻转窗口」（照 applemusic 的 clock.timeAt）：读数不跳时最多允许推进
// 1s（一个采样周期）再加一点抖动容忍，避免页面卡住时凭空往前冲、翻过好几行歌词。
type clock struct {
	// anchorPos 是锚点位置（秒）：最近一次采样（或状态变化）那一刻的真实位置。
	anchorPos float64
	anchorAt  time.Time
	// lastRaw 是最近一次见到的原始采样（秒），-1 = 本会话还没见过任何采样。
	lastRaw float64
	// hasAnchor 报告锚点是否已建立（还没有任何采样时 live 返回 0）。
	hasAnchor bool
	// playing 是本地判定的播放态：暂停时不外推。
	playing bool
	// duration 是曲目时长（秒），0 = 未知；只用于把外推值钳在时长内。
	duration float64
}

// clockEvent 是一次 observe 得出的状态迁移。seek 也复用 evResume（与播放器对外的事件一致：
// AGENTS §5.1 规定 seek 复用 playback_resume）。
type clockEvent int

const (
	evNone clockEvent = iota
	evResume
	evPause
	evSeek
)

const (
	// flipLagMax 是「读数没跳时允许外推的超额」：1s 采样周期之外的容忍量。
	// 取 0.35s 是因为实测采样间隔抖动在 ±40ms 内，另留余量给页面繁忙。
	flipLagMax = 0.35
	// samplePeriodSec 是数据源的名义采样周期。
	samplePeriodSec = 1.0
	// seekForwardSec / seekBackwardSec 是判定 seek 的阈值（秒）。向前阈值更大：正常的采样抖动
	// 与前跳不会差到 1.5s；向后阈值更小以支持行内小回跳（与 cloudmusic 的 1.5/1.0 同量级）。
	seekForwardSec  = 1.5
	seekBackwardSec = 1.0
)

// reset 清空时钟（换歌时调用）。之后第一次 observe 会重新落锚。
func (c *clock) reset() {
	c.anchorPos = 0
	c.anchorAt = time.Time{}
	c.lastRaw = -1
	c.hasAnchor = false
	c.playing = false
	c.duration = 0
}

// liveAt 返回 now 时刻的整曲位置（秒）：锚点 + 自锚点起的墙钟流逝（播放中），钳在时长与翻转窗口内。
func (c *clock) liveAt(now time.Time) float64 {
	pos := c.anchorPos
	if c.playing && !c.anchorAt.IsZero() {
		if elapsed := now.Sub(c.anchorAt).Seconds(); elapsed > 0 {
			pos += elapsed
		}
	}
	// 翻转窗口：页面的读数不跳，就不许凭空前冲（见类型注释）。
	//
	// 基准取 lastRaw 与 anchorPos 的较大者：暂停时锚点冻结在 lastRaw 之后（读数停更、墙钟还在走），
	// 恢复播放那一刻窗口必须以**冻结值**为基准，否则恢复后的前 1 秒会被旧的 lastRaw 卡住、
	// 位置少走最多一个采样周期。两者相等时不改变行为。
	base := c.lastRaw
	if c.anchorPos > base {
		base = c.anchorPos
	}
	if limit := base + samplePeriodSec + flipLagMax; pos > limit {
		pos = limit
	}
	if c.duration > 0 && pos > c.duration {
		pos = c.duration
	}
	if !(pos >= 0) { // NaN 的接受式判定：拒绝式会让 NaN 穿过去
		return 0
	}
	return pos
}

// observe 喂一次采样（rawSec < 0 表示本轮没有有效采样），返回本拍的状态迁移。
//
// 判定顺序（与 cloudmusic / sodamusic 一致）：**seek 优先于暂停恢复**，一拍只返回一个事件。
// seek 只在旧态为播放时判，故不会与恢复同拍（不会有两条 playback_resume，AGENTS §5.1）；
// 与暂停同拍时只返回 evSeek，而播放态已记成暂停、下一拍不会再报 —— 调用方须按本拍的 playing
// 补发暂停（见 clockEmits）。
func (c *clock) observe(rawSec float64, playing bool, durationSec float64, now time.Time) clockEvent {
	if durationSec > 0 {
		c.duration = durationSec
	}
	stateChanged := playing != c.playing

	haveRaw := rawSec >= 0 && rawSec <= math.MaxFloat64
	rawChanged := haveRaw && rawSec != c.lastRaw

	if !c.hasAnchor {
		// 首次（本会话或换歌之后）：直接锚在这一次采样上。换歌那一刻页面的位置与标题是同一次
		// 渲染出来的，属于当前曲目（对照 sodamusic 的 firstSong 判据），没有"上一首的残值"可防。
		//
		// **不返回迁移事件**：建立锚点是「记录初始状态」，不是状态变化。会话刚连上时对外只有
		// 换歌分支的 status 与 all_lyrics（与 sodamusic 换歌分支不发 playback_resume 同形）。
		if haveRaw {
			c.anchorPos = rawSec
			c.lastRaw = rawSec
		} else {
			c.anchorPos = 0
		}
		c.anchorAt = now
		c.hasAnchor = true
		c.playing = playing
		return evNone
	}

	if haveRaw && rawChanged {
		// **先算 drift，再更新 lastRaw**：liveAt 的翻转窗口以 lastRaw 为基准，先把新采样写进去会让
		// 窗口跟着新值放宽。读数停更不足一个窗口时两种顺序结果相同；停更超过窗口（页面卡住）之后，
		// drift 就不再对照已对外报出的、被钳住的位置，而是对照不受钳制的墙钟外推 —— 停更期间的前跳、
		// 以及停更结束时读数一下子追上来，都会被当成「没有偏差」吸收掉，不发 playback_resume。
		drift := rawSec - c.liveAt(now)
		c.lastRaw = rawSec
		if c.playing && (drift > seekForwardSec || drift < -seekBackwardSec) {
			c.anchorPos = rawSec
			c.anchorAt = now
			c.playing = playing
			return evSeek
		}
		c.anchorPos = rawSec
		c.anchorAt = now
	} else if stateChanged {
		// 状态变了但没有新采样：把锚点定在此刻的外推值上（暂停即冻结在这个位置）。
		c.anchorPos = c.liveAt(now)
		c.anchorAt = now
	}

	c.playing = playing
	if stateChanged {
		if playing {
			return evResume
		}
		return evPause
	}
	return evNone
}
