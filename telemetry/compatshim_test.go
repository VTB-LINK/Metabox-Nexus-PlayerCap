package telemetry

// 本文件只测一件事：**兼容性 shim 的检测与上报**。
// edition/arch 的码翻译在 osnaming_test.go，Init 的接线在 initwiring_test.go。
//
// # 这段代码为什么存在
//
// x/sys 对 RtlGetVersion 的文档原话：它 "ignoring manifest semantics but **is affected by
// the application compatibility layer**"。免疫兼容层的是 RtlGetNtVersionNumbers。
//
// 而 park.IsWindows11() 正建立在 RtlGetVersion 之上，于是这条链是通的：
//
//	主播右键 exe →「属性」→「兼容性」→「以 Windows 8 兼容模式运行」
//	  → RtlGetVersion 谎报 6.2.9200
//	  → park.IsWindows11() 返回 false
//	  → Win11 上 park 的强制降级不触发
//	  → DWM 不合成不可见窗口 → 主播的特效画面黑掉
//
// 「开兼容模式试试」正是非技术主播照着教程会干的第一件事。所以这个不一致本身就是要上报的
// 信号——它是一条**与 Sentry 无关的现有 bug 线索**，我们只是顺手把它测出来。

import (
	"testing"

	"github.com/getsentry/sentry-go"
	"golang.org/x/sys/windows"
)

// fakeVersions 让两个 API 各说各话。
func fakeVersions(t *testing.T, realMaj, realMin, realBuild, shimMaj, shimMin, shimBuild uint32) {
	t.Helper()
	origV, origN := rtlGetVersion, rtlGetNtVersionNumbers
	t.Cleanup(func() { rtlGetVersion, rtlGetNtVersionNumbers = origV, origN })

	rtlGetNtVersionNumbers = func() (uint32, uint32, uint32) { return realMaj, realMin, realBuild }
	rtlGetVersion = func() *windows.OsVersionInfoEx {
		return &windows.OsVersionInfoEx{
			MajorVersion: shimMaj, MinorVersion: shimMin, BuildNumber: shimBuild,
		}
	}
}

// TestShimDetected 钉死「两个 API 不一致 = 有 shim」。
//
// 用的是最要命的那个真实组合：真 Win11（10.0.22631），被兼容层报成 Win8（6.2.9200）——
// 那正是 GetVersionEx 在没有清单时返回的值，也正是 park 会被骗到的形状。
//
// 变异自证：把 collectOSInfo 里的 `o.Shimmed = ...` 改成 `false` 即红。
func TestShimDetected(t *testing.T) {
	fakeVersions(t, 10, 0, 22631, 6, 2, 9200)

	o := collectOSInfo()

	if !o.Shimmed {
		t.Error("两个 API 报了不同的版本，必须判为有 shim —— 漏判等于放过 park 黑屏那条链")
	}
	if got := o.version(); got != "10.0.22631" {
		t.Errorf("version() = %q，want \"10.0.22631\" —— 真实版本必须来自免疫 shim 的 "+
			"RtlGetNtVersionNumbers，不能来自被骗的那个", got)
	}
	if got := o.shimVersion(); got != "6.2.9200" {
		t.Errorf("shimVersion() = %q，want \"6.2.9200\" —— 这是 park 实际看到的值，"+
			"上报里必须留着它，否则没法解释 park 为什么判错", got)
	}
}

// TestShimNotDetectedWhenVersionsAgree 两个 API 一致时不许误报。
//
// 误报的代价：每台正常机器都带上 compat_shim=true，这个 tag 就废了 —— 真出事时分不出来。
func TestShimNotDetectedWhenVersionsAgree(t *testing.T) {
	fakeVersions(t, 10, 0, 19045, 10, 0, 19045)

	o := collectOSInfo()

	if o.Shimmed {
		t.Error("两个 API 报的版本一致，不许判为有 shim")
	}
	if got := o.version(); got != "10.0.19045" {
		t.Errorf("version() = %q，want \"10.0.19045\"", got)
	}
}

// TestShimDetectedOnAnyFieldMismatch 三个字段任一不同都算 shim。
//
// 只比 Major 是不够的：兼容层能只改 build（例如把 Win11 的 22631 报成 Win10 的 19045，
// 两者 Major/Minor 都是 10.0）——那恰恰是最难发现、也最能骗过 park 的形状。
func TestShimDetectedOnAnyFieldMismatch(t *testing.T) {
	cases := []struct {
		name                        string
		realMaj, realMin, realBuild uint32
		shimMaj, shimMin, shimBuild uint32
	}{
		{"只有 Major 不同", 10, 0, 19045, 6, 0, 19045},
		{"只有 Minor 不同", 10, 0, 19045, 10, 1, 19045},
		{"只有 Build 不同（Win11 被报成 Win10 —— 最阴的一种）", 10, 0, 22631, 10, 0, 19045},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeVersions(t, c.realMaj, c.realMin, c.realBuild, c.shimMaj, c.shimMin, c.shimBuild)
			if !collectOSInfo().Shimmed {
				t.Error("字段不一致却没判为 shim")
			}
		})
	}
}

// TestShimTagAlwaysSet 钉死 compat_shim tag **两种情况都设**。
//
// 只在 true 时设 tag 的话，Sentry 里就没法回答「有多少主播开着兼容模式」——
// 分母不存在，只能看到分子。
func TestShimTagAlwaysSet(t *testing.T) {
	for _, shimmed := range []bool{true, false} {
		scope := sentry.NewScope()
		applyOSInfo(scope, osInfo{Major: 10, Minor: 0, Build: 19045, Shimmed: shimmed})
		ev := scope.ApplyToEvent(&sentry.Event{}, nil, nil)

		want := "false"
		if shimmed {
			want = "true"
		}
		if got := ev.Tags["windows.compat_shim"]; got != want {
			t.Errorf("Shimmed=%v 时 tag windows.compat_shim = %q，want %q", shimmed, got, want)
		}
	}
}

// TestShimContextOnlyWhenShimmed shim 的详情 context 只在真有 shim 时出现。
//
// 正常机器上带一个空的 shim context 是噪音；而有 shim 时，那个 context 里的
// real vs reported 两个版本号是唯一能解释「park 为什么判错」的东西。
func TestShimContextOnlyWhenShimmed(t *testing.T) {
	scope := sentry.NewScope()
	applyOSInfo(scope, osInfo{Major: 10, Minor: 0, Build: 19045,
		ShimMajor: 10, ShimMinor: 0, ShimBuild: 19045, Shimmed: false})
	ev := scope.ApplyToEvent(&sentry.Event{}, nil, nil)
	if _, ok := ev.Contexts["windows_compat_shim"]; ok {
		t.Error("没有 shim 的机器不该带 shim 详情 context")
	}

	scope = sentry.NewScope()
	applyOSInfo(scope, osInfo{Major: 10, Minor: 0, Build: 22631,
		ShimMajor: 6, ShimMinor: 2, ShimBuild: 9200, Shimmed: true})
	ev = scope.ApplyToEvent(&sentry.Event{}, nil, nil)
	ctx, ok := ev.Contexts["windows_compat_shim"]
	if !ok {
		t.Fatal("有 shim 却没有详情 context —— 那 tag 报了 true 也没法查")
	}
	if ctx["real_version"] != "10.0.22631" {
		t.Errorf("real_version = %v，want \"10.0.22631\"", ctx["real_version"])
	}
	if ctx["reported_version"] != "6.2.9200" {
		t.Errorf("reported_version = %v，want \"6.2.9200\"", ctx["reported_version"])
	}
}
