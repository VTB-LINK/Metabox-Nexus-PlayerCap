package qqmusic

// 本文件只测 extractSongMid：从一块内存里按时长（songPlayTime==durationMs）定位
// 当前歌的 songMid。这是「一错就把别的歌的 mid 安到当前歌、整首放错歌词」的路径。
//
// 抽成纯函数是为了可测：与 FindSongMid 的全内存扫描解耦，无需活的 QQ 音乐进程。
// 但**这不代表 CI 能跑**——qqmusic 整包是 Windows-only（mem.go 的 syscall.NewLazyDLL），
// GOOS=linux 整包编译不过，本测试只能靠本机 `go test ./...` 兜住（见 AGENTS.md §8 的表）。

import "testing"

// 取自 CE 实测的两块真实内存片段（v22.41 紧凑 JSON / 旧版带空格 JSON）。
const (
	// v22.41 播放上报，紧凑无空格：songid 恒 0，songmid 紧跟 songPlayTime。
	compactReport = `{"remainingTime":149234,"songPlayTime":189623,"songid":0,"songmid":"000S48Kb1DJdkJ","songtype":1,"speed":1}`
	// 旧版带空格（下载响应块那种风格）。
	prettyReport = `"remainingTime" : 182846,` + "\n" + `"songPlayTime" : 183000,` + "\n" + `"songmid" : "000mDR751jtpPf",`
)

func TestExtractSongMidCompact(t *testing.T) {
	got := extractSongMid([]byte(compactReport), 189623)
	if got != "000S48Kb1DJdkJ" {
		t.Fatalf("紧凑 JSON 未取到 songMid，got %q", got)
	}
}

func TestExtractSongMidPretty(t *testing.T) {
	got := extractSongMid([]byte(prettyReport), 183000)
	if got != "000mDR751jtpPf" {
		t.Fatalf("带空格 JSON 未取到 songMid，got %q", got)
	}
}

// 时长对不上 → 不返回。否则会把别的歌的 mid 当成当前歌的（整首放错歌词）。
func TestExtractSongMidWrongDuration(t *testing.T) {
	if got := extractSongMid([]byte(compactReport), 137223); got != "" {
		t.Fatalf("时长不匹配应返回空，got %q", got)
	}
}

// 前缀防误匹配：durationMs=18962 绝不能命中 "songPlayTime":189623 的前缀。
// 变异自证：删掉 extractSongMid 里「其后一字节非数字」的守卫，本例会误返回 mid。
func TestExtractSongMidPrefixNotMatched(t *testing.T) {
	if got := extractSongMid([]byte(compactReport), 18962); got != "" {
		t.Fatalf("18962 是 189623 的前缀，绝不该命中，got %q", got)
	}
}

// 邻近性：songPlayTime 命中、但附近没有 songmid（超出 ±500B 窗口）→ 不误取远处的 mid。
func TestExtractSongMidFarMidIgnored(t *testing.T) {
	var sb []byte
	sb = append(sb, []byte(`"songPlayTime":189623`)...)
	sb = append(sb, make([]byte, 800)...) // 拉开距离，超出 500B 窗口
	sb = append(sb, []byte(`"songmid":"000S48Kb1DJdkJ"`)...)
	if got := extractSongMid(sb, 189623); got != "" {
		t.Fatalf("songmid 超出邻近窗口应被忽略，got %q", got)
	}
}
