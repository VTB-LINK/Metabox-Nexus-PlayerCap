//go:build windows

package procscan

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPlayerProcessMapping 守护「播放器名 ↔ 进程名」单一真源：Players 各家齐备、与 PlayerProcess
// 一一对应，且映射值锁死。若某 player 改名而漏改此处，literal 断言即失败。
// windows 门控：PlayerProcess/Players 依赖 windows-only 的 player 包常量；CI（linux）不跑此测试。
func TestPlayerProcessMapping(t *testing.T) {
	if len(Players) != 8 {
		t.Fatalf("len(Players) = %d, want 8", len(Players))
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
		"spotify":      "Spotify.exe",
		"kuwo":         "kwmusic.exe",
	}
	for player, proc := range want {
		if got := PlayerProcess[player]; got != proc {
			t.Errorf("PlayerProcess[%q] = %q, want %q", player, got, proc)
		}
	}
}

func TestPlayerProcesses(t *testing.T) {
	want := map[string][]string{
		"wesing":       {"WeSing.exe"},
		"cloudmusicv3": {"cloudmusic.exe"},
		"qqmusic":      {"qqmusic.exe"},
		"kugou":        {"HD_KuGou.exe", "KuGou.exe"},
		"sodamusic":    {"SodaMusic.exe"},
		"applemusic":   {"AMPLibraryAgent.exe"},
		"spotify":      {"Spotify.exe"},
		"kuwo":         {"kwmusic.exe"},
	}
	for player, expected := range want {
		got := PlayerProcesses(player)
		if !slices.Equal(got, expected) {
			t.Errorf("PlayerProcesses(%q) = %v, want %v", player, got, expected)
		}
	}

	// 返回切片修改隔离：外部改动返回值不得污染内部白名单。
	kg := PlayerProcesses("kugou")
	kg[0] = "tampered.exe"
	if got := PlayerProcesses("kugou"); !slices.Equal(got, []string{"HD_KuGou.exe", "KuGou.exe"}) {
		t.Fatalf("修改返回切片后 PlayerProcesses(\"kugou\") = %v，底层白名单被污染", got)
	}

	for _, unknown := range []string{"", "unknown"} {
		if got := PlayerProcesses(unknown); got != nil {
			t.Errorf("PlayerProcesses(%q) = %v, want nil", unknown, got)
		}
	}

	// 测试临时覆盖 PlayerProcess["kugou"] 时回退为单元素切片（兼容隔离测试桩）。
	orig := PlayerProcess["kugou"]
	t.Cleanup(func() { PlayerProcess["kugou"] = orig })
	PlayerProcess["kugou"] = "fake.exe"
	if got := PlayerProcesses("kugou"); !slices.Equal(got, []string{"fake.exe"}) {
		t.Fatalf("覆写后 PlayerProcesses(\"kugou\") = %v, want [fake.exe]", got)
	}
	PlayerProcess["kugou"] = ""
	if got := PlayerProcesses("kugou"); got != nil {
		t.Fatalf("清空后 PlayerProcesses(\"kugou\") = %v, want nil", got)
	}
}

func TestAliveAny(t *testing.T) {
	selfPID := uint32(os.Getpid())
	selfName := filepath.Base(os.Args[0])

	if !AliveAny(selfPID, "HD_KuGou.exe", selfName) {
		t.Fatalf("AliveAny(%d, HD_KuGou.exe, %q) = false, want true", selfPID, selfName)
	}
	if !AliveAny(selfPID, "", strings.ToUpper(selfName)) {
		t.Fatalf("AliveAny(%d, \"\", %q) = false, want true（应大小写不敏感且忽略空串）", selfPID, strings.ToUpper(selfName))
	}
	if AliveAny(selfPID, selfName+"2", "prefix_"+selfName) {
		t.Fatalf("AliveAny(%d, 近似名) = true, want false", selfPID)
	}
	if AliveAny(selfPID) || AliveAny(selfPID, "", "") {
		t.Fatalf("AliveAny(%d, 空名) = true, want false", selfPID)
	}
	if Alive(selfPID, "") {
		t.Fatalf("Alive(%d, \"\") = true, want false", selfPID)
	}
	if AliveAny(^uint32(0), selfName) {
		t.Fatalf("AliveAny(不存在PID, %q) = true, want false", selfName)
	}
}
