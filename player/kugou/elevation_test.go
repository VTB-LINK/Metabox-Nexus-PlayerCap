package kugou

// 本文件只测 elevationGate：UAC 被拒后的退避与放弃上限。
// 不设上限则 UAC 安全桌面每约 2 秒全屏夺焦一次，直播时是事故。

import (
	"testing"
	"time"
)

// TestElevationGateBacksOffThenGivesUp 钉死 UAC 被拒后的退避与上限。
//
// 为什么必须有上限：EnsurePatched 在弹 UAC 处被拒即返回，主循环随后 waitForCDP——
// 酷狗没在跑时它立即失败（不走 90s 兜底）——再 sleep 2s 就又进 EnsurePatched 弹下一次。
// 无上限即 UAC 安全桌面每约 2 秒全屏夺焦一次，直播时是事故。
//
// 变异自证：去掉 record() 里的 giveUp 判定即红。
func TestElevationGateBacksOffThenGivesUp(t *testing.T) {
	var g elevationGate

	d1, giveUp1 := g.record()
	if giveUp1 {
		t.Fatal("第 1 次提权失败不应立即放弃——用户可能只是误点了「否」")
	}
	if d1 != elevationBackoffs[0] {
		t.Errorf("第 1 次退避应为 %v，实得 %v", elevationBackoffs[0], d1)
	}

	d2, giveUp2 := g.record()
	if giveUp2 {
		t.Fatal("第 2 次提权失败不应放弃（退避表还没用尽）")
	}
	if d2 <= d1 {
		t.Errorf("退避必须递增，否则退化成固定间隔的反复夺焦：d1=%v d2=%v", d1, d2)
	}

	if _, giveUp3 := g.record(); !giveUp3 {
		t.Error("退避表用尽后必须终态放弃，否则 UAC 安全桌面会无限反复夺焦")
	}
}

// TestElevationGateResetOnSuccess 反方向：提权成功后计数必须清零。
//
// 没有这条，「record 永远返回 giveUp=true」这种一次失败就永久禁用酷狗的假修复能通过
// 上面那条；且长时间运行中偶发一次失败会永久消耗掉配额。
func TestElevationGateResetOnSuccess(t *testing.T) {
	var g elevationGate
	g.record()
	g.record()
	g.reset()

	d, giveUp := g.record()
	if giveUp {
		t.Fatal("reset 后应重新从第 1 次算起，不应立即放弃")
	}
	if d != elevationBackoffs[0] {
		t.Errorf("reset 后第 1 次退避应回到 %v，实得 %v", elevationBackoffs[0], d)
	}
}

// TestElevationBackoffsSane 守住退避表本身：必须非空、严格递增、且第一档足够长。
// 第一档太短就失去了意义——2s 那种间隔正是本缺陷的症状。
func TestElevationBackoffsSane(t *testing.T) {
	if len(elevationBackoffs) == 0 {
		t.Fatal("退避表不得为空，否则第 1 次失败就终态放弃")
	}
	if elevationBackoffs[0] < 10*time.Second {
		t.Errorf("首档退避 %v 过短，起不到避免反复夺焦的作用", elevationBackoffs[0])
	}
	for i := 1; i < len(elevationBackoffs); i++ {
		if elevationBackoffs[i] <= elevationBackoffs[i-1] {
			t.Errorf("退避表必须严格递增：[%d]=%v <= [%d]=%v", i, elevationBackoffs[i], i-1, elevationBackoffs[i-1])
		}
	}
}
