package telemetry

// 上报来源 IP。
//
// # 客户端不知道自己的公网 IP
//
// NAT 后面看到的只有内网地址，去问「我的公网 IP 是多少」还得额外发一次请求。所以这里设的
// **不是真 IP，而是字面量 `{{auto}}`** —— Sentry 服务端收到事件时，从那条 TCP 连接的来源
// 地址填进去。这是 Sentry 跨 SDK 的约定，不是我们发明的。
//
// # 别去动 SendDefaultPII，它在这儿是死的
//
// 实测（sentry-go v0.48.0，2026-07-17）：
//
//	SendDefaultPII: false → User.IPAddress = ""
//	SendDefaultPII: true  → User.IPAddress = ""    ← **一模一样**
//
// 那个选项在 Go SDK 里管的是「从**入站 HTTP 请求**的 REMOTE_ADDR 拿访客 IP」——
// 它是给接收请求的 Web 服务用的。PlayerCap 不接受任何入站请求，所以它对我们恒等于没开。
// 名字听着像「要不要发 PII」，实际不是那个意思。
//
// # DataCollection 也不影响这里
//
// v0.48 新增的 DataCollection.UserInfo 文档原话：
//
//	"UserInfo controls automatic population of user.* fields from auto-instrumentation.
//	 This does NOT gate data explicitly set via Scope.SetUser(); that is always attached."
//
// 我们走的正是 SetUser 这条显式路径，所以不受它的默认值变化影响。

import "github.com/getsentry/sentry-go"

// ipAuto 是 Sentry 的服务端 IP 推断哨兵。**必须逐字是这个** —— 它是被服务端识别的字面量，
// 不是占位符模板，写错了不会报错，只会得到一个字面值为 "{{auto}}" 的假 IP。
const ipAuto = "{{auto}}"

// applyUserIP 让 Sentry 记录事件来源的 IP。
//
// 只设 IPAddress，不设 ID/Username/Email —— 我们没有账号体系，也不想给主播编一个身份。
// 设备名不用管：sentry 的默认集成已经把 os.Hostname() 填进 ServerName 了（实测每条事件都有）。
func applyUserIP(scope *sentry.Scope) {
	scope.SetUser(sentry.User{IPAddress: ipAuto})
}
