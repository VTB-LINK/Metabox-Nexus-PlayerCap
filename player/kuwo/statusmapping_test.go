package kuwo

import (
	"errors"
	"fmt"
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
	"Metabox-Nexus-PlayerCap/player/kuwo/watchdog"
	"Metabox-Nexus-PlayerCap/player/launch"
)

// statusFor 把会话错误按进程层次映射为非歌名状态（#231，设计 §3.3 kuwo 行）：
// ErrNotRunning 报 waiting_process（按 seen 区分 process_not_running / process_exited），
// 其余均在进程运行时产生、报 standby 加各自 reason。
// 变异自证（均实测变红）：
//   - 本轮（#231）：statusFor 把 ErrNotRunning 归为在运行（standby）→ 两个未运行子例与区分断言红；
//     ErrNotRunning 忽略 seen（恒传 false）→「未运行/曾见过退出」与区分断言红。
//   - 既有（隔离副本，实测）：把 ErrRestartNotAuthorized 改映射到 version_unsupported → 红；
//     去掉 cdp.ErrVersionUnsupported 分支 → 红；去掉商店版分支（落到断开）、去掉路径未知分支、
//     商店版的 detail 与播放核心不支持的相同、新加的 detail 漏进译表，均红。
func TestStatusForMapsReasons(t *testing.T) {
	origLang := i18n.Language()
	defer i18n.SetLanguage(origLang)

	store := &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\x\kwmusic.exe`}
	cases := []struct {
		name   string
		err    error
		seen   bool
		status string
		reason string
	}{
		{"未运行/从未见过", watchdog.ErrNotRunning, false, "waiting_process", player.ReasonProcessNotRunning},
		{"未运行/曾见过退出", watchdog.ErrNotRunning, true, "waiting_process", player.ReasonProcessExited},
		{"商店版/seen=false", store, false, "standby", player.ReasonVersionUnsupported},
		{"商店版/seen=true", store, true, "standby", player.ReasonVersionUnsupported},
		{"包装商店版/seen=false", fmt.Errorf("wrapped: %w", store), false, "standby", player.ReasonVersionUnsupported},
		{"包装商店版/seen=true", fmt.Errorf("wrapped: %w", store), true, "standby", player.ReasonVersionUnsupported},
		{"读不到路径/seen=false", launch.ErrExeUnknown, false, "standby", player.ReasonNoAdmin},
		{"读不到路径/seen=true", launch.ErrExeUnknown, true, "standby", player.ReasonNoAdmin},
		{"核心不支持/seen=false", pos.ErrUnsupportedCore, false, "standby", player.ReasonVersionUnsupported},
		{"核心不支持/seen=true", pos.ErrUnsupportedCore, true, "standby", player.ReasonVersionUnsupported},
		{"桥版本不支持/seen=false", cdp.ErrVersionUnsupported, false, "standby", player.ReasonVersionUnsupported},
		{"桥版本不支持/seen=true", cdp.ErrVersionUnsupported, true, "standby", player.ReasonVersionUnsupported},
		{"包装桥版本不支持/seen=true", fmt.Errorf("wrapped: %w", cdp.ErrVersionUnsupported), true, "standby", player.ReasonVersionUnsupported},
		{"未授权自动重启/seen=false", watchdog.ErrRestartNotAuthorized, false, "standby", player.ReasonCDPDisconnected},
		{"未授权自动重启/seen=true", watchdog.ErrRestartNotAuthorized, true, "standby", player.ReasonCDPDisconnected},
		{"CDP不可用/seen=false", cdp.ErrUnavailable, false, "standby", player.ReasonCDPDisconnected},
		{"CDP不可用/seen=true", cdp.ErrUnavailable, true, "standby", player.ReasonCDPDisconnected},
		{"端口未就绪/seen=false", watchdog.ErrPortNotReady, false, "standby", player.ReasonCDPDisconnected},
		{"端口未就绪/seen=true", watchdog.ErrPortNotReady, true, "standby", player.ReasonCDPDisconnected},
		{"时钟停滞/seen=true", pos.ErrClockStalled, true, "standby", player.ReasonCDPDisconnected},
		{"其他错误/seen=false", errors.New("other"), false, "standby", player.ReasonCDPDisconnected},
		{"其他错误/seen=true", errors.New("other"), true, "standby", player.ReasonCDPDisconnected},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i18n.SetLanguage(i18n.Chinese)
			got := statusFor(tc.err, tc.seen)
			if got == nil {
				t.Fatalf("statusFor(%v, %v) returned nil", tc.err, tc.seen)
			}
			if got.Status != tc.status || got.Reason != tc.reason || got.Detail == "" {
				t.Fatalf("statusFor(%v, %v) = %+v, want status=%q reason=%q non-empty detail",
					tc.err, tc.seen, got, tc.status, tc.reason)
			}
			// 反例校验：进程不在运行不得报 standby；在运行受阻或断连不得报 waiting_process。
			if errors.Is(tc.err, watchdog.ErrNotRunning) && got.Status == "standby" {
				t.Fatalf("ErrNotRunning 不得报 standby：%v", got)
			}
			if !errors.Is(tc.err, watchdog.ErrNotRunning) && got.Status == "waiting_process" {
				t.Fatalf("进程在运行的错误 %v 不得报 waiting_process：%v", tc.err, got)
			}
			// 核对中英译文有区别（没有漏进译表）。
			i18n.SetLanguage(i18n.English)
			en := statusFor(tc.err, tc.seen)
			i18n.SetLanguage(i18n.Chinese)
			if got.Detail == en.Detail {
				t.Errorf("detail %q 没有中文译文（与英文原文相同）", en.Detail)
			}
			// 核对日志去重键与 statusFor 的非退出 reason 一致。
			wantLogReason := tc.reason
			if wantLogReason == player.ReasonProcessExited {
				wantLogReason = player.ReasonProcessNotRunning
			}
			if gotLog := errLogReason(tc.err); gotLog != wantLogReason {
				t.Errorf("errLogReason(%v) = %q, want %q", tc.err, gotLog, wantLogReason)
			}
		})
	}

	i18n.SetLanguage(i18n.Chinese)
	// 反例：未运行与已退出的 reason 和 detail 须可区分。
	notRunning := statusFor(watchdog.ErrNotRunning, false)
	exited := statusFor(watchdog.ErrNotRunning, true)
	if notRunning.Reason == exited.Reason || notRunning.Detail == exited.Detail {
		t.Fatalf("notRunning=%+v 与 exited=%+v 必须可区分", notRunning, exited)
	}

	// 未授权自动重启：报 cdp_disconnected 且文案与通用断连、版本不支持可区分。
	na := statusFor(watchdog.ErrRestartNotAuthorized, true)
	generic := statusFor(errors.New("other"), true)
	version := statusFor(cdp.ErrVersionUnsupported, true)
	if na.Status != "standby" || na.Reason == player.ReasonVersionUnsupported ||
		na.Detail == generic.Detail || na.Detail == version.Detail {
		t.Fatalf("not-authorized not distinct: %+v", na)
	}

	// 商店版要给出改用官网安装版的提示，不与播放核心不支持混为一条。
	if storeSt := statusFor(store, true); storeSt.Detail == version.Detail {
		t.Fatalf("store detail not distinct: %+v", storeSt)
	}
}
