package session

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/procscan"
)

// 本文件只测进程快照的缓存（Watcher.parents）：真机实测会话枚举每拍约 3 ms、取进程快照约 18 ms，逐拍重取
// 会占去整拍 CPU 的八成，故沿用 procRefresh 内的快照，只在期满或出现缓存里没有的 Active 会话 PID 时重取。
// 直接调 probe，时间用 synctest 的虚拟时钟推进。

// snapSrc 是可替换内容、可计次的进程快照源。
type snapSrc struct {
	procs atomic.Pointer[[]procscan.Proc]
	err   atomic.Bool
	calls atomic.Int32
}

func newSnapSrc(p ...procscan.Proc) *snapSrc {
	s := &snapSrc{}
	s.set(p...)
	return s
}

func (s *snapSrc) set(p ...procscan.Proc) { s.procs.Store(&p) }

func (s *snapSrc) snap() ([]procscan.Proc, error) {
	s.calls.Add(1)
	if s.err.Load() {
		return nil, errHR
	}
	return *s.procs.Load(), nil
}

// 缓存期内各拍不重取快照，期满后的那一拍重取一次；判定结果不受影响。
//
// 变异自证（实测）：parents 每拍都重取 → 缓存期内快照次数大于 1，红；parents 不看 procRefresh（缓存永不过期）
// → 期满后的那一拍快照次数仍为 1，红。
func TestSnapshotCachedUntilRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newSnapSrc(procscan.Proc{PID: target, PPID: 1}, procscan.Proc{PID: audioKid, PPID: target})
		w := NewWatcher(src.snap, func(string) {})
		s := w.Add(target)
		enum := &fakeEnum{eps: []endpoint{{sessions: []sessionInfo{active(audioKid)}}}}

		for i := 0; i < 4; i++ { // 共约 4 s，小于 procRefresh
			w.probe(enum, []*Slot{s})
			if f := s.Flags(time.Now()); f != both {
				t.Fatalf("第 %d 拍: flags = %#x, 期望 %#x", i, f, both)
			}
			time.Sleep(probeInterval)
		}
		if n := src.calls.Load(); n != 1 {
			t.Fatalf("缓存期内取了 %d 次快照, 期望 1", n)
		}

		time.Sleep(procRefresh) // 距上次取快照已满 procRefresh
		w.probe(enum, []*Slot{s})
		if n := src.calls.Load(); n != 2 {
			t.Fatalf("缓存期满后取了 %d 次快照, 期望 2", n)
		}
	})
}

// 出现缓存里没有的 Active 会话 PID（新起的子进程）时，不等期满、立即重取，用新快照判它是否在目标树内。
//
// 变异自证（实测）：parents 不检查 pids 里是否有缓存没有的 PID → 新子进程的会话按旧快照判为断链，发布 (1,0)，
// 红。
func TestSnapshotRefreshedForUnknownActivePID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const newKid = 102
		src := newSnapSrc(procscan.Proc{PID: target, PPID: 1})
		w := NewWatcher(src.snap, func(string) {})
		s := w.Add(target)

		w.probe(&fakeEnum{eps: []endpoint{{sessions: []sessionInfo{active(target)}}}}, []*Slot{s})
		if f := s.Flags(time.Now()); f != both {
			t.Fatalf("目标自身 Active: flags = %#x, 期望 %#x", f, both)
		}

		time.Sleep(probeInterval)
		src.set(procscan.Proc{PID: target, PPID: 1}, procscan.Proc{PID: newKid, PPID: target}) // 目标新起了子进程
		w.probe(&fakeEnum{eps: []endpoint{{sessions: []sessionInfo{active(newKid)}}}}, []*Slot{s})
		if n := src.calls.Load(); n != 2 {
			t.Fatalf("出现未知的 Active PID 后取了 %d 次快照, 期望 2", n)
		}
		if f := s.Flags(time.Now()); f != both {
			t.Fatalf("新子进程的 Active 会话: flags = %#x, 期望 %#x", f, both)
		}
	})
}

// 缓存期满后重取失败：本拍失败（不发布），缓存保持原状；快照恢复后的下一拍照常判定。
//
// 变异自证（实测）：重取失败时仍用旧缓存判定 → 失败那一拍发布了结果，红。
func TestSnapshotRefreshFailureFailsTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newSnapSrc(procscan.Proc{PID: target, PPID: 1}, procscan.Proc{PID: audioKid, PPID: target})
		w := NewWatcher(src.snap, func(string) {})
		s := w.Add(target)
		enum := &fakeEnum{eps: []endpoint{{sessions: []sessionInfo{active(audioKid)}}}}

		w.probe(enum, []*Slot{s})
		time.Sleep(procRefresh + time.Second) // 缓存期满，且第一拍的结果已过存活期
		if f := s.Flags(time.Now()); f != 0 {
			t.Fatalf("前置: 旧结果应已过期, flags = %#x", f)
		}
		src.err.Store(true)
		w.probe(enum, []*Slot{s})
		if f := s.Flags(time.Now()); f != 0 {
			t.Fatalf("重取失败的那一拍: flags = %#x, 期望 0（不发布）", f)
		}

		src.err.Store(false)
		time.Sleep(probeInterval)
		w.probe(enum, []*Slot{s})
		if f := s.Flags(time.Now()); f != both {
			t.Fatalf("快照恢复后: flags = %#x, 期望 %#x", f, both)
		}
	})
}
