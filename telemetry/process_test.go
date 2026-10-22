package telemetry

// 本文件只测一件事：Options.Process 的接线。设了它，事件带 process 标签、playercap 上下文不写
// config 与 overwritten、也没有取自配置的 tag；不设（主服务），scope 与引入它之前逐项一致。

import (
	"reflect"
	"sort"
	"testing"

	"github.com/getsentry/sentry-go"

	"Metabox-Nexus-PlayerCap/config"
)

// initScopeEvent 清空 hub 的 scope 后以 opts 调 Init，返回 scope 生成的事件。hub 跨 Init 复用同一个
// scope，不清空的话别的用例留下的 tag 会混进来；结束时再清一次，不把 process 标签留给别的用例。
func initScopeEvent(t *testing.T, opts Options) *sentry.Event {
	t.Helper()
	orig := dsn
	dsn = probeDSN
	sentry.CurrentHub().Scope().Clear()
	t.Cleanup(func() {
		dsn = orig
		sentry.CurrentHub().Scope().Clear()
	})

	Init(opts)
	ev := sentry.CurrentHub().Scope().ApplyToEvent(&sentry.Event{}, nil, nil)
	if ev == nil {
		t.Fatal("ApplyToEvent 返回 nil")
	}
	return ev
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestInitProcessTagsAndOmitsConfig 断言设了 Process 的事件带 process 标签，playercap 上下文只有
// version、exe_path、sources，不写 config 与 overwritten；版本号与环境照常。
//
// 变异自证（实测）：删掉 Init 里的 applyProcess 调用、去掉 Init 里对 Config 的置空、applyAppInfo
// 改回无条件写 config、或去掉 a.Config == nil 时对两个配置 tag 的跳过，均红。
func TestInitProcessTagsAndOmitsConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ExplicitKeys["poll"] = true // 即便调用方误传了配置，子进程也不写
	ev := initScopeEvent(t, Options{Version: "9.9.9", IsRelease: true, Config: cfg, PlayerNames: []string{"wesing"}, Process: "audio-helper"})

	if got := ev.Tags["process"]; got != "audio-helper" {
		t.Errorf("tag process = %q，期望 \"audio-helper\"", got)
	}
	for _, tag := range []string{"config.customized", "config.effect_strategy"} {
		if v, ok := ev.Tags[tag]; ok {
			t.Errorf("子进程事件带了取自配置的 tag %s=%q：子进程不加载配置，会被读成「主播没动过配置」", tag, v)
		}
	}
	pc, ok := ev.Contexts["playercap"]
	if !ok {
		t.Fatal("没有 playercap context")
	}
	if got, want := sortedKeys(pc), []string{"exe_path", "sources", "version"}; !reflect.DeepEqual(got, want) {
		t.Errorf("playercap 的键 = %v，期望 %v（子进程不写 config 与 overwritten）", got, want)
	}
	if pc["version"] != "9.9.9" {
		t.Errorf("playercap.version = %v，期望 9.9.9", pc["version"])
	}
	opts := sentry.CurrentHub().Client().Options()
	if opts.Release != "9.9.9" || opts.Environment != envProduction {
		t.Errorf("Release=%q Environment=%q，期望 9.9.9、%s", opts.Release, opts.Environment, envProduction)
	}
}

// TestInitWithoutProcessMatchesMainService 是反例：主服务不设 Process，scope 与按引入 Process 之前的
// Init 逐步构造出的完全相同（黄金对比），playercap 上下文的五个键都在，没有 process 标签。
//
// 变异自证（实测）：applyAppInfo 改成恒不写 config、applyProcess 在空串时也打标签，均红。
func TestInitWithoutProcessMatchesMainService(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EffectStrategy = "park"
	cfg.ExplicitKeys["poll"] = true
	names := []string{"wesing", "kugou"}
	ev := initScopeEvent(t, Options{Version: "9.9.9", IsRelease: true, Config: cfg, PlayerNames: names})

	if _, ok := ev.Tags["process"]; ok {
		t.Errorf("主服务的事件带了 process 标签 %q", ev.Tags["process"])
	}
	if got, want := sortedKeys(ev.Contexts["playercap"]), []string{"config", "exe_path", "overwritten", "sources", "version"}; !reflect.DeepEqual(got, want) {
		t.Errorf("playercap 的键 = %v，期望 %v", got, want)
	}

	golden := sentry.NewScope()
	applyOSInfo(golden, collectOSInfo())
	applyAppInfo(golden, collectAppInfo("9.9.9", cfg, names))
	applyUserIP(golden)
	want := golden.ApplyToEvent(&sentry.Event{}, nil, nil)
	// trace 是 sentry 每个 scope 随机生成的关联 ID（见 noticeExempt），不参与比较。
	delete(ev.Contexts, "trace")
	delete(want.Contexts, "trace")
	if !reflect.DeepEqual(ev.Contexts, want.Contexts) {
		t.Errorf("contexts 与黄金不同：\n got: %v\nwant: %v", ev.Contexts, want.Contexts)
	}
	if !reflect.DeepEqual(ev.Tags, want.Tags) {
		t.Errorf("tags 与黄金不同：\n got: %v\nwant: %v", ev.Tags, want.Tags)
	}
	if !reflect.DeepEqual(ev.User, want.User) {
		t.Errorf("user 与黄金不同：\n got: %v\nwant: %v", ev.User, want.User)
	}
}

// TestApplyProcess 断言空串不设标签，非空原样设。
func TestApplyProcess(t *testing.T) {
	for _, c := range []struct {
		in      string
		wantSet bool
	}{{"", false}, {"audio-helper", true}, {"other", true}} {
		scope := sentry.NewScope()
		applyProcess(scope, c.in)
		ev := scope.ApplyToEvent(&sentry.Event{}, nil, nil)
		got, ok := ev.Tags["process"]
		if ok != c.wantSet || (ok && got != c.in) {
			t.Errorf("applyProcess(%q) 后 process = %q（存在=%v），期望存在=%v", c.in, got, ok, c.wantSet)
		}
	}
}
