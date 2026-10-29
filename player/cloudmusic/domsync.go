package cloudmusic

// 按 DOM 跳秒对表（issue #77）的判定与未告知量。设计与真机数据见
// doc/design/cloudmusic-dom-clock-sync-design.md。两件都不依赖 CDP、不取当前时刻，时刻一律由调用方给出。

import (
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

const (
	// domLagSec 是目标落后量 L：对表后时钟比界面进度（DOM 秒数跳变的时刻）晚这么多秒。它不是物理量，
	// DOM 跳秒与出声的先后没有量过；它只表示「保持默认 cloudmusicv3-offset 下的现有观感」：2026-10-09
	// 真机 13 首常态换歌，跳秒那一拍「DOM 秒数 − 时钟」较大一档的均值为 302ms（设计 §1.4、§3.2）。
	// 观感整体偏早或偏晚时只改这一个常量。
	domLagSec = 0.30

	// syncNotifySec 是通知门槛（秒）：校准改动了时钟而前端尚不知道的量达到它即发 playback_resume，
	// 单次校准量达到它时打日志。实测常态换歌的微调单首累计不超过 99ms，留一倍余量（设计 §5.2）。
	syncNotifySec = 0.2
)

// domSample 一拍的观测。
type domSample struct {
	sent, got time.Time // Extract 发出之前、返回之后
	sec       int       // DomTimeSec
	playing   bool      // 时钟在走
	sameSong  bool      // 不是换歌拍，且距换歌已超过 500ms
	seeked    bool      // 本拍判了 seek
}

// usable 报告这一拍能不能用来认跳秒（设计 §2.2）：读得到进度；时钟在走（暂停后 DOM 进度还会走约
// 0.5s，那时时钟已冻结，不能跟）；不在换歌前后（DOM 进度可能还是上一首的，换歌拍的读数也与
// sent / got 不配对）；本拍没有判 seek（时钟刚被置成整秒值）。
func (s domSample) usable() bool {
	return s.sec >= 0 && s.playing && s.sameSong && !s.seeked
}

// domSync 记上一拍的观测，零值可用（零值的 domSample 不可用）。
type domSync struct {
	prev domSample
}

// observe 返回 cur.got 时刻时钟应在的位置；不是可信的跳秒时原样返回 pos。每拍都要调用：不可用的拍
// 把上一拍顶掉，一次跳秒因此要相邻两拍都可用。
//
// 可信的跳秒：本拍与上一拍都可用，且本拍秒数 D 恰为上一拍加 1（不变、回退、一次加 2 以上的都不是，
// 后两种归 seek 检测）。跳秒发生在上一拍的 sent 之后、本拍的 got 之前，所以 got 时刻界面进度
// p ∈ [D, D+u]，u = min(w, 1+r)：w = got − 上一拍的 sent，r = 本拍的往返耗时。第二项的 r 不能省：
// 「p 不到 D+1」只在页面读 DOM 的那一刻成立，Extract 返回得晚时 got 时刻的 p 可以超过 D+1。
//
// 把时钟夹进 [D−L, D−L+u]。这个区间恒含 p−L，夹到最近的边界只让落后量朝 L 移动且不越过，轮询相位的
// 变化不会把它推离 L；每跳落锚或夹到区间中点都没有这个性质，相位交替时会来回校（设计 §3.3）。
func (s *domSync) observe(cur domSample, pos float32) float32 {
	prev := s.prev
	s.prev = cur
	if !prev.usable() || !cur.usable() || cur.sec != prev.sec+1 {
		return pos
	}
	w := cur.got.Sub(prev.sent)
	if w <= 0 {
		return pos
	}
	u := min(w, time.Second+cur.got.Sub(cur.sent))
	lo := float32(cur.sec) - domLagSec
	return player.ClampFloat32(pos, lo, lo+float32(u.Seconds()))
}

// untold 记校准改动了时钟而前端尚不知道的量（秒，向前为正、回拨为负），零值可用。
//
// 前端只在带位置的对外事件上落锚、其间按墙钟推进，position_sample 不广播。校准之后若迟迟没有
// lyric_update（长间奏、无歌词的歌），前端收不到新位置，所以累计到门槛时另发一条 playback_resume
// （设计 §5）。按累计量判而不按单次校准量判：结论就是「前端与后端的位置相差不到门槛」。
type untold struct {
	sec        float32
	lastNotice time.Time // 上一次由校准触发的通知
}

// add 记一次校准的改动量。
func (u *untold) add(delta float32) { u.sec += delta }

// anchored 在带位置的对外事件**成功入队**后调用：前端据它落锚到当时的时钟读数，此前的改动都已包含在内。
// 事件被丢弃时不调，未告知量保留（#133）。
func (u *untold) anchored() { u.sec = 0 }

// due 是纯判断（#133）：报告 now 时刻累计量是否达门槛、且距上一次通知已满限频间隔。**不副作用清零**
// ——现状若在此清零，即便 emitPos 丢弃了那条校准 resume，累计量也已丢失、永不补发。清零与限频时刻的
// 更新移到 noticed，只在由校准触发的那条 resume 成功入队后调。两次通知之间至少隔
// player.PositionSampleInterval；间隔未到时顺延而不丢弃：量保留，间隔一到、仍达门槛就发。
func (u *untold) due(now time.Time) bool {
	return (u.sec >= syncNotifySec || u.sec <= -syncNotifySec) &&
		now.Sub(u.lastNotice) >= player.PositionSampleInterval
}

// noticed 在由校准触发的 playback_resume 成功入队后调用：清零未告知量并记下通知时刻（限频）。
func (u *untold) noticed(now time.Time) { u.sec, u.lastNotice = 0, now }

// notifyCalibration 发由对表校准触发的 playback_resume：累计量达门槛且限频已过（due）时，经 emit 发一条，
// **只在成功入队时** noticed（清零 + 更新限频时刻）——emit 内部成功时也 anchored 清零未告知量，被丢弃时
// 两者都不动，due 下一拍仍为真、排空后补发（#133）。提取成函数是为了让「成功才清零」这条判定离线可测
// （runSession 需真 CDP 连接、没有离线夹具）。参数 emitPos 即 runSession 的 emitPos（返回入队结果、内部
// 成功时 anchored）；参数名保持 emitPos，使发出点仍是 `emitPos(EventXxx, …)`、经 AST 门禁。
func notifyCalibration(now time.Time, pending *untold, isPlaying bool, pos, dur float32, emitPos func(string, interface{}) bool) {
	if isPlaying && pending.due(now) {
		if emitPos(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: pos, Progress: player.ClampProgress(pos, dur)}) {
			pending.noticed(now)
		}
	}
}
