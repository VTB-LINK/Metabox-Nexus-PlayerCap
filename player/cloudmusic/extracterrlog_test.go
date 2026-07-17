package cloudmusic

// 本文件只测 extractErrLog 的降噪状态机：首次打、之后压制、每 60s 复读一次、成功后重置。
// 不测 runSession 的控制流（它需要真 CDP 连接）、不测 Extract 本身。

import (
	"testing"
	"time"
)

// TestExtractErrLogFirstAlwaysLogs 首次出现的错误必须打 —— 不能彻底静默。
//
// 日志走 stderr 且不落盘（AGENTS §0），它是直播出问题时唯一的现场证据。
// 「一次都不打」和「10 行/秒」一样坏。
func TestExtractErrLogFirstAlwaysLogs(t *testing.T) {
	var e extractErrLog
	t0 := time.Now()

	should, repeat, _ := e.next("boom", t0)
	if !should {
		t.Error("首次出现的错误必须打日志")
	}
	if repeat {
		t.Error("首次不该标成「复读」")
	}
}

// TestExtractErrLogSuppressesFlood 钉死本条核心：同一错误在窗口内必须被压制。
//
// 缺陷背景：原实现每 tick 打一次。poll 默认 30 被 `<50ms→100ms` 抬到 100ms → **10 行/秒**，
// 几分钟就把控制台回滚缓冲冲干净，连带冲掉另外三个播放器的全部日志。
//
// 变异自证：把 next 改成恒返回 true 即红。
func TestExtractErrLogSuppressesFlood(t *testing.T) {
	var e extractErrLog
	t0 := time.Now()
	e.next("boom", t0) // 首次

	logged := 0
	// 模拟 100ms 一 tick 跑满 59 秒（真实刷屏速率）
	for i := 1; i <= 590; i++ {
		if should, _, _ := e.next("boom", t0.Add(time.Duration(i)*100*time.Millisecond)); should {
			logged++
		}
	}
	if logged != 0 {
		t.Errorf("60s 窗口内同一错误应全部压制，实得打了 %d 次（原实现是 590 次 = 10 行/秒）", logged)
	}
}

// TestExtractErrLogRepeatsAfterInterval 反方向：不能永久静默，必须周期性复读。
//
// 没有这条，「首次之后永远压制」这种把现场证据彻底掐掉的假修复能通过上面那条。
func TestExtractErrLogRepeatsAfterInterval(t *testing.T) {
	var e extractErrLog
	t0 := time.Now()
	e.next("boom", t0)

	// 恰好到 60s
	should, repeat, dur := e.next("boom", t0.Add(extractErrRepeat))
	if !should {
		t.Fatal("持续 60s 后必须复读一次（stderr 是唯一现场证据）")
	}
	if !repeat {
		t.Error("复读必须标成 repeat，好让日志措辞不同于首次")
	}
	if dur != extractErrRepeat {
		t.Errorf("已持续时长应为 %v，实得 %v", extractErrRepeat, dur)
	}

	// 复读后再次进入压制窗口
	if should, _, _ := e.next("boom", t0.Add(extractErrRepeat+time.Second)); should {
		t.Error("复读后应重新压制，不能变成每 tick 打")
	}

	// 第二次复读：dur 应从**首次故障**起算，不是从上次复读起算
	_, _, dur2 := e.next("boom", t0.Add(2*extractErrRepeat))
	if dur2 != 2*extractErrRepeat {
		t.Errorf("已持续时长应从首次故障起算（%v），实得 %v —— 说明 since 被复读覆盖了",
			2*extractErrRepeat, dur2)
	}
}

// TestExtractErrLogNewErrorLogsImmediately 错误串变了 = 新故障，必须立刻打，不受窗口压制。
// 否则「store not found」变成「connection refused」这种关键转折会被吃掉。
func TestExtractErrLogNewErrorLogsImmediately(t *testing.T) {
	var e extractErrLog
	t0 := time.Now()
	e.next("boom", t0)

	should, repeat, _ := e.next("different", t0.Add(time.Millisecond))
	if !should {
		t.Error("错误串变了即新故障，必须立刻打，不受上一条的窗口压制")
	}
	if repeat {
		t.Error("新错误不是复读")
	}
}

// TestExtractErrLogResetAfterSuccess 取词成功后重置：下次故障可再记一次。
//
// 这条守的是「一次成功之后的新故障不能被旧状态吃掉」—— 网易云短暂抽风又恢复、随后真坏了，
// 那次真坏必须有日志。
func TestExtractErrLogResetAfterSuccess(t *testing.T) {
	var e extractErrLog
	t0 := time.Now()
	e.next("boom", t0)

	e.reset() // Extract 成功

	should, repeat, _ := e.next("boom", t0.Add(time.Millisecond))
	if !should {
		t.Error("成功后重置，同一错误再次出现必须重新打一次")
	}
	if repeat {
		t.Error("重置后再出现算首次，不是复读")
	}
}
