package assoc

// 本文件只测 MergeConclusion：把一个代次里依次各次扫描的结论并成本代次的结论（设计 §6.4 第 4 步「结论」，§7「结论（本代次）」）。
// 认出与不唯一总是取这一次的；找不到、不完整、待换算与读不出（零值）不推翻此前认出的。

import "testing"

// 变异自证（实测）：找不到、不完整、待换算、零值推翻已认出的，各自那一例红；不唯一不推翻已认出的，「认出 A 后不唯一」一例红。
func TestMergeConclusion(t *testing.T) {
	const (
		uA = `http://stream4.qqmusic.qq.com/000AbCd.wma`
		uB = `http://streamundefined.qqmusic.qq.com/123.wma`
	)
	// 此前的与这一次的三项计数各不相同，断言取用的是哪一方
	prev := func(v RecordVerdict, u string) RecordConclusion {
		return RecordConclusion{Verdict: v, URL: u, Qualified: 1, Voided: 2, Malformed: 3}
	}
	cur := func(v RecordVerdict, u string) RecordConclusion {
		return RecordConclusion{Verdict: v, URL: u, Qualified: 4, Voided: 5, Malformed: 6}
	}
	foundA := prev(RecordFound, uA)
	for _, c := range []struct {
		name      string
		prev, cur RecordConclusion
		takeCur   bool
	}{
		{"此前为零值，这一次读不出", RecordConclusion{}, RecordConclusion{}, true},
		{"此前为零值，这一次认出", RecordConclusion{}, cur(RecordFound, uA), true},
		{"此前为零值，这一次找不到", RecordConclusion{}, cur(RecordNotFound, ""), true},
		{"此前为零值，这一次不唯一", RecordConclusion{}, cur(RecordAmbiguous, ""), true},
		{"此前为零值，这一次待换算", RecordConclusion{}, cur(RecordPending, ""), true},
		{"此前为零值，这一次不完整", RecordConclusion{}, cur(RecordIncomplete, ""), true},
		{"认出 A 后认出 A", foundA, cur(RecordFound, uA), true},
		{"认出 A 后认出 B（改换）", foundA, cur(RecordFound, uB), true},
		{"认出 A 后不唯一（作废）", foundA, cur(RecordAmbiguous, ""), true},
		{"认出 A 后找不到", foundA, cur(RecordNotFound, ""), false},
		{"认出 A 后不完整", foundA, cur(RecordIncomplete, ""), false},
		{"认出 A 后待换算", foundA, cur(RecordPending, ""), false},
		{"认出 A 后读不出前缀或歌名不符（零值）", foundA, RecordConclusion{}, false},
		{"不唯一后认出 A", prev(RecordAmbiguous, ""), cur(RecordFound, uA), true},
		{"找不到后认出 A", prev(RecordNotFound, ""), cur(RecordFound, uA), true},
		{"待换算后找不到", prev(RecordPending, ""), cur(RecordNotFound, ""), true},
		{"不唯一后找不到（不唯一不留着）", prev(RecordAmbiguous, ""), cur(RecordNotFound, ""), true},
		{"不完整后读不出（零值）", prev(RecordIncomplete, ""), RecordConclusion{}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := c.prev
			if c.takeCur {
				want = c.cur
			}
			if got := MergeConclusion(c.prev, c.cur); got != want {
				t.Fatalf("MergeConclusion(%+v, %+v) = %+v，want %+v", c.prev, c.cur, got, want)
			}
		})
	}
}

// id 键优先与合并的配合（设计 §6.4 第 4 步「与 id 键优先的配合」，§7「结论（本代次）」第 5 版）：各次的单次结论由 ConcludeRecord
// 按 id 键优先求出，依次经 MergeConclusion 并成本代次的结论；合并规则不变。每次合并后断言本代次的结论、认出的网址，以及是否
// 改换（合并前后认出的网址不同，即读取者要重新求结果，同 readRecord）。网址与编号见 recordconclude_test.go 的 prio* 夹具。
// 变异自证（实测）：去掉 id 键优先，除 22.22 一例外四例红；待换算挪到 id 键优先之前，「只有 songid」一例红；多于一种 id 键
// 网址时取第一种，「两种 id 键网址」一例红。
func TestConcludeThenMergeIDPriority(t *testing.T) {
	type scan struct {
		sc      RecordScan
		songID  uint32
		mid     string
		verdict RecordVerdict
		url     string
		changed bool
	}
	both := func(sc RecordScan, v RecordVerdict, u string, changed bool) scan {
		return scan{sc, prioID, prioMid, v, u, changed}
	}
	for _, c := range []struct {
		name  string
		scans []scan
	}{
		{"只见 mid 键，复扫见两条并存，此后照旧并存", []scan{
			both(prioFull(prioMidA), RecordFound, prioMidA, true),
			both(prioFull(prioMidA, prioIDA), RecordFound, prioIDA, true),
			both(prioFull(prioMidA, prioIDA), RecordFound, prioIDA, false),
		}},
		{"并存，复扫只见 mid 键（id 键那条改写），又并存", []scan{
			both(prioFull(prioMidA, prioIDA), RecordFound, prioIDA, true),
			both(prioFull(prioMidA), RecordFound, prioMidA, true),
			both(prioFull(prioMidA, prioIDA), RecordFound, prioIDA, true),
		}},
		{"22.22 启动：先只见 id 键，后只见 mid 键", []scan{
			both(prioFull(prioIDA), RecordFound, prioIDA, true),
			both(prioFull(prioMidA), RecordFound, prioMidA, true),
		}},
		{"只有 songid 时 id 键合格、mid 键待换算，补上 songmid 后并存", []scan{
			{prioFull(prioMidA, prioIDA), prioID, "", RecordFound, prioIDA, true},
			both(prioFull(prioMidA, prioIDA), RecordFound, prioIDA, false),
		}},
		{"两种 id 键网址，复扫见 id 键一种与 mid 键并存", []scan{
			both(prioFull(prioIDA, prioIDB), RecordAmbiguous, "", false),
			both(prioFull(prioMidA, prioIDA), RecordFound, prioIDA, true),
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var conc RecordConclusion
			for i, s := range c.scans {
				prev := conc
				conc = MergeConclusion(prev, ConcludeRecord(s.sc, s.songID, s.mid))
				if changed := conc.URL != prev.URL; conc.Verdict != s.verdict || conc.URL != s.url || changed != s.changed {
					t.Fatalf("第 %d 次：本代次 %v %q、改换 %v，want %v %q、改换 %v", i+1, conc.Verdict, conc.URL, changed, s.verdict, s.url, s.changed)
				}
			}
		})
	}
}
