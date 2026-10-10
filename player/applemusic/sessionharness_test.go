package applemusic

// 本文件只放会话场景测试的共用夹具：假内存（fakeMem，及让存储时有时无的 flapMem）、假时钟与事件收集
// （harness）。本身不含测试。

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// fakeMem 按测试脚本给出位置结构读数、「正在播放」存储与扫描结果，并数扫描次数、记下每次收不收候选。
type fakeMem struct {
	twins map[uintptr]mem.Twin
	songs []mem.NowPlaying
	docs  []mem.TTMLDoc
	scans int
	wants []bool // 每次全扫的 wantTwins
	// midScan 非 nil 时接管全扫的「途中」（快照已取、还没交回），由它决定何时调 during：用来摆
	// 「扫到一半换了歌」这类场景。为 nil 时 during 调一次。
	midScan func(during func())
}

func (f *fakeMem) ReadTwin(addr uintptr) (mem.Twin, bool) {
	t, ok := f.twins[addr]
	return t, ok
}

// ReadBytes 只认得出当前各存储的地址：返回它的 Sig。存储被换掉（songs 里没有这个地址了）就读不出，
// 与真机上旧存储被释放、覆盖后对不上是一回事。
func (f *fakeMem) ReadBytes(addr uintptr, n int) ([]byte, bool) {
	for _, s := range f.songs {
		if s.Addr == addr && len(s.Sig) >= n {
			return append([]byte(nil), s.Sig[:n]...), true
		}
	}
	return nil, false
}

// Scan 取快照 → 途中（midScan，或调一次 during；真实现在扫描途中反复调 during）→ 交回快照。
func (f *fakeMem) Scan(wantTwins bool, during func()) mem.Snapshot {
	f.scans++
	f.wants = append(f.wants, wantTwins)
	s := mem.Snapshot{Songs: append([]mem.NowPlaying(nil), f.songs...), Docs: append([]mem.TTMLDoc(nil), f.docs...)}
	if wantTwins {
		for a, t := range f.twins {
			if t.Dur >= 15 && t.Dur == math.Trunc(t.Dur) {
				s.Twins = append(s.Twins, a)
			}
		}
		sort.Slice(s.Twins, func(i, j int) bool { return s.Twins[i] < s.Twins[j] })
	}
	switch {
	case f.midScan != nil:
		f.midScan(during)
	case during != nil:
		during()
	}
	return s
}

// flapMem 包住 fakeMem：hide 决定某一次全扫认不认得出「正在播放」存储（nth 是本会话第几次全扫，从 1 起；
// hide 为 nil 时都认得出）。复读（ReadBytes）不受它影响，照 fakeMem.songs 读得出：认出的那一份经得起
// applySnapshot 的复读核对，盯梢也看不出变化，存储「在不在」只在全扫之间翻转。
type flapMem struct {
	*fakeMem
	hide func(wantTwins bool, nth int) bool
}

func (m *flapMem) Scan(wantTwins bool, during func()) mem.Snapshot {
	snap := m.fakeMem.Scan(wantTwins, during)
	if m.hide != nil && m.hide(wantTwins, m.scans) {
		snap.Songs = nil
	}
	return snap
}

// hideCollect 是复审给出的抖动：只有不收候选的全扫认得出存储，收候选的认不出。
func hideCollect(wantTwins bool, _ int) bool { return wantTwins }

// newFlapHarness 起一个内存里一直有一份完整存储、全扫认不认得出由 hide 决定的会话。
func newFlapHarness(t *testing.T, hide func(wantTwins bool, nth int) bool) (*harness, *flapMem) {
	h := newHarness(t)
	m := &flapMem{fakeMem: h.f, hide: hide}
	h.s.r = m
	h.f.songs = np("时有时无")
	return h, m
}

const (
	realTwin uintptr = 0x1000
	// step 是 harness 每步推进的假时间。真机轮询 50ms；测试用 100ms，够细又不慢。
	step = 100 * time.Millisecond
	// flipPhase 是假位置读数相对「开始走」那一刻的滞后：真机开播后第一跳在 1.28~1.54s 才来。
	flipPhase = 0.45
)

// harness 驱动一个 session：每步推进 step 假时钟、tick 一次、收走事件。
// play 为真时 realTwin 的位置从 basePos 起、滞后 flipPhase 后每整秒 +1（与真机的整秒读数一致）。
type harness struct {
	t       *testing.T
	f       *fakeMem
	p       *AppleMusicPlayer
	s       *session
	now     time.Time
	evs     []player.Event
	scanAt  []time.Time // 每次全扫发生在哪一轮（与 fakeMem.wants 一一对应）
	play    bool
	baseAt  time.Time
	basePos float64
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, f: &fakeMem{twins: map[uintptr]mem.Twin{}}, p: New(0, 50), now: time.Unix(1_700_000_000, 0)}
	h.s = newSession(h.p, h.f, func() time.Time { return h.now })
	h.drain()
	return h
}

// setTwin 设 realTwin 的读数；playing 时从 pos 起按墙钟推进。
func (h *harness) setTwin(dur, pos float64, playing bool) {
	h.f.twins[realTwin] = mem.Twin{Dur: dur, Pos: pos}
	h.play, h.baseAt, h.basePos = playing, h.now, pos
}

func (h *harness) drain() {
	for {
		select {
		case e := <-h.p.Events():
			h.evs = append(h.evs, e)
		default:
			return
		}
	}
}

// run 推进 d 的假时间。
func (h *harness) run(d time.Duration) {
	for end := h.now.Add(d); h.now.Before(end); h.now = h.now.Add(step) {
		if h.play {
			tw := h.f.twins[realTwin]
			tw.Pos = h.basePos + math.Floor(math.Max(0, h.now.Sub(h.baseAt).Seconds()-flipPhase))
			h.f.twins[realTwin] = tw
		}
		scans := h.f.scans
		h.s.tick()
		if h.f.scans != scans { // 一轮至多一次全扫
			h.scanAt = append(h.scanAt, h.now)
		}
		h.drain()
	}
}

// waitAnnounce 逐步推进，返回出现 song_info_update 用了多久（含看到它的那一步）；limit 内没出现返回 -1。
func waitAnnounce(h *harness, limit time.Duration) time.Duration {
	from, start := h.mark(), h.now
	for h.now.Sub(start) < limit {
		h.run(step)
		if count(h.since(from), player.EventSongInfoUpdate) > 0 {
			return h.now.Sub(start)
		}
	}
	return -1
}

// since 返回下标 from 之后的事件；mark 返回当前事件数，作为下一段的起点。
func (h *harness) since(from int) []player.Event { return h.evs[from:] }
func (h *harness) mark() int                     { return len(h.evs) }

func count(evs []player.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func statuses(evs []player.Event) []string {
	var out []string
	for _, e := range evs {
		if s, ok := e.Data.(*player.StatusInfo); ok {
			out = append(out, s.Status)
		}
	}
	return out
}

func allLyrics(evs []player.Event) []*player.AllLyricsData {
	var out []*player.AllLyricsData
	for _, e := range evs {
		if d, ok := e.Data.(*player.AllLyricsData); ok {
			out = append(out, d)
		}
	}
	return out
}

func songInfos(evs []player.Event) []*player.SongInfo {
	var out []*player.SongInfo
	for _, e := range evs {
		if d, ok := e.Data.(*player.SongInfo); ok {
			out = append(out, d)
		}
	}
	return out
}

// verdicts 返回 index:-1 的 lyric_update（「无歌词」）。
func verdicts(evs []player.Event) []*player.LyricUpdate {
	var out []*player.LyricUpdate
	for _, e := range evs {
		if d, ok := e.Data.(*player.LyricUpdate); ok && d.Index == -1 {
			out = append(out, d)
		}
	}
	return out
}

// lineIdxs 返回各条 lyric_update 的行号（不含 index:-1 的「无歌词」），按发出的先后。
func lineIdxs(evs []player.Event) []int {
	var out []int
	for _, e := range evs {
		if d, ok := e.Data.(*player.LyricUpdate); ok && d.Index >= 0 {
			out = append(out, d.Index)
		}
	}
	return out
}

// resumes 返回各条 playback_resume 的载荷。
func resumes(evs []player.Event) []*player.PlaybackTimeInfo {
	var out []*player.PlaybackTimeInfo
	for _, e := range evs {
		if e.Type == player.EventPlaybackResume {
			out = append(out, e.Data.(*player.PlaybackTimeInfo))
		}
	}
	return out
}

// docAt 造一份在内存里有若干副本的 TTML 文档：addrs 是各副本的地址（真机上文档被重新取回时会多出
// 新地址的副本）。
func docAt(text string, addrs ...uintptr) mem.TTMLDoc {
	return mem.TTMLDoc{Text: text, Count: len(addrs), Addrs: addrs}
}

// ttmlAt 造一份 Line 型 TTML：<body dur> 为 dur，各行起点为 starts（秒），第 i 行的文本是「第 i 行」。
func ttmlAt(dur float64, starts ...float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line"><body dur="%.3f"><div>`, dur)
	for i, st := range starts {
		fmt.Fprintf(&b, `<p begin="%.3f" end="%.3f" itunes:key="L%d">第 %d 行</p>`, st, st+1, i+1, i)
	}
	b.WriteString(`</div></body></tt>`)
	return b.String()
}

// store 造一份「正在播放」存储：地址与 Sig 由标题、专辑和封面决定——哪样变了，就是另一份存储
// （真机换歌、补上封面时存储多在新地址重建；就地改写的情形由用例自己改 Addr）。cover 为空串表示不带封面。
func store(title, cover string) mem.NowPlaying { return storeOf(title, "专辑", cover) }

func storeOf(title, album, cover string) mem.NowPlaying {
	id := title + "\x00" + album + "\x00" + cover
	h := fnv.New32a()
	h.Write([]byte(id))
	np := mem.NowPlaying{
		Title: title, Artist: "歌手", Album: album, Full: true,
		Addr: uintptr(0x100000) + uintptr(h.Sum32()%0xFFFF)*0x1000,
		Sig:  []byte("store:" + id),
	}
	if cover != "" {
		np.Cover, np.CoverMIME = []byte(cover), "image/jpeg"
	}
	return np
}

// np 是最常用的情形：一份带封面的完整存储。
func np(title string) []mem.NowPlaying {
	return []mem.NowPlaying{store(title, "封面:"+title)}
}
