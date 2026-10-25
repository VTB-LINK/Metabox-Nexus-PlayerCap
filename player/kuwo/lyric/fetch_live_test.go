package lyric

// 本文件是**真机联调**用例，默认跳过：只在 PLAYERCAP_LIVE=1 时跑（仓库惯例，见 AGENTS §9）。
//
// 它证明的不是解析逻辑（那在 codec_test.go / parse_test.go 里离线跑），而是
// 「我们自己拼的请求，服务端认」——即 XOR "yeelion" + base64 这一条链路在真机上成立。
// 走的是公网 HTTP，故不进 CI。
//
// 用法：PLAYERCAP_LIVE=1 go test ./player/kuwo/lyric -run TestFetchLive -v

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

var wordMarkRe = regexp.MustCompile(`<(-?\d+),(-?\d+)>`)

// wordMarkStrings 取出一行里所有逐字标记的原文（保持顺序与重复）。
func wordMarkStrings(s string) []string {
	return wordMarkRe.FindAllString(s, -1)
}

func TestFetchLive(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("跳过真机联调（PLAYERCAP_LIVE=1 才跑）")
	}
	req := Request{Song: "湮灭", Artist: "萨吉", RID: "658118183"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := NewClient().Fetch(ctx, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.Header["tp"] != "content" {
		t.Fatalf("tp=%q（应 content，多半是服务端换了协议）", resp.Header["tp"])
	}
	if !strings.Contains(resp.LRC, "[ti:湮灭]") || !strings.Contains(resp.LRC, "[ar:萨吉]") {
		t.Errorf("歌词头与请求的歌不一致：%q", firstLine(resp.LRC))
	}
	lines := ParseLRC(resp.LRC)
	if len(lines) < 5 {
		t.Fatalf("解析出的行数太少: %d", len(lines))
	}
	t.Logf("header=%v lines=%d first=%+v", resp.Header, len(lines), lines[0])
}

// TestLiveWordMarksCarryTime 是「行内 <a,b> 是时间还是布局」这条结论的**守门用例**。
//
// 判据：同一首歌里重复出现的同一段文字（副歌），标记数字必然不同。
// 布局只依赖文字——相同文字必须给出相同数字；数字不同就证明它带时间信息。
// 实测证据：你的背包（491280316）里「你的背包」出现四次，首字依次是
// <4000,-4000> / <2576,-2576> / <3640,-3640> / <3728,-3728>。
//
// 谁要是把注释或文档改回「像素坐标」，这个用例会红；它同时是校准单位时的锚。
//
// 用法：PLAYERCAP_LIVE=1 KUWO_RID=491280316 go test ./player/kuwo/lyric -run TestLiveWordMarks -v
func TestLiveWordMarksCarryTime(t *testing.T) {
	if os.Getenv("PLAYERCAP_LIVE") != "1" {
		t.Skip("跳过真机联调（PLAYERCAP_LIVE=1 才跑）")
	}
	rid := os.Getenv("KUWO_RID")
	if rid == "" {
		rid = "491280316" // 你的背包（2025 澳门站），副歌重复四次
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := NewClient().Fetch(ctx, Request{RID: rid})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// 行正文 → 该行所有标记的原文序列
	type occ struct {
		timeMs int
		marks  []string
	}
	byText := map[string][]occ{}
	for _, raw := range strings.Split(strings.ReplaceAll(resp.LRC, "\r\n", "\n"), "\n") {
		ts, rest, ok := cutTimestamp(strings.TrimSpace(raw))
		if !ok {
			continue
		}
		text := strings.TrimSpace(stripWordMarks(rest))
		if text == "" {
			continue
		}
		byText[text] = append(byText[text], occ{ts, wordMarkStrings(rest)})
	}

	pairs := 0
	for text, list := range byText {
		if len(list) < 2 || len(list[0].marks) == 0 {
			continue
		}
		pairs++
		same := true
		for i := 1; i < len(list); i++ {
			if strings.Join(list[i].marks, ",") != strings.Join(list[0].marks, ",") {
				same = false
				break
			}
		}
		if same {
			t.Errorf("%q 出现 %d 次、标记完全相同（%v）——那说明标记只依赖文字（布局），"+
				"与「带时间」的结论矛盾，注释与文档都要改回来", text, len(list), list[0].marks)
		} else {
			t.Logf("%q 出现 %d 次，标记不同 ✓（首次 %v … 末次 %v）",
				text, len(list), list[0].marks[:minInt(3, len(list[0].marks))], list[len(list)-1].marks[:minInt(3, len(list[len(list)-1].marks))])
		}
	}
	if pairs == 0 {
		t.Skipf("这首歌里没有带标记的重复行，无法判定（rid=%s，行数=%d）", rid, len(byText))
	}
	t.Logf("带标记的重复行 %d 组", pairs)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
