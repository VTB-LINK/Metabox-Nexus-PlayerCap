package kuwo

import (
	"errors"
	"fmt"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
	"Metabox-Nexus-PlayerCap/player/kuwo/watchdog"
)

// statusFor realises the build-matching design §3.4 table. The manual-launch
// case must not be mistaken for a version rejection, and a version rejection
// must not become a bare disconnect.
// 变异自证（隔离副本，实测）：把 ErrRestartNotAuthorized 改映射到 version_unsupported
// → 红；去掉 cdp.ErrVersionUnsupported 分支 → 红。
func TestStatusForMapsReasons(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{watchdog.ErrNotRunning, player.ReasonProcessNotRunning},
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
	}
	// The not-authorized case is a disconnect with a distinct, actionable detail,
	// not the version-unsupported reason and not the generic retry message.
	naReason, naDetail := statusFor(watchdog.ErrRestartNotAuthorized)
	_, genericDetail := statusFor(errors.New("other"))
	_, versionDetail := statusFor(cdp.ErrVersionUnsupported)
	if naReason == player.ReasonVersionUnsupported || naDetail == genericDetail || naDetail == versionDetail {
		t.Fatalf("not-authorized not distinct: %q", naDetail)
	}
}
