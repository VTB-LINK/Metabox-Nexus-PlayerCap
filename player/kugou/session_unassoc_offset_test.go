package kugou

// 本文件专测酷狗会话侧 #250/#251 全链路行为（设计 §6.4.4、§6.5、§7）：
//   1. 换歌拍在线取词阻塞期间（含在飞 cdpProbe GetPlayInfo 同时阻塞 1s）被 KindUnassociated 零等待抢占，
//      且后续经 nextPlayInfo 串行接管在飞 GetPlayInfo（零并发调用），迟到在线结果绝不重显；
//   2. 换歌拍在线取词阻塞期间经单飞 cdpProbe 持续发现切歌（含 pollMs == 0 不 panic），取消旧歌请求、切换目标、
//      新歌取消关联即时清词；以及 cdpProbe 在飞期间旧歌在线先完成时暂存不提前广播，释放后发现切歌直接丢弃；
//   3. 换歌拍在线取词阻塞期间收到 KindDisabled 不发生 nil panic，照常采用在线歌词，下首歌不再等读取者；
//   4. 建立 bound > 0 后本地歌曲取消关联，再经 KindNone{HasMemoryLyric: true, LocalFile: true} 解除取消关联并
//      触发单飞在线恢复（叠加酷狗偏移与 kugou-offset）；恢复在飞期间再取消关联或切歌时迟到返回被代次丢弃；
//   5. 播放中本地 KindFound 仅偏移变化、已有 rawLines 收到连续 KindNone 偏移变化均重发且不发 playback_resume；
//      大额正偏移钳零后切入同名伴唱（新偏移 0ms）复用 rawLines 时首行恢复原时间戳；
//   6. 接入真实内存模式 *assoc.Reader 到 runSession，验证历史旧 P 过滤、同名无词本地音频不沿用及迟到清词、
//      以及单曲循环重播同 hash 本地文件变为无词时经持续 1s stat 检出并恰好清词一次。
//
// 变异自证（均实测变红后还原）：
//   - 去掉 localNone 的 !res.HasMemoryLyric → TestSessionLocalSongUnassociateThenMemoryLyricRecoveryWithBound/沿用旧词的本地歌收到HasMemoryLyric不被localNone误清且跟随偏移 红；
//   - 去掉 local.results() 的 s.r == nil 判断 → TestSessionOnlineWaitHandlesReaderDisabled nil deref panic 红；
//   - 将 pollNone 的 offsetChanged 挪到改写 currentLyricOffsetMs 之后 → TestSessionMidSongOffsetUpdatesForLocalAndMemoryLyrics/已有rawLines时连续KindNone偏移变化每次重发且不重取词 红；
//   - buildDisplayLyrics 去掉 cloneLines → TestSessionMidSongOffsetUpdatesForLocalAndMemoryLyrics 后两个子用例与 TestShiftTimelineAndCloneLines 红；
//   - 去掉 assoc/reader.go 的 p.played >= fileTime(w.songStart.Add(-playGrace)) → assoc.TestMemPlayedAtRejectsStaleHistoryAndTracksSameHashLoopReplay 红。

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// stubEmptyCoverAPI 将 kugouAPIClient 替换为返回空 JSON 的本地桩，防止 synctest 泡内联网。
func stubEmptyCoverAPI(t *testing.T) {
	t.Helper()
	origAPI := kugouAPIClient
	kugouAPIClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: r}, nil
	})}
	t.Cleanup(func() { kugouAPIClient = origAPI })
}

// stubFetchCtx 接管 fetchLyricsCtx 并确保 fetchLyrics 为 nil，测试结束自动还原。
func stubFetchCtx(t *testing.T, fn func(ctx context.Context, hash string, durationMs int, name, singer string) ([]klyric.Line, error)) {
	t.Helper()
	origLegacy, origCtx := fetchLyrics, fetchLyricsCtx
	fetchLyrics = nil
	fetchLyricsCtx = fn
	t.Cleanup(func() {
		fetchLyrics = origLegacy
		fetchLyricsCtx = origCtx
	})
}

// timedLyrics 构造起始于 startSec 秒（带逐字）与 30s 的两行歌词，便于核对偏移叠加后的 Timestamp 与 PlayTime。
func timedLyrics(tag string, startSec float32) []klyric.Line {
	return []klyric.Line{
		{
			Index: 0,
			Time:  startSec,
			Text:  tag + "一",
			Detailed: player.LyricTextDetailed{
				Timestamp: startSec,
				Duration:  1.0,
				Words: []player.LyricTextDetailedWord{
					{Text: tag, Timestamp: startSec, Duration: 0.5},
					{Text: "一", Timestamp: startSec + 0.5, Duration: 0.5},
				},
			},
		},
		{Index: 1, Time: 30, Text: tag + "二"},
	}
}

// hookPlayInfoSource 是可精确控制每次 GetPlayInfo 行为与并发检测的桩。
type hookPlayInfoSource struct {
	mu            sync.Mutex
	curConcurrent int
	maxConcurrent int
	callIndex     int
	fn            func(call int) (*cdp.PlayInfo, error)
}

func (s *hookPlayInfoSource) GetPlayInfo() (*cdp.PlayInfo, error) {
	s.mu.Lock()
	s.callIndex++
	call := s.callIndex
	s.curConcurrent++
	if s.curConcurrent > s.maxConcurrent {
		s.maxConcurrent = s.curConcurrent
	}
	fn := s.fn
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.curConcurrent--
		s.mu.Unlock()
	}()
	return fn(call)
}

func (s *hookPlayInfoSource) IsClosed() bool { return false }

func (s *hookPlayInfoSource) maxConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConcurrent
}

func hex32Hash(prefix byte, n int) string {
	s := strings.Repeat(strconv.FormatInt(int64(prefix), 16), 16)
	if len(s) > 24 {
		s = s[:24]
	}
	suffix := strconv.FormatInt(int64(n), 16)
	return s + strings.Repeat("0", 32-len(s)-len(suffix)) + suffix
}

func TestSessionUnassociatePreemptsBlockingOnlineAndCDP(t *testing.T) {
	t.Run("阻塞在线取词期间取消关联即时抢占且迟到返回不重显", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		synctest.Test(t, func(t *testing.T) {
			var ctxCanceled atomic.Bool
			lateReleased := make(chan struct{})
			stubFetchCtx(t, func(ctx context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
				select {
				case <-ctx.Done():
					ctxCanceled.Store(true)
				case <-time.After(3 * time.Second):
				}
				// 刻意再等一段时间模拟网络栈迟到返回歌词，验证即使返回歌词也绝不重显。
				time.Sleep(500 * time.Millisecond)
				close(lateReleased)
				return timedLyrics("迟到在线", 1.0), nil
			})

			start := time.Now()
			p := New(0, 1000)
			f := newFakeLocal(nil)

			// 换歌拍 300ms 超时进入在线取词，阻塞 100ms（t=400ms）时送入 KindUnassociated。
			sendAt := 400 * time.Millisecond
			go func() {
				time.Sleep(sendAt)
				f.send(assoc.Result{Hash: "A", Kind: assoc.KindUnassociated})
			}()

			var clearedAt time.Duration
			var gotAllLyrics, gotMinusOne bool
			var gotAllLyricsCount int
			var unexpectedLyrics bool
			doneEvt := make(chan struct{})
			go func() {
				defer close(doneEvt)
				for {
					select {
					case e := <-p.EventCh:
						switch d := e.Data.(type) {
						case *player.AllLyricsData:
							gotAllLyricsCount++
							if d.Count == 0 {
								gotAllLyrics = true
								clearedAt = time.Since(start)
							} else {
								unexpectedLyrics = true
							}
						case *player.LyricUpdate:
							if d.Index == -1 {
								gotMinusOne = true
							} else {
								unexpectedLyrics = true
							}
						}
					case <-p.StopCh:
						return
					}
				}
			}()

			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				if call <= 5 {
					return read("A", "playing", float64(call), 200), nil
				}
				return nil, errors.New("done")
			}}

			p.runSession(src, f.starter())
			<-lateReleased
			p.Stop()
			<-doneEvt
			synctest.Wait()

			if !ctxCanceled.Load() {
				t.Fatal("取消关联抢占后应立即取消在飞在线取词 ctx")
			}
			if !gotAllLyrics || !gotMinusOne {
				t.Fatalf("应发出 count:0 与 index:-1，实际 all_lyrics=%v minusOne=%v", gotAllLyrics, gotMinusOne)
			}
			if delay := clearedAt - sendAt; delay < 0 || delay > 300*time.Millisecond {
				t.Fatalf("取消关联清词延迟 = %v（clearedAt=%v, sendAt=%v），want <= 300ms", delay, clearedAt, sendAt)
			}
			if gotAllLyricsCount != 1 || unexpectedLyrics {
				t.Fatalf("迟到在线返回不应重显歌词：all_lyrics 次数=%d unexpected=%v", gotAllLyricsCount, unexpectedLyrics)
			}
		})
	})

	t.Run("在线取词与在飞cdpProbe同时阻塞时取消关联零等待抢占且零并发GetPlayInfo", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		var ctxCanceled atomic.Bool
		stubFetchCtx(t, func(ctx context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
			select {
			case <-ctx.Done():
				ctxCanceled.Store(true)
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
				return timedLyrics("迟到在线", 1.0), nil
			}
		})

		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			p := New(0, 100)
			f := newFakeLocal(nil)

			probeStarted := make(chan struct{})
			probeDone := make(chan struct{})
			var sendAtNS atomic.Int64
			var clearedAtNS atomic.Int64
			var gotCountZero, gotMinusOne, gotUnexpected atomic.Bool

			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				switch call {
				case 1:
					return read("A", "playing", 5, 200), nil
				case 2:
					// 等待期第一次 cdpProbe 进入 GetPlayInfo 后阻塞 1s。
					close(probeStarted)
					time.Sleep(time.Second)
					close(probeDone)
					return read("A", "playing", 6, 200), nil
				case 3, 4:
					return read("A", "playing", float64(4+call), 200), nil
				default:
					return nil, errors.New("done")
				}
			}}

			// 实时消费事件记录清词时刻。
			evtDone := make(chan struct{})
			go func() {
				defer close(evtDone)
				for {
					select {
					case e := <-p.EventCh:
						switch d := e.Data.(type) {
						case *player.AllLyricsData:
							if d.Count == 0 {
								if !gotCountZero.Swap(true) {
									clearedAtNS.Store(int64(time.Since(start)))
								}
							} else {
								gotUnexpected.Store(true)
							}
						case *player.LyricUpdate:
							if d.Index == -1 {
								gotMinusOne.Store(true)
							} else {
								gotUnexpected.Store(true)
							}
						}
					case <-p.StopCh:
						return
					}
				}
			}()

			sendDone := make(chan struct{})
			go func() {
				defer close(sendDone)
				<-probeStarted
				// cdpProbe 已在飞并阻塞 1s，此刻发送 KindUnassociated。
				sendAtNS.Store(int64(time.Since(start)))
				f.send(assoc.Result{Hash: "A", Kind: assoc.KindUnassociated})
			}()

			p.runSession(src, f.starter())
			<-probeDone
			<-sendDone
			p.Stop()
			<-evtDone
			synctest.Wait()

			if !ctxCanceled.Load() {
				t.Fatal("取消关联应取消在飞在线取词 ctx")
			}
			if !gotCountZero.Load() || !gotMinusOne.Load() {
				t.Fatalf("应发出 count:0 与 index:-1，实际 countZero=%v minusOne=%v", gotCountZero.Load(), gotMinusOne.Load())
			}
			if delay := time.Duration(clearedAtNS.Load() - sendAtNS.Load()); delay < 0 || delay > 300*time.Millisecond {
				t.Fatalf("在飞 GetPlayInfo 阻塞 1s 时取消关联清词延迟 = %v，want <= 300ms（零等待）", delay)
			}
			if mc := src.maxConcurrency(); mc != 1 {
				t.Fatalf("GetPlayInfo 最大并发数 = %d，want 1（nextPlayInfo 必须串行接管在飞探针）", mc)
			}
			if gotUnexpected.Load() {
				t.Fatal("不应发出任何非空歌词事件")
			}
		})
	})

	t.Run("换歌拍local.await收到取消关联跳过同步重读不被阻塞CDP拖延", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(ctx context.Context, _ string, _ int, _, _ string) ([]klyric.Line, error) {
			t.Error("local.await 已收到 KindUnassociated，不应发起在线取词")
			return nil, nil
		})

		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			p := New(0, 100)
			sendAt := 100 * time.Millisecond
			f := newFakeLocal(map[string][]reply{
				"A": {{sendAt, assoc.Result{Hash: "A", Kind: assoc.KindUnassociated}}},
			})

			var clearedAtNS atomic.Int64
			var gotCountZero, gotMinusOne atomic.Bool
			evtDone := make(chan struct{})
			go func() {
				defer close(evtDone)
				for {
					select {
					case e := <-p.EventCh:
						switch d := e.Data.(type) {
						case *player.AllLyricsData:
							if d.Count == 0 && !gotCountZero.Swap(true) {
								clearedAtNS.Store(int64(time.Since(start)))
							}
						case *player.LyricUpdate:
							if d.Index == -1 {
								gotMinusOne.Store(true)
							}
						}
					case <-p.StopCh:
						return
					}
				}
			}()

			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				switch call {
				case 1:
					return read("A", "playing", 5, 200), nil
				case 2:
					// 模拟 CDP 读阻塞 1s：若换歌拍取消关联仍做同步重读，清词会被拖延 1s。
					time.Sleep(time.Second)
					return read("A", "playing", 6, 200), nil
				default:
					return nil, errors.New("done")
				}
			}}

			p.runSession(src, f.starter())
			p.Stop()
			<-evtDone
			synctest.Wait()

			if !gotCountZero.Load() || !gotMinusOne.Load() {
				t.Fatalf("应发出 count:0 与 index:-1，实际 countZero=%v minusOne=%v", gotCountZero.Load(), gotMinusOne.Load())
			}
			if delay := time.Duration(clearedAtNS.Load()) - sendAt; delay < 0 || delay > 300*time.Millisecond {
				t.Fatalf("local.await 取消关联清词延迟 = %v（clearedAt=%v, sendAt=%v），want <= 300ms（不得被第 2 次阻塞 GetPlayInfo 拖延）",
					delay, time.Duration(clearedAtNS.Load()), sendAt)
			}
		})
	})

	t.Run("等待期cdpProbe已采到拖进度暂停_抢占清词复用最新样本且补发playback_pause", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(ctx context.Context, _ string, _ int, _, _ string) ([]klyric.Line, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			p := New(0, 1000)
			f := newFakeLocal(nil)

			// t=3.0s 发送 KindUnassociated 抢占（此前 1.3s、2.3s 的 cdpProbe 已读到 paused@80s）。
			sendDone := make(chan struct{})
			go func() {
				defer close(sendDone)
				time.Sleep(3 * time.Second)
				f.send(assoc.Result{Hash: "A", Kind: assoc.KindUnassociated})
			}()

			var call4AtNS atomic.Int64
			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				switch call {
				case 1:
					return read("A", "playing", 10, 200), nil
				case 2, 3:
					// 等待期 1.3s 与 2.3s 两次 cdpProbe 均读到已拖至 80s 且 paused。
					return read("A", "paused", 80, 200), nil
				case 4:
					call4AtNS.Store(int64(time.Since(start)))
					return nil, errors.New("done")
				default:
					return nil, errors.New("done")
				}
			}}

			p.runSession(src, f.starter())
			<-sendDone
			synctest.Wait()
			evts := drainAll(p)

			// 3.0s 抢占当拍不得发起额外同步 GetPlayInfo，第 4 次调用须在下一拍（t >= 3.9s）。
			if call4At := time.Duration(call4AtNS.Load()); call4At < 3900*time.Millisecond {
				t.Fatalf("抢占当拍不应增加同步 GetPlayInfo，第 4 次调用发生在 %v，want >= 3.9s", call4At)
			}

			var allPos, luPos float32 = -1, -1
			var pausePos float32 = -1
			var allIdx, luIdx, pauseIdx = -1, -1, -1
			for i, e := range evts {
				switch d := e.Data.(type) {
				case *player.AllLyricsData:
					if d.Count == 0 {
						allPos = d.Position
						allIdx = i
					}
				case *player.LyricUpdate:
					if d.Index == -1 {
						luPos = d.Position
						luIdx = i
					}
				case *player.PlaybackTimeInfo:
					if e.Type == player.EventPlaybackPause {
						pausePos = d.Position
						pauseIdx = i
					}
				}
			}
			if !approxEq(allPos, 80.0) || !approxEq(luPos, 80.0) {
				t.Fatalf("抢占清词位置应精确为 80.00s，实得 all_lyrics=%.2fs lyric_update=%.2fs", allPos, luPos)
			}
			if pauseIdx < 0 || pauseIdx <= luIdx || allIdx < 0 || !approxEq(pausePos, 80.0) {
				t.Fatalf("抢占清词后应紧随发出 playback_pause@80.00s，实得 allIdx=%d luIdx=%d pauseIdx=%d pausePos=%.2fs",
					allIdx, luIdx, pauseIdx, pausePos)
			}
		})
	})
}

func TestSessionSongSwitchDuringBlockingOnlineFetch(t *testing.T) {
	for _, pollMs := range []int{100, 0} {
		t.Run("阻塞在线取词期间切歌并取消新歌_pollMs="+strconv.Itoa(pollMs), func(t *testing.T) {
			stubEmptyCoverAPI(t)
			var canceledA atomic.Bool
			var lateADone atomic.Bool
			stubFetchCtx(t, func(ctx context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
				if hash == "A" {
					select {
					case <-ctx.Done():
						canceledA.Store(true)
					case <-time.After(3 * time.Second):
					}
					// 即使 3s 后仍迟到返回歌 A 歌词，也绝不应广播。
					time.Sleep(3 * time.Second)
					lateADone.Store(true)
					return timedLyrics("歌A迟到", 1.0), nil
				}
				return timedLyrics("歌B在线", 1.0), nil
			})

			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				p := New(0, pollMs)
				var unassocSentAtNS atomic.Int64
				f := newFakeLocal(map[string][]reply{
					"B": {{50 * time.Millisecond, assoc.Result{Hash: "B", Kind: assoc.KindUnassociated}}},
				})

				var switched atomic.Bool
				go func() {
					// A 的 local.await 300ms 超时进入在线取词，阻塞 100ms（t=400ms）后切到 B。
					time.Sleep(400 * time.Millisecond)
					switched.Store(true)
				}()

				var bClearAtNS atomic.Int64
				var gotBClear, gotBMinusOne, gotALyrics atomic.Bool
				evtDone := make(chan struct{})
				go func() {
					defer close(evtDone)
					for {
						select {
						case e := <-p.EventCh:
							switch d := e.Data.(type) {
							case *player.AllLyricsData:
								if strings.Contains(d.Title, "歌A") {
									gotALyrics.Store(true)
								}
								if strings.Contains(d.Title, "歌B") && d.Count == 0 {
									if !gotBClear.Swap(true) {
										bClearAtNS.Store(int64(time.Since(start)))
									}
								} else if d.Count > 0 {
									gotALyrics.Store(true)
								}
							case *player.LyricUpdate:
								if d.Index == -1 {
									gotBMinusOne.Store(true)
								}
							}
						case <-p.StopCh:
							return
						}
					}
				}()

				var bCalls atomic.Int32
				src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
					if !switched.Load() {
						return read("A", "playing", 1, 200), nil
					}
					bc := int(bCalls.Add(1))
					if bc == 1 {
						unassocSentAtNS.Store(int64(time.Since(start) + 50*time.Millisecond))
					}
					if bc <= 4 {
						// 推进到 3.5s 之后以覆盖歌 A 迟到返回。
						if bc == 4 {
							time.Sleep(4 * time.Second)
						}
						return read("B", "playing", float64(bc), 200), nil
					}
					return nil, errors.New("done")
				}}

				p.runSession(src, f.starter())
				p.Stop()
				<-evtDone
				synctest.Wait()

				if !canceledA.Load() {
					t.Fatal("切歌到 B 后应取消歌 A 的在线取词 ctx")
				}
				if !slices.Equal(f.set, []string{"A", "B"}) {
					t.Fatalf("SetHash = %q，want [A B]", f.set)
				}
				if gotALyrics.Load() {
					t.Fatal("绝不应广播歌 A 的任何歌词事件")
				}
				if !gotBClear.Load() || !gotBMinusOne.Load() {
					t.Fatalf("应发出歌 B 的清词事件，实际 clear=%v minusOne=%v", gotBClear.Load(), gotBMinusOne.Load())
				}
				if delay := time.Duration(bClearAtNS.Load() - unassocSentAtNS.Load()); delay < 0 || delay > 300*time.Millisecond {
					t.Fatalf("歌 B 取消关联清词延迟 = %v，want <= 300ms", delay)
				}
				if !lateADone.Load() {
					t.Fatal("应已推进过歌 A 迟到返回时点以验证不广播")
				}
			})
		})
	}

	t.Run("cdpProbe在飞期间歌A先完成则暂存_释放发现切歌B后丢弃歌A", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		synctest.Test(t, func(t *testing.T) {
			probeEntered := make(chan struct{})
			releaseProbe := make(chan struct{})
			releaseFetchA := make(chan struct{})

			stubFetchCtx(t, func(ctx context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
				if hash == "A" {
					<-releaseFetchA
					return timedLyrics("歌A在线", 1.0), nil
				}
				return timedLyrics("歌B在线", 1.0), nil
			})

			p := New(0, 50)
			f := newFakeLocal(map[string][]reply{
				"B": {{10 * time.Millisecond, found("B", timedLyrics("歌B本地", 1.0))}},
			})

			var allLyricsTitles []string
			var mu sync.Mutex
			evtDone := make(chan struct{})
			go func() {
				defer close(evtDone)
				for {
					select {
					case e := <-p.EventCh:
						if d, ok := e.Data.(*player.AllLyricsData); ok {
							mu.Lock()
							allLyricsTitles = append(allLyricsTitles, d.Title)
							mu.Unlock()
						}
					case <-p.StopCh:
						return
					}
				}
			}()

			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				switch call {
				case 1:
					return read("A", "playing", 1, 200), nil
				case 2:
					// 等待期第一次 cdpProbe 进入后阻塞，待释放时返回已切到歌 B。
					close(probeEntered)
					<-releaseProbe
					return read("B", "playing", 0, 200), nil
				case 3, 4:
					return read("B", "playing", 1, 200), nil
				default:
					return nil, errors.New("done")
				}
			}}

			var earlyCount int
			go func() {
				<-probeEntered
				// 先释放歌 A 在线取词，此时 probe 仍在飞，必须暂存歌 A 不提前广播。
				close(releaseFetchA)
				time.Sleep(100 * time.Millisecond)
				mu.Lock()
				earlyCount = len(allLyricsTitles)
				mu.Unlock()
				// 再释放 probe 返回歌 B。
				close(releaseProbe)
			}()

			p.runSession(src, f.starter())
			p.Stop()
			<-evtDone
			synctest.Wait()

			if earlyCount != 0 {
				t.Fatalf("cdpProbe 在飞期间不得提前广播歌 A 歌词，已发出 %d 条：%q", earlyCount, allLyricsTitles)
			}
			if !slices.Equal(f.set, []string{"A", "B"}) {
				t.Fatalf("SetHash = %q，want [A B]", f.set)
			}
			if len(allLyricsTitles) != 1 || !strings.Contains(allLyricsTitles[0], "歌B") {
				t.Fatalf("应丢弃歌 A 并仅广播歌 B，实际 all_lyrics titles=%q", allLyricsTitles)
			}
		})
	})
}

func TestSessionOnlineWaitHandlesReaderDisabled(t *testing.T) {
	stubEmptyCoverAPI(t)
	var fetched []string
	stubFetchCtx(t, func(ctx context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
		fetched = append(fetched, hash)
		if hash == "A" {
			time.Sleep(200 * time.Millisecond)
			return timedLyrics("A在线", 1.0), nil
		}
		return timedLyrics("B在线", 1.0), nil
	})

	synctest.Test(t, func(t *testing.T) {
		p := New(0, 1000)
		f := newFakeLocal(nil)

		// A 的 local.await 300ms 超时进入阻塞 200ms 的在线取词；阻塞 50ms（t=350ms）时送入 KindDisabled。
		go func() {
			time.Sleep(350 * time.Millisecond)
			f.send(assoc.Result{Hash: "A", Kind: assoc.KindDisabled})
		}()

		reads := slices.Concat(
			twice(read("A", "playing", 2, 200)),
			twice(read("B", "playing", 2, 200)),
		)
		p.runSession(&scriptedSource{reads: reads}, f.starter())
		synctest.Wait()
		evts := drainAll(p)

		if !slices.Equal(f.set, []string{"A"}) {
			t.Fatalf("停用后 B 不应再 SetHash，实际 SetHash=%q", f.set)
		}
		if !slices.Equal(fetched, []string{"A", "B"}) {
			t.Fatalf("在线取词应为 [A B]，实际 %q", fetched)
		}
		checkLyricEvents(t, lyricEvents(evts), []string{
			"all_lyrics@2.00:A在线一/A在线二", "lyric_update#0:A在线一",
			"all_lyrics@2.00:B在线一/B在线二", "lyric_update#0:B在线一",
		})
	})
}

func TestSessionLocalSongUnassociateThenMemoryLyricRecoveryWithBound(t *testing.T) {
	t.Run("本地歌取消关联后内存有词重选恢复并叠加偏移", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		var bFetchCalls atomic.Int32
		stubFetchCtx(t, func(ctx context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
			if hash == "B" {
				bFetchCalls.Add(1)
				return timedLyrics("B恢复", 5.0), nil
			}
			return timedLyrics("A在线", 1.0), nil
		})

		synctest.Test(t, func(t *testing.T) {
			// offsetMs = -500（offsetSec = -0.5s），与酷狗 +1500ms（提前 1.5s）叠加后：
			// 原 5.0s -> shiftTimeline(+1500ms) = 3.5s -> PlayTime = 3.0s。
			p := New(-500, 1000)
			f := newFakeLocal(map[string][]reply{
				"A": {
					// 先送早于 start-5s 的历史 P，再送有效 PlayedAt 建立 bound > 0。
					{100 * time.Millisecond, onlineNone("A", lpt(-10*time.Second))},
					{500 * time.Millisecond, onlineNone("A", lpt(0))},
				},
				"B": {
					{100 * time.Millisecond, assoc.Result{Hash: "B", Kind: assoc.KindUnassociated, LocalFile: true, PlayedAt: lpt(2 * time.Second)}},
					// 随后用户在酷狗重新选词（无可读磁盘文件，仅内存有行且带 +1500ms 偏移）。
					{1500 * time.Millisecond, assoc.Result{Hash: "B", Kind: assoc.KindNone, HasMemoryLyric: true, LocalFile: true, Retry: false, PlayedAt: lpt(2 * time.Second), LyricOffsetMs: 1500}},
					// 重复同样信号不应重复启动在线恢复。
					{2500 * time.Millisecond, assoc.Result{Hash: "B", Kind: assoc.KindNone, HasMemoryLyric: true, LocalFile: true, Retry: false, PlayedAt: lpt(2 * time.Second), LyricOffsetMs: 1500}},
				},
			})

			reads := slices.Concat(
				twice(read("A", "playing", 0, 200)),
				once(read("A", "playing", 1, 200)),
				once(read("B", "playing", 0, 200)),
				once(read("B", "playing", 1, 200)),
				once(read("B", "playing", 2, 200)),
				once(read("B", "playing", 4, 200)),
				once(read("B", "playing", 5, 200)),
			)
			p.runSession(&scriptedSource{reads: reads}, f.starter())
			synctest.Wait()
			evts := drainAll(p)

			if got := bFetchCalls.Load(); got != 1 {
				t.Fatalf("B 异步在线恢复应恰好调用 1 次，实际 %d 次", got)
			}

			var recovered *player.AllLyricsData
			var sawBClear bool
			for _, e := range evts {
				if al, ok := e.Data.(*player.AllLyricsData); ok && strings.Contains(al.Title, "歌B") {
					if al.Count == 0 {
						sawBClear = true
					} else {
						recovered = al
					}
				}
			}
			if !sawBClear {
				t.Fatal("B 应先发出取消关联清词 all_lyrics(count=0)")
			}
			if recovered == nil || len(recovered.Lyrics) != 2 || recovered.Lyrics[0].Text != "B恢复一" {
				t.Fatalf("B 应恢复显示在线歌词，实得 %+v", recovered)
			}
			line0 := recovered.Lyrics[0]
			if !approxEq(line0.Timestamp, 3.5) || !approxEq(line0.PlayTime, 3.0) {
				t.Fatalf("B 恢复首行 Timestamp=%.2f PlayTime=%.2f，want 3.50 / 3.00", line0.Timestamp, line0.PlayTime)
			}
			if w := line0.TextDetailed.Words[0]; !approxEq(w.Timestamp, 3.5) || !approxEq(w.PlayTime, 3.0) {
				t.Fatalf("B 恢复首字 Timestamp=%.2f PlayTime=%.2f，want 3.50 / 3.00", w.Timestamp, w.PlayTime)
			}
		})
	})

	t.Run("沿用旧词的本地歌收到HasMemoryLyric不被localNone误清且跟随偏移", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(_ context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
			if hash == "A" {
				return timedLyrics("A词", 5.0), nil
			}
			return nil, nil
		})

		synctest.Test(t, func(t *testing.T) {
			p := New(0, 1000)
			f := newFakeLocal(map[string][]reply{
				"A": {{100 * time.Millisecond, onlineNone("A", lpt(0))}},
				"XB": {
					// 换歌拍超时沿用 A（carried=true），播放中收到 HasMemoryLyric=true + LocalFile=true + PlayedAt 满足 ownPlay。
					{1500 * time.Millisecond, assoc.Result{Hash: "XB", Kind: assoc.KindNone, HasMemoryLyric: true, LocalFile: true, PlayedAt: lpt(2 * time.Second), LyricOffsetMs: 1500}},
				},
			})
			reads := slices.Concat(
				beats(sameName, "A", 2)[0],
				beats(sameName, "A", 2)[1],
				beats(sameName, "XB", 3)[0],
				beats(sameName, "XB", 3)[1],
				beats(sameName, "XB", 3)[2],
			)
			p.runSession(&scriptedSource{reads: reads}, f.starter())
			synctest.Wait()
			evts := drainAll(p)

			var all []*player.AllLyricsData
			for _, e := range evts {
				if al, ok := e.Data.(*player.AllLyricsData); ok {
					all = append(all, al)
				}
			}
			// 期望 3 次 all_lyrics：A(5.0s)、XB 换歌拍沿用 A(5.0s)、XB 播放中收到 HasMemoryLyric +1500ms 重算(3.5s)，绝不清词。
			if len(all) != 3 || all[2].Count != 2 || !approxEq(all[2].Lyrics[0].Timestamp, 3.5) {
				t.Fatalf("HasMemoryLyric 不应触发 localNone 清词而应按 +1500ms 重算，实得 all_lyrics=%+v（事件=%q）", all, lyricEvents(evts))
			}
		})
	})

	t.Run("在线恢复在飞期间再次取消关联时迟到返回被代次丢弃", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		synctest.Test(t, func(t *testing.T) {
			fetchStarted := make(chan struct{})
			releaseFetch := make(chan struct{})
			var fetchCanceled atomic.Bool

			stubFetchCtx(t, func(ctx context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
				if hash == "B" {
					close(fetchStarted)
					select {
					case <-ctx.Done():
						fetchCanceled.Store(true)
					case <-releaseFetch:
					}
					// 即使返回歌词也不得再显示。
					return timedLyrics("B迟到恢复", 1.0), nil
				}
				return nil, nil
			})

			p := New(0, 1000)
			f := newFakeLocal(map[string][]reply{
				"B": {
					{100 * time.Millisecond, assoc.Result{Hash: "B", Kind: assoc.KindUnassociated}},
					{1500 * time.Millisecond, assoc.Result{Hash: "B", Kind: assoc.KindNone, HasMemoryLyric: true, LyricOffsetMs: 1000}},
				},
			})

			go func() {
				<-fetchStarted
				// 恢复在飞期间再次点击取消关联。
				f.send(assoc.Result{Hash: "B", Kind: assoc.KindUnassociated})
				time.Sleep(1500 * time.Millisecond)
				close(releaseFetch)
			}()

			reads := slices.Concat(
				once(read("B", "playing", 0, 200)),
				once(read("B", "playing", 1, 200)),
				once(read("B", "playing", 2, 200)),
				once(read("B", "playing", 3, 200)),
				once(read("B", "playing", 4, 200)),
				once(read("B", "playing", 5, 200)),
			)
			p.runSession(&scriptedSource{reads: reads}, f.starter())
			synctest.Wait()
			evts := drainAll(p)

			if !fetchCanceled.Load() {
				t.Fatal("再次取消关联时应取消在飞的恢复请求 ctx")
			}
			for _, e := range evts {
				if al, ok := e.Data.(*player.AllLyricsData); ok && al.Count > 0 {
					t.Fatalf("被作废的迟到恢复结果绝不应广播，实得 %+v", al)
				}
			}
		})
	})

	t.Run("在线恢复在飞期间切歌C时迟到返回不串入C", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		synctest.Test(t, func(t *testing.T) {
			fetchBStarted := make(chan struct{})
			releaseFetchB := make(chan struct{})
			var fetchBCanceled atomic.Bool

			stubFetchCtx(t, func(ctx context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
				if hash == "B" {
					close(fetchBStarted)
					select {
					case <-ctx.Done():
						fetchBCanceled.Store(true)
					case <-releaseFetchB:
					}
					return timedLyrics("B串台歌词", 1.0), nil
				}
				return nil, nil
			})

			p := New(0, 1000)
			f := newFakeLocal(map[string][]reply{
				"B": {
					{100 * time.Millisecond, assoc.Result{Hash: "B", Kind: assoc.KindUnassociated}},
					{1500 * time.Millisecond, assoc.Result{Hash: "B", Kind: assoc.KindNone, HasMemoryLyric: true}},
				},
				"C": {
					{100 * time.Millisecond, assoc.Result{Hash: "C", Kind: assoc.KindNone}},
				},
			})

			var switchToC atomic.Bool
			go func() {
				<-fetchBStarted
				switchToC.Store(true)
				time.Sleep(1500 * time.Millisecond)
				close(releaseFetchB)
			}()

			cCalls := 0
			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				if !switchToC.Load() {
					return read("B", "playing", float64(call), 200), nil
				}
				cCalls++
				if cCalls <= 4 {
					return read("C", "playing", float64(cCalls), 200), nil
				}
				return nil, errors.New("done")
			}}

			p.runSession(src, f.starter())
			synctest.Wait()
			evts := drainAll(p)

			if !fetchBCanceled.Load() {
				t.Fatal("切歌到 C 时应取消 B 在飞的在线恢复请求 ctx")
			}
			for _, e := range evts {
				if al, ok := e.Data.(*player.AllLyricsData); ok && al.Count > 0 {
					t.Fatalf("歌 B 迟到恢复歌词不应串入歌 C，实得 %+v", al)
				}
			}
		})
	})
}

func TestSessionMidSongOffsetUpdatesForLocalAndMemoryLyrics(t *testing.T) {
	t.Run("本地KindFound仅偏移变化重发且不发playback_resume", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(context.Context, string, int, string, string) ([]klyric.Line, error) {
			return nil, nil
		})

		synctest.Test(t, func(t *testing.T) {
			p := New(-500, 1000)
			f := newFakeLocal(map[string][]reply{
				"A": {
					{100 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindFound, Lines: timedLyrics("本地", 5.0), Source: assoc.SourceAssociation, LyricOffsetMs: 0}},
					{1500 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindFound, Lines: timedLyrics("本地", 5.0), Source: assoc.SourceAssociation, LyricOffsetMs: 1500}},
				},
			})

			reads := slices.Concat(
				twice(read("A", "playing", 4.0, 200)),
				once(read("A", "playing", 4.0, 200)),
				once(read("A", "playing", 4.0, 200)),
			)
			p.runSession(&scriptedSource{reads: reads}, f.starter())
			synctest.Wait()
			evts := drainAll(p)

			var all []*player.AllLyricsData
			var updates []*player.LyricUpdate
			for _, e := range evts {
				switch d := e.Data.(type) {
				case *player.AllLyricsData:
					all = append(all, d)
				case *player.LyricUpdate:
					updates = append(updates, d)
				case *player.PlaybackTimeInfo:
					if e.Type == player.EventPlaybackResume || e.Type == player.EventPlaybackPause {
						t.Fatalf("偏移更新不应发 %s", e.Type)
					}
				}
			}
			if len(all) != 2 {
				t.Fatalf("应发出 2 次 all_lyrics（初始 0ms + 变更 +1500ms），实得 %d 次", len(all))
			}
			if !approxEq(all[0].Lyrics[0].Timestamp, 5.0) || !approxEq(all[0].Lyrics[0].PlayTime, 4.5) {
				t.Fatalf("首次首行 Timestamp=%.2f PlayTime=%.2f，want 5.00 / 4.50", all[0].Lyrics[0].Timestamp, all[0].Lyrics[0].PlayTime)
			}
			if !approxEq(all[1].Lyrics[0].Timestamp, 3.5) || !approxEq(all[1].Lyrics[0].PlayTime, 3.0) {
				t.Fatalf("偏移 +1500ms 后首行 Timestamp=%.2f PlayTime=%.2f，want 3.50 / 3.00", all[1].Lyrics[0].Timestamp, all[1].Lyrics[0].PlayTime)
			}
			// 位置 4.0s、offsetSec -0.5s -> matchTime = 4.5s；首次首行 5.0s 未命中，偏移后首行 3.5s 命中并补发 lyric_update#0。
			if len(updates) != 1 || updates[0].Index != 0 || !approxEq(updates[0].Timestamp, 3.5) {
				t.Fatalf("偏移更新后应按新时间轴补发第 0 行 lyric_update，实得 %+v", updates)
			}
		})
	})

	t.Run("已有rawLines时连续KindNone偏移变化每次重发且不重取词", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		var fetchCalls atomic.Int32
		stubFetchCtx(t, func(context.Context, string, int, string, string) ([]klyric.Line, error) {
			fetchCalls.Add(1)
			return timedLyrics("在线", 5.0), nil
		})

		synctest.Test(t, func(t *testing.T) {
			p := New(0, 1000)
			f := newFakeLocal(map[string][]reply{
				"A": {
					{1500 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindNone, HasMemoryLyric: true, LyricOffsetMs: 1500}},
					{2500 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindNone, HasMemoryLyric: true, LyricOffsetMs: 2000}},
				},
			})

			reads := slices.Concat(
				twice(read("A", "playing", 6.0, 200)),
				once(read("A", "playing", 6.0, 200)),
				once(read("A", "playing", 6.0, 200)),
				once(read("A", "playing", 6.0, 200)),
			)
			p.runSession(&scriptedSource{reads: reads}, f.starter())
			synctest.Wait()
			evts := drainAll(p)

			if fetchCalls.Load() != 1 {
				t.Fatalf("应仅在换歌拍在线取词 1 次，实际 %d 次", fetchCalls.Load())
			}
			var allTS []float32
			var updateCount int
			for _, e := range evts {
				switch d := e.Data.(type) {
				case *player.AllLyricsData:
					allTS = append(allTS, d.Lyrics[0].Timestamp)
				case *player.LyricUpdate:
					updateCount++
				case *player.PlaybackTimeInfo:
					if e.Type == player.EventPlaybackResume || e.Type == player.EventPlaybackPause {
						t.Fatalf("偏移更新不应发 %s", e.Type)
					}
				}
			}
			if len(allTS) != 3 || !approxEq(allTS[0], 5.0) || !approxEq(allTS[1], 3.5) || !approxEq(allTS[2], 3.0) {
				t.Fatalf("三次 all_lyrics 首行 Timestamp = %v，want [5.0 3.5 3.0]", allTS)
			}
			if updateCount != 3 {
				t.Fatalf("每次重算应重置行号并发出 lyric_update，实得 %d 次，want 3", updateCount)
			}
		})
	})

	t.Run("歌首1s在大额正偏移+2000ms钳零后切同名伴唱0ms首行恢复1s", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(_ context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
			return nil, nil
		})

		synctest.Test(t, func(t *testing.T) {
			p := New(0, 1000)
			f := newFakeLocal(map[string][]reply{
				"A":  {{100 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindFound, Lines: timedLyrics("原唱", 1.0), Source: assoc.SourceAssociation, LyricOffsetMs: 2000}}},
				"XA": {{100 * time.Millisecond, onlineNone("XA", lpt(2*time.Second))}},
			})

			reads := slices.Concat(
				beats(sameName, "A", 1)[0],
				beats(sameName, "XA", 1)[0],
			)
			p.runSession(&scriptedSource{reads: reads}, f.starter())
			synctest.Wait()
			evts := drainAll(p)

			var all []*player.AllLyricsData
			for _, e := range evts {
				if al, ok := e.Data.(*player.AllLyricsData); ok {
					all = append(all, al)
				}
			}
			if len(all) != 2 {
				t.Fatalf("应有 2 次 all_lyrics，实得 %d 次", len(all))
			}
			if !approxEq(all[0].Lyrics[0].Timestamp, 0.0) {
				t.Fatalf("歌 A +2000ms 钳零首行应为 0.0，实得 %.2f", all[0].Lyrics[0].Timestamp)
			}
			if !approxEq(all[1].Lyrics[0].Timestamp, 1.0) {
				t.Fatalf("同名伴唱复用 0ms 首行应恢复为 1.0，实得 %.2f", all[1].Lyrics[0].Timestamp)
			}
		})
	})

	t.Run("磁盘暂缺KindNone不归零已保留偏移_内存HasMemoryLyric仍可调回0ms", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(context.Context, string, int, string, string) ([]klyric.Line, error) {
			return nil, nil
		})

		synctest.Test(t, func(t *testing.T) {
			p := New(0, 1000)
			f := newFakeLocal(map[string][]reply{
				"A": {
					{100 * time.Millisecond, assoc.Result{
						Hash:          "A",
						Kind:          assoc.KindFound,
						Lines:         timedLyrics("本地", 5.0),
						Source:        assoc.SourceAssociation,
						PlayedAt:      lpt(0),
						LyricOffsetMs: 1500,
					}},
					// 磁盘模式文件暂缺且无缓存：HasMemoryLyric=false、LyricOffsetMs=0 为默认零值，绝不能把 +1500ms 归零。
					{1500 * time.Millisecond, assoc.Result{
						Hash:           "A",
						Kind:           assoc.KindNone,
						Retry:          true,
						AssocSkipped:   true,
						HasMemoryLyric: false,
						PlayedAt:       lpt(0),
						LyricOffsetMs:  0,
					}},
					// 内存模式明确报告 HasMemoryLyric=true 且 LyricOffsetMs=0：应正常调回 0ms 并重发。
					{2500 * time.Millisecond, assoc.Result{
						Hash:           "A",
						Kind:           assoc.KindNone,
						HasMemoryLyric: true,
						PlayedAt:       lpt(0),
						LyricOffsetMs:  0,
					}},
				},
			})

			reads := slices.Concat(
				twice(read("A", "playing", 1.0, 200)),
				once(read("A", "playing", 2.0, 200)),
				once(read("A", "playing", 3.0, 200)),
				once(read("A", "playing", 4.0, 200)),
			)
			p.runSession(&scriptedSource{reads: reads}, f.starter())
			synctest.Wait()
			evts := drainAll(p)

			var all []*player.AllLyricsData
			for _, e := range evts {
				if al, ok := e.Data.(*player.AllLyricsData); ok {
					all = append(all, al)
				}
			}
			// 节拍：t=0.1s(pos=1.0) 初始 +1500ms -> ts=3.5；t=1.5s(pos=2.40) 即时收 1500ms 磁盘暂缺不重发；
			// t=2.5s(pos=3.40) 即时收 2500ms 内存 HasMemoryLyric=true 调回 0ms -> ts=5.0。
			if len(all) != 2 {
				t.Fatalf("应发出恰好 2 次 all_lyrics（磁盘暂缺不重发 + 内存调回 0ms 重发），实得 %d 次（事件=%q）", len(all), lyricEvents(evts))
			}
			if !approxEq(all[0].Position, 1.0) || !approxEq(all[0].Lyrics[0].Timestamp, 3.5) {
				t.Fatalf("第 1 次 all_lyrics pos=%.2f ts=%.2f，want 1.00 / 3.50", all[0].Position, all[0].Lyrics[0].Timestamp)
			}
			if !approxEq(all[1].Position, 3.4) || !approxEq(all[1].Lyrics[0].Timestamp, 5.0) {
				t.Fatalf("第 2 次 all_lyrics 应在 3.40s（而非 2.40s 磁盘暂缺时刻）把首行调回 5.0s，实得 pos=%.2f ts=%.2f",
					all[1].Position, all[1].Lyrics[0].Timestamp)
			}
		})
	})
}

func TestSessionMemoryReaderPreservesLocalFileNoReuseClearAndSingleLoop(t *testing.T) {
	t.Run("首首歌遇过旧P再落盘真实P后切同名无词本地音频不沿用及迟到清词", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(_ context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
			return nil, nil
		})

		hashA := hex32Hash(0xaa, 1)
		hashB := hex32Hash(0xbb, 2)
		hashC := hex32Hash(0xcc, 3)
		const (
			pathA  = `D:\Lyrics\A.lrc`
			audioB = `D:\Music\B.flac`
			audioC = `D:\Music\C.flac`
		)

		synctest.Test(t, func(t *testing.T) {
			var mu sync.Mutex
			curSnap := assoc.MemTestSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				TaskID:          1,
				LyricFilePath:   pathA,
			}
			rows := map[string]assoc.MemTestPlayingRow{
				// 首首歌起初只有早于 songStart-5s 的历史旧 P。
				hashA: {PlayedAt: fileTime(time.Now().Add(-time.Hour)), Path: "歌手 - 同名歌.mp3"},
			}
			dbMtime := int64(1000)

			starter := func(ctx context.Context) localLyrics {
				r := assoc.StartMemoryForTest(ctx, assoc.MemTestConfig{
					Snapshot: func() assoc.MemTestSnapshot {
						mu.Lock()
						defer mu.Unlock()
						return curSnap
					},
					PlayingRow: func(hash string) (assoc.MemTestPlayingRow, error) {
						mu.Lock()
						defer mu.Unlock()
						return rows[hash], nil
					},
					ReadFile: func(path string) ([]byte, error) {
						if path == pathA {
							return []byte("[00:01.00]歌A词一\n[00:30.00]歌A词二\n"), nil
						}
						return nil, fs.ErrNotExist
					},
					StatDB: func() (int64, time.Time, error) {
						mu.Lock()
						sec := dbMtime
						mu.Unlock()
						return 1 << 20, time.Unix(sec, 0), nil
					},
				})
				if r == nil {
					return nil
				}
				return r
			}

			atSame := func(hash string, sec float64) *cdp.PlayInfo {
				r := read(hash, "playing", sec, 200)
				r.Filename = sameName
				return r
			}

			// 调度内存与库状态推进，配合 1s 轮询驱动真实 assoc.Reader。
			p := New(0, 1000)
			call := 0
			src := &hookPlayInfoSource{fn: func(c int) (*cdp.PlayInfo, error) {
				call++
				switch {
				case call == 1 || call == 2:
					// 歌 A 换歌拍与重读（t≈0s），库内仅有历史旧 P。
					return atSame(hashA, 0), nil
				case call == 3:
					// 1s 后刷入歌 A 的真实 P（T1）并推进 dbMtime，等 1s stat 检出补发。
					mu.Lock()
					rows[hashA] = assoc.MemTestPlayingRow{PlayedAt: fileTime(time.Now()), Path: "歌手 - 同名歌.mp3"}
					dbMtime = 1001
					mu.Unlock()
					return atSame(hashA, 1), nil
				case call >= 4 && call <= 6:
					return atSame(hashA, float64(call-2)), nil
				case call == 7:
					// 切到同名无词本地音频 B：换歌拍库内已有真实 P（T2 > T1），验证不沿用 A。
					mu.Lock()
					rows[hashB] = assoc.MemTestPlayingRow{PlayedAt: fileTime(time.Now()), Path: audioB}
					dbMtime = 1002
					curSnap = assoc.MemTestSnapshot{
						SongItemPtr:     0x200,
						CurrentSongHash: hashB,
						NoLyricFound:    true,
					}
					mu.Unlock()
					return atSame(hashB, 0), nil
				case call == 8:
					return atSame(hashB, 0), nil
				case call == 9:
					// 切回有词的 A 以便下一首 C 测试「迟到队列记录清词」。
					mu.Lock()
					rows[hashA] = assoc.MemTestPlayingRow{PlayedAt: fileTime(time.Now()), Path: "歌手 - 同名歌.mp3"}
					dbMtime = 1003
					curSnap = assoc.MemTestSnapshot{
						SongItemPtr:     0x300,
						CurrentSongHash: hashA,
						CLyricPtr:       0x20000,
						TaskID:          2,
						LyricFilePath:   pathA,
					}
					mu.Unlock()
					return atSame(hashA, 0), nil
				case call == 10 || call == 11 || call == 12:
					return atSame(hashA, float64(call-9)), nil
				case call == 13:
					// 切到同名无词本地音频 C：换歌拍库内尚无 C 的 P（模拟酷狗延迟写库），换歌拍先沿用 A。
					mu.Lock()
					curSnap = assoc.MemTestSnapshot{
						SongItemPtr:     0x400,
						CurrentSongHash: hashC,
						NoLyricFound:    true,
					}
					mu.Unlock()
					return atSame(hashC, 0), nil
				case call == 14:
					return atSame(hashC, 0), nil
				case call == 15:
					// 1s 后库内迟到落盘 C 的本地文件 P 并更新 dbMtime。
					mu.Lock()
					rows[hashC] = assoc.MemTestPlayingRow{PlayedAt: fileTime(time.Now()), Path: audioC}
					dbMtime = 1004
					mu.Unlock()
					return atSame(hashC, 1), nil
				case call >= 16 && call <= 19:
					return atSame(hashC, float64(call-14)), nil
				default:
					return nil, errors.New("done")
				}
			}}

			p.runSession(src, starter)
			synctest.Wait()
			evts := drainAll(p)

			var allCounts []int
			for _, e := range evts {
				if al, ok := e.Data.(*player.AllLyricsData); ok {
					allCounts = append(allCounts, al.Count)
				}
			}
			// 期望序列：A(2) -> B(0，不沿用A) -> A(2) -> C换歌拍沿用A(2) -> C迟到落盘清词(0)。
			wantCounts := []int{2, 0, 2, 2, 0}
			if !slices.Equal(allCounts, wantCounts) {
				t.Fatalf("all_lyrics Count 序列 = %v，want %v（事件=%q）", allCounts, wantCounts, lyricEvents(evts))
			}
		})
	})

	t.Run("播A建bound再播有词本地文件B随后同hash单曲循环无词恰好清词一次", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(_ context.Context, hash string, _ int, _, _ string) ([]klyric.Line, error) {
			return nil, nil
		})

		hashA := hex32Hash(0xaa, 11)
		hashB := hex32Hash(0xbb, 12)
		const (
			pathA  = `D:\Lyrics\A2.lrc`
			pathB  = `D:\Lyrics\B2.lrc`
			audioB = `D:\Music\LoopB.flac`
		)

		synctest.Test(t, func(t *testing.T) {
			var mu sync.Mutex
			curSnap := assoc.MemTestSnapshot{
				SongItemPtr:     0x100,
				CurrentSongHash: hashA,
				CLyricPtr:       0x10000,
				TaskID:          1,
				LyricFilePath:   pathA,
			}
			rows := map[string]assoc.MemTestPlayingRow{
				hashA: {PlayedAt: fileTime(time.Now()), Path: "歌手 - 歌A.mp3"},
			}
			dbMtime := int64(2000)

			starter := func(ctx context.Context) localLyrics {
				r := assoc.StartMemoryForTest(ctx, assoc.MemTestConfig{
					Snapshot: func() assoc.MemTestSnapshot {
						mu.Lock()
						defer mu.Unlock()
						return curSnap
					},
					PlayingRow: func(hash string) (assoc.MemTestPlayingRow, error) {
						mu.Lock()
						defer mu.Unlock()
						return rows[hash], nil
					},
					ReadFile: func(path string) ([]byte, error) {
						switch path {
						case pathA:
							return []byte("[00:01.00]歌A一\n"), nil
						case pathB:
							return []byte("[00:01.00]本地B一\n"), nil
						}
						return nil, fs.ErrNotExist
					},
					StatDB: func() (int64, time.Time, error) {
						mu.Lock()
						sec := dbMtime
						mu.Unlock()
						return 1 << 20, time.Unix(sec, 0), nil
					},
				})
				if r == nil {
					return nil
				}
				return r
			}

			p := New(0, 1000)
			call := 0
			src := &hookPlayInfoSource{fn: func(c int) (*cdp.PlayInfo, error) {
				call++
				switch {
				case call == 1 || call == 2 || call == 3:
					// 歌 A 建立 bound。
					return read(hashA, "playing", float64(call-1), 200), nil
				case call == 4:
					// 切到有词本地文件 B（PlayedAt = T1）。
					mu.Lock()
					rows[hashB] = assoc.MemTestPlayingRow{PlayedAt: fileTime(time.Now()), Path: audioB}
					dbMtime = 2001
					curSnap = assoc.MemTestSnapshot{
						SongItemPtr:     0x200,
						CurrentSongHash: hashB,
						CLyricPtr:       0x20000,
						TaskID:          2,
						LyricFilePath:   pathB,
					}
					mu.Unlock()
					return read(hashB, "playing", 0, 200), nil
				case call == 5 || call == 6 || call == 7:
					return read(hashB, "playing", float64(call-4), 200), nil
				case call == 8:
					// 保持 hash == hashB 单曲循环重播：第二轮无词且库刷入 T2 > T1。
					mu.Lock()
					rows[hashB] = assoc.MemTestPlayingRow{PlayedAt: fileTime(time.Now()), Path: audioB}
					dbMtime = 2002
					curSnap = assoc.MemTestSnapshot{
						SongItemPtr:     0x200,
						CurrentSongHash: hashB,
						NoLyricFound:    true,
					}
					mu.Unlock()
					return read(hashB, "playing", 0, 200), nil
				case call >= 9 && call <= 14:
					// 持续推进 6 拍，确认经 1s stat 检出 T2 后恰好清词一次且后续幂等不重发。
					return read(hashB, "playing", float64(call-8), 200), nil
				default:
					return nil, errors.New("done")
				}
			}}

			p.runSession(src, starter)
			synctest.Wait()
			evts := drainAll(p)

			var bClears int
			var bShown bool
			for _, e := range evts {
				if al, ok := e.Data.(*player.AllLyricsData); ok && strings.Contains(al.Title, hashB) {
					if al.Count > 0 {
						bShown = true
					} else {
						bClears++
					}
				}
			}
			if !bShown {
				t.Fatalf("第一轮应显示本地 B 的歌词，事件=%q", lyricEvents(evts))
			}
			if bClears != 1 {
				t.Fatalf("单曲循环无词应恰好清词 1 次，实得 %d 次（事件=%q）", bClears, lyricEvents(evts))
			}
		})
	})
}

func TestSessionSteadyPlaybackResponsiveDuringBlockingCDPAndPollWait(t *testing.T) {
	t.Run("稳定播放期GetPlayInfo阻塞1s时取消关联在300ms内生效且零并发GetPlayInfo", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(context.Context, string, int, string, string) ([]klyric.Line, error) {
			return timedLyrics("在线", 1.0), nil
		})

		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			p := New(0, 100)
			f := newFakeLocal(map[string][]reply{
				"A": {{50 * time.Millisecond, found("A", timedLyrics("本地", 1.0))}},
			})

			blockingStarted := make(chan struct{})
			var sendAtNS, clearedAtNS atomic.Int64
			var gotInitial, gotCountZero, gotMinusOne atomic.Bool

			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				switch call {
				case 1, 2:
					return read("A", "playing", 5, 200), nil
				case 3:
					close(blockingStarted)
					time.Sleep(time.Second)
					return read("A", "playing", 6, 200), nil
				case 4:
					return read("A", "playing", 7, 200), nil
				default:
					return nil, errors.New("done")
				}
			}}

			evtDone := make(chan struct{})
			go func() {
				defer close(evtDone)
				for {
					select {
					case e := <-p.EventCh:
						switch d := e.Data.(type) {
						case *player.AllLyricsData:
							if d.Count == 2 {
								gotInitial.Store(true)
							} else if d.Count == 0 && !gotCountZero.Swap(true) {
								clearedAtNS.Store(int64(time.Since(start)))
							}
						case *player.LyricUpdate:
							if d.Index == -1 {
								gotMinusOne.Store(true)
							}
						}
					case <-p.StopCh:
						return
					}
				}
			}()

			sendDone := make(chan struct{})
			go func() {
				defer close(sendDone)
				<-blockingStarted
				time.Sleep(50 * time.Millisecond)
				sendAtNS.Store(int64(time.Since(start)))
				f.send(assoc.Result{Hash: "A", Kind: assoc.KindUnassociated})
			}()

			p.runSession(src, f.starter())
			<-sendDone
			p.Stop()
			<-evtDone
			synctest.Wait()

			if !gotInitial.Load() {
				t.Fatal("稳定播放前应先发出初始歌词")
			}
			if !gotCountZero.Load() || !gotMinusOne.Load() {
				t.Fatalf("应发出 count:0 与 index:-1，实际 countZero=%v minusOne=%v", gotCountZero.Load(), gotMinusOne.Load())
			}
			if delay := time.Duration(clearedAtNS.Load() - sendAtNS.Load()); delay < 0 || delay > 300*time.Millisecond {
				t.Fatalf("稳定播放期阻塞 GetPlayInfo 时取消关联清词延迟 = %v，want <= 300ms", delay)
			}
			if mc := src.maxConcurrency(); mc != 1 {
				t.Fatalf("GetPlayInfo 最大并发数 = %d，want 1", mc)
			}
		})
	})

	t.Run("稳定播放期GetPlayInfo阻塞1s时偏移更新在300ms内生效且零并发GetPlayInfo", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(context.Context, string, int, string, string) ([]klyric.Line, error) {
			return timedLyrics("在线", 5.0), nil
		})

		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			p := New(0, 100)
			f := newFakeLocal(map[string][]reply{
				"A": {{50 * time.Millisecond, found("A", timedLyrics("本地", 5.0))}},
			})

			blockingStarted := make(chan struct{})
			var sendAtNS, shiftedAtNS, shiftedLineAtNS atomic.Int64
			var gotShiftedAll, gotShiftedLine atomic.Bool

			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				switch call {
				case 1, 2:
					return read("A", "playing", 4, 200), nil
				case 3:
					close(blockingStarted)
					time.Sleep(time.Second)
					return read("A", "playing", 5, 200), nil
				case 4:
					return read("A", "playing", 6, 200), nil
				default:
					return nil, errors.New("done")
				}
			}}

			evtDone := make(chan struct{})
			go func() {
				defer close(evtDone)
				for {
					select {
					case e := <-p.EventCh:
						switch d := e.Data.(type) {
						case *player.AllLyricsData:
							if len(d.Lyrics) > 0 && approxEq(d.Lyrics[0].Timestamp, 3.5) && !gotShiftedAll.Swap(true) {
								shiftedAtNS.Store(int64(time.Since(start)))
							}
						case *player.LyricUpdate:
							if d.Index == 0 && approxEq(d.Timestamp, 3.5) && !gotShiftedLine.Swap(true) {
								shiftedLineAtNS.Store(int64(time.Since(start)))
							}
						}
					case <-p.StopCh:
						return
					}
				}
			}()

			sendDone := make(chan struct{})
			go func() {
				defer close(sendDone)
				<-blockingStarted
				time.Sleep(50 * time.Millisecond)
				sendAtNS.Store(int64(time.Since(start)))
				f.send(assoc.Result{Hash: "A", Kind: assoc.KindNone, HasMemoryLyric: true, LyricOffsetMs: 1500})
			}()

			p.runSession(src, f.starter())
			<-sendDone
			p.Stop()
			<-evtDone
			synctest.Wait()

			if !gotShiftedAll.Load() {
				t.Fatal("稳定播放期阻塞 GetPlayInfo 时应发出新时间轴 all_lyrics（首行 3.5s）")
			}
			if delay := time.Duration(shiftedAtNS.Load() - sendAtNS.Load()); delay < 0 || delay > 300*time.Millisecond {
				t.Fatalf("稳定播放期阻塞 GetPlayInfo 时偏移更新延迟 = %v，want <= 300ms", delay)
			}
			if !gotShiftedLine.Load() {
				t.Fatal("稳定播放期偏移更新后应紧跟补发当前行 lyric_update#0（3.5s）")
			}
			if lineDelay := time.Duration(shiftedLineAtNS.Load() - sendAtNS.Load()); lineDelay < 0 || lineDelay > 300*time.Millisecond {
				t.Fatalf("偏移更新后当前行 lyric_update 补发延迟 = %v，want <= 300ms", lineDelay)
			}
			if mc := src.maxConcurrency(); mc != 1 {
				t.Fatalf("GetPlayInfo 最大并发数 = %d，want 1", mc)
			}
		})
	})

	t.Run("稳定播放期pollInterval=1000ms内偏移变更与取消关联均在300ms内即时生效", func(t *testing.T) {
		stubEmptyCoverAPI(t)
		stubFetchCtx(t, func(context.Context, string, int, string, string) ([]klyric.Line, error) {
			return nil, nil
		})

		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			p := New(0, 1000)
			f := newFakeLocal(map[string][]reply{
				"A": {{50 * time.Millisecond, found("A", timedLyrics("本地", 5.0))}},
			})

			steadyBeat := make(chan struct{})
			var offsetSendAtNS, offsetGotAtNS atomic.Int64
			var unassocSendAtNS, unassocGotAtNS atomic.Int64
			var gotShiftedAll, gotShiftedLine, gotCountZero, gotMinusOne atomic.Bool

			src := &hookPlayInfoSource{fn: func(call int) (*cdp.PlayInfo, error) {
				switch call {
				case 1, 2:
					return read("A", "playing", 4, 200), nil
				case 3:
					close(steadyBeat)
					return read("A", "playing", 5, 200), nil
				case 4:
					return read("A", "playing", 6, 200), nil
				default:
					return nil, errors.New("done")
				}
			}}

			evtDone := make(chan struct{})
			go func() {
				defer close(evtDone)
				for {
					select {
					case e := <-p.EventCh:
						switch d := e.Data.(type) {
						case *player.AllLyricsData:
							if len(d.Lyrics) > 0 && approxEq(d.Lyrics[0].Timestamp, 3.5) && !gotShiftedAll.Swap(true) {
								offsetGotAtNS.Store(int64(time.Since(start)))
							} else if d.Count == 0 && !gotCountZero.Swap(true) {
								unassocGotAtNS.Store(int64(time.Since(start)))
							}
						case *player.LyricUpdate:
							if d.Index == 0 && approxEq(d.Timestamp, 3.5) {
								gotShiftedLine.Store(true)
							} else if d.Index == -1 {
								gotMinusOne.Store(true)
							}
						}
					case <-p.StopCh:
						return
					}
				}
			}()

			sendDone := make(chan struct{})
			go func() {
				defer close(sendDone)
				<-steadyBeat
				time.Sleep(100 * time.Millisecond)
				offsetSendAtNS.Store(int64(time.Since(start)))
				f.send(assoc.Result{Hash: "A", Kind: assoc.KindNone, HasMemoryLyric: true, LyricOffsetMs: 1500})
				time.Sleep(100 * time.Millisecond)
				unassocSendAtNS.Store(int64(time.Since(start)))
				f.send(assoc.Result{Hash: "A", Kind: assoc.KindUnassociated})
			}()

			p.runSession(src, f.starter())
			<-sendDone
			p.Stop()
			<-evtDone
			synctest.Wait()

			if !gotShiftedAll.Load() || !gotShiftedLine.Load() {
				t.Fatalf("kugou-poll=1000 期间送入的偏移变更应即时生效：all=%v line=%v", gotShiftedAll.Load(), gotShiftedLine.Load())
			}
			if delay := time.Duration(offsetGotAtNS.Load() - offsetSendAtNS.Load()); delay < 0 || delay > 300*time.Millisecond {
				t.Fatalf("pollInterval=1000ms 内偏移变更延迟 = %v，want <= 300ms", delay)
			}
			if !gotCountZero.Load() || !gotMinusOne.Load() {
				t.Fatalf("kugou-poll=1000 期间送入的取消关联应发出 count:0 与 index:-1：countZero=%v minusOne=%v", gotCountZero.Load(), gotMinusOne.Load())
			}
			if delay := time.Duration(unassocGotAtNS.Load() - unassocSendAtNS.Load()); delay < 0 || delay > 300*time.Millisecond {
				t.Fatalf("pollInterval=1000ms 内取消关联延迟 = %v，want <= 300ms", delay)
			}
		})
	})
}
