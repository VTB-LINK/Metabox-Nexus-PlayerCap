package config

// 本文件只测一件事：prior-player 里出现未知播放器名时，必须有一条可见告警。
//
// 它守的是**静默失效**，不是功能正确性。未知名字本身无害——Router 会给它建一个永远收不到
// 事件的状态条目，恒 idle，不阻塞也不抢主输出。有害的是用户把 sodamusic 拼成 sodamusci 之后，
// 配置看着生效了、日志一个字都没有，而优先播放器实际从未生效；排查时几乎不会怀疑到拼写。
//
// 能做成门禁是因为 logger 走的是 stdlib log 的默认 logger，log.SetOutput 可以捕获。

import (
	"bytes"
	stdlog "log"
	"strings"
	"testing"
)

// captureLog 捕获一段代码里经 logger 打出的内容。
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldFlags := stdlog.Flags()
	stdlog.SetOutput(&buf)
	stdlog.SetFlags(0)
	defer func() {
		stdlog.SetOutput(nil) // 交回给 testing 的默认行为
		stdlog.SetFlags(oldFlags)
	}()
	fn()
	return buf.String()
}

// registerForTest 往包级注册表塞入测试用的播放器名。
//
// registeredPlayers 是包级全局、且 RegisterPlayer 只增不减（生产里由各播放器包 init 调用一次）。
// 测试里没有清理接口，故本文件只**追加**独有的名字、断言也只针对它们，不依赖注册表为空，
// 这样与同包其它测试的执行顺序无关。
func registerForTest(names ...string) {
	for _, n := range names {
		RegisterPlayer(n)
	}
}

func TestPriorPlayerWarnsOnUnknownName(t *testing.T) {
	registerForTest("testplayer-alpha", "testplayer-beta")

	cases := []struct {
		name      string
		prior     []string
		wantWarn  bool
		wantNamed string
	}{
		{"已知播放器不告警", []string{"testplayer-alpha"}, false, ""},
		{"多个已知播放器不告警", []string{"testplayer-alpha", "testplayer-beta"}, false, ""},
		{"不配优先播放器不告警", nil, false, ""},
		{"未知名字必须告警且点名", []string{"testplayer-alpah"}, true, "testplayer-alpah"},
		{"已知与未知混排，只对未知告警", []string{"testplayer-alpha", "testplayer-gamma"}, true, "testplayer-gamma"},
	}

	for _, c := range cases {
		cfg := Config{PriorPlayer: c.prior}
		out := captureLog(t, func() { warnUnknownPriorPlayers(&cfg) })
		got := strings.Contains(out, "prior-player")

		if got != c.wantWarn {
			t.Fatalf("%s：告警=%v want %v；实际输出 %q", c.name, got, c.wantWarn, out)
		}
		if c.wantWarn && !strings.Contains(out, c.wantNamed) {
			t.Fatalf("%s：告警里必须点名 %q，实际输出 %q", c.name, c.wantNamed, out)
		}
		// 只对未知的那个告警：已知名字不该被点名。
		if c.name == "已知与未知混排，只对未知告警" && strings.Contains(out, "%q") {
			t.Fatalf("%s：格式串未展开，实际输出 %q", c.name, out)
		}
	}
}

// warnUnknownPriorPlayers 绝不能改变配置——它只告警，不过滤。
// 变异自证：在函数里加一句「未知项从 cfg.PriorPlayer 里删掉」，本测试当场红。
func TestPriorPlayerWarnDoesNotMutateConfig(t *testing.T) {
	registerForTest("testplayer-alpha")
	cfg := Config{PriorPlayer: []string{"testplayer-alpha", "testplayer-unknown"}}
	captureLog(t, func() { warnUnknownPriorPlayers(&cfg) })

	if len(cfg.PriorPlayer) != 2 ||
		cfg.PriorPlayer[0] != "testplayer-alpha" || cfg.PriorPlayer[1] != "testplayer-unknown" {
		t.Fatalf("PriorPlayer 被改动了：%v —— 本函数只允许告警，过滤会让 Router 的分组与配置不一致", cfg.PriorPlayer)
	}
}
