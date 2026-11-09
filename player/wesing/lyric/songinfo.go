package lyric

import (
	"encoding/json"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

// SongInfo 歌曲信息
type SongInfo struct {
	Mid    string // 歌曲 MID
	Name   string // 歌曲名
	Singer string // 歌手名
}

// FindSongInfo 按窗口歌名 expectedName 在进程内存里找歌曲信息记录（UTF-16 JSON 里的 "songname"），供 PCM 模式的
// 交叉核对（设计 §3.1）：只认歌名等于 expectedName 的记录，取地址最高的一条。匹配不到（含 expectedName 为空）返回
// 错误，不退回别的记录：#146 之前退回地址最高的有效记录，可能是上一首（#145 真机：「blue hour」读到了「High」的）。
func FindSongInfo(handle syscall.Handle, expectedName string) (SongInfo, error) {
	return findSongInfo(procMemory{handle}, procScan(handle), expectedName, false)
}

// FindSongInfoPreferMID 同 FindSongInfo，供兜底模式（宣告与补读）：同名的记录里取地址最高的、有 mid 的一条，都没有
// mid 才取地址最高的一条（设计 §3.1，§9 二测：内存里有前半段被覆盖、没有 mid 的残缺副本）。PCM 模式的核对不用它：
// 同名的旧记录会带着别的 mid 进入核对，把正确的解码器拒掉（兜底歌手设计审 R3）。
func FindSongInfoPreferMID(handle syscall.Handle, expectedName string) (SongInfo, error) {
	return findSongInfo(procMemory{handle}, procScan(handle), expectedName, true)
}

// findSongInfo 在可写区全扫一遍 "songname":"，从地址最高的命中往下解析，只认歌名等于 expectedName 的记录。
// preferMID 为假时第一条即结果；为真时第一条有 mid 的即结果，都没有 mid 取第一条。命中数不设上限（#163）。
func findSongInfo(m memory, scan scanFunc, expectedName string, preferMID bool) (SongInfo, error) {
	pattern, mask := stringToUTF16LEAOB(`"songname":"`)
	results := scan(pattern, mask)
	if len(results) == 0 {
		return SongInfo{}, i18n.Errorf("songname field not found")
	}
	var best SongInfo
	found := false
	for i := len(results) - 1; i >= 0; i-- {
		info, ok := tryParseFromSongNameAddr(m, results[i])
		if !ok || info.Name != expectedName {
			continue
		}
		if !found {
			best, found = info, true
		}
		if !preferMID || info.Mid != "" {
			best = info
			break
		}
	}
	if !found {
		return SongInfo{}, i18n.Errorf("Found %d songname matches but none named %q", len(results), expectedName)
	}
	return best, nil
}

// tryParseFromSongNameAddr 解析 "songname":" 命中 addr 处的记录：向后读 2048 字节取歌名与歌手，向前读 200 字节取 mid。
// 歌手只在同一个对象里取（设计 §3.2，§9 二测）："singername":" 须在歌名之后的第一个 } 之前，没有就歌手为空、记录
// 仍有效。#146 之前在读到的 2 KB 里找，越过对象结尾取到别的记录的歌手（Shivers 读成了林俊杰），找不到则整条作废。
func tryParseFromSongNameAddr(m memory, addr uint32) (SongInfo, bool) {
	buf, ok := m.bytes(addr+24, 2048)
	if !ok {
		return SongInfo{}, false
	}

	text := utf16LEBytesToString(buf)

	// 歌名、歌手按 JSON 规则解码（jsonString）：转义的引号不是结尾（#146 代码审）。长度上限按原文计。
	name, nameEndIdx, ok := jsonString(text)
	if !ok || nameEndIdx <= 0 || nameEndIdx > 200 {
		return SongInfo{}, false
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > 50 {
		return SongInfo{}, false
	}

	// 从歌名的结尾引号之后找：歌名里的 } 不算对象结尾。值为空也有效；有值时找不到结尾引号、转义不合法、或超过 100
	// 字节，整条不合格。
	singer := ""
	after := text[nameEndIdx+1:]
	const singerKey = `"singername":"`
	if i := strings.Index(after, singerKey); i >= 0 {
		if end := strings.IndexByte(after, '}'); end < 0 || i < end {
			s, singerEndIdx, ok := jsonString(after[i+len(singerKey):])
			if !ok || singerEndIdx > 100 {
				return SongInfo{}, false
			}
			singer = s
		}
	}

	// 提取 MID：向前读取找到 "mid":"..." 字段
	mid := ""
	if addr > 200 {
		if prevBuf, ok := m.bytes(addr-200, 200); ok {
			mid = midBefore(utf16LEBytesToString(prevBuf))
		}
	}

	return SongInfo{Mid: mid, Name: name, Singer: singer}, true
}

// midBefore 在 "songname" 之前的文本 prev 里取同一个对象的 mid：最后一个 "mid":" 之后的值。从那里到 prev 末尾
// 不能有 {、} 或 NUL，否则 mid 属于前面别的记录，不认（#146 审查：兜底模式按它查封面）。值的长度上限 20。
func midBefore(prev string) string {
	const key = `"mid":"`
	i := strings.LastIndex(prev, key)
	if i < 0 {
		return ""
	}
	rest := prev[i+len(key):]
	if strings.ContainsAny(rest, "{}\x00") {
		return ""
	}
	if end := strings.IndexByte(rest, '"'); end > 0 && end <= 20 {
		return rest[:end]
	}
	return ""
}

// ===== 按 mid 查歌曲信息与专辑（#146，设计见 doc/design/wesing-cover-by-mid-design.md）=====
//
// FindSongInfo 在 #146 之前匹配不到窗口歌名时退回地址最高的记录，可能是上一首；原有的 FindCoverURL 有两层不以 mid
// 为依据（设计 §1，已删除）。这里只认以 mid 为锚的记录。按 mid 查歌曲信息在 PCM 模式取歌曲信息，mid 取自伴奏解码器
// 的路径（PCMClock.MID）；兜底模式只用它补歌手（设计 §3.2）。专辑两种模式都用，兜底模式的 mid 取自窗口歌名匹配上的
// 记录（设计 §3.4）。

const (
	midLen          = 14   // 歌曲 mid 与专辑 mid 的长度
	byMIDMaxHits    = 64   // 每种形式从地址最高的命中往下至多看几处（设计 §3.2，设计审 F3）
	songObjReadLen  = 1024 // 歌曲信息每处命中向后至多读的字节数
	albumReadBack   = 64   // 专辑 mid 每处命中向前至多读的字节数（设计 §3.4）
	songNameMaxRune = 50   // 歌名长度上限，同 tryParseFromSongNameAddr
	singerMaxBytes  = 100  // 歌手长度上限（UTF-8 字节），同 tryParseFromSongNameAddr
	albumLogKey     = "m_strAlbumID = "
	albumRespKey    = `"albummid":"`
)

// scanFunc 在可写区全扫一遍 pattern，按地址升序返回命中（同 proc.AOBScan）。测试里换成扫假内存。
type scanFunc func(pattern []byte, mask []bool) []uint32

func procScan(handle syscall.Handle) scanFunc {
	return func(pattern []byte, mask []bool) []uint32 {
		return proc.AOBScan(handle, pattern, mask, proc.EnumWritableRegions(handle))
	}
}

// validMID 报告 s 是否为 14 位字母数字（歌曲 mid、专辑 mid 的形状）。
func validMID(s string) bool {
	if len(s) != midLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') {
			return false
		}
	}
	return true
}

// FindSongInfoByMID 按歌曲 mid 查开唱时留下的 StartKSong 记录（设计 §3.2），返回其歌名与歌手。
func FindSongInfoByMID(handle syscall.Handle, mid string) (SongInfo, bool) {
	return songInfoByMID(procMemory{handle}, procScan(handle), mid)
}

// songInfoByMID 在可写区找 UTF-16 的 "mid":"<mid>"，从地址最高的命中往下至多看 64 处，第一处合格的即结果。
// 只匹配 "mid":" 紧跟 mid 的写法：歌单类记录的引号被转义（\"mid\":\"…），不认，其字段内容与 StartKSong 相同，
// 漏掉不影响结果。每处向后读 1 KB，读不出的这一处不合格；一次查找的额外读取量因此不超过 64 KB。
func songInfoByMID(m memory, scan scanFunc, mid string) (SongInfo, bool) {
	if !validMID(mid) {
		return SongInfo{}, false
	}
	key, mask := stringToUTF16LEAOB(`"mid":"` + mid + `"`)
	hits := scan(key, mask)
	for i, n := len(hits)-1, 0; i >= 0 && n < byMIDMaxHits; i, n = i-1, n+1 {
		b, ok := m.bytes(hits[i]+uint32(len(key)), songObjReadLen)
		if !ok {
			continue
		}
		if name, singer, ok := parseSongObject(utf16LEBytesToString(b)); ok {
			return SongInfo{Mid: mid, Name: name, Singer: singer}, true
		}
	}
	return SongInfo{}, false
}

// parseSongObject 在 text（"mid":"<mid>" 之后的内容）里取同一个 JSON 对象的 songname 与 singername：读到
// 第一个 } 为止，两个键都要有，歌名非空。长度上限同 tryParseFromSongNameAddr；歌手可以为空。
func parseSongObject(text string) (name, singer string, ok bool) {
	end := strings.IndexByte(text, '}')
	if end < 0 {
		return "", "", false
	}
	obj := text[:end]
	name, ok = jsonStringField(obj, `"songname":"`)
	if !ok || name == "" || utf8.RuneCountInString(name) > songNameMaxRune {
		return "", "", false
	}
	singer, ok = jsonStringField(obj, `"singername":"`)
	if !ok || len(singer) > singerMaxBytes {
		return "", "", false
	}
	return name, singer, true
}

// jsonStringField 取 obj 里 key（含开头的引号与冒号、值的开引号）之后的 JSON 字符串值，解码见 jsonString。
func jsonStringField(obj, key string) (string, bool) {
	i := strings.Index(obj, key)
	if i < 0 {
		return "", false
	}
	v, _, ok := jsonString(obj[i+len(key):])
	return v, ok
}

// jsonString 解码 rest 开头的 JSON 字符串值（开引号在 rest 之前），返回值与结尾引号在 rest 里的字节下标：结尾是第一个
// 未被反斜杠转义的引号，按 JSON 规则解码转义（#146 代码审：歌名里的 \" 不能当成结尾）。没有结尾或转义不合法时不认。
func jsonString(rest string) (string, int, bool) {
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '\\':
			j++ // 跳过被转义的字符
		case '"':
			var v string
			if json.Unmarshal([]byte(`"`+rest[:j]+`"`), &v) != nil {
				return "", 0, false
			}
			return v, j, true
		}
	}
	return "", 0, false
}

// FindAlbumMID 按歌曲 mid 查它的专辑 mid（设计 §3.4），找不到返回空串。
func FindAlbumMID(handle syscall.Handle, mid string) string {
	return albumMID(procMemory{handle}, procScan(handle), mid)
}

// albumMID 依次查两种以歌曲 mid 为锚的形式，各在可写区全扫一遍，值须为 14 位字母数字：
//  1. 日志行（UTF-16）「…, m_strAlbumID = <A>, strMid = <mid>」：向前至多 64 字节，取同一行里 m_strAlbumID = 之后
//     到 , strMid 之前的值；
//  2. 回包（UTF-8）「{…"albummid":"<A>","ksongmid":"<mid>"}」：向前至多 64 字节，在同一个对象里取 "albummid":"
//     之后的值。
//
// 日志行查到即返回，不再扫回包：两处都有而不相等时取日志行的（设计 §3.4），所以回包只在日志行查不到时才有用。
func albumMID(m memory, scan scanFunc, mid string) string {
	if !validMID(mid) {
		return ""
	}
	key, mask := stringToUTF16LEAOB(", strMid = " + mid)
	if a := albumBefore(m, scan(key, mask), func(b []byte) string {
		back := utf16LEBytesToString(b)
		i := strings.LastIndex(back, albumLogKey)
		if i < 0 {
			return ""
		}
		// 命中紧跟在值后面：值之后到命中之间不能有别的字符，同一行由此保证。
		return back[i+len(albumLogKey):]
	}); a != "" {
		return a
	}
	resp := []byte(`"ksongmid":"` + mid + `"`)
	return albumBefore(m, scan(resp, allTrue(len(resp))), func(b []byte) string {
		back := string(b)
		i := strings.LastIndex(back, albumRespKey)
		if i < 0 {
			return ""
		}
		rest := back[i+len(albumRespKey):]
		if strings.ContainsAny(rest, "{}") { // 跨了对象
			return ""
		}
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			return ""
		}
		return rest[:j]
	})
}

// albumBefore 从地址最高的命中往下至多看 64 处，每处读命中之前至多 64 字节交给 extract，第一个合格的值即结果。
func albumBefore(m memory, hits []uint32, extract func(back []byte) string) string {
	for i, n := len(hits)-1, 0; i >= 0 && n < byMIDMaxHits; i, n = i-1, n+1 {
		start := hits[i] - min(hits[i], albumReadBack)
		b, ok := m.bytes(start, hits[i]-start)
		if !ok {
			continue
		}
		if a := extract(b); validMID(a) {
			return a
		}
	}
	return ""
}

// AlbumCoverURL 由专辑 mid 构造封面地址（设计 §3.4）：…/mid_album_800/<A[12]>/<A[13]>/<A>.jpg。专辑 mid 不合格
// 时返回空串。
func AlbumCoverURL(albumMID string) string {
	if !validMID(albumMID) {
		return ""
	}
	return "http://imgcache.qq.com/music/photo/mid_album_800/" + albumMID[12:13] + "/" + albumMID[13:] + "/" + albumMID + ".jpg"
}

func allTrue(n int) []bool {
	mask := make([]bool, n)
	for i := range mask {
		mask[i] = true
	}
	return mask
}

// stringToUTF16LEAOB 将 ASCII 字符串转换为 UTF-16LE 的 AOB 字节模式
func stringToUTF16LEAOB(s string) ([]byte, []bool) {
	pattern := make([]byte, len(s)*2)
	mask := make([]bool, len(s)*2)
	for i, c := range s {
		pattern[i*2] = byte(c)
		pattern[i*2+1] = 0x00
		mask[i*2] = true
		mask[i*2+1] = true
	}
	return pattern, mask
}

func utf16LEBytesToString(buf []byte) string {
	chars := make([]uint16, 0, len(buf)/2)
	for i := 0; i+1 < len(buf); i += 2 {
		val := uint16(buf[i]) | uint16(buf[i+1])<<8
		chars = append(chars, val)
	}
	return string(utf16.Decode(chars))
}
