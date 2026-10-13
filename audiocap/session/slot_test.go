package session

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/frame"
)

// 本文件只测槽位 Slot：已发布的结果到帧标志的映射，以及存活期与从未发布。

const (
	known = frame.FlagSessionKnown
	both  = frame.FlagSessionKnown | frame.FlagSessionActive
)

// 变异自证（实测）：去掉过期判断 →「超过存活期 1ns」两例红；「>」改为「>=」→「恰到存活期」两例红；负结果
// 也返回 FlagSessionActive →「负结果」各例红；从未发布时返回 FlagSessionKnown →「从未发布」红。
func TestSlotFlags(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var never Slot
	if f := never.Flags(t0); f != 0 {
		t.Fatalf("从未发布: flags = %#x, 期望 0", f)
	}
	var nilSlot *Slot
	if f := nilSlot.Flags(t0); f != 0 {
		t.Fatalf("nil 槽位: flags = %#x, 期望 0", f)
	}
	for _, active := range []bool{true, false} {
		var s Slot
		s.Publish(active, t0)
		fresh := known
		if active {
			fresh = both
		}
		for _, c := range []struct {
			name string
			now  time.Time
			want uint32
		}{
			{"发布当刻", t0, fresh},
			{"读取时刻早于结果（读写交错）", t0.Add(-time.Second), fresh},
			{"恰到存活期", t0.Add(resultTTL), fresh},
			{"超过存活期 1ns", t0.Add(resultTTL + time.Nanosecond), 0},
			{"远超存活期", t0.Add(time.Hour), 0},
		} {
			if f := s.Flags(c.now); f != c.want {
				t.Errorf("active=%v %s: flags = %#x, 期望 %#x", active, c.name, f, c.want)
			}
		}
	}
}

// 槽位不产生 (known, active) = (0, 1)，也不产生这两位之外的位：遍历各结果与各时刻。
//
// 变异自证（实测）：过期时返回 FlagSessionActive（保留 active、清掉 known）→ 红。
func TestSlotFlagsNeverActiveWithoutKnown(t *testing.T) {
	t0 := time.Unix(1000, 0)
	for _, active := range []bool{true, false} {
		var s Slot
		s.Publish(active, t0)
		for d := -time.Second; d <= 2*resultTTL; d += 250 * time.Millisecond {
			f := s.Flags(t0.Add(d))
			if f&^both != 0 || (f&frame.FlagSessionActive != 0 && f&frame.FlagSessionKnown == 0) {
				t.Fatalf("active=%v 距发布 %v: flags = %#x", active, d, f)
			}
		}
	}
}
