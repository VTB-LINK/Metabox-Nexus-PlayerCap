// Package manager 是主服务侧的音频捕获管理器：进程发现 + 订阅门控 + 在播预热、经 Job Object
// 监管 helper 子进程、退避重启、帧活性看门狗、stdout 解复用（见 docs/audio-waveform-capture-design.md
// §3.1/§5/§6）。
//
// 本文件（gating.go，无 build tag）只放**无平台依赖的纯决策**：AudioSink 接口、门控函数
// desiredCaptures、退避与看门狗常量、看门狗判定 judgeWatchdog 与逐播放器失败记账、空闲帧判定
// idlePlayers，故 CI 可测且能 GOOS=linux 编译；捕获编排/Job Object/退避执行/看门狗与空闲帧接线在
// manager_windows.go + jobobject_windows.go（//go:build windows）。gating.go 不 import config/logger
// （保 §0.3 GOOS=linux 绿）。
package manager

import "time"

const (
	// 退避：helper spawn 失败或退出后指数退避重启，封顶 backoffCap。spawn 失败、或退出时存活不足
	// quickFailWindow（看门狗所杀与按需拆除除外），记一次快速失败，累计 maxQuickFails 次即放弃本会话（不再无限 spawn 烧 CPU，
	// 波形降级，§6）。看门狗所杀的 helper 不计快速失败、不抬退避。helper 稳定运行满 quickFailWindow
	// 后（reapHelper 与 teardownHelper 均适用）快速失败计数与退避清零。maxQuickFails 为 5 时 backoffCap
	// 实际不可达，只作上限保护。单路捕获失败逐播放器记账，沿用同一组常量（recordCaptureFailure /
	// captureBackoff）。
	backoffBase     = 1 * time.Second
	backoffCap      = 30 * time.Second
	quickFailWindow = 10 * time.Second
	maxQuickFails   = 5

	// hangThreshold 是帧活性看门狗阈值：某已下发的 player 持续无帧即判该路捕获失败或 helper 假死。
	// helper 与该路捕获正常时，不论有无音频、不论播放状态，至少约每 100ms 出一帧（有声为真帧，无声为
	// 保活零帧；wasapi.Read 单次至多阻塞 100ms），故 2s ≈ 20 个静音节拍，不误判，又足够快地抓住失效（§6）。
	hangThreshold = 2 * time.Second

	// noCaptureAfter 是「某播放器当前无捕获」的判定阈值：距最近一次转发它的真帧超过此时长，idleKeepalive
	// 即推 player 为其名的空闲帧。捕获正常时 helper 最长约 100ms 出一帧（见 hangThreshold），取 3 倍余量，
	// 正常捕获中不会插入空闲帧。
	noCaptureAfter = 300 * time.Millisecond
)

// frameSnap 是看门狗判定的单行快照，在锁内从 lastFrame 取出、锁外交 judgeWatchdog 判定。
type frameSnap struct {
	player       string
	last         time.Time // lastFrame；下发 capture 时置为下发时刻（基线），此后每收一帧刷新
	has          bool      // lastFrame 是否存在
	dispatchedAt time.Time // 本次下发 capture 的时刻；无记录为零值
}

// judgeWatchdog 是看门狗的纯判定，与播放状态无关：
//
//	stale：has 且距 last 超过 hangThreshold 的 player，按 rows 顺序；
//	alive：存在某行 has、last 晚于 dispatchedAt（本次下发后收到过真帧，不是基线）且距 last 未超
//	       hangThreshold，即 helper 仍在出帧。dispatchedAt 为零值的行不算存活证据；
//	killHelper：stale 非空且无 alive。stale 为空时恒为 false。
//
// killHelper 为假而 stale 非空，说明 helper 存活、只是这几路捕获失败，由调用方逐路停止重试。
func judgeWatchdog(rows []frameSnap, now time.Time) (stale []string, killHelper bool) {
	alive := false
	for _, r := range rows {
		if !r.has {
			continue
		}
		if now.Sub(r.last) > hangThreshold {
			stale = append(stale, r.player)
		} else if !r.dispatchedAt.IsZero() && r.last.After(r.dispatchedAt) {
			alive = true
		}
	}
	return stale, len(stale) > 0 && !alive
}

// captureFailure 是单个播放器的捕获失败记账（仅 Run goroutine 访问）。
type captureFailure struct {
	count   int       // 连续失败次数
	retryAt time.Time // 此刻之前不重新下发 capture
	givenUp bool      // 已放弃，直到该播放器进程重启（PID 变化）
	pid     uint32    // 失败时该播放器的 PID
}

// recordCaptureFailure 在 f 上记一次失败并返回新记账：at 为失败时刻，dispatchedAt 为本次下发时刻
// （零值视为不足窗口），pid 为失败时该播放器的 PID。本次捕获跑满 quickFailWindow 后才失败，次数
// 重置为 1；否则加 1。次数达到 maxQuickFails 即放弃；未放弃时 retryAt = at + captureBackoff(次数)。
func recordCaptureFailure(f captureFailure, at, dispatchedAt time.Time, pid uint32) captureFailure {
	if !dispatchedAt.IsZero() && at.Sub(dispatchedAt) >= quickFailWindow {
		f.count = 1
	} else {
		f.count++
	}
	f.pid = pid
	if f.count >= maxQuickFails {
		f.givenUp = true
		return f
	}
	f.retryAt = at.Add(captureBackoff(f.count))
	return f
}

// captureBackoff 返回第 n 次连续失败后的重试退避：backoffBase << (n-1)，封顶 backoffCap。
// 逐次翻倍而非直接移位，n 很大时也不溢出。
func captureBackoff(n int) time.Duration {
	d := backoffBase
	for ; n > 1 && d < backoffCap; n-- {
		d *= 2
	}
	return min(d, backoffCap)
}

// captureStable 判定本次捕获是否已跑稳：最后收帧距本次下发已达 quickFailWindow。为真时调用方清除
// 该播放器的失败记账。dispatchedAt 为零值时恒为假。
func captureStable(dispatchedAt, last time.Time) bool {
	return !dispatchedAt.IsZero() && last.Sub(dispatchedAt) >= quickFailWindow
}

// filterWant 就地过滤 want（player→pid）：剔除已放弃、或 now 早于 retryAt 的 player；若 want 中的
// PID 与记账 PID 不同（播放器已重启），删除其记账且保留该 player。
func filterWant(want map[string]uint32, fails map[string]captureFailure, now time.Time) {
	for player, pid := range want {
		f, ok := fails[player]
		if !ok {
			continue
		}
		if f.pid != pid {
			delete(fails, player)
			continue
		}
		if f.givenUp || now.Before(f.retryAt) {
			delete(want, player)
		}
	}
}

// NormParams 是某播放器本轮的音量归一化下发参数：manager 据 config 解析后构造 capture 控制命令用。
// 无平台依赖，置于 gating.go 以保 GOOS=linux 可构建（§0.3）。
type NormParams struct {
	Enabled bool
	Target  int // 目标响度 dBFS
	MaxGain int // 最大增益 dB
}

// AudioSink 是 manager 依赖的 hub 能力子集（server 的 *audioHub 满足它）。用接口解耦，
// manager 不 import server，也便于 gating 纯 Go 测试注入桩。
type AudioSink interface {
	// BroadcastAudioFrame 把一帧自包含字节按 player 键扇出（drop-oldest 非阻塞在 hub 内保证）。
	BroadcastAudioFrame(player string, payload []byte)
	// HasSubscribers 报告 name 这家是否需要被捕获：存在 name 的 per-player 订阅，
	// 或（存在 root 订阅且 name == 当前 active）。root-only（OBS 主用法）时对 active 恒真。
	// 同时是 player=name 空闲帧的推送门控（idlePlayers）。
	HasSubscribers(name string) bool
	// HasRootSubscriber 是否存在 root 订阅（决定是否预热「在播集合」）。
	HasRootSubscriber() bool
	// AnySubscribers 是否有任一订阅（给「扫描本身」门控，无人看则不扫不抓）。
	AnySubscribers() bool
	// ActivePlayer 当前 activePlayer 缓存（空串=无活跃）。供空闲空帧门控：root 有订阅且此为空时推 player="" 空帧。
	ActivePlayer() string
}

// desiredCaptures 计算「本轮该捕获哪些播放器」：
//
//	被 hasSub 命中的（per-player 订阅，或 root 订阅下的 active）—— active 恒经此入选；
//	外加 root 订阅时的整个在播集合（预热：切歌选中谁都已热、root 无缝接管，§3.1）。
//
// playing 是「状态为 playing」的集合，hasSub 即 sink.HasSubscribers。无订阅（hasSub 全假、
// 无 root）→ 空集，上游据此拆 helper（按需节能）。
func desiredCaptures(names []string, hasRoot bool, playing map[string]bool, hasSub func(string) bool) map[string]bool {
	d := map[string]bool{}
	for _, n := range names {
		if hasSub(n) || (hasRoot && playing[n]) {
			d[n] = true
		}
	}
	return d
}

// idlePlayers 返回本拍需要推 player=<名> 空闲帧的播放器，按 names 顺序：hasSub 为真，且 lastForward 中
// 无记录（从未转发过真帧），或 now 距该记录严格大于 after（恰好等于 after 不推）。lastForward 只记
// demux 转发真帧的时刻，不含下发 capture 时置的看门狗基线。hasSub 即 sink.HasSubscribers。
func idlePlayers(names []string, hasSub func(string) bool, lastForward map[string]time.Time, now time.Time, after time.Duration) []string {
	var idle []string
	for _, n := range names {
		if last, ok := lastForward[n]; ok && now.Sub(last) <= after {
			continue
		}
		if hasSub(n) {
			idle = append(idle, n)
		}
	}
	return idle
}
