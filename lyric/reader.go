package lyric

import (
	"fmt"
	"syscall"
	"Metabox-Nexus-WesingCap/proc"
)

// LyricLine 表示一行歌词
type LyricLine struct {
	Index int     // 行索引
	Time  float32 // 开始时间（秒）
	Text  string  // 歌词文本
}

// LoadLyrics 从内存中加载所有歌词行
// 数据结构: subStructAddr+0x48 = vector<LyricEntry*> begin
//           subStructAddr+0x50 = vector<LyricEntry*> end
func LoadLyrics(handle syscall.Handle, subStructAddr uint32) ([]LyricLine, error) {
	// 读取歌词条目向量的 begin 和 end 指针
	beginPtr, err := proc.ReadUint32(handle, subStructAddr+0x48)
	if err != nil {
		return nil, fmt.Errorf("读取歌词向量 begin 失败: %v", err)
	}
	endPtr, err := proc.ReadUint32(handle, subStructAddr+0x50)
	if err != nil {
		return nil, fmt.Errorf("读取歌词向量 end 失败: %v", err)
	}

	if endPtr <= beginPtr || beginPtr == 0 {
		return nil, fmt.Errorf("歌词向量为空 (begin=0x%X, end=0x%X)", beginPtr, endPtr)
	}

	numEntries := (endPtr - beginPtr) / 4
	if numEntries > 1000 {
		return nil, fmt.Errorf("歌词数量异常: %d", numEntries)
	}

	var lyrics []LyricLine

	for i := uint32(0); i < numEntries; i++ {
		// 读取 LyricEntry 指针
		entryPtr, err := proc.ReadUint32(handle, beginPtr+i*4)
		if err != nil || entryPtr == 0 {
			continue
		}

		// 读取开始时间（LyricEntry+0x00 = float）
		timeVal, err := proc.ReadFloat32(handle, entryPtr)
		if err != nil {
			continue
		}

		// 过滤垃圾数据：已有有效歌词后遇到 time<=0，说明后面全是无效数据
		if len(lyrics) > 0 && timeVal <= 0 {
			break
		}
		// 时间不应该回退（垃圾数据经常 time=0）
		if len(lyrics) > 0 && timeVal < lyrics[len(lyrics)-1].Time-1 {
			break
		}

		// 读取字符向量（LyricEntry+0x08 = begin, +0x0C = end）
		charBegin, err := proc.ReadUint32(handle, entryPtr+0x08)
		if err != nil || charBegin == 0 {
			continue
		}
		charEnd, err := proc.ReadUint32(handle, entryPtr+0x0C)
		if err != nil || charEnd <= charBegin {
			continue
		}

		numChars := (charEnd - charBegin) / 4
		if numChars > 500 {
			continue // 跳过异常数据
		}

		// 逐元素读取：CharElement* → RenderData* → UTF-16LE 字符串
		// 注意：中文歌词每个 CharElement 是单个汉字，英文歌词每个是一个单词
		text := make([]rune, 0, numChars*4)
		for c := uint32(0); c < numChars; c++ {
			charElemPtr, err := proc.ReadUint32(handle, charBegin+c*4)
			if err != nil || charElemPtr == 0 {
				continue
			}

			// CharElement+0x00 → RenderData*
			renderPtr, err := proc.ReadUint32(handle, charElemPtr)
			if err != nil || renderPtr < 0x00100000 {
				continue
			}

			// RenderData+0x00 → null 结尾的 UTF-16LE 字符串
			// 读取最多 64 个 wchar（128 字节），覆盖最长的英文单词
			rawBytes, err := proc.ReadBytes(handle, renderPtr, 128)
			if err != nil || len(rawBytes) < 2 {
				continue
			}
			for j := 0; j+1 < len(rawBytes); j += 2 {
				wchar := uint16(rawBytes[j]) | uint16(rawBytes[j+1])<<8
				if wchar == 0 {
					break
				}
				text = append(text, rune(wchar))
			}
		}

		if len(text) > 0 {
			lyrics = append(lyrics, LyricLine{
				Index: int(i),
				Time:  timeVal,
				Text:  string(text),
			})
		}
	}

	return lyrics, nil
}

// PrintLyrics 打印所有歌词行
func PrintLyrics(lyrics []LyricLine) {
	for _, l := range lyrics {
		fmt.Printf("    [%2d] %6.1fs  %s\n", l.Index, l.Time, l.Text)
	}
}

// FindCurrentLine 根据播放时间查找当前歌词行
// 返回最后一个 time <= playTime 的歌词行索引，-1 表示未开始
func FindCurrentLine(lyrics []LyricLine, playTime float32) int {
	current := -1
	for i, l := range lyrics {
		if playTime >= l.Time {
			current = i
		} else {
			break // 歌词按时间排序，后面的都更大
		}
	}
	return current
}

// isValidLyricText 检查文本是否是合理的歌词（非乱码）
// 垃圾数据通常包含大量罕见 Unicode 字符（如韩文 Jamo、私用区等）
func isValidLyricText(s string) bool {
	if len(s) == 0 {
		return false
	}
	valid := 0
	total := 0
	for _, r := range s {
		total++
		// ASCII 可打印字符、中文、日文、韩文常用字、常见标点
		if (r >= 0x20 && r <= 0x7E) || // ASCII 可打印
			(r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一汉字
			(r >= 0x3000 && r <= 0x303F) || // CJK 标点
			(r >= 0x3040 && r <= 0x30FF) || // 日文假名
			(r >= 0xAC00 && r <= 0xD7AF) || // 韩文音节
			(r >= 0xFF00 && r <= 0xFFEF) || // 全角字符
			(r >= 0x2000 && r <= 0x206F) || // 通用标点
			r == 0x2019 || r == 0x2018 || // 智能引号
			r == 0x00E9 || r == 0x00E0 || r == 0x00FC { // 常见重音字母
			valid++
		}
	}
	// 至少 60% 的字符应是合理的
	return float64(valid)/float64(total) > 0.6
}
