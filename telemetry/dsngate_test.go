package telemetry

// 本文件只测一件事：**DSN 的有无如何决定遥测启不启用**。
// environment 的映射在 environment_test.go。
//
// 这条门控是整个「不把 DSN 写进仓库」方案的承重墙：本地开发、以及任何没配 SENTRY_DSN 的
// 构建，全靠空 DSN 自动禁用，没有别的开关。

import (
	"testing"

	"github.com/getsentry/sentry-go"
)

// TestEnabledFollowsDSN 钉死 Enabled 只看 dsn 有没有值。
//
// 变异自证：`return dsn != ""` 改成 `return true` 或 `return dsn == ""`，两种都红。
func TestEnabledFollowsDSN(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })

	dsn = ""
	if Enabled() {
		t.Error("空 DSN 必须判为未启用 —— 本地构建与未注入的 CI 构建全靠这条自动禁用")
	}

	dsn = "https://public@o0.ingest.sentry.io/0"
	if !Enabled() {
		t.Error("注入了 DSN 就必须判为启用，否则 CI 构建白配了 secret")
	}
}

// TestEmptyDSNNeverTouchesSentry 钉死「空 DSN 时 Init 根本不碰 sentry」。
//
// 为什么要测这条、而不是信 sentry 自己会处理空 DSN：实测 sentry-go v0.48.0，
// `sentry.Init(ClientOptions{Dsn: ""})` **返回 nil error 并且真的装了一个全局 client**
// （`CurrentHub().Client()` 非 nil，`CaptureException` 还返回非 nil 的 EventID）。
// 也就是说 sentry 不会因为 DSN 是空的就拒绝初始化 —— 它照样把自己装上，只是发不出去。
//
// 所以「不上报」这个结果，是我们的早退挣来的，不是 sentry 送的。这个测试守的就是那道早退。
//
// 变异自证：删掉 Init 里的 `if !Enabled() { return }` 即红 —— 全局 hub 的 client 会被
// sentry.Init 换成非 nil，前后不再相等。
func TestEmptyDSNNeverTouchesSentry(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })

	before := sentry.CurrentHub().Client()

	dsn = ""
	Init(Options{Version: "0.0.0", IsRelease: false})

	if after := sentry.CurrentHub().Client(); after != before {
		t.Error("空 DSN 下 Init 动了全局 sentry hub —— 早退没生效，" +
			"于是本地开发也会装上一个 sentry client")
	}
}
