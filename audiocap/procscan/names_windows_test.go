//go:build windows

package procscan

import "testing"

// TestPlayerProcessMapping 守护「播放器名 ↔ 进程名」单一真源：Players 各家齐备、与 PlayerProcess
// 一一对应，且映射值锁死。若某 player 改名而漏改此处，literal 断言即失败。
// windows 门控：PlayerProcess/Players 依赖 windows-only 的 player 包常量；CI（linux）不跑此测试。
func TestPlayerProcessMapping(t *testing.T) {
	if len(Players) != 6 {
		t.Fatalf("len(Players) = %d, want 6", len(Players))
	}
	if len(PlayerProcess) != len(Players) {
		t.Errorf("PlayerProcess 有 %d 项，Players 有 %d 项，应一一对应", len(PlayerProcess), len(Players))
	}
	for _, p := range Players {
		if _, ok := PlayerProcess[p]; !ok {
			t.Errorf("Players 中的 %q 在 PlayerProcess 无映射", p)
		}
	}

	want := map[string]string{
		"wesing":       "WeSing.exe",
		"cloudmusicv3": "cloudmusic.exe",
		"qqmusic":      "qqmusic.exe",
		"kugou":        "KuGou.exe",
		"sodamusic":    "SodaMusic.exe",
		"applemusic":   "AMPLibraryAgent.exe",
	}
	for player, proc := range want {
		if got := PlayerProcess[player]; got != proc {
			t.Errorf("PlayerProcess[%q] = %q, want %q", player, got, proc)
		}
	}
}
