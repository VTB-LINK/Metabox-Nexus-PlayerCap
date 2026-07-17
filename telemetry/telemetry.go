// Package telemetry 把运行时故障上报到 Sentry。
//
// **本包是纯增量的可观测性设施：它的任何一步失败都不许影响主程序。** 这个服务挂了 = 直播
// 事故，而「没上报」只是我们少看一条数据。所以包内所有错误一律 log 后返回 —— 不向上传播
// error（没有任何调用方能对它做出有意义的反应）、不 panic、不阻塞采集循环。
//
// # DSN 与启用条件
//
// dsn 编译期由 ldflags 注入，与 main.Version 走同一条路（release.yml、build-windows.yml
// 的 `go build -ldflags` 那行）。**空 DSN = 禁用**，于是本地 `go build`、以及任何没配
// SENTRY_DSN 的构建，天然不上报 —— 不需要额外的开关配置项，也不需要把 DSN 写进仓库。
//
// # Enabled 为什么不问 sentry 自己
//
// 实测 sentry-go v0.48.0，空 DSN 下：
//
//	sentry.Init(ClientOptions{Dsn: ""})  → 返回 nil error（不是错误）
//	sentry.CurrentHub().Client()         → **非 nil**
//	sentry.CaptureException(err)         → 返回**非 nil 的 EventID**
//
// 事件确实发不出去（没装 transport），但上面三个信号**没有一个**能用来回答「遥测启用了
// 吗」——它们全都会说「启用了」。所以只能自己记 dsn，见 Enabled。
package telemetry

import (
	"github.com/getsentry/sentry-go"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
)

var log = logger.New("Telemetry")

// dsn 由 ldflags 在编译期注入：
//
//	go build -ldflags "-X 'Metabox-Nexus-PlayerCap/telemetry.dsn=https://...'"
//
// **别给它填默认值。** DSN 本身设计上是可公开的（它会被编译进发给主播的 exe，反编译即得，
// 官方也是这么定位的），但真实 DSN 一旦写进这里就等于提交进了 git 历史 —— 换 DSN 时改不掉，
// 且仓库公开性一旦变化就变成了个问题。CI 从 secret 注入是唯一的路。
//
// 空串是**有意义的常态**而非未配置：本地开发、以及任何未注入的构建，都靠它自动禁用。
var dsn = ""

// Sentry 的 environment 维度。只有两个取值。
//
// 判据直接复用 main.isReleaseVersion —— 那是自动更新的 kill switch，updatedecision_test.go
// 守着它，且两条流水线的 VERSION 形状正好落在判据两边（实测）：
//
//	release.yml        VERSION="${TAG_NAME#v}"                     → "2.1.0"        → semver 有效 → production
//	build-windows.yml  VERSION="${OUTPUT_NAME}-${BUILD_TIME}-${COMMIT_HASH}"
//	                                                               → "Metabox-..."  → semver 无效 → development
//
// dev 构建用非 semver 是**故意的**（它同时是「跳过自动更新检查」的开关），我们只是搭了这趟
// 顺风车，没有新增任何机制。反过来说：**谁要改 build-windows.yml 的 VERSION 格式，得知道
// 它会同时挪动 Sentry 的环境归属。**
//
// 注意 prerelease（如 3.0.0-beta.32）判为 production —— 它是发给主播、走自动更新的真实流量。
// environment 回答的是「谁在跑」，不是「多稳定」。要单独看 beta，用 release 维度筛。
const (
	envProduction  = "production"
	envDevelopment = "development"
)

// Options 是 Init 的入参。
type Options struct {
	// Version 是 main.Version 的原值（ldflags 注入的那个），直接用作 Sentry 的 release 名。
	//
	// 不做 displayVersion 处理：release.yml 注入的本来就是裸版本号（tag 的 v 前缀在 CI 里
	// 已经剥掉了），而 dev 构建的 "Metabox-Nexus-PlayerCap-<时间>-<commit>" 带 commit hash，
	// 正好唯一标识一次构建。两种形状都能直接当 release 名用。
	Version string

	// IsRelease 来自 main.isReleaseVersion(Version)，决定 environment。
	//
	// 由 main 传进来、而不是本包自己判：那个函数是自动更新的 kill switch，有测试守着；
	// 在这儿复制一份 semver 判据，迟早会跟它分叉，而分叉的后果是「dev 流量混进
	// production」——一种没人会注意到的错。
	IsRelease bool

	// Config 是 resolved 之后的配置。整份带上 —— 「主播改了什么」常常就是「为什么只有他崩」
	// 的答案，见 appinfo.go。
	Config config.Config

	// PlayerNames 是播放器注册表（main.go 里那个）。要它是因为 cfg.Players 只含**被显式配过**
	// 的播放器，而我们要报的是每个播放器实际在用的 offset/poll（未配的会回落到全局值）。
	PlayerNames []string
}

// Enabled 报告遥测是否启用，即 DSN 有没有被注入。
//
// **别改成去问 sentry。** 见包注释那段实测：空 DSN 下 client 非 nil、EventID 也非 nil，
// 没有一个信号能用。
func Enabled() bool { return dsn != "" }

// environmentFor 把「是不是发布版本」映射成 Sentry 的 environment。
func environmentFor(isRelease bool) string {
	if isRelease {
		return envProduction
	}
	return envDevelopment
}

// Init 初始化 Sentry。未注入 DSN 时直接返回 —— 那是本地构建的常态，不是错误。
//
// 不返回 error：遥测起不来不是主程序需要处理的事。
func Init(opts Options) {
	if !Enabled() {
		log.Info("未注入 DSN，遥测已禁用")
		return
	}

	env := environmentFor(opts.IsRelease)
	err := sentry.Init(sentry.ClientOptions{
		Dsn:         dsn,
		Release:     opts.Version,
		Environment: env,

		// panic 上报没有栈等于没报 —— 只剩一行错误字符串，定位不到任何东西。
		AttachStacktrace: true,

		// 裁掉 Guard 自己那一帧，理由见 panic.go 的 trimGuardFrame —— 简言之：sentry 只过滤
		// 它自己包内的帧，我们的 recover 函数会永远占据最内层，而最内层恒定有让不同 panic
		// 塌进同一个 issue 的风险。
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			trimGuardFrame(event)
			return event
		},

		// 下面三个是**配额闸门，必须显式写，别依赖默认值**。
		//
		// 免费版 5k event/月，且 Custom Rate Limits 要 Business plan —— 服务端**没有任何
		// 东西**能拦住我们，客户端是唯一的防线。logs 与 metrics 都是独立于 error 的配额项，
		// 我们一个都不用。
		//
		// 「别依赖默认值」不是洁癖：sentry-go 没有 1.0，近 6 个 minor 每个都带 breaking
		// change，其中一次正是把 EnableLogs **反转**成了 DisableLogs。默认值翻转过一次，
		// 就能再翻一次，而那种翻转是静默烧配额。
		DisableLogs:    true,
		DisableMetrics: true,
		EnableTracing:  false,
	})
	if err != nil {
		// 走到这儿说明 DSN 注入了但解析不了（sentry.Init 只在 DSN 格式错误时返回 error）。
		// 大概率是 CI 的 secret 配错了。主程序照常跑，只是没上报。
		log.Warn("遥测初始化失败: %v，程序继续运行", err)
		return
	}

	// 系统信息挂进 scope，之后每条上报都自动带上。
	//
	// 采集放在这里、而不是每次上报时现采：这些值在进程生命周期内不会变，而 panic 上报路径上
	// 应当尽可能少做事 —— 那时进程已经在死了。
	//
	// 默认的 Environment 集成给的 contexts["os"].name 只是 runtime.GOOS（字符串 "windows"），
	// 没有版本/build/edition。这一坨全得自己采，见 sysinfo.go。
	sys := collectOSInfo()
	app := collectAppInfo(opts.Version, opts.Config, opts.PlayerNames)
	sentry.ConfigureScope(func(scope *sentry.Scope) {
		applyOSInfo(scope, sys)
		applyAppInfo(scope, app)
		applyUserIP(scope) // 见 userip.go：SendDefaultPII 在 Go SDK 里给不出 IP，实测过
	})

	log.Info("遥测已启用（environment=%s release=%s）", env, opts.Version)
	logOSInfo(sys)
	logAppInfo(app)
}
