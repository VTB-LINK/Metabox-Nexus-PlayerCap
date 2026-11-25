package qqmusic

// 本文件只测换歌后歌词补取的窗口与重试节流 backfillWindow（#227，doc/design/qqmusic-start-stall-design.md §6.10）：窗口内
// 每 500 ms 重试一次，窗口用尽时已判暂停则保持、未判暂停才放弃，arm 重开窗口并清掉节流。runSession 的接线（重试体只在
// case backfillRetry 下、判恢复时重设窗口）由 playstate_test.go 的源码扫描 k、l 守。回放夹具不复述补取窗口（补取按登记的
// 时刻送达），没有回放用例。
//
// 变异在一份分离的副本里逐条注入、跑完即还原，结果记在各用例的注释里（均实测）。

import "testing"

var stepName = map[backfillStep]string{backfillWait: "Wait", backfillRetry: "Retry", backfillHold: "Hold", backfillGiveUp: "GiveUp"}

// TestBackfillWindowNext 是设计 §6.10 的表驱动：arm 于 0，依次 next。截止 8000 含端点，节流 500 ms 含端点。
//
// 变异自证（均实测）：backfillHold 改为放弃（修复之前的行为）、窗口用尽而已判暂停仍返回 backfillRetry，8001 已判暂停一例
// 红；截止改为不含端点，8000 一例红；节流比较改为含端点（恰 500 ms 不重试），500 一例红。
func TestBackfillWindowNext(t *testing.T) {
	var w backfillWindow
	w.arm(jat(0))
	for _, c := range []struct {
		at     int
		paused bool
		want   backfillStep
	}{
		{0, false, backfillRetry},
		{499, false, backfillWait},
		{500, false, backfillRetry},
		{8000, false, backfillRetry}, // 恰为截止，距上一次 7500 ms
		{8001, true, backfillHold},
		{8001, false, backfillGiveUp},
	} {
		if got := w.next(jat(c.at), c.paused); got != c.want {
			t.Errorf("next(%d, 已判暂停=%v) = %s，期望 %s", c.at, c.paused, stepName[got], stepName[c.want])
		}
	}
}

// TestBackfillWindowRearmSoon：arm 于 0、next 于 0 重试；100 时再 arm，同一时刻 next 仍须重试（arm 清掉了 lastRetry）。
// 判恢复紧跟在一次重试之后的情形即此：不清的话，恢复后的第一次重试要晚到距上一次满 500 ms。
//
// 变异自证（均实测）：arm 不清 lastRetry，本例红（得 Wait），本文件的另两例不红（首次 arm 时本就为零，30000 再 arm 时
// 距上一次重试早已超过 500 ms）。
func TestBackfillWindowRearmSoon(t *testing.T) {
	var w backfillWindow
	w.arm(jat(0))
	if got := w.next(jat(0), false); got != backfillRetry {
		t.Fatalf("next(0) = %s，期望 Retry", stepName[got])
	}
	w.arm(jat(100))
	if got := w.next(jat(100), false); got != backfillRetry {
		t.Fatalf("100 再 arm 之后 next(100) = %s，期望 Retry", stepName[got])
	}
}

// TestBackfillWindowSequence：arm 于 0，每 31 ms 一拍，自 1000 起已判暂停，到 30000 为止：重试第一次在 0，相邻两次相隔
// 500～531 ms，最后一次不晚于 8000，此后一次也没有，也不放弃（暂停中用尽即保持，不在暂停中每 500 ms 做一次堆扫描）。
// 随后 arm(30000)（判恢复）、未判暂停：30000 那一拍重试，38000 之后的第一拍（38029）放弃，此前不放弃。
//
// 变异自证（均实测）：backfillHold 改为放弃（修复之前的行为），本例红（8029 放弃）；窗口用尽而已判暂停仍返回
// backfillRetry，本例红（8000 之后仍在重试）。
func TestBackfillWindowSequence(t *testing.T) {
	var w backfillWindow
	w.arm(jat(0))
	var retries []int
	for ms := 0; ms <= 30000; ms += tick {
		switch got := w.next(jat(ms), ms >= 1000); got {
		case backfillRetry:
			retries = append(retries, ms)
		case backfillGiveUp:
			t.Fatalf("%dms 放弃了补取：已判暂停时窗口用尽应保持", ms)
		}
	}
	if len(retries) == 0 || retries[0] != 0 || retries[len(retries)-1] > 8000 {
		t.Fatalf("重试的拍 %v：第一次应在 0，最后一次不晚于 8000", retries)
	}
	for i := 1; i < len(retries); i++ {
		if d := retries[i] - retries[i-1]; d < 500 || d > 500+tick {
			t.Fatalf("第 %d、%d 次重试相隔 %dms（%v），期望 500～%dms", i-1, i, d, retries, 500+tick)
		}
	}

	w.arm(jat(30000))
	for ms := 30000; ; ms += tick {
		got := w.next(jat(ms), false)
		if ms == 30000 && got != backfillRetry {
			t.Fatalf("arm(30000) 之后 30000 那一拍为 %s，期望 Retry", stepName[got])
		}
		if got == backfillGiveUp {
			if want := 30000 + (8000/tick+1)*tick; ms != want {
				t.Fatalf("放弃在 %dms，期望 38000 之后的第一拍 %dms", ms, want)
			}
			return
		}
		if ms > 40000 {
			t.Fatalf("到 %dms 仍未放弃", ms)
		}
	}
}
