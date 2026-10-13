package session

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/procscan"
)

// 本文件只测一拍探测结果的发布规则（设计 §4.2、§4.2.1）：什么算正结果，失败时什么情况下仍可发布负结果，
// 经 Watcher.probe 与注入的假枚举器走完 scan、decide 与写槽位。

// fakeEnum 每次 enumerate 返回同一组结果，并计次。计数原子读写：synctest 的计时器唤醒不给竞态检测建立
// 先后关系。
type fakeEnum struct {
	eps   []endpoint
	err   error
	calls atomic.Int32
}

func (f *fakeEnum) enumerate() ([]endpoint, error) {
	f.calls.Add(1)
	return f.eps, f.err
}

var errHR = errors.New("hr=0x88890004")

const (
	target    = 100 // 目标进程（capture 的 pid）
	audioKid  = 101 // 目标的子进程（Chromium 系的音频子进程）
	helperPID = 400 // helper 自己：进程 loopback 的捕获会话挂在目标所在的端点上，不在目标树内
)

// snapshotOf 返回固定的进程快照：目标、其子进程与 helper 各一。
func snapshotOf() func() ([]procscan.Proc, error) {
	return func() ([]procscan.Proc, error) {
		return []procscan.Proc{{PID: target, PPID: 1}, {PID: audioKid, PPID: target}, {PID: helperPID, PPID: 1}}, nil
	}
}

// probeOnce 对目标 PID 新建槽位、按 eps 做一拍探测，返回此刻的帧标志。
func probeOnce(t *testing.T, eps []endpoint) uint32 {
	t.Helper()
	w := NewWatcher(snapshotOf(), func(string) {})
	s := w.Add(target)
	w.probe(&fakeEnum{eps: eps}, []*Slot{s})
	return s.Flags(time.Now())
}

func active(pid uint32) sessionInfo   { return sessionInfo{active: true, pid: pid} }
func inactive(pid uint32) sessionInfo { return sessionInfo{pid: pid} }

// 正结果只认目标树内、Active、非系统声音的会话。
//
// 变异自证（实测）：scan 不看 active（存在会话即计入）→「目标只有 Inactive 会话」红；decide 不判 inTree（任何
// Active 会话都算）→「同端点有 helper 自己的 Active 会话」红；scan 不看 system →「系统声音会话」红。
func TestPublishOnlyActiveTargetTree(t *testing.T) {
	for _, c := range []struct {
		name string
		eps  []endpoint
		want uint32
	}{
		{"目标自身 Active", []endpoint{{sessions: []sessionInfo{active(target)}}}, both},
		{"目标的子进程 Active", []endpoint{{sessions: []sessionInfo{active(audioKid)}}}, both},
		{"非默认端点上 Active", []endpoint{{}, {}, {sessions: []sessionInfo{active(target)}}}, both},
		{"目标只有 Inactive 会话", []endpoint{{sessions: []sessionInfo{inactive(target), inactive(audioKid)}}}, known},
		{"同端点有 helper 自己的 Active 会话", []endpoint{{sessions: []sessionInfo{active(helperPID), inactive(target)}}}, known},
		{"系统声音会话", []endpoint{{sessions: []sessionInfo{{active: true, pid: target, system: true}}}}, known},
		{"PID 0 的 Active 会话", []endpoint{{sessions: []sessionInfo{active(0)}}}, known},
		{"没有端点", nil, known},
	} {
		if f := probeOnce(t, c.eps); f != c.want {
			t.Errorf("%s: flags = %#x, 期望 %#x", c.name, f, c.want)
		}
	}
}

// 不完整的一拍：找到目标即发布正结果，没找到则不发布（槽位保持从未发布的 0）；Inactive 会话的 PID 查询失败
// 不算不完整。
//
// 变异自证（实测）：decide 不看 complete（不完整也发布负结果）→「端点失败」「会话失败」「Active 会话取不到
// PID」三例红；scan 先判 pidErr 后判 active（Inactive 的 PID 失败也算不完整）→「Inactive 会话取不到 PID」红；
// decide 在 complete 为假时先返回、不再找正结果 →「一端点失败、另一端点找到目标」红。
func TestPublishIncompleteTick(t *testing.T) {
	for _, c := range []struct {
		name string
		eps  []endpoint
		want uint32
	}{
		{"一端点失败、另一端点找到目标", []endpoint{{err: errHR}, {sessions: []sessionInfo{active(target)}}}, both},
		{"端点失败、另一端点为空", []endpoint{{err: errHR}, {}}, 0},
		{"会话失败、其余会话无目标", []endpoint{{sessions: []sessionInfo{{err: errHR}, inactive(target)}}}, 0},
		{"Active 会话取不到 PID", []endpoint{{sessions: []sessionInfo{{active: true, pidErr: errHR}}}}, 0},
		{"Active 会话取不到 PID、另一会话是目标", []endpoint{{sessions: []sessionInfo{{active: true, pidErr: errHR}, active(audioKid)}}}, both},
		{"Inactive 会话取不到 PID", []endpoint{{sessions: []sessionInfo{{pidErr: errHR}, inactive(target)}}}, known},
	} {
		if f := probeOnce(t, c.eps); f != c.want {
			t.Errorf("%s: flags = %#x, 期望 %#x", c.name, f, c.want)
		}
	}
}

// 整拍失败（枚举器报错、进程快照失败）时不写槽位，即使会话里有目标。
//
// 变异自证（实测）：快照失败时按空快照照常判定 → 目标自身的会话仍判为正结果，红。
func TestPublishNothingWhenTickFails(t *testing.T) {
	eps := []endpoint{{sessions: []sessionInfo{active(target)}}}
	w := NewWatcher(func() ([]procscan.Proc, error) { return nil, errHR }, func(string) {})
	s := w.Add(target)
	w.probe(&fakeEnum{eps: eps}, []*Slot{s})
	if f := s.Flags(time.Now()); f != 0 {
		t.Fatalf("进程快照失败: flags = %#x, 期望 0", f)
	}
	w = NewWatcher(snapshotOf(), func(string) {})
	s = w.Add(target)
	w.probe(&fakeEnum{eps: eps, err: errHR}, []*Slot{s})
	if f := s.Flags(time.Now()); f != 0 {
		t.Fatalf("枚举器报错: flags = %#x, 期望 0", f)
	}
}

// 不完整的一拍不刷新已有结果：先有正结果，2 秒后一拍里唯一的 Active 会话取不到 PID，槽位仍是 2 秒前的正
// 结果，并按它的时刻在 3 秒存活期后过期，而不是被刷新成负结果或续期。
//
// 变异自证（实测）：scan 忽略 pidErr、把该会话按 PID 0 计 → 第二拍发布负结果，红。
func TestIncompleteTickDoesNotRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := NewWatcher(snapshotOf(), func(string) {})
		s := w.Add(target)
		w.probe(&fakeEnum{eps: []endpoint{{sessions: []sessionInfo{active(target)}}}}, []*Slot{s})
		time.Sleep(2 * time.Second)
		w.probe(&fakeEnum{eps: []endpoint{{sessions: []sessionInfo{{active: true, pidErr: errHR}}}}}, []*Slot{s})
		if f := s.Flags(time.Now()); f != both {
			t.Fatalf("不完整的一拍之后: flags = %#x, 期望仍为上一拍的 %#x", f, both)
		}
		time.Sleep(resultTTL - 2*time.Second + time.Millisecond)
		if f := s.Flags(time.Now()); f != 0 {
			t.Fatalf("首拍之后超过存活期: flags = %#x, 期望 0（不完整的一拍不得续期）", f)
		}
	})
}

// 端点 E1 上有目标的 Active 会话但每拍都查询失败，端点 E2 每拍成功返回空：经 Run 连续 12 拍，帧标志始终
// 为 0，不得持续产生 (1,0)（设计 §4.2.1 的理由：静音时前端会在 10 秒窗口后误报）。
//
// 变异自证（实测）：decide 不看 complete → 每拍为 (1,0)，红。
func TestFailingEndpointNeverYieldsNegative(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		enum := &fakeEnum{eps: []endpoint{{err: errHR}, {}}}
		w := NewWatcher(snapshotOf(), func(string) {})
		s := w.Add(target)
		done := make(chan struct{})
		go func() {
			w.Run(enum)
			close(done)
		}()
		for i := range 12 {
			synctest.Wait()
			if n := enum.calls.Load(); n != int32(i+1) {
				t.Fatalf("第 %d 拍: 枚举 %d 次", i+1, n)
			}
			if f := s.Flags(time.Now()); f != 0 {
				t.Fatalf("第 %d 拍: flags = %#x, 期望 0", i+1, f)
			}
			time.Sleep(probeInterval)
		}
		w.Stop()
		<-done
	})
}

// 一拍对多个槽位各按自己的目标判定。
func TestProbeJudgesEachSlot(t *testing.T) {
	w := NewWatcher(snapshotOf(), func(string) {})
	a, b := w.Add(target), w.Add(helperPID+1)
	w.probe(&fakeEnum{eps: []endpoint{{sessions: []sessionInfo{active(audioKid)}}}}, []*Slot{a, b})
	if fa, fb := a.Flags(time.Now()), b.Flags(time.Now()); fa != both || fb != known {
		t.Fatalf("flags a=%#x b=%#x, 期望 %#x、%#x", fa, fb, both, known)
	}
}

// 不完整的拍首次即报诊断，此后每 60 秒至多一条，带上其间不完整的拍数；完整的拍不报。
//
// 变异自证（实测）：去掉限频 → 第 2 拍即报出第二条，红；限频后不清零计数 → 第二条报 61 拍，红。
func TestDiagRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got []string
		w := NewWatcher(snapshotOf(), func(s string) { got = append(got, s) })
		s := w.Add(target)
		bad := &fakeEnum{eps: []endpoint{{err: errHR}}}
		w.probe(&fakeEnum{}, []*Slot{s})
		if len(got) != 0 {
			t.Fatalf("完整的拍报了诊断: %q", got)
		}
		for i := 0; i < 60; i++ {
			w.probe(bad, []*Slot{s})
			time.Sleep(time.Second)
		}
		if len(got) != 1 || !strings.Contains(got[0], "共 1 拍") || !strings.Contains(got[0], "端点 #0") {
			t.Fatalf("60 拍内的诊断 = %q, 期望首拍一条", got)
		}
		w.probe(bad, []*Slot{s}) // 距首条恰 60 秒
		if len(got) != 2 || !strings.Contains(got[1], "共 60 拍") {
			t.Fatalf("第 61 拍后的诊断 = %q, 期望第二条报 60 拍", got)
		}
	})
}
