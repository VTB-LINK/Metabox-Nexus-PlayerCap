package wesing

import (
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
)

// pauseDuration 读数停住多久判为暂停。
const pauseDuration = 1 * time.Second

// tickJudge 是 pollLyrics 每拍的位置判定（#177，设计见 doc/design/wesing-forward-seek-design.md §4.10）：回跳与
// 前跳、兜底模式的最小化外推、暂停与恢复。从 pollLyrics 原样抽出、加上前跳，时刻改由调用方传入，为的是按拍
// 驱动、离线测「发一条」「不重发」；读数、窗口检查、altWatch、CKSongsGraph、歌词行与位置采样仍在 pollLyrics。
type tickJudge struct {
	last   float32 // 上一拍报出的位置（外推之后），−1 为还没有；回跳与暂停判定比的是它
	paused bool

	frozen      bool
	frozenSince time.Time

	wasMinimized bool
	minimizedAt  time.Time
	atMinimize   float32

	base lyric.SeekBase // 前跳的基准（设计 §4.1）
}

func newTickJudge() tickJudge { return tickJudge{last: -1} }

// tickOut 是一拍的判定结果。emitTick 按回跳或前跳、暂停、恢复的次序发出，三者同一拍至多一种。
type tickOut struct {
	pos  float32 // 本拍报出的位置：直读值，兜底模式最小化外推时为外推值
	from float32 // 上一拍报出的位置（日志用）

	back, fwd bool // 判为回跳 / 前跳：发 playback_resume
	resetLine bool // 复位行号：跳转那拍重发当前行，同一行内的跳转也重发（lyric_update-SSE 只收 lyric_update）
	unpause   bool // 跳转那拍把已判的暂停置为播放，先发 status_update(playing)（前跳只在未判暂停时判，只有回跳会）
	pause     bool // 判为暂停：status_update(paused) 与 playback_pause
	resume    bool // 从暂停恢复：status_update(playing) 与 playback_resume，不复位行号
}

// step 在每拍读数成功之后调一次：raw 是本拍外推之前的直读值，now 是读数成功之后取的时刻，minimized、moving
// 是本拍的最小化与拖窗标志（PCM 模式恒为假），dur 是总时长（外推的上限）。
func (j *tickJudge) step(raw float32, now time.Time, minimized, moving bool, dur float32) tickOut {
	o := tickOut{pos: raw, from: j.last}

	// 跳转检测排在暂停检测之前，避免恢复与跳转同拍两条 playback_resume。回跳在暂停中也判；前跳用本拍判定
	// 之前的暂停状态，暂停中不判，由下面的恢复路径发一条（设计 §4.6、§4.7）。
	o.back = seekBack(j.last, raw, minimized)
	o.fwd = j.base.Forward(raw, now, j.paused, minimized)
	if o.back || o.fwd {
		o.resetLine = true
		if j.paused {
			j.paused = false
			o.unpause = true
		}
		j.frozen = false
	}

	if minimized {
		if j.paused {
			j.wasMinimized = true
			j.frozen = false
		} else {
			if !j.wasMinimized {
				j.minimizedAt = now
				j.atMinimize = raw
				j.wasMinimized = true
			}
			o.pos = j.atMinimize + float32(now.Sub(j.minimizedAt).Seconds())
			if dur > 0 && o.pos > dur {
				o.pos = dur
			}
			j.frozen = false
		}
	} else {
		j.wasMinimized = false
		if moving {
			j.frozen = false
		} else if j.last >= 0 && raw == j.last {
			if !j.frozen {
				j.frozenSince = now
				j.frozen = true
			}
			if now.Sub(j.frozenSince) >= pauseDuration && !j.paused {
				j.paused = true
				o.pause = true
			}
		} else {
			j.frozen = false
			if j.paused {
				j.paused = false
				o.resume = true
			}
		}
	}

	j.last = o.pos
	j.base.Settle(o.pos, now)
	return o
}

// emitTick 按 step 的结果发事件。跳转与恢复的位置都是本拍直读值 raw（最小化外推期间不判跳转、不判暂停）。
func (p *WesingPlayer) emitTick(o tickOut, raw float32, title string, dur float32) {
	if o.back || o.fwd {
		if o.back {
			log.Info("Seek-back detected: %.2fs → %.2fs", o.from, raw)
		} else {
			log.Info("forward seek detected: %.2fs → %.2fs", o.from, raw)
		}
		if o.unpause {
			p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: title})
		}
		p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: raw, Progress: player.ClampProgress(raw, dur)})
	}
	if o.pause {
		log.Info("paused @ %.2fs", raw)
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "paused", Detail: title})
		p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: raw, Progress: player.ClampProgress(raw, dur)})
	}
	if o.resume {
		log.Info("resumed @ %.2fs", raw)
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: title})
		p.Emit(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: raw, Progress: player.ClampProgress(raw, dur)})
	}
}
