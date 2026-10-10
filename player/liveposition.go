package player

import "time"

// LivePosition 返回 now 时刻的整曲播放位置（秒）：播放中为锚点位置加上自锚点时刻起经过的时间，
// 暂停中即锚点位置；durationSec > 0 时不超过它。
//
// 锚点是一次进度读数与读到它的时刻。轮询循环在读数之后做了耗时的事（kugou、qqmusic 换歌时联网取词）
// 再发事件时，用它把位置推到发出的那一刻：服务端以事件到达的时刻为采样时刻（server 包的
// PlayerState.livePosition），事件带着早先读到的位置出去，缓存的位置就落后这段耗时。
//
// anchorTime 为零值（尚未落锚）或 now 不晚于 anchorTime 时不推进，返回锚点位置（仍受时长约束）。
func LivePosition(anchorSec float32, anchorTime, now time.Time, playing bool, durationSec float32) float32 {
	pos := anchorSec
	if playing && !anchorTime.IsZero() {
		if elapsed := now.Sub(anchorTime); elapsed > 0 {
			pos += float32(elapsed.Seconds())
		}
	}
	if durationSec > 0 && pos > durationSec {
		pos = durationSec
	}
	return pos
}
