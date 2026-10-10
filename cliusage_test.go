package main

// 本文件只测一件事：-h 经真实的 config.Load 输出的用法里，开关类参数带取值提示 [true|false]，其余参数
// 的那一项照旧（issue #92）。-h 在 Load 里退出，同 cliexit_test.go 在子进程里跑（runLoad）。一项的格式
// 逐类见 config/flagusage_test.go。

import (
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/config"
)

// TestHelpShowsSwitchValueHint -h 以 0 退出、Load 不返回；带提示的恰是全局开关与每个注册的播放器的开关，
// 取值型参数的各项与改动前相同。
//
// 变异自证（隔离的 worktree，实测）：flagUsage 不加提示 → 红；给所有参数都加提示 → 红；按名字判断而漏掉
// 每播放器的开关（只认 audio-normalize 一个名字）→ 红。
func TestHelpShowsSwitchValueHint(t *testing.T) {
	code, stdout, stderr := runLoad(t, "-h")
	usage := usageOf(stderr)
	if code != 0 || stdout != "" || usage == "" {
		t.Fatalf("-h：退出码 %d，stdout %q，stderr:\n%s", code, stdout, stderr)
	}

	want := []string{"  -audio-normalize [true|false]\n    \t音量归一化开关\n"}
	players := config.RegisteredPlayers()
	for _, p := range players {
		want = append(want, "  -"+p+"-audio-normalize [true|false]\n    \t"+p+" 音量归一化开关（不传=跟随全局）\n")
	}
	// 取值型参数的几项，与加提示之前相同：加提示前后各编一份测试二进制，两份 -h 的输出逐行比对过，
	// 70 行里只有 7 个开关的那一行不同（实测）。
	want = append(want,
		"  -addr string\n    \tWebSocket 监听地址\n",
		"  -offset int\n    \t歌词延迟（毫秒），正值=延后，负值=提前\n",
		"  -audio-normalize-target int\n    \t音量归一化目标响度（dBFS），范围 [-60,0]\n",
		"  -prior-player string\n    \t优先播放器列表，逗号分隔（如 wesing,kugou）；传空串=无优先播放器\n",
		"  -wesing-idle-hide int\n    \twesing 无活跃自动隐藏（秒，0=关，不传=跟随全局）\n",
		"  -kugou-offset int\n    \tkugou 歌词延迟（毫秒），正值=延后，负值=提前\n",
	)
	for _, w := range want {
		if !strings.Contains(usage, w) {
			t.Errorf("用法里缺这一项：\n%s", w)
		}
	}
	if len(players) == 0 {
		t.Error("没有注册的播放器：每播放器的开关没有被检查到")
	}
	if got := strings.Count(usage, "[true|false]"); got != 1+len(players) {
		t.Errorf("带取值提示的有 %d 项，期望 %d 项（全局开关与 %d 个播放器的开关）:\n%s", got, 1+len(players), len(players), usage)
	}
}
