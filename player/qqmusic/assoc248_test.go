package qqmusic

// 本文件只测 #248（消除被匹配为在线歌曲的本地文件开播闪无歌词，doc/design/qqmusic-assoc-followup-design.md §2）：
//   1. 同首补齐在线编号保持 gen 不变：模式乙（无编号已认出本地 49 行）、模式甲（49 行在通道待取）、普通在线歌（srcNone 归一为 srcOnline）
//      及暂停中补取窗口用尽后恢复补到编号清零兜底状态；
//   2. 认领当拍有编号且关联表可能存在本地条目时，在线 0 行暂入未决态（不抢发 index:-1），待本地结果、srcOnline、resUnknown、
//      resStopped 或满 localDecideWindow（1s）的 expireWait 解除，且 srcOnline→srcOnline 解除时不打多余来源切换日志；
//   3. assocReader.LocalCheckPossible() 在启动初值、有/无本地条目、表缺失/失败、RM 致命错误、停用/panic/取消与 nil 接收者下的真值；
//   4. 同 gen 保留与 srcNone→srcOnline 归一化期间 Furigana 深拷贝与摘要不变量。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
)

// makeLRC248 生成 n 行有效 LRC 文本，供读取者经真实解析路径产出指定行数。
func makeLRC248(n int, prefix string) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "[%02d:%02d.00]%s-%02d\n", i/60, i%60, prefix, i+1)
	}
	return b.String()
}

// makeLines248 生成 n 行带逐字时间轴的 lyricLine，供在线原始行与 Furigana 回归测试。
func makeLines248(n int, prefix string) []lyricLine {
	out := make([]lyricLine, n)
	for i := range n {
		ts := float32(i + 1)
		out[i] = lyricLine{
			Index: i,
			Time:  ts,
			Text:  fmt.Sprintf("%s-%02d", prefix, i+1),
			Detailed: player.LyricTextDetailed{
				Timestamp: ts,
				Duration:  0.8,
				Words:     []player.LyricTextDetailedWord{{Timestamp: ts, Duration: 0.8, Text: prefix}},
			},
		}
	}
	return out
}

// TestAssoc248BackfillModeB 回归 #248 模式乙（对应 2026-11-24 验收第 2、3、6 次）：
// 无编号认领 → 读取者认出本地 49 行并发出 → 同 gen 补齐 songMid、在线返回 0 行 →
// refresh 返回 changed=false 保持 srcLocal，takeResult 也不重发，IdentifyLocal 不跑第 2 次，不扫在线记录。
// 变异自证（隔离副本实测）：assocreader.go 的 onSong 同 gen 分支清空 w.localPath（或再调 w.restartLocal），本例因第 1.5s 重跑
// 本地认定（[0s 1.5s] != [0s]）而红。
func TestAssoc248BackfillModeB(t *testing.T) {
	e := widEnv(widItemA, widItemF)
	e.fs.put(widLyrF, []byte(makeLRC248(49, "本地海底")))
	e.rm.fn = openBy([]string{widF}, testQQPID)

	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		if !r.LocalCheckPossible() {
			t.Fatal("关联表含本地条目时 LocalCheckPossible 应为 true")
		}
		var st lyricState
		t0 := time.Now()
		k := songKey{gen: 1, name: widMatch}
		st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
		r.Submit(k)
		st.refresh(k.hasIDs(), stateOffsetSec)

		// 无编号阶段：读取者第 0 秒认出本地 49 行，会话接纳。
		synctest.Wait()
		got, changed := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec)
		if !changed || len(got) != 49 || st.source() != srcLocal || st.localPath() != widF || st.undecided() {
			t.Fatalf("无编号阶段接纳本地 49 行失败：changed=%v len=%d src=%v local=%q undecided=%v",
				changed, len(got), st.source(), st.localPath(), st.undecided())
		}
		wantTimes(t, "认领后本地认定", e.rm.callTimes(), secs(0))

		// 1.5s 后补取拿到 songMid：保持 gen=1 不变、不 reset，在线 API 返回 0 行。
		sleepUntil(e, 1500*time.Millisecond)
		k.songMid = widMid
		k.detail = songDetail{}
		r.Submit(k)
		st.setOnlineRaw(nil)

		lines, changed := st.refresh(k.hasIDs(), stateOffsetSec)
		if changed || lines != nil || st.source() != srcLocal || st.localPath() != widF || st.undecided() {
			t.Fatalf("补取同 gen 刷新不应清空本地 49 行：changed=%v len=%d src=%v local=%q undecided=%v",
				changed, len(lines), st.source(), st.localPath(), st.undecided())
		}

		// 读取者处理完同 gen 补齐编号后发出的仍是同一份 srcLocal，指纹不变且不重跑 RM、不扫在线记录。
		synctest.Wait()
		lines, changed = takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec)
		if changed || lines != nil || st.source() != srcLocal || st.localPath() != widF {
			t.Fatalf("补取后 takeResult 不应重发或改变本地来源：changed=%v len=%d src=%v local=%q",
				changed, len(lines), st.source(), st.localPath())
		}
		sleepUntil(e, 30*time.Second)
		noAssocResult(t, r)
		wantTimes(t, "全程本地认定", e.rm.callTimes(), secs(0))
		wantTimes(t, "全程读记录", e.mem.scanTimes(), nil)
	})
}

// TestAssoc248BackfillModeA 回归 #248 模式甲（对应 2026-11-24 验收第 1、4、5 次）：
// 无编号认领 → 读取者将同 gen 的 srcLocal 49 行写入 results 通道尚未被取走 →
// 补取同 gen 补齐 songMid、在线返回 0 行 → takeResult 接纳 49 行，0 次 index:-1，不重跑第 2 次 IdentifyLocal。
func TestAssoc248BackfillModeA(t *testing.T) {
	t.Run("真读取者通道待取时同 gen 补齐编号", func(t *testing.T) {
		e := widEnv(widItemA, widItemF)
		e.fs.put(widLyrF, []byte(makeLRC248(49, "本地海底")))
		e.rm.fn = openBy([]string{widF}, testQQPID)

		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			var st lyricState
			t0 := time.Now()
			k := songKey{gen: 1, name: widMatch}
			st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
			r.Submit(k)
			st.refresh(k.hasIDs(), stateOffsetSec)

			// 模拟轮询线程阻塞在 FindSongMid 期间：读取者已完成第 0 秒本地认定并写入 results 通道。
			synctest.Wait()
			if len(r.Results()) != 1 {
				t.Fatalf("通道中应已有待取的本地结果，实得 %d 条", len(r.Results()))
			}

			// 补取拿到 songMid：同 gen 原地更新并 Submit，在线返回 0 行。
			k.songMid = widMid
			k.detail = songDetail{}
			r.Submit(k)
			st.setOnlineRaw(nil)
			_, _ = st.refresh(k.hasIDs(), stateOffsetSec)
			synctest.Wait()
			got, changed := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec)
			if !changed || len(got) != 49 || st.source() != srcLocal || st.localPath() != widF || st.undecided() {
				t.Fatalf("模式甲补取应接纳同 gen 49 行本地歌词：changed=%v len=%d src=%v local=%q undecided=%v",
					changed, len(got), st.source(), st.localPath(), st.undecided())
			}
			if lyricsUndecided(got, false, st.source(), st.undecided()) {
				t.Fatal("接纳本地 49 行后 lyricsUndecided 应为 false")
			}
			sleepUntil(e, 30*time.Second)
			noAssocResult(t, r)
			wantTimes(t, "全程本地认定", e.rm.callTimes(), secs(0))
			wantTimes(t, "全程读记录", e.mem.scanTimes(), nil)
		})
	})

	t.Run("读取者尚未处理补齐提交时直接接纳通道中原同 gen 本地结果", func(t *testing.T) {
		var st lyricState
		t0 := time.Unix(1_700_000_000, 0)
		st.reset(1, true, t0.Add(localDecideWindow))
		st.refresh(false, stateOffsetSec)

		ch := make(chan assocResult, 1)
		local49 := desiredRes(1, srcLocal, -1000, makeLines248(49, "本地海底"), "local49")
		local49.localPath = widF
		ch <- local49

		// 补取同 gen 补上编号、在线 0 行；刷新暂入 undecided，紧接 takeResult 立即接纳通道里的 49 行。
		st.setOnlineRaw(nil)
		_, _ = st.refresh(true, stateOffsetSec)
		if !st.undecided() {
			t.Fatal("取走本地结果前在线 0 行且 waitLocal=true 应处于 undecided")
		}
		got, changed := takeResult(&st, ch, true, stateOffsetSec)
		if !changed || len(got) != 49 || st.source() != srcLocal || st.localPath() != widF || st.undecided() {
			t.Fatalf("应直接接纳通道中的同 gen 本地 49 行：changed=%v len=%d src=%v local=%q undecided=%v",
				changed, len(got), st.source(), st.localPath(), st.undecided())
		}
	})
}

// TestAssoc248BackfillPlainOnline 回归普通在线歌同 gen 补齐编号：
//  1. 无编号认领收到 srcNone → 同 gen 补齐 songMid 并 setOnlineRaw 66 行 → refresh(true, ...) 将 srcNone 归一为 srcOnline，
//     当拍返回 66 行；
//  2. 无编号认领在暂停中补取窗口用尽（detailFailed）→ 恢复后补齐编号清零 detail（detailNone）→ 读取者清零 fill 且不抢跑兜底，
//     后续封面失败再报 detailFailed 时正常兜底并启动记录扫描认出在线关联。
//
// 变异自证（隔离副本实测）：删掉 lyricstate.go 中 songHasIDs && p.src == srcNone 归一，子用例 1 因 changed=false、src=无而红。
func TestAssoc248BackfillPlainOnline(t *testing.T) {
	t.Run("无编号收 srcNone 后补齐编号并 setOnlineRaw 66 行", func(t *testing.T) {
		e := widEnv(widItemF) // 表里有本地条目（使 LocalCheckPossible=true），但未被 QQ 打开
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			var st lyricState
			t0 := time.Now()
			k := songKey{gen: 1, name: "普通在线歌"}
			st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
			r.Submit(k)
			st.refresh(k.hasIDs(), stateOffsetSec)

			// 读取者第 0 秒本地未认出，返回 srcNone；takeResult 接纳后 s.want.source == srcNone 且 waitLocal 解除。
			synctest.Wait()
			if lines, changed := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec); changed || lines != nil {
				t.Fatalf("无编号阶段 srcNone 与已登记指纹相同，不应重发：changed=%v len=%d", changed, len(lines))
			}
			if st.want.kind != resDesired || st.want.source != srcNone {
				t.Fatalf("s.want 应已记录 srcNone，实得 kind=%d src=%v", st.want.kind, st.want.source)
			}

			// 1.5s 后补取拿到 songMid 并取到 66 行在线歌词：同 gen 原地更新，refresh 将 srcNone 归一为 srcOnline。
			sleepUntil(e, 1500*time.Millisecond)
			k.songMid = widMid
			k.detail = songDetail{}
			r.Submit(k)
			st.setOnlineRaw(makeLines248(66, "在线歌词"))

			got, changed := st.refresh(k.hasIDs(), stateOffsetSec)
			if !changed || len(got) != 66 || st.source() != srcOnline || st.undecided() {
				t.Fatalf("补取普通在线歌应归一为 srcOnline 并发出 66 行：changed=%v len=%d src=%v undecided=%v",
					changed, len(got), st.source(), st.undecided())
			}

			// 读取者返回同 gen 的 srcOnline，指纹已匹配不再重发。
			synctest.Wait()
			if lines, changed := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec); changed || lines != nil {
				t.Fatalf("读取者随后返回的 srcOnline 不应重复发送：changed=%v len=%d", changed, len(lines))
			}
		})
	})

	t.Run("暂停窗口用尽记 detailFailed 后恢复同 gen 补齐编号清零 fill 并认出在线关联", func(t *testing.T) {
		e := widEnv(widItemA, widItemF) // 表里有 widMid 关联与未打开的本地条目
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			k := songKey{gen: 1, name: widMatch}
			r.Submit(k)
			wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")

			// 第 8.1s 暂停中补取窗口用尽，记 detailFailed。
			sleepUntil(e, 8100*time.Millisecond)
			k.detail = failedDetail
			r.Submit(k)
			wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
			wantTimes(t, "无编号阶段不兜底", e.byID.callTimes(), nil)

			// 第 20.1s 恢复后补到 songID，同 gen 原地更新并清零 detail。
			sleepUntil(e, 20100*time.Millisecond)
			k.songID = widID
			k.detail = songDetail{}
			r.Submit(k)
			wantDesired(t, latestResult(t, r), 1, srcOnline, 0, false, "")
			sleepUntil(e, 20500*time.Millisecond)
			wantTimes(t, "封面请求前不抢跑兜底", e.byID.callTimes(), nil)

			// 第 20.5s 封面请求失败再报 detailFailed：因 fill 已清零，立即按此时刻发起兜底并认出在线关联 A。
			k.detail = failedDetail
			r.Submit(k)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 300, false, "", assocLines...)
			wantTimes(t, "兜底按新时刻立即发起一次", e.byID.callTimes(), ms(20500))
			wantTimes(t, "记录扫描自补齐后启动", e.mem.scanTimes(), ms(20500))
		})
	})
}

// TestAssoc248ClaimWithIDsWaitLocal 覆盖认领当拍有编号时的 waitLocal / undecided / expireWait 全状态机流转。
// 变异自证（隔离副本实测）：删掉 lyricstate.go 中 resUnknown 解除 waitLocal，「解除未决态：读取者回 resUnknown」因
// changed=false、undecided=true 而红；删掉 refresh 中 #244 same 判断的 len(lines) > 0，四项解除未决子用例均因 changed=false 而红。
func TestAssoc248ClaimWithIDsWaitLocal(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	dl := t0.Add(localDecideWindow)

	t.Run("在线 0 行暂入未决，读取者回 srcLocal 49 行解除未决并发出", func(t *testing.T) {
		var st lyricState
		st.reset(1, true, dl)
		st.setOnlineRaw(nil)
		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 0 || st.source() != srcOnline || !st.undecided() {
			t.Fatalf("认领当拍在线 0 行应进入未决态：changed=%v len=%d src=%v undecided=%v",
				changed, len(lines), st.source(), st.undecided())
		}
		if !lyricsUndecided(lines, false, st.source(), st.undecided()) {
			t.Fatal("未决态下 lyricsUndecided 应为 true")
		}

		// 旧代次的 resUnknown 不得解除当前代次未决态。
		if l, ch := st.apply(assocResult{gen: 0, kind: resUnknown}, true, stateOffsetSec); ch || l != nil || !st.undecided() {
			t.Fatalf("旧代次 resUnknown 不应解除未决态：changed=%v undecided=%v", ch, st.undecided())
		}

		local49 := desiredRes(1, srcLocal, -1000, makeLines248(49, "本地"), "local49")
		local49.localPath = widF
		ch := make(chan assocResult, 1)
		ch <- local49
		got, changed := takeResult(&st, ch, true, stateOffsetSec)
		if !changed || len(got) != 49 || st.source() != srcLocal || st.localPath() != widF || st.undecided() {
			t.Fatalf("收到本地 49 行应解除未决并发出：changed=%v len=%d src=%v local=%q undecided=%v",
				changed, len(got), st.source(), st.localPath(), st.undecided())
		}
		if lyricsUndecided(got, false, st.source(), st.undecided()) {
			t.Fatal("发出本地 49 行后 lyricsUndecided 应为 false")
		}
	})

	// 四种解除未决态落定无歌词的方式：srcOnline、resUnknown、resStopped、expireWait。
	resolveCases := []struct {
		name    string
		resolve func(t *testing.T, st *lyricState) ([]lyricLine, bool)
	}{
		{
			name: "读取者回 srcOnline",
			resolve: func(t *testing.T, st *lyricState) ([]lyricLine, bool) {
				ch := make(chan assocResult, 1)
				ch <- onlineRes(1, 0)
				return takeResult(st, ch, true, stateOffsetSec)
			},
		},
		{
			name: "读取者回 resUnknown",
			resolve: func(t *testing.T, st *lyricState) ([]lyricLine, bool) {
				ch := make(chan assocResult, 1)
				ch <- assocResult{gen: 1, kind: resUnknown}
				return takeResult(st, ch, true, stateOffsetSec)
			},
		},
		{
			name: "读取者回 resStopped",
			resolve: func(t *testing.T, st *lyricState) ([]lyricLine, bool) {
				ch := make(chan assocResult, 1)
				ch <- stoppedRes(1)
				return takeResult(st, ch, true, stateOffsetSec)
			},
		},
		{
			name: "满 localDecideWindow 调用 expireWait",
			resolve: func(t *testing.T, st *lyricState) ([]lyricLine, bool) {
				if l, ch := st.expireWait(dl.Add(-time.Millisecond), true, stateOffsetSec); ch || l != nil || !st.undecided() {
					t.Fatalf("到期前 1ms 不应提前解除未决态：changed=%v undecided=%v", ch, st.undecided())
				}
				return st.expireWait(dl, true, stateOffsetSec)
			},
		},
	}
	for _, c := range resolveCases {
		t.Run("解除未决态："+c.name, func(t *testing.T) {
			logs := captureAssocLog(t)
			var st lyricState
			st.reset(1, true, dl)
			st.setOnlineRaw(nil)
			lines, changed := st.refresh(true, stateOffsetSec)
			if !changed || len(lines) != 0 || st.source() != srcOnline || !st.undecided() {
				t.Fatalf("初始未决态不符：changed=%v len=%d src=%v undecided=%v",
					changed, len(lines), st.source(), st.undecided())
			}

			got, changed := c.resolve(t, &st)
			if !changed || len(got) != 0 || st.undecided() || st.source() != srcOnline {
				t.Fatalf("%s 应解除未决态、changed=true、空行且保持 srcOnline：changed=%v len=%d undecided=%v src=%v",
					c.name, changed, len(got), st.undecided(), st.source())
			}
			if lyricsUndecided(got, false, st.source(), st.undecided()) {
				t.Fatalf("%s 解除后 lyricsUndecided 应为 false", c.name)
			}
			if n := countLog(logs(), "*", "歌词来源改为"); n != 0 {
				t.Fatalf("%s 解除未决态（srcOnline→srcOnline）不应打来源切换日志，实得 %d 条：\n%s", c.name, n, logs())
			}
			// 再次调用 expireWait 不再触发重发。
			if l, ch := st.expireWait(dl.Add(time.Second), true, stateOffsetSec); ch || l != nil {
				t.Fatalf("已解除未决后再次 expireWait 不应重发：changed=%v len=%d", ch, len(l))
			}
		})
	}

	t.Run("waitLocal=false 时当拍即已决", func(t *testing.T) {
		var st lyricState
		st.reset(1, false, dl)
		st.setOnlineRaw(nil)
		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 0 || st.source() != srcOnline || st.undecided() {
			t.Fatalf("waitLocal=false 时应直接落定：changed=%v len=%d src=%v undecided=%v",
				changed, len(lines), st.source(), st.undecided())
		}
		if lyricsUndecided(lines, false, st.source(), st.undecided()) {
			t.Fatal("waitLocal=false 且 pending=false 时 lyricsUndecided 应为 false")
		}
	})

	t.Run("stopped 后 reset 传 waitLocal=true 也不等待", func(t *testing.T) {
		var st lyricState
		st.reset(1, true, dl)
		st.apply(stoppedRes(1), true, stateOffsetSec)
		st.reset(2, true, dl.Add(time.Second))
		st.setOnlineRaw(nil)
		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 0 || st.source() != srcOnline || st.undecided() {
			t.Fatalf("已停用后换歌不应再等待本地认定：changed=%v len=%d src=%v undecided=%v",
				changed, len(lines), st.source(), st.undecided())
		}
	})

	t.Run("waitLocal=true 但在线非空时不入未决态且后续解除不重发", func(t *testing.T) {
		var st lyricState
		st.reset(1, true, dl)
		st.setOnlineRaw(makeLines248(66, "在线"))
		lines, changed := st.refresh(true, stateOffsetSec)
		if !changed || len(lines) != 66 || st.source() != srcOnline || st.undecided() {
			t.Fatalf("在线 66 行不应进入未决态：changed=%v len=%d src=%v undecided=%v",
				changed, len(lines), st.source(), st.undecided())
		}
		if l, ch := st.apply(assocResult{gen: 1, kind: resUnknown}, true, stateOffsetSec); ch || l != nil {
			t.Fatalf("非未决态下 resUnknown 解除 waitLocal 不应重发：changed=%v len=%d", ch, len(l))
		}
		if l, ch := st.expireWait(dl, true, stateOffsetSec); ch || l != nil {
			t.Fatalf("非未决态下 expireWait 不应重发：changed=%v len=%d", ch, len(l))
		}
	})
}

// TestAssoc248LocalCheckPossible 覆盖 assocReader.LocalCheckPossible() 全部真值转移路径。
func TestAssoc248LocalCheckPossible(t *testing.T) {
	t.Run("nil 接收者返回 false", func(t *testing.T) {
		var r *assocReader
		if r.LocalCheckPossible() {
			t.Fatal("nil assocReader.LocalCheckPossible() 应为 false")
		}
	})

	t.Run("启动初值 true、首读阻塞中为 true、解析无本地条目后转 false", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := widEnv(widItemA) // 仅在线条目，无本地条目
			var g gate
			g.arm()
			e.fs.onRead = func(p string) {
				if strings.EqualFold(p, testTablePath) {
					g.wait()
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.setT0(time.Now())
			r, ok := startAssocReader(ctx, e.deps())
			if !ok {
				t.Fatal("startAssocReader 失败")
			}
			synctest.Wait()
			if !r.LocalCheckPossible() {
				t.Fatal("首次表解析完成前 LocalCheckPossible 应保持初值 true")
			}
			g.release()
			synctest.Wait()
			if r.LocalCheckPossible() {
				t.Fatal("解析到仅含在线条目的关联表后 LocalCheckPossible 应为 false")
			}
			cancel()
			synctest.Wait()
		})
	})

	t.Run("启动时关联表不存在立即转 false", func(t *testing.T) {
		e := newReaderEnv() // 未放置 testTablePath
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			if r.LocalCheckPossible() {
				t.Fatal("启动时关联表不存在，LocalCheckPossible 应为 false")
			}
		})
	})

	t.Run("含本地条目 true → 改表无本地 false → 恢复含本地 true → 解析失败 false → 恢复 true → 表消失 false", func(t *testing.T) {
		e := widEnv(widItemA, widItemF)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			if !r.LocalCheckPossible() {
				t.Fatal("含 KindLocal 条目时应为 true")
			}

			// 改表仅留在线条目，稳定 1s 后重读。
			e.fs.put(testTablePath, tableDat(widItemA))
			sleepUntil(e, 2500*time.Millisecond)
			if r.LocalCheckPossible() {
				t.Fatal("改表无本地条目后应为 false")
			}

			// 改回含本地条目。
			e.fs.put(testTablePath, tableDat(widItemA, widItemF))
			sleepUntil(e, 5*time.Second)
			if !r.LocalCheckPossible() {
				t.Fatal("改回含本地条目后应为 true")
			}

			// 写入坏表触发暂时性 parseFailed（一次结构错误）。
			e.fs.put(testTablePath, []byte("corrupt-dat-once"))
			sleepUntil(e, 7500*time.Millisecond)
			if r.LocalCheckPossible() {
				t.Fatal("表解析失败后应为 false")
			}

			// 恢复有效含本地条目表。
			e.fs.put(testTablePath, tableDat(widItemA, widItemF))
			sleepUntil(e, 10*time.Second)
			if !r.LocalCheckPossible() {
				t.Fatal("恢复有效含本地条目表后应为 true")
			}

			// 表消失（tick 中 !st.ok）。
			e.fs.remove(testTablePath)
			sleepUntil(e, 12*time.Second)
			if r.LocalCheckPossible() {
				t.Fatal("表文件消失后应为 false")
			}
		})
	})

	t.Run("RM 致命错误后转 false", func(t *testing.T) {
		e := widEnv(widItemA, widItemF)
		e.rm.fn = func([]string) ([]uint32, error) {
			return nil, i18n.Errorf("RmStartSession failed: %w", windows.ERROR_SEM_TIMEOUT)
		}
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			if !r.LocalCheckPossible() {
				t.Fatal("RM 尚未报错前应为 true")
			}
			r.Submit(songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch})
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 300, false, "", assocLines...)
			if r.LocalCheckPossible() {
				t.Fatal("RM 报致命错误（localOff=true）后 LocalCheckPossible 应为 false")
			}
		})
	})

	t.Run("stop 与 recoverStop 及 ctx 取消退出后均为 false", func(t *testing.T) {
		t.Run("两次同一结构错误触发 stop", func(t *testing.T) {
			stubAssocReports(t)
			e := widEnv(widItemF)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				if !r.LocalCheckPossible() {
					t.Fatal("初始含本地条目应为 true")
				}
				e.fs.put(testTablePath, []byte("bad-same-structure"))
				sleepUntil(e, 5*time.Second)
				if r.LocalCheckPossible() {
					t.Fatal("stop 停用后 LocalCheckPossible 应为 false")
				}
			})
		})

		t.Run("读表 panic 触发 recoverStop", func(t *testing.T) {
			stubAssocReports(t)
			e := widEnv(widItemF)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				if !r.LocalCheckPossible() {
					t.Fatal("初始含本地条目应为 true")
				}
				e.fs.setHooks(func(p string) {
					if strings.EqualFold(p, testTablePath) {
						panic("injected panic for LocalCheckPossible")
					}
				}, nil)
				e.fs.put(testTablePath, tableDat(widItemF))
				sleepUntil(e, 3*time.Second)
				if r.LocalCheckPossible() {
					t.Fatal("recoverStop 退出后 LocalCheckPossible 应为 false")
				}
			})
		})

		t.Run("ctx 取消退出", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := widEnv(widItemF)
				ctx, cancel := context.WithCancel(context.Background())
				e.setT0(time.Now())
				r, ok := startAssocReader(ctx, e.deps())
				if !ok {
					t.Fatal("startAssocReader 失败")
				}
				synctest.Wait()
				if !r.LocalCheckPossible() {
					t.Fatal("取消前含本地条目应为 true")
				}
				cancel()
				synctest.Wait()
				if r.LocalCheckPossible() {
					t.Fatal("ctx 取消退出后 LocalCheckPossible 应为 false")
				}
			})
		})
	})
}

// TestAssoc248FuriganaInvariant 验证 #248 改动期间 Furigana 深拷贝与行摘要不变量完好。
func TestAssoc248FuriganaInvariant(t *testing.T) {
	wantKana := player.LyricFurigana{{Text: "海底", Ruby: "かいてい"}}
	t0 := time.Unix(1_700_000_000, 0)
	dl := t0.Add(localDecideWindow)

	t.Run("同 gen 保留本地带 Furigana 歌词与深拷贝隔离", func(t *testing.T) {
		var st lyricState
		st.reset(1, true, dl)
		st.refresh(false, stateOffsetSec)

		localLines := makeLines248(2, "海")
		localLines[0].Furigana = slices.Clone(wantKana)
		res := desiredRes(1, srcLocal, -1000, localLines, "local-kana")
		res.localPath = widF

		out, changed := st.apply(res, false, stateOffsetSec)
		if !changed || !reflect.DeepEqual(out[0].Furigana, wantKana) {
			t.Fatalf("本地歌词 Furigana 丢失：changed=%v got=%#v", changed, out[0].Furigana)
		}
		// 篡改输出切片与原结果切片，同 gen 补齐编号后 refresh 仍保持不变、不重发。
		out[0].Furigana[0].Ruby = "tampered-out"
		st.setOnlineRaw(nil)
		if l, ch := st.refresh(true, stateOffsetSec); ch || l != nil || st.source() != srcLocal {
			t.Fatalf("同 gen 补齐编号应保持本地带 Furigana 歌词不重发：changed=%v src=%v", ch, st.source())
		}
		// 换偏移强制重发，确认内部保存的 Furigana 未受污染。
		res2 := desiredRes(1, srcLocal, 0, localLines, "local-kana")
		res2.localPath = widF
		out2, changed := st.apply(res2, true, stateOffsetSec)
		if !changed || !reflect.DeepEqual(out2[0].Furigana, wantKana) {
			t.Fatalf("二次输出 Furigana 受污染：changed=%v got=%#v want=%#v", changed, out2[0].Furigana, wantKana)
		}
	})

	t.Run("srcNone 归一为 srcOnline 后完整保留并区分 Furigana 摘要", func(t *testing.T) {
		var st lyricState
		st.reset(1, true, dl)
		st.refresh(false, stateOffsetSec)
		st.apply(noneRes(1), false, stateOffsetSec)

		raw := makeLines248(2, "海")
		raw[0].Furigana = slices.Clone(wantKana)
		st.setOnlineRaw(raw)
		raw[0].Furigana[0].Ruby = "tampered-input"

		out, changed := st.refresh(true, stateOffsetSec)
		if !changed || st.source() != srcOnline || !reflect.DeepEqual(out[0].Furigana, wantKana) {
			t.Fatalf("srcNone→srcOnline 归一后 Furigana 不符：changed=%v src=%v got=%#v", changed, st.source(), out[0].Furigana)
		}
		out[0].Furigana[0].Ruby = "tampered-output"

		// 相同 Furigana 再次 setOnlineRaw 不重发。
		same := makeLines248(2, "海")
		same[0].Furigana = slices.Clone(wantKana)
		st.setOnlineRaw(same)
		if l, ch := st.refresh(true, stateOffsetSec); ch || l != nil {
			t.Fatalf("相同 Furigana 不应重发：changed=%v", ch)
		}

		// 仅修改 Ruby 文本必须触发重发。
		changedKana := player.LyricFurigana{{Text: "海底", Ruby: "うみぞこ"}}
		diff := makeLines248(2, "海")
		diff[0].Furigana = slices.Clone(changedKana)
		st.setOnlineRaw(diff)
		out2, changed := st.refresh(true, stateOffsetSec)
		if !changed || !reflect.DeepEqual(out2[0].Furigana, changedKana) {
			t.Fatalf("仅修改 Furigana Ruby 应触发重发：changed=%v got=%#v want=%#v", changed, out2[0].Furigana, changedKana)
		}
	})
}

// TestAssoc248BackfillCoverDeferredWhileUndecided 回归 Codex 审查 P2 反例：
// 关联表含本地条目、无编号认领（t=0）后，补取编号与在线 0 行（t=100ms）快于首轮本地认定；若当拍立即同步调用阻塞 5s 的
// fetchCoverURL，会阻塞轮询线程推迟 takeResult / expireWait 解除未决态（超出 1s 硬上界）。
// 本测试以真实 assocReader、lyricState、takeResult 与 backfillCoverGate 驱动 50ms 轮询拍，验证：
//  1. RM 在 t=230ms 返回非本地（srcOnline）时，t=250ms 准时解除未决并发出无歌词，t=300ms 才执行延后的 5s 封面请求；
//  2. RM 在 t=230ms 认出本地 49 行（srcLocal）时，t=250ms 准时发出 49 行且全程 0 次无歌词，t=300ms 才执行延后的 5s 封面请求；
//  3. 慢盘 RM 超过 1s 仍未返回时，t=1s expireWait 准时解除未决并发出无歌词，t=1.05s 才执行延后的 5s 封面请求；
//  4. 换歌丢弃旧 gen 的待补封面（reset 与 take 的 gen 守卫）、放行拍（released=true）不排空、非未决态当拍立即请求封面。
//
// 变异自证（隔离副本实测）：把 backfillCoverGate.schedule 的 if undecided 改为 if false（无条件立即请求封面），本例前三项时序
// 子用例因发出时刻被阻塞到 5.1s 且第四项 schedule 返回 true 而全红；删掉 backfillCoverGate.take 的 pc.gen != curGen 核对，
// 第四项与 TestRunSessionAssocWiring 同红。
func TestAssoc248BackfillCoverDeferredWhileUndecided(t *testing.T) {
	type coverCall struct {
		at    time.Duration
		sid   uint32
		smid  string
		title string
	}
	type emitEvent struct {
		at       time.Duration
		noLyrics bool
		lines    int
	}

	// runTimeline 在 synctest 泡内模拟 50ms 轮询：t=0 无编号认领，t=100ms 补取拿到 widMid 与在线 0 行；
	// rmAt 控制首轮 RM 放行时刻（<=0 表示不阻塞），localOpen 为真时 RM 认出 widF（49 行本地歌词）。
	// 封面请求同步阻塞 5s（由通道放行或定时睡眠模拟阻塞）。
	runTimeline := func(t *testing.T, items []assocItem, rmAt time.Duration, localOpen bool) (emits []emitEvent, covers []coverCall, finalSrc lyricSource) {
		t.Helper()
		e := widEnv(items...)
		e.fs.put(widLyrF, []byte(makeLRC248(49, "本地海底")))
		var rmGate gate
		if rmAt > 0 {
			e.beforeStart = rmGate.arm
			e.rm.fn = func(paths []string) ([]uint32, error) {
				rmGate.wait()
				if localOpen {
					return openBy([]string{widF}, testQQPID)(paths)
				}
				return nil, nil
			}
		} else if localOpen {
			e.rm.fn = openBy([]string{widF}, testQQPID)
		}

		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			if rmAt > 0 {
				defer rmGate.release()
			}
			t0 := time.Now()
			var st lyricState
			var pendingCover backfillCoverGate
			var currentLyrics []lyricLine
			lyricsPending := true
			lastName := widMatch

			showLyrics := func(lines []lyricLine) {
				undecided := lyricsUndecided(lines, lyricsPending, st.source(), st.undecided())
				if undecided && len(currentLyrics) == 0 {
					return
				}
				currentLyrics = lines
				if len(lines) == 0 && !undecided {
					emits = append(emits, emitEvent{at: time.Since(t0), noLyrics: true})
				} else {
					emits = append(emits, emitEvent{at: time.Since(t0), lines: len(lines)})
				}
			}
			runBackfillCover := func(sid uint32, smid, _, _, title string) {
				covers = append(covers, coverCall{at: time.Since(t0), sid: sid, smid: smid, title: title})
				// 同步阻塞 5s，模拟慢网络 fetchCoverURL（真实 httpClient 超时可达 10s）。
				coverBlock := make(chan struct{})
				go func() {
					time.Sleep(5 * time.Second)
					close(coverBlock)
				}()
				<-coverBlock
			}

			// t=0：无编号认领。
			k := songKey{gen: 1, name: lastName}
			pendingCover.reset()
			st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
			r.Submit(k)
			st.refresh(k.hasIDs(), stateOffsetSec)

			// 异步在 rmAt 时刻放开首轮 RM 查询。
			if rmAt > 0 {
				go func() {
					time.Sleep(rmAt)
					rmGate.release()
				}()
			}

			const poll = 50 * time.Millisecond
			for step := 1; step <= 26; step++ {
				time.Sleep(poll)
				sampleTime := time.Now()
				elapsed := sampleTime.Sub(t0)

				// 模拟 t=100ms 补取拿到 widMid 与在线 0 行（快于首轮 RM）。
				if lyricsPending && elapsed >= 100*time.Millisecond {
					lyricsPending = false
					if !k.hasIDs() {
						k.songMid = widMid
						k.detail = songDetail{}
						r.Submit(k)
					}
					st.setOnlineRaw(nil)
					shown, changed := st.refresh(k.hasIDs(), stateOffsetSec)
					if l, ok := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec); ok {
						shown, changed = l, true
					}
					if !changed {
						shown = currentLyrics
					}
					showLyrics(shown)
					if pendingCover.schedule(st.undecided(), k.gen, 10, 0, widMid, lastName, "歌手", lastName+" - 歌手") {
						runBackfillCover(0, widMid, lastName, "歌手", lastName+" - 歌手")
					}
				}

				// 紧随补取块排空延后封面请求（与 runSession 同口径）。
				if pc, ok := pendingCover.take(false, st.undecided(), true, k.gen); ok {
					runBackfillCover(pc.sid, pc.smid, pc.name, pc.singer, pc.title)
				}

				// 每拍结果块：takeResult + !changed 时 expireWait。
				lines, changed := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec)
				if !changed {
					lines, changed = st.expireWait(sampleTime, k.hasIDs(), stateOffsetSec)
				}
				if changed {
					showLyrics(lines)
				}
			}
			finalSrc = st.source()
		})
		return emits, covers, finalSrc
	}

	t.Run("补取0行快于RM，t=230ms回srcOnline时先发无歌词再执行延后封面", func(t *testing.T) {
		emits, covers, src := runTimeline(t, []assocItem{widItemF}, 230*time.Millisecond, false)
		if src != srcOnline {
			t.Fatalf("最终来源 %v，want srcOnline", src)
		}
		if len(emits) != 1 || !emits[0].noLyrics {
			t.Fatalf("发出事件 %+v，want 恰 1 次无歌词", emits)
		}
		if emits[0].at != 250*time.Millisecond {
			t.Fatalf("发出无歌词时刻 %v，want 250ms（不得被 5s 封面阻塞）", emits[0].at)
		}
		if len(covers) != 1 || covers[0].at != 300*time.Millisecond || covers[0].smid != widMid {
			t.Fatalf("封面请求 %+v，want 在解除未决后的下一拍 300ms 发起 1 次", covers)
		}
	})

	t.Run("补取0行快于RM，t=230ms回srcLocal 49行时先发49行且0次无歌词再执行延后封面", func(t *testing.T) {
		emits, covers, src := runTimeline(t, []assocItem{widItemF}, 230*time.Millisecond, true)
		if src != srcLocal {
			t.Fatalf("最终来源 %v，want srcLocal", src)
		}
		if len(emits) != 1 || emits[0].noLyrics || emits[0].lines != 49 {
			t.Fatalf("发出事件 %+v，want 恰 1 次 49 行且 0 次无歌词", emits)
		}
		if emits[0].at != 250*time.Millisecond {
			t.Fatalf("发出本地 49 行时刻 %v，want 250ms（不得被 5s 封面阻塞）", emits[0].at)
		}
		if len(covers) != 1 || covers[0].at != 300*time.Millisecond || covers[0].smid != widMid {
			t.Fatalf("封面请求 %+v，want 在解除未决后的下一拍 300ms 发起 1 次", covers)
		}
	})

	t.Run("慢盘RM阻塞超1s，t=1s expireWait准时兜底发无歌词不被5s封面阻塞", func(t *testing.T) {
		emits, covers, src := runTimeline(t, []assocItem{widItemF}, 2*time.Second, false)
		if src != srcOnline {
			t.Fatalf("最终来源 %v，want srcOnline", src)
		}
		if len(emits) != 1 || !emits[0].noLyrics {
			t.Fatalf("发出事件 %+v，want 恰 1 次无歌词", emits)
		}
		if emits[0].at != time.Second {
			t.Fatalf("expireWait 发出无歌词时刻 %v，want 1s（硬上界不得被 5s 封面突破）", emits[0].at)
		}
		if len(covers) != 1 || covers[0].at != 1050*time.Millisecond || covers[0].smid != widMid {
			t.Fatalf("封面请求 %+v，want 在 1s 解除未决后的下一拍 1.05s 发起 1 次", covers)
		}
	})

	t.Run("非未决态（无本地条目）当拍立即请求封面", func(t *testing.T) {
		unrelatedOnline := assocItem{song: midURL("003OtherMid"), lyric: lyrOK}
		emits, covers, src := runTimeline(t, []assocItem{unrelatedOnline}, 0, false)
		if src != srcOnline {
			t.Fatalf("最终来源 %v，want srcOnline", src)
		}
		if len(emits) != 1 || !emits[0].noLyrics || emits[0].at != 100*time.Millisecond {
			t.Fatalf("无本地条目时应在补取当拍 100ms 立即发无歌词，实得 %+v", emits)
		}
		if len(covers) != 1 || covers[0].at != 100*time.Millisecond {
			t.Fatalf("非未决态应在补取当拍 100ms 立即请求封面，实得 %+v", covers)
		}
	})

	t.Run("换歌丢弃旧gen的pendingCover、放行拍与未决拍不排空", func(t *testing.T) {
		// 1. 换歌调用 reset() 清空。
		var g backfillCoverGate
		if g.schedule(true, 1, 10, 100, "Mid1", "歌1", "歌手1", "歌1 - 歌手1") {
			t.Fatal("undecided=true 时 schedule 应返回 false")
		}
		// 仍在未决态、放行拍或歌名不符时均不取出且保留暂存。
		if _, ok := g.take(false, true, true, 1); ok {
			t.Fatal("undecided=true 时不应排空")
		}
		if _, ok := g.take(true, false, true, 1); ok {
			t.Fatal("released=true（暂扣放行拍）时不应排空")
		}
		if _, ok := g.take(false, false, false, 1); ok {
			t.Fatal("非已认领歌名拍（占位串/换歌过渡）不应排空")
		}
		g.reset()
		if _, ok := g.take(false, false, true, 1); ok {
			t.Fatal("换歌 reset() 后不应再取出旧封面")
		}

		// 2. 即使未调用 reset()，curGen 前进后 take 也必须核对 gen 并丢弃旧封面。
		if g.schedule(true, 1, 10, 100, "Mid1", "歌1", "歌手1", "歌1 - 歌手1") {
			t.Fatal("undecided=true 时 schedule 应返回 false")
		}
		if _, ok := g.take(false, false, true, 2); ok {
			t.Fatal("curGen 变为 2 时应丢弃 gen=1 的旧待补封面")
		}
		if _, ok := g.take(false, false, true, 1); ok {
			t.Fatal("被 gen 守卫丢弃的旧待补封面不应残留在 gate 中")
		}

		// 3. 正常条件取出恰一次，且保留完整参数（含 coverGen）。
		if g.schedule(true, 3, 30, 300, "Mid3", "歌3", "歌手3", "歌3 - 歌手3") {
			t.Fatal("undecided=true 时 schedule 应返回 false")
		}
		pc, ok := g.take(false, false, true, 3)
		if !ok || pc.gen != 3 || pc.coverGen != 30 || pc.sid != 300 || pc.smid != "Mid3" || pc.title != "歌3 - 歌手3" {
			t.Fatalf("正常排空结果不符：ok=%v pc=%+v", ok, pc)
		}
		if _, ok := g.take(false, false, true, 3); ok {
			t.Fatal("排空后不应重复取出")
		}
	})
}

// TestAssoc248PollSleep 覆盖 lyricState.pollSleep 各分支与边界，以及 QQMusicPlayer.sleepOrStop 的打断行为（#248）。
func TestAssoc248PollSleep(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	dl := t0.Add(localDecideWindow)

	t.Run("非未决态原样返回poll", func(t *testing.T) {
		polls := []time.Duration{2000 * time.Millisecond, 50 * time.Millisecond, 30 * time.Millisecond}

		// 1. 零值/刚 reset 尚未 refresh（adopted=false）
		var st lyricState
		st.reset(1, true, dl)
		for _, poll := range polls {
			if got := st.pollSleep(poll, t0.Add(100*time.Millisecond)); got != poll {
				t.Errorf("未 adopt 时 pollSleep(%v) = %v，want %v", poll, got, poll)
			}
		}

		// 2. 在线歌词非空（非未决）
		st.setOnlineRaw(makeLines248(5, "在线"))
		st.refresh(true, stateOffsetSec)
		for _, poll := range polls {
			if got := st.pollSleep(poll, t0.Add(100*time.Millisecond)); got != poll {
				t.Errorf("在线非空时 pollSleep(%v) = %v，want %v", poll, got, poll)
			}
		}

		// 3. 在线 0 行解除未决后（expireWait）
		st.reset(2, true, dl)
		st.setOnlineRaw(nil)
		st.refresh(true, stateOffsetSec)
		st.expireWait(dl, true, stateOffsetSec)
		for _, poll := range polls {
			if got := st.pollSleep(poll, dl); got != poll {
				t.Errorf("解除未决后 pollSleep(%v) = %v，want %v", poll, got, poll)
			}
		}
	})

	t.Run("未决态按step缩短并精准对齐deadline且绝不返回0", func(t *testing.T) {
		var st lyricState
		st.reset(1, true, dl)
		st.setOnlineRaw(nil)
		st.refresh(true, stateOffsetSec)
		if !st.undecided() {
			t.Fatal("前置条件：应处于未决态")
		}

		cases := []struct {
			name string
			poll time.Duration
			now  time.Time
			want time.Duration
		}{
			{"poll=2000ms 余量900ms缩至50ms", 2000 * time.Millisecond, dl.Add(-900 * time.Millisecond), localDecidePollStep},
			{"poll=50ms 余量900ms保持50ms", 50 * time.Millisecond, dl.Add(-900 * time.Millisecond), localDecidePollStep},
			{"poll=30ms 余量900ms不放大", 30 * time.Millisecond, dl.Add(-900 * time.Millisecond), 30 * time.Millisecond},
			{"poll=2000ms 余量20ms精准对齐", 2000 * time.Millisecond, dl.Add(-20 * time.Millisecond), 20 * time.Millisecond},
			{"poll=30ms 余量20ms精准对齐", 30 * time.Millisecond, dl.Add(-20 * time.Millisecond), 20 * time.Millisecond},
			{"poll=30ms 余量40ms取30ms", 30 * time.Millisecond, dl.Add(-40 * time.Millisecond), 30 * time.Millisecond},
			{"poll=2000ms rem==0返回50ms防自旋", 2000 * time.Millisecond, dl, localDecidePollStep},
			{"poll=2000ms rem<0返回50ms防自旋", 2000 * time.Millisecond, dl.Add(500 * time.Millisecond), localDecidePollStep},
			{"poll=30ms rem==0返回30ms防自旋", 30 * time.Millisecond, dl, 30 * time.Millisecond},
			{"poll=30ms rem<0返回30ms防自旋", 30 * time.Millisecond, dl.Add(100 * time.Millisecond), 30 * time.Millisecond},
		}
		for _, c := range cases {
			if got := st.pollSleep(c.poll, c.now); got != c.want {
				t.Errorf("%s：pollSleep(%v) = %v，want %v", c.name, c.poll, got, c.want)
			}
		}
	})

	t.Run("sleepOrStop正常睡满与被Stop立即打断", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := New(0, 2000)
			start := time.Now()
			if p.sleepOrStop(50 * time.Millisecond) {
				t.Fatal("未 Stop 时 sleepOrStop 应返回 false")
			}
			if d := time.Since(start); d != 50*time.Millisecond {
				t.Fatalf("未 Stop 时休眠时长 %v，want 50ms", d)
			}

			start = time.Now()
			done := make(chan bool, 1)
			go func() {
				done <- p.sleepOrStop(2 * time.Second)
			}()
			synctest.Wait()
			time.Sleep(15 * time.Millisecond)
			p.Stop()
			if stopped := <-done; !stopped {
				t.Fatal("Stop 后 sleepOrStop 应返回 true")
			}
			if d := time.Since(start); d != 15*time.Millisecond {
				t.Fatalf("Stop 应在 15ms 立即打断 2s 休眠，实得 %v", d)
			}
		})
	})
}

// TestAssoc248UndecidedDeadlineUnderLongPoll 回归长轮询配置、读失败拍与慢网络下未决态截止检查（#248）：
// 配置合法最大轮询 qqmusic-poll=2000ms 且关联表含本地条目时，播放已有编号的在线纯音乐或遭遇 ReadAllMetadata 连续失败、
// 换歌/补取网络耗时 >= 1s 时，未决态不得受长轮询或尾部跳过拖延突破 localDecideWindow（1s）硬上界。
// 本测试使用 testing/synctest + 真实 assocReader + lyricState + pollSleep + EffectivePollInterval(2000) 覆盖：
//  1. poll=2000ms，网络 t=100ms 返回 0 行，后台 RM t=230ms 返回非本地 → <=300ms 采纳并发出 index:-1，随后休眠恢复 2000ms；
//  2. poll=2000ms，网络 t=100ms 返回 0 行，后台 RM t=230ms 返回本地 49 行 → <=300ms 发出 49 行且 0 次 index:-1，随后恢复 2000ms；
//  3. poll=2000ms，网络 t=123ms 返回 0 行，慢盘 RM 阻塞 >1s → 精准在 t=1.0s 由 expireWait 解除未决并发出 index:-1，随后恢复 2000ms；
//  4. poll=2000ms，t=100ms 后 ReadAllMetadata 连续失败（仅走 if err != nil 的未决 drainAssoc），RM t=230ms 返回与慢盘 t=1.0s 到期
//     均准时解除未决并发出 index:-1、不卡死也不自旋；
//  5. 换歌与补取网络耗时 1.2s >= localDecideWindow 时，当拍 expireWait 直接解除未决并发出 index:-1（补取不误延后封面请求）。
//
// 变异自证（隔离临时副本实测）：把 lyricstate.go 的 pollSleep 改为恒返回 poll，本例全部 5 项 poll=2000ms 时序子用例
// （发出时刻被拖延至 2.1s / 2.123s）与 TestAssoc248PollSleep 均红。
func TestAssoc248UndecidedDeadlineUnderLongPoll(t *testing.T) {
	type emitEvent struct {
		at       time.Duration
		noLyrics bool
		lines    int
	}

	poll := EffectivePollInterval(2000)
	if poll != 2*time.Second {
		t.Fatalf("前置条件：EffectivePollInterval(2000) = %v，want 2s", poll)
	}

	// runClaimLongPoll 模拟已有编号在线纯音乐在 poll=2000ms 下的认领与轮询过程：
	// t=0 认领并提交读取者，网络取词在 fetchDur 返回 0 行；rmAt 控制后台首轮 RM 放行时刻，localOpen 为真时认出 widF 49 行；
	// failReads 为真时模拟认领后的每一拍 ReadAllMetadata() 均失败（仅走 if err != nil 的未决 drainAssoc(time.Now())）。
	runClaimLongPoll := func(t *testing.T, fetchDur, rmAt time.Duration, localOpen, failReads bool) (emits []emitEvent, sleeps []time.Duration, finalSrc lyricSource) {
		t.Helper()
		e := widEnv(widItemF)
		e.fs.put(widLyrF, []byte(makeLRC248(49, "本地海底")))
		var rmGate gate
		e.beforeStart = rmGate.arm
		e.rm.fn = func(paths []string) ([]uint32, error) {
			rmGate.wait()
			if localOpen {
				return openBy([]string{widF}, testQQPID)(paths)
			}
			return nil, nil
		}

		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			defer rmGate.release()
			t0 := time.Now()
			var st lyricState
			var currentLyrics []lyricLine
			lastName := widMatch

			showLyrics := func(lines []lyricLine) {
				undecided := lyricsUndecided(lines, false, st.source(), st.undecided())
				if undecided && len(currentLyrics) == 0 {
					return
				}
				currentLyrics = lines
				if len(lines) == 0 && !undecided {
					emits = append(emits, emitEvent{at: time.Since(t0), noLyrics: true})
				} else {
					emits = append(emits, emitEvent{at: time.Since(t0), lines: len(lines)})
				}
			}
			drainAssoc := func(now time.Time) {
				lines, changed := takeResult(&st, r.Results(), true, stateOffsetSec)
				if !changed {
					lines, changed = st.expireWait(now, true, stateOffsetSec)
				}
				if changed {
					showLyrics(lines)
				}
			}

			// 异步在 rmAt 时刻放开后台首轮 RM 查询。
			go func() {
				time.Sleep(rmAt)
				rmGate.release()
			}()

			// t=0：已有编号认领，设 waitDeadline = t0 + localDecideWindow，并提交给读取者。
			k := songKey{gen: 1, songID: widID, songMid: widMid, name: lastName}
			st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
			r.Submit(k)

			// 模拟网络取词 + 封面耗时 fetchDur 后返回 0 行，截止检查以墙钟 time.Now() 判定。
			time.Sleep(fetchDur)
			st.setOnlineRaw(nil)
			currentLyrics, _ = st.refresh(k.hasIDs(), stateOffsetSec)
			if lines, changed := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec); changed {
				currentLyrics = lines
			} else if lines, changed := st.expireWait(time.Now(), k.hasIDs(), stateOffsetSec); changed {
				currentLyrics = lines
			}
			if len(currentLyrics) == 0 && !lyricsUndecided(currentLyrics, false, st.source(), st.undecided()) {
				emits = append(emits, emitEvent{at: time.Since(t0), noLyrics: true})
			} else if len(currentLyrics) > 0 {
				emits = append(emits, emitEvent{at: time.Since(t0), lines: len(currentLyrics)})
			}

			// 轮询循环：按 lyricSt.pollSleep(poll, time.Now()) 休眠；记录休眠长度，解除未决后再记录一拍以验证恢复为 2000ms。
			recovered := false
			for iter := 0; iter < 40; iter++ {
				d := st.pollSleep(poll, time.Now())
				sleeps = append(sleeps, d)
				if !st.undecided() {
					if recovered {
						break
					}
					recovered = true
				}
				time.Sleep(d)
				if failReads {
					// 模拟 ReadAllMetadata() 失败跳过尾部，仅走 if err != nil 内的未决 drainAssoc(time.Now())。
					if lastName != "" && st.undecided() {
						drainAssoc(time.Now())
					}
					continue
				}
				if lastName != "" {
					drainAssoc(time.Now())
				}
			}
			finalSrc = st.source()
		})
		return emits, sleeps, finalSrc
	}

	t.Run("poll=2000ms 后台t=230ms回非本地，<=300ms发index:-1随后恢复2000ms", func(t *testing.T) {
		emits, sleeps, src := runClaimLongPoll(t, 100*time.Millisecond, 230*time.Millisecond, false, false)
		if src != srcOnline {
			t.Fatalf("最终来源 %v，want srcOnline", src)
		}
		if len(emits) != 1 || !emits[0].noLyrics {
			t.Fatalf("发出事件 %+v，want 恰 1 次无歌词", emits)
		}
		if emits[0].at > 300*time.Millisecond {
			t.Fatalf("发出无歌词时刻 %v，超过 300ms（poll=2000ms 不得拖延未决态）", emits[0].at)
		}
		if emits[0].at != 250*time.Millisecond {
			t.Fatalf("发出无歌词时刻 %v，want 250ms", emits[0].at)
		}
		if len(sleeps) < 2 || sleeps[0] != localDecidePollStep || sleeps[len(sleeps)-1] != poll {
			t.Fatalf("休眠序列 %v，期望未决期间为 %v、解除后恢复 %v", sleeps, localDecidePollStep, poll)
		}
	})

	t.Run("poll=2000ms 后台t=230ms回本地49行，<=300ms发49行且0次无歌词随后恢复2000ms", func(t *testing.T) {
		emits, sleeps, src := runClaimLongPoll(t, 100*time.Millisecond, 230*time.Millisecond, true, false)
		if src != srcLocal {
			t.Fatalf("最终来源 %v，want srcLocal", src)
		}
		if len(emits) != 1 || emits[0].noLyrics || emits[0].lines != 49 {
			t.Fatalf("发出事件 %+v，want 恰 1 次 49 行且 0 次无歌词", emits)
		}
		if emits[0].at != 250*time.Millisecond {
			t.Fatalf("发出本地 49 行时刻 %v，want 250ms", emits[0].at)
		}
		if len(sleeps) < 2 || sleeps[len(sleeps)-1] != poll {
			t.Fatalf("解除未决后休眠应恢复 %v，实得 %v", poll, sleeps)
		}
	})

	t.Run("poll=2000ms 慢盘阻塞超1s，精准在t=1.0s由expireWait解除未决随后恢复2000ms", func(t *testing.T) {
		// 取 123ms（非 50ms 整倍数），同时验证最后一拍按 rem=27ms 精准对齐 waitDeadline=1.0s。
		emits, sleeps, src := runClaimLongPoll(t, 123*time.Millisecond, 2*time.Second, false, false)
		if src != srcOnline {
			t.Fatalf("最终来源 %v，want srcOnline", src)
		}
		if len(emits) != 1 || !emits[0].noLyrics {
			t.Fatalf("发出事件 %+v，want 恰 1 次无歌词", emits)
		}
		if emits[0].at != localDecideWindow {
			t.Fatalf("expireWait 发出无歌词时刻 %v，want 精准 1s", emits[0].at)
		}
		if !slices.Contains(sleeps, 27*time.Millisecond) {
			t.Fatalf("休眠序列 %v 应包含精准对齐余量 27ms", sleeps)
		}
		if sleeps[len(sleeps)-1] != poll {
			t.Fatalf("解除未决后休眠应恢复 %v，实得 %v", poll, sleeps)
		}
	})

	t.Run("poll=2000ms ReadAllMetadata连续失败，后台t=230ms返回仍准时解除未决不自旋", func(t *testing.T) {
		emits, sleeps, src := runClaimLongPoll(t, 100*time.Millisecond, 230*time.Millisecond, false, true)
		if src != srcOnline {
			t.Fatalf("最终来源 %v，want srcOnline", src)
		}
		if len(emits) != 1 || !emits[0].noLyrics || emits[0].at != 250*time.Millisecond {
			t.Fatalf("连续读失败时应在 t=250ms 准时发无歌词，实得 %+v", emits)
		}
		for i, d := range sleeps {
			if d <= 0 {
				t.Fatalf("第 %d 拍休眠时长 %v <= 0（发生忙等自旋）", i, d)
			}
		}
		if len(sleeps) > 10 || sleeps[len(sleeps)-1] != poll {
			t.Fatalf("连续读失败解除未决后应恢复 %v 且无多余唤醒，实得 %d 次 %v", poll, len(sleeps), sleeps)
		}
	})

	t.Run("poll=2000ms ReadAllMetadata连续失败且慢盘超1s，t=1.0s准时expireWait解除不自旋", func(t *testing.T) {
		emits, sleeps, src := runClaimLongPoll(t, 123*time.Millisecond, 2*time.Second, false, true)
		if src != srcOnline {
			t.Fatalf("最终来源 %v，want srcOnline", src)
		}
		if len(emits) != 1 || !emits[0].noLyrics || emits[0].at != localDecideWindow {
			t.Fatalf("连续读失败且慢盘时应在 t=1.0s 精准发无歌词，实得 %+v", emits)
		}
		for i, d := range sleeps {
			if d <= 0 {
				t.Fatalf("第 %d 拍休眠时长 %v <= 0（发生忙等自旋）", i, d)
			}
		}
		if len(sleeps) > 25 || sleeps[len(sleeps)-1] != poll {
			t.Fatalf("到期解除后应恢复 %v 且无多余唤醒，实得 %d 次 %v", poll, len(sleeps), sleeps)
		}
	})

	t.Run("换歌网络耗时1.2s>=1s，当拍expireWait直接解除未决不多等一拍", func(t *testing.T) {
		emits, sleeps, src := runClaimLongPoll(t, 1200*time.Millisecond, 3*time.Second, false, false)
		if src != srcOnline {
			t.Fatalf("最终来源 %v，want srcOnline", src)
		}
		if len(emits) != 1 || !emits[0].noLyrics || emits[0].at != 1200*time.Millisecond {
			t.Fatalf("换歌当拍耗时 1.2s 时应于 t=1.2s 当拍即发无歌词，实得 %+v", emits)
		}
		if len(sleeps) == 0 || sleeps[0] != poll {
			t.Fatalf("换歌当拍已解除未决，首拍休眠应直接为 %v，实得 %v", poll, sleeps)
		}
	})

	t.Run("补取网络耗时1.2s>=1s，当拍expireWait直接解除未决且不误延后封面", func(t *testing.T) {
		e := widEnv(widItemF)
		var rmGate gate
		e.beforeStart = rmGate.arm
		e.rm.fn = func([]string) ([]uint32, error) {
			rmGate.wait()
			return nil, nil
		}
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			defer rmGate.release()
			t0 := time.Now()
			var st lyricState
			var pendingCover backfillCoverGate
			var currentLyrics []lyricLine
			var emits []emitEvent

			showLyrics := func(lines []lyricLine) {
				undecided := lyricsUndecided(lines, false, st.source(), st.undecided())
				if undecided && len(currentLyrics) == 0 {
					return
				}
				currentLyrics = lines
				if len(lines) == 0 && !undecided {
					emits = append(emits, emitEvent{at: time.Since(t0), noLyrics: true})
				} else {
					emits = append(emits, emitEvent{at: time.Since(t0), lines: len(lines)})
				}
			}

			// t=0：无编号认领。
			k := songKey{gen: 1, name: widMatch}
			st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
			r.Submit(k)
			st.refresh(k.hasIDs(), stateOffsetSec)

			// t=100ms 触发补取，fetchLRC 耗时 1.2s 于 t=1.3s 返回 0 行，截止检查以墙钟 time.Now() 判定。
			time.Sleep(100 * time.Millisecond)
			k.songMid = widMid
			k.detail = songDetail{}
			r.Submit(k)
			time.Sleep(1200 * time.Millisecond)

			st.setOnlineRaw(nil)
			shown, changed := st.refresh(k.hasIDs(), stateOffsetSec)
			if l, ok := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec); ok {
				shown, changed = l, true
			} else if l, ok := st.expireWait(time.Now(), k.hasIDs(), stateOffsetSec); ok {
				shown, changed = l, true
			}
			if !changed {
				shown = currentLyrics
			}
			showLyrics(shown)
			runCoverNow := pendingCover.schedule(st.undecided(), k.gen, 10, 0, widMid, widMatch, "歌手", widMatch+" - 歌手")

			if st.undecided() {
				t.Fatal("补取耗时已超 1s，当拍 expireWait 应已解除未决")
			}
			if len(emits) != 1 || !emits[0].noLyrics || emits[0].at != 1300*time.Millisecond {
				t.Fatalf("补取当拍应于 t=1.3s 直接发无歌词，实得 %+v", emits)
			}
			if !runCoverNow {
				t.Fatal("当拍已解除未决，pendingCover.schedule 应返回 true（不误延后封面请求）")
			}
			if got := st.pollSleep(poll, time.Now()); got != poll {
				t.Fatalf("当拍已解除未决，pollSleep 应返回 %v，实得 %v", poll, got)
			}
		})
	})
}

// TestAssoc248WallClockDeadlineOnRereadFailure 回归重读失败（rereadSameSong 返回 false）导致 sampleTime 停留在旧时刻时，
// 换歌拍、补取拍与每拍尾部的未决态截止检查改用墙钟 time.Now() 仍当拍解除未决并发出 index:-1（#248）。
//
// 变异自证（隔离临时副本实测）：把 qqmusic.go 换歌/补取/尾部任一处 time.Now() 改回 sampleTime，TestRunSessionAssocWiring
// 各自变红；把本例复述的三处 time.Now() 改回 sampleTime，全部 5 个子用例（读失败/占位串/进度归零暂扣、补取重读失败、每拍尾部）均红。
func TestAssoc248WallClockDeadlineOnRereadFailure(t *testing.T) {
	poll := EffectivePollInterval(2000)

	// 三种会导致 rereadSameSong 返回 false、sampleTime 保持不更新的情形：读失败、窗口占位串、进度归零暂扣。
	rereadFailures := []struct {
		name  string
		fresh *SongMetadata
		err   error
	}{
		{"读失败", nil, errors.New("read failed")},
		{"占位串", &SongMetadata{Name: "QQ音乐", ProgressMs: 30000}, nil},
		{"进度归零暂扣", &SongMetadata{Name: widMatch, ProgressMs: 0}, nil},
	}

	t.Run("换歌拍网络耗时1.2s且rereadSameSong失败（sampleTime滞留t=0）", func(t *testing.T) {
		for _, rf := range rereadFailures {
			t.Run(rf.name, func(t *testing.T) {
				e := widEnv(widItemF)
				var rmGate gate
				e.beforeStart = rmGate.arm
				e.rm.fn = func([]string) ([]uint32, error) {
					rmGate.wait()
					return nil, nil
				}
				runAssocReader(t, e, func(t *testing.T, r *assocReader) {
					defer rmGate.release()
					t0 := time.Now()
					sampleTime := t0
					const prevMs uint32 = 30000

					var st lyricState
					k := songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch}
					st.reset(k.gen, r.LocalCheckPossible(), sampleTime.Add(localDecideWindow))
					r.Submit(k)

					// 网络取词 + 封面耗时 1.2s 返回 0 行，但重读不满足 rereadSameSong，sampleTime 停留在 t=0。
					time.Sleep(1200 * time.Millisecond)
					if rereadSameSong(rf.fresh, rf.err, widMatch, prevMs) {
						t.Fatalf("前置条件：%s 时 rereadSameSong 应返回 false", rf.name)
					}

					st.setOnlineRaw(nil)
					currentLyrics, _ := st.refresh(k.hasIDs(), stateOffsetSec)
					if !st.undecided() {
						t.Fatal("前置条件：在线 0 行后应处于未决态")
					}
					// 对照验证：若仍传滞留在 t=0 的 sampleTime，expireWait 会漏判未过期。
					if _, leaked := st.expireWait(sampleTime, k.hasIDs(), stateOffsetSec); leaked || !st.undecided() {
						t.Fatal("对照断言：滞留在 t=0 的 sampleTime 不应触发 expireWait")
					}
					// 按墙钟 time.Now() 检查，当拍（t=1.2s）立即解除未决并发出 index:-1。
					if lines, changed := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec); changed {
						currentLyrics = lines
					} else if lines, changed := st.expireWait(time.Now(), k.hasIDs(), stateOffsetSec); changed {
						currentLyrics = lines
					}
					emittedNoLyrics := len(currentLyrics) == 0 && !lyricsUndecided(currentLyrics, false, st.source(), st.undecided())
					if st.undecided() || !emittedNoLyrics {
						t.Fatalf("换歌拍重读失败（%s）时应凭墙钟 time.Now() 当拍解除未决并走 emitNoLyrics：undecided=%v emitted=%v",
							rf.name, st.undecided(), emittedNoLyrics)
					}
					if got := st.pollSleep(poll, time.Now()); got != poll {
						t.Fatalf("当拍已解除未决，pollSleep 应立即恢复 %v，实得 %v", poll, got)
					}
				})
			})
		}
	})

	t.Run("补取拍网络耗时1.2s且rereadSameSong失败（sampleTime滞留t=100ms），当拍解除且不误延后封面", func(t *testing.T) {
		e := widEnv(widItemF)
		var rmGate gate
		e.beforeStart = rmGate.arm
		e.rm.fn = func([]string) ([]uint32, error) {
			rmGate.wait()
			return nil, nil
		}
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			defer rmGate.release()
			t0 := time.Now()
			var st lyricState
			var pendingCover backfillCoverGate
			var currentLyrics []lyricLine
			var noLyricsAt []time.Duration

			showLyrics := func(lines []lyricLine) {
				undecided := lyricsUndecided(lines, false, st.source(), st.undecided())
				if undecided && len(currentLyrics) == 0 {
					return
				}
				currentLyrics = lines
				if len(lines) == 0 && !undecided {
					noLyricsAt = append(noLyricsAt, time.Since(t0))
				}
			}

			// t=0：无编号认领。
			k := songKey{gen: 1, name: widMatch}
			st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
			r.Submit(k)
			st.refresh(k.hasIDs(), stateOffsetSec)

			// t=100ms：进入补取拍（sampleTime = t0 + 100ms），fetchLRC 耗时 1.2s 于 t=1.3s 返回 0 行；
			// 重读失败导致 sampleTime 停留在 t0 + 100ms（早于 waitDeadline = t0 + 1s）。
			time.Sleep(100 * time.Millisecond)
			sampleTime := time.Now()
			k.songMid = widMid
			k.detail = songDetail{}
			r.Submit(k)
			time.Sleep(1200 * time.Millisecond)
			if rereadSameSong(nil, errors.New("read failed"), widMatch, 30000) {
				t.Fatal("前置条件：读失败时 rereadSameSong 应返回 false")
			}

			st.setOnlineRaw(nil)
			shown, changed := st.refresh(k.hasIDs(), stateOffsetSec)
			if _, leaked := st.expireWait(sampleTime, k.hasIDs(), stateOffsetSec); leaked || !st.undecided() {
				t.Fatal("对照断言：滞留在 t=100ms 的 sampleTime 不应触发 expireWait")
			}
			if l, ok := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec); ok {
				shown, changed = l, true
			} else if l, ok := st.expireWait(time.Now(), k.hasIDs(), stateOffsetSec); ok {
				shown, changed = l, true
			}
			if !changed {
				shown = currentLyrics
			}
			showLyrics(shown)
			runCoverNow := pendingCover.schedule(st.undecided(), k.gen, 10, 0, widMid, widMatch, "歌手", widMatch+" - 歌手")

			if st.undecided() || len(noLyricsAt) != 1 || noLyricsAt[0] != 1300*time.Millisecond {
				t.Fatalf("补取拍重读失败时应凭墙钟 time.Now() 于 t=1.3s 当拍解除并发无歌词：undecided=%v noLyricsAt=%v", st.undecided(), noLyricsAt)
			}
			if !runCoverNow {
				t.Fatal("当拍已解除未决，pendingCover.schedule 应返回 true（不误延后封面请求）")
			}
		})
	})

	t.Run("每拍尾部sampleTime滞留t=800ms而墙钟已达t=1.1s，drainAssoc(time.Now())当拍解除未决", func(t *testing.T) {
		e := widEnv(widItemF)
		var rmGate gate
		e.beforeStart = rmGate.arm
		e.rm.fn = func([]string) ([]uint32, error) {
			rmGate.wait()
			return nil, nil
		}
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			defer rmGate.release()
			t0 := time.Now()
			var st lyricState
			var currentLyrics []lyricLine
			var noLyricsAt []time.Duration

			showLyrics := func(lines []lyricLine) {
				undecided := lyricsUndecided(lines, false, st.source(), st.undecided())
				if undecided && len(currentLyrics) == 0 {
					return
				}
				currentLyrics = lines
				if len(lines) == 0 && !undecided {
					noLyricsAt = append(noLyricsAt, time.Since(t0))
				}
			}
			drainAssoc := func(now time.Time) {
				lines, changed := takeResult(&st, r.Results(), true, stateOffsetSec)
				if !changed {
					lines, changed = st.expireWait(now, true, stateOffsetSec)
				}
				if changed {
					showLyrics(lines)
				}
			}

			// t=0：已有编号认领，t=100ms 进入未决态（waitDeadline = t0 + 1s）。
			k := songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch}
			st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
			r.Submit(k)
			time.Sleep(100 * time.Millisecond)
			st.setOnlineRaw(nil)
			st.refresh(true, stateOffsetSec)

			// t=800ms：本拍首读落 sampleTime=t0+800ms；拍中阻塞 300ms 且未刷新 sampleTime，到达尾部时墙钟已 t=1.1s。
			time.Sleep(700 * time.Millisecond)
			sampleTime := time.Now()
			time.Sleep(300 * time.Millisecond)

			// 对照验证：若尾部仍传 sampleTime=t0+800ms，未决态不会解除。
			drainAssoc(sampleTime)
			if !st.undecided() || len(noLyricsAt) != 0 {
				t.Fatalf("对照断言：滞留 sampleTime=800ms 不应解除未决，undecided=%v noLyricsAt=%v", st.undecided(), noLyricsAt)
			}
			// 尾部改传墙钟 time.Now()（t=1.1s），当拍立即解除未决并发出 index:-1。
			drainAssoc(time.Now())
			if st.undecided() || len(noLyricsAt) != 1 || noLyricsAt[0] != 1100*time.Millisecond {
				t.Fatalf("尾部应凭墙钟 time.Now() 于 t=1.1s 当拍解除未决并发无歌词：undecided=%v noLyricsAt=%v", st.undecided(), noLyricsAt)
			}
			if got := st.pollSleep(poll, time.Now()); got != poll {
				t.Fatalf("解除未决后 pollSleep 应恢复 %v，实得 %v", poll, got)
			}
		})
	})
}

// TestAssoc248SwitchHoldInteraction 回归 #202 换歌前归零暂扣与 #248 首轮本地认定未决态在长轮询（poll=2000ms）下的协同不变量：
// 曲 A 在 30s 认领进入未决态（waitDeadline=1s），于 t=0.8s 进度归零进入 400ms 暂扣（held=true）：
//  1. 0.8s..1.15s 暂扣期间严格抑制旧歌 A 的一切对外事件（0 次 index:-1），且不静默翻转 undecided 吞掉后续补发；
//  2. 若 t=1.0s 换新歌 B，hold.drop 丢弃暂扣、reset 切到曲 B，旧歌 A 全程 0 次 index:-1（零切歌前闪屏）；
//  3. 若期满未换歌，因未决态下 pollSleep 维持 50ms 缩步，t=1.2s 放行当拍（绝不被 poll=2000ms 拖到 2.8s）经
//     drainAssoc(time.Now()) 立即解除未决发出 index:-1，随后休眠恢复 2000ms。
//
// 变异自证（隔离临时副本实测）：把 lyricstate.go 的 pollSleep 在 rem <= 0 时改回返回 poll，本例放行子用例（暂扣被拖至
// 2200ms、实得 5 拍）与 TestAssoc248PollSleep 均红。
func TestAssoc248SwitchHoldInteraction(t *testing.T) {
	type songEmit struct {
		song     string
		at       time.Duration
		noLyrics bool
	}
	poll := EffectivePollInterval(2000)

	runHoldScenario := func(t *testing.T, switchToBAt time.Duration) (emits []songEmit, sleeps []time.Duration, heldTicks int, releasedAt time.Duration, claimedBAt time.Duration) {
		t.Helper()
		e := widEnv(widItemF)
		var rmGate gate
		e.beforeStart = rmGate.arm
		e.rm.fn = func([]string) ([]uint32, error) {
			rmGate.wait()
			return nil, nil
		}

		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			defer rmGate.release()
			t0 := time.Now()
			var st lyricState
			var hold switchHold
			var currentLyrics []lyricLine
			lastName := "曲A"
			var lastMemProgress uint32 = 30000
			k := songKey{gen: 1, songID: widID, songMid: widMid, name: lastName}

			showLyrics := func(lines []lyricLine) {
				undecided := lyricsUndecided(lines, false, st.source(), st.undecided())
				if undecided && len(currentLyrics) == 0 {
					return
				}
				currentLyrics = lines
				if len(lines) == 0 && !undecided {
					emits = append(emits, songEmit{song: lastName, at: time.Since(t0), noLyrics: true})
				}
			}
			drainAssoc := func(now time.Time) {
				lines, changed := takeResult(&st, r.Results(), k.hasIDs(), stateOffsetSec)
				if !changed {
					lines, changed = st.expireWait(now, k.hasIDs(), stateOffsetSec)
				}
				if changed {
					showLyrics(lines)
				}
			}

			// t=0：曲 A 在 30s 认领；t=100ms 在线返回 0 行进入未决态（waitDeadline = t0 + 1s）。
			st.reset(k.gen, r.LocalCheckPossible(), t0.Add(localDecideWindow))
			r.Submit(k)
			time.Sleep(100 * time.Millisecond)
			st.setOnlineRaw(nil)
			st.refresh(k.hasIDs(), stateOffsetSec)
			if !st.undecided() {
				t.Fatal("前置条件：曲 A 应进入未决态")
			}

			recovered := false
			for iter := 0; iter < 50; iter++ {
				d := st.pollSleep(poll, time.Now())
				sleeps = append(sleeps, d)
				if !st.undecided() && (switchToBAt == 0 || lastName == "曲B") {
					if recovered {
						break
					}
					recovered = true
				}
				time.Sleep(d)
				sampleTime := time.Now()
				elapsed := sampleTime.Sub(t0)

				// 构造本拍读数：t<0.8s 保持 30s；t>=0.8s 进度归零；若指定换歌则在 switchToBAt 换歌名为曲 B。
				metaName := "曲A"
				var metaProgress uint32 = 30000
				if elapsed >= 800*time.Millisecond {
					metaProgress = 0
				}
				if switchToBAt > 0 && elapsed >= switchToBAt {
					metaName = "曲B"
					metaProgress = 0
				}

				// 切歌检测先于暂扣判定（同 runSession）：换歌时 hold.drop 丢弃暂扣并切换曲 B（假设曲 B 有歌词，非未决）。
				if metaName != lastName {
					hold.drop(sampleTime)
					lastName = metaName
					lastMemProgress = metaProgress
					claimedBAt = elapsed
					k = songKey{gen: k.gen + 1, songID: widID + 1, songMid: "000SongBMid00", name: lastName}
					st.reset(k.gen, r.LocalCheckPossible(), sampleTime.Add(localDecideWindow))
					r.Submit(k)
					st.setOnlineRaw(makeLines248(10, "曲B歌词"))
					currentLyrics, _ = st.refresh(k.hasIDs(), stateOffsetSec)
					continue
				}

				// #202 换歌前归零暂扣：暂扣期间抑制本首一切对外事件（含未决态 index:-1），continue 跳过尾部。
				held, released := hold.step(lastMemProgress, metaProgress, sampleTime)
				if held {
					heldTicks++
					// 暂扣期间不得发出任何旧歌 A 的无歌词事件，且不得静默翻转 undecided。
					if len(emits) != 0 {
						t.Fatalf("t=%v 暂扣期间不应发出任何对外事件，实得 %+v", elapsed, emits)
					}
					if !st.undecided() {
						t.Fatalf("t=%v 暂扣期间不应静默翻转 undecided", elapsed)
					}
					continue
				}
				if released {
					releasedAt = elapsed
					lastMemProgress = metaProgress
				}

				if lastName != "" {
					drainAssoc(time.Now())
				}
			}
		})
		return emits, sleeps, heldTicks, releasedAt, claimedBAt
	}

	t.Run("暂扣期间t=1.0s换新歌B：旧歌A全程0次index:-1，曲B在t=1.0s准时认领", func(t *testing.T) {
		emits, sleeps, heldTicks, releasedAt, claimedBAt := runHoldScenario(t, time.Second)
		if heldTicks == 0 {
			t.Fatal("前置条件：t=0.8s..0.95s 应进入暂扣")
		}
		if releasedAt != 0 {
			t.Fatalf("换歌应丢弃暂扣而不放行，实得 releasedAt=%v", releasedAt)
		}
		if claimedBAt != time.Second {
			t.Fatalf("曲 B 应在 t=1.0s 准时认领（不被 poll=2000ms 拖延），实得 %v", claimedBAt)
		}
		for _, ev := range emits {
			if ev.song == "曲A" && ev.noLyrics {
				t.Fatalf("换歌前旧歌 A 不得闪发 index:-1，实得 %+v", emits)
			}
		}
		if len(sleeps) == 0 || sleeps[len(sleeps)-1] != poll {
			t.Fatalf("切到有歌词的曲 B 后休眠应恢复 %v，实得 %v", poll, sleeps)
		}
	})

	t.Run("暂扣期满未换歌：0.8s..1.15s抑制事件，t=1.2s放行当拍立即发index:-1并恢复2000ms", func(t *testing.T) {
		emits, sleeps, heldTicks, releasedAt, _ := runHoldScenario(t, 0)
		if heldTicks != 8 {
			t.Fatalf("0.8s..1.15s 按 50ms 步进应暂扣 8 拍，实得 %d 拍", heldTicks)
		}
		if releasedAt != 1200*time.Millisecond {
			t.Fatalf("放行时刻 %v，want 1.2s（不得被 poll=2000ms 拖延至 2.8s）", releasedAt)
		}
		if len(emits) != 1 || emits[0].song != "曲A" || !emits[0].noLyrics || emits[0].at != 1200*time.Millisecond {
			t.Fatalf("放行当拍 t=1.2s 应立即发出恰 1 次曲 A 无歌词，实得 %+v", emits)
		}
		for i, d := range sleeps {
			if d <= 0 {
				t.Fatalf("第 %d 拍休眠 %v <= 0（忙等自旋）", i, d)
			}
		}
		if len(sleeps) < 2 || sleeps[len(sleeps)-1] != poll {
			t.Fatalf("放行拍解除未决后休眠应恢复 %v，实得 %v", poll, sleeps)
		}
	})
}
