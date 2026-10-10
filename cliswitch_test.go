package main

// 本文件只测一件事：开关类命令行参数（-audio-normalize、-<player>-audio-normalize）的空格赋值写法经真实的
// config.Load 按字面生效，且不影响它后面的参数（issue #92）。
//
// 4.0 之前 `-audio-normalize false` 被解析为开，false 之后的参数全部不生效，也不报错。各例以 config.yml
// 文本与命令行参数跑真实的 Load（loadConfig），断言启动横幅的整行与 /service-status 的配置三件套。规整
// 实参的函数自身见 config/joinboolargs_test.go；多余的词报错退出见 cliexit_test.go。

import (
	"slices"
	"testing"
)

const normTail = " audio-normalize-target=-14dBFS audio-normalize-max-gain=30dB"

// TestSwitchTakesSpaceSeparatedValue 开关的三种写法（空格赋值、等号赋值、裸写）各按字面生效，写在最前、
// 中间、最后都一样；显式给出的键在横幅上带 *、进 config_overwritten。
//
// 变异自证（隔离的 worktree，实测）：
//   - Load 不规整实参、直接解析 os.Args[1:]，并去掉对剩余实参的检查（即改动前的行为）→ 标「空格」的
//     11 例红，标「反例」的 5 例仍绿。
//   - 只是不规整实参而保留检查；joinBoolArgs 不跳过取值型参数的取值；joinBoolArgs 把是不是开关写死成
//     全局的 audio-normalize 一个名字 → 开关后面的字面量成了多余的词，测试进程在 Load 里以 2 退出，
//     整包失败（这时没有逐例的失败行，标准错误里有那条报错与用法）。
func TestSwitchTakesSpaceSeparatedValue(t *testing.T) {
	cases := []struct {
		name string
		yml  string
		args []string
		want []string       // 横幅里必须原样出现的整行
		echo map[string]any // 键 → config 与 config_effective 里的取值；这些键都须在 config_overwritten 里
		not  []string       // 不得出现在 config_overwritten 里的键
	}{
		{name: "空格：false，其后的参数照常生效", args: []string{"-audio-normalize", "false", "-poll", "50"},
			want: []string{
				lineNorm + "*audio-normalize=false" + normTail,
				lineGlobal + "offset=-200ms *poll=50ms",
				"   播放器: wesing (offset=-200ms poll=50ms idle-hide=0s audio-normalize=false)"},
			echo: map[string]any{"audio-normalize": false, "poll": 50}},
		{name: "空格：开关写在最后", args: []string{"-poll", "50", "-audio-normalize", "false"},
			want: []string{
				lineNorm + "*audio-normalize=false" + normTail,
				lineGlobal + "offset=-200ms *poll=50ms"},
			echo: map[string]any{"audio-normalize": false, "poll": 50}},
		{name: "空格：开关写在中间，前后都是取值型参数",
			args: []string{"-offset", "-300", "-audio-normalize", "false", "-poll", "50", "-kugou-offset", "-430"},
			want: []string{
				lineNorm + "*audio-normalize=false" + normTail,
				lineGlobal + "*offset=-300ms *poll=50ms",
				"   播放器: kugou (*offset=-430ms poll=50ms idle-hide=0s audio-normalize=false)"},
			echo: map[string]any{"audio-normalize": false, "poll": 50, "offset": -300, "kugou-offset": -430}},
		{name: "空格：true 盖过 config.yml 的 false", yml: "audio-normalize: false\n", args: []string{"-audio-normalize", "true", "-poll", "50"},
			want: []string{
				lineNorm + "*audio-normalize=true" + normTail,
				lineGlobal + "offset=-200ms *poll=50ms"},
			echo: map[string]any{"audio-normalize": true, "poll": 50}},
		{name: "空格：每播放器的开关，全局为关", yml: "audio-normalize: false\n",
			args: []string{"-qqmusic-audio-normalize", "true", "-wesing-audio-normalize", "false"},
			want: []string{
				lineNorm + "*audio-normalize=false" + normTail,
				"   播放器: qqmusic (offset=-200ms poll=30ms idle-hide=0s *audio-normalize=true)",
				"   播放器: wesing (offset=-200ms poll=30ms idle-hide=0s *audio-normalize=false)",
				"   播放器: kugou (offset=-200ms poll=30ms idle-hide=0s audio-normalize=false)"},
			echo: map[string]any{"qqmusic-audio-normalize": true, "wesing-audio-normalize": false},
			not:  []string{"kugou-audio-normalize"}},
		{name: "空格：每播放器的开关，全局为开",
			args: []string{"-wesing-audio-normalize", "false", "-qqmusic-audio-normalize", "true", "-kugou-poll", "40"},
			want: []string{
				lineNorm + "audio-normalize=true" + normTail,
				"   播放器: wesing (offset=-200ms poll=30ms idle-hide=0s *audio-normalize=false)",
				"   播放器: qqmusic (offset=-200ms poll=30ms idle-hide=0s *audio-normalize=true)",
				"   播放器: kugou (offset=-200ms *poll=40ms idle-hide=0s audio-normalize=true)"},
			echo: map[string]any{"wesing-audio-normalize": false, "qqmusic-audio-normalize": true, "kugou-poll": 40},
			not:  []string{"audio-normalize"}},
		{name: "空格：0 与 1", args: []string{"-audio-normalize", "0", "-kugou-audio-normalize", "1", "-poll", "50"},
			want: []string{
				lineNorm + "*audio-normalize=false" + normTail,
				"   播放器: kugou (offset=-200ms poll=50ms idle-hide=0s *audio-normalize=true)",
				"   播放器: wesing (offset=-200ms poll=50ms idle-hide=0s audio-normalize=false)"},
			echo: map[string]any{"audio-normalize": false, "kugou-audio-normalize": true, "poll": 50}},
		{name: "空格：两个横线", args: []string{"--audio-normalize", "false", "--poll", "50"},
			want: []string{
				lineNorm + "*audio-normalize=false" + normTail,
				lineGlobal + "offset=-200ms *poll=50ms"},
			echo: map[string]any{"audio-normalize": false, "poll": 50}},
		{name: "空格：同一个开关写两次，以后一次为准", yml: "audio-normalize: false\n",
			args: []string{"-audio-normalize", "false", "-audio-normalize", "true", "-poll", "50"},
			want: []string{
				lineNorm + "*audio-normalize=true" + normTail,
				lineGlobal + "offset=-200ms *poll=50ms"},
			echo: map[string]any{"audio-normalize": true, "poll": 50}},
		{name: "空格：取值型参数的取值是布尔字面量", args: []string{"-addr", "true", "-audio-normalize", "false"},
			want: []string{
				lineListen + "*addr=true",
				lineNorm + "*audio-normalize=false" + normTail},
			echo: map[string]any{"addr": "true", "audio-normalize": false}},
		{name: "空格：空串取值", args: []string{"-prior-player", "", "-audio-normalize", "false"},
			want: []string{
				linePrior + "*prior-player=[] prior-player-expire=15s",
				lineNorm + "*audio-normalize=false" + normTail},
			echo: map[string]any{"audio-normalize": false}},

		{name: "反例：裸写仍为开", yml: "audio-normalize: false\nqqmusic-audio-normalize: false\n",
			args: []string{"-audio-normalize", "-qqmusic-audio-normalize"},
			want: []string{
				lineNorm + "*audio-normalize=true" + normTail,
				"   播放器: qqmusic (offset=-200ms poll=30ms idle-hide=0s *audio-normalize=true)"},
			echo: map[string]any{"audio-normalize": true, "qqmusic-audio-normalize": true}},
		{name: "反例：裸写后接别的参数", yml: "audio-normalize: false\n", args: []string{"-audio-normalize", "-poll", "50"},
			want: []string{
				lineNorm + "*audio-normalize=true" + normTail,
				lineGlobal + "offset=-200ms *poll=50ms"},
			echo: map[string]any{"audio-normalize": true, "poll": 50}},
		{name: "反例：裸写在最后", yml: "audio-normalize: false\n", args: []string{"-poll", "50", "-audio-normalize"},
			want: []string{
				lineNorm + "*audio-normalize=true" + normTail,
				lineGlobal + "offset=-200ms *poll=50ms"},
			echo: map[string]any{"audio-normalize": true, "poll": 50}},
		{name: "反例：等号写法", yml: "qqmusic-audio-normalize: false\n",
			args: []string{"-audio-normalize=false", "-qqmusic-audio-normalize=true", "-poll", "50"},
			want: []string{
				lineNorm + "*audio-normalize=false" + normTail,
				lineGlobal + "offset=-200ms *poll=50ms",
				"   播放器: qqmusic (offset=-200ms poll=50ms idle-hide=0s *audio-normalize=true)"},
			echo: map[string]any{"audio-normalize": false, "qqmusic-audio-normalize": true, "poll": 50}},
		{name: "反例：没传开关，不带标记", args: []string{"-poll", "50"},
			want: []string{
				lineNorm + "audio-normalize=true" + normTail,
				playerLine("wesing", "poll=50ms")},
			echo: map[string]any{"poll": 50},
			not:  []string{"audio-normalize", "wesing-audio-normalize"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadConfig(t, c.yml, c.args...)
			w, e, o := buildConfigEcho(&cfg, echoPlayers, effectiveEffectStrategy(cfg.EffectStrategy, false))
			wantBannerLines(t, bannerConfigLines(w, e, o, cfg.RejectedValues, echoPlayers), c.want...)
			for key, want := range c.echo {
				wv, _ := w.Get(key)
				ev, _ := e.Get(key)
				if wv != want || ev != want {
					t.Errorf("%s：config 为 %v、config_effective 为 %v，期望都是 %v", key, wv, ev, want)
				}
				if !slices.Contains(o, key) {
					t.Errorf("%s 不在 config_overwritten 里：%v", key, o)
				}
			}
			for _, key := range c.not {
				if slices.Contains(o, key) {
					t.Errorf("%s 不该在 config_overwritten 里：%v", key, o)
				}
			}
		})
	}
}
