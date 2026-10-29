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

// TestUntoldDueIsPure due 是纯判断（#133）：为真时不动 sec、lastNotice；清零与限频时刻只由 noticed 更新。
//
// 变异自证：due 返回前清零（现状旧行为），本例的 sec 断言红；noticed 不清零或不记时刻，对应断言红（实测）。
func TestUntoldDueIsPure(t *testing.T) {
	var u untold
	u.add(0.25)
	if !u.due(syncT0) {
		t.Fatal("0.25s、距上次通知无限久：应为真")
	}
	if u.sec != 0.25 {
		t.Errorf("due 是纯判断，不该清零 sec，实得 %v", u.sec)
	}
	if !u.lastNotice.IsZero() {
		t.Error("due 是纯判断，不该更新 lastNotice")
	}
	u.noticed(syncT0)
	if u.sec != 0 || !u.lastNotice.Equal(syncT0) {
		t.Errorf("noticed 应把 sec 清零并记下时刻，实得 sec=%v lastNotice=%v", u.sec, u.lastNotice)
	}
}

// TestUntoldDroppedResumeRetained 校准 resume 被丢弃（不调 noticed）时未告知量保留，排空后下一拍仍 due，
// 补发成功才清零（#133）。
//
// 变异自证：若 due 自行清零（或无条件 noticed），丢弃后第二次 due 为假、永不补发，本例红。
func TestUntoldDroppedResumeRetained(t *testing.T) {
	var u untold
	u.add(0.25)
	if !u.due(syncT0) {
		t.Fatal("0.25s：第一次应 due")
	}
	// 模拟 emitPos 丢弃：不调 noticed。量保留。
	if u.sec != 0.25 {
		t.Errorf("丢弃后未告知量应仍为 0.25，实得 %v", u.sec)
	}
	if !u.due(secAt(5)) {
		t.Error("排空后仍达门槛、限频已过：应补发")
	}
	u.noticed(secAt(5)) // 这次成功入队
	if u.due(secAt(5.1)) {
		t.Error("补发成功已清零，不该再发")
	}
}

// TestUntoldClearedByNotice 通知成功入队即清零：同一笔改动只通知一次。
//
// 变异自证：noticed 不清零，本例红（满 5 秒时又报一次，实测）。
func TestUntoldClearedByNotice(t *testing.T) {
	var u untold
	u.add(0.4)
	if !u.due(syncT0) {
		t.Fatal("0.4s 应通知")
	}
	u.noticed(syncT0)
	if u.due(syncT0.Add(player.PositionSampleInterval)) {
		t.Error("通知之后未告知量应为 0，间隔满了也不该再通知")
	}
}

// TestUntoldAnchoredClearsNotLastNotice 非校准事件成功入队：anchored 清零 sec，但不动 lastNotice（限频只由
// 校准 resume 的 noticed 更新）。
//
// 变异自证：anchored 不清零 sec（空操作），sec 断言红（实测）。lastNotice 不受 anchored 影响是设计不变量
// （anchored 无时刻来源可改写），由末尾的 due 仍为真守住。
func TestUntoldAnchoredClearsNotLastNotice(t *testing.T) {
	var u untold
	u.add(0.4)
	u.anchored() // 例如一条 lyric_update 成功入队
	if u.sec != 0 {
		t.Errorf("anchored 应清零 sec，实得 %v", u.sec)
	}
	if !u.lastNotice.IsZero() {
		t.Error("anchored 不该动 lastNotice（限频只由校准 resume 更新）")
	}
	u.add(0.25)
	if !u.due(syncT0) {
		t.Error("lastNotice 仍为零、累计达门槛：应 due（anchored 不占限频）")
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
	u.noticed(syncT0)

	u.add(0.215)
	// 变异自证：去掉限频，这里红（实测）。
	for _, s := range []float64{0.1, 1, 2.5, 4.999} {
		if u.due(secAt(s)) {
			t.Fatalf("距上一次通知 %.3fs，不足 5 秒，不该通知", s)
		}
	}
	// 变异自证：顺延写成丢弃（间隔未到时清零）、间隔比较改为严格大于（恰满 5 秒仍不发），这里均红（实测）。
	if !u.due(secAt(5)) {
		t.Error("恰满 5 秒、未告知量仍在：应通知（顺延，不丢弃）")
	}
	u.noticed(secAt(5))
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
	u.noticed(syncT0)
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
	u.noticed(syncT0)
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

// TestNotifyCalibrationClearsOnlyOnEnqueue：notifyCalibration 只在校准 resume 成功入队后才清零未告知量、
// 更新限频时刻；被丢弃时两者都保留、不发事件，下一拍 due 仍为真、排空后补发（#133）。这条「成功才清零」
// 的判定在 runSession 里（无离线夹具），提取成 notifyCalibration 后在此直接守住。
//
// 变异自证：把 noticed 移到 `if emit` 之外（无条件调），本用例「丢弃后 sec 保留」红；
// due 自行清零，同样红。
func TestNotifyCalibrationClearsOnlyOnEnqueue(t *testing.T) {
	var u untold
	u.add(0.25) // 达门槛、lastNotice 为零值 → due 为真

	var emitted []string
	drop := true
	emit := func(typ string, _ interface{}) bool {
		if drop {
			return false // 通道满：丢弃，不入队、不 anchored
		}
		u.anchored() // 镜像 emitPos：成功入队时清零未告知量
		emitted = append(emitted, typ)
		return true
	}

	// 暂停时不发（isPlaying=false）。
	notifyCalibration(secAt(10), &u, false, 30, 100, emit)
	if len(emitted) != 0 || u.sec != 0.25 {
		t.Fatalf("暂停不该发校准通知：emitted=%v sec=%v", emitted, u.sec)
	}

	// 丢弃：不发、不 noticed，未告知量与限频时刻保留。
	drop = true
	notifyCalibration(secAt(10), &u, true, 30, 100, emit)
	if len(emitted) != 0 {
		t.Fatalf("丢弃时不该发事件，实得 %v", emitted)
	}
	if u.sec != 0.25 {
		t.Fatalf("丢弃后未告知量应保留 0.25，实得 %v", u.sec)
	}
	if !u.due(secAt(10)) {
		t.Fatal("丢弃后 due 应仍为真，下一拍补发")
	}

	// 成功：发一条 resume，anchored + noticed 清零、更新限频。
	drop = false
	notifyCalibration(secAt(11), &u, true, 30, 100, emit)
	if len(emitted) != 1 || emitted[0] != player.EventPlaybackResume {
		t.Fatalf("成功时应发一条 playback_resume，实得 %v", emitted)
	}
	if u.sec != 0 {
		t.Fatalf("成功入队后未告知量应清零，实得 %v", u.sec)
	}
}
