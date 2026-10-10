package mem

// 本文件只测「正在播放」属性存储的识别（findNowPlaying）：认哪些、拒哪些、取值是否完整。
//
// 夹具取自 2026-10-08 真机内存（AppleMusic.exe 1.1540，当时在放刘森《叫魂》）：
//   - testdata/nowplaying_full.bin：一条完整存储（5 段，以 StorageSize=0 收尾）。原长 103856 字节，
//     其中封面 JPEG（F29F85E0 段 #27，VT_STREAM）103345 字节；为控制体积只保留前 16 字节并同步改写
//     流长度、该属性的 ValueSize 与所在段的 StorageSize，其余字节原样。
//   - testdata/nowplaying_fragment.bin：同一时刻内存里的残片（85 字节，Title > 空的 Music 段，无应用名段），
//     换歌后旧存储留下的就是这种形态。
//
// 合成用例（buildChain）只用来覆盖真机夹具没有的形态：Windows 设置搜索那类带 System.Title 的存储、
// 超长标题、代理对。

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读夹具 %s: %v", name, err)
	}
	return b
}

// lpwstr 编一个 VT_LPWSTR 属性值：[ValueSize][PropID][Reserved][VT 0x1F][pad][字符数含 0][UTF-16][对齐]。
func lpwstr(id uint32, s string) []byte {
	u := append(utf16.Encode([]rune(s)), 0)
	v := make([]byte, 13, 13+4+2*len(u)+3)
	binary.LittleEndian.PutUint32(v[4:], id)
	binary.LittleEndian.PutUint16(v[9:], vtLPWSTR)
	v = binary.LittleEndian.AppendUint32(v, uint32(len(u)))
	for _, c := range u {
		v = binary.LittleEndian.AppendUint16(v, c)
	}
	for len(v)%4 != 0 {
		v = append(v, 0)
	}
	binary.LittleEndian.PutUint32(v, uint32(len(v)))
	return v
}

// storageBytes 编一段 Storage：[StorageSize]["1SPS"][FMTID][values][0]。
func storageBytes(fmtid [16]byte, values ...[]byte) []byte {
	b := make([]byte, 4, 64)
	b = append(b, spsMagic...)
	b = append(b, fmtid[:]...)
	for _, v := range values {
		b = append(b, v...)
	}
	b = append(b, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	return b
}

// buildChain 把若干段首尾相接，再补整条存储的 0 结尾。
func buildChain(storages ...[]byte) []byte {
	var b []byte
	for _, s := range storages {
		b = append(b, s...)
	}
	return append(b, 0, 0, 0, 0)
}

func appleChain(title, music string) []byte {
	return buildChain(
		storageBytes(fmtidSummary, lpwstr(pidTitle, title)),
		storageBytes(fmtidMusic, lpwstr(pidArtist, music)),
		storageBytes(fmtidAppName, lpwstr(pidAppName, "Apple Music")),
	)
}

// 真机完整存储：认出且取值完整。
func TestFindNowPlayingRealStore(t *testing.T) {
	got := findNowPlaying(readFixture(t, "nowplaying_full.bin"))
	if len(got) != 1 {
		t.Fatalf("应认出 1 份，got %d 份", len(got))
	}
	g := got[0]
	if g.Title != "叫魂" || g.Artist != "刘森" || g.Album != "废话三篇 - Single" || !g.Full || g.Addr != 0 {
		t.Fatalf("got %q / %q / %q full=%v addr=%d, want 叫魂 / 刘森 / 废话三篇 - Single full=true addr=0",
			g.Title, g.Artist, g.Album, g.Full, g.Addr)
	}
}

// 换歌残片（Music 段为空、无应用名段）不算。
//
// 变异自证：把 findNowPlaying 的收录条件去掉 hasMusic && hasApp（两项一起去掉），本例变红；残片两段都缺，
// 只去掉其中一项本例仍绿，单项由 TestFindNowPlayingNeedsBothSegments 守。
func TestFindNowPlayingRejectsFragment(t *testing.T) {
	if got := findNowPlaying(readFixture(t, "nowplaying_fragment.bin")); len(got) != 0 {
		t.Fatalf("残片不应被认作正在播放：%+v", got)
	}
}

// 审查方真机：同进程里 Windows 设置搜索的属性存储也带 System.Title，取值是关键词串。它们没有
// System.Music 段与应用名段，必须被拒。混在真机夹具之间也只认出真的那一份。
//
// 变异自证：同上，两项一起去掉，本例认出 2 份而红；单项同样由 TestFindNowPlayingNeedsBothSegments 守。
func TestFindNowPlayingRejectsSettingsSearchStore(t *testing.T) {
	settings := buildChain(storageBytes(fmtidSummary, lpwstr(pidTitle, "聚焦;Windows 聚焦;图像;照片;背景")))
	junk := bytes.Repeat([]byte("1SPS\x00\xff"), 7) // 裸魔数与垃圾，不成段
	var mem []byte
	mem = append(mem, junk...)
	mem = append(mem, settings...)
	mem = append(mem, junk...)
	mem = append(mem, readFixture(t, "nowplaying_fragment.bin")...)
	mem = append(mem, readFixture(t, "nowplaying_full.bin")...)
	mem = append(mem, junk...)
	got := findNowPlaying(mem)
	if len(got) != 1 || got[0].Title != "叫魂" {
		t.Fatalf("应只认出《叫魂》一份，got %+v", got)
	}
}

// 输入是别的进程的任意字节：任何截断都不得 panic；链没收尾（截在中间）就不算。
//
// 变异自证：去掉 findNowPlaying 的 terminated 条件，截掉最后 4 字节（整条存储的 0 结尾）的那一例变红。
func TestFindNowPlayingTruncated(t *testing.T) {
	full := readFixture(t, "nowplaying_full.bin")
	for n := 0; n < len(full); n++ {
		if got := findNowPlaying(full[:n]); len(got) != 0 {
			t.Fatalf("截到 %d/%d 字节仍认出 %+v", n, len(full), got)
		}
	}
}

// 标题按记录长度完整读取（旧实现只看锚点后 120 字节，约 50 个字符以上就截断）；代理对按 UTF-16 合成。
//
// 变异自证：把 decodeUTF16Z 改回逐码元转 rune，🎵 变成两个 U+FFFD 而红。
func TestFindNowPlayingLongTitleAndSurrogates(t *testing.T) {
	title := "🎵" + strings.Repeat("很长的标题", 20) + "𠀀"
	got := findNowPlaying(appleChain(title, "歌手 — 专辑 — 副标题"))
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Title != title {
		t.Fatalf("标题被改写：got %q（%d 字）, want %d 字", got[0].Title, len([]rune(got[0].Title)), len([]rune(title)))
	}
	// 只按第一个「 — 」拆：专辑名里还可以有。
	if got[0].Artist != "歌手" || got[0].Album != "专辑 — 副标题" || got[0].Full {
		t.Fatalf("歌手/专辑/Full 拆错：%+v", got[0])
	}
}

// System.Music 里没有分隔符时整串是歌手。
func TestSplitArtistAlbum(t *testing.T) {
	cases := []struct{ in, artist, album string }{
		{"大张伟 — 人间精品", "大张伟", "人间精品"},
		{"Unknown Artist", "Unknown Artist", ""},
		{"A—B", "A—B", ""}, // 两侧没有空格的不是 Apple Music 的分隔符
	}
	for _, c := range cases {
		if a, b := splitArtistAlbum(c.in); a != c.artist || b != c.album {
			t.Errorf("splitArtistAlbum(%q) = (%q,%q), want (%q,%q)", c.in, a, b, c.artist, c.album)
		}
	}
}

// 两个段判据各自都要守：只有 System.Music 段、没有应用名段的不算；只有应用名段、没有 System.Music 段
// 的也不算。真机夹具的残片与设置搜索存储两段都缺，单独去掉其中一条判据测不出来，所以另用合成链。
//
// 变异自证：findNowPlaying 的收录条件只去掉 hasApp，第一例变红；只去掉 hasMusic，第二例变红。
func TestFindNowPlayingNeedsBothSegments(t *testing.T) {
	noApp := buildChain(
		storageBytes(fmtidSummary, lpwstr(pidTitle, "只有 Music 段")),
		storageBytes(fmtidMusic, lpwstr(pidArtist, "歌手 — 专辑")),
	)
	if got := findNowPlaying(noApp); len(got) != 0 {
		t.Fatalf("没有应用名段的存储不应被认作正在播放：%+v", got)
	}
	noMusic := buildChain(
		storageBytes(fmtidSummary, lpwstr(pidTitle, "只有应用名段")),
		storageBytes(fmtidAppName, lpwstr(pidAppName, "Apple Music")),
	)
	if got := findNowPlaying(noMusic); len(got) != 0 {
		t.Fatalf("没有 System.Music 段的存储不应被认作正在播放：%+v", got)
	}
	if got := findNowPlaying(appleChain("三段齐全", "歌手 — 专辑")); len(got) != 1 {
		t.Fatalf("三段齐全的合成链应被认出（对照组），got %+v", got)
	}
}
