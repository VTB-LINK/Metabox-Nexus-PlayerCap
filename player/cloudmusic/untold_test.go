package cloudmusic

// 本文件只测 untold：未告知量的累计、清零、通知门槛与限频（顺延而不丢弃）。
// 不测 runSession 在哪些事件之后调 anchored（那由 AST 门禁 player/cloudmusicanchor_lint_test.go 守）。
// 时间一律由用例给定。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// TestUntoldThreshold 门槛：累计量的绝对值达到 0.2s 才该通知，向前与回拨同样对待，正负相消。
func TestUntoldThreshold(t *testing.T) {
	cases := []struct {
		name   string
		deltas []float32
		want   bool
	}{
		// 变异自证：due 恒返回 false（从不发），期望通知的四例均红；门槛比较改为严格大于，本例红（实测）。
		{"一次向前 0.2s：到门槛", []float32{0.2}, true},
		// 变异自证：去掉门槛（整段删掉），期望不通知的六例均红；改成有量就发（`u.sec == 0` 才不发），
		// 本例与另外三例（累计 0.15s、回拨 0.19s、只剩 0.1s）红（实测）。
		{"一次向前 0.19s：不到", []float32{0.19}, false},
		{"分两次累计到 0.25s", []float32{0.10, 0.15}, true},
		{"分两次累计 0.15s：不到", []float32{0.10, 0.05}, false},
		// 变异自证：只判向前（去掉 `u.sec <= -syncNotifySec`），回拨而期望通知的两例红（实测）。
		{"回拨 0.25s：到门槛", []float32{-0.25}, true},
		{"回拨 0.19s：不到", []float32{-0.19}, false},
		{"分两次回拨到 0.3s", []float32{-0.15, -0.15}, true},
		// 变异自证：改动量取绝对值累加，以下两例均红（实测）。
		{"向前 0.15s 再回拨 0.15s：相消", []float32{0.15, -0.15}, false},
		{"向前 0.3s 再回拨 0.2s：只剩 0.1s", []float32{0.30, -0.20}, false},
		{"没有校准", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var u untold
			for _, d := range c.deltas {
				u.add(d)
			}
			// 第一次通知无须等间隔。
			if got := u.due(syncT0); got != c.want {
				t.Errorf("due = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestUntoldClearedByAnchor 带位置的对外事件发出之后，此前的改动都已包含在它的位置里，不再通知。
//
// 变异自证：anchored 不清零，本例红（实测）。
func TestUntoldClearedByAnchor(t *testing.T) {
	var u untold
	u.add(0.4)
	u.anchored()
	if u.due(syncT0) {
		t.Error("anchored 之后未告知量应为 0，不该通知")
	}
	u.add(0.1)
	if u.due(secAt(1)) {
		t.Error("anchored 之后只累计了 0.1s，不该通知（此前的 0.4s 不应留着）")
	}
}

// TestUntoldClearedByNotice 通知发出即清零：同一笔改动只通知一次。
//
// 变异自证：due 为真之后不清零，本例红（满 5 秒时又报一次，实测）。
func TestUntoldClearedByNotice(t *testing.T) {
	var u untold
	u.add(0.4)
	if !u.due(syncT0) {
		t.Fatal("0.4s 应通知")
	}
	if u.due(syncT0.Add(player.PositionSampleInterval)) {
		t.Error("通知之后未告知量应为 0，间隔满了也不该再通知")
	}
}

// TestUntoldRateLimitDefers 限频：两次通知之间至少隔 PositionSampleInterval；间隔未到时顺延，
// 量保留，间隔一到、仍达门槛就发。
func TestUntoldRateLimitDefers(t *testing.T) {
	var u untold
	u.add(0.4)
	if !u.due(syncT0) {
		t.Fatal("第一次通知无须等间隔")
	}

	u.add(0.215)
	// 变异自证：去掉限频，这里红（实测）。
	for _, s := range []float64{0.1, 1, 2.5, 4.999} {
		if u.due(secAt(s)) {
			t.Fatalf("距上一次通知 %.3fs，不足 5 秒，不该通知", s)
		}
	}
	// 变异自证：顺延写成丢弃（间隔未到时清零）、每次检查都记时刻（间隔被不断推后）、间隔比较改为
	// 含等号（恰满 5 秒仍不发），这里均红（实测）。
	if !u.due(secAt(5)) {
		t.Error("恰满 5 秒、未告知量仍在：应通知（顺延，不丢弃）")
	}
	if u.due(secAt(5.1)) {
		t.Error("刚通知过，量已清零")
	}
}

// TestUntoldDeferredThenAnchored 顺延期间有带位置的对外事件发出：前端已在后端的时间线上，
// 间隔到了也不再补发。
//
// 变异自证：anchored 不清零，本例红（实测）。
func TestUntoldDeferredThenAnchored(t *testing.T) {
	var u untold
	u.add(0.4)
	if !u.due(syncT0) {
		t.Fatal("第一次通知无须等间隔")
	}
	u.add(0.3)
	if u.due(secAt(1)) {
		t.Fatal("不足 5 秒，不该通知")
	}
	u.anchored() // 例如这时来了一条 lyric_update
	if u.due(secAt(5)) {
		t.Error("顺延期间已被带位置的事件清零，间隔到了也不该补发")
	}
}

// TestUntoldAccumulatesWhileDeferred 顺延期间照常累计：到点时按当时的累计量判。
func TestUntoldAccumulatesWhileDeferred(t *testing.T) {
	var u untold
	u.add(0.4)
	if !u.due(syncT0) {
		t.Fatal("第一次通知无须等间隔")
	}
	u.add(0.25)
	if u.due(secAt(1)) {
		t.Fatal("不足 5 秒，不该通知")
	}
	u.add(-0.15) // 其后的回拨把累计量带回门槛以内
	if u.due(secAt(5)) {
		t.Error("到点时累计量只剩 0.1s，不该通知")
	}
	u.add(0.15)
	if !u.due(secAt(5.1)) {
		t.Error("累计量回到 0.25s 且早已满 5 秒：应通知")
	}
}
