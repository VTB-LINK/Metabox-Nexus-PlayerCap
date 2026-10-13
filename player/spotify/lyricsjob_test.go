package spotify

// 本文件测异步取词（issue #107）：代次与在飞状态（lyricsJobs）、补取与在飞的先后（retryDue）、
// 取词结果怎么落地（settleLyrics）、取词 goroutine 本身（runLyricsJob / fetchLyrics：各步之间作废即退出、
// 送结果不挂住、CLI 失败的降级、三层取词的优先级）。CLI 与 Go 直取经包级 seam 换成桩，CDP 会话换成
// fakeLyricsClient，不碰 Spotify、不联网。runSession 里的调用点（换歌时 start、每拍开头收结果、会话结束 stop）
// 仍靠人守（AGENTS §4）。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/spotify/lyric"
	"Metabox-Nexus-PlayerCap/player/spotify/watchdog"
)

// ── lyricsJobs ──

// 变异自证（均实测）：accept 不比代次 → 「旧代次」红；stop 不推进代次 → 「每次 start 都应换代次」红；
// start 不先 stop（旧的不取消、代次只在 start 里前进）→ 「换歌时旧的 ctx 应被取消」红；accept 认下后不清在飞 →
// 「认下之后不应再算在飞」红；stop 不取消 ctx → 「换歌时旧的 ctx 应被取消」红。等价变异一条，不红：代次的前进从
// stop 挪到 start（accept 另要求在飞，会话结束之后送进来的结果照样不认）。
func TestLyricsJobs(t *testing.T) {
	var j lyricsJobs
	if j.cancel != nil {
		t.Fatal("零值不应有在飞")
	}
	if j.accept(0) {
		t.Fatal("零值：没有在飞时不认任何结果（含代次 0）")
	}

	ctxA, genA := j.start()
	if j.cancel == nil {
		t.Fatal("start 之后应有在飞")
	}
	if j.accept(genA - 1) {
		t.Fatal("旧代次的结果不应被认下")
	}
	if !j.accept(genA) {
		t.Fatal("当前代次的结果应被认下")
	}
	if j.cancel != nil {
		t.Fatal("认下之后不应再算在飞")
	}
	if j.accept(genA) {
		t.Fatal("重复的结果不应被认下第二次")
	}
	if ctxA.Err() == nil {
		t.Fatal("认下之后应释放它的 ctx")
	}

	// 换歌：起新的一次即作废在飞的旧的一次。
	ctxB, genB := j.start()
	ctxC, genC := j.start()
	if ctxB.Err() == nil {
		t.Fatal("换歌时旧的 ctx 应被取消")
	}
	if ctxC.Err() != nil {
		t.Fatal("新的 ctx 不应被取消")
	}
	if genB == genC || genA == genB {
		t.Fatalf("每次 start 都应换代次，得到 %d %d %d", genA, genB, genC)
	}
	if j.accept(genB) {
		t.Fatal("已作废的那次的结果不应被认下")
	}

	// 会话结束：stop 作废在飞的，之后送进来的结果（代次 C）也不认；重复 stop 无害。
	j.stop()
	j.stop()
	if ctxC.Err() == nil {
		t.Fatal("stop 应取消在飞的 ctx")
	}
	if j.cancel != nil {
		t.Fatal("stop 之后不应有在飞")
	}
	if j.accept(genC) {
		t.Fatal("会话结束之后送进来的结果不应被认下")
	}

	// 换歌连按多次：只认最后一次。
	var gens []uint64
	var ctxs []context.Context
	for i := 0; i < 5; i++ {
		ctx, g := j.start()
		gens = append(gens, g)
		ctxs = append(ctxs, ctx)
	}
	for i, g := range gens[:4] {
		if ctxs[i].Err() == nil {
			t.Errorf("第 %d 次的 ctx 应已被取消", i+1)
		}
		if j.accept(g) {
			t.Errorf("第 %d 次（非最后一次）的结果不应被认下", i+1)
		}
	}
	if !j.accept(gens[4]) {
		t.Fatal("最后一次的结果应被认下")
	}
}

// 补取与在飞：在飞时不起补取，也不把这一拍的补取记掉——上一次落地后下一拍就补，不必再等一整个间隔。
// 变异自证：retryDue 先问 due、再看在飞 → 「上一次落地后，已到期的补取应在这一拍就起」红（实测）。播放态的那一半见
// session_test.go 的 TestRetryDuePaused。
func TestRetryDueWaitsForInFlight(t *testing.T) {
	var j lyricsJobs
	var r lyricsRetry
	r.start("a", t0)

	_, gen := j.start() // 上一次（例如换歌那次的取词）还在飞
	if j.retryDue(&r, true, t0.Add(5*time.Second)) {
		t.Fatal("在飞时不应起补取")
	}
	if j.retryDue(&r, true, t0.Add(6*time.Second)) {
		t.Fatal("在飞时不应起补取")
	}
	if !j.accept(gen) {
		t.Fatal("前置条件：应认下在飞的那次")
	}
	if !j.retryDue(&r, true, t0.Add(6200*time.Millisecond)) {
		t.Fatal("上一次落地后，已到期的补取应在这一拍就起")
	}
	_, gen = j.start()
	if j.retryDue(&r, true, t0.Add(20*time.Second)) {
		t.Fatal("补取自己在飞时不应再起一次")
	}
	j.accept(gen)
	if j.retryDue(&r, true, t0.Add(10*time.Second)) {
		t.Fatal("节奏照旧：上一次补取起于 +6.2s，+10s 不应再补")
	}
	if !j.retryDue(&r, true, t0.Add(11200*time.Millisecond)) {
		t.Fatal("节奏照旧：上一次补取起于 +6.2s，+11.2s 应补")
	}

	// 不在等歌词（没 start 过或已 stop）：不在飞也不补。
	var idle lyricsRetry
	if j.retryDue(&idle, true, t0.Add(time.Hour)) {
		t.Fatal("不在等歌词时不应补取")
	}
}

// ── settleLyrics ──

func mustParse(t *testing.T, body string) *lyric.Response {
	t.Helper()
	resp, err := lyric.ParseBody([]byte(body))
	if err != nil {
		t.Fatalf("夹具解析失败: %v", err)
	}
	return resp
}

const (
	syncedBody   = `{"lyrics":{"syncType":"LINE_SYNCED","provider":"MusixMatch","lines":[{"startTimeMs":"1000","words":"第一行"},{"startTimeMs":"2500","words":"第二行"}]}}`
	unsyncedBody = `{"lyrics":{"syncType":"UNSYNCED","lines":[{"startTimeMs":"0","words":"第一行"},{"startTimeMs":"0","words":"第二行"}]}}`
	hintBody     = `{"lyrics":{"syncType":"LINE_SYNCED","lines":[{"startTimeMs":"0","words":"纯音乐，请欣赏"}]}}`
	twoLineHint  = `{"lyrics":{"syncType":"LINE_SYNCED","lines":[{"startTimeMs":"0","words":"纯音乐，请欣赏"},{"startTimeMs":"3000","words":"第二行"}]}}`
	zeroBody     = `{"lyrics":{"syncType":"LINE_SYNCED","lines":[]}}`
)

// 变异自证：去掉 IsPureMusicOnly 的判断 → 两条「提示语」红；换歌那次不看 trackID 就开始补取 → 「没有 URI」红；
// 补取没拿到时不看 resp 一律停止 → 「补取仍失败」红；拿到歌词时不停止补取 → 两条「拿到」与「两行中的一行」红；
// 换歌那次不看 resp、有 URI 就补取（改回旧写法）→ 换歌那次「提示语」「UNSYNCED」「零行」三例红（均实测）。
// 不看 notFound（issue #111 之前的写法）→ 「换歌那次 404」「补取时 404」红；没取到响应的失败也当作已答复（加上 `r.diag != ""`）
// → 两条 401 与两条 403 红（均实测）。
func TestSettleLyrics(t *testing.T) {
	synced := mustParse(t, syncedBody)
	unsynced := mustParse(t, unsyncedBody)
	hint := mustParse(t, hintBody)
	hint2 := mustParse(t, twoLineHint)
	zero := mustParse(t, zeroBody)

	cases := []struct {
		name                string
		r                   lyricsResult
		wantLines           int
		wantHint            string
		wantStart, wantStop bool
	}{
		{"换歌那次拿到", lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, lines: lyric.Parse(synced), resp: synced}, 2, "", false, true},
		{"补取拿到", lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, lines: lyric.Parse(synced), resp: synced}, 2, "", false, true},
		{"换歌那次只有提示语：按无歌词处理，平台已答复、不补取", lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, lines: lyric.Parse(hint), resp: hint}, 0, "纯音乐，请欣赏", false, false},
		{"补取时只有提示语：停止补取", lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, lines: lyric.Parse(hint), resp: hint}, 0, "纯音乐，请欣赏", false, true},
		{"提示语只是两行中的一行：照常是歌词", lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, lines: lyric.Parse(hint2), resp: hint2}, 2, "", false, true},
		{"换歌那次 UNSYNCED：平台已答复、不补取", lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, resp: unsynced, diag: "x"}, 0, "", false, false},
		{"换歌那次零行：平台已答复、不补取", lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, resp: zero, diag: "x"}, 0, "", false, false},
		{"补取时 UNSYNCED：停止补取", lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, resp: unsynced, diag: "x"}, 0, "", false, true},
		{"换歌那次请求失败（没取到响应）、有 URI：开始补取", lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, diag: "HTTP 403"}, 0, "", true, false},
		{"换歌那次没有 URI（CLI 缺失或失败）：不补取", lyricsResult{lyricsJob: lyricsJob{}}, 0, "", false, false},
		{"补取仍失败（没取到响应）：照节奏继续", lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, diag: "HTTP 403"}, 0, "", false, false},
		// 平台答复 404（notFound）与「取到响应却没有行级歌词」同等处理。
		{"换歌那次 404：平台已答复、不补取", lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, diag: "HTTP 404", notFound: true}, 0, "", false, false},
		{"补取时 404：停止补取", lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, diag: "HTTP 404", notFound: true}, 0, "", false, true},
		{"换歌那次 401：没取到响应、有 URI，开始补取", lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, diag: "HTTP 401"}, 0, "", true, false},
		{"补取时 401：照节奏继续", lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, diag: "HTTP 401"}, 0, "", false, false},
		{"换歌那次 404 且没有 URI：不补取", lyricsResult{lyricsJob: lyricsJob{}, notFound: true}, 0, "", false, false},
		{"补取时 404 而又带着歌词（不应出现）：按拿到处理", lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, lines: lyric.Parse(synced), resp: synced, notFound: true}, 2, "", false, true},
	}
	for _, c := range cases {
		s := settleLyrics(c.r)
		if len(s.lines) != c.wantLines || s.hint != c.wantHint || s.startRetry != c.wantStart || s.stopRetry != c.wantStop {
			t.Errorf("%s：得到 lines=%d hint=%q start=%v stop=%v，期望 lines=%d hint=%q start=%v stop=%v",
				c.name, len(s.lines), s.hint, s.startRetry, s.stopRetry, c.wantLines, c.wantHint, c.wantStart, c.wantStop)
		}
	}
}

// ── runLyricsJob / fetchLyrics ──

// fakeLyricsClient 是 CDP 会话的桩：按字段返回，记下调用顺序；pageBlock 非空时页面取词先等它关闭。
type fakeLyricsClient struct {
	token     string
	pageBody  string
	pageErr   error
	cacheBody string
	cacheErr  error
	pageEnter chan struct{}
	pageBlock chan struct{}

	mu    sync.Mutex
	calls []string
}

func (f *fakeLyricsClient) note(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeLyricsClient) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLyricsClient) Token() string       { f.note("token"); return f.token }
func (f *fakeLyricsClient) AppPlatform() string { return "Win32_x86_64" }
func (f *fakeLyricsClient) AppVersion() string  { return "1.3.4.258" }
func (f *fakeLyricsClient) FetchLyrics(trackID, token string) (string, error) {
	f.note("page:" + trackID)
	if f.pageEnter != nil {
		close(f.pageEnter)
	}
	if f.pageBlock != nil {
		<-f.pageBlock
	}
	return f.pageBody, f.pageErr
}
func (f *fakeLyricsClient) FetchViaPageCache(trackID string) (string, error) {
	f.note("cache:" + trackID)
	return f.cacheBody, f.cacheErr
}

// lyricsStubs 替换 CLI 与 Go 直取两个 seam；测试结束还原。
type lyricsStubs struct {
	mu        sync.Mutex
	cliCalls  int
	np        watchdog.NowPlaying
	npErr     error
	cliEnter  chan struct{}
	cliBlock  chan struct{}
	direct    []string // 直取被调用时的 trackID
	directRes *lyric.Response
	directErr error
	dirEnter  chan struct{}
	dirBlock  chan struct{}
}

func useLyricsStubs(t *testing.T, s *lyricsStubs) {
	t.Helper()
	origCLI, origDirect := readNowPlaying, fetchLyricsDirect
	readNowPlaying = func(string) (watchdog.NowPlaying, error) {
		s.mu.Lock()
		s.cliCalls++
		s.mu.Unlock()
		if s.cliEnter != nil {
			close(s.cliEnter)
		}
		if s.cliBlock != nil {
			<-s.cliBlock
		}
		return s.np, s.npErr
	}
	fetchLyricsDirect = func(trackID, _, _, _ string, _ time.Duration) (*lyric.Response, error) {
		s.mu.Lock()
		s.direct = append(s.direct, trackID)
		s.mu.Unlock()
		if s.dirEnter != nil {
			close(s.dirEnter)
		}
		if s.dirBlock != nil {
			<-s.dirBlock
		}
		return s.directRes, s.directErr
	}
	t.Cleanup(func() { readNowPlaying, fetchLyricsDirect = origCLI, origDirect })
}

func (s *lyricsStubs) counts() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cliCalls, append([]string(nil), s.direct...)
}

// runJob 在 goroutine 里跑一次 runLyricsJob，返回它结束时关闭的通道。
func runJob(ctx context.Context, job lyricsJob, cliPath string, c lyricsClient, out chan lyricsResult) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLyricsJob(ctx, job, cliPath, c, out)
	}()
	return done
}

func waitDone(t *testing.T, label string, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s：取词 goroutine 未在 2s 内退出", label)
	}
}

func recvResult(t *testing.T, out chan lyricsResult) lyricsResult {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内没有收到取词结果")
	}
	return lyricsResult{}
}

func sameStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：得到 %q，期望 %q", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s：得到 %q，期望 %q", label, got, want)
		}
	}
}

// 换歌那次：CLI 取 URI → Go 直取拿到；结果带着代次、trackID、行与播放态对照。
// 变异自证：cliMismatch 不看 iconKnown → 「图标认不出」红；trackID 改用 np.URI 原样（与前一条同时注入）→ 本例「结果不对」
// 与 TestRunLyricsJobWithoutURI 的播客单集一例红（均实测）。
func TestRunLyricsJobSongChange(t *testing.T) {
	synced := mustParse(t, syncedBody)
	s := &lyricsStubs{np: watchdog.NowPlaying{URI: "spotify:track:abc", IsPlaying: true, HasTrack: true}, directRes: synced}
	useLyricsStubs(t, s)
	c := &fakeLyricsClient{token: "Bearer x"}
	out := make(chan lyricsResult, 1)

	waitDone(t, "换歌那次", runJob(context.Background(), lyricsJob{gen: 7, name: "歌名", singer: "歌手", pagePlaying: true, iconKnown: true}, `C:\cli.exe`, c, out))
	r := recvResult(t, out)
	if r.gen != 7 || r.retry || r.trackID != "abc" || len(r.lines) != 2 || r.resp != synced || r.diag != "" || r.cliMismatch {
		t.Fatalf("结果不对：%+v", r)
	}
	if cli, direct := s.counts(); cli != 1 || len(direct) != 1 || direct[0] != "abc" {
		t.Fatalf("应读一次 CLI、直取一次 abc，得到 cli=%d direct=%q", cli, direct)
	}
	sameStrings(t, "直取拿到就不再走页面", c.callList(), []string{"token"})

	// 页面图标与 CLI 的播放态对照：只在图标认得出时判。
	for _, tc := range []struct {
		name                   string
		pagePlaying, iconKnown bool
		want                   bool
	}{
		{"一致", true, true, false},
		{"不一致", false, true, true},
		{"图标认不出", false, false, false},
	} {
		out := make(chan lyricsResult, 1)
		waitDone(t, tc.name, runJob(context.Background(), lyricsJob{pagePlaying: tc.pagePlaying, iconKnown: tc.iconKnown}, `C:\cli.exe`, c, out))
		if r := recvResult(t, out); r.cliMismatch != tc.want {
			t.Errorf("%s：cliMismatch = %v，期望 %v", tc.name, r.cliMismatch, tc.want)
		}
	}
}

// CLI 的三种降级：没有 CLI、CLI 失败、CLI 说没有曲目 —— 都没有 URI，不发任何取词请求，照常送回（落地时按无歌词、不补取）。
// 变异自证：去掉 fetchLyrics 里 `trackID == ""` 的提前返回 → 四例都以空串直取、碰了 CDP 会话，红；
// 补取那次也读 CLI → 「补取不读 CLI」红（两条同时注入，各自的断言都红，实测）。
func TestRunLyricsJobWithoutURI(t *testing.T) {
	cases := []struct {
		name    string
		cliPath string
		np      watchdog.NowPlaying
		npErr   error
		wantCLI int
	}{
		{"没有 CLI", "", watchdog.NowPlaying{}, nil, 0},
		{"CLI 失败", `C:\cli.exe`, watchdog.NowPlaying{URI: "spotify:track:zzz", HasTrack: true}, errors.New("exit status 1"), 1},
		{"CLI 说没有曲目", `C:\cli.exe`, watchdog.NowPlaying{}, nil, 1},
		{"CLI 给的不是曲目 URI（播客单集）", `C:\cli.exe`, watchdog.NowPlaying{URI: "spotify:episode:e1", IsPlaying: true, HasTrack: true}, nil, 1},
	}
	for _, tc := range cases {
		s := &lyricsStubs{np: tc.np, npErr: tc.npErr}
		useLyricsStubs(t, s)
		c := &fakeLyricsClient{token: "Bearer x"}
		out := make(chan lyricsResult, 1)
		waitDone(t, tc.name, runJob(context.Background(), lyricsJob{gen: 1, pagePlaying: true, iconKnown: true}, tc.cliPath, c, out))
		r := recvResult(t, out)
		if r.trackID != "" || r.lines != nil || r.resp != nil || r.diag != "" || r.cliMismatch {
			t.Errorf("%s：结果应为空，得到 %+v", tc.name, r)
		}
		if cli, direct := s.counts(); cli != tc.wantCLI || len(direct) != 0 {
			t.Errorf("%s：cli=%d direct=%q，期望 cli=%d、不直取", tc.name, cli, direct, tc.wantCLI)
		}
		if calls := c.callList(); len(calls) != 0 {
			t.Errorf("%s：没有 URI 时不应碰 CDP 会话，得到 %q", tc.name, calls)
		}
		if st := settleLyrics(r); st.startRetry || st.stopRetry || len(st.lines) != 0 {
			t.Errorf("%s：没有 URI 时不补取，得到 %+v", tc.name, st)
		}
	}

	// 补取那次：不读 CLI，直接用带来的 trackID。
	s := &lyricsStubs{directErr: errors.New("HTTP 403")}
	useLyricsStubs(t, s)
	c := &fakeLyricsClient{token: "Bearer x", pageErr: errors.New("timeout"), cacheErr: errors.New("miss")}
	out := make(chan lyricsResult, 1)
	waitDone(t, "补取", runJob(context.Background(), lyricsJob{gen: 2, retry: true, trackID: "t1"}, `C:\cli.exe`, c, out))
	r := recvResult(t, out)
	if cli, _ := s.counts(); cli != 0 {
		t.Fatalf("补取不读 CLI，得到 %d 次", cli)
	}
	if !r.retry || r.trackID != "t1" || r.diag != "timeout" {
		t.Fatalf("补取的结果不对：%+v", r)
	}
}

// 三层取词的优先级与诊断串（fetchLyrics）：直取拿到即止；直取失败走页面；页面失败或没有 token 走缓存；
// 都不成时诊断串取最后一次带 token 的错误，没有 token 时是「lyrics unavailable」；任一层解析成功而没有可用的行
// （UNSYNCED、零行）当场返回响应与诊断。
// 变异自证：直取失败后不走页面 → 「直取 403」红；诊断串改取缓存的错误 → 「全不成」与「没有 token 且缓存不中」红；
// 缓存层解析成功而零行时改回落到末尾 → 缓存给 UNSYNCED / 零行的三例红（均实测）。
func TestFetchLyricsPriority(t *testing.T) {
	synced := mustParse(t, syncedBody)
	unsynced := mustParse(t, unsyncedBody)
	cases := []struct {
		name      string
		directRes *lyric.Response
		directErr error
		client    *fakeLyricsClient
		wantCalls []string
		wantLines int
		wantResp  bool
		wantDiag  string
	}{
		{"直取拿到", synced, nil, &fakeLyricsClient{token: "Bearer x"}, []string{"token"}, 2, true, ""},
		{"直取 UNSYNCED：当场返回", unsynced, nil, &fakeLyricsClient{token: "Bearer x"}, []string{"token"}, 0, true, noLineLyricsDiag(unsynced)},
		{"直取 403 → 页面拿到", nil, errors.New("HTTP 403"), &fakeLyricsClient{token: "Bearer x", pageBody: syncedBody}, []string{"token", "page:t"}, 2, true, ""},
		{"直取与页面都失败 → 缓存拿到", nil, errors.New("HTTP 403"), &fakeLyricsClient{token: "Bearer x", pageErr: errors.New("page 500"), cacheBody: syncedBody}, []string{"token", "page:t", "cache:t"}, 2, true, ""},
		{"没有 token → 只走缓存", nil, nil, &fakeLyricsClient{cacheBody: syncedBody}, []string{"token", "cache:t"}, 2, true, ""},
		{"全不成：诊断串取最后一次带 token 的错误", nil, errors.New("HTTP 403"), &fakeLyricsClient{token: "Bearer x", pageErr: errors.New("page 500"), cacheErr: errors.New("miss")}, []string{"token", "page:t", "cache:t"}, 0, false, "page 500"},
		{"没有 token 且缓存不中", nil, nil, &fakeLyricsClient{cacheErr: errors.New("miss")}, []string{"token", "cache:t"}, 0, false, i18n.T("lyrics unavailable")},
		// 缓存层与前两层一致：解析成功而没有可用的行，返回响应与「没有行级歌词」的诊断，不当成取不到。
		{"没有 token、缓存给 UNSYNCED", nil, nil, &fakeLyricsClient{cacheBody: unsyncedBody}, []string{"token", "cache:t"}, 0, true, noLineLyricsDiag(unsynced)},
		{"没有 token、缓存给零行", nil, nil, &fakeLyricsClient{cacheBody: zeroBody}, []string{"token", "cache:t"}, 0, true, noLineLyricsDiag(mustParse(t, zeroBody))},
		{"直取与页面都失败、缓存给 UNSYNCED：诊断是没有行级歌词，不是带 token 的错误", nil, errors.New("HTTP 403"), &fakeLyricsClient{token: "Bearer x", pageErr: errors.New("page 500"), cacheBody: unsyncedBody}, []string{"token", "page:t", "cache:t"}, 0, true, noLineLyricsDiag(unsynced)},
		{"没有 token、缓存给解析不了的串：仍是取不到", nil, nil, &fakeLyricsClient{cacheBody: `{"lyrics":`}, []string{"token", "cache:t"}, 0, false, i18n.T("lyrics unavailable")},
	}
	for _, tc := range cases {
		s := &lyricsStubs{directRes: tc.directRes, directErr: tc.directErr}
		useLyricsStubs(t, s)
		c := tc.client
		lines, resp, diag, nf := fetchLyrics(context.Background(), c, "t")
		sameStrings(t, tc.name, c.callList(), tc.wantCalls)
		if len(lines) != tc.wantLines || (resp != nil) != tc.wantResp || diag != tc.wantDiag || nf {
			t.Errorf("%s：lines=%d resp=%v diag=%q notFound=%v，期望 lines=%d resp=%v diag=%q notFound=false", tc.name, len(lines), resp != nil, diag, nf, tc.wantLines, tc.wantResp, tc.wantDiag)
		}
	}
}

// httpErr 是带状态码的取词错误（与 lyric.Fetch、cdp 页面内取词返回的同型），文案与状态码各自给出。
func httpErr(status int, text string) error {
	return &lyric.StatusError{Status: status, Err: errors.New(text)}
}

// 平台答复「没有这首歌的歌词」（notFound）：只认页面内取词与页面缓存的 HTTP 404；Go 直取的状态码（真机稳定 403）不参与，
// 401 / 403 / 400 / 5xx、超时、只在文案里写着 404 的普通错误都不是。404 之后仍走缓存，缓存取到响应以响应为准。
// 对应 2026-10-13 真机日志：无歌词曲目换歌那次页面内取词回 404（直取照常 403）。
// 变异自证（均实测）：直取的错误也参与判定 → 「直取 404 不算、页面给解析不了的串」红（页面出错时会覆盖掉，只有这一例拦得住）；
// 页面的 404 不置位 → 「页面 404」三例与「包了一层」红；缓存层不参与 → 「缓存 404」两例红；IsNotFound 不看状态码 →
// 401 / 403 / 400 / 500 与「缓存 401」各例红；IsNotFound 改成类型断言 → 「包了一层」红。
func TestFetchLyricsNotFound(t *testing.T) {
	synced := mustParse(t, syncedBody)
	unsynced := mustParse(t, unsyncedBody)
	direct403 := httpErr(403, "direct 403")
	unavailable := i18n.T("lyrics unavailable")
	cases := []struct {
		name      string
		directRes *lyric.Response
		directErr error
		client    *fakeLyricsClient
		wantLines int
		wantResp  bool
		wantDiag  string
		wantNF    bool
	}{
		{"直取 403、页面 404、缓存不中", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(404, "page 404"), cacheErr: errors.New("miss")}, 0, false, "page 404", true},
		{"直取 403、页面 404、缓存返回空串", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(404, "page 404")}, 0, false, "page 404", true},
		{"直取网络错误、页面 404", nil, errors.New("dial tcp: timeout"), &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(404, "page 404")}, 0, false, "page 404", true},
		{"页面 404 包了一层（errors.As 取得到）", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: fmt.Errorf("wrapped: %w", httpErr(404, "page 404"))}, 0, false, "wrapped: page 404", true},
		{"直取 404 不算（直取的状态码不参与）、页面 401", nil, httpErr(404, "direct 404"), &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(401, "page 401")}, 0, false, "page 401", false},
		{"直取 404 不算、页面超时", nil, httpErr(404, "direct 404"), &fakeLyricsClient{token: "Bearer x", pageErr: errors.New("timeout")}, 0, false, "timeout", false},
		{"直取 404 不算、页面给解析不了的串、缓存不中", nil, httpErr(404, "direct 404"), &fakeLyricsClient{token: "Bearer x", pageBody: `{"lyrics":`}, 0, false, "direct 404", false},
		{"页面 401（token 过期）", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(401, "page 401")}, 0, false, "page 401", false},
		{"页面 403", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(403, "page 403")}, 0, false, "page 403", false},
		{"页面 400（缺 app-platform 头）", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(400, "page 400")}, 0, false, "page 400", false},
		{"页面 500", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(500, "page 500")}, 0, false, "page 500", false},
		{"页面普通错误、文案里写着 404：不按文案判", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: errors.New("page-side lyrics fetch failed: http 404")}, 0, false, "page-side lyrics fetch failed: http 404", false},
		{"页面 200 而 UNSYNCED：原行为（有响应，不是 notFound）", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageBody: unsyncedBody}, 0, true, noLineLyricsDiag(unsynced), false},
		{"直取 UNSYNCED：当场返回，不是 notFound", unsynced, nil, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(404, "page 404")}, 0, true, noLineLyricsDiag(unsynced), false},
		{"页面 404 而缓存取到歌词：以歌词为准", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(404, "page 404"), cacheBody: syncedBody}, 2, true, "", false},
		{"页面 404 而缓存给 UNSYNCED：以响应为准", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(404, "page 404"), cacheBody: unsyncedBody}, 0, true, noLineLyricsDiag(unsynced), false},
		{"没有 token、缓存 404", nil, nil, &fakeLyricsClient{cacheErr: httpErr(404, "cache 404")}, 0, false, unavailable, true},
		{"页面 401、缓存 404", nil, direct403, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(401, "page 401"), cacheErr: httpErr(404, "cache 404")}, 0, false, "page 401", true},
		{"没有 token、缓存 401", nil, nil, &fakeLyricsClient{cacheErr: httpErr(401, "cache 401")}, 0, false, unavailable, false},
		{"直取拿到：不是 notFound", synced, nil, &fakeLyricsClient{token: "Bearer x", pageErr: httpErr(404, "page 404")}, 2, true, "", false},
	}
	for _, tc := range cases {
		useLyricsStubs(t, &lyricsStubs{directRes: tc.directRes, directErr: tc.directErr})
		lines, resp, diag, nf := fetchLyrics(context.Background(), tc.client, "t")
		if len(lines) != tc.wantLines || (resp != nil) != tc.wantResp || diag != tc.wantDiag || nf != tc.wantNF {
			t.Errorf("%s：lines=%d resp=%v diag=%q notFound=%v，期望 lines=%d resp=%v diag=%q notFound=%v",
				tc.name, len(lines), resp != nil, diag, nf, tc.wantLines, tc.wantResp, tc.wantDiag, tc.wantNF)
		}
	}
}

// 换歌那次与补取那次都把 notFound 随结果交回，落地时不起补取 / 停止补取；401 照常补取。
// 变异自证：runLyricsJob 不带回 notFound（第四个返回值丢弃）→ 两例 404 红（实测）。
func TestRunLyricsJobCarriesNotFound(t *testing.T) {
	cases := []struct {
		name               string
		job                lyricsJob
		pageErr            error
		wantNF             bool
		wantStart, wantStp bool
	}{
		{"换歌那次 404：不起补取", lyricsJob{gen: 1}, httpErr(404, "page 404"), true, false, false},
		{"补取那次 404：停止补取", lyricsJob{gen: 1, retry: true, trackID: "abc"}, httpErr(404, "page 404"), true, false, true},
		{"换歌那次 401：开始补取", lyricsJob{gen: 1}, httpErr(401, "page 401"), false, true, false},
		{"补取那次 401：照节奏继续", lyricsJob{gen: 1, retry: true, trackID: "abc"}, httpErr(401, "page 401"), false, false, false},
	}
	for _, tc := range cases {
		useLyricsStubs(t, &lyricsStubs{np: watchdog.NowPlaying{URI: "spotify:track:abc", HasTrack: true}, directErr: httpErr(403, "direct 403")})
		out := make(chan lyricsResult, 1)
		waitDone(t, tc.name, runJob(context.Background(), tc.job, `C:\cli.exe`, &fakeLyricsClient{token: "Bearer x", pageErr: tc.pageErr}, out))
		r := recvResult(t, out)
		s := settleLyrics(r)
		if r.notFound != tc.wantNF || s.startRetry != tc.wantStart || s.stopRetry != tc.wantStp {
			t.Errorf("%s：notFound=%v start=%v stop=%v，期望 notFound=%v start=%v stop=%v", tc.name, r.notFound, s.startRetry, s.stopRetry, tc.wantNF, tc.wantStart, tc.wantStp)
		}
	}
}

// 缓存层给出 UNSYNCED / 零行时，结果交给 settleLyrics：补取那次停止补取，换歌那次不起补取（平台已答复没有行级歌词，
// 再问也不会有）；缓存给的串解析不了时仍按取不到：补取那次照节奏继续，换歌那次开始补取。
// 变异自证：缓存层解析成功而零行时改回落到末尾的「lyrics unavailable」→ 前两例红（resp 为 nil，实测）；
// 换歌那次不看 resp、有 URI 就补取 → 前两例的「换歌那次」红（实测）。
func TestCacheNoLineLyricsSettle(t *testing.T) {
	cases := []struct {
		name                     string
		cacheBody                string
		wantRetryStop, wantStart bool
	}{
		{"缓存给 UNSYNCED", unsyncedBody, true, false},
		{"缓存给零行", zeroBody, true, false},
		{"缓存给解析不了的串", `not json`, false, true},
	}
	for _, tc := range cases {
		useLyricsStubs(t, &lyricsStubs{})
		lines, resp, diag, nf := fetchLyrics(context.Background(), &fakeLyricsClient{cacheBody: tc.cacheBody}, "t")
		if nf {
			t.Errorf("%s：没有 404，不应是 notFound", tc.name)
		}
		s := settleLyrics(lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "t"}, lines: lines, resp: resp, diag: diag})
		if s.stopRetry != tc.wantRetryStop || s.startRetry || len(s.lines) != 0 {
			t.Errorf("%s（补取那次）：settle = %+v，期望 stopRetry=%v、不开始补取、没有行", tc.name, s, tc.wantRetryStop)
		}
		s = settleLyrics(lyricsResult{lyricsJob: lyricsJob{trackID: "t"}, lines: lines, resp: resp, diag: diag})
		if s.startRetry != tc.wantStart || s.stopRetry || len(s.lines) != 0 {
			t.Errorf("%s（换歌那次）：settle = %+v，期望 startRetry=%v、没有行", tc.name, s, tc.wantStart)
		}
	}
}

// 作废后在下一步边界退出：CLI、Go 直取、页面取词三步途中各作废一次，之后的步骤一步都不发，也不送结果。
// 变异自证：runLyricsJob 里 CLI 之后不查 ctx → 「CLI 途中」红；fetchLyrics 里直取之后不查 → 「直取途中」红；
// 页面之后不查 → 「页面途中」红；送结果前不查 ctx（直接 select，select 在两路都就绪时随机挑）→ 「直取途中」「页面途中」
// 的「作废后不应送结果」红（跑 5 次每次至少一例红；均实测）。
func TestRunLyricsJobStopsAtStepBoundary(t *testing.T) {
	t.Run("CLI 途中", func(t *testing.T) {
		s := &lyricsStubs{np: watchdog.NowPlaying{URI: "spotify:track:abc", HasTrack: true}, cliEnter: make(chan struct{}), cliBlock: make(chan struct{})}
		useLyricsStubs(t, s)
		c := &fakeLyricsClient{token: "Bearer x"}
		out := make(chan lyricsResult, 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := runJob(ctx, lyricsJob{gen: 1}, `C:\cli.exe`, c, out)
		<-s.cliEnter
		cancel()
		close(s.cliBlock)
		waitDone(t, "CLI 途中", done)
		if _, direct := s.counts(); len(direct) != 0 || len(c.callList()) != 0 || len(out) != 0 {
			t.Fatalf("CLI 途中作废后不应再取词、不送结果：direct=%q calls=%q out=%d", direct, c.callList(), len(out))
		}
	})
	t.Run("直取途中", func(t *testing.T) {
		s := &lyricsStubs{directErr: errors.New("HTTP 403"), dirEnter: make(chan struct{}), dirBlock: make(chan struct{})}
		useLyricsStubs(t, s)
		c := &fakeLyricsClient{token: "Bearer x", pageBody: syncedBody, cacheBody: syncedBody}
		out := make(chan lyricsResult, 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := runJob(ctx, lyricsJob{gen: 1, retry: true, trackID: "t"}, "", c, out)
		<-s.dirEnter
		cancel()
		close(s.dirBlock)
		waitDone(t, "直取途中", done)
		sameStrings(t, "直取途中作废后不走页面与缓存", c.callList(), []string{"token"})
		if len(out) != 0 {
			t.Fatal("作废后不应送结果")
		}
	})
	t.Run("页面途中", func(t *testing.T) {
		s := &lyricsStubs{directErr: errors.New("HTTP 403")}
		useLyricsStubs(t, s)
		c := &fakeLyricsClient{token: "Bearer x", pageErr: errors.New("timeout"), cacheBody: syncedBody, pageEnter: make(chan struct{}), pageBlock: make(chan struct{})}
		out := make(chan lyricsResult, 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := runJob(ctx, lyricsJob{gen: 1, retry: true, trackID: "t"}, "", c, out)
		<-c.pageEnter
		cancel()
		close(c.pageBlock)
		waitDone(t, "页面途中", done)
		sameStrings(t, "页面途中作废后不走缓存", c.callList(), []string{"token", "page:t"})
		if len(out) != 0 {
			t.Fatal("作废后不应送结果")
		}
	})
}

// 还没被调度到就已作废（换歌、会话结束先于 goroutine 开跑）：一步都不做——不起 CLI、不直取、不碰 CDP 会话，不送结果。
// 变异自证：去掉 runLyricsJob 入口的 ctx 检查 → 「换歌那次」红（CLI 被调用一次）；去掉 fetchLyrics 开头的 ctx 检查 →
// 「直接调 fetchLyrics」红（直取被调用一次）。补取那次两道检查任留一道即绿（均实测）。
func TestLyricsJobCanceledBeforeStart(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name    string
		job     lyricsJob
		cliPath string
	}{
		{"换歌那次（有 CLI）", lyricsJob{gen: 1}, `C:\cli.exe`},
		{"补取那次（有 trackID）", lyricsJob{gen: 2, retry: true, trackID: "t"}, `C:\cli.exe`},
	}
	for _, tc := range cases {
		s := &lyricsStubs{np: watchdog.NowPlaying{URI: "spotify:track:abc", IsPlaying: true, HasTrack: true}, directRes: mustParse(t, syncedBody)}
		useLyricsStubs(t, s)
		c := &fakeLyricsClient{token: "Bearer x", pageBody: syncedBody, cacheBody: syncedBody}
		out := make(chan lyricsResult, 1)
		waitDone(t, tc.name, runJob(canceled, tc.job, tc.cliPath, c, out))
		if cli, direct := s.counts(); cli != 0 || len(direct) != 0 {
			t.Errorf("%s：已作废仍起了操作：cli=%d direct=%q", tc.name, cli, direct)
		}
		if calls := c.callList(); len(calls) != 0 {
			t.Errorf("%s：已作废仍碰了 CDP 会话：%q", tc.name, calls)
		}
		if len(out) != 0 {
			t.Errorf("%s：已作废不应送结果", tc.name)
		}
	}

	s := &lyricsStubs{directRes: mustParse(t, syncedBody)}
	useLyricsStubs(t, s)
	c := &fakeLyricsClient{token: "Bearer x", pageBody: syncedBody, cacheBody: syncedBody}
	if lines, resp, diag, nf := fetchLyrics(canceled, c, "t"); lines != nil || resp != nil || diag != "" || nf {
		t.Errorf("直接调 fetchLyrics：已作废应返回空，得到 lines=%d resp=%v diag=%q notFound=%v", len(lines), resp != nil, diag, nf)
	}
	if _, direct := s.counts(); len(direct) != 0 || len(c.callList()) != 0 {
		t.Errorf("直接调 fetchLyrics：已作废仍起了操作：direct=%q calls=%q", direct, c.callList())
	}
}

// 结果通道满了（循环还没收走上一个）：goroutine 等着，不丢自己的结果；被作废就放弃、退出，绝不永久挂住。
// 变异自证：送结果改成 `select { case out <- r: default: }` → 「腾出后照送」红（结果丢了）；
// 改成不带 ctx 的阻塞发送 → 「作废即退出」红（2s 内不退出，实测）。
func TestRunLyricsJobSendWhenChannelFull(t *testing.T) {
	stale := lyricsResult{lyricsJob: lyricsJob{gen: 1}}

	t.Run("腾出后照送", func(t *testing.T) {
		useLyricsStubs(t, &lyricsStubs{})
		out := make(chan lyricsResult, 1)
		out <- stale
		done := runJob(context.Background(), lyricsJob{gen: 2, retry: true}, "", &fakeLyricsClient{}, out)
		select {
		case <-done:
			t.Fatal("通道满时不应丢掉结果就退出")
		case <-time.After(100 * time.Millisecond):
		}
		if r := <-out; r.gen != 1 {
			t.Fatalf("先收到的应是占着通道的旧结果，得到代次 %d", r.gen)
		}
		if r := recvResult(t, out); r.gen != 2 {
			t.Fatalf("腾出后应收到它的结果，得到代次 %d", r.gen)
		}
		waitDone(t, "腾出后照送", done)
	})

	t.Run("作废即退出", func(t *testing.T) {
		useLyricsStubs(t, &lyricsStubs{})
		out := make(chan lyricsResult, 1)
		out <- stale
		ctx, cancel := context.WithCancel(context.Background())
		done := runJob(ctx, lyricsJob{gen: 2, retry: true}, "", &fakeLyricsClient{}, out)
		time.Sleep(50 * time.Millisecond) // 让它走到送结果那一步
		cancel()
		waitDone(t, "作废即退出", done)
		if r := <-out; r.gen != 1 || len(out) != 0 {
			t.Fatalf("通道里应只剩原来的旧结果，得到代次 %d、余 %d", r.gen, len(out))
		}
	})
}

// 会话结束（StopCh 或 CDP 断开）时 runSession 的 defer jobs.stop() 作废在飞的那次：卡在某一步的 goroutine 做完这一步即退出，
// 没人收结果也不挂住。这里照 runSession 的用法驱动 lyricsJobs + runLyricsJob。
// 变异自证：lyricsJobs.stop 不取消 ctx → 本例红（goroutine 卡在送结果，2s 内不退出，实测）。
func TestSessionEndReleasesJob(t *testing.T) {
	s := &lyricsStubs{directRes: mustParse(t, syncedBody), dirEnter: make(chan struct{}), dirBlock: make(chan struct{})}
	useLyricsStubs(t, s)
	out := make(chan lyricsResult) // 无缓冲：会话已结束，不会有人再收
	var j lyricsJobs
	ctx, gen := j.start()
	done := runJob(ctx, lyricsJob{gen: gen, retry: true, trackID: "t"}, "", &fakeLyricsClient{token: "Bearer x"}, out)
	<-s.dirEnter
	j.stop() // 会话结束
	close(s.dirBlock)
	waitDone(t, "会话结束", done)
}

// 换歌连按多次：照 runSession 的协议（每次换歌 start 一次并起 goroutine，每拍开头收一个、accept 判定），
// 只有最后一首的结果落地，前几首的要么没送、要么送了被丢弃；全部 goroutine 都退出。
// 第一首的结果在换歌之前就已送进通道、循环还没收（拿到结果时歌已变），它排在最前面被收到。
// 变异自证：accept 不比代次 → 本例红（落地的是第一首，实测）。
func TestRapidSongChangesApplyOnlyLast(t *testing.T) {
	synced := mustParse(t, syncedBody)
	useLyricsStubs(t, &lyricsStubs{np: watchdog.NowPlaying{URI: "spotify:track:x", HasTrack: true}, directRes: synced})
	out := make(chan lyricsResult, 1)
	var j lyricsJobs
	var dones []chan struct{}
	var last uint64
	for i := 0; i < 5; i++ {
		ctx, gen := j.start()
		last = gen
		dones = append(dones, runJob(ctx, lyricsJob{gen: gen}, `C:\cli.exe`, &fakeLyricsClient{token: "Bearer x"}, out))
		if i == 0 {
			waitDone(t, "第 1 首", dones[0]) // 它的结果已在通道里
		}
	}
	applied := 0
	deadline := time.After(2 * time.Second)
	for applied == 0 {
		select {
		case r := <-out:
			if j.accept(r.gen) {
				applied++
				if r.gen != last {
					t.Fatalf("落地的应是最后一首（代次 %d），得到 %d", last, r.gen)
				}
			}
		case <-deadline:
			t.Fatal("2s 内最后一首的结果没有落地")
		}
	}
	for i, d := range dones {
		waitDone(t, fmt.Sprintf("第 %d 首", i+1), d)
	}
	select {
	case r := <-out:
		if j.accept(r.gen) {
			t.Fatalf("最后一首落地之后不应再有结果被认下，得到代次 %d", r.gen)
		}
	default:
	}
}
