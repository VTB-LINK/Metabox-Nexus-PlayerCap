package qqmusic

// 本文件测取词之后的重读与同一首重开（设计 §6.8「重开」、§8），以及换歌认领时的改名证据（第 5 版，设计 §6.8「改名证据」）：
// 载入值可能只出现在阻塞的取词之后被采用的重读里。
// runSession 要真进程、没有离线夹具，这里照它复述与重开、改名证据相关的几步（reopenReplay.beat）：loadOpen 由
// newLoadEdge(轮询间隔) 建出；整拍元数据读失败即补记一次读不成、结束这一拍；每拍开头算一次 loadUsable、以首读 step（传读数
// 时刻）并按载入支写入；换歌认领时先取上一首采纳的本地文件、求改名证据（claim），经 excludeFor 交 excludeLocal，再 reset 与
// 写入；换歌认领与补取两处取词之后被采用的重读同样交给 loadOpen（传重读时刻）、按载入支写入；补取块编号由缺到有时 gen 加一、
// 不改 excludeLocal；暂扣与暂扣支；每拍至多写入一次；每拍取一条结果。判定函数（loadingUsable、loadEdge 与其窗口、
// reopenGuard、excludeFor、rereadSameSong、switchHold）、歌词状态机、takeResult 与（部分用例里的）读取者都是真的。
// runSession 与这份复述是否一致由 assocwiring_test.go 的源码扫描守形状。

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// reopenRead 是一次元数据读数：歌名、进度与播放状态字段（ok 为假即播放状态读失败）；metaFail 为真即整拍元数据读失败
// （runSession 补记一次读不成后 continue，其余字段不看）。
type reopenRead struct {
	name     string
	ms       uint32
	state    uint32
	ok       bool
	metaFail bool
}

func rr(name string, ms, state uint32) reopenRead {
	return reopenRead{name: name, ms: ms, state: state, ok: true}
}

// reopenFetch 是一拍里的取词（换歌认领且编号已知，或补取）：先调 during（如 QQ 在取词期间从另一入口重开、改写假内存里的
// 记录），耗时 dur 后重读到 reread（nil 即重读失败）；ok 为取词成功，补取成功后取封面耗时 cover。
type reopenFetch struct {
	during func()
	dur    time.Duration
	reread *reopenRead
	ok     bool
	cover  time.Duration
}

// reopenWrite 是一次写入重开：第几拍（自 1 起）、信号是不是载入值（假即暂扣放行）。
type reopenWrite struct {
	beat      int
	byLoading bool
}

type reopenReplay struct {
	r     *assocReader // nil 即没有读取者（Submit 什么也不做，取不到结果）
	spec  playStateSpec
	poll  time.Duration
	bad   bool    // 本会话已判标志不可信（judge.bad）
	ids   songKey // 当前歌曲的编号，认领时写进 key
	beats int

	loadOpen loadEdge
	reopenG  reopenGuard
	hold     switchHold
	lastName string
	lastMem  uint32
	pending  bool // lyricsPending：在线取词还在补取
	key      songKey
	st       lyricState
	shown    []string   // 最近给出的行（原文|翻译）
	shows    [][]string // 每次要求重发时给出的行，即发出的各次歌词
	writes   []reopenWrite
	excludes []string         // 每次写入新 gen（换歌认领、补取块编号由缺到有）时 songKey 的 excludeLocal
	evidence []renameEvidence // 每次换歌认领求得的改名证据
}

func (p *reopenReplay) reopenSong(byLoading bool, progressMs uint32) {
	p.reopenG.noteReopen(progressMs)
	p.key.reopen++
	p.st.reopened(p.key.reopen)
	p.r.Submit(p.key)
	p.writes = append(p.writes, reopenWrite{p.beats, byLoading})
}

func (p *reopenReplay) show(lines []lyricLine, changed bool) {
	if changed {
		p.shown = lineTexts(lines)
		p.shows = append(p.shows, p.shown)
	}
}

// beat 照 runSession 复述一拍：first 为首读，f 非 nil 时这一拍取词。换歌认领且编号已知时取词；补取只在补取进行中、未放行、
// 歌名为已认领的、编号已知（p.ids）且用例给了 f 的拍做（runSession 里补取的节流由 backfillWindow 管，这里由用例给定）。
// loadOpen 在第一拍按此时的轮询间隔建出（同 runSession 在轮询循环之前，用例可在第一拍之前改 p.poll）。
func (p *reopenReplay) beat(first reopenRead, f *reopenFetch) {
	time.Sleep(p.poll)
	p.beats++
	if p.beats == 1 {
		p.loadOpen = newLoadEdge(p.poll)
	}
	if first.metaFail { // 整拍元数据读失败：补记一次读不成，结束这一拍
		p.loadOpen.missed()
		return
	}
	meta := first
	sampleTime := time.Now()

	loadUsable := p.spec.loadingUsable(p.bad, p.poll)
	reopenedThisTick := false
	if edge := p.loadOpen.step(p.spec, meta.state, meta.ok, sampleTime); p.reopenG.shouldReopenByLoading(loadUsable, edge, meta.name, p.lastName, p.key.hasIDs()) && !reopenedThisTick {
		reopenedThisTick = true
		p.reopenSong(true, meta.ms)
	} else if !reopenedThisTick && p.reopenG.shouldReopenByFallback(false, meta.name, p.lastName, p.key.hasIDs()) {
		reopenedThisTick = true
		p.reopenSong(false, meta.ms)
	}
	// fetch 做一次取词与其后的重读，返回重读是否被采用（采用时读数与播放状态换成重读的）。
	fetch := func() bool {
		if f.during != nil {
			f.during()
		}
		time.Sleep(f.dur)
		var fresh *SongMetadata
		var rerr error = errFakeNoAccess
		if f.reread != nil {
			fresh, rerr = &SongMetadata{Name: f.reread.name, ProgressMs: f.reread.ms, PlayState: f.reread.state, PlayStateOK: f.reread.ok}, nil
		}
		if !rereadSameSong(fresh, rerr, p.lastName, p.lastMem) {
			return false
		}
		sampleTime = time.Now()
		meta.ms, meta.state, meta.ok = fresh.ProgressMs, fresh.PlayState, fresh.PlayStateOK
		return true
	}

	if meta.name != p.lastName && meta.name != "" && !strings.HasPrefix(meta.name, "QQ音乐") {
		p.hold.drop(sampleTime)
		p.lastName, p.lastMem = meta.name, meta.ms
		p.reopenG.reset()
		// 同 runSession 的换歌分支：上一首采纳的本地文件在 reset 之前取，改名证据成立时交空（第 5 版）
		prevLocal := p.st.localPath()
		renamed := p.loadOpen.claim(loadUsable, p.spec, p.poll)
		p.evidence = append(p.evidence, renamed)
		p.key = songKey{gen: p.key.gen + 1, songID: p.ids.songID, songMid: p.ids.songMid, name: meta.name, excludeLocal: excludeFor(prevLocal, renamed)}
		p.excludes = append(p.excludes, p.key.excludeLocal)
		p.st.reset(p.key.gen, false, time.Time{})
		p.r.Submit(p.key)
		p.pending = true
		if p.key.hasIDs() && f != nil {
			if fetch() {
				if edge := p.loadOpen.step(p.spec, meta.state, meta.ok, sampleTime); p.reopenG.shouldReopenByLoading(loadUsable, edge, meta.name, p.lastName, p.key.hasIDs()) && !reopenedThisTick {
					reopenedThisTick = true
					p.reopenSong(true, meta.ms)
				}
			}
			if f.ok {
				// 同 runSession 的换歌拍：先按已接受的结果折算，取到本代次有变化的结果即换成它，合并后照现状发一次。
				p.pending = false
				p.st.setOnlineRaw(shiftFixture())
				lines, _ := p.st.refresh(p.key.hasIDs(), stateOffsetSec)
				if l, changed := takeResult(&p.st, p.r.Results(), p.key.hasIDs(), stateOffsetSec); changed {
					lines = l
				}
				p.show(lines, true)
			}
		} else {
			p.st.refresh(p.key.hasIDs(), stateOffsetSec)
		}
		f = nil
	}

	held, released := p.hold.step(p.lastMem, meta.ms, sampleTime)
	if held {
		return
	}
	if p.pending && !released && meta.name == p.lastName && f != nil && p.ids.hasIDs() {
		if !p.key.hasIDs() {
			// 同 runSession 的补取块：编号由缺到有时 gen 加一、reopen 归零、reset 并写入，excludeLocal 沿用认领时的，不求改名证据
			p.key.gen++
			p.key.reopen = 0
			p.key.songID, p.key.songMid = p.ids.songID, p.ids.songMid
			p.st.reset(p.key.gen, false, time.Time{})
			p.r.Submit(p.key)
			p.excludes = append(p.excludes, p.key.excludeLocal)
		}
		if fetch() {
			if edge := p.loadOpen.step(p.spec, meta.state, meta.ok, sampleTime); p.reopenG.shouldReopenByLoading(loadUsable, edge, meta.name, p.lastName, p.key.hasIDs()) && !reopenedThisTick {
				reopenedThisTick = true
				p.reopenSong(true, meta.ms)
			}
		}
		if f.ok {
			p.pending = false
			p.st.setOnlineRaw(shiftFixture())
			p.show(p.st.refresh(p.key.hasIDs(), stateOffsetSec))
			p.show(takeResult(&p.st, p.r.Results(), p.key.hasIDs(), stateOffsetSec))
			time.Sleep(f.cover)
		}
	}
	if meta.ms != p.lastMem {
		seekDetected := released && meta.ms < p.lastMem
		if p.lastMem > 0 && seekBack(p.lastMem, meta.ms) {
			seekDetected = true
		}
		if seekDetected {
			if p.reopenG.shouldReopenByFallback(released && meta.ms < p.lastMem, meta.name, p.lastName, p.key.hasIDs()) && !reopenedThisTick {
				reopenedThisTick = true
				p.reopenSong(false, meta.ms)
			}
		}
		p.lastMem = meta.ms
	}
	p.reopenG.stepProgress(meta.ms, held, p.loadOpen.loading)
	if p.lastName != "" {
		p.show(takeResult(&p.st, p.r.Results(), p.key.hasIDs(), stateOffsetSec))
	}
}

func newReopenReplay(r *assocReader) *reopenReplay {
	return &reopenReplay{r: r, spec: knownVersions["22.82"].PlayState, poll: 50 * time.Millisecond,
		ids: songKey{songID: 100, songMid: "003M"}}
}

func wantWrites(t *testing.T, p *reopenReplay, want ...reopenWrite) {
	t.Helper()
	if !slices.Equal(p.writes, want) {
		t.Fatalf("写入重开 %+v，want %+v", p.writes, want)
	}
	if p.key.reopen != uint64(len(want)) {
		t.Fatalf("会话的 reopen %d，want %d", p.key.reopen, len(want))
	}
}

// 终审反例：22.82、轮询 50 ms、编号已知，读取者已按 mid 键记录认出并显示关联 A，在线取词还在补取。某拍首读为在播值 3、
// 进度 800 ms，随即补取；取词期间 QQ 从专辑页重开同一首（记录改为 id 键），取词返回后重读到载入值 2、进度 0，被采用（回退
// 不足 2 秒）；取封面耗时 500 ms，长过载入的持续时间，下一拍读到 3。重读里的载入值由无到有即写入一次重开，读取者重读记录，
// 最终显示关联 B。本代次第 1 秒的复扫（1.05 s）落在取词期间、同样认出 id 键，不重开也会换上 B，所以重开与否靠写入的断言区分。
// 变异自证（实测，改的是本复述）：补取重读处不交给 loadOpen（即修复之前的接线），只见到 3、3，不写重开而红。
func TestReopenSeenInFillReread(t *testing.T) {
	e := reopenEnv(midURL("003M"))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		p := newReopenReplay(r)
		p.beat(rr("百", 0, 3), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(rr("百", 100, 3))}) // 认领，在线取词失败
		for ms := uint32(150); ms <= 750; ms += 50 {
			p.beat(rr("百", ms, 3), nil)
		}
		if !slices.Equal(p.shown, assocLines) {
			t.Fatalf("重开之前显示 %q，want 关联 A", p.shown)
		}
		p.beat(rr("百", 800, 3), &reopenFetch{during: func() { reopenRecord(e, idURL(100)) }, dur: 300 * time.Millisecond,
			reread: ptr(rr("百", 0, 2)), ok: true, cover: 500 * time.Millisecond})
		p.beat(rr("百", 900, 3), nil)
		p.beat(rr("百", 950, 3), nil)
		wantWrites(t, p, reopenWrite{15, true})
		if !slices.Equal(p.shown, assocLines2) {
			t.Fatalf("重开之后显示 %q，want 关联 B", p.shown)
		}
		// 认领那一拍（50 ms）、本代次第 1 秒的复扫（1.05 s）与重读写入重开之时（850 ms 起取词 300 ms）各扫一次
		wantTimes(t, "扫描", e.mem.scanTimes(), []time.Duration{50 * time.Millisecond, 1050 * time.Millisecond, 1150 * time.Millisecond})
	})
}

func ptr[T any](v T) *T { return &v }

// 终审第 2 轮的反例：认领拍首读为新歌、进度 100 ms、在播值 3，认领时已写入读取者。阻塞的取词期间读取者先按 mid 键记录认出
// 关联 A，随后 QQ 从另一入口重开同一首（记录改为 id 键，关联 B）；取词返回后重读同名、进度 0、载入值 2，下一拍仍为 2，之后为 3。
// 认领重读里的载入值由无到有即写一次重开，读取者再扫一轮，最终显示 B；（1，0）的 A 在认领拍被拒、一次也不显示。
// 变异自证（实测，改的是本复述）：换歌重读处只交给 loadOpen、不写（第 1 轮的接线），reopen 一直为 0、显示 A 而红。
func TestReopenSeenInClaimReread(t *testing.T) {
	e := reopenEnv(midURL("003M"))
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		p := newReopenReplay(r)
		p.beat(rr("百", 100, 3), &reopenFetch{during: func() {
			synctest.Wait() // 读取者处理认领时的写入：扫描、认出 A、发出（1，0）的结果
			wantTimes(t, "认领时的扫描", e.mem.scanTimes(), []time.Duration{50 * time.Millisecond})
			reopenRecord(e, idURL(100))
		}, dur: 200 * time.Millisecond, reread: ptr(rr("百", 0, 2)), ok: true})
		p.beat(rr("百", 50, 2), nil)
		p.beat(rr("百", 100, 3), nil)
		p.beat(rr("百", 150, 3), nil)
		wantWrites(t, p, reopenWrite{1, true})
		if !slices.Equal(p.shown, assocLines2) {
			t.Fatalf("显示 %q，want 关联 B", p.shown)
		}
		for _, s := range p.shows {
			if slices.Equal(s, assocLines) {
				t.Fatalf("显示过关联 A：%q", p.shows)
			}
		}
		wantTimes(t, "扫描", e.mem.scanTimes(), []time.Duration{50 * time.Millisecond, 250 * time.Millisecond})
	})
}

// 换歌认领取词之后的重读：首读不是载入值、重读才见到，即认领之后开始了一次新的载入，按由无到有写一次重开，下一拍仍为载入值
// 不再写，gen 仍为 1。反例：首读已是载入值、重读仍为载入值（step 已记下首读），不写；重读与其后各拍都是在播值，不写；首读为
// 载入值而重读被拒（歌名不同），不看重读，下一拍仍为载入值也不写。
// 变异自证（实测，改的是本复述）：换歌重读处只交给 loadOpen、不写，第一例不写而红；换歌重读处不交给 loadOpen，第一例到下一拍
// 才写、拍号不符而红。
func TestReopenOnClaimReread(t *testing.T) {
	for _, c := range []struct {
		name   string
		first  reopenRead
		reread reopenRead
		then   uint32 // 其后两拍的播放状态（之后一拍为 3）
		want   []reopenWrite
	}{
		{"首读为在播值、重读为载入值", rr("百", 0, 3), rr("百", 50, 2), 2, []reopenWrite{{1, true}}},
		{"首读即为载入值、重读仍为载入值", rr("百", 0, 2), rr("百", 50, 2), 2, nil},
		{"重读与其后都是在播值", rr("百", 0, 3), rr("百", 50, 3), 3, nil},
		{"首读为载入值、重读被拒（歌名不同）", rr("百", 0, 2), rr("千", 50, 2), 2, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newReopenReplay(nil)
				p.beat(c.first, &reopenFetch{dur: 200 * time.Millisecond, reread: ptr(c.reread), ok: true})
				p.beat(rr("百", 100, c.then), nil)
				p.beat(rr("百", 150, c.then), nil)
				p.beat(rr("百", 200, 3), nil)
				wantWrites(t, p, c.want...)
				if p.key.gen != 1 {
					t.Fatalf("gen %d，want 1", p.key.gen)
				}
			})
		})
	}
}

// QQ 先换歌名、后开始载入（新歌自己的载入只出现在认领重读里）时，新歌在认领拍多写一次重开：读取者多扫一轮，与重读为在播值
// （不写）的同一首相比，最终显示相同（关联 A）。只断言最终显示与扫描次数：多写一次时此前的关联结果因代次不符被拒，认领拍先发
// 在线歌词、关联歌词晚这一轮出现，两者发出的中间序列不同。
// 变异自证（实测，改的是本复述）：重开不写入读取者，多写一次的那一例最终只显示在线歌词而红。
func TestClaimRereadExtraReopenSameResult(t *testing.T) {
	run := func(t *testing.T, state uint32) (*reopenReplay, []time.Duration) {
		var p *reopenReplay
		var scans []time.Duration
		e := reopenEnv(midURL("003M"))
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			p = newReopenReplay(r)
			p.beat(rr("百", 100, 3), &reopenFetch{dur: 200 * time.Millisecond, reread: ptr(rr("百", 0, state)), ok: true})
			for ms := uint32(50); ms <= 500; ms += 50 {
				p.beat(rr("百", ms, 3), nil)
			}
			scans = e.mem.scanTimes()
		})
		return p, scans
	}
	extra, extraScans := run(t, 2)
	plain, plainScans := run(t, 3)
	wantWrites(t, extra, reopenWrite{1, true})
	wantWrites(t, plain)
	for _, c := range []struct {
		name string
		p    *reopenReplay
	}{{"多写一次重开", extra}, {"不写重开", plain}} {
		if !slices.Equal(c.p.shown, assocLines) {
			t.Fatalf("%s：最终显示 %q，want 关联 A", c.name, c.p.shown)
		}
	}
	if len(extraScans) != 2 || len(plainScans) != 1 {
		t.Fatalf("扫描次数：多写一次的 %v，不写的 %v；want 2 次与 1 次", extraScans, plainScans)
	}
}

// 每拍至多写入一次：拍首已因载入值写过重开，补取之后的重读仍是载入值，不再写；重读为在播值、下一拍又是在播值也不再写。
// 拍首写过之后重读不可能再见到由无到有（拍首的 step 已记下载入值），所以「本拍一次」的约束在这里只是兜底：本复述去掉它，
// 本例仍绿（实测）；runSession 里去掉它由 assocwiring_test.go 的门禁报出。
func TestReopenAtMostOncePerBeat(t *testing.T) {
	for _, c := range []struct {
		name   string
		reread reopenRead
	}{
		{"重读仍是载入值", rr("百", 850, 2)},
		{"重读为在播值", rr("百", 850, 3)},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newReopenReplay(nil)
				p.beat(rr("百", 0, 3), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(rr("百", 100, 3))})
				p.beat(rr("百", 800, 2), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(c.reread)})
				p.beat(rr("百", 900, 3), nil)
				wantWrites(t, p, reopenWrite{2, true})
			})
		})
	}
}

// 补取之后的重读被拒（歌名不同；回退超过 2 秒而落在开头 1 秒内，会触发暂扣），重读里的载入值不交给 loadOpen：这一拍不写
// 重开，下一拍首读为载入值时才按由无到有写入（若交给了 loadOpen，下一拍就不是由无到有）。
// 变异自证（实测，改的是本复述）：重读不经 rereadSameSong、一律采用，两例都在重读那一拍写了重开而红。
func TestReopenIgnoresRejectedFillReread(t *testing.T) {
	for _, c := range []struct {
		name   string
		before uint32
		reread reopenRead
		next   reopenRead
	}{
		{"歌名不同", 800, rr("千", 50, 2), rr("百", 850, 2)},
		{"回退超过 2 秒、落在开头", 30000, rr("百", 500, 2), rr("百", 500, 2)},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newReopenReplay(nil)
				p.beat(rr("百", 0, 3), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(rr("百", 100, 3))})
				p.beat(rr("百", c.before, 3), nil)
				p.beat(rr("百", c.before, 3), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(c.reread)})
				wantWrites(t, p)
				p.beat(c.next, nil)
				wantWrites(t, p, reopenWrite{4, true})
			})
		})
	}
}

// 认领拍编号已知、关联表另有本地条目 F 且被 QQ 打开（设计 §6.5 第 6 步「显示的转移与事件」，第 5 版）：读取者（真的，配占用
// 查询桩）在取词返回之前给出本地文件的结果 → 换歌拍直接发本地歌词，发出的只有本地那一份；占用查询在取词期间阻塞、取词返回之后
// 才给出 → 换歌拍先发在线，下一拍换成本地。两例都不读当前歌曲记录（第 0 秒的认定先于读记录）。
func TestClaimTakesLocalResultWithIDs(t *testing.T) {
	setup := func(e *readerEnv) {
		e.fs.put(testTablePath, tableDat(append(slices.Clone(reopenTables), widItemF)...))
		e.fs.put(widF, []byte("audio"))
		e.fs.put(widLyrF, []byte(widLRC))
	}
	t.Run("取词返回之前给出", func(t *testing.T) {
		e := reopenEnv(midURL("003M"))
		setup(e)
		e.rm.fn = openBy([]string{widF}, testQQPID)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			p := newReopenReplay(r)
			p.beat(rr("百", 0, 3), &reopenFetch{dur: 200 * time.Millisecond, reread: ptr(rr("百", 50, 3)), ok: true})
			for ms := uint32(100); ms <= 500; ms += 50 {
				p.beat(rr("百", ms, 3), nil)
			}
			if len(p.shows) != 1 || !slices.Equal(p.shows[0], widLines) {
				t.Fatalf("发出的歌词 %q，want 只有本地那一份", p.shows)
			}
			wantTimes(t, "扫描", e.mem.scanTimes(), nil)
		})
	})
	t.Run("取词返回之后给出", func(t *testing.T) {
		e := reopenEnv(midURL("003M"))
		setup(e)
		var g gate
		e.beforeStart = g.arm
		e.rm.fn = func(paths []string) ([]uint32, error) {
			g.wait()
			return openBy([]string{widF}, testQQPID)(paths)
		}
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			p := newReopenReplay(r)
			p.beat(rr("百", 0, 3), &reopenFetch{dur: 200 * time.Millisecond, reread: ptr(rr("百", 50, 3)), ok: true})
			if len(p.shows) != 1 || !slices.Equal(p.shows[0], onlineTexts) {
				t.Fatalf("换歌拍发出的歌词 %q，want 在线", p.shows)
			}
			g.release()
			p.beat(rr("百", 100, 3), nil)
			if len(p.shows) != 2 || !slices.Equal(p.shows[1], widLines) {
				t.Fatalf("下一拍发出的歌词 %q，want 换成本地", p.shows)
			}
			for ms := uint32(150); ms <= 500; ms += 50 {
				p.beat(rr("百", ms, 3), nil)
			}
			if len(p.shows) != 2 {
				t.Fatalf("发出的歌词 %q，want 只有在线与本地两次", p.shows)
			}
			wantTimes(t, "扫描", e.mem.scanTimes(), nil)
		})
	})
}

// 载入信号不可用（本会话已判标志不可信；实际轮询超过 1 秒）：补取之后的重读见到载入值也不写重开；随后进度回到开头、暂扣
// 放行按回跳补发时由暂扣支写入一次。
// 变异自证（实测，改的是本复述）：补取重读的载入支去掉 loadUsable，两例在重读那一拍多写一次而红。
func TestReopenFillRereadUnusable(t *testing.T) {
	for _, c := range []struct {
		name string
		bad  bool
		poll time.Duration
	}{
		{"已判不可信", true, 50 * time.Millisecond},
		{"轮询超过 1 秒", false, 1500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newReopenReplay(nil)
				p.bad, p.poll = c.bad, c.poll
				p.beat(rr("百", 0, 3), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(rr("百", 100, 3))})
				p.beat(rr("百", 30000, 3), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(rr("百", 30100, 2))})
				wantWrites(t, p)
				for range 12 {
					p.beat(rr("百", 0, 3), nil)
				}
				if len(p.writes) != 1 || p.writes[0].byLoading {
					t.Fatalf("写入重开 %+v，want 暂扣支写入一次", p.writes)
				}
			})
		})
	}
}

// adoptLocal 以本代次来源为本地文件 path 的结果经状态机造出「上一首采纳过的本地文件」。
func adoptLocal(t *testing.T, p *reopenReplay, path string) {
	t.Helper()
	p.st.apply(assocResult{gen: p.key.gen, reopen: p.key.reopen, kind: resDesired, source: srcLocal, localPath: path}, p.key.hasIDs(), stateOffsetSec)
	if p.st.localPath() != path {
		t.Fatalf("采纳的本地文件 %q，want %q —— 夹具失效", p.st.localPath(), path)
	}
}

// renameFirstPlay 复述 11-24 的首次放（设计 §2.5、§6.5 第 5 步）：认领文件名（编号缺位）的那一拍读到 2，随即造出采纳的本地文件
// X（widF）；第 2～15 拍歌名不变、读到 3（mid 可改写其中的读数）；第 16 拍（约 0.75 秒后）歌名变为匹配名，QQ 改名，这一拍读到
// renameState；随后编号落位，第 17 拍补取块写入新 gen 并取词。
func renameFirstPlay(t *testing.T, p *reopenReplay, renameState uint32, mid func(beat int, r reopenRead) reopenRead) {
	t.Helper()
	p.ids = songKey{}
	p.beat(rr(widStem, 0, 2), nil)
	adoptLocal(t, p, widF)
	for i := 2; i <= 15; i++ {
		r := rr(widStem, uint32(i-1)*50, 3)
		if mid != nil {
			r = mid(i, r)
		}
		p.beat(r, nil)
	}
	p.beat(rr(widMatch, 750, renameState), nil)
	p.ids = songKey{songID: widID}
	p.beat(rr(widMatch, 800, 3), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(rr(widMatch, 900, 3)), ok: true})
}

// 换歌认领时的改名证据（设计 §6.5 第 5 步、§6.8「改名证据」、§7「会话」的「改名证据」），照 runSession 复述：真的歌词状态机与
// loadEdge，记下每次写入新 gen 的 excludeLocal（认领、补取块编号由缺到有）。各例断言写入的 excludeLocal 序列与改名那次认领的原因。
//   - 现行版本表（22.82，G 为 0）：11-24 首次放的序列 → 改名那次交 X、编号落位之后仍为 X，原因为门槛为 0，与第 4 版逐事件相同。
//   - 注入 G = 100 ms 的同一序列 → 改名那次交空、编号落位之后仍为空。
//   - 注入 G 而没有证据，交 X：改名那一拍读到 2；其间有一拍整拍元数据读失败；轮询 500 ms；补取的取词阻塞 400 ms、其间真实
//     换到别的歌（状态 3→2→3 都落在取词期间），取词之后的重读因歌名不同被拒，下一拍首读即为新歌（第 5 版第 1 轮设计审的反例）。
//   - 真实换歌（反例，真读取者配占用查询桩）：上一首为本地文件 X，换到在线歌曲 Y，换歌那一拍读到 2 → 交 X；QQ 仍开着 X，读取者
//     不认定它，Y 为在线。
//
// 变异自证（实测）：claim 不看 sawGap，「补取的取词阻塞 400 ms」红；claim 不比轮询间隔与门槛，「轮询 500 ms」红；excludeFor 反过来，
// 各例红；给 22.82 填 100 ms，「现行版本表」红。
func TestRenameEvidenceReplay(t *testing.T) {
	const (
		zeroGate = "no loading sample threshold for this QQ Music version"
		slowPoll = "poll interval not shorter than the loading sample threshold"
		edgeSeen = "loading state seen"
		unread   = "unreadable play state"
		gapSeen  = "sampling gap"
	)
	check := func(t *testing.T, p *reopenReplay, excludes []string, ok bool, reason string) {
		t.Helper()
		if !slices.Equal(p.excludes, excludes) {
			t.Fatalf("写入的 excludeLocal %q，want %q", p.excludes, excludes)
		}
		if len(p.evidence) < 2 || p.evidence[1].ok != ok || p.evidence[1].reason != reason {
			t.Fatalf("改名证据 %+v，want 第二次认领 ok %v、原因 %q", p.evidence, ok, reason)
		}
	}
	for _, c := range []struct {
		name     string
		spec     playStateSpec
		poll     time.Duration
		state    uint32 // 改名那一拍的播放状态
		mid      func(int, reopenRead) reopenRead
		excludes []string
		ok       bool
		reason   string
	}{
		{"现行版本表（G 为 0）", knownVersions["22.82"].PlayState, 0, 3, nil, []string{"", widF, widF}, false, zeroGate},
		{"注入 G、有证据", spec100(), 0, 3, nil, []string{"", "", ""}, true, ""},
		{"注入 G、改名那一拍读到 2", spec100(), 0, 2, nil, []string{"", widF, widF}, false, edgeSeen},
		{"注入 G、其间有一拍整拍元数据读失败", spec100(), 0, 3, func(i int, r reopenRead) reopenRead {
			if i == 8 {
				return reopenRead{metaFail: true}
			}
			return r
		}, []string{"", widF, widF}, false, unread},
		{"注入 G、轮询 500 ms", spec100(), 500 * time.Millisecond, 3, nil, []string{"", widF, widF}, false, slowPoll},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newReopenReplay(nil)
				p.spec = c.spec
				if c.poll > 0 {
					p.poll = c.poll
				}
				renameFirstPlay(t, p, c.state, c.mid)
				if p.key.gen != 3 || p.key.songID != widID {
					t.Fatalf("会话的 gen %d、songID %d，want 3、%d（认领、改名、编号落位）", p.key.gen, p.key.songID, widID)
				}
				check(t, p, c.excludes, c.ok, c.reason)
			})
		})
	}

	t.Run("注入 G、补取的取词阻塞 400 ms（第 5 版第 1 轮设计审）", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newReopenReplay(nil)
			p.spec = spec100()
			p.beat(rr("甲", 0, 2), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(rr("甲", 100, 3))}) // 认领 X，在线取词失败
			adoptLocal(t, p, widF)
			for ms := uint32(150); ms <= 350; ms += 50 {
				p.beat(rr("甲", ms, 3), nil)
			}
			p.beat(rr("甲", 400, 3), &reopenFetch{dur: 400 * time.Millisecond, reread: ptr(rr("乙", 0, 3)), ok: true})
			p.beat(rr("乙", 50, 3), nil)
			check(t, p, []string{"", widF}, false, gapSeen)
			if p.evidence[1].maxGap != 450*time.Millisecond {
				t.Fatalf("最长读数间隔 %v，want 450ms", p.evidence[1].maxGap)
			}
		})
	})

	t.Run("真实换歌（反例）", func(t *testing.T) {
		e := widEnv(widItemF) // 表里只有 X 的本地条目，Y 没有条目
		e.rm.fn = openBy([]string{widF}, testQQPID)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			p := newReopenReplay(r)
			p.spec = spec100()
			p.ids = songKey{}
			for ms := uint32(0); ms <= 400; ms += 50 {
				p.beat(rr(widStem, ms, 3), nil) // 本地文件 X，编号缺位，读取者第 0 秒认定 X
			}
			if p.st.localPath() != widF {
				t.Fatalf("上一首采纳的本地文件 %q，want %q", p.st.localPath(), widF)
			}
			p.ids = songKey{songID: widID}
			p.beat(rr(widMatch, 0, 2), &reopenFetch{dur: 100 * time.Millisecond, reread: ptr(rr(widMatch, 100, 2)), ok: true}) // 换到在线歌曲 Y
			for ms := uint32(150); ms <= 600; ms += 50 {
				p.beat(rr(widMatch, ms, 3), nil)
			}
			check(t, p, []string{"", widF}, false, edgeSeen)
			if p.st.source() != srcOnline || !slices.Equal(p.shown, onlineTexts) {
				t.Fatalf("Y 的来源 %v、显示 %q，want 在线 %q", p.st.source(), p.shown, onlineTexts)
			}
		})
	})
}
