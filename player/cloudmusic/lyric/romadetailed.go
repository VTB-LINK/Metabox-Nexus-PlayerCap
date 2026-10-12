package lyric

import (
	"regexp"
	"strings"
	"unicode"

	"Metabox-Nexus-PlayerCap/player"
)

// romaSyllableRegex 认一个音节：ASCII 字母，后面至多一位声调数字。
//
// 2026-10-10 核对过的 3 首带逐字歌词的粤语歌里，配得上的 129 行的音节全是小写 ASCII 字母，不带声调。
// 大写与末尾的一位声调数字（粤拼的写法）是同一个音节的另一种写法，照认，没有见过实例。带标点、汉字的
// 不是一个字的读音；带变音符的写法没有见过，一并不认。不认的音节所在的行不给逐字音译。
var romaSyllableRegex = regexp.MustCompile(`^[A-Za-z]+[0-9]?$`)

// FillRomaDetailed 给整首歌的行补上逐字音译（RomaTextDetailed）。须在 MergeRomalrc 与 MergeYRC 之后调用。
//
// 网易云的音译只有逐行的，没有字级时间。一个汉字读一个音节的歌（粤语）可以把这一行的音节按顺序配到
// 逐字歌词的字上。日文不行：一个汉字可以读成多个音节，字数与音节数碰巧相等时配出来是错位的。所以
// 带逐字歌词的行里只要有一行出现假名或谚文，整首都不补。
//
// 只看带逐字歌词的行，不看别的行：lrc 里带时间戳的署名行（作词、作曲）在 yrc 里不是歌词行（yrc 的署名
// 是 JSON 行，ParseYRC 跳过），配不到逐字歌词，而粤语翻唱歌的署名里会有带假名的人名，把它算进来整首
// 都不补；日文、韩文歌的歌词行有逐字歌词，照样被排除；没有逐字歌词的行本来也配不出逐字音译。
//
// 逐行的条件见 romaDetailed，任一条不满足这一行就是零值：只留行级的 RomaText，不给错位的逐字。
// 只写 RomaTextDetailed，其余字段不动。
func FillRomaDetailed(lines []LyricLine) {
	for _, l := range lines {
		if len(l.TextDetailed.Words) > 0 && strings.ContainsFunc(l.Text, isKanaOrHangul) {
			return
		}
	}
	for i := range lines {
		lines[i].RomaTextDetailed = romaDetailed(lines[i].RomaText, lines[i].TextDetailed)
	}
}

func isKanaOrHangul(r rune) bool {
	return unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// romaDetailed 按一行的逐字歌词给它的行级音译配上字级时间。以下全部满足才给，否则返回零值：
//   - 除纯空白的单元外，每个逐字单元去掉首尾空白后恰是一个汉字（带标点、拉丁字母、数字的单元，
//     一个单元里有多个字的，都不算）；
//   - 音译按空白切出的音节数等于汉字单元数，每个音节都是罗马字（romaSyllableRegex）；
//   - 这样配出的候选满足各播放器共同的约定（player.BuildRomaDetailed）：行级音译非空；各单元的文本
//     拼起来、去掉首尾空白，与行级音译逐字节相同；行级与各单元的起始、时长是有限的非负数，单元的
//     起始在行内不回退。yrc 的字时间回退的行因此只留行级的音译。
//
// 第 j 个音节取第 j 个汉字单元的时间，文本见 romaUnits；整行的时间取逐字歌词的。时间都是照搬：
// PlayTime 在 MergeYRC 里已按 offset 算过，这里不再算。返回的 Words 是新分配的，不与 detail.Words
// 共用底层数组。
func romaDetailed(romaText string, detail player.LyricTextDetailed) player.LyricTextDetailed {
	var words []player.LyricTextDetailedWord
	for _, w := range detail.Words {
		text := []rune(strings.TrimSpace(w.Text))
		if len(text) == 0 {
			continue
		}
		if len(text) != 1 || !unicode.Is(unicode.Han, text[0]) {
			return player.LyricTextDetailed{}
		}
		words = append(words, w)
	}
	units := romaUnits(romaText)
	if len(words) != len(units) {
		return player.LyricTextDetailed{}
	}
	for i, unit := range units {
		if !romaSyllableRegex.MatchString(strings.TrimSpace(unit)) {
			return player.LyricTextDetailed{}
		}
		words[i].Text = unit
	}
	detail.Words = words
	return player.BuildRomaDetailed(detail, romaText)
}

// romaUnits 把行级音译切成逐字音译各单元的文本：每个单元是一个音节加上它后面紧随的空白（原样，连续
// 多个空格也照留），最后一个音节不带；首尾的空白不进任何单元。
func romaUnits(romaText string) []string {
	rest := strings.TrimSpace(romaText)
	syllables := strings.Fields(rest)
	units := make([]string, len(syllables))
	for i, syllable := range syllables {
		rest = rest[len(syllable):]
		gap := len(rest) - len(strings.TrimLeftFunc(rest, unicode.IsSpace))
		units[i] = syllable + rest[:gap]
		rest = rest[gap:]
	}
	return units
}
