package lyric

// 本文件只测 findBestYRCDetail 的平局判据：时间差相等时必须确定地选同一个条目。
// 不测 YRC 解析本身、不测贪心抢占（那是已知且有意保留的算法特性，见下）、
// 不测容差取值（toleranceMs=3000 是 366ea00 实测调过的，别动）。

import (
	"fmt"
	"testing"
)

// yrcTwoWords 造一条 YRC：给定起始毫秒 + 文本，两个字。
func yrcTwoWords(startMs int, a, b string) string {
	return fmt.Sprintf("[%d,1000](%d,500,0)%s(%d,500,0)%s\n", startMs, startMs, a, startMs+500, b)
}

// TestFindBestYRCDetailTieIsDeterministic 钉死平局的确定性。
//
// 缺陷背景：findBestYRCDetail 用 `for yrcTimeMs, detail := range details` 遍历 —— map 序
// 随机 —— 而判据是严格小于（`diff < bestDiff`），于是时间差**相等**时保留的是「先遇到的
// 那个」，也就是随机的那个。同一份输入每次跑可能挑中不同条目，逐字高亮层随机错位，
// 且故障不可复现。实测同一输入 200 次得 10:183 / 14:17 的分布。
//
// 这里构造严格平局：LRC 行在 12.0s，两个同文本 YRC 条目分别在 10.0s 与 14.0s，
// 时间差都是 2000ms。加了 tie-break 后必须恒选时间戳小的（10.0s）。
//
// 变异自证：去掉 `|| (diff == bestDiff && yrcTimeMs < bestKey)` 即红。
func TestFindBestYRCDetailTieIsDeterministic(t *testing.T) {
	// 两个候选与 12.0s 的距离相等 —— 严格平局
	yrc := yrcTwoWords(10000, "同", "词") + yrcTwoWords(14000, "同", "词")

	for i := range 100 {
		lyrics := []LyricLine{{Time: 12.0, Text: "同词"}}
		MergeYRC(lyrics, yrc, 0)

		w := lyrics[0].TextDetailed.Words
		if len(w) != 2 {
			t.Fatalf("第 %d 次：应匹配到一个两字条目，实得 %d 个字", i, len(w))
		}
		// 平局取时间戳小的 → 10.0s 那条
		if got := w[0].Timestamp; got != 10.0 {
			t.Fatalf("第 %d 次：平局应恒取时间戳小的条目（10.0s），实得 %.2f —— map 序在决定结果",
				i, got)
		}
	}
}

// TestFindBestYRCDetailPrefersNearest 反方向回归：别把主判据改坏了。
// 没有这条，「永远取时间戳最小的」这种假修复能通过上面那条。
func TestFindBestYRCDetailPrefersNearest(t *testing.T) {
	// 10.0s 距离 2000ms，13.0s 距离 1000ms —— 不是平局，必须取更近的 13.0s
	yrc := yrcTwoWords(10000, "同", "词") + yrcTwoWords(13000, "同", "词")

	lyrics := []LyricLine{{Time: 12.0, Text: "同词"}}
	MergeYRC(lyrics, yrc, 0)

	if got := lyrics[0].TextDetailed.Words[0].Timestamp; got != 13.0 {
		t.Errorf("非平局时必须取时间差最小的（13.0s，差 1000ms），实得 %.2f", got)
	}
}

// TestFindBestYRCDetailToleranceUnchanged 容差边界不得被 tie-break 改动。
//
// toleranceMs=3000 是 366ea00 从 1250 实测调上来的（同 commit 还加了 65 行漂移容忍测试），
// 且现有绿色用例里存在 2470ms 的**合法**漂移 —— 容差确实需要这么宽，别收紧。
func TestFindBestYRCDetailToleranceUnchanged(t *testing.T) {
	// 差 2900ms：在容差内，应匹配上
	lyrics := []LyricLine{{Time: 12.0, Text: "同词"}}
	MergeYRC(lyrics, yrcTwoWords(14900, "同", "词"), 0)
	if len(lyrics[0].TextDetailed.Words) == 0 {
		t.Error("差 2900ms 在 3000ms 容差内，应匹配上")
	}

	// 差 3100ms：超出容差，不该匹配
	lyrics = []LyricLine{{Time: 12.0, Text: "同词"}}
	MergeYRC(lyrics, yrcTwoWords(15100, "同", "词"), 0)
	if len(lyrics[0].TextDetailed.Words) != 0 {
		t.Error("差 3100ms 超出 3000ms 容差，不该匹配")
	}
}

// TestMergeYRCGreedyIsUnchanged 钉死「贪心抢占」**仍然存在** —— 这不是遗漏，是有意保留。
//
// 三行同文本、YRC 只有后两条时，line0 会抢走 12.0 那条、line1 抢走 14.0、line2 落空。
// 全局最优匹配能修好它，但笔记的判断是「别改算法」，理由经核实成立：不崩不 panic，
// 行级歌词仍按正确时刻上屏，损伤仅限逐字高亮层；触发需四条件合取（有 YRC + 完全同文本
// 重复行 + 间距 < 3s + YRC 恰好漏一条）；而「常见」这个断言没有真实歌曲样本佐证。
// 改的是直播主路径、收益未证 —— 按「稳定 > 正确」不动。
//
// 本用例的作用是**记录现状**：哪天有人真要上全局最优匹配，这条会红，那时应当**更新它**
// 而不是删掉它 —— 它标记的是一个已知的、有意的取舍。
func TestMergeYRCGreedyIsUnchanged(t *testing.T) {
	yrc := yrcTwoWords(12000, "同", "词") + yrcTwoWords(14000, "同", "词")
	lyrics := []LyricLine{
		{Time: 10.0, Text: "同词"},
		{Time: 12.0, Text: "同词"},
		{Time: 14.0, Text: "同词"},
	}
	MergeYRC(lyrics, yrc, 0)

	if got := len(lyrics[2].TextDetailed.Words); got != 0 {
		t.Errorf("贪心抢占是当前有意保留的行为：末行应落空（实得 %d 个字）。"+
			"若这里变绿说明算法被改成了全局最优匹配 —— 那是好事，但请连同本用例一起更新，"+
			"并确认 doc/hardening-notes-2026-07.md 的 §8a 判断已重新评估", got)
	}
	// 前两行确实抢到了（证明落空不是因为整个没匹配上）
	if len(lyrics[0].TextDetailed.Words) == 0 || len(lyrics[1].TextDetailed.Words) == 0 {
		t.Error("前置条件：前两行应各抢到一条")
	}
}
