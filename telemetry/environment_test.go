package telemetry

// 本文件只测一件事：**「是不是发布版本」如何映射成 Sentry 的 environment**。
// DSN 门控在 dsngate_test.go。
//
// 判据本身（isReleaseVersion）不在这里测 —— 它住在 main 包，由 updatedecision_test.go
// 守着，且那边已经用两条流水线的真实 VERSION 形状钉过：
//
//	"3.0.0"                                    → true   （release.yml 的形状）
//	"Metabox-Nexus-PlayerCap-20260426-deadbee" → false  （build-windows.yml 的形状）
//
// 本文件只管从那个 bool 到字符串的这一跳。

import "testing"

// TestEnvironmentForMapping 钉死映射方向。
//
// 写反的后果不对称，所以方向本身值得一个测试：dev 流量混进 production 会污染主播真机的
// 错误统计（且没人会注意到 —— 两边都有 event，看着都正常）；反过来 production 流量掉进
// development，则是线上故障从我们眼皮底下消失。
//
// 变异自证：把 `if isRelease` 改成 `if !isRelease` 即红。
func TestEnvironmentForMapping(t *testing.T) {
	if got := environmentFor(true); got != envProduction {
		t.Errorf("发布版本必须进 production，实得 %q", got)
	}
	if got := environmentFor(false); got != envDevelopment {
		t.Errorf("非发布版本（dev 构建、本地构建）必须进 development，实得 %q", got)
	}
}

// TestEnvironmentValuesAreSentryConventional Sentry 的 environment 是个自由字符串，
// 写错了不会报错 —— 只会在控制台里凭空多出一个环境，且旧数据不会迁移过去。
//
// 变异自证：把任一常量改掉即红。
func TestEnvironmentValuesAreSentryConventional(t *testing.T) {
	if envProduction != "production" {
		t.Errorf("envProduction = %q，Sentry 的约定值是 \"production\"", envProduction)
	}
	if envDevelopment != "development" {
		t.Errorf("envDevelopment = %q，Sentry 的约定值是 \"development\"", envDevelopment)
	}
}
