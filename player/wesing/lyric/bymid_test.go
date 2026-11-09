package lyric

// 本文件测按 mid 查歌曲信息与专辑（#146，设计 §3.1、§3.2、§3.4、§4）：FindSongInfoByMID、FindAlbumMID、
// AlbumCoverURL 与路径 mid。假内存沿用 roma_test.go 的 fakeMem，按真机只读探针（2026-11-04）所见的形状摆出
// 各种记录；可写区的全扫换成扫假内存。

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

const (
	bmMID       = "000GUpTN2D3Eys" // 漠河舞厅
	bmPrevMID   = "001BQTcC4T48UG" // 这世界那么多人（上一首）
	bmAlbum     = "003JbcCj0BKtoz" // 漠河舞厅的专辑
	bmPrevAlbum = "001fN0ks2oUDvX" // 这世界那么多人的专辑
	bmPad       = 0x500            // 每条记录之后补的 0：每处向后读 1 KB 读得出
)

// fakeScan 按地址升序在假内存里找 pattern（mask 为假的字节不比），同 proc.AOBScan；scans 记下扫了几遍。
func fakeScan(f *fakeMem, scans *int) scanFunc {
	return func(pattern []byte, mask []bool) []uint32 {
		*scans++
		keys := make([]uint32, 0, len(f.b))
		for a := range f.b {
			keys = append(keys, a)
		}
		slices.Sort(keys)
		var hits []uint32
		for _, a := range keys {
			ok := true
			for j := range pattern {
				v, has := f.b[a+uint32(j)]
				if !has || mask[j] && v != pattern[j] {
					ok = false
					break
				}
			}
			if ok {
				hits = append(hits, a)
			}
		}
		return hits
	}
}

// zeros 在 [addr, addr+n) 写 0。
func (f *fakeMem) zeros(addr, n uint32) {
	for i := uint32(0); i < n; i++ {
		f.b[addr+i] = 0
	}
}

// utf16At 在 addr 处写 s 的 UTF-16LE，其后补 pad 个 0。
func (f *fakeMem) utf16At(addr uint32, s string, pad uint32) {
	u := utf16.Encode([]rune(s))
	for i, c := range u {
		f.b[addr+uint32(i)*2] = byte(c)
		f.b[addr+uint32(i)*2+1] = byte(c >> 8)
	}
	f.zeros(addr+uint32(len(u))*2, pad)
}

// asciiAt 在 addr 处写 s 的 UTF-8 字节，其后补 pad 个 0。
func (f *fakeMem) asciiAt(addr uint32, s string, pad uint32) {
	for i := 0; i < len(s); i++ {
		f.b[addr+uint32(i)] = s[i]
	}
	f.zeros(addr+uint32(len(s)), pad)
}

// readLog 记下每次 bytes 读取的长度，用来数看了几处命中、每处读了多少。
type readLog struct {
	*fakeMem
	reads []uint32
}

func (r *readLog) bytes(addr, n uint32) ([]byte, bool) {
	r.reads = append(r.reads, n)
	return r.fakeMem.bytes(addr, n)
}

// startKSong 是开唱时内存里 StartKSong 命令的 JSON（真机探针所见的字段与次序）。
func startKSong(mid, name, singer string) string {
	return `{"cmd":"KSong","subcmd":{"value":"StartKSong","data":{"mid":"` + mid + `","singerid":"","songname":"` + name +
		`","size":"12645042","singername":"` + singer + `","lSongMask":"25848388311044","iHasCp":"-1"}}}`
}

// 按 mid 查歌曲信息（设计 §3.2、§4）。记录各占一个 0x1000 的格子，地址越大越新。
//
// 变异自证（实测）：
//   - 从地址最低的命中往上看：「同 mid 多份副本取最高地址」得到最旧的那份而红；
//   - 模式去掉 mid 之后的引号：「mid 只是更长的值的前缀」被认下而红；
//   - 不读到第一个 } 为止（在整段里找）：「songname 在 } 之后」「singername 在 } 之后」被认下而红；
//   - 不要求 singername 键：「缺 singername」「singername 在 } 之后」以空歌手被认下而红；
//   - 不要求歌名非空：「歌名为空」被认下而红；
//   - 歌名上限改成 49 个字：「歌名恰 50 个字」红；去掉上限：「歌名 51 个字」被认下而红；
//   - 歌手上限改成 99 字节：「歌手恰 100 字节」红；去掉上限：「歌手 101 字节」被认下而红；
//   - 要求歌手非空（照搬 tryParseFromSongNameAddr）：「歌手为空」红；
//   - 不校验传入的 mid：「mid 为空」「mid 只有 13 位」认下了对应的记录、各扫了一遍而红；
//   - 读不出就停（break 而不是 continue）：「最高的那处向后读不出」得不到下面那份而红；
//   - 结尾取第一个引号、不跳过转义（#146 代码审之前的写法）：转义各例被截断或认不下而红；
//   - 不按 JSON 解码、原样返回：转义各例带着反斜杠返回而红。
func TestSongInfoByMID(t *testing.T) {
	type rec struct {
		text string
		pad  uint32
	}
	ok := func(name, singer string) SongInfo { return SongInfo{Mid: bmMID, Name: name, Singer: singer} }
	none := SongInfo{}
	cases := []struct {
		name string
		mid  string
		recs []rec
		want SongInfo
	}{
		{"StartKSong 记录", bmMID, []rec{{startKSong(bmMID, "漠河舞厅", "柳爽"), bmPad}}, ok("漠河舞厅", "柳爽")},
		{"同 mid 多份副本取最高地址", bmMID, []rec{
			{startKSong(bmMID, "漠河舞厅", "旧副本一"), bmPad},
			{startKSong(bmMID, "漠河舞厅", "旧副本二"), bmPad},
			{startKSong(bmMID, "漠河舞厅", "柳爽"), bmPad},
		}, ok("漠河舞厅", "柳爽")},
		{"mid 只出现在日志行，没有 JSON 对象", bmMID, []rec{
			{"CProtocolGetAlbumID::OnTaskNotifyCompleted(), m_strAlbumID = " + bmAlbum + ", strMid = " + bmMID +
				` {"songname":"漠河舞厅","singername":"柳爽"}`, bmPad},
		}, none},
		{"别的 mid 的记录", bmMID, []rec{{startKSong(bmPrevMID, "这世界那么多人", "莫文蔚"), bmPad}}, none},
		{"mid 只是更长的值的前缀", bmMID, []rec{{startKSong(bmMID+"X", "漠河舞厅", "柳爽"), bmPad}}, none},
		{"歌单类记录（引号转义、带空格）", bmMID, []rec{
			{`[{\"mid\":\"` + bmMID + `\", \"duration\":\"\", \"songname\":\"漠河舞厅\", \"singername\":\"柳爽\"}]`, bmPad},
		}, none},
		{"对象里缺 songname", bmMID, []rec{{`{"mid":"` + bmMID + `","singerid":"","singername":"柳爽"}`, bmPad}}, none},
		{"songname 在 } 之后（属于下一个对象）", bmMID, []rec{
			{`{"mid":"` + bmMID + `","singerid":"","singername":"柳爽"},{"songname":"漠河舞厅"}`, bmPad},
		}, none},
		{"singername 在 } 之后", bmMID, []rec{
			{`{"mid":"` + bmMID + `","songname":"漠河舞厅"},{"singername":"柳爽"}`, bmPad},
		}, none},
		{"缺 singername", bmMID, []rec{{`{"mid":"` + bmMID + `","songname":"漠河舞厅","size":"1"}`, bmPad}}, none},
		{"歌名为空", bmMID, []rec{{startKSong(bmMID, "", "柳爽"), bmPad}}, none},
		{"歌名恰 50 个字", bmMID, []rec{{startKSong(bmMID, strings.Repeat("漠", 50), "柳爽"), bmPad}}, ok(strings.Repeat("漠", 50), "柳爽")},
		{"歌名 51 个字", bmMID, []rec{{startKSong(bmMID, strings.Repeat("漠", 51), "柳爽"), bmPad}}, none},
		{"歌手恰 100 字节", bmMID, []rec{{startKSong(bmMID, "漠河舞厅", strings.Repeat("a", 100)), bmPad}}, ok("漠河舞厅", strings.Repeat("a", 100))},
		{"歌手 101 字节", bmMID, []rec{{startKSong(bmMID, "漠河舞厅", strings.Repeat("a", 101)), bmPad}}, none},
		{"歌手为空", bmMID, []rec{{startKSong(bmMID, "漠河舞厅", ""), bmPad}}, ok("漠河舞厅", "")},
		{"最高的那处向后读不出，用下面那份", bmMID, []rec{
			{startKSong(bmMID, "漠河舞厅", "柳爽"), bmPad},
			{startKSong(bmMID, "漠河舞厅", "读不全"), 0},
		}, ok("漠河舞厅", "柳爽")},
		// JSON 转义（#146 代码审）：结尾是第一个未被转义的引号，值按 JSON 规则解码。
		{"歌名带转义引号", bmMID, []rec{{startKSong(bmMID, `Song \"Live\"`, "柳爽"), bmPad}}, ok(`Song "Live"`, "柳爽")},
		{"歌手带转义反斜杠", bmMID, []rec{{startKSong(bmMID, "漠河舞厅", `A\\B`), bmPad}}, ok("漠河舞厅", `A\B`)},
		{"歌名带 Unicode 转义", bmMID, []rec{{startKSong(bmMID, `Caf\u00e9`, "柳爽"), bmPad}}, ok("Café", "柳爽")},
		{"歌名以转义反斜杠结尾", bmMID, []rec{{startKSong(bmMID, `End\\`, "柳爽"), bmPad}}, ok(`End\`, "柳爽")},
		{"转义不合法", bmMID, []rec{{startKSong(bmMID, `Bad\q`, "柳爽"), bmPad}}, none},
		{"mid 为空", "", []rec{{startKSong("", "空", "空"), bmPad}}, none},
		{"mid 只有 13 位", bmMID[:13], []rec{{startKSong(bmMID[:13], "漠河舞厅", "柳爽"), bmPad}}, none},
	}
	for _, c := range cases {
		f := newFakeMem()
		for i, r := range c.recs {
			f.utf16At(0x02000000+uint32(i)*0x1000, r.text, r.pad)
		}
		scans := 0
		got, found := songInfoByMID(f, fakeScan(f, &scans), c.mid)
		if got != c.want || found != (c.want != none) {
			t.Errorf("%s：得到 (%+v, %v)，want %+v", c.name, got, found, c.want)
		}
		if wantScans := map[bool]int{true: 1, false: 0}[validMID(c.mid)]; scans != wantScans {
			t.Errorf("%s：扫了 %d 遍，want %d", c.name, scans, wantScans)
		}
	}
}

// 每处向后至多读 1 KB（设计 §3.2）：对象的 } 落在 1 KB 之内才认。读取从 "mid":"<mid>" 之后算起，
// 1 KB = 512 个 UTF-16 码元。
//
// 变异自证（实测）：读取长度改成 1026，「} 是第 513 个码元」被认下、两例都读了 1026 字节而红；改成 1022，
// 「} 是第 512 个码元」找不到、两例都读了 1022 字节而红；不读到第一个 } 为止，「} 是第 513 个码元」被认下而红。
func TestSongInfoByMIDReadsAtMost1KB(t *testing.T) {
	head := `,"songname":"漠河舞厅","singername":"柳爽","pad":"`
	for _, c := range []struct {
		units int // } 是 "mid":"<mid>" 之后的第几个码元
		found bool
	}{{512, true}, {513, false}} {
		after := head + strings.Repeat("a", c.units-len(utf16.Encode([]rune(head)))-2) + `"}`
		if n := len(utf16.Encode([]rune(after))); n != c.units {
			t.Fatalf("用例前提不成立：%d 个码元", n)
		}
		f := newFakeMem()
		f.utf16At(0x02000000, `{"mid":"`+bmMID+`"`+after, bmPad)
		r := &readLog{fakeMem: f}
		scans := 0
		if _, found := songInfoByMID(r, fakeScan(f, &scans), bmMID); found != c.found {
			t.Errorf("} 是第 %d 个码元：found = %v，want %v", c.units, found, c.found)
		}
		if fmt.Sprint(r.reads) != "[1024]" {
			t.Errorf("} 是第 %d 个码元：读取 %v，want 恰一次 1024 字节", c.units, r.reads)
		}
	}
}

// 上限（设计 §3.2，设计审 F3）：命中从地址最高的一处往下至多看 64 处，第一处合格的即结果；64 处都不合格
// 就返回未找到，不再往下。不合格的副本缺 songname。
//
// 变异自证（实测）：上限改成 65、或去掉上限，「合格的只在第 65 处」被找到、看了 65 处而红；上限改成 2，三例
// 都红；从低往高看，「合格的在第 3 处」得到第 70 处、另两例看的处数不对而红。
func TestSongInfoByMIDHitLimit(t *testing.T) {
	bad := `{"mid":"` + bmMID + `","singerid":"","singername":"柳爽"}`
	cases := []struct {
		name      string
		goodAt    []int // 从高往低数的第几处合格（1 起）
		want      string
		wantReads int
	}{
		{"同一 mid 70 处命中，合格的只在第 65 处", []int{65}, "", 64},
		{"合格的在第 3 处，更低处也有合格的", []int{3, 10, 70}, "第 3 处", 3},
		{"合格的在第 64 处", []int{64}, "第 64 处", 64},
	}
	for _, c := range cases {
		f := newFakeMem()
		const n = 70
		for i := range n {
			fromTop := n - i
			text := bad
			if slices.Contains(c.goodAt, fromTop) {
				text = startKSong(bmMID, fmt.Sprintf("第 %d 处", fromTop), "柳爽")
			}
			f.utf16At(0x02000000+uint32(i)*0x1000, text, bmPad)
		}
		r := &readLog{fakeMem: f}
		scans := 0
		got, _ := songInfoByMID(r, fakeScan(f, &scans), bmMID)
		if got.Name != c.want || len(r.reads) != c.wantReads || scans != 1 {
			t.Errorf("%s：得到 %q、看了 %d 处、扫了 %d 遍，want %q、%d 处、1 遍", c.name, got.Name, len(r.reads), scans, c.want, c.wantReads)
		}
	}
}

// 日志行与回包（真机探针所见的形状）。
func albumLogLine(album, mid string) string {
	return "[11-04 21:03:17.512][I] CProtocolGetAlbumID::OnTaskNotifyCompleted(), m_strAlbumID = " + album + ", strMid = " + mid + "\r\n"
}

func albumResp(album, mid string) string {
	return `{"code":0,"subcode":0,"message":"","default":0,"data":{"albummid":"` + album + `","ksongmid":"` + mid + `"}}`
}

// 按歌曲 mid 查专辑 mid（设计 §3.4、§4）。utf16 是日志行（UTF-16），utf8 是回包（UTF-8），都按给出的次序
// 从低到高摆放。
//
// 变异自证（实测）：
//   - 日志行查到之后仍扫回包、以回包为准：「只有日志行」「两者相符」「两者不符时取日志行」「日志行多份取最高
//     地址」红；
//   - 日志行只取值的前 14 个字符（不要求值紧跟着命中）：「专辑 mid 15 位（日志行）」「日志行的值与命中之间换了行」
//     被认下而红；
//   - 值只要非空、不要求 14 位字母数字：两种形式的「13 位」「含非法字符」、「15 位」「换了行」被认下而红；
//   - 校验只看长度：两种形式的「含非法字符」红（TestPathMID 的「含下划线」也红）；
//   - 回包不查跨对象：「albummid 在上一个对象里」被认下而红；
//   - 从低往高看：「日志行多份取最高地址」「回包多份取最高地址」得到旧的那份而红；
//   - 不校验传入的 mid：「mid 为空」认下了 mid 为空的回包、扫了 2 遍而红。
//
// 「回包里 albummid 为空」在「值只要非空」的变异下仍绿：空值本来就不是候选，这一例守的是取值不越过引号。
func TestAlbumMID(t *testing.T) {
	cases := []struct {
		name      string
		mid       string
		utf16     []string
		utf8      []string
		want      string
		wantScans int
	}{
		{"只有日志行", bmMID, []string{albumLogLine(bmAlbum, bmMID)}, nil, bmAlbum, 1},
		{"只有回包", bmMID, nil, []string{albumResp(bmAlbum, bmMID)}, bmAlbum, 2},
		{"两者相符", bmMID, []string{albumLogLine(bmAlbum, bmMID)}, []string{albumResp(bmAlbum, bmMID)}, bmAlbum, 1},
		{"两者不符时取日志行", bmMID, []string{albumLogLine(bmAlbum, bmMID)}, []string{albumResp(bmPrevAlbum, bmMID)}, bmAlbum, 1},
		{"日志行多份取最高地址", bmMID, []string{albumLogLine(bmPrevAlbum, bmMID), albumLogLine(bmAlbum, bmMID)}, nil, bmAlbum, 1},
		{"回包多份取最高地址", bmMID, nil, []string{albumResp(bmPrevAlbum, bmMID), albumResp(bmAlbum, bmMID)}, bmAlbum, 2},
		{"日志行锚定的 mid 不同", bmMID, []string{albumLogLine(bmPrevAlbum, bmPrevMID)}, nil, "", 2},
		{"回包锚定的 mid 不同", bmMID, nil, []string{albumResp(bmPrevAlbum, bmPrevMID)}, "", 2},
		{"日志行锚定的 mid 不同、回包相符", bmMID, []string{albumLogLine(bmPrevAlbum, bmPrevMID)}, []string{albumResp(bmAlbum, bmMID)}, bmAlbum, 2},
		{"专辑 mid 13 位（日志行）", bmMID, []string{albumLogLine(bmAlbum[:13], bmMID)}, nil, "", 2},
		{"专辑 mid 15 位（日志行）", bmMID, []string{albumLogLine(bmAlbum+"x", bmMID)}, nil, "", 2},
		{"专辑 mid 含非法字符（日志行）", bmMID, []string{albumLogLine("003JbcCj0BK-oz", bmMID)}, nil, "", 2},
		{"专辑 mid 13 位（回包）", bmMID, nil, []string{albumResp(bmAlbum[:13], bmMID)}, "", 2},
		{"专辑 mid 含非法字符（回包）", bmMID, nil, []string{albumResp("003JbcCj0BK oz", bmMID)}, "", 2},
		{"回包里 albummid 为空", bmMID, nil, []string{albumResp("", bmMID)}, "", 2},
		{"回包 albummid 在上一个对象里", bmMID, nil, []string{`{"albummid":"` + bmAlbum + `"},{"ksongmid":"` + bmMID + `"}`}, "", 2},
		{"日志行的值与命中之间换了行", bmMID, []string{"m_strAlbumID = " + bmAlbum + "\r\n, strMid = " + bmMID}, nil, "", 2},
		{"日志行缺 m_strAlbumID", bmMID, []string{"CProtocolGetAlbumID::OnTaskNotifyCompleted(), strMid = " + bmMID}, nil, "", 2},
		{"mid 为空", "", nil, []string{albumResp(bmAlbum, "")}, "", 0},
	}
	for _, c := range cases {
		f := newFakeMem()
		// 每条记录之前补 0x100 个 0：向前读的 64 字节读得出，不合格只能是因为内容。
		addr := uint32(0x03000000)
		for _, s := range c.utf16 {
			f.zeros(addr, 0x100)
			f.utf16At(addr+0x100, s, 0x10)
			addr += 0x1000
		}
		for _, s := range c.utf8 {
			f.zeros(addr, 0x100)
			f.asciiAt(addr+0x100, s, 0x10)
			addr += 0x1000
		}
		scans := 0
		if got := albumMID(f, fakeScan(f, &scans), c.mid); got != c.want || scans != c.wantScans {
			t.Errorf("%s：得到 %q、扫了 %d 遍，want %q、%d 遍", c.name, got, scans, c.want, c.wantScans)
		}
	}
}

// 上限（设计 §3.4）：同 §3.2，每种形式至多看 64 处；每处向前至多读 64 字节。不合格的日志行专辑 mid 只有 13 位。
//
// 变异自证（实测）：上限改成 65，「合格的只在第 65 处」被找到、读了 65 次而红；上限改成 2，三例都红；从低往高
// 看，三例的结果或读取次数不对而红；向前读取改成 66 字节，三例的读取长度不对而红；改成 56 字节（28 个码元，
// 装不下「m_strAlbumID = 」加 14 位），三例的读取长度不对、后两例找不到而红（TestAlbumMID 的日志行各例也红）。
func TestAlbumMIDLimits(t *testing.T) {
	cases := []struct {
		name   string
		goodAt []int
		want   string
		looked int // 日志行看了几处
	}{
		{"日志行 70 处，合格的只在第 65 处", []int{65}, "", 64},
		{"合格的在第 3 处", []int{3}, bmAlbum, 3},
		{"合格的在第 64 处", []int{64}, bmAlbum, 64},
	}
	for _, c := range cases {
		f := newFakeMem()
		const n = 70
		for i := range n {
			album := bmAlbum[:13]
			if slices.Contains(c.goodAt, n-i) {
				album = bmAlbum
			}
			f.utf16At(0x03000000+uint32(i)*0x1000, albumLogLine(album, bmMID), 0x10)
		}
		// 日志行查不到时接着扫回包；这里没有回包，不再读。
		r := &readLog{fakeMem: f}
		scans := 0
		got := albumMID(r, fakeScan(f, &scans), bmMID)
		if got != c.want || len(r.reads) != c.looked {
			t.Errorf("%s：得到 %q、读了 %d 次，want %q、%d 次", c.name, got, len(r.reads), c.want, c.looked)
		}
		for _, n := range r.reads {
			if n != 64 {
				t.Errorf("%s：每处应向前读 64 字节，实得 %v", c.name, r.reads)
				break
			}
		}
	}
}

// 按窗口歌名查记录时，songname 之前同一个对象里的 mid（兜底模式按它查封面，#146 审查）。prev 是命中之前的文本，
// 结尾紧挨着 "songname"。
//
// 变异自证（实测）：不查 {、}、NUL，「前一条记录的 mid，隔着 NUL」「隔着 }」两例取到了上一首的 mid 而红。
func TestMidBefore(t *testing.T) {
	for _, c := range []struct{ name, prev, want string }{
		{"StartKSong 记录", `"subcmd":{"value":"StartKSong","data":{"mid":"` + bmMID + `","singerid":"",`, bmMID},
		{"前一条记录的 mid，隔着 NUL", `{"mid":"` + bmPrevMID + `","songname":"旧歌","singername":"甲"}` + "\x00" + `{`, ""},
		{"前一条记录的 mid，隔着 }", `{"mid":"` + bmPrevMID + `","singername":"甲"},{`, ""},
		{"两个 mid，最后一个在同一个对象里", `{"mid":"` + bmPrevMID + `"},{"mid":"` + bmMID + `","singerid":"",`, bmMID},
		{"没有 mid", `{"singerid":"",`, ""},
		{"mid 为空", `{"mid":"","singerid":"",`, ""},
		{"mid 超过 20 个字符", `{"mid":"` + strings.Repeat("A", 21) + `",`, ""},
		{"mid 没有结尾引号", `{"mid":"` + bmMID, ""},
	} {
		if got := midBefore(c.prev); got != c.want {
			t.Errorf("%s：midBefore = %q，want %q", c.name, got, c.want)
		}
	}
}

// 封面地址：取专辑 mid 的倒数第二、第一位作两级目录（设计 §3.4，真机样例 v/X/001fN0ks2oUDvX）。
//
// 变异自证（实测）：目录取第 11、12 位，三个样例都红；不校验专辑 mid，「13 位」给出了地址、「空」越界 panic 而红。
func TestAlbumCoverURL(t *testing.T) {
	for _, c := range []struct{ album, want string }{
		{bmPrevAlbum, "http://imgcache.qq.com/music/photo/mid_album_800/v/X/001fN0ks2oUDvX.jpg"},
		{bmAlbum, "http://imgcache.qq.com/music/photo/mid_album_800/o/z/003JbcCj0BKtoz.jpg"},
		{"000y5gq7449K9I", "http://imgcache.qq.com/music/photo/mid_album_800/9/I/000y5gq7449K9I.jpg"},
		{bmAlbum[:13], ""},
		{"", ""},
	} {
		if got := AlbumCoverURL(c.album); got != c.want {
			t.Errorf("AlbumCoverURL(%q) = %q，want %q", c.album, got, c.want)
		}
	}
}

// 路径 mid（设计 §3.1）：…\<mid>\<mid>_accompany.pcm，目录名与文件名去掉后缀的部分相同、且是 14 位字母数字
// 才算。
//
// 变异自证（实测）：不比目录名，「目录是上一首」「目录名大小写不同」给出了 mid 而红；去掉 14 位字母数字的
// 校验，「13 位」「15 位」「含下划线」给出了 mid 而红；后缀区分大小写，「后缀大写」红；只认反斜杠，「正斜杠」
// 红；没有目录也放行，「只有文件名」给出了 mid 而红（TestConfirmPCMCarriesPathMID 的「路径不带目录」也红）。
func TestPathMID(t *testing.T) {
	root := `C:\Users\u\AppData\Roaming\Tencent\WeSing\WeSingCache\WeSingDL\Res\`
	for _, c := range []struct{ name, path, want string }{
		{"真机的形状", root + bmMID + `\` + bmMID + `_accompany.pcm`, bmMID},
		{"后缀大写", root + bmMID + `\` + bmMID + `_ACCOMPANY.PCM`, bmMID},
		{"正斜杠", "C:/Res/" + bmMID + "/" + bmMID + "_accompany.pcm", bmMID},
		{"目录只有一级", bmMID + `\` + bmMID + `_accompany.pcm`, bmMID},
		{"目录是上一首", root + bmPrevMID + `\` + bmMID + `_accompany.pcm`, ""},
		{"目录名大小写不同", root + strings.ToLower(bmMID) + `\` + bmMID + `_accompany.pcm`, ""},
		{"13 位", root + bmMID[:13] + `\` + bmMID[:13] + `_accompany.pcm`, ""},
		{"15 位", root + bmMID + `x\` + bmMID + `x_accompany.pcm`, ""},
		{"含下划线", root + `000GUpTN2D3E_s\000GUpTN2D3E_s_accompany.pcm`, ""},
		{"只有文件名", bmMID + `_accompany.pcm`, ""},
		{"原唱", root + bmMID + `\` + bmMID + `_kongsinger.pcm`, ""},
		{"空路径", "", ""},
	} {
		if got := pathMID(c.path); got != c.want {
			t.Errorf("%s：pathMID(%q) = %q，want %q", c.name, c.path, got, c.want)
		}
	}
}

// 核对通过时的路径 mid 取自那一次的路径，不是交叉核对用的 mid（设计 §3.3：路径 mid 不参与证明路径自己；
// 交叉核对的 mid 为空时照样有路径 mid）。
//
// 变异自证（实测）：把交叉核对用的 mid 当作路径 mid，「mid 为空：跳过核对」得到空串、「路径不带目录」得到交叉
// 核对的 mid 而红。
func TestConfirmPCMCarriesPathMID(t *testing.T) {
	const inst = 0x0B0E0000
	total := secBytes(183.693)
	cases := []struct {
		name, path, mid string
		swapAt          int    // 第几次等待之后把路径换成这首歌的（0 = 不换）
		want            string // MID()
	}{
		{"相符", accPath, testSongMID, 0, testSongMID},
		{"mid 为空：跳过核对", pcmPath(testPrevMID, "_accompany.pcm"), "", 0, testPrevMID},
		{"先旧后新", pcmPath(testPrevMID, "_accompany.pcm"), testSongMID, 2, testSongMID},
		{"路径不带目录", testSongMID + "_accompany.pcm", testSongMID, 0, ""},
	}
	for _, c := range cases {
		f := newFakeMem()
		f.pcmInst(inst, testPCMVtable, c.path, total, secBytes(0.4))
		waits := 0
		clock, err := confirmPCM(f, testPCMVtable, inst, c.mid, func() {
			if waits++; waits == c.swapAt {
				f.setPath(inst, accPath)
			}
		})
		if err != nil {
			t.Errorf("%s：err = %v", c.name, err)
			continue
		}
		if clock.MID() != c.want {
			t.Errorf("%s：MID() = %q，want %q", c.name, clock.MID(), c.want)
		}
	}
}
