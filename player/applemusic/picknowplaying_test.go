package applemusic

// 本文件只测 pickNowPlaying：一次全扫的多份「正在播放」存储里挑当前歌，结果确定。

import (
	"slices"
	"testing"

	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// 依次比：不是上一首期间就在的完整副本数、带封面的完整副本数（本例的存储都不带封面，这一档恒相等，
// 见 TestPickNowPlayingCoveredFull）、完整副本数、歌手非空、不是上一首期间就在的副本数、票数、
// 是否当前曲目、字典序——每次结果相同。
//
// 「旧歌残留」一例照 2026-10-08 端到端真机复跑的内存原样：早先播过的《History》留着 2 份非完整副本，
// 当前歌《秦皇岛》只有 1 份完整副本。按票数挑会选中 History，那次三次换歌三次宣告错。
//
// 变异自证：删掉排序里 `freshFull` 与 `full` 两档（剩下的先比新副本数、票数），「旧歌残留」一例选中
// History 而红；只删 `freshFull` 一档，「上一首留着两份完整副本」一例按完整副本数选到上一首而红；删掉
// `fresh` 一档，「都没有完整副本」一例按票数选到残留的而红；删掉「歌手非空」那一档，「加载占位」一例
// 选到占位而红；删掉「是否当前曲目」那一档，「同票保持当前」一例选到 "A" 而红。
func TestPickNowPlaying(t *testing.T) {
	chain := func(title, artist string, full bool, addr uintptr) mem.NowPlaying {
		return mem.NowPlaying{Title: title, Artist: artist, Album: "专辑", Full: full, Addr: addr}
	}
	n := func(title string, full bool, addr uintptr) mem.NowPlaying { return chain(title, "歌手", full, addr) }
	cur := songState{title: "B", artist: "歌手", album: "专辑", resolved: true}
	oldFull, oldFull2 := n("A 上一首", true, 0x10), n("A 上一首", true, 0x11)
	stale1, stale2 := n("A 残留", false, 0x10), n("A 残留", false, 0x11)
	cases := []struct {
		name  string
		songs []mem.NowPlaying
		prev  map[uintptr]string
		cur   songState
		want  string
	}{
		{"旧歌残留", []mem.NowPlaying{n("History", false, 1), n("History", false, 2), n("秦皇岛", true, 3)}, nil, songState{}, "秦皇岛"},
		{"上一首的完整副本还没释放", []mem.NowPlaying{oldFull, n("Z 新歌", true, 0x20)}, map[uintptr]string{0x10: chainID(oldFull)}, songState{}, "Z 新歌"},
		{"上一首留着两份完整副本，新歌只有一份", []mem.NowPlaying{oldFull, oldFull2, n("Z 新歌", true, 0x20)}, map[uintptr]string{0x10: chainID(oldFull), 0x11: chainID(oldFull2)}, songState{}, "Z 新歌"},
		{"只剩上一首期间就在的那份（重播）", []mem.NowPlaying{oldFull}, map[uintptr]string{0x10: chainID(oldFull)}, songState{}, "A 上一首"},
		{"都没有完整副本：新出现的胜过残留的", []mem.NowPlaying{stale1, stale2, n("Z 新歌", false, 0x20)}, map[uintptr]string{0x10: chainID(stale1), 0x11: chainID(stale2)}, songState{}, "Z 新歌"},
		{"加载占位", []mem.NowPlaying{chain("正在连接…", "", true, 1), n("真歌名", true, 2)}, nil, songState{}, "真歌名"},
		{"都没有完整副本时票多者胜", []mem.NowPlaying{n("A", false, 1), n("B", false, 2), n("A", false, 3)}, nil, songState{}, "A"},
		{"完整副本数相同比票数", []mem.NowPlaying{n("A", true, 1), n("B", true, 2), n("A", false, 3)}, nil, songState{}, "A"},
		{"同票保持当前", []mem.NowPlaying{n("A", true, 1), n("B", true, 2)}, nil, cur, "B"},
		{"都不是当前按字典序", []mem.NowPlaying{n("D", true, 1), n("C", true, 2)}, nil, cur, "C"},
	}
	for _, c := range cases {
		for i := 0; i < 20; i++ { // 多跑几遍：结果不能依赖 map 迭代序
			got, ok := pickNowPlaying(c.songs, c.prev, c.cur)
			if !ok || got.Title != c.want {
				t.Fatalf("%s: got %q, want %s", c.name, got.Title, c.want)
			}
		}
	}
	if _, ok := pickNowPlaying(nil, nil, cur); ok {
		t.Fatal("没有存储时应返回 ok=false")
	}
}

// 多份完整副本并存时取带封面的那份：这一档排在「不是上一首期间就在的完整副本数」之后、「完整副本数」
// 之前。
//
// 2026-10-09 真机（逐秒采样全部存储）：完整副本分带封面与不带封面两种。带封面的在全部采样里都是当前
// 歌名；不带封面的会残留——一份《crossing field》的从 03:47:54 留到 03:50:34，其间当前歌依次换成《过火》
// 《crossing field》《信仰》。「重启」「连续运行」两例照 03:47:58 的内存原样（《过火》带封面、
// 《crossing field》不带封面，各一份完整副本）：服务在这期间重启，确认位置结构之前的全扫 prev 为空、
// 两份都算新的，确认之后两份又都是上一首期间就在的；没有这一档时都落到字典序，选中 crossing field。
//
// 变异自证：删掉排序里 `fullCover` 一档，「重启」「连续运行」「残留的份数更多」三例选中 crossing field
// 而红；把它挪到 `freshFull` 之前，「新歌只有不带封面的」一例选中上一首而红；把它写成替换 `full` 一档
// （而不是插入），「没有带封面的：完整副本多者胜」「带封面的非完整副本不算」两例选到非完整的而红；计数时
// 不要求 Full（凡带封面的都数），「带封面的非完整副本不算」一例选中它而红；把判新的条件改成只看地址在
// 不在 prev 里（不比内容标识），「就地改写」一例落到字典序选中另一首而红；把这一档的比较写成 `>=`
// （两边相等也分先后），「各有一份带封面的」两例随输入顺序选出不同的歌而红。
func TestPickNowPlayingCoveredFull(t *testing.T) {
	st := func(title string, full bool, cover string, addr uintptr) mem.NowPlaying {
		np := mem.NowPlaying{Title: title, Artist: "歌手", Album: "专辑", Full: full, Addr: addr}
		if cover != "" {
			np.Cover, np.CoverMIME = []byte(cover), "image/jpeg"
		}
		return np
	}
	seen := func(nps ...mem.NowPlaying) map[uintptr]string {
		m := map[uintptr]string{}
		for _, np := range nps {
			m[np.Addr] = chainID(np)
		}
		return m
	}
	playing := st("过火", true, "过火的封面", 0x5143000)
	residue := st("crossing field", true, "", 0xc7388b0)
	// 就地改写：0x3138000 原是《上一首》带封面的完整副本，现在歌名换成了《新歌》，封面还是上一首的。
	before, rewritten := st("上一首", true, "上一首的封面", 0x3138000), st("新歌", true, "上一首的封面", 0x3138000)
	other := st("另一首", true, "另一首的封面", 0x4a02000)
	cases := []struct {
		name  string
		songs []mem.NowPlaying
		prev  map[uintptr]string
		cur   songState
		want  string
	}{
		{"重启：prev 为空", []mem.NowPlaying{playing, residue}, nil, songState{}, "过火"},
		{"连续运行：两份都是上一首期间就在的", []mem.NowPlaying{playing, residue}, seen(playing, residue), songState{}, "过火"},
		{"残留的份数更多也不压过带封面的", []mem.NowPlaying{playing, residue, st("crossing field", false, "", 0xc7372b0)}, seen(playing, residue, st("crossing field", false, "", 0xc7372b0)), songState{}, "过火"},
		{"新歌只有不带封面的完整副本，上一首带封面的还没被改写", []mem.NowPlaying{before, st("Z 新歌", true, "", 0x2b62b0)}, seen(before), songState{}, "Z 新歌"},
		{"就地改写：同一地址换了歌名，算新的", []mem.NowPlaying{rewritten, other}, seen(before, other), songState{}, "新歌"},
		{"没有带封面的：完整副本多者胜", []mem.NowPlaying{st("B", false, "", 1), st("B", false, "", 2), st("A", true, "", 3)}, seen(st("B", false, "", 1), st("B", false, "", 2), st("A", true, "", 3)), songState{}, "A"},
		{"带封面的非完整副本不算", []mem.NowPlaying{st("A 非完整", false, "图", 1), st("B 完整", true, "", 2)}, seen(st("A 非完整", false, "图", 1), st("B 完整", true, "", 2)), songState{}, "B 完整"},
		{"各有一份带封面的：字典序", []mem.NowPlaying{st("D", true, "图一", 1), st("C", true, "图二", 2)}, nil, songState{}, "C"},
		{"各有一份带封面的：保持当前", []mem.NowPlaying{st("D", true, "图一", 1), st("C", true, "图二", 2)}, nil, songState{title: "D", artist: "歌手", album: "专辑", resolved: true}, "D"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reversed := append([]mem.NowPlaying(nil), c.songs...)
			slices.Reverse(reversed)
			for _, songs := range [][]mem.NowPlaying{c.songs, reversed} { // 结果不能依赖输入顺序
				for i := 0; i < 20; i++ { // 也不能依赖 map 迭代序
					got, ok := pickNowPlaying(songs, c.prev, c.cur)
					if !ok || got.Title != c.want {
						t.Fatalf("got %q, want %s", got.Title, c.want)
					}
				}
			}
		})
	}
}

// 返回的是代表这首歌的那一份存储：完整的优先，带封面的优先，再取地址小的——盯梢与取封面都用它。
//
// 变异自证：把 betterRep 里「带封面的优先」那一档删掉，选中地址更小、不带封面的那份而红。
func TestPickNowPlayingRepresentative(t *testing.T) {
	plain := mem.NowPlaying{Title: "歌", Artist: "歌手", Addr: 1}
	fullNoCover := mem.NowPlaying{Title: "歌", Artist: "歌手", Full: true, Addr: 2}
	fullCover := mem.NowPlaying{Title: "歌", Artist: "歌手", Full: true, Addr: 3, Cover: []byte("图"), CoverMIME: "image/jpeg"}
	for _, order := range [][]mem.NowPlaying{{plain, fullNoCover, fullCover}, {fullCover, fullNoCover, plain}} {
		got, ok := pickNowPlaying(order, nil, songState{})
		if !ok || got.Addr != 3 || got.Cover == nil {
			t.Fatalf("代表应是带封面的完整副本（地址 3），got 地址 %d cover=%v", got.Addr, got.Cover != nil)
		}
	}
}
