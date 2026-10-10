package player

// 本文件只测 BaseEmitter.EmitPositionSample：播放中位置采样的发出间隔、载荷与对位置入参的校验
// （issue #70）。各播放器是否调用它由 positionsample_lint_test.go 守，调用条件靠人。

import (
	"math"
	"testing"
	"time"
)

var sampleT0 = time.Unix(1_700_000_000, 0)

// drainEvents 非阻塞地取空事件通道。
func drainEvents(b *BaseEmitter) []Event {
	var out []Event
	for {
		select {
		case e := <-b.EventCh:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestEmitPositionSampleInterval 钉住发出的节奏：首次调用即发，其后距上一条满 PositionSampleInterval
// 才发，间隔从实际发出的那一条起算。
//
// 变异自证：把间隔比较的 `<` 改成 `<=`，「恰好满间隔」两处红；去掉对上一条时刻的记录，间隔内的
// 四处红（每次调用都发）。
func TestEmitPositionSampleInterval(t *testing.T) {
	b := NewBaseEmitter("x")
	steps := []struct {
		name string
		at   time.Duration
		want bool
	}{
		{"首次调用", 0, true},
		{"1 秒后", time.Second, false},
		{"差 1 纳秒满间隔", PositionSampleInterval - time.Nanosecond, false},
		{"恰好满间隔", PositionSampleInterval, true},
		{"上一条之后 1 秒", PositionSampleInterval + time.Second, false},
		{"距上一条差 1 纳秒", 2*PositionSampleInterval - time.Nanosecond, false},
		{"距上一条恰好满间隔", 2 * PositionSampleInterval, true},
		{"久未调用（暂停之后恢复）", 10 * PositionSampleInterval, true},
	}
	for _, st := range steps {
		b.EmitPositionSample(sampleT0.Add(st.at), 10, 100)
		if got := len(drainEvents(&b)) == 1; got != st.want {
			t.Errorf("%s：发出 = %v，期望 %v", st.name, got, st.want)
		}
	}
}

// TestEmitPositionSamplePayload 钉住事件的形状：类型 position_sample，载荷是 *PlaybackTimeInfo
// （事件载荷一律传结构体指针，AGENTS §3.2），position 为入参原值，progress 经 ClampProgress。
//
// 变异自证：把 Progress 改成直接 `position / duration`，时长为 0 的一例得 +Inf、超过时长的一例得
// 1.25 而红；把载荷改成值类型，类型断言红。
func TestEmitPositionSamplePayload(t *testing.T) {
	cases := []struct {
		name         string
		pos, dur     float32
		wantProgress float32
	}{
		{"常规", 30, 120, 0.25},
		{"时长未知", 30, 0, 0},
		{"位置超过时长", 150, 120, 1},
	}
	for _, c := range cases {
		b := NewBaseEmitter("某播放器")
		b.EmitPositionSample(sampleT0, c.pos, c.dur)
		evs := drainEvents(&b)
		if len(evs) != 1 {
			t.Fatalf("%s：应发出一条事件，实得 %d", c.name, len(evs))
		}
		e := evs[0]
		if e.Type != EventPositionSample || e.PlayerName != "某播放器" {
			t.Errorf("%s：事件 = {%q, %q}，期望 {%q, %q}", c.name, e.PlayerName, e.Type, "某播放器", EventPositionSample)
		}
		info, ok := e.Data.(*PlaybackTimeInfo)
		if !ok {
			t.Fatalf("%s：载荷类型 %T，期望 *PlaybackTimeInfo", c.name, e.Data)
		}
		if info.Position != c.pos || info.Progress != c.wantProgress {
			t.Errorf("%s：载荷 = %+v，期望 position %v、progress %v", c.name, *info, c.pos, c.wantProgress)
		}
	}
}

// TestEmitPositionSampleRejectsBadPosition 钉住非有限或为负的位置不发：NaN 进了缓存，补发与 HTTP 快照
// 的 JSON 编码就会失败（见 ClampProgress 的注释）。被拒的调用不占用间隔，紧接着的合法位置照发。
//
// 变异自证：去掉对位置的判定，四例都发出事件而红；把判定写成拒绝式 `position < 0 || position >
// math.MaxFloat32`，NaN 一例红；把记录时刻挪到判定之前，四例的「紧接着的合法位置」都发不出而红。
func TestEmitPositionSampleRejectsBadPosition(t *testing.T) {
	for name, pos := range map[string]float32{
		"NaN":  float32(math.NaN()),
		"+Inf": float32(math.Inf(1)),
		"-Inf": float32(math.Inf(-1)),
		"负数":   -0.5,
	} {
		b := NewBaseEmitter("x")
		b.EmitPositionSample(sampleT0, pos, 100)
		if n := len(drainEvents(&b)); n != 0 {
			t.Errorf("%s：不应发出事件，实得 %d 条", name, n)
		}
		b.EmitPositionSample(sampleT0, 10, 100)
		if n := len(drainEvents(&b)); n != 1 {
			t.Errorf("%s：被拒的调用不应占用间隔，紧接着的合法位置应发出，实得 %d 条", name, n)
		}
	}
}

// TestEmitPositionSampleNonBlocking 钉住采样走 Emit 的非阻塞发送：事件通道已满时丢弃，轮询循环不被卡住。
//
// 变异自证：把 EmitPositionSample 里的 b.Emit 改成 `b.EventCh <- Event{...}` 阻塞发送，本测试 2s 超时而红。
func TestEmitPositionSampleNonBlocking(t *testing.T) {
	b := NewBaseEmitter("x")
	for range cap(b.EventCh) {
		b.Emit(EventLyricIdle, struct{}{})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.EmitPositionSample(sampleT0, 10, 100)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("事件通道已满时 EmitPositionSample 被阻塞")
	}
	if n := len(b.EventCh); n != cap(b.EventCh) {
		t.Errorf("通道里应仍是原先那 %d 条，实得 %d", cap(b.EventCh), n)
	}
}
