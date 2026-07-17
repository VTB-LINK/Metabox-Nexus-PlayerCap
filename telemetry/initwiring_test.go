package telemetry

// 本文件只测一件事：**Init 真的把该传的东西接到了 sentry 上**。
// DSN 门控在 dsngate_test.go，environment 的纯函数映射在 environment_test.go。
//
// # 这个文件是补一个缺口，缺口值得写下来
//
// 另两个测试文件**结构性地够不到** sentry.Init 的调用点：dsngate 用空 DSN 调 Init，正好
// 走早退；environment 只测 environmentFor 这个纯函数，从不看它的返回值有没有被用上。
// 实测 Init 覆盖率 33.3%，整个 ClientOptions 字面量**零覆盖** —— 于是下面七个变异全部存活，
// 四道门禁（gofmt/build/vet/test）一道不响：
//
//	Environment: env → envProduction     存活
//	Dsn: dsn → ""                        存活
//	Release: opts.Version → ""           存活
//	AttachStacktrace: true → false       存活
//	DisableLogs → false                  存活
//	DisableMetrics → false               存活
//	EnableTracing → true                 存活
//
// 最狠的是第一条：写死成 production 后，dev 构建的流量混进主播真机的统计，而启动日志打的
// 仍是**独立算出**的 env 变量 —— 日志说 development，实际发送 production，两者相反且都「正常」。
// environment_test.go 的注释自称守的就是这个方向，实测它只守到纯函数出口为止。
//
// 教训：测纯函数便宜，测接线才值钱。**判据是「这个变异会不会红」，不是「这行有没有测试」。**

import (
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"

	"Metabox-Nexus-PlayerCap/config"
)

// probeDSN 格式合法、主机不存在。sentry.Init 只解析 DSN 不连接，且本文件只 Init 不 Capture，
// 所以零出网、零 event、不烧配额。
const probeDSN = "https://public@o0.ingest.sentry.io/0"

// TestInitAppliesOptionsToSentry 钉死 Init 传给 sentry 的每一个字段。
//
// 手法：Init 之后读 `sentry.CurrentHub().Client().Options()` —— 那是 sentry **实际生效**的
// 配置，不是我们自己的副本。任何字段接错、写死、漏传，这里都红。
func TestInitAppliesOptionsToSentry(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = probeDSN

	Init(Options{Version: "9.9.9", IsRelease: true})

	client := sentry.CurrentHub().Client()
	if client == nil {
		t.Fatal("非空 DSN 下 Init 之后 sentry 仍没有 client —— 根本没初始化")
	}
	got := client.Options()

	// ── 接线：这三个必须来自入参，不能是写死的字面量 ──

	if got.Dsn != probeDSN {
		t.Errorf("Dsn = %q，want %q\n"+
			"门控看的是包变量 dsn，实际投递地址却是别的 —— 日志会照旧打「遥测已启用」，"+
			"而 event 一条也发不出去（空 DSN 下 sentry.Init 返回 nil error，见包注释）",
			got.Dsn, probeDSN)
	}
	if got.Release != "9.9.9" {
		t.Errorf("Release = %q，want \"9.9.9\"\n"+
			"event 会丢失版本归属：Sentry 里看得到崩溃，却认不出是哪次构建 —— "+
			"而 dev 构建的版本号带 commit hash，正是唯一能定位构建的东西", got.Release)
	}
	if got.Environment != envProduction {
		t.Errorf("Environment = %q，want %q\n"+
			"IsRelease=true 必须进 production。这一条写死的话，启动日志打的仍是独立算出的 env，"+
			"与实际发送值相反 —— 两边都「看着正常」，没人会发现", got.Environment, envProduction)
	}

	// ── 配额闸门：telemetry.go 的注释自称「客户端是唯一的防线」，那这道防线就得有人守 ──
	//
	// 免费版 5k event/月，Custom Rate Limits 要 Business plan —— 服务端拦不住我们。
	// 而 sentry-go 每个 minor 都带 breaking change（EnableLogs 就被反转成过 DisableLogs），
	// 升级时默认值再翻一次，这几条断言就是唯一会喊的人。

	if !got.AttachStacktrace {
		t.Error("AttachStacktrace 必须为 true —— 第 6 步的 panic 上报没有栈等于没报，" +
			"只剩一行错误字符串，定位不到任何东西")
	}
	if !got.DisableLogs {
		t.Error("DisableLogs 必须为 true —— logs 是独立于 error 的配额项，我们一条都不用")
	}
	if !got.DisableMetrics {
		t.Error("DisableMetrics 必须为 true —— 同上，独立配额项")
	}
	if got.EnableTracing {
		t.Error("EnableTracing 必须为 false —— trace 是另一套配额，且会给直播热路径加开销")
	}
}

// TestInitAttachesOSInfoToScope 钉死**接线**：Init 真的把系统信息挂到了 scope 上。
//
// 采得再对，没挂上去等于没采，而且四道门禁全绿。手法：从 hub 的 scope 生成一个事件，
// 看 context/tag 在不在 —— 那是每条上报实际会带上的东西。
//
// 变异自证：删掉 Init 里的 sentry.ConfigureScope(...) 即红。
func TestInitAttachesOSInfoToScope(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = probeDSN

	Init(Options{Version: "9.9.9", IsRelease: true})

	ev := sentry.CurrentHub().Scope().ApplyToEvent(&sentry.Event{}, nil, nil)
	if ev == nil {
		t.Fatal("ApplyToEvent 返回 nil")
	}

	osCtx, ok := ev.Contexts["os"]
	if !ok {
		t.Fatal("Init 之后 scope 里没有 os context —— 每条上报都会缺系统信息，" +
			"而 sentry 的默认集成只会填一个 runtime.GOOS（字符串 \"windows\"）")
	}
	// 大小写是有意的判据：我们填 "Windows"，默认集成填的是 runtime.GOOS 即小写 "windows"。
	// 读到小写就说明我们的没生效、被默认值顶上了。
	if name := osCtx["name"]; name != "Windows" {
		t.Errorf("os.name = %v，want \"Windows\"（小写的 \"windows\" 意味着这是 sentry 默认"+
			"集成填的 runtime.GOOS，我们采的那份没生效）", name)
	}
	if v, _ := osCtx["version"].(string); !strings.HasPrefix(v, "10.") && !strings.HasPrefix(v, "11.") {
		t.Errorf("os.version = %q —— 本机跑出来应当是 10.x（Win10/11 的 major 都是 10）", v)
	}
	if _, ok := ev.Tags["windows.compat_shim"]; !ok {
		t.Error("没有 windows.compat_shim tag —— 那条 tag 是 park 黑屏那条线索的唯一分面")
	}
	if _, ok := ev.Tags["os.build"]; !ok {
		t.Error("没有 os.build tag")
	}
}

// TestInitAttachesAppInfoToScope 钉死**接线**：Init 真的把自身信息挂到了 scope 上。
//
// 变异自证：删掉 Init 里 ConfigureScope 中的 applyAppInfo(scope, app) 即红。
func TestInitAttachesAppInfoToScope(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = probeDSN

	cfg := config.DefaultConfig()
	cfg.EffectStrategy = "park"
	cfg.ExplicitKeys["poll"] = true

	Init(Options{
		Version:     "9.9.9",
		IsRelease:   true,
		Config:      cfg,
		PlayerNames: []string{"wesing", "kugou"},
	})

	ev := sentry.CurrentHub().Scope().ApplyToEvent(&sentry.Event{}, nil, nil)
	if ev == nil {
		t.Fatal("ApplyToEvent 返回 nil")
	}

	pc, ok := ev.Contexts["playercap"]
	if !ok {
		t.Fatal("Init 之后 scope 里没有 playercap context —— 上报里不会有版本/路径/配置")
	}
	if pc["version"] != "9.9.9" {
		t.Errorf("playercap.version = %v，want \"9.9.9\"", pc["version"])
	}
	if _, ok := pc["config"]; !ok {
		t.Error("playercap context 里没有 config —— 「主播改了什么」常是「为什么只有他崩」的答案")
	}
	if _, ok := pc["exe_path"]; !ok {
		t.Error("playercap context 里没有 exe_path")
	}

	// park 是个已知的故障维度（Win11 上屏外保活失效会黑屏），必须能分面。
	if got := ev.Tags["config.effect_strategy"]; got != "park" {
		t.Errorf("tag config.effect_strategy = %q，want \"park\"", got)
	}
	if got := ev.Tags["config.customized"]; got != "true" {
		t.Errorf("tag config.customized = %q，want \"true\"（cfg 里显式设了 poll）", got)
	}
}

// TestInitUsesEnvironmentForResult 钉死 development 方向。
//
// 与上面那条互补：上面用 IsRelease=true 覆盖 production，这条覆盖 development。
// 两条都在，`Environment:` 那一行就没法写死成任何字面量 —— 写死成谁都会有一条红。
// 单独一条是不够的，这正是原来那个「只测纯函数」的坑的变种。
func TestInitUsesEnvironmentForResult(t *testing.T) {
	orig := dsn
	t.Cleanup(func() { dsn = orig })
	dsn = probeDSN

	Init(Options{Version: "Metabox-Nexus-PlayerCap-20260717-002700-abc1234", IsRelease: false})

	client := sentry.CurrentHub().Client()
	if client == nil {
		t.Fatal("非空 DSN 下 Init 之后 sentry 仍没有 client")
	}
	if got := client.Options().Environment; got != envDevelopment {
		t.Errorf("Environment = %q，want %q —— dev 构建（非 semver 版本号）必须进 development，"+
			"否则 dev 流量混进主播真机的错误统计", got, envDevelopment)
	}
}
