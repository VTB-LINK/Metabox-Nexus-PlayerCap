package lyric

// 音译（发音标注：韩文、日文等歌的罗马音）轨。**纯内存读取**，与主歌词同一条定位链、同一种结构，
// 不读磁盘上的缓存文件、不发任何网络请求。
//
// LyricHost 里并排放着几条歌词轨，每条是一个 0x70 字节的子结构、虚表相同（2026-10-09 真机，
// 一首韩英混合、两首日文）：
//
//	host+0x0C  轨 0  主歌词（FindLyricHost 返回的 subStructAddr）
//	host+0x7C  轨 1  三首歌上都是空的（没有可读的行，路径是空串）
//	host+0xEC  轨 2  音译
//
// 每条轨：
//
//	+0x00         虚表
//	+0x04         CString：这条轨的歌词文件路径。主歌词是 …\<mid>\<mid>.qrc，音译是 …\<mid>\<mid>_Roma.qrc
//	+0x48 / +0x4C vector<LyricEntry*> 的 begin / end（+0x50 是容量末尾，不是 end）
//
// LyricEntry / CharElement 的布局与主歌词相同（见 reader.go）。音译轨与主歌词逐行对应：行数相同、
// 行起始时间逐位相等（行内的字级单元并不一一对应：一个汉字在音译轨里可以拆成几个音节，各单元带着
// 自己的起始与时长，见 trackLine）；混排行里的英文词原样保留——「저기 멀리 looks like tiny」对
// 「jo gi  mol li  looks like tiny」；纯英文行有的是单独一个空格、有的照抄原文（「Yell」对「Yell」）。
//
// 这条轨在不在，与全民K歌界面上的音译开关无关：关掉开关后它照样在、行数不变（同日实测）。
//
// 认轨靠路径，不靠「第几条」：轨的次序是观察到的、不是约定，文件名是全民K歌自己起的。路径要以
// _Roma.qrc 结尾，并且去掉 _Roma 后与主歌词轨的路径相同——音译轨会是上一首歌留下的（见 findRomaTrack）。
// CString 是 ATL 式的：指针指向字符，字符数在指针前 12 字节处。

import (
	"math"
	"strings"
	"syscall"
	"unicode/utf16"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

const (
	trackStride      = 0x70 // 相邻两条歌词轨的间距
	maxTracks        = 4    // 最多看几条轨（真机见过 3 条）
	trackPathOff     = 0x04
	trackLinesOff    = 0x48 // vector begin；end 在其后 4 字节
	romaPathSuffix   = "_roma.qrc"
	mainPathExt      = ".qrc" // 音译文件名去掉 _Roma 之后剩下的扩展名，即主歌词文件的扩展名
	maxPathChars     = 520
	maxTrackLines    = 1000
	maxLineWords     = 500
	romaTimeEpsilon  = 0.02 // 音译行与主歌词行起始时间的容差（秒）；真机上两者逐位相等
	minHeapPtr       = 0x00100000
	cstringLenOffset = 12 // CString 的字符数在字符指针前 12 字节
)

// memory 是读目标进程内存的最小接口：生产实现是 procMemory，测试喂一份假内存。
type memory interface {
	u32(addr uint32) (uint32, bool)
	f32(addr uint32) (float32, bool)
	bytes(addr, n uint32) ([]byte, bool)
}

type procMemory struct{ h syscall.Handle }

func (m procMemory) u32(addr uint32) (uint32, bool) {
	v, err := proc.ReadUint32(m.h, addr)
	return v, err == nil
}

func (m procMemory) f32(addr uint32) (float32, bool) {
	v, err := proc.ReadFloat32(m.h, addr)
	return v, err == nil
}

func (m procMemory) bytes(addr, n uint32) ([]byte, bool) {
	b, err := proc.ReadBytes(m.h, addr, n)
	return b, err == nil
}

// LoadRoma 给 lines 逐行填上音译（RomaText，及其逐字时间轴 RomaDetailed），返回填上的行数与 pending。
//
// subStructAddr 是 FindLyricHost 返回的主歌词子结构地址，lines 是 LoadLyrics 的结果。任何一步读不出、
// 对不上都只是少填——音译是纯增量，绝不影响主歌词。
//
// pending 为真只有一种情形：这首歌的音译轨登记了（路径对得上主歌词），行向量却还是空的——行也许
// 稍后才解析出来，值得过一会儿再读。其余都不是 pending，再等也不会变：没有音译轨、音译轨是别的歌的、
// 轨里有行而一行都没配上（每行都只有一个空格的纯英文歌就是这样）。
func LoadRoma(handle syscall.Handle, subStructAddr uint32, lines []LyricLine) (attached int, pending bool) {
	return loadRoma(procMemory{handle}, subStructAddr, lines)
}

func loadRoma(m memory, mainTrack uint32, lines []LyricLine) (attached int, pending bool) {
	track, ok := findRomaTrack(m, mainTrack)
	if !ok {
		return 0, false
	}
	begin, ok1 := m.u32(track + trackLinesOff)
	end, ok2 := m.u32(track + trackLinesOff + 4)
	if !ok1 || !ok2 || begin < minHeapPtr || end <= begin {
		return 0, true
	}
	n := (end - begin) / 4
	if n > maxTrackLines {
		return 0, false
	}
	// 主歌词的 Index 是它在自己那条轨里的行号（LoadLyrics 跳过读不出的行，下标与行号不一定相等）。
	byIndex := make(map[int]*LyricLine, len(lines))
	for i := range lines {
		byIndex[lines[i].Index] = &lines[i]
	}
	for j := uint32(0); j < n; j++ {
		line := byIndex[int(j)]
		if line == nil {
			continue
		}
		entry, ok := m.u32(begin + j*4)
		if !ok || entry < minHeapPtr {
			continue
		}
		// 同一行号、起始时间也相同，才是这一行的音译：两条轨来自同一首歌的两份歌词文件，对不上
		// 说明读到的不是我们以为的东西，这一行宁可没有。
		t, ok := m.f32(entry)
		if !ok || !IsPlausiblePlayTime(t) || math.Abs(float64(t-line.Time)) > romaTimeEpsilon {
			continue
		}
		text, words := trackLine(m, entry)
		if text == "" {
			continue
		}
		line.RomaText = text
		attached++
		// 逐字：整行的字级时间轴合法才附上（判据同主歌词的逐字），否则只留行级的 RomaText。
		if len(words) > 0 {
			last := words[len(words)-1]
			line.RomaDetailed = player.LyricTextDetailed{
				Timestamp: line.Time,
				Duration:  last.Timestamp + last.Duration - words[0].Timestamp,
				Words:     words,
			}
		}
	}
	return attached, false
}

// findRomaTrack 在主歌词轨之后的几条轨里找**这首歌的**音译轨：虚表与主歌词轨相同（确实是一条歌词轨）、
// 路径是主歌词那份文件对应的 _Roma.qrc（见 isRomaPathOf）。虚表对不上就停——再往后已经不是歌词轨，
// 不能当结构去读。
//
// 为什么要对主歌词的路径：音译轨会是上一首歌的（AGENTS §7.1）。2026-10-09 真机，从带音译的《红日》
// 切到不带音译的《爱丫爱丫》，主歌词轨换成了新歌，音译轨原封不动留着《红日》的路径与 113 行。只看
// 后缀会认下它；行号相同、起始时间差在容差内的行就配上了上一首的音译（不少歌的第 0 行都从 0 秒起），
// 随 all_lyrics 留到整首结束。那一次没配上只是因为两首歌没有哪一行的起始时间碰巧相同。
func findRomaTrack(m memory, mainTrack uint32) (uint32, bool) {
	vtable, ok := m.u32(mainTrack)
	if !ok || vtable < minHeapPtr {
		return 0, false
	}
	// 主歌词的路径读不出时 mainPath 是空串，下面哪条轨都对不上：宁可没有音译。
	//
	// 已知的边界，没有为它加等待：主歌词轨重新加载的瞬间路径会是空串（同日真机，同一首歌重播的
	// 那一下，约 100ms 后恢复），那一刻来认轨就认不到，这一首没有音译。重播不重新加载歌词；切歌时
	// 同日五次实测都是主歌词轨的路径换好之后 0.5~0.9 秒才读到这里，没有撞上过。
	mainPathPtr, _ := m.u32(mainTrack + trackPathOff)
	mainPath, _ := readCString(m, mainPathPtr)
	for i := uint32(1); i < maxTracks; i++ {
		track := mainTrack + i*trackStride
		if vt, ok := m.u32(track); !ok || vt != vtable {
			return 0, false
		}
		pathPtr, ok := m.u32(track + trackPathOff)
		if !ok {
			continue
		}
		if path, _ := readCString(m, pathPtr); isRomaPathOf(path, mainPath) {
			return track, true
		}
	}
	return 0, false
}

// isRomaPathOf 报告 path 是不是 mainPath 那首歌的音译文件：以 _Roma.qrc 结尾，并且把 _Roma 去掉后
// 与 mainPath 相同，都不分大小写（真机：…\<mid>\<mid>.qrc 对 …\<mid>\<mid>_Roma.qrc）。
func isRomaPathOf(path, mainPath string) bool {
	cut := len(path) - len(romaPathSuffix)
	if cut <= 0 || !strings.EqualFold(path[cut:], romaPathSuffix) {
		return false
	}
	return strings.EqualFold(path[:cut]+mainPathExt, mainPath)
}

// trackLine 读一行的各个字级单元（CharElement：+0x00 → 文本，+0x04 起始秒，+0x08 时长秒）。
//
// text 是各单元的文本拼起来、去掉首尾空白——与 QQ 音乐同源的音译格式一致，词间的空格是数据的
// 一部分，原样保留。words 是逐单元的时间轴，文本不裁剪（空格单元也在：拼起来就是 text 去空白前
// 的样子）；任一单元的时间不合法（判据同 LoadLyrics 的逐字：起始合理、行内不回退、时长在 [0,100]）
// 就整行不给，返回 nil——不推半截错位的逐字。
//
// 任何一个单元读不出（元素指针、文本指针、文本本身），text 与 words 都不给、这一行不配：跳过它把
// 其余的拼起来得到的是缺字的音译（三个单元中间那个读不出 → "hi ri"、两个音节），而歌词每首只加载
// 一次，配上了就不会再重读。长度为 0 的空单元是读出来了、只是没有内容，跳过它不算缺字。
func trackLine(m memory, entry uint32) (text string, words []player.LyricTextDetailedWord) {
	begin, ok1 := m.u32(entry + 0x08)
	end, ok2 := m.u32(entry + 0x0C)
	if !ok1 || !ok2 || begin < minHeapPtr || end <= begin {
		return "", nil
	}
	n := (end - begin) / 4
	if n > maxLineWords {
		return "", nil
	}
	var sb strings.Builder
	wordsOK := true
	var lastStart float32 = -1
	for c := uint32(0); c < n; c++ {
		elem, ok := m.u32(begin + c*4)
		if !ok || elem < minHeapPtr {
			return "", nil
		}
		textPtr, ok := m.u32(elem)
		if !ok {
			return "", nil
		}
		unit, ok := readCString(m, textPtr)
		if !ok {
			return "", nil
		}
		if unit == "" {
			continue
		}
		sb.WriteString(unit)
		start, ok1 := m.f32(elem + 0x04)
		dur, ok2 := m.f32(elem + 0x08)
		if !ok1 || !ok2 || !IsPlausiblePlayTime(start) || start < lastStart-0.001 || !(dur >= 0 && dur <= 100) {
			wordsOK = false
		}
		lastStart = start
		words = append(words, player.LyricTextDetailedWord{Timestamp: start, Duration: dur, Text: unit})
	}
	if !wordsOK {
		words = nil
	}
	return strings.TrimSpace(sb.String()), words
}

// readCString 读一个 CString 的内容：字符数取自指针前的头部。读不出、长度离谱返回 ("", false)；
// 长度为 0 的空串是读出来了，返回 ("", true)——调用方要分得清「没有内容」与「没读到」。
func readCString(m memory, ptr uint32) (string, bool) {
	if ptr < minHeapPtr {
		return "", false
	}
	n, ok := m.u32(ptr - cstringLenOffset)
	if !ok || n > maxPathChars {
		return "", false
	}
	if n == 0 {
		return "", true
	}
	b, ok := m.bytes(ptr, n*2)
	if !ok || uint32(len(b)) < n*2 {
		return "", false
	}
	u := make([]uint16, 0, n)
	for i := uint32(0); i < n; i++ {
		c := uint16(b[i*2]) | uint16(b[i*2+1])<<8
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u)), true
}
