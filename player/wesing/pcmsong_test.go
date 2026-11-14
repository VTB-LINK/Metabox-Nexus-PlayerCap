package wesing

// 本文件测 PCM 模式的宣告前流程 resolvePCM（#146，设计 §3.3、§3.4、§4）：#145 的交叉核对原样保留，核对通过
// 之后按 a / b / c 三种情形选宣告用的歌曲信息，封面按路径 mid 查专辑。查询全换成假的。

import (
	"errors"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
)

const (
	psWindow    = "漠河舞厅"
	psMID       = "000GUpTN2D3Eys" // 这首（窗口上的歌）
	psPrevMID   = "001BQTcC4T48UG" // 上一首「这世界那么多人」
	psAlbum     = "003JbcCj0BKtoz"
	psPrevAlbum = "001fN0ks2oUDvX"
)

// fakePCM 是一组假查询。rec 是按窗口歌名查到的记录（Name 为空即查不到）；path 是伴奏路径里的 mid，核对与真实的
// ConfirmPCM 同口径：交叉核对的 mid 为空、或等于路径 mid 才通过。calls 记下调用的先后。
type fakePCM struct {
	rec     lyric.SongInfo
	path    string
	records map[string]lyric.SongInfo // 按 mid 查得到的记录
	albums  map[string]string         // 歌曲 mid → 专辑 mid
	pos     float32                   // 宣告前重读到的位置；假时钟核对那次的位置恒为 0
	calls   []string
}

func (f *fakePCM) queries() pcmQueries {
	return pcmQueries{
		findInfo: func(w string) (lyric.SongInfo, error) {
			f.calls = append(f.calls, "info("+w+")")
			if f.rec.Name == "" {
				return lyric.SongInfo{}, errors.New("songname field not found")
			}
			return f.rec, nil
		},
		confirm: func(mid string) (*lyric.PCMClock, string, error) {
			f.calls = append(f.calls, "confirm("+mid+")")
			if mid != "" && mid != f.path {
				return nil, "", errors.New("the accompaniment decoder path does not match the song mid")
			}
			return &lyric.PCMClock{}, f.path, nil
		},
		byMID: func(mid string) (lyric.SongInfo, bool) {
			f.calls = append(f.calls, "bymid("+mid+")")
			r, ok := f.records[mid]
			return r, ok
		},
		album: func(mid string) string {
			f.calls = append(f.calls, "album("+mid+")")
			return f.albums[mid]
		},
		position: func(*lyric.PCMClock) float32 {
			f.calls = append(f.calls, "position")
			return f.pos
		},
	}
}

// 三种情形与 F1、F2 两个反例（设计 §3.3、§4）。
//
// 变异自证（实测）：
//   - 交叉核对直接用记录的 mid（不经 confirmMID）：「F1」「情形 a：按窗口歌名退回的是上一首」把上一首 A 的 mid
//     交给核对、转了兜底而红（TestResolvePCMF1AnnouncesNothingOfPrevious 也红）；
//   - 交叉核对传空串（跳过核对）：「F2」「记录给出了 mid、路径不是它」认下了不符的路径而红，「情形 a」「情形 b」
//     的调用不对而红；
//   - 情形 b 不要求记录歌名等于窗口歌名（只比 mid）：「窗口标题乱码、退回的记录恰是这首」被当作情形 b 而红；
//   - 情形 c 沿用按窗口歌名退回的记录（#145 的做法）：「F1」宣告了 A 的歌名与歌手而红，情形 c 另两例也红；
//   - 情形 b 优先于 a：「a 与 b 都有，取 a」「情形 a，歌手为空」取了窗口记录的歌手而红；
//   - 封面按记录的 mid 查：「F1」「情形 a：按窗口歌名退回的是上一首」查了 A 的专辑而红，情形 c 两例也红；
//   - 情形 b 不记为已确认：「情形 b」红；
//   - 路径 mid 为空也去查：「路径 mid 为空」多出两次查询而红；
//   - 没有歌手也拼「 - 」：没有歌手的七例标题不对而红。
//
// 宣告用的初始位置在查找全部做完之后重读（#191）。变异自证（实测）：
//   - 重读挪到核对之后、查找之前：带调用次序的五个成功例红；
//   - 不重读、取核对那次的读数（clock.Position()）：「情形 a：按路径 mid 查到记录」「路径 mid 为空」的位置为 0 而红；
//   - 重读挪进路径 mid 非空的分支：「路径 mid 为空」少了重读而红。
func TestResolvePCM(t *testing.T) {
	cur := lyric.SongInfo{Mid: psMID, Name: psWindow, Singer: "柳爽"}
	prev := lyric.SongInfo{Mid: psPrevMID, Name: "这世界那么多人", Singer: "莫文蔚"}
	albums := map[string]string{psMID: psAlbum, psPrevMID: psPrevAlbum}
	cover := lyric.AlbumCoverURL(psAlbum)
	cases := []struct {
		name    string
		window  string
		f       fakePCM
		wantErr bool
		want    pcmSong
		calls   string
	}{
		{"情形 a：按路径 mid 查到记录", psWindow,
			fakePCM{rec: cur, path: psMID, records: map[string]lyric.SongInfo{psMID: cur}, albums: albums, pos: 1.65}, false,
			pcmSong{title: psWindow + " - 柳爽", name: psWindow, singer: "柳爽", cover: cover, mid: psMID, confirmed: true, pos: 1.65},
			"info(漠河舞厅) confirm(" + psMID + ") bymid(" + psMID + ") album(" + psMID + ") position"},
		{"情形 a，歌手为空", psWindow,
			fakePCM{rec: cur, path: psMID, records: map[string]lyric.SongInfo{psMID: {Mid: psMID, Name: psWindow}}, albums: albums}, false,
			pcmSong{title: psWindow, name: psWindow, cover: cover, mid: psMID, confirmed: true}, ""},
		{"情形 a：按窗口歌名退回的是上一首，按路径 mid 查到这首", psWindow,
			fakePCM{rec: prev, path: psMID, records: map[string]lyric.SongInfo{psMID: cur}, albums: albums}, false,
			pcmSong{title: psWindow + " - 柳爽", name: psWindow, singer: "柳爽", cover: cover, mid: psMID, confirmed: true},
			"info(漠河舞厅) confirm() bymid(" + psMID + ") album(" + psMID + ") position"},
		{"a 与 b 都有，取 a", psWindow,
			fakePCM{rec: lyric.SongInfo{Mid: psMID, Name: psWindow, Singer: "窗口记录的歌手"}, path: psMID,
				records: map[string]lyric.SongInfo{psMID: cur}, albums: albums}, false,
			pcmSong{title: psWindow + " - 柳爽", name: psWindow, singer: "柳爽", cover: cover, mid: psMID, confirmed: true}, ""},
		{"情形 b：按路径 mid 查不到，窗口歌名匹配上的记录的 mid 等于路径 mid", psWindow,
			fakePCM{rec: cur, path: psMID, albums: albums}, false,
			pcmSong{title: psWindow + " - 柳爽", name: psWindow, singer: "柳爽", cover: cover, mid: psMID, confirmed: true},
			"info(漠河舞厅) confirm(" + psMID + ") bymid(" + psMID + ") album(" + psMID + ") position"},
		{"情形 c：记录尚未写入", psWindow,
			fakePCM{path: psMID, albums: albums}, false,
			pcmSong{title: psWindow, name: psWindow, cover: cover, mid: psMID},
			"info(漠河舞厅) confirm() bymid(" + psMID + ") album(" + psMID + ") position"},
		{"情形 c：窗口歌名匹配上的记录没有 mid", psWindow,
			fakePCM{rec: lyric.SongInfo{Name: psWindow, Singer: "柳爽"}, path: psMID, albums: albums}, false,
			pcmSong{title: psWindow, name: psWindow, cover: cover, mid: psMID}, ""},
		{"情形 c：窗口标题乱码、退回的记录恰是这首（不认，设计 §3.3 的 b 要求窗口歌名匹配上）", "ĆęÄ×ÎčĚü",
			fakePCM{rec: cur, path: psMID, albums: albums}, false,
			pcmSong{title: "ĆęÄ×ÎčĚü", name: "ĆęÄ×ÎčĚü", cover: cover, mid: psMID}, ""},
		{"情形 c：专辑也查不到", psWindow,
			fakePCM{path: psMID}, false,
			pcmSong{title: psWindow, name: psWindow, mid: psMID}, ""},
		{"F1：按路径 mid 查不到，按窗口歌名退回上一首 A 的完整记录", psWindow,
			fakePCM{rec: prev, path: psMID, albums: albums}, false,
			pcmSong{title: psWindow, name: psWindow, cover: cover, mid: psMID},
			"info(漠河舞厅) confirm() bymid(" + psMID + ") album(" + psMID + ") position"},
		{"F2：同名的 A、B，窗口歌名匹配上的是 B，路径是上一首残留的 A", psWindow,
			fakePCM{rec: lyric.SongInfo{Mid: psMID, Name: psWindow, Singer: "B 的歌手"}, path: psPrevMID,
				records: map[string]lyric.SongInfo{psPrevMID: {Mid: psPrevMID, Name: psWindow, Singer: "A 的歌手"}}, albums: albums}, true,
			pcmSong{}, "info(漠河舞厅) confirm(" + psMID + ")"},
		{"记录给出了 mid、路径不是它：核对不过，照 #145 转兜底", psWindow,
			fakePCM{rec: cur, path: ""}, true,
			pcmSong{}, "info(漠河舞厅) confirm(" + psMID + ")"},
		{"路径 mid 为空（跳过核对）：不查歌曲信息与专辑", psWindow,
			fakePCM{path: "", albums: albums, pos: 2.4}, false,
			pcmSong{title: psWindow, name: psWindow, pos: 2.4}, "info(漠河舞厅) confirm() position"},
	}
	for _, c := range cases {
		clock, got, err := resolvePCM(c.window, c.f.queries())
		if (err != nil) != c.wantErr || (clock == nil) != c.wantErr {
			t.Errorf("%s：err = %v、clock = %v，want 出错 %v", c.name, err, clock, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("%s：\n got %+v\nwant %+v", c.name, got, c.want)
		}
		if calls := strings.Join(c.f.calls, " "); c.calls != "" && calls != c.calls {
			t.Errorf("%s：调用 %q，want %q", c.name, calls, c.calls)
		}
		for _, call := range c.f.calls {
			if strings.Contains(call, "("+psPrevMID+")") && !strings.HasPrefix(call, "confirm") {
				t.Errorf("%s：拿上一首的 mid 查了 %s", c.name, call)
			}
		}
	}
}

// F1 的宣告不含上一首 A 的任何内容：歌名、歌手、标题、封面都不是 A 的（设计 §4）。与上表的「F1」同一组输入，
// 逐字段说明，免得上表的整体比较掩盖了是哪一项。
func TestResolvePCMF1AnnouncesNothingOfPrevious(t *testing.T) {
	f := fakePCM{
		rec:    lyric.SongInfo{Mid: psPrevMID, Name: "这世界那么多人", Singer: "莫文蔚"},
		path:   psMID,
		albums: map[string]string{psMID: psAlbum, psPrevMID: psPrevAlbum},
	}
	_, s, err := resolvePCM(psWindow, f.queries())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, v := range []string{s.title, s.name, s.singer, s.cover} {
		if strings.Contains(v, "这世界那么多人") || strings.Contains(v, "莫文蔚") || strings.Contains(v, psPrevAlbum) {
			t.Errorf("宣告带上了上一首的内容：%+v", s)
		}
	}
	if s.name != psWindow || s.singer != "" || s.confirmed || s.mid != psMID {
		t.Errorf("应为窗口歌名、歌手为空、未确认、mid 为路径 mid：%+v", s)
	}
	if want := lyric.AlbumCoverURL(psAlbum); s.cover != want {
		t.Errorf("封面应按路径 mid 查：%q，want %q", s.cover, want)
	}
}
