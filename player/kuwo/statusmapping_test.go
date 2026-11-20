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

// statusFor realises the build-matching design §3.4 table. The manual-launch
// case must not be mistaken for a version rejection, and a version rejection
// must not become a bare disconnect.
// 变异自证（隔离副本，实测）：把 ErrRestartNotAuthorized 改映射到 version_unsupported
// → 红；去掉 cdp.ErrVersionUnsupported 分支 → 红。商店版门禁（#212）的两条：去掉商店版分支（落到断开）、
// 去掉路径未知分支、商店版的 detail 与播放核心不支持的相同、新加的 detail 漏进译表，均实测变红。
func TestStatusForMapsReasons(t *testing.T) {
	store := &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\x\kwmusic.exe`}
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{watchdog.ErrNotRunning, player.ReasonProcessNotRunning},
		{store, player.ReasonVersionUnsupported},
		{fmt.Errorf("wrapped: %w", store), player.ReasonVersionUnsupported},
		{launch.ErrExeUnknown, player.ReasonNoAdmin},
		{pos.ErrUnsupportedCore, player.ReasonVersionUnsupported},
		{cdp.ErrVersionUnsupported, player.ReasonVersionUnsupported},
		{fmt.Errorf("wrapped: %w", cdp.ErrVersionUnsupported), player.ReasonVersionUnsupported},
		{watchdog.ErrRestartNotAuthorized, player.ReasonCDPDisconnected},
		{cdp.ErrUnavailable, player.ReasonCDPDisconnected},
		{watchdog.ErrPortNotReady, player.ReasonCDPDisconnected},
		{errors.New("other"), player.ReasonCDPDisconnected},
	} {
		reason, detail := statusFor(tc.err)
		if reason != tc.reason || detail == "" {
			t.Fatalf("%v -> reason=%q detail=%q want reason=%q", tc.err, reason, detail, tc.reason)
		}
		// detail 存进变量再交给 i18n.T，catalogcoverage 扫不到（AGENTS.md §6.4 ⑧），这里逐条查译表里有它。
		if i18n.T(detail) == detail {
			t.Errorf("detail %q 没有中文译文", detail)
		}
	}
	// The not-authorized case is a disconnect with a distinct, actionable detail,
	// not the version-unsupported reason and not the generic retry message.
	naReason, naDetail := statusFor(watchdog.ErrRestartNotAuthorized)
	_, genericDetail := statusFor(errors.New("other"))
	_, versionDetail := statusFor(cdp.ErrVersionUnsupported)
	if naReason == player.ReasonVersionUnsupported || naDetail == genericDetail || naDetail == versionDetail {
		t.Fatalf("not-authorized not distinct: %q", naDetail)
	}
	// 商店版要给出改用官网安装版的提示，不与播放核心不支持混为一条。
	if _, storeDetail := statusFor(store); storeDetail == versionDetail {
		t.Fatalf("store detail not distinct: %q", storeDetail)
	}
}
