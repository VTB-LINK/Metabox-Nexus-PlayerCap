package qqmusic

// 本文件只测 readSongPrefix 读 Struct1 的专辑、歌手、歌名（设计 §6.4 第 4 步「前缀」），两种字符串模型：
//   - 宽字符（22.82 等）：三个 32 位指针所指的 UTF-16 串原样返回（不去空白、不去控制字符、不改大小写，不经 sanitizeString）；
//     各至多 256 个码元、其内须有 NUL；指针为 0、读指针或读串失败、256 个码元内没有 NUL 即读不出；不成对的代理项原样解码为
//     U+FFFD，由 assoc.RecordNeedle 拒绝。
//   - 窄字符（22.16、22.22）：三个 24 字节的 MSVC 32 位 std::string，经 ssoFromBuf 解出、原样返回字节，不转码；堆上的串须读满，
//     解出的字节数须等于长度字段、在 1～768 字节之间、其内没有 NUL、是合法的 UTF-8，不合即读不出。
//
// 地址空间是假的。

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

const (
	prefixStruct1 = 0x10000
	prefixStrings = 0x20000
)

// prefixSpace 拼出 Struct1（22.82 的偏移）与三个串：ptrs 非 nil 时取代按串算出的指针；strs 是三个串各自的原始字节
// （专辑、歌手、歌名），依次放在 prefixStrings 起的私有可写区里，各占 1024 字节。地址 0 处也放一个读得出的串：
// 指针为 0 须由 readSongPrefix 自己拒绝，不能靠读失败兜住。
func prefixSpace(strs [3][]byte, ptrs *[3]uint32) *fakeSpace {
	offs := knownVersions["22.82"]
	var area []byte
	var p [3]uint32
	for i, s := range strs {
		p[i] = uint32(prefixStrings + len(area))
		area = append(area, s...)
		area = append(area, make([]byte, 1024-len(s))...)
	}
	if ptrs != nil {
		p = *ptrs
	}
	st := make([]byte, 12)
	for i, off := range [...]uintptr{offs.AlbumOff, offs.SingerOff, offs.NameOff} {
		binary.LittleEndian.PutUint32(st[off:], p[i])
	}
	return &fakeSpace{regions: []fakeRegion{
		privRegion(0, wide("零页")),
		{memRegion{base: prefixStruct1, size: 12, state: windows.MEM_COMMIT, protect: windows.PAGE_READWRITE, typ: memImage}, st},
		privRegion(prefixStrings, area),
	}}
}

func wide(s string) []byte { return utf16z(s) }

// 原样返回：首尾空格、控制字符、大小写、中日文都保留；255 个码元加 NUL 读得出；串紧挨区域末尾、只读到一部分而 NUL 在其中也读得出。
// 变异自证（实测）：读出后经 sanitizeString，带空格与控制字符的两例红；码元上限少算一个，255 个码元一例红。
func TestReadSongPrefixRaw(t *testing.T) {
	long := strings.Repeat("长", assoc.RecordStrMax-1)
	for _, c := range []struct {
		name                string
		album, singer, song string
	}{
		{"首尾空格", " Album ", "Singer ", " Song"},
		{"控制字符与大小写", "ALBUM\x01", "sInGeR", "Song\x7f"},
		{"中日文", "夏鳴。2016", "Akie秋绘", "君の知らない物語 (Inst.)"},
		{"255 个码元", long, "歌手", "歌名"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := prefixSpace([3][]byte{wide(c.album), wide(c.singer), wide(c.song)}, nil)
			a, sg, n, ok := readSongPrefix(s.source(), prefixStruct1, knownVersions["22.82"])
			if !ok || a != c.album || sg != c.singer || n != c.song {
				t.Fatalf("读到 %q、%q、%q（ok %v），want %q、%q、%q", a, sg, n, ok, c.album, c.singer, c.song)
			}
		})
	}
	t.Run("紧挨区域末尾", func(t *testing.T) {
		s := prefixSpace([3][]byte{wide("专辑"), wide("歌手"), wide("歌名")}, nil)
		str := &s.regions[len(s.regions)-1]
		str.data = str.data[:2048+len(wide("歌名"))]
		str.size = uintptr(len(str.data))
		if _, _, n, ok := readSongPrefix(s.source(), prefixStruct1, knownVersions["22.82"]); !ok || n != "歌名" {
			t.Fatalf("歌名 %q（ok %v），want 歌名", n, ok)
		}
	})
}

// 读不出：专辑、歌手、歌名的指针任一为 0；256 个码元内没有 NUL；读 Struct1 的指针失败；读串失败。
// 变异自证（实测）：不查指针为 0，三个指针为 0 的例子读了地址 0 处的串而红；不要求 NUL，256 个码元一例红；
// 读串失败时按空串处理，读串失败一例红。
func TestReadSongPrefixUnreadable(t *testing.T) {
	good := [3][]byte{wide("专辑"), wide("歌手"), wide("歌名")}
	noNUL := []byte(strings.Repeat("A\x00", assoc.RecordStrMax))
	for _, c := range []struct {
		name  string
		space func() *fakeSpace
	}{
		{"专辑指针为 0", func() *fakeSpace { return prefixSpace(good, &[3]uint32{0, prefixStrings + 1024, prefixStrings + 2048}) }},
		{"歌手指针为 0", func() *fakeSpace { return prefixSpace(good, &[3]uint32{prefixStrings, 0, prefixStrings + 2048}) }},
		{"歌名指针为 0", func() *fakeSpace { return prefixSpace(good, &[3]uint32{prefixStrings, prefixStrings + 1024, 0}) }},
		{"256 个码元内没有 NUL", func() *fakeSpace {
			return prefixSpace([3][]byte{wide("专辑"), wide("歌手"), append(noNUL, 0, 0)}, nil)
		}},
		{"读 Struct1 的指针失败", func() *fakeSpace {
			s := prefixSpace(good, nil)
			s.fail = func(addr uintptr, _ int) int {
				if addr >= prefixStruct1 && addr < prefixStruct1+12 {
					return 0
				}
				return -1
			}
			return s
		}},
		{"读串失败", func() *fakeSpace {
			s := prefixSpace(good, nil)
			s.fail = func(addr uintptr, _ int) int {
				if addr == prefixStrings+1024 {
					return 0
				}
				return -1
			}
			return s
		}},
		{"指针指向不可读的地址", func() *fakeSpace {
			return prefixSpace(good, &[3]uint32{prefixStrings, 0x7000_0000, prefixStrings + 2048})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if a, s, n, ok := readSongPrefix(c.space().source(), prefixStruct1, knownVersions["22.82"]); ok {
				t.Fatalf("读出了 %q、%q、%q，want 读不出", a, s, n)
			}
		})
	}
}

// 不成对的代理项原样解码为 U+FFFD，readSongPrefix 照常返回，由 assoc.RecordNeedle 拒绝、不扫。
func TestReadSongPrefixLoneSurrogate(t *testing.T) {
	lone := binary.LittleEndian.AppendUint16(nil, 0xD800)
	lone = append(lone, wide("尾")...)
	s := prefixSpace([3][]byte{wide("专辑"), wide("歌手"), lone}, nil)
	a, sg, n, ok := readSongPrefix(s.source(), prefixStruct1, knownVersions["22.82"])
	if want := string(utf16.Decode([]uint16{0xD800, '尾'})); !ok || n != want {
		t.Fatalf("歌名 %q（ok %v），want %q", n, ok, want)
	}
	if _, ok := assoc.RecordNeedle(a, sg, n); ok {
		t.Fatal("含 U+FFFD 的歌名拼出了前缀，want 拒绝")
	}
}

// —— 窄字符版本（22.16、22.22）——

const (
	narrowLow     = 0x10000 // 「堆指针不大于 0x10000」一例的指针：这里放着读得出的串，拒绝只能靠指针的判定
	narrowStruct1 = 0x30000
	narrowHeap    = 0x40000
	narrowSlot    = 2048 // 堆上每段占的字节数

	kimi = "君の知らない物語"         // 24 字节，在堆上（容量 31）
	inst = "君の知らない物語 (Inst.)" // 32 字节，在堆上（容量 47）
	// gbkWudi 是 GBK 编码的「无地自容」（8 字节，不是合法的 UTF-8）。
	gbkWudi = "\xCE\xDE\xB5\xD8\xD7\xD4\xC8\xDD"
)

var narrowVersions = []string{"22.16", "22.22"}

// narrowSpace 按 ver 的偏移拼出 Struct1（三个 24 字节的 std::string）与堆：strs 是专辑、歌手、歌名的原始字节，依次放在
// narrowHeap 起的第 0、1、2 格（各占 narrowSlot 字节，其后为 NUL），Struct1 里照 narrowField 放（短于 16 字节的内联）；
// fields 里非 nil 的一项整个取代该段的 24 字节。narrowLow 处另有一个读得出的 10 字节 ASCII 串。
func narrowSpace(ver string, strs, fields [3][]byte) *fakeSpace {
	offs := knownVersions[ver]
	heap := make([]byte, 3*narrowSlot)
	st := make([]byte, max(offs.AlbumOff, offs.SingerOff, offs.NameOff)+24)
	for i, off := range [...]uintptr{offs.AlbumOff, offs.SingerOff, offs.NameOff} {
		copy(heap[i*narrowSlot:], strs[i])
		f := fields[i]
		if f == nil {
			f = narrowField(strs[i], heapSlot(i))
		}
		copy(st[off:], f)
	}
	return &fakeSpace{regions: []fakeRegion{
		privRegion(narrowLow, []byte("Low string\x00")),
		{memRegion{base: narrowStruct1, size: uintptr(len(st)), state: windows.MEM_COMMIT, protect: windows.PAGE_READWRITE, typ: memImage}, st},
		privRegion(narrowHeap, heap),
	}}
}

// heapSlot 是堆上第 i 格的地址。
func heapSlot(i int) uint32 { return uint32(narrowHeap + i*narrowSlot) }

// le32 是 p 的 4 字节小端表示，作 std::string 的堆指针。
func le32(p uint32) []byte { return binary.LittleEndian.AppendUint32(nil, p) }

// 窄字符版本，22.16、22.22 的偏移各跑一遍：内联（短于 16 字节）、堆上（长于 15 字节）、一短一长混合、15 与 16 字节的分界、
// 首尾空格，都原样读出 UTF-8 字节（不去空白、不转码），拼出的前缀与同样三段放进宽字符假内存（22.82 的偏移）所得逐字节相同。
// 变异自证（实测）：窄字符版本也走宽字符一支，各例红。
func TestReadSongPrefixNarrow(t *testing.T) {
	for _, ver := range narrowVersions {
		for _, c := range []struct {
			name                string
			album, singer, song string
		}{
			{"内联", "Album", "Singer", "Song"},
			{"堆上", kimi, "スーパーセル", inst},
			{"长短长", kimi, "supercell", inst},
			{"短长短", "A", kimi, "Song"},
			{"15 与 16 字节", "123456789012345", "1234567890123456", "Song"},
			{"首尾空格", " Album ", "Singer ", " " + inst + " "},
		} {
			t.Run(ver+"/"+c.name, func(t *testing.T) {
				s := narrowSpace(ver, [3][]byte{[]byte(c.album), []byte(c.singer), []byte(c.song)}, [3][]byte{})
				a, sg, n, ok := readSongPrefix(s.source(), narrowStruct1, knownVersions[ver])
				if !ok || a != c.album || sg != c.singer || n != c.song {
					t.Fatalf("读到 %q、%q、%q（ok %v），want %q、%q、%q", a, sg, n, ok, c.album, c.singer, c.song)
				}
				got, ok1 := assoc.RecordNeedle(a, sg, n)
				ws := prefixSpace([3][]byte{wide(c.album), wide(c.singer), wide(c.song)}, nil)
				wa, wsg, wn, ok2 := readSongPrefix(ws.source(), prefixStruct1, knownVersions["22.82"])
				want, ok3 := assoc.RecordNeedle(wa, wsg, wn)
				if !ok1 || !ok2 || !ok3 || !bytes.Equal(got, want) {
					t.Fatalf("窄字符的前缀 %q（%v），宽字符的 %q（%v、%v），want 逐字节相同", got, ok1, want, ok2, ok3)
				}
			})
		}
	}
}

// 窄字符版本读不出（22.16、22.22 各跑一遍）：堆上的串只读到一部分；内联串的 NUL 早于长度；长度为 0（内联，与 clear 之后容量
// 保留的堆上）；长度 769、999（ssoFromBuf 收、本读法不收）与 1000（ssoFromBuf 也不收）；串内有 NUL；不是合法的 UTF-8；
// 容量大于 15 而堆指针不大于 0x10000；24 字节的结构本身读不满；专辑、歌手、歌名任一段读不出而其余两段可读。
// 变异自证（实测）：去掉长度相等的校验，「NUL 早于长度」一例红；上界改为 1000，长度 769、999 两例红；去掉 UTF-8 校验，GBK 一例
// 红；短读照样交出读到的部分、同时去掉长度相等的校验，「只读到一部分」一例红（单去短读拒绝是等价变异：半截短于长度字段，
// 仍被长度校验挡下，各例都绿）；去掉长度下界，长度为 0 两例红；去掉串内 NUL 的校验，「串内有 NUL」一例红；不查 24 字节是否
// 读满，「结构本身读不满」一例红。
func TestReadSongPrefixNarrowUnreadable(t *testing.T) {
	good := [3][]byte{[]byte(kimi), []byte("supercell"), []byte(inst)}
	with := func(i int, s []byte) [3][]byte {
		g := good
		g[i] = s
		return g
	}
	field := func(i int, f []byte) [3][]byte {
		var fs [3][]byte
		fs[i] = f
		return fs
	}
	unreadable := ssoField(le32(0x7000_0000), 20, 31) // 堆指针指向读不到的地址
	for _, ver := range narrowVersions {
		for _, c := range []struct {
			name  string
			space func() *fakeSpace
		}{
			{"堆上的串只读到一部分", func() *fakeSpace {
				s := narrowSpace(ver, good, [3][]byte{})
				s.fail = func(addr uintptr, _ int) int {
					if addr == uintptr(heapSlot(0)) {
						return len("君の") // 读到的部分本身是合法的 UTF-8
					}
					return -1
				}
				return s
			}},
			{"内联串的 NUL 早于长度", func() *fakeSpace {
				return narrowSpace(ver, good, field(2, ssoField([]byte("Song\x00abcdefg"), 12, 15)))
			}},
			{"长度为 0（内联）", func() *fakeSpace { return narrowSpace(ver, good, field(2, ssoField(nil, 0, 15))) }},
			{"长度为 0（堆上，clear 之后容量保留）", func() *fakeSpace {
				return narrowSpace(ver, good, field(2, ssoField(le32(heapSlot(2)), 0, 47)))
			}},
			{"长度 769", func() *fakeSpace { return narrowSpace(ver, with(2, bytes.Repeat([]byte("a"), 769)), [3][]byte{}) }},
			{"长度 999", func() *fakeSpace { return narrowSpace(ver, with(2, bytes.Repeat([]byte("a"), 999)), [3][]byte{}) }},
			{"长度 1000", func() *fakeSpace { return narrowSpace(ver, with(2, bytes.Repeat([]byte("a"), 1000)), [3][]byte{}) }},
			{"串内有 NUL（堆上 10 字节）", func() *fakeSpace {
				return narrowSpace(ver, with(2, []byte("abc\x00efghij")), field(2, ssoField(le32(heapSlot(2)), 10, 31)))
			}},
			{"不是合法的 UTF-8（GBK）", func() *fakeSpace { return narrowSpace(ver, with(0, []byte(gbkWudi)), [3][]byte{}) }},
			{"容量大于 15 而堆指针不大于 0x10000", func() *fakeSpace {
				return narrowSpace(ver, good, field(2, ssoField(le32(narrowLow), 10, 31)))
			}},
			// 专辑是最后一段，取内联的串：缺的 4 字节恰是容量，补零后仍是一个合法的内联串，拒绝只能靠读满 24 字节的判定。
			{"24 字节的结构本身读不满", func() *fakeSpace {
				s := narrowSpace(ver, with(0, []byte("Album")), [3][]byte{})
				st := &s.regions[1]
				st.data = st.data[:len(st.data)-4]
				st.size = uintptr(len(st.data))
				return s
			}},
			{"专辑读不出", func() *fakeSpace { return narrowSpace(ver, good, field(0, unreadable)) }},
			{"歌手读不出", func() *fakeSpace { return narrowSpace(ver, good, field(1, unreadable)) }},
			{"歌名读不出", func() *fakeSpace { return narrowSpace(ver, good, field(2, unreadable)) }},
		} {
			t.Run(ver+"/"+c.name, func(t *testing.T) {
				if a, s, n, ok := readSongPrefix(c.space().source(), narrowStruct1, knownVersions[ver]); ok {
					t.Fatalf("读出了 %q、%q、%q，want 读不出", a, s, n)
				}
			})
		}
	}
}

// 窄字符串的上界（22.16、22.22 各跑一遍）：768 字节（256 个三字节的字符，与宽字符的 256 个码元对应）收，769 字节不收。
// 变异自证（实测）：上界改为 1000，769 字节一例红；窄字符版本也走宽字符一支，768 字节一例红。
func TestReadSongPrefixNarrowBoundary(t *testing.T) {
	at := strings.Repeat("長", assoc.RecordStrMax)
	for _, ver := range narrowVersions {
		for _, c := range []struct {
			album string
			ok    bool
		}{{at, true}, {at + "a", false}} {
			t.Run(fmt.Sprintf("%s/%d 字节", ver, len(c.album)), func(t *testing.T) {
				s := narrowSpace(ver, [3][]byte{[]byte(c.album), []byte("supercell"), []byte(inst)}, [3][]byte{})
				a, _, _, ok := readSongPrefix(s.source(), narrowStruct1, knownVersions[ver])
				if ok != c.ok || ok && a != c.album {
					t.Fatalf("专辑 %d 字节：ok %v、读到 %d 字节，want ok %v", len(c.album), ok, len(a), c.ok)
				}
			})
		}
	}
}
