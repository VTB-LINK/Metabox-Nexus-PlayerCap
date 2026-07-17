package config

// 本文件只测轮询间隔的钳制：全局与 per-player 覆盖必须走同一条边界。
// 不测 mergeYAML 的解析、不测 config.yml 的路径解析、不测 offset。
//
// 为什么手搓 Config 而不调 Load()：Load() 往全局 flag.CommandLine 注册 flag，
// 二次调用会 panic（flag redefined: poll），表驱动测试没法反复调它。

import "testing"

func pollPtr(v int) *int { return &v }

// TestClampPollsAppliesToPerPlayer 钉死本条核心：**per-player 覆盖必须被钳制**。
//
// 缺陷背景：钳制原本只作用于全局 cfg.Poll，而 GetPlayerPoll 命中 per-player 覆盖时
// 原样返回 *pc.Poll，main.go 直接把它喂进各播放器的 New()。真正会咬人的是 poll <= 0：
// time.Sleep(<=0) 立即返回 → 主循环无界忙等；wesing 还会每 33 圈自旋一次
// CreateToolhelp32Snapshot（全进程表快照）+ EnumWindows。而 kugou 完全没有自保
// （kugou.go:295 的 pollMs 直通到 :360/:557 的 time.Sleep），这条钳制是它唯一的保护。
//
// **这个用例必须断言 map 里的值真被改写**，不能只测 clampPoll(0)==10 —— 那样把
// clampPolls 里的 per-player 循环整个删掉，测试照样绿（实测过，见 commit 记录）。
func TestClampPollsAppliesToPerPlayer(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"忙等：Sleep(0) 立即返回，主循环空转", 0, pollMin},
		{"负值：同样是忙等", -5, pollMin},
		{"1ms 热循环（AGENTS.md 点名的那个）", 1, pollMin},
		{"过大：超出上界", 99999, pollMax},
		{"边界内不动", 500, 500},
		{"恰好下界", pollMin, pollMin},
		{"恰好上界", pollMax, pollMax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{
				Poll:    30,
				Players: map[string]*PlayerConfig{"kugou": {Poll: pollPtr(c.in)}},
			}
			clampPolls(&cfg)

			got := cfg.Players["kugou"].Poll
			if got == nil {
				t.Fatal("Poll 指针被清成 nil —— 覆盖丢失，会静默回退到全局值")
			}
			if *got != c.want {
				t.Errorf("kugou-poll=%d 应钳制为 %d，实得 %d —— 这个值会被 GetPlayerPoll 原样喂进播放器 New()",
					c.in, c.want, *got)
			}
		})
	}
}

// TestClampPollsAppliesToGlobal 回归：全局那半边本来就是对的，别改坏了。
func TestClampPollsAppliesToGlobal(t *testing.T) {
	cfg := Config{Poll: 1, Players: map[string]*PlayerConfig{}}
	clampPolls(&cfg)
	if cfg.Poll != pollMin {
		t.Errorf("全局 poll=1 应钳制为 %d，实得 %d", pollMin, cfg.Poll)
	}

	cfg = Config{Poll: 99999, Players: map[string]*PlayerConfig{}}
	clampPolls(&cfg)
	if cfg.Poll != pollMax {
		t.Errorf("全局 poll=99999 应钳制为 %d，实得 %d", pollMax, cfg.Poll)
	}
}

// TestClampPollsKeepsNilOverride nil 覆盖必须保持 nil。
//
// AGENTS.md §3.3：PlayerConfig.Offset/Poll 必须保持 *int —— nil（未设置，沿用全局）
// 与 0（显式设成 0）必须可区分。钳制若把 nil 实体化成 pollMin，「沿用全局」的语义就没了：
// 用户没写 kugou-poll 却会得到 10 而不是全局值。
func TestClampPollsKeepsNilOverride(t *testing.T) {
	cfg := Config{
		Poll: 30,
		Players: map[string]*PlayerConfig{
			"kugou":  {Poll: nil, Offset: nil},
			"wesing": {Poll: pollPtr(5)},
		},
	}
	clampPolls(&cfg)

	if cfg.Players["kugou"].Poll != nil {
		t.Errorf("nil 覆盖被实体化成 %d —— 「沿用全局」的语义丢了", *cfg.Players["kugou"].Poll)
	}
	// 同一次调用里，非 nil 的那个仍要被钳制（证明不是靠「整个跳过」蒙混过关）
	if got := cfg.Players["wesing"].Poll; got == nil || *got != pollMin {
		t.Errorf("同一次调用里 wesing-poll=5 仍应被钳制为 %d", pollMin)
	}
}

// TestClampPollsLeavesOffsetAlone 钳制不得碰 Offset —— 它有完全不同的合法范围
// （wesing-offset: 0 是模板里的合法示例，负值也合法）。
func TestClampPollsLeavesOffsetAlone(t *testing.T) {
	cfg := Config{
		Poll: 30,
		Players: map[string]*PlayerConfig{
			"wesing": {Offset: pollPtr(0), Poll: pollPtr(5)},
			"kugou":  {Offset: pollPtr(-100)},
		},
	}
	clampPolls(&cfg)

	if got := cfg.Players["wesing"].Offset; got == nil || *got != 0 {
		t.Errorf("wesing-offset: 0 是合法配置，不得被钳制，实得 %v", got)
	}
	if got := cfg.Players["kugou"].Offset; got == nil || *got != -100 {
		t.Errorf("offset 负值合法，不得被钳制，实得 %v", got)
	}
}

// TestClampPollsHandlesNilPlayerConfig 防御：Players 里出现 nil 值不得 panic。
func TestClampPollsHandlesNilPlayerConfig(t *testing.T) {
	cfg := Config{Poll: 30, Players: map[string]*PlayerConfig{"ghost": nil}}
	clampPolls(&cfg) // 不 panic 即通过
}
