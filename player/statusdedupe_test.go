package player

// 本文件只测 BaseEmitter.EmitStatusIfChanged（issue #119）：没运行的播放器外层循环每轮都要声明一次
// 「未运行」，内容没变就不该每轮都推给订阅者；别的状态插进来、或上一次没发成，就要重新发。

import "testing"

func waitingInfo() *StatusInfo {
	return &StatusInfo{Status: "waiting_process", Detail: "QQ Music not running", Reason: ReasonProcessNotRunning}
}

func statusTypes(evts []Event) []string {
	var out []string
	for _, e := range evts {
		if e.Type == EventStatusUpdate {
			out = append(out, e.Data.(*StatusInfo).Status)
		} else {
			out = append(out, e.Type)
		}
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 相同内容连续声明只发第一次，返回值如实报告。
//
// 变异自证（均实测）：EmitStatusIfChanged 里去掉比较（总是发）即红。
func TestEmitStatusIfChangedSuppressesRepeat(t *testing.T) {
	b := NewBaseEmitter("test")
	for i, want := range []bool{true, false, false, false} {
		if got := b.EmitStatusIfChanged(waitingInfo()); got != want {
			t.Errorf("第 %d 次：返回 %v，期望 %v", i+1, got, want)
		}
	}
	if evts := drain(&b); len(evts) != 1 {
		t.Fatalf("应只发出一条，实得 %d 条：%v", len(evts), statusTypes(evts))
	}
}

// Status、Detail、Reason 任一不同都算有变化，发出后同样的再次声明又被压住。
func TestEmitStatusIfChangedEmitsOnAnyFieldChange(t *testing.T) {
	variants := map[string]*StatusInfo{
		"Status 不同": {Status: "standby", Detail: "QQ Music not running", Reason: ReasonProcessNotRunning},
		"Detail 不同": {Status: "waiting_process", Detail: "Cannot read the process", Reason: ReasonProcessNotRunning},
		"Reason 不同": {Status: "waiting_process", Detail: "QQ Music not running", Reason: ReasonNoAdmin},
	}
	for name, v := range variants {
		t.Run(name, func(t *testing.T) {
			b := NewBaseEmitter("test")
			b.EmitStatusIfChanged(waitingInfo())
			if !b.EmitStatusIfChanged(v) {
				t.Fatal("内容有变化，应当发出")
			}
			cp := *v
			if b.EmitStatusIfChanged(&cp) {
				t.Error("变化后同样的内容再次声明，应当被压住")
			}
			if evts := drain(&b); len(evts) != 2 {
				t.Errorf("应发出 2 条，实得 %d 条", len(evts))
			}
		})
	}
}

// 中间经 Emit 发出了别的状态（playing、standby 等），下一次声明就是有变化，要重新发——
// 否则会话结束后「未运行」永远发不出来。
//
// 变异自证（均实测）：emit 里不记 Emit 发出的状态（只在 EmitStatusIfChanged 里记）即红。
func TestEmitStatusIfChangedOtherStatusResets(t *testing.T) {
	b := NewBaseEmitter("test")
	b.EmitStatusIfChanged(waitingInfo())
	b.Emit(EventStatusUpdate, &StatusInfo{Status: "playing", Detail: "song"})
	b.Emit(EventStatusUpdate, &StatusInfo{Status: "standby", Detail: "exited", Reason: ReasonProcessExited})
	if !b.EmitStatusIfChanged(waitingInfo()) {
		t.Fatal("中间有别的状态，再次声明应当发出")
	}
	if got, want := statusTypes(drain(&b)), []string{"waiting_process", "playing", "standby", "waiting_process"}; !eq(got, want) {
		t.Errorf("事件序列 %v，期望 %v", got, want)
	}
}

// 经 Emit 直接发出的同样内容也算「最近一次发出」。
func TestEmitStatusIfChangedCountsPlainEmit(t *testing.T) {
	b := NewBaseEmitter("test")
	b.Emit(EventStatusUpdate, waitingInfo())
	if b.EmitStatusIfChanged(waitingInfo()) {
		t.Error("Emit 刚发过同样内容，不该再发")
	}
}

// 非状态事件不影响比较。
func TestEmitStatusIfChangedIgnoresOtherEvents(t *testing.T) {
	b := NewBaseEmitter("test")
	b.EmitStatusIfChanged(waitingInfo())
	b.Emit(EventClearSongData, nil)
	b.Emit(EventSongInfoUpdate, &SongInfo{Title: "x"})
	if b.EmitStatusIfChanged(waitingInfo()) {
		t.Error("中间只有非状态事件，不该再发")
	}
}

// 通道满、事件被丢弃时不能记成「已发出」，否则这条状态就永远发不出去了。
//
// 变异自证（均实测）：emit 里不论入队与否都记即红。
func TestEmitStatusIfChangedDroppedIsNotRecorded(t *testing.T) {
	b := NewBaseEmitter("test")
	for len(b.EventCh) < cap(b.EventCh) {
		b.Emit(EventSongInfoUpdate, &SongInfo{Title: "filler"})
	}
	if b.EmitStatusIfChanged(waitingInfo()) {
		t.Fatal("通道已满，应报告没发出")
	}
	drain(&b)
	if !b.EmitStatusIfChanged(waitingInfo()) {
		t.Error("上一次没发成，腾出空位后应当发出")
	}
}

// 载荷为空或类型不对的状态事件不让它 panic，也不记。
func TestEmitStatusIfChangedToleratesMalformedStatusEvents(t *testing.T) {
	b := NewBaseEmitter("test")
	b.Emit(EventStatusUpdate, nil)
	b.Emit(EventStatusUpdate, "not a status")
	var nilInfo *StatusInfo
	b.Emit(EventStatusUpdate, nilInfo)
	if !b.EmitStatusIfChanged(waitingInfo()) {
		t.Error("此前没有有效状态记录，应当发出")
	}
}
