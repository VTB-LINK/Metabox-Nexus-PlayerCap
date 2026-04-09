package lyric

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

// SongInfo 歌曲信息
type SongInfo struct {
	Mid    string // 歌曲 MID
	Name   string // 歌曲名
	Singer string // 歌手名
	Cover  string // 封面 base64（data:image/jpeg;base64,...）
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

	// 优先匹配窗口标题对应的歌名（如果有多个匹配，取最高地址的）
	if expectedName != "" {
		var bestInfo SongInfo
		var bestAddr uint32
		for _, addr := range results {
			info, ok := tryParseFromSongNameAddr(handle, addr)
			if ok && info.Name == expectedName && addr > bestAddr {
				bestInfo = info
				bestAddr = addr
			}
		}
		if bestAddr > 0 {
			return bestInfo, nil
		}
	}

	// 没有匹配到预期歌名，返回最高地址的有效结果（最新分配的数据）
	for i := len(results) - 1; i >= 0; i-- {
		info, ok := tryParseFromSongNameAddr(handle, results[i])
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

	// 提取 MID：向前读取找到 "mid":"..." 字段
	mid := ""
	if addr > 200 {
		prevBuf, err := proc.ReadBytes(handle, addr-200, 200)
		if err == nil {
			prevText := utf16LEBytesToString(prevBuf)
			midKey := `"mid":"`
			if midIdx := strings.LastIndex(prevText, midKey); midIdx >= 0 {
				midRest := prevText[midIdx+len(midKey):]
				if endIdx := strings.Index(midRest, `"`); endIdx > 0 && endIdx <= 20 {
					mid = midRest[:endIdx]
				}
			}
		}
	}

	return SongInfo{Mid: mid, Name: name, Singer: singer}, true
}

// FindCoverURL 从进程内存中搜索当前歌曲的封面 URL
// songMID: 当前歌曲的 MID（如 "003msPI80HftbW"），用于精确匹配避免拿到其他歌的封面
// 策略: 搜索 "{songMID}.jpg" 的 UTF-16LE，在附近查找完整的 imgcache URL
func FindCoverURL(handle syscall.Handle, songMID string) string {
	if songMID == "" {
		return ""
	}

	regions := proc.EnumWritableRegions(handle)

	// 构造 "{songMID}.jpg" 的 UTF-16LE AOB 模式
	jpgSuffix := songMID + ".jpg"
	jpgPattern, jpgMask := stringToUTF16LEAOB(jpgSuffix)
	results := proc.AOBScan(handle, jpgPattern, jpgMask, regions)

	for _, addr := range results {
		// 向前读取更多内容，查找 "http" 开头的 URL
		// 典型格式: http://imgcache.qq.com/music/photo/mid_album_500/X/Y/{albumMID}.jpg
		// songMID.jpg 出现在本地路径中（只做定位用），封面 URL 通常在附近内存区域
		// 先检查这个 .jpg 是不是在一个 URL 里（向前最多 300 字节）
		url := tryExtractURLBefore(handle, addr, 300)
		if url != "" && strings.Contains(url, "imgcache.qq.com") {
			return url
		}
	}

	// 备选策略: 搜索 "mid_album_500/" 模式，找到所有封面 URL，
	// 然后看哪个 URL 在距离 songMID 最近的内存区域
	midAlbumPattern, midAlbumMask := proc.ParseAOBPattern(
		"6D 00 69 00 64 00 5F 00 61 00 6C 00 62 00 75 00 6D 00 5F 00 35 00 30 00 30 00 2F 00")
	albumResults := proc.AOBScan(handle, midAlbumPattern, midAlbumMask, regions)

	// 同时搜索 songMID 出现的位置
	midPattern, midMask := stringToUTF16LEAOB(songMID)
	midResults := proc.AOBScan(handle, midPattern, midMask, regions)

	// 找到离 songMID 最近的 mid_album_500 URL
	for _, albumAddr := range albumResults {
		url := tryExtractURLBefore(handle, albumAddr, 300)
		if url == "" || !strings.Contains(url, "imgcache.qq.com") {
			continue
		}
		if !strings.HasSuffix(url, ".jpg") && !strings.HasSuffix(url, ".png") {
			// 截取到 .jpg 或 .png 结尾
			if idx := strings.Index(url, ".jpg"); idx > 0 {
				url = url[:idx+4]
			} else if idx := strings.Index(url, ".png"); idx > 0 {
				url = url[:idx+4]
			} else {
				continue
			}
		}
		// 检查附近（±8KB）是否有当前歌曲的 songMID
		for _, midAddr := range midResults {
			dist := int64(albumAddr) - int64(midAddr)
			if dist < 0 {
				dist = -dist
			}
			if dist < 8192 {
				return url
			}
		}
	}

	// 最后备选: 返回最后一个有效的 imgcache URL（最新的通常在最高地址）
	for i := len(albumResults) - 1; i >= 0; i-- {
		url := tryExtractURLBefore(handle, albumResults[i], 300)
		if url != "" && strings.Contains(url, "imgcache.qq.com") {
			if idx := strings.Index(url, ".jpg"); idx > 0 {
				return url[:idx+4]
			}
			if idx := strings.Index(url, ".png"); idx > 0 {
				return url[:idx+4]
			}
		}
	}

	return ""
}

// tryExtractURLBefore 从指定地址向前搜索 "http" 并提取完整 URL
func tryExtractURLBefore(handle syscall.Handle, addr uint32, backBytes uint32) string {
	if addr < backBytes {
		return ""
	}
	backBuf, err := proc.ReadBytes(handle, addr-backBytes, backBytes)
	if err != nil {
		return ""
	}
	// UTF-16LE 中搜索最后出现的 "http" = 68 00 74 00 74 00 70 00
	httpStart := -1
	for i := len(backBuf) - 8; i >= 0; i -= 2 {
		if backBuf[i] == 0x68 && backBuf[i+1] == 0x00 &&
			backBuf[i+2] == 0x74 && backBuf[i+3] == 0x00 &&
			backBuf[i+4] == 0x74 && backBuf[i+5] == 0x00 &&
			backBuf[i+6] == 0x70 && backBuf[i+7] == 0x00 {
			httpStart = i
			break
		}
	}
	if httpStart < 0 {
		return ""
	}

	urlStartAddr := addr - backBytes + uint32(httpStart)
	urlBuf, err := proc.ReadBytes(handle, urlStartAddr, 512)
	if err != nil {
		return ""
	}
	return utf16LEBytesToStringUntilNull(urlBuf)
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

// FetchCoverBase64 下载封面图片并返回 base64 编码的 data URI
func FetchCoverBase64(coverURL string) string {
	if coverURL == "" {
		return ""
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(coverURL)
	if err != nil {
		log.Warn("下载封面失败: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	// 限制最大 2MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return ""
	}

	// 根据内容类型确定 MIME
	mimeType := "image/jpeg"
	if strings.HasSuffix(coverURL, ".png") {
		mimeType = "image/png"
	}

	encoded := base64.StdEncoding.EncodeToString(body)
	log.Detail("封面已获取 (%d bytes → base64)", len(body))
	return "data:" + mimeType + ";base64," + encoded
}

func utf16LEBytesToString(buf []byte) string {
	chars := make([]uint16, 0, len(buf)/2)
	for i := 0; i+1 < len(buf); i += 2 {
		val := uint16(buf[i]) | uint16(buf[i+1])<<8
		chars = append(chars, val)
	}
	return string(utf16.Decode(chars))
}

// utf16LEBytesToStringUntilNull 将 UTF-16LE 字节转为字符串（遇 null 终止）
func utf16LEBytesToStringUntilNull(buf []byte) string {
	chars := make([]uint16, 0, len(buf)/2)
	for i := 0; i+1 < len(buf); i += 2 {
		val := uint16(buf[i]) | uint16(buf[i+1])<<8
		if val == 0 {
			break
		}
		chars = append(chars, val)
	}
	return string(utf16.Decode(chars))
}
