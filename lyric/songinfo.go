package lyric

import (
	"fmt"
	"strings"
	"syscall"
	"unicode/utf16"
	"Metabox-Nexus-WesingCap/proc"
)

// SongInfo 歌曲信息
type SongInfo struct {
	Name   string // 歌曲名
	Singer string // 歌手名
}

// FindSongInfo 从进程内存中搜索当前歌曲的 JSON 信息
// expectedName 是从窗口标题获取的歌名，用于从多个缓存中匹配当前歌曲
func FindSongInfo(handle syscall.Handle, expectedName string) (SongInfo, error) {
	// 搜索 UTF-16LE 编码的 "songname":"
	pattern, mask := proc.ParseAOBPattern(
		"22 00 73 00 6F 00 6E 00 67 00 6E 00 61 00 6D 00 65 00 22 00 3A 00 22 00")

	regions := proc.EnumWritableRegions(handle)
	results := proc.AOBScan(handle, pattern, mask, regions)

	if len(results) == 0 {
		return SongInfo{}, fmt.Errorf("未找到 songname 字段")
	}

	// 优先匹配窗口标题对应的歌名
	if expectedName != "" {
		for _, addr := range results {
			info, ok := tryParseFromSongNameAddr(handle, addr)
			if ok && info.Name == expectedName {
				return info, nil
			}
		}
	}

	// 没有匹配到预期歌名，返回第一个有效结果
	for _, addr := range results {
		info, ok := tryParseFromSongNameAddr(handle, addr)
		if ok {
			return info, nil
		}
	}

	return SongInfo{}, fmt.Errorf("找到 %d 个 songname 匹配但无有效歌曲信息", len(results))
}

func tryParseFromSongNameAddr(handle syscall.Handle, addr uint32) (SongInfo, bool) {
	buf, err := proc.ReadBytes(handle, addr+24, 2048)
	if err != nil {
		return SongInfo{}, false
	}

	text := utf16LEBytesToString(buf)

	nameEndIdx := strings.Index(text, `"`)
	if nameEndIdx <= 0 || nameEndIdx > 200 {
		return SongInfo{}, false
	}
	name := text[:nameEndIdx]
	if len([]rune(name)) < 1 || len([]rune(name)) > 50 {
		return SongInfo{}, false
	}

	singerKey := `"singername":"`
	singerIdx := strings.Index(text, singerKey)
	if singerIdx < 0 {
		return SongInfo{}, false
	}

	rest := text[singerIdx+len(singerKey):]
	singerEndIdx := strings.Index(rest, `"`)
	if singerEndIdx <= 0 || singerEndIdx > 100 {
		return SongInfo{}, false
	}
	singer := rest[:singerEndIdx]

	return SongInfo{Name: name, Singer: singer}, true
}

func utf16LEBytesToString(buf []byte) string {
	chars := make([]uint16, 0, len(buf)/2)
	for i := 0; i+1 < len(buf); i += 2 {
		val := uint16(buf[i]) | uint16(buf[i+1])<<8
		chars = append(chars, val)
	}
	return string(utf16.Decode(chars))
}
