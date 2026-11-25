package qqmusic

// 本文件测换歌认领时的改名证据与载入段的记录（#218 第 5 版，设计 §6.5 第 5 步、§6.8「改名证据」「载入段的记录」、§7「会话」）：
// loadEdge 的窗口与载入段（表驱动，读数时刻由用例给定，不取当前时刻）、excludeFor、两条 Detail，以及版本表的门槛 G 全为 0。
// runSession 的接线由 assocwiring_test.go 守；会话一侧照 runSession 的复述由 reopenreread_test.go 的 TestRenameEvidenceReplay 守。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
)

// spec100 是机制用例注入门槛的规格：以 22.82 的播放状态为底，只在测试里把 LoadingMinGap 设为 100 ms（版本表不改）。
func spec100() playStateSpec {
	s := knownVersions["22.82"].PlayState
	s.LoadingMinGap = 100 * time.Millisecond
	return s
}

// lwEv 是交给 loadEdge 的一件事：读成的读数（'r'）、读失败的读数（'f'，rawOK 为假）、整拍元数据读失败（'m'，missed）、
// 一次中途的换歌认领（'c'）。at 为读数时刻，单位 ms，相对 lwT0，可为负。
type lwEv struct {
	kind byte
	at   int
	raw  uint32
}

func lwRead(at int, raw uint32) lwEv { return lwEv{'r', at, raw} }
func lwFail(at int) lwEv             { return lwEv{'f', at, 2} } // 原始取值即使是载入值 2，读失败也不算
func lwMiss() lwEv                   { return lwEv{kind: 'm'} }
func lwClaim() lwEv                  { return lwEv{kind: 'c'} }

var lwT0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local)

func lwAt(ms int) time.Time { return lwT0.Add(msd(ms)) } // msd 见 playstate_test.go

// lwFeed 依次把 evs 交给 e；中途的认领按 usable、spec、poll 求值，返回各次中途认领的结果。
func lwFeed(e *loadEdge, spec playStateSpec, usable bool, poll time.Duration, evs []lwEv) []renameEvidence {
	var mids []renameEvidence
	for _, ev := range evs {
		switch ev.kind {
		case 'r':
			e.step(spec, ev.raw, true, lwAt(ev.at))
		case 'f':
			e.step(spec, ev.raw, false, lwAt(ev.at))
		case 'm':
			e.missed()
		case 'c':
			mids = append(mids, e.claim(usable, spec, poll))
		}
	}
	return mids
}

// lwFirstPlay 是 11-24 首次放的窗口：认领文件名的那一拍（t=0）读到 2，随即认领（见到边沿、不成立、清零）；此后相邻读数交替
// 相隔 50、60 ms，都是 3，最后一个读数（t=710）是改名的那一拍。
func lwFirstPlay() []lwEv {
	evs := []lwEv{lwRead(0, 2), lwClaim()}
	at := 0
	for i := 0; at < 700; i++ {
		at += 50 + 10*(i%2)
		evs = append(evs, lwRead(at, 3))
	}
	return evs
}

// lwEvery 返回 t=0 读到 3、随即认领，此后每 step ms 读到 3、直到不小于 until 的读数。
func lwEvery(step, until int) []lwEv {
	evs := []lwEv{lwRead(0, 3), lwClaim()}
	for at := step; at-step < until; at += step {
		evs = append(evs, lwRead(at, 3))
	}
	return evs
}

// lwEdit 返回 evs 的拷贝，第 i 个（负数自末尾数）换成 ev。
func lwEdit(evs []lwEv, i int, ev lwEv) []lwEv {
	out := append([]lwEv(nil), evs...)
	if i < 0 {
		i += len(out)
	}
	out[i] = ev
	return out
}

// 版本表里每个版本的门槛 G 都为 0：核定之前改名证据一律不成立（设计 §6.5 第 5 步「门槛」）；新增版本照抄一整段也是 0。
// 变异自证（实测）：给 22.82 填 100 ms，本例红。
func TestKnownVersionsLoadingMinGapZero(t *testing.T) {
	for ver, o := range knownVersions {
		if o.PlayState.LoadingMinGap != 0 {
			t.Errorf("%s 的 PlayState.LoadingMinGap = %v，want 0（未经设计 §10 第 26 项实测核定）", ver, o.PlayState.LoadingMinGap)
		}
	}
}

// 现行版本表（22.82 的取值，G 为 0）：轮询 50 ms、读数每 50 ms 一次都为 3、没有读失败 → 不成立，原因为门槛为 0。第 5 版第 2 轮
// 设计审的反例：读数在 t=0、50 ms 都为 3，真实换歌的载入值落在 t=10～30 ms 之间、读数里看不到 → 不成立（排除上一首的文件）。
// 变异自证（实测）：给 22.82 填 100 ms，两例都成立而红。
func TestLoadEdgeClaimProductionZero(t *testing.T) {
	spec, poll := knownVersions["22.82"].PlayState, 50*time.Millisecond
	usable := spec.loadingUsable(false, poll)
	if !usable {
		t.Fatal("22.82、轮询 50 ms 的载入信号应可用 —— 用例前提不成立")
	}
	for _, c := range []struct {
		name string
		evs  []lwEv
		gap  time.Duration
	}{
		{"每 50 ms 一次都为 3", lwEvery(50, 700), msd(50)},
		{"反例：载入值落在两次读数之间", []lwEv{lwRead(0, 3), lwClaim(), lwRead(50, 3)}, msd(50)},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newLoadEdge(poll)
			e.onSegment = nil
			lwFeed(&e, spec, usable, poll, c.evs)
			got := e.claim(usable, spec, poll)
			if got.ok || got.reason != "no loading sample threshold for this QQ Music version" || got.maxGap != c.gap {
				t.Fatalf("claim = %+v，want 不成立、门槛为 0、最长间隔 %v", got, c.gap)
			}
		})
	}
}

// 改名证据的窗口（设计 §6.5 第 5 步、§7「会话」的「改名证据」），注入 G = 100 ms。各例断言最后一次认领的成立与否、原因与窗口里
// 相邻读数的最长间隔。
// 变异自证（实测）：claim 不看 sawGap，「会话阻塞 450 ms」「相隔 100 ms」「认领前后相隔 170 ms」红；claim 不比轮询间隔与门槛，
// 「慢轮询漏看载入」「轮询 100 ms」红；门槛比较写成 >，「相隔 100 ms」红；claim 同时清掉 lastAt，「认领前后相隔 170 ms」红（另有
// 「轮询 1.5 秒」「慢轮询漏看载入」最长间隔不符而红）；claim 不清 sawEdge，「成立」「相隔 99 ms」等认领前一拍为 2 的五例红。
func TestLoadEdgeClaim(t *testing.T) {
	const (
		unusable  = "loading signal unusable"
		zeroGate  = "no loading sample threshold for this QQ Music version"
		slowPoll  = "poll interval not shorter than the loading sample threshold"
		edgeSeen  = "loading state seen"
		unreadble = "unreadable play state"
		gapSeen   = "sampling gap"
	)
	fp := lwFirstPlay()
	allFail := []lwEv{lwFail(0), lwClaim()}
	for at := 50; at <= 700; at += 50 {
		allFail = append(allFail, lwFail(at))
	}
	missAt330 := append(append(append([]lwEv(nil), fp[:7]...), lwMiss()), fp[8:]...) // t=330 那一拍整拍读失败
	if fp[7].at != 330 {
		t.Fatalf("首次放序列第 7 个读数在 %d ms，want 330 —— 夹具变了", fp[7].at)
	}
	gapAt := func(gap int) []lwEv { // 首次放序列，t=110 之后的那次读数相隔 gap ms，其余照旧
		evs := []lwEv{lwRead(0, 2), lwClaim(), lwRead(50, 3), lwRead(110, 3)}
		for at := 110 + gap; at <= 760; at += 50 {
			evs = append(evs, lwRead(at, 3))
		}
		return evs
	}
	after170 := []lwEv{lwRead(0, 3), lwClaim()}
	for at := 170; at <= 720; at += 50 {
		after170 = append(after170, lwRead(at, 3))
	}
	for _, c := range []struct {
		name   string
		spec   playStateSpec
		bad    bool          // 本会话已判标志不可信
		poll   time.Duration // 0 即 50 ms
		evs    []lwEv
		ok     bool
		reason string
		maxGap time.Duration
	}{
		{"成立：首次放、改名前后都没有载入值", spec100(), false, 0, fp, true, "", msd(60)},
		{"改名那一拍读到 2", spec100(), false, 0, lwEdit(fp, -1, lwRead(710, 2)), false, edgeSeen, msd(60)},
		{"改名前一拍出现 2", spec100(), false, 0, lwEdit(fp, -2, lwRead(660, 2)), false, edgeSeen, msd(60)},
		{"窗口里重开的边沿（补取 gen 加一与重开都不清零）", spec100(), false, 0, lwEdit(fp, 6, lwRead(270, 2)), false, edgeSeen, msd(60)},
		{"认领之后的重读里出现 2", spec100(), false, 0, append([]lwEv{lwRead(0, 3), lwClaim(), lwRead(60, 2)}, fp[3:]...), false, edgeSeen, msd(60)},
		{"其间有一拍播放状态读失败", spec100(), false, 0, lwEdit(fp, 7, lwFail(330)), false, unreadble, msd(110)},
		{"其间有一拍整拍元数据读失败", spec100(), false, 0, missAt330, false, unreadble, msd(110)},
		{"本会话已判标志不可信", spec100(), true, 0, fp, false, unusable, msd(60)},
		{"轮询 1.5 秒", spec100(), false, 1500 * time.Millisecond, []lwEv{lwRead(0, 3), lwClaim(), lwRead(1500, 3)}, false, unusable, msd(1500)},
		{"20.05 的取值（没有载入取值）", func() playStateSpec { s := knownVersions["20.05"].PlayState; s.LoadingMinGap = msd(100); return s }(),
			false, 0, fp, false, unusable, msd(60)},
		{"未收录播放状态的版本", playStateSpec{LoadingMinGap: msd(100)}, false, 0, allFail, false, unusable, 0},
		{"22.52 的取值（门槛为 0）", knownVersions["22.52"].PlayState, false, 0, fp, false, zeroGate, msd(60)},
		{"每拍都读不成（借用偏移表的版本）", spec100(), false, 0, allFail, false, unreadble, 0},
		{"慢轮询漏看载入（第 5 版第 1 轮设计审）", spec100(), false, 500 * time.Millisecond,
			[]lwEv{lwRead(0, 3), lwClaim(), lwRead(500, 3)}, false, slowPoll, msd(500)},
		{"会话阻塞 450 ms（第 5 版第 1 轮设计审）", spec100(), false, 0,
			[]lwEv{lwRead(0, 3), lwClaim(), lwRead(50, 3), lwRead(100, 3), lwRead(550, 3), lwRead(600, 3)}, false, gapSeen, msd(450)},
		{"相隔 99 ms", spec100(), false, 0, gapAt(99), true, "", msd(99)},
		{"相隔 100 ms", spec100(), false, 0, gapAt(100), false, gapSeen, msd(100)},
		{"轮询 99 ms", spec100(), false, 99 * time.Millisecond, lwEvery(99, 700), true, "", msd(99)},
		{"轮询 100 ms", spec100(), false, 100 * time.Millisecond, lwEvery(100, 700), false, slowPoll, msd(100)},
		{"认领前后相隔 170 ms（lastAt 跨认领保留）", spec100(), false, 0, after170, false, gapSeen, msd(170)},
	} {
		t.Run(c.name, func(t *testing.T) {
			poll := c.poll
			if poll == 0 {
				poll = 50 * time.Millisecond
			}
			usable := c.spec.loadingUsable(c.bad, poll)
			e := newLoadEdge(poll)
			e.onSegment = nil
			lwFeed(&e, c.spec, usable, poll, c.evs)
			got := e.claim(usable, c.spec, poll)
			if got.ok != c.ok || got.reason != c.reason || got.maxGap != c.maxGap {
				t.Fatalf("claim = {ok %v reason %q maxGap %v}，want {ok %v reason %q maxGap %v}", got.ok, got.reason, got.maxGap, c.ok, c.reason, c.maxGap)
			}
		})
	}
}

// 认领求值之后清零窗口（边沿、读不成、采样缺口与最长间隔），下一次认领只看这次之后；lastAt、loading 与载入段的状态不清。
// 认领之前先见到边沿、读失败与 300 ms 的缺口，那次认领不成立（见到边沿）；此后每 50 ms 读到 3，下一次认领成立、最长间隔 50 ms。
// 变异自证（实测）：claim 不清 sawEdge，下一次认领不成立而红；claim 同时清掉 lastAt，lastAt 一项红。
func TestLoadEdgeClaimClearsWindow(t *testing.T) {
	spec, poll := spec100(), 50*time.Millisecond
	e := newLoadEdge(poll)
	e.onSegment = nil
	mids := lwFeed(&e, spec, true, poll, []lwEv{lwRead(0, 2), lwFail(50), lwRead(300, 2), lwClaim()})
	if len(mids) != 1 || mids[0].ok || mids[0].reason != "loading state seen" || mids[0].maxGap != msd(300) {
		t.Fatalf("第一次认领 %+v，want 不成立、见到载入值、最长间隔 300ms", mids)
	}
	if e.sawEdge || e.sawFail || e.sawGap || e.maxGap != 0 {
		t.Fatalf("认领之后窗口未清零：sawEdge %v sawFail %v sawGap %v maxGap %v", e.sawEdge, e.sawFail, e.sawGap, e.maxGap)
	}
	if !e.lastAt.Equal(lwAt(300)) || !e.loading || !e.inSeg || !e.seg.first.Equal(lwAt(0)) {
		t.Fatalf("认领清掉了不该清的：lastAt %v loading %v inSeg %v seg.first %v", e.lastAt, e.loading, e.inSeg, e.seg.first)
	}
	for at := 350; at <= 900; at += 50 {
		e.step(spec, 3, true, lwAt(at))
	}
	if got := e.claim(true, spec, poll); !got.ok || got.reason != "" || got.maxGap != msd(50) {
		t.Fatalf("下一次认领 %+v，want 成立、最长间隔 50ms", got)
	}
}

// excludeFor：证据成立即交空（上一首的文件照常入候选），不成立即交上一首采纳过的文件；上一首没有采纳本地文件时都为空。
// 变异自证（实测）：反过来（成立交原值、不成立交空），前两例红。
func TestExcludeFor(t *testing.T) {
	const x = `E:\Music\上一首.flac`
	for _, c := range []struct {
		name string
		prev string
		ev   renameEvidence
		want string
	}{
		{"成立", x, renameEvidence{ok: true}, ""},
		{"不成立", x, renameEvidence{reason: "sampling gap"}, x},
		{"上一首为空、成立", "", renameEvidence{ok: true}, ""},
		{"上一首为空、不成立", "", renameEvidence{reason: "loading state seen"}, ""},
	} {
		if got := excludeFor(c.prev, c.ev); got != c.want {
			t.Errorf("%s：excludeFor = %q，want %q", c.name, got, c.want)
		}
	}
}

// lwSeg 构造期望的载入段：pre 为 noPre 即没有 t₀。
const noPre = -1 << 30

func lwSeg(pre, first, last, post int) loadSegment {
	s := loadSegment{first: lwAt(first), last: lwAt(last), post: lwAt(post)}
	if pre != noPre {
		s.pre = lwAt(pre)
	}
	return s
}

// 载入段的记录（设计 §6.8「载入段的记录」、§7「会话」），轮询 50 ms，onSegment 换成记录器。各例断言报出的段（四个时刻与四个
// 标记）、上下界及其可用；真实时长已知的，断言它落在上下界之间。
// 变异自证（实测）：上界改为 t₃ − t₁（旧定义的跨度），「短载入不被记长」「只有一个载入样本」等有 t₀ 的各例红；读失败按非载入
// 样本结束一段，「读失败夹在两个载入样本之间」「读失败在段后」「段未结束不报」红；读失败记作段前的非载入样本，「读失败在段前」红。
func TestLoadSegment(t *testing.T) {
	withFlags := func(s loadSegment, f func(*loadSegment)) loadSegment { f(&s); return s }
	type bound struct {
		d  int
		ok bool
	}
	type want struct {
		seg          loadSegment
		lower, upper bound
		actual       int // 真实时长 ms，0 即不断言
	}
	for _, c := range []struct {
		name string
		evs  []lwEv
		want []want
	}{
		{"短载入不被记长（第 5 版第 2 轮设计审）", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwRead(50, 2), lwRead(100, 3)},
			[]want{{lwSeg(-50, 0, 50, 100), bound{50, true}, bound{150, true}, 52}}},
		{"只有一个载入样本", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwRead(50, 3)},
			[]want{{lwSeg(-50, 0, 0, 50), bound{0, true}, bound{100, true}, 0}}},
		{"读失败夹在两个载入样本之间（不结束一段）", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwFail(50), lwRead(100, 2), lwRead(150, 3)},
			[]want{{withFlags(lwSeg(-50, 0, 100, 150), func(s *loadSegment) { s.failInside = true }), bound{0, false}, bound{200, true}, 0}}},
		{"读失败在段后（段后样本为其后读成的）", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwFail(50), lwRead(100, 3)},
			[]want{{withFlags(lwSeg(-50, 0, 0, 100), func(s *loadSegment) { s.fail = true }), bound{0, true}, bound{150, true}, 0}}},
		{"读失败在段前（不算非载入样本）", []lwEv{lwRead(-100, 3), lwFail(-50), lwRead(0, 2), lwRead(50, 3)},
			[]want{{withFlags(lwSeg(-100, 0, 0, 50), func(s *loadSegment) { s.fail = true }), bound{0, true}, bound{150, true}, 0}}},
		{"阻塞造成的缺口在段后", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwRead(450, 3)},
			[]want{{withFlags(lwSeg(-50, 0, 0, 450), func(s *loadSegment) { s.gap = true }), bound{0, true}, bound{500, true}, 0}}},
		{"缺口落在两个载入样本之间", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwRead(450, 2), lwRead(500, 3)},
			[]want{{withFlags(lwSeg(-50, 0, 450, 500), func(s *loadSegment) { s.gapInside = true }), bound{0, false}, bound{550, true}, 0}}},
		{"相隔恰为两倍轮询不算缺口", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwRead(100, 3)},
			[]want{{lwSeg(-50, 0, 0, 100), bound{0, true}, bound{150, true}, 0}}},
		{"相隔超过两倍轮询 1 ms 即缺口", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwRead(101, 3)},
			[]want{{withFlags(lwSeg(-50, 0, 0, 101), func(s *loadSegment) { s.gap = true }), bound{0, true}, bound{151, true}, 0}}},
		{"会话第一次读数即为载入值（QQ 启动后载入上次的歌）", []lwEv{lwRead(0, 2), lwRead(50, 2), lwRead(100, 2), lwRead(150, 3)},
			[]want{{lwSeg(noPre, 0, 100, 150), bound{100, true}, bound{0, false}, 0}}},
		{"段跨过一次认领", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwClaim(), lwRead(50, 2), lwRead(100, 3)},
			[]want{{lwSeg(-50, 0, 50, 100), bound{50, true}, bound{150, true}, 0}}},
		{"暂停值与未知值都是非载入样本", []lwEv{lwRead(-50, 4), lwRead(0, 2), lwRead(50, 9)},
			[]want{{lwSeg(-50, 0, 0, 50), bound{0, true}, bound{100, true}, 0}}},
		{"两段各一条", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwRead(50, 3), lwRead(100, 2), lwRead(150, 3)},
			[]want{{lwSeg(-50, 0, 0, 50), bound{0, true}, bound{100, true}, 0}, {lwSeg(50, 100, 100, 150), bound{0, true}, bound{100, true}, 0}}},
		{"段未结束不报", []lwEv{lwRead(-50, 3), lwRead(0, 2), lwRead(50, 2), lwFail(100)}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec, poll := spec100(), 50*time.Millisecond
			var got []loadSegment
			e := newLoadEdge(poll)
			e.onSegment = func(s loadSegment) { got = append(got, s) }
			lwFeed(&e, spec, true, poll, c.evs)
			if len(got) != len(c.want) {
				t.Fatalf("报出 %d 段 %+v，want %d 段", len(got), got, len(c.want))
			}
			for i, w := range c.want {
				g := got[i]
				if g != w.seg {
					t.Fatalf("第 %d 段 %+v，want %+v", i, g, w.seg)
				}
				lo, loOK := g.lower()
				up, upOK := g.upper()
				if loOK != w.lower.ok || loOK && lo != msd(w.lower.d) || upOK != w.upper.ok || upOK && up != msd(w.upper.d) {
					t.Fatalf("第 %d 段下界 %v（%v）、上界 %v（%v），want %dms（%v）、%dms（%v）", i, lo, loOK, up, upOK,
						w.lower.d, w.lower.ok, w.upper.d, w.upper.ok)
				}
				if w.actual > 0 && (msd(w.actual) < lo || msd(w.actual) > up) {
					t.Fatalf("第 %d 段真实时长 %dms 不在 [%v, %v] 之内", i, w.actual, lo, up)
				}
			}
		})
	}
}

// 改名证据的 Detail（设计 §6.9）：上一首有采纳的本地文件时一条，成立与不成立两种写法、原因经 i18n.T、带最长间隔；上一首为空不打。
// 变异自证（实测）：上一首为空也打，「上一首为空」红；不成立的原因不经 i18n.T，中文环境下「不成立」红。
func TestLogRenameEvidence(t *testing.T) {
	const (
		okFmt  = "Song changed; keeping the previous local file as a candidate (same playback renamed by QQ Music); longest gap between play-state reads %v"
		badFmt = "Song changed; excluding the previous local file: %s; longest gap between play-state reads %v"
		x      = `E:\Music\上一首.flac`
	)
	logs := captureAssocLog(t)
	logRenameEvidence(x, renameEvidence{ok: true, maxGap: msd(55)})
	if want := fmt.Sprintf(i18n.T(okFmt), msd(55)); countLog(logs(), "+", want) != 1 {
		t.Fatalf("成立：want 一条 %q\n%s", want, logs())
	}
	logRenameEvidence(x, renameEvidence{reason: "sampling gap", maxGap: msd(450)})
	if want := fmt.Sprintf(i18n.T(badFmt), i18n.T("sampling gap"), msd(450)); countLog(logs(), "+", want) != 1 {
		t.Fatalf("不成立：want 一条 %q\n%s", want, logs())
	}
	before := logs()
	logRenameEvidence("", renameEvidence{ok: true, maxGap: msd(50)})
	logRenameEvidence("", renameEvidence{reason: "loading state seen", maxGap: msd(50)})
	if logs() != before {
		t.Fatalf("上一首为空也打了 Detail：%s", strings.TrimPrefix(logs(), before))
	}
}

// 载入段的 Detail（设计 §6.9）：四个时刻按 15:04:05.000、没有 t₀ 写「—」，上下界取到毫秒、不可用写译文，标记按次序经 i18n.T、
// 有才加。
// 变异自证（实测）：标记的次序颠倒、不可用不经 i18n.T，「没有 t₀、四个标记」红；上下界不取到毫秒，「取到毫秒」红。
func TestLogLoadSegment(t *testing.T) {
	const segFmt = "QQ Music loading segment: non-loading %s, loading %s to %s, non-loading %s; at least %s, at most %s%s"
	ts := func(at time.Time) string { return at.Format("15:04:05.000") }
	sub := func(ms float64) time.Time { return lwT0.Add(time.Duration(ms * float64(time.Millisecond))) }
	all := i18n.T("; unreadable play state between the loading samples") + i18n.T("; sampling gap between the loading samples") +
		i18n.T("; unreadable play state next to the segment") + i18n.T("; sampling gap next to the segment")
	for _, c := range []struct {
		name string
		seg  loadSegment
		want string
	}{
		{"完整、无标记", lwSeg(-50, 0, 50, 100),
			fmt.Sprintf(i18n.T(segFmt), ts(lwAt(-50)), ts(lwAt(0)), ts(lwAt(50)), ts(lwAt(100)), "50ms", "150ms", "")},
		{"取到毫秒", loadSegment{pre: sub(-49.6), first: sub(0), last: sub(50.6), post: sub(100.2)},
			fmt.Sprintf(i18n.T(segFmt), ts(sub(-49.6)), ts(sub(0)), ts(sub(50.6)), ts(sub(100.2)), "51ms", "150ms", "")},
		{"没有 t₀、四个标记", loadSegment{first: lwAt(0), last: lwAt(450), post: lwAt(500), fail: true, gap: true, failInside: true, gapInside: true},
			fmt.Sprintf(i18n.T(segFmt), "—", ts(lwAt(0)), ts(lwAt(450)), ts(lwAt(500)), i18n.T("unavailable"), i18n.T("unavailable"), all)},
		{"只有段前后的读失败", withFail(lwSeg(-50, 0, 0, 100)),
			fmt.Sprintf(i18n.T(segFmt), ts(lwAt(-50)), ts(lwAt(0)), ts(lwAt(0)), ts(lwAt(100)), "0s", "150ms", i18n.T("; unreadable play state next to the segment"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureAssocLog(t)
			logLoadSegment(c.seg)
			if countLog(logs(), "+", c.want) != 1 {
				t.Fatalf("want 一条 %q\n%s", c.want, logs())
			}
		})
	}
}

func withFail(s loadSegment) loadSegment { s.fail = true; return s }
