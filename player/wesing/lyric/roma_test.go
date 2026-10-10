package lyric

// 本文件只测音译轨：在 LyricHost 的几条歌词轨里认出它，并按行号 + 行起始时间配到主歌词上。
// 用一份假内存按真机布局摆出各条轨（布局见 roma.go 头注释）。

import (
	"math"
	"testing"
	"unicode/utf16"
)

// fakeMem 是一份稀疏的假内存：没写过的地址读不出。maxRead 记下被要求读的最长一段。
type fakeMem struct {
	b       map[uint32]byte
	next    uint32 // 下一个可分配的「堆」地址
	maxRead uint32
}

func newFakeMem() *fakeMem { return &fakeMem{b: map[uint32]byte{}, next: 0x00500000} }

func (f *fakeMem) bytes(addr, n uint32) ([]byte, bool) {
	if n > f.maxRead {
		f.maxRead = n
	}
	if n > 1<<20 {
		return nil, false
	}
	out := make([]byte, n)
	for i := uint32(0); i < n; i++ {
		v, ok := f.b[addr+i]
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

func (f *fakeMem) u32(addr uint32) (uint32, bool) {
	b, ok := f.bytes(addr, 4)
	if !ok {
		return 0, false
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24, true
}

func (f *fakeMem) f32(addr uint32) (float32, bool) {
	v, ok := f.u32(addr)
	return math.Float32frombits(v), ok
}

func (f *fakeMem) put32(addr, v uint32) {
	for i := uint32(0); i < 4; i++ {
		f.b[addr+i] = byte(v >> (8 * i))
	}
}

func (f *fakeMem) alloc(n uint32) uint32 {
	a := f.next
	f.next += (n + 0x1F) &^ 0xF
	return a
}

// cstring 写一个 ATL 式 CString，返回字符指针：头部 16 字节（字符数在指针前 12 字节），其后是
// UTF-16 字符与结尾的 0。
func (f *fakeMem) cstring(s string) uint32 {
	u := utf16.Encode([]rune(s))
	hdr := f.alloc(16 + uint32(len(u)+1)*2)
	p := hdr + 16
	f.put32(hdr, 0x5EFD9000)
	f.put32(hdr+4, uint32(len(u)))
	f.put32(hdr+8, uint32(len(u))+2)
	f.put32(hdr+12, 1)
	for i, c := range u {
		f.b[p+uint32(i)*2] = byte(c)
		f.b[p+uint32(i)*2+1] = byte(c >> 8)
	}
	f.b[p+uint32(len(u))*2], f.b[p+uint32(len(u))*2+1] = 0, 0
	return p
}

// entry 写一个 LyricEntry（+0x00 行起始秒、+0x08/+0x0C 字级单元向量），每个词一个 CharElement。
func (f *fakeMem) entry(t float32, words ...string) uint32 {
	vec := f.alloc(uint32(len(words))*4 + 4)
	for i, w := range words {
		elem := f.alloc(0x20)
		f.put32(elem, f.cstring(w))
		f.put32(elem+4, math.Float32bits(t))
		f.put32(elem+8, math.Float32bits(0.3))
		f.put32(vec+uint32(i)*4, elem)
	}
	e := f.alloc(0x20)
	f.put32(e, math.Float32bits(t))
	f.put32(e+4, math.Float32bits(2))
	f.put32(e+8, vec)
	f.put32(e+0xC, vec+uint32(len(words))*4)
	f.put32(e+0x10, vec+uint32(len(words))*4)
	return e
}

// unit 是音译轨的一个字级单元：文本、起始秒、时长秒。
type unit struct {
	text       string
	start, dur float32
}

// entryTimed 同 entry，但每个单元各带自己的起始与时长。
func (f *fakeMem) entryTimed(t float32, units ...unit) uint32 {
	vec := f.alloc(uint32(len(units))*4 + 4)
	for i, u := range units {
		elem := f.alloc(0x20)
		f.put32(elem, f.cstring(u.text))
		f.put32(elem+4, math.Float32bits(u.start))
		f.put32(elem+8, math.Float32bits(u.dur))
		f.put32(vec+uint32(i)*4, elem)
	}
	e := f.alloc(0x20)
	f.put32(e, math.Float32bits(t))
	f.put32(e+4, math.Float32bits(2))
	f.put32(e+8, vec)
	f.put32(e+0xC, vec+uint32(len(units))*4)
	f.put32(e+0x10, vec+uint32(len(units))*4)
	return e
}

// track 在 addr 处写一条歌词轨：虚表、路径、行向量。spare 是 end 之后、容量之内的残留项。
func (f *fakeMem) track(addr, vtable uint32, path string, entries []uint32, spare ...uint32) {
	f.put32(addr, vtable)
	f.put32(addr+trackPathOff, f.cstring(path))
	if len(entries)+len(spare) == 0 {
		f.put32(addr+trackLinesOff, 0)
		f.put32(addr+trackLinesOff+4, 0)
		f.put32(addr+trackLinesOff+8, 0)
		return
	}
	vec := f.alloc(uint32(len(entries)+len(spare))*4 + 4)
	for i, e := range append(append([]uint32{}, entries...), spare...) {
		f.put32(vec+uint32(i)*4, e)
	}
	f.put32(addr+trackLinesOff, vec)
	f.put32(addr+trackLinesOff+4, vec+uint32(len(entries))*4)
	f.put32(addr+trackLinesOff+8, vec+uint32(len(entries)+len(spare))*4)
}

const (
	testMain   uint32 = 0x00200000 // 主歌词轨
	testVtable uint32 = 0x5EFD6D54
	// 真机上主歌词与音译是同一个目录下的两份文件，文件名只差 _Roma。
	resDir   = `C:\Users\u\AppData\Roaming\Tencent\WeSing\WeSingCache\WeSingDL\Res\000AkNWi0qab5P` + `\`
	mainPath = resDir + `000AkNWi0qab5P.qrc`
	romaPath = resDir + `000AkNWi0qab5P_Roma.qrc`
)

// mainLines 是 LoadLyrics 会给出的主歌词（只用到 Index 与 Time）。
func mainLines() []LyricLine {
	return []LyricLine{
		{Index: 0, Time: 1.22, Text: "Uh you wanna feel the vibe"},
		{Index: 1, Time: 6.019, Text: "저기 멀리 looks like tiny"},
		{Index: 2, Time: 9.917, Text: "지금부터 하나"},
	}
}

// romaEntries 是与 mainLines 逐行对应的音译行，照真机原样：纯英文行是单独一个空格，混排行保留英文词。
func romaEntries(f *fakeMem) []uint32 {
	return []uint32{
		f.entry(1.22, " "),
		f.entry(6.019, "jo gi  ", "mol li  ", "looks ", "like ", "tiny"),
		f.entry(9.917, "ji gem bu to  ", "ha na  "),
	}
}

// 真机布局（2026-10-09《Dreams Come True》）：轨 0 主歌词、轨 1 空、轨 2 音译。逐行配上，
// 词间空格原样保留、首尾去掉；只有一个空格的行没有音译。
//
// 变异自证：把 trackLine 末尾的 strings.TrimSpace 去掉，纯英文那行得到 " "、配上 3 行而红。
func TestLoadRomaAttachesLines(t *testing.T) {
	f := newFakeMem()
	f.track(testMain, testVtable, mainPath, nil)
	f.track(testMain+trackStride, testVtable, "", nil)
	f.track(testMain+2*trackStride, testVtable, romaPath, romaEntries(f))
	lines := mainLines()
	n, pending := loadRoma(f, testMain, lines)
	if n != 2 || pending {
		t.Fatalf("应配上 2 行、pending=false，got %d %v", n, pending)
	}
	want := []string{"", "jo gi  mol li  looks like tiny", "ji gem bu to  ha na"}
	for i, w := range want {
		if lines[i].RomaText != w {
			t.Errorf("第 %d 行音译 = %q，want %q", i, lines[i].RomaText, w)
		}
	}
	if n := len(lines[0].RomaDetailed.Words); n != 0 {
		t.Errorf("没有音译的行不应有逐字音译，got %d 个单元", n)
	}
}

// 逐字音译：音译轨自己的字级单元连同时间一起带出来。真机（2026-10-09《ホタルノヒカリ》「ひかり 燃えて
// いけ」）：主歌词「り 」一个单元，音译是「ri 」加一个单独的空格单元、时长对半分——单元与主歌词不一一
// 对应，文本不裁剪，拼起来去掉首尾空白就是 RomaText。
//
// 变异自证：把 loadRoma 里给 line.RomaDetailed 赋值的那一段删掉，本例没有逐字音译而红；把 trackLine 里
// 单元的 Text 换成 strings.TrimSpace(unit)，空格单元的文本变成空串而红。
func TestLoadRomaDetailedWords(t *testing.T) {
	f := newFakeMem()
	f.track(testMain, testVtable, mainPath, nil)
	f.track(testMain+trackStride, testVtable, "", nil)
	f.track(testMain+2*trackStride, testVtable, romaPath, []uint32{f.entryTimed(13.711,
		unit{"hi ", 13.711, 0.2}, unit{"ka ", 13.911, 0.49}, unit{"ri ", 14.401, 0.26},
		unit{"  ", 14.661, 0.26}, unit{"mo ", 14.921, 0.48})})
	lines := []LyricLine{{Index: 0, Time: 13.711, Text: "ひかり 燃"}}
	if n, _ := loadRoma(f, testMain, lines); n != 1 || lines[0].RomaText != "hi ka ri   mo" {
		t.Fatalf("前置：应配上这一行，got %d 行 %q", n, lines[0].RomaText)
	}
	d := lines[0].RomaDetailed
	if len(d.Words) != 5 || d.Words[3].Text != "  " || d.Words[4].Text != "mo " {
		t.Fatalf("应有 5 个单元、空格单元原样保留，got %+v", d.Words)
	}
	if w := d.Words[2]; w.Timestamp != 14.401 || w.Duration != 0.26 {
		t.Errorf("第 3 个单元的时间 = %v+%v，want 14.401+0.26", w.Timestamp, w.Duration)
	}
	if d.Timestamp != 13.711 || math.Abs(float64(d.Duration)-1.69) > 0.001 {
		t.Errorf("整行：timestamp=%v duration=%v，want 13.711 与 1.69（末单元终点 − 首单元起点）", d.Timestamp, d.Duration)
	}
}

// 字级时间轴不合法（起始回退、NaN、时长为负）的行：行级的 RomaText 照给，逐字不给——不推半截错位的逐字。
//
// 变异自证：把 trackLine 末尾的 `if !wordsOK { words = nil }` 删掉，三例都带出了错位的逐字而红。
func TestLoadRomaDetailedInvalidTiming(t *testing.T) {
	nan := float32(math.NaN())
	for name, units := range map[string][]unit{
		"起始回退":   {{"hi ", 13.7, 0.2}, {"ka ", 12.0, 0.3}},
		"起始 NaN": {{"hi ", 13.7, 0.2}, {"ka ", nan, 0.3}},
		"时长为负":   {{"hi ", 13.7, 0.2}, {"ka ", 13.9, -1}},
	} {
		f := newFakeMem()
		f.track(testMain, testVtable, mainPath, nil)
		f.track(testMain+trackStride, testVtable, romaPath, []uint32{f.entryTimed(13.7, units...)})
		lines := []LyricLine{{Index: 0, Time: 13.7}}
		n, _ := loadRoma(f, testMain, lines)
		if n != 1 || lines[0].RomaText != "hi ka" {
			t.Errorf("%s：行级音译应照给，got %d 行 %q", name, n, lines[0].RomaText)
		}
		if len(lines[0].RomaDetailed.Words) != 0 {
			t.Errorf("%s：时间轴不合法不应给逐字，got %+v", name, lines[0].RomaDetailed.Words)
		}
	}
}

// 认轨靠路径（_Roma.qrc 后缀不分大小写），不靠「第几条」：排在前面的别的轨（这里假设是一条翻译轨）
// 不能被当成音译。
//
// 变异自证：把 isRomaPathOf 改成恒真（同虚表的第一条就用），配上了「翻译」的文字而红。
func TestLoadRomaPicksTrackByPath(t *testing.T) {
	f := newFakeMem()
	f.track(testMain, testVtable, mainPath, nil)
	f.track(testMain+trackStride, testVtable, resDir+`000AkNWi0qab5P_Trans.qrc`,
		[]uint32{f.entry(1.22, "译文一"), f.entry(6.019, "译文二"), f.entry(9.917, "译文三")})
	f.track(testMain+2*trackStride, testVtable, resDir+`000AkNWi0qab5P_ROMA.QRC`, romaEntries(f))
	lines := mainLines()
	if n, _ := loadRoma(f, testMain, lines); n != 2 || lines[1].RomaText != "jo gi  mol li  looks like tiny" || lines[0].RomaText != "" {
		t.Fatalf("应只用路径以 _Roma.qrc 结尾的那条轨，got %d 行，%q / %q", n, lines[0].RomaText, lines[1].RomaText)
	}
}

// 音译轨得是这首歌的：换歌之后它会原封不动留着上一首的路径与行（2026-10-09 真机，《红日》切到不带
// 音译的《爱丫爱丫》，主歌词轨是新歌、音译轨还是《红日》的 113 行）。行号相同、起始时间也相同的行
// 会配上上一首的音译——这里两首歌的第 0 行都从 0 秒起。主歌词的路径读不出时同样不认。
//
// 变异自证：把 isRomaPathOf 里与 mainPath 的比对去掉（只看 _Roma.qrc 后缀），两例都配上了别的歌的
// "A no uta" 而红。
func TestLoadRomaRejectsOtherSongsTrack(t *testing.T) {
	stale := func(f *fakeMem) []uint32 { return []uint32{f.entry(0, "A no uta "), f.entry(6.5, "a ")} }
	cases := []struct {
		name  string
		build func(f *fakeMem)
	}{
		{"音译轨是别的歌的", func(f *fakeMem) {
			f.track(testMain, testVtable, `C:\x\BBBB\BBBB.qrc`, nil)
			f.track(testMain+2*trackStride, testVtable, `C:\x\AAAA\AAAA_Roma.qrc`, stale(f))
		}},
		{"主歌词的路径读不出", func(f *fakeMem) {
			f.track(testMain, testVtable, "", nil)
			f.put32(testMain+trackPathOff, 0x00700000) // 指向没映射的地址
			f.track(testMain+2*trackStride, testVtable, `C:\x\AAAA\AAAA_Roma.qrc`, stale(f))
		}},
	}
	for _, c := range cases {
		f := newFakeMem()
		f.track(testMain+trackStride, testVtable, "", nil)
		c.build(f)
		lines := []LyricLine{{Index: 0, Time: 0}, {Index: 1, Time: 9.9}}
		if n, pending := loadRoma(f, testMain, lines); n != 0 || pending || lines[0].RomaText != "" {
			t.Errorf("%s：不应配上、也不必等，got %d 行 pending=%v %q", c.name, n, pending, lines[0].RomaText)
		}
	}
}

// 主歌词轨后面紧跟的不是歌词轨（虚表不同）：到此为止，不再往后当结构去读——哪怕更后面恰好有一段
// 看起来像音译轨的内存。
//
// 变异自证：把 findRomaTrack 里虚表不同就返回改成 continue（跳过它接着找），用上了后面那段而红。
func TestLoadRomaStopsAtForeignObject(t *testing.T) {
	f := newFakeMem()
	f.track(testMain, testVtable, mainPath, nil)
	f.track(testMain+trackStride, 0x5E1B40A8, "", nil) // 别的对象
	f.track(testMain+2*trackStride, testVtable, romaPath, romaEntries(f))
	lines := mainLines()
	if n, pending := loadRoma(f, testMain, lines); n != 0 || pending || lines[1].RomaText != "" {
		t.Fatalf("虚表对不上之后不应再找，got %d 行 pending=%v %q", n, pending, lines[1].RomaText)
	}
}

// 按行号配、并要求行起始时间相同。主歌词的 Index 是它在自己那条轨里的行号，LoadLyrics 跳过读不出的
// 行时下标与行号不相等；音译某一行的时间对不上主歌词的同号行，这一行就不配。
//
// 变异自证：把 loadRoma 里按 Index 找行改成按下标（lines[j]），行号有空缺时后面的行都错开一位、
// 时间对不上，只配上 1 行而红；把起始时间的容差判断去掉，时间对不上的那行也配上而红。
func TestLoadRomaMatchesByIndexAndTime(t *testing.T) {
	f := newFakeMem()
	f.track(testMain, testVtable, mainPath, nil)
	f.track(testMain+trackStride, testVtable, "", nil)
	f.track(testMain+2*trackStride, testVtable, romaPath, []uint32{
		f.entry(1.22, "a "),
		f.entry(6.019, "b "),
		f.entry(9.917, "c "),
		f.entry(13.5, "d "), // 主歌词第 3 行在 12.963：差了半秒
	})
	lines := []LyricLine{ // 行号 1 被 LoadLyrics 跳过了
		{Index: 0, Time: 1.22},
		{Index: 2, Time: 9.917},
		{Index: 3, Time: 12.963},
	}
	n, _ := loadRoma(f, testMain, lines)
	if n != 2 || lines[0].RomaText != "a" || lines[1].RomaText != "c" || lines[2].RomaText != "" {
		t.Fatalf("应按行号配上 0、2 两行，时间对不上的第 3 行不配；got %d 行 %q %q %q",
			n, lines[0].RomaText, lines[1].RomaText, lines[2].RomaText)
	}
}

// 行向量的 end 在 +0x4C；+0x50 是容量末尾，end 之后、容量之内的是残留项（真机：69 行、容量 94）。
// 残留项即使行号、时间都对得上，也不是这首歌的。
//
// 变异自证：把 loadRoma 里读 end 的偏移从 trackLinesOff+4 改成 trackLinesOff+8，配上了残留的那行而红。
func TestLoadRomaUsesVectorEndNotCapacity(t *testing.T) {
	f := newFakeMem()
	f.track(testMain, testVtable, mainPath, nil)
	f.track(testMain+trackStride, testVtable, "", nil)
	f.track(testMain+2*trackStride, testVtable, romaPath,
		[]uint32{f.entry(1.22, "a "), f.entry(6.019, "b ")},
		f.entry(9.917, "上一首歌留下的 "))
	lines := mainLines()
	if n, _ := loadRoma(f, testMain, lines); n != 2 || lines[2].RomaText != "" {
		t.Fatalf("end 之后的残留项不应配上，got %d 行，第 2 行 %q", n, lines[2].RomaText)
	}
}

// pending 只报一种情形：这首歌的音译轨登记了、行向量却还是空的——只有它值得等一会儿再读
// （wesing.go 的 waitRoma，那段等待卡在宣告之前）。轨里有行而一行都没配上不是 pending：每行都只有
// 一个空格的纯英文歌、行都对不上的轨，再等也不会变。
//
// 变异自证：把 loadRoma 里行向量为空时的返回值从 (0, true) 改成 (0, false)，「登记了但还是空的」
// 一例而红；把末尾的 `return attached, false` 改成 `return attached, attached == 0`（配上 0 行就算
// pending，即改动前的行为），「每行都只有一个空格」「行都对不上」两例而红。
func TestLoadRomaPending(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		entries func(f *fakeMem) []uint32
		pending bool
	}{
		{"没有音译轨", "", nil, false},
		{"登记了但还是空的", romaPath, nil, true},
		{"每行都只有一个空格", romaPath, func(f *fakeMem) []uint32 {
			return []uint32{f.entry(1.22, " "), f.entry(6.019, " "), f.entry(9.917, " ")}
		}, false},
		{"行都对不上", romaPath, func(f *fakeMem) []uint32 {
			return []uint32{f.entry(3.3, "x "), f.entry(8.8, "y ")}
		}, false},
	}
	for _, c := range cases {
		f := newFakeMem()
		f.track(testMain, testVtable, mainPath, nil)
		f.track(testMain+trackStride, testVtable, "", nil)
		var entries []uint32
		if c.entries != nil {
			entries = c.entries(f)
		}
		f.track(testMain+2*trackStride, testVtable, c.path, entries)
		if n, pending := loadRoma(f, testMain, mainLines()); n != 0 || pending != c.pending {
			t.Errorf("%s：应为 (0, %v)，got (%d, %v)", c.name, c.pending, n, pending)
		}
	}
}

// 一行里有单元读不出，这一行就不配：跳过它把其余的拼起来是缺字的音译（"hi ri"、两个音节的逐字），
// 而歌词每首只加载一次，配上了就不会再重读。行级的 RomaText 与逐字的 RomaDetailed 一起不给。长度为 0
// 的空单元是读出来了、只是没有内容，不算读不出，它不占逐字的单元。
//
// 变异自证：trackLine 的循环里有三处 `return "", nil`，逐一改成 `continue`（跳过这个单元接着拼）——
// 元素指针不像指针那一处，「元素指针是空的」一例得到 "hi ri" 与两个音节而红；元素里的文本指针读不出
// 那一处，「元素读不出」一例而红；readCString 返回 false 那一处，「文本读不出」一例而红。把 readCString
// 里 n == 0 时的返回值改成 ("", false)，「空单元」一例整行没配上而红。
func TestLoadRomaDropsLineWithUnreadableUnit(t *testing.T) {
	cases := []struct {
		name      string
		corrupt   func(f *fakeMem, vec uint32)
		want      string
		wantWords int
	}{
		{"对照组", func(f *fakeMem, vec uint32) {}, "hi ka ri", 3},
		{"元素指针是空的", func(f *fakeMem, vec uint32) { f.put32(vec+4, 0) }, "", 0},
		{"元素读不出", func(f *fakeMem, vec uint32) { f.put32(vec+4, 0x00700000) }, "", 0}, // 指向没映射的地址
		{"文本读不出", func(f *fakeMem, vec uint32) {
			elem, _ := f.u32(vec + 4)
			f.put32(elem, 0x00700000)
		}, "", 0},
		{"空单元", func(f *fakeMem, vec uint32) {
			elem, _ := f.u32(vec + 4)
			f.put32(elem, f.cstring(""))
		}, "hi ri", 2},
	}
	for _, c := range cases {
		f := newFakeMem()
		f.track(testMain, testVtable, mainPath, nil)
		f.track(testMain+trackStride, testVtable, "", nil)
		e := f.entryTimed(13.7, unit{"hi ", 13.7, 0.2}, unit{"ka ", 13.9, 0.3}, unit{"ri ", 14.2, 0.3})
		vec, _ := f.u32(e + 0x08)
		c.corrupt(f, vec)
		f.track(testMain+2*trackStride, testVtable, romaPath, []uint32{e})
		lines := []LyricLine{{Index: 0, Time: 13.7}}
		loadRoma(f, testMain, lines)
		if got := len(lines[0].RomaDetailed.Words); lines[0].RomaText != c.want || got != c.wantWords {
			t.Errorf("%s：RomaText = %q、逐字 %d 个单元，want %q、%d 个", c.name, lines[0].RomaText, got, c.want, c.wantWords)
		}
	}
}

// CString 的字符数来自指针前的头部，是从别人进程里读来的：离谱的长度不能照着去读。读不出与
// 「读出来是空串」要分得清（第二个返回值）。
//
// 变异自证：把 readCString 里 `n > maxPathChars` 的上限去掉，按头部里的 0x7FFFFFFF 去读而红。
func TestReadCStringBounds(t *testing.T) {
	f := newFakeMem()
	good := f.cstring("jo gi  ")
	if got, ok := readCString(f, good); got != "jo gi  " || !ok {
		t.Fatalf("对照组：got %q %v", got, ok)
	}
	if got, ok := readCString(f, f.cstring("")); got != "" || !ok {
		t.Fatalf("长度为 0 的空串是读出来了：应为 (\"\", true)，got (%q, %v)", got, ok)
	}
	huge := f.cstring("x")
	f.put32(huge-cstringLenOffset, 0x7FFFFFFF)
	if got, ok := readCString(f, huge); got != "" || ok {
		t.Fatalf("长度离谱应返回 (\"\", false)，got %d 个字符 %v", len(got), ok)
	}
	if f.maxRead > maxPathChars*2 {
		t.Fatalf("不应照着离谱的长度去读（被要求读 %d 字节）", f.maxRead)
	}
	if got, ok := readCString(f, 0x00700000); got != "" || ok { // 没映射的地址
		t.Fatalf("读不出应返回 (\"\", false)，got %q %v", got, ok)
	}
	if got, ok := readCString(f, 0x1000); got != "" || ok { // 不像指针
		t.Fatalf("不像指针应返回 (\"\", false)，got %q %v", got, ok)
	}
}
