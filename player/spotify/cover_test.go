package spotify

// 本文件测封面：页面 URL 换 LARGE 档的规则（largeCoverURL），与 runCoverFetch 的出口——base64 先取 LARGE 档、
// 取不到退回页面原 URL，两条 song_info_update 的 Cover 都是页面原 URL，换歌后不补发。下载经 fetchCoverBase64 桩，不联网。

import (
	"context"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

const coverHash = "e8b066f70c206551210d902b"

// 变异自证：去掉 SMALL 档 → 「SMALL 档」红；去掉 DEFAULT 的 WebP 版 → 「DEFAULT 档的 WebP 版」红；
// 档位码改为在路径任意位置查找并替换 → 带查询串、hash 里含档位码字样两例、档位码不在路径开头共四例红；
// 前缀比较改成不分大小写 → 「主机大写」红（均实测）。
func TestLargeCoverURL(t *testing.T) {
	large := coverURLPrefix + coverSizeLarge + coverHash
	cases := []struct {
		name, in, want string
	}{
		{"DEFAULT 档", coverURLPrefix + coverSizeDefault + coverHash, large},
		{"DEFAULT 档的 WebP 版（页面给的就是这档）", coverURLPrefix + coverSizeDefaultWebP + coverHash, large},
		{"LARGE 档的 WebP 版（不在表里）", coverURLPrefix + "ab67616d0000aa54" + coverHash, coverURLPrefix + "ab67616d0000aa54" + coverHash},
		{"SMALL 档", coverURLPrefix + coverSizeSmall + coverHash, large},
		{"带查询串：只换档位码，查询串原样", coverURLPrefix + coverSizeDefault + coverHash + "?x=1&y=ab67616d00004851", large + "?x=1&y=ab67616d00004851"},
		{"hash 里含 SMALL 档位码字样：只换开头那段", coverURLPrefix + coverSizeDefault + coverSizeSmall + "00000000", coverURLPrefix + coverSizeLarge + coverSizeSmall + "00000000"},
		{"已是 LARGE、hash 里含 DEFAULT 档位码字样：不动", coverURLPrefix + coverSizeLarge + coverSizeDefault + "00000000", coverURLPrefix + coverSizeLarge + coverSizeDefault + "00000000"},
		{"已是 LARGE", large, large},
		{"艺人头像（别的档位码前缀）", coverURLPrefix + "ab6761610000e5eb" + coverHash, coverURLPrefix + "ab6761610000e5eb" + coverHash},
		{"档位码大写（不认识）", coverURLPrefix + "AB67616D00001E02" + coverHash, coverURLPrefix + "AB67616D00001E02" + coverHash},
		{"主机大写（不认识；浏览器给的 currentSrc 主机恒为小写）", "https://I.SCDN.CO/image/" + coverSizeDefault + coverHash, "https://I.SCDN.CO/image/" + coverSizeDefault + coverHash},
		{"http://", "http://i.scdn.co/image/" + coverSizeDefault + coverHash, "http://i.scdn.co/image/" + coverSizeDefault + coverHash},
		{"别的主机", "https://mosaic.scdn.co/640/" + coverSizeDefault + coverHash, "https://mosaic.scdn.co/640/" + coverSizeDefault + coverHash},
		{"别的路径", "https://i.scdn.co/images/" + coverSizeDefault + coverHash, "https://i.scdn.co/images/" + coverSizeDefault + coverHash},
		{"档位码不在路径开头", coverURLPrefix + "x" + coverSizeDefault + coverHash, coverURLPrefix + "x" + coverSizeDefault + coverHash},
		{"只有前缀", coverURLPrefix, coverURLPrefix},
		{"空串", "", ""},
		{"data URI", "data:image/jpeg;base64,/9j/2Q==", "data:image/jpeg;base64,/9j/2Q=="},
	}
	for _, c := range cases {
		if got := largeCoverURL(c.in); got != c.want {
			t.Errorf("%s：largeCoverURL(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// coverStub 按 URL 返回预设的 base64，记下调用顺序；block 非空时每次下载先等它关闭。
type coverStub struct {
	mu      sync.Mutex
	calls   []string
	results map[string]string
	entered chan struct{}
	block   chan struct{}
}

func (s *coverStub) fetch(_, u string, _ time.Duration) string {
	s.mu.Lock()
	s.calls = append(s.calls, u)
	s.mu.Unlock()
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}
	if s.block != nil {
		<-s.block
	}
	return s.results[u]
}

func (s *coverStub) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func useCoverStub(t *testing.T, s *coverStub) {
	t.Helper()
	orig := fetchCoverBase64
	fetchCoverBase64 = s.fetch
	t.Cleanup(func() { fetchCoverBase64 = orig })
}

// songInfos 收走 EventCh 里当前已有的全部 song_info_update。
func songInfos(t *testing.T, p *SpotifyPlayer) []*player.SongInfo {
	t.Helper()
	var out []*player.SongInfo
	for {
		select {
		case evt := <-p.EventCh:
			if evt.Type != player.EventSongInfoUpdate {
				continue
			}
			si, ok := evt.Data.(*player.SongInfo)
			if !ok {
				t.Fatalf("song_info_update 的 Data 类型不对: %T", evt.Data)
			}
			out = append(out, si)
		default:
			return out
		}
	}
}

func sameCalls(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：下载调用 %q，期望 %q", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s：下载调用 %q，期望 %q", label, got, want)
		}
	}
}

// 两条 song_info_update：第一条只带 URL，第二条带 b64；两条的 Cover 都是页面原 URL。
func checkTwoInfos(t *testing.T, label string, infos []*player.SongInfo, pageURL, wantB64 string) {
	t.Helper()
	if len(infos) != 2 {
		t.Fatalf("%s：应发两条 song_info_update，得到 %d 条", label, len(infos))
	}
	for i, si := range infos {
		if si.Cover != pageURL {
			t.Errorf("%s：第 %d 条的 Cover = %q，期望页面原 URL %q（前端按它判断是不是同一首）", label, i+1, si.Cover, pageURL)
		}
		if si.Title != "歌名 - 歌手" || si.Name != "歌名" || si.Singer != "歌手" {
			t.Errorf("%s：第 %d 条的歌曲信息不对：%+v", label, i+1, si)
		}
	}
	if infos[0].CoverBase64 != "" {
		t.Errorf("%s：第一条不应带 b64", label)
	}
	if infos[1].CoverBase64 != wantB64 {
		t.Errorf("%s：第二条的 b64 = %q，期望 %q", label, infos[1].CoverBase64, wantB64)
	}
}

// 变异自证：下载改回直接用页面原 URL → 前三例红；去掉退回原 URL 的那次下载 → 「LARGE 档取不到」「两档都取不到」
// 与「换不了档位」红；第二条的 Cover 改填 LARGE 档 URL → 前三例红（均实测）。
func TestRunCoverFetchPrefersLarge(t *testing.T) {
	page := coverURLPrefix + coverSizeDefault + coverHash
	large := coverURLPrefix + coverSizeLarge + coverHash

	t.Run("LARGE 档取到", func(t *testing.T) {
		s := &coverStub{results: map[string]string{large: "b64-640", page: "b64-300"}}
		useCoverStub(t, s)
		p := New(0, 500)
		p.runCoverFetch(context.Background(), page, "歌名", "歌手", "歌名 - 歌手")
		sameCalls(t, "LARGE 档取到", s.callList(), []string{large})
		checkTwoInfos(t, "LARGE 档取到", songInfos(t, p), page, "b64-640")
	})

	t.Run("LARGE 档取不到：退回页面原 URL", func(t *testing.T) {
		s := &coverStub{results: map[string]string{page: "b64-300"}}
		useCoverStub(t, s)
		p := New(0, 500)
		p.runCoverFetch(context.Background(), page, "歌名", "歌手", "歌名 - 歌手")
		sameCalls(t, "LARGE 档取不到", s.callList(), []string{large, page})
		checkTwoInfos(t, "LARGE 档取不到", songInfos(t, p), page, "b64-300")
	})

	t.Run("两档都取不到：第二条照发、b64 为空", func(t *testing.T) {
		s := &coverStub{}
		useCoverStub(t, s)
		p := New(0, 500)
		p.runCoverFetch(context.Background(), page, "歌名", "歌手", "歌名 - 歌手")
		sameCalls(t, "两档都取不到", s.callList(), []string{large, page})
		checkTwoInfos(t, "两档都取不到", songInfos(t, p), page, "")
	})

	// 换不了档位（已是 LARGE、不认识的前缀）：只下载一次，不重复下同一个 URL。
	for _, u := range []string{large, coverURLPrefix + "ab6761610000e5eb" + coverHash, "https://example.com/a.jpg"} {
		s := &coverStub{}
		useCoverStub(t, s)
		p := New(0, 500)
		p.runCoverFetch(context.Background(), u, "歌名", "歌手", "歌名 - 歌手")
		sameCalls(t, "换不了档位 "+u, s.callList(), []string{u})
		checkTwoInfos(t, "换不了档位 "+u, songInfos(t, p), u, "")
	}

	t.Run("页面没给封面：只发一条、不下载", func(t *testing.T) {
		s := &coverStub{}
		useCoverStub(t, s)
		p := New(0, 500)
		p.runCoverFetch(context.Background(), "", "歌名", "歌手", "歌名 - 歌手")
		sameCalls(t, "没给封面", s.callList(), nil)
		if infos := songInfos(t, p); len(infos) != 1 || infos[0].Cover != "" {
			t.Fatalf("没给封面时应只发一条、Cover 为空，得到 %+v", infos)
		}
	})
}

// LARGE 档下载途中换歌：不再退回原 URL 下载，也不补发第二条（上一首的封面会盖住当前歌）。
// 取消时机落在下载途中（理由同 kugou 的 TestRunCoverFetchRespectsCancel：调用前就取消会在顶部 return，测不到后面的检查）。
// 变异自证：去掉退回下载前的 `ctx.Err() == nil` → 调用列表多出页面原 URL，红；去掉发第二条之前的 ctx 检查 → 补发了第二条，红（均实测）。
func TestRunCoverFetchRespectsCancel(t *testing.T) {
	page := coverURLPrefix + coverSizeDefault + coverHash
	large := coverURLPrefix + coverSizeLarge + coverHash
	s := &coverStub{entered: make(chan struct{}, 1), block: make(chan struct{})}
	useCoverStub(t, s)
	p := New(0, 500)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.runCoverFetch(ctx, page, "歌名", "歌手", "歌名 - 歌手")
	}()
	select {
	case <-s.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("封面下载未在 2s 内发起")
	}
	cancel()
	close(s.block) // LARGE 档返回空：没取消的话会退回原 URL 再下一次
	<-done

	sameCalls(t, "下载途中换歌", s.callList(), []string{large})
	if infos := songInfos(t, p); len(infos) != 1 {
		t.Fatalf("下载途中换歌后只应有第一条 song_info_update，得到 %d 条", len(infos))
	}
}
