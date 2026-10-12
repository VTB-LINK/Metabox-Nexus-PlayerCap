package lyric

// 本文件只测逐字音译（roma_text_detailed）在本包里的产出：FillRomaDetailed 把行级音译按顺序配到逐字歌词的
// 字上的判据，以及 HTTP API 取词（FetchLyrics）接上了它。样例是自拟的短句，不取自任何歌曲。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// romaYRC 造一行 YRC：从 startMs 起，每个单元 200ms、首尾相接。
func romaYRC(startMs int, units ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%d,%d]", startMs, 200*len(units))
	for i, u := range units {
		fmt.Fprintf(&b, "(%d,200,0)%s", startMs+200*i, u)
	}
	b.WriteString("\n")
	return b.String()
}

// romaLine 直接造一行：逐字单元从 10s 起每个 200ms，PlayTime 是按 offset -0.5 算过的（MergeYRC 之后的
// 样子）。不给 units 即没有逐字歌词。
func romaLine(roma string, units ...string) LyricLine {
	l := LyricLine{Time: 10, Text: strings.Join(units, ""), SubText: "译文", RomaText: roma}
	if len(units) == 0 {
		l.Text = "今晚食乜嘢"
		return l
	}
	l.TextDetailed = player.LyricTextDetailed{Timestamp: 10, PlayTime: 9.5, Duration: 0.2 * float32(len(units))}
	for i, u := range units {
		ts := float32(10000+200*i) / 1000
		l.TextDetailed.Words = append(l.TextDetailed.Words, player.LyricTextDetailedWord{
			Timestamp: ts, PlayTime: player.AdjustLyricPlayTime(ts, -0.5), Duration: 0.2, Text: u,
		})
	}
	return l
}

// fillRoma 调 FillRomaDetailed，并断言除 RomaTextDetailed 之外，各行的每个字段（含逐字歌词的每个单元）
// 都没有被改：音译是纯增量，主歌词、翻译、行级音译与逐字歌词在任何情形下都不变。
func fillRoma(t *testing.T, lines []LyricLine) {
	t.Helper()
	before := make([]LyricLine, len(lines))
	for i, l := range lines {
		l.TextDetailed.Words = slices.Clone(l.TextDetailed.Words)
		before[i] = l
	}
	FillRomaDetailed(lines)
	for i, l := range lines {
		l.RomaTextDetailed = before[i].RomaTextDetailed
		if !reflect.DeepEqual(l, before[i]) {
			t.Fatalf("第 %d 行除逐字音译之外的字段被改了：\n前 %+v\n后 %+v", i, before[i], l)
		}
	}
}

// wantRoma 断言这一行产出了逐字音译并满足对外的约定：各单元的文本是 texts；第 j 个单元的时间取自第 j 个
// 非空白的逐字单元；整行的时间取自逐字歌词；各单元的文本拼起来、去掉首尾空白，等于行级音译。
func wantRoma(t *testing.T, l LyricLine, texts ...string) {
	t.Helper()
	d := l.RomaTextDetailed
	if len(d.Words) != len(texts) {
		t.Fatalf("逐字音译有 %d 个单元，应为 %d 个：%+v", len(d.Words), len(texts), d.Words)
	}
	var han []player.LyricTextDetailedWord
	for _, w := range l.TextDetailed.Words {
		if strings.TrimSpace(w.Text) != "" {
			han = append(han, w)
		}
	}
	if len(han) != len(texts) {
		t.Fatalf("用例有误：%d 个非空白的逐字单元，期望 %d 个音节", len(han), len(texts))
	}
	for j, w := range d.Words {
		if w.Text != texts[j] {
			t.Errorf("第 %d 个单元的文本 = %q，应为 %q", j, w.Text, texts[j])
		}
		if w.Timestamp != han[j].Timestamp || w.Duration != han[j].Duration || w.PlayTime != han[j].PlayTime {
			t.Errorf("第 %d 个单元的时间 = %v/%v/%v，应取第 %d 个汉字单元的 %v/%v/%v",
				j, w.Timestamp, w.PlayTime, w.Duration, j, han[j].Timestamp, han[j].PlayTime, han[j].Duration)
		}
	}
	if td := l.TextDetailed; d.Timestamp != td.Timestamp || d.PlayTime != td.PlayTime || d.Duration != td.Duration {
		t.Errorf("整行的时间 = %v/%v/%v，应取逐字歌词的 %v/%v/%v", d.Timestamp, d.PlayTime, d.Duration, td.Timestamp, td.PlayTime, td.Duration)
	}
	if got := strings.TrimSpace(player.BuildDetailedText(d.Words)); got != l.RomaText {
		t.Errorf("各单元拼起来去掉首尾空白 = %q，应等于行级音译 %q", got, l.RomaText)
	}
}

// wantNoRoma 断言这一行没有逐字音译，且是零值（对外序列化为 {}）。
func wantNoRoma(t *testing.T, l LyricLine) {
	t.Helper()
	if !reflect.DeepEqual(l.RomaTextDetailed, player.LyricTextDetailed{}) {
		t.Fatalf("不该有逐字音译（应为零值），got %+v", l.RomaTextDetailed)
	}
}

// TestFillRomaDetailedPairsSyllablesWithHanUnits 配得上的行：第 j 个音节取第 j 个汉字单元的时间，纯空白的
// 单元跳过；单元的文本是音节加上它在行级音译里紧随其后的空白（原样），最后一个不带。
//
// 变异自证（隔离的 worktree，实测）：纯空白的单元不跳过、或算作一个字 → 三个带空白单元的例红；时间按原下标
// 取（不跳过空白单元）→ 同三例红；单元之间的空白归一成一个空格 → 连续多个空格、制表符与全角空格两例红；
// 单元的文本不带空白 → 除「只有一个字」之外各例红；音节的正则不认声调数字、不认大写 → 各自对应的一例红；
// play_time 取 timestamp 或置零、整行的时间不取逐字歌词的、挑汉字单元时复用逐字歌词的底层数组 → 各例均红。
func TestFillRomaDetailedPairsSyllablesWithHanUnits(t *testing.T) {
	const roma = "gam maan sik mat je"
	plain := []string{"gam ", "maan ", "sik ", "mat ", "je"}
	cases := []struct {
		name  string
		roma  string
		units []string
		want  []string
	}{
		{"粤语正常行", roma, []string{"今", "晚", "食", "乜", "嘢"}, plain},
		{"只有一个字", "gam", []string{"今"}, []string{"gam"}},
		{"半角空格单元", roma, []string{"今", "晚", " ", "食", "乜", "嘢"}, plain},
		{"全角空格单元", roma, []string{"今", "晚", "\u3000", "食", "乜", "嘢"}, plain},
		{"行首行尾的空白单元", roma, []string{" ", "今", "晚", "食", "乜", "嘢", "\u3000"}, plain},
		{"汉字单元自带首尾空白", roma, []string{"今", "晚 ", " 食", "乜\u3000", "嘢"}, plain},
		{"音译里连续多个空格", "gam  maan   sik mat je", []string{"今", "晚", "食", "乜", "嘢"},
			[]string{"gam  ", "maan   ", "sik ", "mat ", "je"}},
		{"音译里的制表符与全角空格", "gam\tmaan\u3000sik mat je", []string{"今", "晚", "食", "乜", "嘢"},
			[]string{"gam\t", "maan\u3000", "sik ", "mat ", "je"}},
		{"音节带一位声调数字", "gam1 maan5 sik6 mat1 je5", []string{"今", "晚", "食", "乜", "嘢"},
			[]string{"gam1 ", "maan5 ", "sik6 ", "mat1 ", "je5"}},
		{"音节带大写字母", "Gam MAAN sik mat je", []string{"今", "晚", "食", "乜", "嘢"},
			[]string{"Gam ", "MAAN ", "sik ", "mat ", "je"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := []LyricLine{romaLine(c.roma, c.units...)}
			fillRoma(t, lines)
			wantRoma(t, lines[0], c.want...)
		})
	}
}

// TestFillRomaDetailedRejectsLine 配不上的行只留行级的音译，逐字音译为零值：宁可没有，不给错位的。
//
// 变异自证（隔离的 worktree，实测）：单元只看第一个字是不是汉字 → 「汉字单元带标点」「一个单元两个汉字，
// 音节数等于单元数」红；单元只要求是一个字符、不要求是汉字 → 单个拉丁字母、数字、标点三例红；纯空白的单元
// 算作一个字 → 「空白单元不算字」红；去掉字数与音节数的比较 → 「音节比字多一个」下标越界；去掉音节形状的
// 检查 → 「音节……」八例红；正则允许多位数字、认任意字母数字、去掉首尾锚 → 各自对应的例红；不经
// player.BuildRomaDetailed、直接返回候选 → 音译首部、尾部有空白与「没有行级音译，单元全是空白」三例红；换回
// 只核对拼接的旧写法（不查行级音译非空）→ 「没有行级音译，单元全是空白」红。
//
// 等价变异两条，都由 player.BuildRomaDetailed 的拼接核对兜住，各自再绕开它即红：字数与音节数只拦「字少于
// 音节」（多出来的单元仍是汉字，拼起来不等于行级音译）；romaUnits 不先去首尾空白。
func TestFillRomaDetailedRejectsLine(t *testing.T) {
	han := []string{"今", "晚", "食", "乜", "嘢"}
	cases := []struct {
		name  string
		roma  string
		units []string
	}{
		// 逐字单元不是「一个汉字」。各例的音节数都等于非空白的单元数，只有单元的判据拦得住。
		{"单元是拉丁字母单词", "gam maan ok", []string{"今", "晚", "OK"}},
		{"单元是单个拉丁字母", "gam maan di", []string{"今", "晚", "D"}},
		{"单元是数字", "gam maan saam", []string{"今", "晚", "3"}},
		{"单元是标点", "gam maan aa", []string{"今", "晚", "！"}},
		{"汉字单元带标点", "gam maan", []string{"今", "晚，"}},
		{"一个单元两个汉字，音节数等于单元数", "gam sik", []string{"今晚", "食"}},
		{"一个单元两个汉字，音节数等于字数", "gam maan sik", []string{"今晚", "食"}},
		// 字数与音节数不等。
		{"字比音节多一个", "gam maan sik mat", han},
		{"音节比字多一个", "gam maan sik mat je aa", han},
		{"空白单元不算字", "gam maan sik mat je aa", []string{"今", "晚", " ", "食", "乜", "嘢"}},
		// 音节不是罗马字。
		{"音节里混入汉字", "gam 晚 sik mat je", han},
		{"音节带标点", "gam maan, sik mat je", han},
		{"音节带括号", "(gam maan sik mat je)", han},
		{"音节以数字开头", "gam 4m sik mat je", han},
		{"音节只有数字", "gam 5 sik mat je", han},
		{"音节带两位数字", "gam12 maan sik mat je", han},
		{"音节带变音符", "gām maan sik mat je", han},
		{"音节是全角字母", "ｇａｍ maan sik mat je", han},
		// 行级音译首尾带空白：各单元拼起来去掉首尾空白不可能与它逐字节相同。
		{"音译首部有空白", " gam maan sik mat je", han},
		{"音译尾部有空白", "gam maan sik mat je ", han},
		// 缺一样。
		{"没有逐字歌词", "gam maan sik mat je", nil},
		{"没有行级音译", "", han},
		{"没有行级音译，单元全是空白", "", []string{" ", "\u3000"}},
		{"音译只有空白", " ", han},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := []LyricLine{romaLine(c.roma, c.units...)}
			fillRoma(t, lines)
			wantNoRoma(t, lines[0])
		})
	}
}

// romaSong 走取词时的顺序：ParseLRC → MergeRomalrc → MergeYRC → FillRomaDetailed。
func romaSong(t *testing.T, lrc, romalrc, yrc string, offsetSec float32) []LyricLine {
	t.Helper()
	lines := ParseLRC(lrc)
	MergeRomalrc(lines, romalrc)
	MergeYRC(lines, yrc, offsetSec)
	fillRoma(t, lines)
	return lines
}

// 两行都配得上的一首歌。第一行的 YRC 比 LRC 早 100ms：逐字音译的时间取自 YRC。
const (
	romaSongLRC     = "[00:10.00]今晚食乜嘢\n[00:12.00]听日再讲\n"
	romaSongRomalrc = "[00:10.00]gam maan sik mat je\n[00:12.00]ting jat zoi gong\n"
)

var romaSongYRC = romaYRC(9900, "今", "晚", "食", "乜", "嘢") + romaYRC(12000, "听", "日", "再", "讲")

// romaYRCOf 造一行 YRC，text 的每个字符各占一个单元。
func romaYRCOf(startMs int, text string) string {
	return romaYRC(startMs, strings.Split(text, "")...)
}

// TestFillRomaDetailedSkipsSongWithKanaOrHangul 带逐字歌词的行里只要有一行出现假名或谚文，整首都不给逐字
// 音译，包括那些逐行看全都配得上的行：日文的一个汉字可以读成多个音节，字数与音节数相等也可能是错位的。
// 没有逐字歌词的行不看：署名行（作词、作曲）在 YRC 里不是歌词行，里面带假名的人名不该让整首粤语歌都不给。
//
// 变异自证（隔离的 worktree，实测）：改回扫所有行（不问有没有逐字歌词）→ 「不算」三例红；不排除、或改成只
// 跳过含假名或谚文的那一行 → 前七例全红；条件写反（只看没有逐字歌词的行）→ 前七例与「不算」三例全红；判据里
// 去掉平假名、片假名、谚文中的一种 → 用到它的各例红。
func TestFillRomaDetailedSkipsSongWithKanaOrHangul(t *testing.T) {
	// 只多一行带逐字歌词、含假名或谚文的行：整首都没有。最后两例里那一行是署名行自己（YRC 里也有它），
	// 或者另有一条干净的署名行：看的是这一行带不带逐字歌词，不是它是不是署名行。
	for name, c := range map[string]struct{ lrc, yrc string }{
		"平假名":          {"[00:14.00]ありがとう\n", romaYRCOf(14000, "ありがとう")},
		"片假名":          {"[00:14.00]カラオケ\n", romaYRCOf(14000, "カラオケ")},
		"半角片假名":        {"[00:14.00]ｶﾗｵｹ\n", romaYRCOf(14000, "ｶﾗｵｹ")},
		"谚文":           {"[00:14.00]안녕\n", romaYRCOf(14000, "안녕")},
		"汉字行里夹一个假名":    {"[00:14.00]今晚の月\n", romaYRCOf(14000, "今晚の月")},
		"署名行自己带逐字歌词":   {"[00:00.00]作曲 : やまだ\n", romaYRCOf(0, "作曲やまだ")},
		"署名行干净，歌词行含假名": {"[00:00.00]作曲 : 某人\n[00:14.00]ありがとう\n", romaYRCOf(14000, "ありがとう")},
	} {
		t.Run(name, func(t *testing.T) {
			lines := romaSong(t, romaSongLRC+c.lrc, romaSongRomalrc, romaSongYRC+c.yrc, -0.5)
			withKana := 0
			for _, l := range lines {
				if len(l.TextDetailed.Words) > 0 && strings.ContainsFunc(l.Text, isKanaOrHangul) {
					withKana++
				}
				wantNoRoma(t, l)
			}
			if withKana != 1 || len(lines[len(lines)-2].TextDetailed.Words) == 0 {
				t.Fatalf("用例有误：应恰有一行带逐字歌词且含假名或谚文、其余歌词行也带逐字歌词，got %+v", lines)
			}
		})
	}

	// 假名、谚文只出现在没有逐字歌词的行里：不算，别的行照常给。
	for name, extra := range map[string]string{
		"署名行里的假名":    "[00:00.00]作曲 : やまだ\n",
		"署名行里的谚文":    "[00:00.00]作词 : 김\n",
		"没有逐字歌词的假名行": "[00:14.00]ありがとう\n",
	} {
		t.Run("不算："+name, func(t *testing.T) {
			lines := romaSong(t, romaSongLRC+extra, romaSongRomalrc, romaSongYRC, -0.5)
			if len(lines) != 3 {
				t.Fatalf("应解析出 3 行，实得 %d", len(lines))
			}
			for _, l := range lines {
				switch l.Time {
				case 10:
					wantRoma(t, l, "gam ", "maan ", "sik ", "mat ", "je")
				case 12:
					wantRoma(t, l, "ting ", "jat ", "zoi ", "gong")
				default:
					if len(l.TextDetailed.Words) != 0 || !strings.ContainsFunc(l.Text, isKanaOrHangul) {
						t.Fatalf("用例有误：多出来的那一行应含假名或谚文、且没有逐字歌词，got %+v", l)
					}
					wantNoRoma(t, l)
				}
			}
		})
	}

	// 对照：多出来的那一行是英文、标点或中英混排时不影响别的行，它自己配不上而已。
	for name, extra := range map[string]string{
		"英文行":   "[00:14.00]hello world\n",
		"标点行":   "[00:14.00]……！\n",
		"中英混排行": "[00:14.00]OK啦\n",
		"空行":    "[00:14.00]\n",
	} {
		t.Run("对照："+name, func(t *testing.T) {
			yrc := romaSongYRC + romaYRC(14000, "OK", "啦")
			lines := romaSong(t, romaSongLRC+extra, romaSongRomalrc+"[00:14.00]ok laa\n", yrc, -0.5)
			if len(lines) != 3 {
				t.Fatalf("应解析出 3 行，实得 %d", len(lines))
			}
			wantRoma(t, lines[0], "gam ", "maan ", "sik ", "mat ", "je")
			wantRoma(t, lines[1], "ting ", "jat ", "zoi ", "gong")
			wantNoRoma(t, lines[2])
		})
	}
}

// TestFillRomaDetailedPlayTimeFollowsOffset 逐字音译的 play_time 照搬逐字歌词的：MergeYRC 已按 offset 算过
// 一次，不再算第二次。offset 为正、为负、负到把 play_time 压到 0 的三种取值各验一遍。
//
// 变异自证（隔离的 worktree，实测）：单元的 play_time 取 timestamp（不带 offset）→ 三例红；置零 → offset 为
// -0.5 与 0.3 两例红（-20 时本来就是 0）。
func TestFillRomaDetailedPlayTimeFollowsOffset(t *testing.T) {
	for _, off := range []float32{-0.5, 0.3, -20} {
		t.Run(fmt.Sprintf("offset=%v", off), func(t *testing.T) {
			lines := romaSong(t, romaSongLRC, romaSongRomalrc, romaSongYRC, off)
			wantRoma(t, lines[0], "gam ", "maan ", "sik ", "mat ", "je")
			wantRoma(t, lines[1], "ting ", "jat ", "zoi ", "gong")

			d := lines[0].RomaTextDetailed
			if d.Timestamp != 9.9 || d.Duration != 1 {
				t.Errorf("整行的 timestamp/duration = %v/%v，应取 YRC 的 9.9/1（不是 LRC 的 10）", d.Timestamp, d.Duration)
			}
			if want := player.AdjustLyricPlayTime(9.9, off); d.PlayTime != want {
				t.Errorf("整行的 play_time = %v，应为 %v", d.PlayTime, want)
			}
			for j, w := range d.Words {
				ts := float32(9900+200*j) / 1000
				if w.Timestamp != ts || w.Duration != 0.2 {
					t.Errorf("第 %d 个单元的 timestamp/duration = %v/%v，应为 %v/0.2", j, w.Timestamp, w.Duration, ts)
				}
				want := player.AdjustLyricPlayTime(ts, off)
				if w.PlayTime != want {
					t.Errorf("第 %d 个单元的 play_time = %v，应为 %v（timestamp %v 按 offset %v 算一次）", j, w.PlayTime, want, ts, off)
				}
				if off != -20 && w.PlayTime == w.Timestamp {
					t.Errorf("第 %d 个单元的 play_time 等于 timestamp：用例没有打到 offset", j)
				}
			}
			if off == -20 && d.Words[4].PlayTime != 0 {
				t.Errorf("offset 负到超过时间戳时 play_time 应为 0，got %v", d.Words[4].PlayTime)
			}
		})
	}
}

// TestFillRomaDetailedRejectsInvalidTimeline 配出的候选还要过各播放器共同的约定（player.BuildRomaDetailed）：
// 逐字歌词的字时间不合法（字的起始在行内回退、时长为负）的行只留行级的音译，别的行不受影响。起始相同
// 不算回退；被跳过的纯空白单元不参与判定。
//
// 变异自证（隔离的 worktree，实测）：不经 player.BuildRomaDetailed、直接返回候选，或换回只核对拼接的旧写法
// （不核对时间）→ 第一例与三条不合法的例红；纯空白的单元不跳过、算作一个字、时间按原下标取 → 「对照：回退的
// 是被跳过的空白单元」红。
func TestFillRomaDetailedRejectsInvalidTimeline(t *testing.T) {
	t.Run("YRC 里第二、三个字的起始对调", func(t *testing.T) {
		yrc := "[9900,1000](9900,200,0)今(10300,200,0)晚(10100,200,0)食(10500,200,0)乜(10700,200,0)嘢\n" +
			romaYRC(12000, "听", "日", "再", "讲")
		lines := romaSong(t, romaSongLRC, romaSongRomalrc, yrc, -0.5)
		if len(lines) != 2 || len(lines[0].TextDetailed.Words) != 5 || lines[0].RomaText != "gam maan sik mat je" {
			t.Fatalf("用例有误：第一行的行级音译与逐字歌词应照常取到，got %+v", lines)
		}
		wantNoRoma(t, lines[0])
		wantRoma(t, lines[1], "ting ", "jat ", "zoi ", "gong")
	})

	const roma = "gam maan sik mat je"
	han := []string{"今", "晚", "食", "乜", "嘢"}
	withBlank := []string{"今", "晚", " ", "食", "乜", "嘢"}
	cases := []struct {
		name   string
		units  []string
		tamper func(d *player.LyricTextDetailed)
		ok     bool
	}{
		{"字的起始回退 1ms", han, func(d *player.LyricTextDetailed) { d.Words[2].Timestamp = d.Words[1].Timestamp - 0.001 }, false},
		{"字的时长为负", han, func(d *player.LyricTextDetailed) { d.Words[1].Duration = -0.1 }, false},
		{"整行的时长为负", han, func(d *player.LyricTextDetailed) { d.Duration = -1 }, false},
		{"对照：两个字起始相同", han, func(d *player.LyricTextDetailed) { d.Words[2].Timestamp = d.Words[1].Timestamp }, true},
		{"对照：回退的是被跳过的空白单元", withBlank, func(d *player.LyricTextDetailed) { d.Words[2].Timestamp = 0 }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := []LyricLine{romaLine(roma, c.units...)}
			c.tamper(&lines[0].TextDetailed)
			fillRoma(t, lines)
			if c.ok {
				wantRoma(t, lines[0], "gam ", "maan ", "sik ", "mat ", "je")
			} else {
				wantNoRoma(t, lines[0])
			}
		})
	}
}

// TestFillRomaDetailedDoesNotShareWords 逐字音译的单元是另行分配的：改它不影响逐字歌词，反过来也一样。
//
// 变异自证（隔离的 worktree，实测）：挑汉字单元时复用逐字歌词的底层数组（words := detail.Words[:0]）→ 红：
// 逐字歌词各单元的文本被音节盖掉，本例与经 fillRoma 的各用例只要走到给单元写文本的那一步，都红在「其余字段
// 被改」上。返回的切片不与候选共用由 player.BuildRomaDetailed 保证，用例在 player/romadetailed_test.go。
func TestFillRomaDetailedDoesNotShareWords(t *testing.T) {
	lines := romaSong(t, romaSongLRC, romaSongRomalrc, romaSongYRC, -0.5)
	l := &lines[0]
	wantRoma(t, *l, "gam ", "maan ", "sik ", "mat ", "je")

	l.RomaTextDetailed.Words[0].Text = "x"
	l.RomaTextDetailed.Words[0].Timestamp = 99
	if w := l.TextDetailed.Words[0]; w.Text != "今" || w.Timestamp != 9.9 {
		t.Fatalf("改逐字音译的单元波及了逐字歌词：%+v", w)
	}

	l.TextDetailed.Words[1].Text = "y"
	l.TextDetailed.Words[1].Timestamp = 88
	if w := l.RomaTextDetailed.Words[1]; w.Text != "maan " || w.Timestamp != 10.1 {
		t.Fatalf("改逐字歌词的单元波及了逐字音译：%+v", w)
	}
}

// cannedTransport 不出网，对任何请求都回同一段正文。
type cannedTransport string

func (body cannedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

// TestFetchLyricsFillsRomaDetailed HTTP API 取词这条路径接上了逐字音译：配得上的歌带上，含假名的歌整首没有。
//
// 变异自证（隔离的 worktree，实测）：FetchLyrics 里删掉 FillRomaDetailed 的调用、挪到 MergeYRC 之前、挪到
// MergeRomalrc 之前 → 「配得上的歌」红；去掉整首的排除 → 「含假名的歌」红。
func TestFetchLyricsFillsRomaDetailed(t *testing.T) {
	fetch := func(t *testing.T, lrc, yrc string) []LyricLine {
		t.Helper()
		lyricOf := func(s string) map[string]string { return map[string]string{"lyric": s} }
		body, err := json.Marshal(map[string]any{
			"lrc": lyricOf(lrc), "romalrc": lyricOf(romaSongRomalrc), "yrc": lyricOf(yrc),
		})
		if err != nil {
			t.Fatal(err)
		}
		orig := httpClient
		httpClient = &http.Client{Transport: cannedTransport(body)}
		defer func() { httpClient = orig }()

		lines, err := FetchLyrics("1", -0.5)
		if err != nil {
			t.Fatalf("FetchLyrics: %v", err)
		}
		return lines
	}

	t.Run("配得上的歌", func(t *testing.T) {
		lines := fetch(t, romaSongLRC, romaSongYRC)
		if len(lines) != 2 {
			t.Fatalf("应解析出 2 行，实得 %d", len(lines))
		}
		wantRoma(t, lines[0], "gam ", "maan ", "sik ", "mat ", "je")
		wantRoma(t, lines[1], "ting ", "jat ", "zoi ", "gong")
		if got, want := lines[0].RomaTextDetailed.Words[1].PlayTime, player.AdjustLyricPlayTime(10.1, -0.5); got != want {
			t.Errorf("第二个音节的 play_time = %v，应为 %v", got, want)
		}
	})
	t.Run("含假名的歌", func(t *testing.T) {
		lines := fetch(t, romaSongLRC+"[00:14.00]ありがとう\n", romaSongYRC+romaYRCOf(14000, "ありがとう"))
		if len(lines) != 3 || lines[0].RomaText != "gam maan sik mat je" || len(lines[0].TextDetailed.Words) != 5 || len(lines[2].TextDetailed.Words) != 5 {
			t.Fatalf("用例有误：行级音译与逐字歌词应照常取到，含假名的那一行也带逐字歌词，got %+v", lines)
		}
		for _, l := range lines {
			wantNoRoma(t, l)
		}
	})
}
