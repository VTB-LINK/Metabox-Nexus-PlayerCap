package assoc

// 本文件用 testing/synctest 与内存桩测双源融合状态机（#250/#251，设计 §6.2、§6.3、§6.4.2、§6.6）：
//   - 地址复用与取消关联分支在 25ms 双拍稳定窗口后放行，绝不永久 Pending；
//   - 观测缺口下的内嵌缓存 Hash 确定性拦截，与无内嵌 Hash 时的延后及超时覆盖自愈；
//   - PlayedAt 拒绝过旧历史 P，以及单曲循环重播时经 1s 持续 stat 检出新 PlayedAt；
//   - 同文件连续调偏移零重读/零 stat 歌词文件，且卡死网络盘读文件不阻塞取消关联、后台 I/O 单飞；
//   - PID 变更重连新进程，以及连续 3 次 BadVtable 自动关闭句柄降级至磁盘模式。

import (
	"context"
	"io/fs"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

type stubMemSource struct {
	mu       sync.Mutex
	pid      uint32
	snap     memSnapshot
	status   memStatus
	onRead   func(n int) (memSnapshot, memStatus)
	reads    int
	closed   bool
	closeCnt int
}

func (s *stubMemSource) ReadSnapshot() (memSnapshot, memStatus) {
	s.mu.Lock()
	s.reads++
	n := s.reads
	fn := s.onRead
	snap := s.snap
	st := s.status
	s.mu.Unlock()
	if fn != nil {
		return fn(n)
	}
	return snap, st
}

func (s *stubMemSource) PID() uint32 { return s.pid }

func (s *stubMemSource) Close() {
	s.mu.Lock()
	s.closed = true
	s.closeCnt++
	s.mu.Unlock()
}

func (s *stubMemSource) set(snap memSnapshot, st memStatus) {
	s.mu.Lock()
	s.snap = snap
	s.status = st
	s.mu.Unlock()
}

func (s *stubMemSource) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func TestMemOwnershipAddressReuseAndCancelBranch(t *testing.T) {
	hashA := hex32("aa", 1)
	hashB := hex32("bb", 2)
	hashC := hex32("cc", 3)
	const pathA = `D:\a.lrc`

	src := &stubMemSource{
		pid: okConfirmed.PID,
		snap: memSnapshot{
			SongItemPtr:     0x100,
			CurrentSongHash: hashA,
			CLyricPtr:       0x10000,
			TaskID:          7,
			LyricFilePath:   pathA,
		},
		status: memStatusOK,
	}

	f := &fakeDeps{
		openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
			return src, memOpenOK, "fp-test", nil
		},
		readFn: func(_ time.Duration, path string) ([]byte, error) {
			if path == pathA {
				return []byte("[00:01.00]歌A\n"), nil
			}
			return nil, fs.ErrNotExist
		},
	}

	runReader(t, f, func(t *testing.T, r *Reader) {
		// 首首歌 A 确认 (clyricPtr=0x10000, taskID=7)。
		r.SetHash(hashA)
		res := latest(t, r)
		wantKind(t, res, hashA, KindFound)
		if len(res.Lines) != 1 || res.Lines[0].Text != "歌A" {
			t.Fatalf("首首歌结果 %+v，want 歌A", res)
		}

		// 切入 B 后立即切入 C（漏采 B），C 复用 0x10000 且走右键取消关联保持 taskID=7。
		src.set(memSnapshot{
			SongItemPtr:        0x300,
			CurrentSongHash:    hashC,
			CLyricPtr:          0x10000,
			TaskID:             7,
			IsUserUnassociated: true,
			AssocPath:          "用户取消关联",
		}, memStatusOK)
		r.SetHash(hashB)
		r.SetHash(hashC)

		// 24ms 时尚未过 25ms 双拍稳定窗口，不得提前放行。
		time.Sleep(24 * time.Millisecond)
		noResult(t, r)

		// 满 25ms 后五元组稳定，立即发出 KindUnassociated（绝不永久 Pending）。
		time.Sleep(time.Millisecond)
		res = latest(t, r)
		wantKind(t, res, hashC, KindUnassociated)
	})
}

func TestMemOwnershipObserverGapAndEmbeddedCacheHash(t *testing.T) {
	hashA := hex32("aa", 1)
	hashB := hex32("bb", 2)
	hashC := hex32("cc", 3)
	const pathA = `D:\a.lrc`
	cacheB := okConfirmed.LyricDir + `\歌手 - 歌B-` + hashB + `-123-00000000.krc`
	cacheC := okConfirmed.LyricDir + `\歌手 - 歌C-` + hashC + `-456-00000000.krc`

	t.Run("内嵌缓存Hash确定性拦截旧歌缓存直至100ms切换到C", func(t *testing.T) {
		src := &stubMemSource{
			pid: okConfirmed.PID,
			snap: memSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				TaskID:          1,
				LyricFilePath:   pathA,
			},
			status: memStatusOK,
		}
		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src, memOpenOK, "fp-test", nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				switch path {
				case pathA:
					return []byte("[00:01.00]歌A\n"), nil
				case cacheB:
					return []byte("[00:01.00]歌B缓存\n"), nil
				case cacheC:
					return []byte("[00:01.00]歌C缓存\n"), nil
				}
				return nil, fs.ErrNotExist
			},
		}

		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash(hashA)
			wantKind(t, latest(t, r), hashA, KindFound)

			// 切到 C，但 CLyric 仍指向 B 的缓存文件（KrcID != 0，文件名内嵌 hashB）。
			src.set(memSnapshot{
				SongItemPtr:     0x300,
				CurrentSongHash: hashC,
				CLyricPtr:       0x10000,
				KrcID:           123,
				TaskID:          2,
				LyricFilePath:   cacheB,
			}, memStatusOK)
			r.SetHash(hashC)

			// +25ms、+50ms、+99ms 均被内嵌缓存 Hash 确定性拦截，绝不发布 B。
			time.Sleep(25 * time.Millisecond)
			noResult(t, r)
			time.Sleep(74 * time.Millisecond)
			noResult(t, r)

			// +100ms 切换为 C 的缓存文件。
			time.Sleep(time.Millisecond)
			src.set(memSnapshot{
				SongItemPtr:     0x300,
				CurrentSongHash: hashC,
				CLyricPtr:       0x20000,
				KrcID:           456,
				TaskID:          3,
				LyricFilePath:   cacheC,
			}, memStatusOK)

			// 等待下一轮快速重试 + 25ms 双拍稳定后发布 C。
			time.Sleep(100 * time.Millisecond)
			res := latest(t, r)
			wantKind(t, res, hashC, KindFound)
			if res.Source != SourceCache || len(res.Lines) != 1 || res.Lines[0].Text != "歌C缓存" {
				t.Fatalf("结果 %+v，want SourceCache / 歌C缓存", res)
			}
		})
	})

	t.Run("无内嵌Hash第2拍变更延至第3拍放行", func(t *testing.T) {
		const (
			manualOld = `D:\manual_old.lrc`
			manualC   = `D:\manual_c.lrc`
		)
		src := &stubMemSource{
			pid: okConfirmed.PID,
			snap: memSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				TaskID:          1,
				LyricFilePath:   pathA,
			},
			status: memStatusOK,
		}
		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src, memOpenOK, "fp-test", nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				switch path {
				case pathA:
					return []byte("[00:01.00]歌A\n"), nil
				case manualOld:
					return []byte("[00:01.00]旧歌词\n"), nil
				case manualC:
					return []byte("[00:01.00]新歌词C\n"), nil
				}
				return nil, fs.ErrNotExist
			},
		}

		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash(hashA)
			wantKind(t, latest(t, r), hashA, KindFound)

			src.set(memSnapshot{
				SongItemPtr:     0x300,
				CurrentSongHash: hashC,
				CLyricPtr:       0x10000,
				TaskID:          2,
				LyricFilePath:   manualOld,
			}, memStatusOK)
			r.SetHash(hashC)
			synctest.Wait()

			// 在第 2 拍（+25ms）到来前改变 CLyricPtr 为新对象。
			time.Sleep(20 * time.Millisecond)
			src.set(memSnapshot{
				SongItemPtr:     0x300,
				CurrentSongHash: hashC,
				CLyricPtr:       0x20000,
				TaskID:          3,
				LyricFilePath:   manualC,
			}, memStatusOK)

			// 第 2 拍（+25ms）发现变化，刷新基准不放行。
			time.Sleep(5 * time.Millisecond)
			noResult(t, r)

			// 第 3 拍（+50ms）稳定放行并发布 C。
			time.Sleep(25 * time.Millisecond)
			res := latest(t, r)
			wantKind(t, res, hashC, KindFound)
			if len(res.Lines) != 1 || res.Lines[0].Text != "新歌词C" {
				t.Fatalf("结果 %+v，want 新歌词C", res)
			}
		})
	})

	t.Run("无内嵌Hash停顿100ms超出25ms窗口后下一拍覆盖自愈为C", func(t *testing.T) {
		const (
			manualOld = `D:\manual_old.lrc`
			manualC   = `D:\manual_c.lrc`
		)
		src := &stubMemSource{
			pid: okConfirmed.PID,
			snap: memSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				TaskID:          1,
				LyricFilePath:   pathA,
			},
			status: memStatusOK,
		}
		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src, memOpenOK, "fp-test", nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				switch path {
				case pathA:
					return []byte("[00:01.00]歌A\n"), nil
				case manualOld:
					return []byte("[00:01.00]旧歌词\n"), nil
				case manualC:
					return []byte("[00:01.00]新歌词C\n"), nil
				}
				return nil, fs.ErrNotExist
			},
		}

		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash(hashA)
			wantKind(t, latest(t, r), hashA, KindFound)

			src.set(memSnapshot{
				SongItemPtr:     0x300,
				CurrentSongHash: hashC,
				CLyricPtr:       0x10000,
				TaskID:          2,
				LyricFilePath:   manualOld,
			}, memStatusOK)
			r.SetHash(hashC)

			// 25ms 启发式窗口内 UI 未变，先放行旧对象。
			time.Sleep(25 * time.Millisecond)
			first := latest(t, r)
			wantKind(t, first, hashC, KindFound)
			if len(first.Lines) != 1 || first.Lines[0].Text != "旧歌词" {
				t.Fatalf("首个结果 %+v，want 旧歌词", first)
			}

			// +100ms 时 UI 才更新为 C。
			time.Sleep(75 * time.Millisecond)
			src.set(memSnapshot{
				SongItemPtr:     0x300,
				CurrentSongHash: hashC,
				CLyricPtr:       0x20000,
				TaskID:          3,
				LyricFilePath:   manualC,
			}, memStatusOK)

			// 常规 200ms 轮询下一拍（+225ms）自动覆盖自愈为 C。
			time.Sleep(150 * time.Millisecond)
			healed := latest(t, r)
			wantKind(t, healed, hashC, KindFound)
			if len(healed.Lines) != 1 || healed.Lines[0].Text != "新歌词C" {
				t.Fatalf("自愈结果 %+v，want 新歌词C", healed)
			}
		})
	})
}

func TestMemPlayedAtRejectsStaleHistoryAndTracksSameHashLoopReplay(t *testing.T) {
	hashA := hex32("aa", 1)
	hashB := hex32("bb", 2)
	const (
		pathA  = `D:\a.lrc`
		pathB  = `D:\b.lrc`
		audioB = `D:\Music\B.flac`
	)

	var mu sync.Mutex
	curPlaying := map[string]playing{}
	dbMtime := int64(1000)

	src := &stubMemSource{pid: okConfirmed.PID, status: memStatusOK}
	f := &fakeDeps{
		openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
			return src, memOpenOK, "fp-test", nil
		},
		playingRowFn: func(_ context.Context, _ string, hash string) (playing, error) {
			mu.Lock()
			defer mu.Unlock()
			return curPlaying[hash], nil
		},
		statFn: func(_ time.Duration) (int64, time.Time, error) {
			mu.Lock()
			sec := dbMtime
			mu.Unlock()
			return 1 << 20, time.Unix(sec, 0), nil
		},
		readFn: func(_ time.Duration, path string) ([]byte, error) {
			switch path {
			case pathA:
				return []byte("[00:01.00]歌A\n"), nil
			case pathB:
				return []byte("[00:01.00]歌B\n"), nil
			}
			return nil, fs.ErrNotExist
		},
	}

	runReader(t, f, func(t *testing.T, r *Reader) {
		// 首首歌 A：库内仅有 1 小时前的历史旧 P（早于 songStart - 5s）。
		staleT := fileTime(time.Now().Add(-time.Hour))
		mu.Lock()
		curPlaying[hashA] = playing{played: staleT, path: "歌手 - A.mp3"}
		mu.Unlock()
		src.set(memSnapshot{
			SongItemPtr:     0x100,
			CurrentSongHash: hashA,
			CLyricPtr:       0x10000,
			LyricFilePath:   pathA,
		}, memStatusOK)

		r.SetHash(hashA)
		res := latest(t, r)
		wantKind(t, res, hashA, KindFound)
		if res.PlayedAt != 0 {
			t.Fatalf("过旧历史 P 不应被采信，实得 PlayedAt=%d", res.PlayedAt)
		}

		// 随后刷入真实 T1 并更新库 mtime（稳定 1s 后由后台 stat 触发重读）。
		t1 := fileTime(time.Now())
		mu.Lock()
		curPlaying[hashA] = playing{played: t1, path: "歌手 - A.mp3"}
		dbMtime = 1001
		mu.Unlock()

		time.Sleep(2500 * time.Millisecond)
		res = latest(t, r)
		wantKind(t, res, hashA, KindFound)
		if res.PlayedAt != t1 || res.LocalFile {
			t.Fatalf("补发结果 %+v，want PlayedAt=%d、LocalFile=false", res, t1)
		}

		// 切入本地歌曲 B，先采纳 PlayedAt=T2。
		time.Sleep(time.Second)
		t2 := fileTime(time.Now())
		mu.Lock()
		curPlaying[hashB] = playing{played: t2, path: audioB}
		dbMtime = 1002
		mu.Unlock()
		src.set(memSnapshot{
			SongItemPtr:     0x200,
			CurrentSongHash: hashB,
			CLyricPtr:       0x20000,
			LyricFilePath:   pathB,
		}, memStatusOK)

		r.SetHash(hashB)
		time.Sleep(30 * time.Millisecond)
		res = latest(t, r)
		wantKind(t, res, hashB, KindFound)
		if res.PlayedAt != t2 || !res.LocalFile {
			t.Fatalf("歌曲 B 首次结果 %+v，want PlayedAt=%d、LocalFile=true", res, t2)
		}

		// 保持 hash == B 单曲循环重播：库 mtime 更新并刷入 T3 > T2，内存变为 NoLyricFound=true。
		time.Sleep(time.Second)
		t3 := fileTime(time.Now())
		mu.Lock()
		curPlaying[hashB] = playing{played: t3, path: audioB}
		dbMtime = 1003
		mu.Unlock()
		src.set(memSnapshot{
			SongItemPtr:     0x200,
			CurrentSongHash: hashB,
			NoLyricFound:    true,
		}, memStatusOK)

		// 经持续 1s stat 检出并补发携带 PlayedAt=T3 的 KindNone。
		time.Sleep(3 * time.Second)
		res = latest(t, r)
		wantKind(t, res, hashB, KindNone)
		if res.PlayedAt != t3 || !res.LocalFile {
			t.Fatalf("单曲循环重播结果 %+v，want KindNone、PlayedAt=%d、LocalFile=true", res, t3)
		}
	})
}

func TestMemUnassociateNotBlockedByStuckFileIOAndCacheZeroStat(t *testing.T) {
	t.Run("连续5次调偏移零重读且不stat歌词文件", func(t *testing.T) {
		hashA := hex32("aa", 1)
		const lyricPath = `D:\a.lrc`

		var readCalls atomic.Int32
		var fileDeleted atomic.Bool
		var statLyricCalls atomic.Int32

		src := &stubMemSource{
			pid: okConfirmed.PID,
			snap: memSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				AssocPath:       lyricPath,
				LyricFilePath:   lyricPath,
				LyricOffsetMs:   0,
			},
			status: memStatusOK,
		}

		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src, memOpenOK, "fp-test", nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				readCalls.Add(1)
				if fileDeleted.Load() || path != lyricPath {
					return nil, fs.ErrNotExist
				}
				return []byte(testLRC), nil
			},
			statFn: func(_ time.Duration) (int64, time.Time, error) {
				return 1 << 20, time.Unix(1000, 0), nil
			},
		}
		// 显式包装底层 deps.stat 以校验绝不 stat 歌词文件路径。
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.t0 = time.Now()
			d := f.deps()
			origStat := d.stat
			d.stat = func(p string) (int64, time.Time, error) {
				if p == lyricPath {
					statLyricCalls.Add(1)
				}
				return origStat(p)
			}
			r := start(ctx, d)
			if r == nil {
				t.Fatal("start 返回 nil")
			}
			synctest.Wait()

			r.SetHash(hashA)
			res := latest(t, r)
			wantKind(t, res, hashA, KindFound)
			if res.LyricOffsetMs != 0 || readCalls.Load() != 1 {
				t.Fatalf("首拍结果 %+v readCalls=%d，want offset=0 / readCalls=1", res, readCalls.Load())
			}

			// 模拟原歌词文件已被删除。
			fileDeleted.Store(true)

			// 连续 5 次调节 LyricOffsetMs（500 -> 1000 -> 1500 -> 2000 -> 2500）。
			for _, off := range []int{500, 1000, 1500, 2000, 2500} {
				src.set(memSnapshot{
					SongItemPtr:     0x100,
					CurrentSongHash: hashA,
					CLyricPtr:       0x10000,
					AssocPath:       lyricPath,
					LyricFilePath:   lyricPath,
					LyricOffsetMs:   off,
				}, memStatusOK)
				time.Sleep(memPollEvery)
				res = latest(t, r)
				wantKind(t, res, hashA, KindFound)
				if res.LyricOffsetMs != off {
					t.Fatalf("偏移=%d 时结果 LyricOffsetMs=%d", off, res.LyricOffsetMs)
				}
			}

			if n := readCalls.Load(); n != 1 {
				t.Fatalf("同一文件调偏移后 readFile 调用了 %d 次，want 1", n)
			}
			if n := statLyricCalls.Load(); n != 0 {
				t.Fatalf("对歌词文件调用了 %d 次 stat，want 0", n)
			}

			cancel()
			synctest.Wait()
			if n := readers.Load(); n != 0 {
				t.Fatalf("取消后存活读取者 %d，want 0", n)
			}
		})
	})

	t.Run("网络盘读文件卡死不阻塞取消关联且活跃IO协程不超过1", func(t *testing.T) {
		hashA := hex32("aa", 1)
		const stuckPath = `\\nas\stuck\slow.lrc`

		var g gate
		var activeIO, maxActiveIO, enteredIO atomic.Int32

		src := &stubMemSource{
			pid: okConfirmed.PID,
			snap: memSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				AssocPath:       stuckPath,
				LyricFilePath:   stuckPath,
			},
			status: memStatusOK,
		}

		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src, memOpenOK, "fp-test", nil
			},
			playingRowFn: func(_ context.Context, _ string, _ string) (playing, error) {
				return playing{}, nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				cur := activeIO.Add(1)
				for {
					prev := maxActiveIO.Load()
					if cur <= prev || maxActiveIO.CompareAndSwap(prev, cur) {
						break
					}
				}
				enteredIO.Add(1)
				defer activeIO.Add(-1)
				g.wait()
				return []byte(testLRC), nil
			},
		}

		runReader(t, f, func(t *testing.T, r *Reader) {
			g.arm()
			defer g.release()

			r.SetHash(hashA)
			synctest.Wait()
			if enteredIO.Load() != 1 || activeIO.Load() != 1 {
				t.Fatalf("期望已进入 1 个阻塞 readFile，实得 entered=%d active=%d", enteredIO.Load(), activeIO.Load())
			}

			// 内存切换为取消关联；worker 必须在 <= 300ms 内立即发出 KindUnassociated。
			src.set(memSnapshot{
				SongItemPtr:        0x100,
				CurrentSongHash:    hashA,
				CLyricPtr:          0,
				IsUserUnassociated: true,
				AssocPath:          "用户取消关联",
			}, memStatusOK)

			time.Sleep(250 * time.Millisecond)
			res := latest(t, r)
			wantKind(t, res, hashA, KindUnassociated)

			// 继续推进 2s（期间周期性 1s DB stat 到期），断言活跃阻塞 I/O goroutine 数不超过 1。
			time.Sleep(2 * time.Second)
			if maxN := maxActiveIO.Load(); maxN > 1 {
				t.Fatalf("并发活跃阻塞 I/O 协程峰值 %d，want <= 1", maxN)
			}

			// 释放阻塞门，确保旧文件读回包已被 memFileGen 作废，且 synctest 泡干净退出。
			g.release()
			synctest.Wait()
			noResult(t, r)
		})
	})
}

func TestMemPIDChangeAndBadVtableFallback(t *testing.T) {
	hashA := hex32("aa", 1)
	hashB := hex32("bb", 2)

	t.Run("PID变更关闭旧memSource并打开新PID", func(t *testing.T) {
		const (
			path101 = `D:\pid101.lrc`
			path202 = `D:\pid202.lrc`
		)
		src1 := &stubMemSource{
			pid: 101,
			snap: memSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				LyricFilePath:   path101,
			},
			status: memStatusOK,
		}
		src2 := &stubMemSource{
			pid: 202,
			snap: memSnapshot{
				SongItemPtr:     0x200,
				CurrentSongHash: hashA,
				CLyricPtr:       0x20000,
				LyricFilePath:   path202,
			},
			status: memStatusOK,
		}

		var mu sync.Mutex
		curPID := uint32(101)
		var openedPIDs []uint32

		f := &fakeDeps{
			confirmFn: func(_ int) (watchdog.Confirmed, error) {
				mu.Lock()
				pid := curPID
				mu.Unlock()
				c := okConfirmed
				c.PID = pid
				return c, nil
			},
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				mu.Lock()
				openedPIDs = append(openedPIDs, pid)
				mu.Unlock()
				if pid == 101 {
					return src1, memOpenOK, "fp-101", nil
				}
				return src2, memOpenOK, "fp-202", nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				switch path {
				case path101:
					return []byte("[00:01.00]来自PID101\n"), nil
				case path202:
					return []byte("[00:01.00]来自PID202\n"), nil
				}
				return nil, fs.ErrNotExist
			},
		}

		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash(hashA)
			res := latest(t, r)
			wantKind(t, res, hashA, KindFound)
			if len(res.Lines) != 1 || res.Lines[0].Text != "来自PID101" {
				t.Fatalf("PID 101 结果 %+v，want 来自PID101", res)
			}

			// 旧进程退出（memStatusProcessGone），新进程 PID=202 就绪。
			mu.Lock()
			curPID = 202
			mu.Unlock()
			src1.set(memSnapshot{}, memStatusProcessGone)

			// 推进 200ms 触发快照读取检测到 ProcessGone，立即关闭旧 memSource。
			time.Sleep(memPollEvery)
			synctest.Wait()
			if !src1.isClosed() {
				t.Fatal("原进程 memStatusProcessGone 后旧 memSource 未关闭")
			}

			// 推进 confirmEvery 触发重确认并打开新 PID。
			time.Sleep(confirmEvery)
			res = latest(t, r)
			wantKind(t, res, hashA, KindFound)
			if len(res.Lines) != 1 || res.Lines[0].Text != "来自PID202" {
				t.Fatalf("PID 202 结果 %+v，want 来自PID202", res)
			}

			mu.Lock()
			gotPIDs := append([]uint32(nil), openedPIDs...)
			mu.Unlock()
			if len(gotPIDs) != 2 || gotPIDs[0] != 101 || gotPIDs[1] != 202 {
				t.Fatalf("打开 PID 序列 %v，want [101 202]", gotPIDs)
			}
		})
	})

	t.Run("同PID连续3次BadVtable关闭句柄并降级读库产出候选", func(t *testing.T) {
		var openCount atomic.Int32
		src := &stubMemSource{
			pid:    okConfirmed.PID,
			status: memStatusBadVtable,
		}
		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				openCount.Add(1)
				return src, memOpenOK, "fp-bad", nil
			},
			candsFn: func(_ int, hash string) ([]candidate, error) {
				return []candidate{{Path: "a.lrc", Source: SourceCache, UpdateTime: 1}}, nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				if path == cachePath {
					return []byte("[00:01.00]来自磁盘降级\n"), nil
				}
				return nil, fs.ErrNotExist
			},
		}

		runReader(t, f, func(t *testing.T, r *Reader) {
			// 第 1 次 BadVtable（t=0）。
			r.SetHash(hashA)
			synctest.Wait()
			if src.isClosed() {
				t.Fatal("第 1 次 BadVtable 不应立即降级关闭")
			}
			noResult(t, r)

			// 第 2 次 BadVtable（+50ms）。
			time.Sleep(memFastRetry)
			synctest.Wait()
			if src.isClosed() {
				t.Fatal("第 2 次 BadVtable 不应立即降级关闭")
			}
			noResult(t, r)

			// 第 3 次 BadVtable（+100ms）：关闭句柄并立即降级读库产出候选。
			time.Sleep(memFastRetry)
			res := latest(t, r)
			wantKind(t, res, hashA, KindFound)
			if !src.isClosed() {
				t.Fatal("连续 3 次 BadVtable 后 memSource 未关闭")
			}
			if len(res.Lines) != 1 || res.Lines[0].Text != "来自磁盘降级" {
				t.Fatalf("降级结果 %+v，want 来自磁盘降级", res)
			}

			// 后续切歌继续走磁盘模式，不再重新打开内存。
			r.SetHash(hashB)
			res = latest(t, r)
			wantKind(t, res, hashB, KindFound)
			if n := openCount.Load(); n != 1 {
				t.Fatalf("降级后 openMem 被调用 %d 次，want 1", n)
			}
		})
	})

	t.Run("BadVtable间夹杂Torn或NotReady清零连续计数不降级", func(t *testing.T) {
		const memPath = `D:\mem_recovered.lrc`
		var candsCalls atomic.Int32
		src := &stubMemSource{
			pid: okConfirmed.PID,
			onRead: func(n int) (memSnapshot, memStatus) {
				switch n {
				case 1, 3, 5:
					return memSnapshot{}, memStatusBadVtable
				case 2:
					return memSnapshot{}, memStatusTorn
				case 4:
					return memSnapshot{}, memStatusNotReady
				default:
					return memSnapshot{
						SongItemPtr:     0x100,
						CurrentSongHash: hashA,
						CLyricPtr:       0x10000,
						LyricFilePath:   memPath,
						LyricOffsetMs:   500,
					}, memStatusOK
				}
			},
		}
		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src, memOpenOK, "fp-nonconsec", nil
			},
			candsFn: func(_ int, hash string) ([]candidate, error) {
				candsCalls.Add(1)
				return []candidate{{Path: "a.lrc", Source: SourceCache, UpdateTime: 1}}, nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				switch path {
				case memPath:
					return []byte("[00:01.00]内存模式恢复\n"), nil
				case cachePath:
					return []byte("[00:01.00]磁盘降级\n"), nil
				}
				return nil, fs.ErrNotExist
			},
		}

		runReader(t, f, func(t *testing.T, r *Reader) {
			// 第 1 拍：BadVtable
			r.SetHash(hashA)
			synctest.Wait()
			if src.isClosed() || candsCalls.Load() != 0 {
				t.Fatalf("第 1 拍后不应降级：closed=%v candsCalls=%d", src.isClosed(), candsCalls.Load())
			}
			noResult(t, r)

			// 第 2~5 拍：Torn -> BadVtable -> NotReady -> BadVtable，均不应触发降级。
			for step := 2; step <= 5; step++ {
				time.Sleep(memFastRetry)
				synctest.Wait()
				if src.isClosed() || candsCalls.Load() != 0 {
					t.Fatalf("第 %d 拍非连续失败后不应降级：closed=%v candsCalls=%d", step, src.isClosed(), candsCalls.Load())
				}
				noResult(t, r)
			}

			// 第 6 拍：OK，正常发出内存模式 KindFound。
			time.Sleep(memFastRetry)
			res := latest(t, r)
			wantKind(t, res, hashA, KindFound)
			if src.isClosed() || candsCalls.Load() != 0 {
				t.Fatalf("第 6 拍 OK 后不应降级：closed=%v candsCalls=%d", src.isClosed(), candsCalls.Load())
			}
			if len(res.Lines) != 1 || res.Lines[0].Text != "内存模式恢复" || res.LyricOffsetMs != 500 {
				t.Fatalf("第 6 拍结果 %+v，want 内存模式恢复 / offset=500", res)
			}
		})
	})

	t.Run("memOpenUnsupported静默退回磁盘模式且不上报遥测", func(t *testing.T) {
		reports := stubReports(t)
		var openCount atomic.Int32
		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				openCount.Add(1)
				return nil, memOpenUnsupported, "kugou.dll(ts=0x1,size=0x2)", nil
			},
		}
		out := captureLog(t, func() {
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash(hashA)
				wantKind(t, latest(t, r), hashA, KindFound)
				r.SetHash(hashB)
				wantKind(t, latest(t, r), hashB, KindFound)
			})
		})
		if n := openCount.Load(); n != 1 {
			t.Fatalf("memOpenUnsupported 后 openMem 被调用 %d 次，want 1", n)
		}
		if got := reports(); len(got) != 0 {
			t.Fatalf("memOpenUnsupported 不应上报遥测，实得 %+v", got)
		}
		if countLog(out, "Local lyric association disabled: %v") != 0 {
			t.Fatalf("不应停用读取者：\n%s", out)
		}
	})

	t.Run("memOpenNotReady达10次上限后固定磁盘模式", func(t *testing.T) {
		var openCount atomic.Int32
		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				openCount.Add(1)
				return nil, memOpenNotReady, "", nil
			},
		}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash(hashA)
			wantKind(t, latest(t, r), hashA, KindFound)
			time.Sleep(20 * time.Second)
			if n := openCount.Load(); n != maxMemNotReady {
				t.Fatalf("memOpenNotReady 调用 %d 次，want 达到上限 %d 后停止", n, maxMemNotReady)
			}
		})
	})

	t.Run("内存CLyric有行向量但无文件路径时产出HasMemoryLyric与偏移", func(t *testing.T) {
		src := &stubMemSource{
			pid: okConfirmed.PID,
			snap: memSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				VecBegin:        0x20000,
				VecEnd:          0x20100,
				LyricOffsetMs:   -1200,
			},
			status: memStatusOK,
		}
		f := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src, memOpenOK, "fp-vec", nil
			},
		}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash(hashA)
			res := latest(t, r)
			wantKind(t, res, hashA, KindNone)
			if !res.HasMemoryLyric || res.LyricOffsetMs != -1200 {
				t.Fatalf("结果 %+v，want HasMemoryLyric=true / LyricOffsetMs=-1200", res)
			}
		})
	})
}

func TestMemDBPlayingRowTransientRetryBackoff(t *testing.T) {
	hashA := hex32("aa", 1)
	const pathA = `D:\a.lrc`

	var mu sync.Mutex
	var calls []time.Duration
	var wantPlayed int64

	src := &stubMemSource{
		pid: okConfirmed.PID,
		snap: memSnapshot{
			SongItemPtr:     0x100,
			CurrentSongHash: hashA,
			CLyricPtr:       0x10000,
			LyricFilePath:   pathA,
		},
		status: memStatusOK,
	}

	var f *fakeDeps
	f = &fakeDeps{
		openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
			return src, memOpenOK, "fp-db-retry", nil
		},
		statFn: func(_ time.Duration) (int64, time.Time, error) {
			return 1 << 20, time.Unix(1000, 0), nil
		},
		playingRowFn: func(_ context.Context, _ string, _ string) (playing, error) {
			mu.Lock()
			calls = append(calls, f.since())
			n := len(calls)
			p := wantPlayed
			mu.Unlock()
			if n <= 2 {
				return playing{}, errTransientStub
			}
			return playing{played: p, path: "歌手 - A.mp3"}, nil
		},
		readFn: func(_ time.Duration, path string) ([]byte, error) {
			if path == pathA {
				return []byte("[00:01.00]歌A\n"), nil
			}
			return nil, fs.ErrNotExist
		},
	}

	getCalls := func() []time.Duration {
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}

	runReader(t, f, func(t *testing.T, r *Reader) {
		mu.Lock()
		wantPlayed = fileTime(time.Now())
		t1 := wantPlayed
		mu.Unlock()

		// t=0s：首拍 playingRow 失败，仍先发出歌词（PlayedAt=0），并退避 1s。
		r.SetHash(hashA)
		res := latest(t, r)
		wantKind(t, res, hashA, KindFound)
		if res.PlayedAt != 0 {
			t.Fatalf("首拍失败时 PlayedAt=%d，want 0", res.PlayedAt)
		}
		if got := getCalls(); !slices.Equal(got, []time.Duration{0}) {
			t.Fatalf("t=0s 调用时刻 %v，want [0s]", got)
		}

		// t=1s：第 2 次重试仍失败，退避翻倍至 2s（下次在 3s）。
		time.Sleep(time.Second)
		noResult(t, r)
		if got := getCalls(); !slices.Equal(got, []time.Duration{0, time.Second}) {
			t.Fatalf("t=1s 调用时刻 %v，want [0s 1s]", got)
		}

		// t=2s：处于 2s 退避窗口内，不应无退避连打。
		time.Sleep(time.Second)
		noResult(t, r)
		if got := getCalls(); !slices.Equal(got, []time.Duration{0, time.Second}) {
			t.Fatalf("t=2s 调用时刻 %v，want 仍为 [0s 1s]", got)
		}

		// t=3s：第 3 次重试成功，立即补发携带 PlayedAt=T1 的结果。
		time.Sleep(time.Second)
		res = latest(t, r)
		wantKind(t, res, hashA, KindFound)
		if res.PlayedAt != t1 {
			t.Fatalf("恢复后结果 PlayedAt=%d，want %d", res.PlayedAt, t1)
		}
		if got := getCalls(); !slices.Equal(got, []time.Duration{0, time.Second, 3 * time.Second}) {
			t.Fatalf("t=3s 调用时刻 %v，want [0s 1s 3s]", got)
		}

		// 成功后库 stat 不变，继续推进 5s 不应再重复查 playingRow。
		time.Sleep(5 * time.Second)
		noResult(t, r)
		if got := getCalls(); len(got) != 3 {
			t.Fatalf("成功后又调用了 playingRow，调用时刻 %v，want 仍为 3 次", got)
		}
	})
}

func TestMemDBPlayingRowSlowFailureRespectsBackoff(t *testing.T) {
	hashA := hex32("aa", 1)
	const pathA = `D:\a.lrc`

	var mu sync.Mutex
	var calls []time.Duration
	var wantPlayed int64

	src := &stubMemSource{
		pid: okConfirmed.PID,
		snap: memSnapshot{
			SongItemPtr:     0x100,
			CurrentSongHash: hashA,
			CLyricPtr:       0x10000,
			LyricFilePath:   pathA,
		},
		status: memStatusOK,
	}

	var f *fakeDeps
	f = &fakeDeps{
		openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
			return src, memOpenOK, "fp-db-slow-retry", nil
		},
		statFn: func(_ time.Duration) (int64, time.Time, error) {
			return 1 << 20, time.Unix(1000, 0), nil
		},
		playingRowFn: func(_ context.Context, _ string, _ string) (playing, error) {
			mu.Lock()
			calls = append(calls, f.since())
			n := len(calls)
			p := wantPlayed
			mu.Unlock()
			switch n {
			case 1:
				return playing{}, errTransientStub
			case 2:
				// 模拟慢查询耗时 1.5s（超过 statEvery=1s），于 t=2.5s 失败返回。
				time.Sleep(1500 * time.Millisecond)
				return playing{}, errTransientStub
			default:
				return playing{played: p, path: "歌手 - A.mp3"}, nil
			}
		},
		readFn: func(_ time.Duration, path string) ([]byte, error) {
			if path == pathA {
				return []byte("[00:01.00]歌A\n"), nil
			}
			return nil, fs.ErrNotExist
		},
	}

	getCalls := func() []time.Duration {
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}

	runReader(t, f, func(t *testing.T, r *Reader) {
		mu.Lock()
		wantPlayed = fileTime(time.Now())
		t1 := wantPlayed
		mu.Unlock()

		// t=0s：首拍 playingRow 立即失败（退避 1s）。
		r.SetHash(hashA)
		res := latest(t, r)
		wantKind(t, res, hashA, KindFound)
		if res.PlayedAt != 0 {
			t.Fatalf("首拍失败时 PlayedAt=%d，want 0", res.PlayedAt)
		}
		if got := getCalls(); !slices.Equal(got, []time.Duration{0}) {
			t.Fatalf("t=0s 调用时刻 %v，want [0s]", got)
		}

		// t=2.0s：第 2 次查询于 t=1s 发起且仍在执行中，不得提前排队或发起第 3 次。
		time.Sleep(2 * time.Second)
		noResult(t, r)
		if got := getCalls(); !slices.Equal(got, []time.Duration{0, time.Second}) {
			t.Fatalf("t=2.0s 调用时刻 %v，want [0s 1s]", got)
		}

		// t=2.5s：第 2 次查询失败返回，退避翻倍至 2s（下次不得早于 4.5s）。
		time.Sleep(500 * time.Millisecond)
		noResult(t, r)
		if got := getCalls(); !slices.Equal(got, []time.Duration{0, time.Second}) {
			t.Fatalf("t=2.5s 调用时刻 %v，want [0s 1s]（不得在慢失败返回后立即重打）", got)
		}

		// t=3.5s：仍处于 2s 退避窗口内。
		time.Sleep(time.Second)
		noResult(t, r)
		if got := getCalls(); !slices.Equal(got, []time.Duration{0, time.Second}) {
			t.Fatalf("t=3.5s 调用时刻 %v，want [0s 1s]", got)
		}

		// t=4.4s：退避仍差 100ms 未到期。
		time.Sleep(900 * time.Millisecond)
		noResult(t, r)
		if got := getCalls(); !slices.Equal(got, []time.Duration{0, time.Second}) {
			t.Fatalf("t=4.4s 调用时刻 %v，want [0s 1s]", got)
		}

		// t=4.5s：退避到期发起第 3 次调用并成功补发 PlayedAt=T1。
		time.Sleep(100 * time.Millisecond)
		res = latest(t, r)
		wantKind(t, res, hashA, KindFound)
		if res.PlayedAt != t1 {
			t.Fatalf("恢复后结果 PlayedAt=%d，want %d", res.PlayedAt, t1)
		}
		if got := getCalls(); !slices.Equal(got, []time.Duration{0, time.Second, 4500 * time.Millisecond}) {
			t.Fatalf("t=4.5s 调用时刻 %v，want [0s 1s 4.5s]", got)
		}

		// 成功后库 stat 不变，继续推进 5s 不应再重查。
		time.Sleep(5 * time.Second)
		noResult(t, r)
		if got := getCalls(); len(got) != 3 {
			t.Fatalf("成功后又调用了 playingRow，调用时刻 %v，want 仍为 3 次", got)
		}
	})
}

func TestMemDiskIOGlobalSingleFlightAcrossSessions(t *testing.T) {
	hash1 := hex32("aa", 1)
	hash2 := hex32("bb", 2)
	const (
		stuckPath = `\\nas\stuck\s1.lrc`
		s2Path    = `D:\s2.lrc`
	)

	synctest.Test(t, func(t *testing.T) {
		var g gate
		g.arm()
		defer g.release()

		var activeReads, maxActiveReads, s2Reads atomic.Int32
		trackEnter := func() {
			cur := activeReads.Add(1)
			for {
				prev := maxActiveReads.Load()
				if cur <= prev || maxActiveReads.CompareAndSwap(prev, cur) {
					break
				}
			}
		}

		// 会话 1：网络盘 readFile 持续阻塞。
		src1 := &stubMemSource{
			pid: okConfirmed.PID,
			snap: memSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hash1,
				CLyricPtr:       0x10000,
				AssocPath:       stuckPath,
				LyricFilePath:   stuckPath,
			},
			status: memStatusOK,
		}
		f1 := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src1, memOpenOK, "fp-s1", nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				trackEnter()
				defer activeReads.Add(-1)
				g.wait()
				return []byte(testLRC), nil
			},
		}
		ctx1, cancel1 := context.WithCancel(context.Background())
		defer cancel1()
		f1.t0 = time.Now()
		r1 := start(ctx1, f1.deps())
		if r1 == nil {
			t.Fatal("会话 1 start 返回 nil")
		}
		synctest.Wait()

		r1.SetHash(hash1)
		synctest.Wait()
		if activeReads.Load() != 1 || diskIOActive.Load() != 1 {
			t.Fatalf("会话 1 阻塞时 activeReads=%d diskIOActive=%d，want 1 / 1", activeReads.Load(), diskIOActive.Load())
		}

		// 取消会话 1 但暂不释放门闩：worker 退出（readers==0），旧 diskIO 仍占用全局名额（diskIOActive==1）。
		cancel1()
		synctest.Wait()
		if readers.Load() != 0 || diskIOActive.Load() != 1 {
			t.Fatalf("取消会话 1 后 readers=%d diskIOActive=%d，want 0 / 1", readers.Load(), diskIOActive.Load())
		}

		// 启动会话 2 并写入新歌 hash2（需读本地文件 s2Path）。
		src2 := &stubMemSource{
			pid: okConfirmed.PID,
			snap: memSnapshot{
				SongItemPtr:     0x200,
				CurrentSongHash: hash2,
				CLyricPtr:       0x20000,
				AssocPath:       s2Path,
				LyricFilePath:   s2Path,
			},
			status: memStatusOK,
		}
		f2 := &fakeDeps{
			openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
				return src2, memOpenOK, "fp-s2", nil
			},
			readFn: func(_ time.Duration, path string) ([]byte, error) {
				trackEnter()
				defer activeReads.Add(-1)
				s2Reads.Add(1)
				if path == s2Path {
					return []byte("[00:01.00]会话2歌词\n"), nil
				}
				return nil, fs.ErrNotExist
			},
		}
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		f2.t0 = time.Now()
		r2 := start(ctx2, f2.deps())
		if r2 == nil {
			t.Fatal("会话 2 start 返回 nil")
		}
		synctest.Wait()

		r2.SetHash(hash2)
		time.Sleep(memPollEvery)
		synctest.Wait()
		if maxActiveReads.Load() > 1 || s2Reads.Load() != 0 {
			t.Fatalf("旧 I/O 未退出期间不得新启读文件：maxActiveReads=%d s2Reads=%d", maxActiveReads.Load(), s2Reads.Load())
		}

		// 旧 I/O 阻塞期间，会话 2 仍须在 <= 300ms 内响应内存 KindUnassociated。
		src2.set(memSnapshot{
			SongItemPtr:        0x200,
			CurrentSongHash:    hash2,
			CLyricPtr:          0,
			IsUserUnassociated: true,
			AssocPath:          "用户取消关联",
		}, memStatusOK)
		time.Sleep(250 * time.Millisecond)
		wantKind(t, latest(t, r2), hash2, KindUnassociated)

		// 释放会话 1 门闩，确认旧 I/O 干净退出、diskIOActive 归零。
		g.release()
		synctest.Wait()
		if diskIOActive.Load() != 0 || activeReads.Load() != 0 {
			t.Fatalf("释放门闩后 diskIOActive=%d activeReads=%d，want 0 / 0", diskIOActive.Load(), activeReads.Load())
		}

		// 恢复会话 2 为本地文件，下一次轮询自动拉起文件读取并产出 KindFound。
		src2.set(memSnapshot{
			SongItemPtr:     0x200,
			CurrentSongHash: hash2,
			CLyricPtr:       0x20000,
			AssocPath:       s2Path,
			LyricFilePath:   s2Path,
		}, memStatusOK)
		time.Sleep(250 * time.Millisecond)
		res := latest(t, r2)
		wantKind(t, res, hash2, KindFound)
		if s2Reads.Load() != 1 || len(res.Lines) != 1 || res.Lines[0].Text != "会话2歌词" {
			t.Fatalf("会话 2 恢复结果 %+v s2Reads=%d，want 会话2歌词 / s2Reads=1", res, s2Reads.Load())
		}
		if maxActiveReads.Load() > 1 {
			t.Fatalf("全程活跃 readFile 并发峰值 %d，want <= 1", maxActiveReads.Load())
		}

		// 取消会话 2，确认 readers 与 diskIOActive 全部归零。
		cancel2()
		synctest.Wait()
		if readers.Load() != 0 || diskIOActive.Load() != 0 {
			t.Fatalf("取消会话 2 后 readers=%d diskIOActive=%d，want 0 / 0", readers.Load(), diskIOActive.Load())
		}
	})
}

func TestMemFileRetryBlockedStillPublishesOffsetUpdates(t *testing.T) {
	hashA := hex32("aa", 1)
	const retryPath = `\\nas\retry\a.lrc`

	var g gate
	retryEntered := make(chan struct{})
	var readCalls atomic.Int32

	src := &stubMemSource{
		pid: okConfirmed.PID,
		snap: memSnapshot{
			SongItemPtr:     0x100,
			CurrentSongHash: hashA,
			CLyricPtr:       0x10000,
			VecBegin:        0x20000,
			VecEnd:          0x20100,
			AssocPath:       retryPath,
			LyricFilePath:   retryPath,
			LyricOffsetMs:   0,
		},
		status: memStatusOK,
	}

	f := &fakeDeps{
		openMemFn: func(pid uint32) (memSource, memOpenKind, string, error) {
			return src, memOpenOK, "fp-retry-offset", nil
		},
		readFn: func(_ time.Duration, path string) ([]byte, error) {
			if path != retryPath {
				return nil, fs.ErrNotExist
			}
			switch readCalls.Add(1) {
			case 1:
				return nil, fs.ErrNotExist
			default:
				close(retryEntered)
				g.wait()
				return []byte("[00:01.00]重读恢复歌词\n"), nil
			}
		},
	}

	runReader(t, f, func(t *testing.T, r *Reader) {
		g.arm()
		defer g.release()

		// 首次 readFile 暂时失败：缓存 retry=true 并发出 KindNone{HasMemoryLyric: true, Retry: true, LyricOffsetMs: 0}。
		r.SetHash(hashA)
		res := latest(t, r)
		if res.Hash != hashA || res.Kind != KindNone || len(res.Lines) != 0 || res.Source != 0 ||
			!res.HasMemoryLyric || !res.Retry || res.LyricOffsetMs != 0 {
			t.Fatalf("首次失败结果 %+v，want KindNone{Hash:%s, HasMemoryLyric:true, Retry:true, LyricOffsetMs:0}", res, hashA)
		}

		// 推进 5s 到达重试期限，令第 2 次 readFile 进入门闩阻塞。
		time.Sleep(retryEvery)
		<-retryEntered
		synctest.Wait()
		if got := readCalls.Load(); got != 2 {
			t.Fatalf("5s 重试到期后 readFile 调用 %d 次，want 2", got)
		}

		// 文件重读阻塞期间内存偏移改为 1500ms：<= 300ms 内仍须发布最新偏移。
		src.set(memSnapshot{
			SongItemPtr:     0x100,
			CurrentSongHash: hashA,
			CLyricPtr:       0x10000,
			VecBegin:        0x20000,
			VecEnd:          0x20100,
			AssocPath:       retryPath,
			LyricFilePath:   retryPath,
			LyricOffsetMs:   1500,
		}, memStatusOK)

		time.Sleep(250 * time.Millisecond)
		res = latest(t, r)
		if res.Hash != hashA || res.Kind != KindNone || len(res.Lines) != 0 || res.Source != 0 ||
			!res.HasMemoryLyric || !res.Retry || res.LyricOffsetMs != 1500 {
			t.Fatalf("重试阻塞期间偏移更新结果 %+v，want KindNone{Hash:%s, HasMemoryLyric:true, Retry:true, LyricOffsetMs:1500}", res, hashA)
		}

		// 释放门闩返回合法 LRC：收到 KindFound{LyricOffsetMs: 1500}。
		g.release()
		synctest.Wait()
		res = latest(t, r)
		wantKind(t, res, hashA, KindFound)
		if res.LyricOffsetMs != 1500 || len(res.Lines) != 1 || res.Lines[0].Text != "重读恢复歌词" {
			t.Fatalf("门闩释放后结果 %+v，want KindFound{LyricOffsetMs:1500, 文本:重读恢复歌词}", res)
		}
	})
}
