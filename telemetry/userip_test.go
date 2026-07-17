package telemetry

// 本文件只测一件事：**上报里带着让 Sentry 推断 IP 的哨兵**。
//
// 这条曾经是个静默的功能缺口：所有事件都正常，只是 Sentry 里的 IP 永远为空 —— 而没人会
// 因为「少了一个字段」去报 bug。它之所以被发现，是有人主动去 Sentry 里找了一眼。

import (
	"testing"

	"github.com/getsentry/sentry-go"
)

// TestUserIPIsAutoSentinel 钉死哨兵值逐字正确。
//
// `{{auto}}` 是被 Sentry **服务端**识别的字面量，不是模板占位符。写错（比如写成 `{auto}`、
// `{{ auto }}`、或者真去查一个公网 IP 填进去）都不会报错 —— 只会让 Sentry 里出现一个
// 字面值为那串字符的假 IP，看着像有数据，实际是垃圾。
//
// 变异自证：改 ipAuto 的值即红。
func TestUserIPIsAutoSentinel(t *testing.T) {
	if ipAuto != "{{auto}}" {
		t.Errorf("ipAuto = %q，want \"{{auto}}\" —— 这是 Sentry 服务端识别的字面量，"+
			"错一个字符就会变成一个字面值为该字符串的假 IP", ipAuto)
	}
}

// TestApplyUserIPSetsSentinel applyUserIP 把哨兵写进 scope，且只写 IP。
//
// 只设 IPAddress 是刻意的：我们没有账号体系，不给主播编身份。多设 ID/Username/Email 会让
// Sentry 的 "Users" 统计凭空多出一个假用户维度。
func TestApplyUserIPSetsSentinel(t *testing.T) {
	scope := sentry.NewScope()
	applyUserIP(scope)
	ev := scope.ApplyToEvent(&sentry.Event{}, nil, nil)

	if ev.User.IPAddress != "{{auto}}" {
		t.Errorf("User.IPAddress = %q，want \"{{auto}}\"", ev.User.IPAddress)
	}
	if ev.User.ID != "" || ev.User.Username != "" || ev.User.Email != "" {
		t.Errorf("除 IP 外不该设任何 user 字段，实得 ID=%q Username=%q Email=%q",
			ev.User.ID, ev.User.Username, ev.User.Email)
	}
}

// TestInitAttachesUserIP 钉死**接线**：Init 真的把它挂上了。
//
// 变异自证：删掉 Init 里的 applyUserIP(scope) 即红。
func TestInitAttachesUserIP(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = probeDSN

	Init(Options{Version: "9.9.9", IsRelease: true})

	ev := sentry.CurrentHub().Scope().ApplyToEvent(&sentry.Event{}, nil, nil)
	if ev == nil {
		t.Fatal("ApplyToEvent 返回 nil")
	}
	if ev.User.IPAddress != "{{auto}}" {
		t.Errorf("Init 之后 scope 里的 User.IPAddress = %q，want \"{{auto}}\" —— "+
			"没挂上的话所有事件的 IP 都是空的，而且不会有任何报错", ev.User.IPAddress)
	}
}

// TestSendDefaultPIIDoesNotProvideIP 把「SendDefaultPII 给不出 IP」这条实测钉成可执行的证据。
//
// 这不是测我们的代码，是测 sentry-go 的行为。它存在的理由：`SendDefaultPII` 这个名字看着
// 就该管「要不要发 IP」，将来一定会有人（包括我自己）想「设了它就不用 SetUser 了吧」。
// 实测 v0.48.0：开不开它，User.IPAddress 都是空 —— 那个选项管的是**入站 HTTP 请求**的
// REMOTE_ADDR，而 PlayerCap 不接受入站请求。
//
// 如果哪天 sentry-go 改了这个行为，这个测试会 Skip 并提示可以简化。
func TestSendDefaultPIIDoesNotProvideIP(t *testing.T) {
	var got *sentry.Event
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:            probeDSN,
		SendDefaultPII: true, // 就算开着
		BeforeSend: func(ev *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			got = ev
			return nil // 扣下不发
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// 用独立 hub + 干净 scope：不能让我们自己 Init 设的 {{auto}} 混进来，
	// 否则这个测试就变成了自我印证。
	hub := sentry.NewHub(client, sentry.NewScope())
	hub.CaptureMessage("probe")
	hub.Flush(0)

	if got == nil {
		t.Fatal("没有捕获到事件")
	}
	if got.User.IPAddress != "" {
		t.Skipf("sentry-go 改了行为：SendDefaultPII=true now yields IPAddress=%q —— "+
			"applyUserIP 也许可以简化成只设这个选项，去核实一遍", got.User.IPAddress)
	}
}
