// Package pos 从酷我进程内存里读播放位置。
//
// 2026-10-22: 默认 Open / OpenCore 已改为 KwService 中 PlayerCore 的高频毫秒位置，
// 见 core_windows.go。新调用方使用 CoreClock，不再用下面的四秒 Tracker。
// 以下保留的是旧 OpenSnapshot 的发现记录，仅用于对照与诊断。
//
// 为什么不用 CDP：酷我的网页桥**不给时间**。实测（2026-10，两个会话）在能连上的页面上：
//   - `GetPlayingProgress`（酷我自己的单曲页就用它取位置）恒返回空串，即使 `GetPlayStatus`
//     是 `Status=1`（播放中）；
//   - `GetPlayStatus` 只有 `Status` 与 `SongId`，`GetNowplayingSong` 只有元数据，都没有毫秒；
//   - 网页 DOM 里没有时间轴（时间轴在原生播放条上），`document.body.innerText` 抓不到 `mm:ss`。
//
// 位置在客户端的「当前歌曲信息」结构里，布局（实测 9.6.0.0，x86）：
//
//	+0   char rid[16]   "MUSIC_<digits>"，NUL 结尾（≤15 字节时走 std::string 的 SSO，内联在这里）
//	+16  int32         15（质量/码率相关，实测 15）
//	+20  int32         15
//	+24  int32         时长（毫秒）
//	+28  int32         位置（毫秒）  ← 我们要的
//	+32  int32         1（播放标志，未验证语义）
//
// **位置是低频快照**：实测每 4.000 秒刷新一次，每次恰好 +4000ms（rate 严格 1:1）。所以读到的
// 值最多滞后 4 秒，必须用 Tracker 做「锚点 + 墙钟插值」，不能直接当实时位置发出去。
//
// 结构地址每次启动都变（堆），所以每首歌都要重新定位：先用 rid 字符串锚，长度超过 SSO 上限
// （16 字节）时退回用时长锚。
package pos

import (
	"errors"
	"fmt"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// 结构内偏移（相对 rid 字符串起点）。
const (
	offQualityHi = 16 // int32
	offQualityLo = 20 // int32
	offDuration  = 24 // int32 毫秒
	offPosition  = 28 // int32 毫秒
)

// 快照节奏：实测 4.000 秒一次，每次 +4000ms。
const (
	// SnapshotInterval 是客户端刷新位置快照的周期（实测）。
	SnapshotInterval = 4 * time.Second
	// stallSlack 是「多久没等到下一次刷新就认为不再推进」的余量。
	stallSlack = 500 * time.Millisecond
	// seekTolerance 是判定「跳变（seek/循环）」的容差：新读数与「锚点 + 经过时间」差得
	// 超过它就认为不是一次正常刷新。实测正常刷新的误差在 20ms 以内，故 1.5s 很宽。
	seekTolerance = 1500 * time.Millisecond
)

// ErrNotFound 表示在进程内存里没找到当前歌曲的结构。
var ErrNotFound = errors.New("kuwo pos: song info structure not found")

// Song 是一次定位的结果。
type Song struct {
	// RID 是定位用的 rid（形如 MUSIC_204419549）。
	RID string
	// DurationMs 是结构里的时长（毫秒）。它可能小于平台时长（例如只缓存了一部分时实测是 29000）。
	DurationMs int32
	// PositionAddr 是位置字段的地址。
	PositionAddr uintptr
}

// Validate 校验候选地址处的字节是否满足结构布局。
// buf 必须是 addr 起至少 offPosition+4 字节。
func Validate(buf []byte, rid string) (durationMs int32, ok bool) {
	if len(buf) < offPosition+4 {
		return 0, false
	}
	want := rid
	if len(want) == 0 || len(want) > 16 {
		return 0, false
	}
	// rid 必须逐字节匹配，且紧跟 NUL（SSO 的内联串）
	for i := 0; i < len(want); i++ {
		if buf[i] != want[i] {
			return 0, false
		}
	}
	if buf[len(want)] != 0 {
		return 0, false
	}
	dur := int32(getU32(buf[offDuration:]))
	position := int32(getU32(buf[offPosition:]))
	q1 := int32(getU32(buf[offQualityHi:]))
	q2 := int32(getU32(buf[offQualityLo:]))
	if dur < 3_000 || dur > 3_600_000 {
		return 0, false
	}
	if position < 0 || position > dur {
		return 0, false
	}
	// 这两个槽实测都是 15（质量/码率档）；放宽到 0..100 以免别的档位被误杀。
	if q1 < 0 || q1 > 100 || q2 < 0 || q2 > 100 {
		return 0, false
	}
	return dur, true
}

// Tracker 把旧 OpenSnapshot 的 4 秒一次快照变成连续位置。
// Deprecated: 新接入使用 OpenCore 和 CoreClock，后者直接使用真实读数且暂停不外推。
//
// 用法：每 poll 周期调一次 Observe(读到的新位置)；需要位置时调 Estimate。
// 快照在 SnapshotInterval+stallSlack 内没再更新时视为不再推进（暂停/缓冲），Estimate 冻结在锚点。
type Tracker struct {
	anchorMs   int32
	anchorAt   time.Time
	lastMs     int32
	lastChange time.Time
	have       bool
	// jumps 记录被检测到的跳变次数（seek/换歌），供日志与门禁用。
	jumps int
}

// Observe 记录一次读数。
//
// changed 表示读数与上次不同（新快照、seek 或换歌）。只有读数**变化**时才重新落锚，
// 这样两次刷新之间的重复读数不会把锚点不断后移。
//
// seek 区分「跳变」与「正常刷新」。判据刻意宽松且**只抓明显的**，因为第一次读到的快照
// 可能已经滞后了将近一个周期（相位未知，实测有 +4000 只隔 1.2 秒就到的情况）：
//   - 新读数比旧读数小 → 一定不是正常推进（单曲循环回 0、向后拖拽）；
//   - 新读数比旧读数大出「经过时间 + 一个快照周期 + 容差」→ 向前拖拽。
//
// 介于两者之间的一律当正常刷新。宁可漏报（只影响 overlay 的动画重置），不可把每 4 秒的
// 正常刷新都误报成 seek；重锚点本身总是发生的，位置不会因此错。
func (t *Tracker) Observe(posMs int32, now time.Time) (changed, seek bool) {
	if !t.have {
		t.have = true
		t.anchorMs = posMs
		t.anchorAt = now
		t.lastMs = posMs
		t.lastChange = now
		return false, false
	}
	if posMs == t.lastMs {
		return false, false
	}
	elapsed := now.Sub(t.anchorAt)
	step := int64(posMs) - int64(t.lastMs)
	limit := int64(elapsed/time.Millisecond) + int64(SnapshotInterval/time.Millisecond) + int64(seekTolerance/time.Millisecond)
	seek = posMs < t.lastMs || step > limit
	t.anchorMs = posMs
	t.anchorAt = now
	t.lastMs = posMs
	t.lastChange = now
	if seek {
		t.jumps++
	}
	return true, seek
}

// Estimate 返回 now 时刻的位置（秒）。快照停更超过 SnapshotInterval+stallSlack 即冻结在锚点。
func (t *Tracker) Estimate(now time.Time, durationMs int32) float32 {
	if !t.have {
		return 0
	}
	playing := !t.Stalled(now)
	var durSec float32
	if durationMs > 0 {
		durSec = float32(durationMs) / 1000
	}
	return player.LivePosition(float32(t.anchorMs)/1000, t.anchorAt, now, playing, durSec)
}

// Stalled 报告快照是否已经停更（暂停、缓冲或换歌中）。
func (t *Tracker) Stalled(now time.Time) bool {
	if !t.have {
		return true
	}
	return now.Sub(t.lastChange) > SnapshotInterval+stallSlack
}

// Jumps 返回检测到的跳变（seek/循环/换歌）次数。
func (t *Tracker) Jumps() int { return t.jumps }

// Reset 清空状态（换歌时调）。
func (t *Tracker) Reset() { *t = Tracker{} }

func getU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// RIDWithPrefix 把纯数字 id 规范成 MUSIC_ 前缀形式。
func RIDWithPrefix(rid string) string {
	if len(rid) >= 6 && rid[:6] == "MUSIC_" {
		return rid
	}
	return "MUSIC_" + rid
}

// String 便于日志。
func (s Song) String() string {
	return fmt.Sprintf("kuwo pos: %s dur=%dms @0x%X", s.RID, s.DurationMs, s.PositionAddr)
}
