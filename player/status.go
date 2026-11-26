package player

import "time"

// 非歌名状态的合成（#231，设计 doc/design/converge-player-behavior-231-design.md §3.2）。
//
// 状态表示播放器所处的层次，reason 表示原因，二者的含义对八家一致：
//
//	waiting_process  进程没在运行          process_not_running / process_exited
//	standby          进程在运行而读不了它  cdp_disconnected / version_unsupported / no_admin / install_not_found / metadata_unavailable
//	waiting_song     已连上而没有当前歌曲  no_song / window_not_open
//
// 进程是否在运行先于一切判定：读不了的原因（版本、商店版、提权、安装）在进程不在运行时只写进 detail，
// 状态仍是 waiting_process。播放器子包里这几种状态只经本文件的 ProcessStatus 与 NoSongStatus 合成，
// 且一律经 EmitStatusIfChanged 发出（✅ player/waitingstatus_lint_test.go）。

// ProcessDetails 是一家播放器在「没在读歌」时各种情形的 detail（已经过 i18n.T 的文本）。
type ProcessDetails struct {
	NotRunning   string // waiting_process / process_not_running
	Exited       string // waiting_process / process_exited
	Disconnected string // standby / cdp_disconnected（Blocked 没有原因时用）
}

// Blocked 是进程在运行而读不了的原因。Reason 为空表示没有受阻，按调试通道未连上处理。
type Blocked struct {
	Reason           string // standby 的 reason：ReasonVersionUnsupported / ReasonNoAdmin / ReasonInstallNotFound / ReasonMetadataUnavailable / ReasonCDPDisconnected
	Detail           string // 进程在运行时的 detail
	NotRunningDetail string // 进程不在运行时写进 waiting_process 的 detail；空则用 ProcessDetails 的 NotRunning 或 Exited
}

// ProcessStatus 按「进程是否在运行先于一切」合成非歌名状态（设计 §3.2）。
//
// seen 表示本次服务期间曾见到该播放器的进程在运行，由各家外层循环维护：每见到一次在运行即置真，不复位。
// 进程不在运行时 b 只贡献 NotRunningDetail，不会得出 standby；b.Reason 为空（含零值）时，进程在运行
// 报 standby / cdp_disconnected，保证返回值的 Status 与 Reason 都非空。
func ProcessStatus(running, seen bool, d ProcessDetails, b Blocked) *StatusInfo {
	if !running {
		reason, detail := ReasonProcessNotRunning, d.NotRunning
		if seen {
			reason, detail = ReasonProcessExited, d.Exited
		}
		if b.NotRunningDetail != "" {
			detail = b.NotRunningDetail
		}
		return &StatusInfo{Status: "waiting_process", Detail: detail, Reason: reason}
	}
	if b.Reason == "" {
		return &StatusInfo{Status: "standby", Detail: d.Disconnected, Reason: ReasonCDPDisconnected}
	}
	return &StatusInfo{Status: "standby", Detail: b.Detail, Reason: b.Reason}
}

// NoSongStatus 合成「已连上而没有当前歌曲」的状态：waiting_song，reason 为 ReasonNoSong 或
// ReasonWindowNotOpen（wesing），原样带出。
func NoSongStatus(reason, detail string) *StatusInfo {
	return &StatusInfo{Status: "waiting_song", Detail: detail, Reason: reason}
}

// RecheckWait 等待 total（「版本不支持」等判定的重查间隔），其间每 step 调一次 running，使进程退出在一个
// step 内即被察觉，而不是等满 total（设计 §3.2：重查间隔只用于重做那项判定）。收到 stop 返回
// (true, false)；running 返回假即返回 (false, true)，不再调用它；等满返回 (false, false)。
// 每个 step 末尾调一次（含等满的那一刻），不在开始时调：调用方刚判过进程在运行。
func RecheckWait(stop <-chan struct{}, total, step time.Duration, running func() bool) (stopped, gone bool) {
	deadline := time.Now().Add(total)
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			return false, false
		}
		if wait > step {
			wait = step
		}
		t := time.NewTimer(wait)
		select {
		case <-stop:
			t.Stop()
			return true, false
		case <-t.C:
		}
		if !running() {
			return false, true
		}
	}
}
