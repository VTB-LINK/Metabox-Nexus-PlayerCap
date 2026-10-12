package cloudmusic

// 本文件只测逐字音译（roma_text_detailed）从取词结果到对外载荷的接线：resolveCDPLyrics 产出它，toExtracted
// 搬进轮询用的结构，toLyricItems（all_lyrics 的行）与 lineUpdate（lyric_update）各自带上，两者同源。
// 配字的判据在 lyric 包的 romadetailed_test.go。样例是自拟的短句，不取自任何歌曲。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/lyric"
)

// 三行：第一行配得上（YRC 比 LRC 早 100ms）；第二行中英混排，配不上；第三行没有逐字歌词。
const (
	wireLRC     = "[00:10.00]今晚食乜嘢\n[00:12.00]OK啦\n[00:14.00]听日再讲\n"
	wireTlyric  = "[00:10.00]今晚吃什么\n"
	wireRomalrc = "[00:10.00]gam maan sik mat je\n[00:12.00]ok laa\n[00:14.00]ting jat zoi gong\n"
	wireYRC     = "[9900,1000](9900,200,0)今(10100,200,0)晚(10300,200,0)食(10500,200,0)乜(10700,200,0)嘢\n" +
		"[12000,400](12000,200,0)OK(12200,200,0)啦\n"
	wireOffset = float32(-0.5)
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRomaDetailedReachesAllLyricsAndLyricUpdate CDP 取词的结果一路带到 all_lyrics 的行与 lyric_update：
// 配得上的行两处都带、内容相同，配不上的行两处都是 {}；主歌词、翻译、行级音译、逐字歌词与不补逐字音译时
// 逐字段相同。
//
// 变异自证（隔离的 worktree，实测）：resolveCDPLyrics 里删掉 lyric.FillRomaDetailed 的调用、或挪到 MergeYRC
// 之前 → 红；toLyricItems 不赋 RomaTextDetailed → 红；lineUpdate 不赋 → 红；toExtracted 改回逐字段拷贝而不带
// 新字段 → 红；cdp.ExtractedLyric 不加这个字段 → toExtracted 里的转换编译不过。
func TestRomaDetailedReachesAllLyricsAndLyricUpdate(t *testing.T) {
	parsed, ok := resolveCDPLyrics(wireLRC, wireTlyric, wireRomalrc, wireYRC, wireOffset)
	if !ok || len(parsed) != 3 {
		t.Fatalf("应解析出 3 行，got ok=%v %d 行", ok, len(parsed))
	}

	// 其余字段：与不补逐字音译的同一条解析链路逐字段相同。
	base := lyric.ParseLRC(wireLRC)
	lyric.MergeTlyric(base, wireTlyric)
	lyric.MergeRomalrc(base, wireRomalrc)
	lyric.MergeYRC(base, wireYRC, wireOffset)
	for i, l := range parsed {
		l.RomaTextDetailed = player.LyricTextDetailed{}
		if !reflect.DeepEqual(l, base[i]) {
			t.Fatalf("第 %d 行除逐字音译之外的字段变了：\n不补 %+v\n补了 %+v", i, base[i], l)
		}
	}

	want := player.LyricTextDetailed{
		Timestamp: 9.9, PlayTime: player.AdjustLyricPlayTime(9.9, wireOffset), Duration: 1,
		Words: []player.LyricTextDetailedWord{
			{Timestamp: 9.9, PlayTime: player.AdjustLyricPlayTime(9.9, wireOffset), Duration: 0.2, Text: "gam "},
			{Timestamp: 10.1, PlayTime: player.AdjustLyricPlayTime(10.1, wireOffset), Duration: 0.2, Text: "maan "},
			{Timestamp: 10.3, PlayTime: player.AdjustLyricPlayTime(10.3, wireOffset), Duration: 0.2, Text: "sik "},
			{Timestamp: 10.5, PlayTime: player.AdjustLyricPlayTime(10.5, wireOffset), Duration: 0.2, Text: "mat "},
			{Timestamp: 10.7, PlayTime: player.AdjustLyricPlayTime(10.7, wireOffset), Duration: 0.2, Text: "je"},
		},
	}
	if want.Words[1].PlayTime == want.Words[1].Timestamp {
		t.Fatal("用例有误：play_time 应与 timestamp 不同")
	}

	active := toExtracted(parsed)
	items := toLyricItems(active, wireOffset)
	if len(items) != 3 {
		t.Fatalf("all_lyrics 应有 3 行，got %d", len(items))
	}
	updates := make([]*player.LyricUpdate, len(active))
	for i, l := range active {
		updates[i] = lineUpdate(i, l, wireOffset, 10.2, 200)
	}

	// 配得上的行：两处都带，且相同。
	if got := items[0].RomaTextDetailed; !reflect.DeepEqual(got, want) {
		t.Errorf("all_lyrics 的行：roma_text_detailed =\n%+v\nwant\n%+v", got, want)
	}
	if got := updates[0].RomaTextDetailed; !reflect.DeepEqual(got, want) {
		t.Errorf("lyric_update：roma_text_detailed =\n%+v\nwant\n%+v", got, want)
	}
	for where, js := range map[string]string{"all_lyrics 的行": mustJSON(t, items[0]), "lyric_update": mustJSON(t, updates[0])} {
		if !strings.Contains(js, `"roma_text_detailed":{"timestamp":9.9,`) || !strings.Contains(js, `"text":"maan "`) {
			t.Errorf("%s 的 JSON 里没有逐字音译：%s", where, js)
		}
	}

	// 配不上的行（中英混排、没有逐字歌词）：两处都是零值，序列化为 {}。
	for _, i := range []int{1, 2} {
		for where, d := range map[string]player.LyricTextDetailed{"all_lyrics 的行": items[i].RomaTextDetailed, "lyric_update": updates[i].RomaTextDetailed} {
			if !reflect.DeepEqual(d, player.LyricTextDetailed{}) {
				t.Errorf("第 %d 行 %s：不该有逐字音译，got %+v", i, where, d)
			}
		}
		for where, js := range map[string]string{"all_lyrics 的行": mustJSON(t, items[i]), "lyric_update": mustJSON(t, updates[i])} {
			if !strings.Contains(js, `"roma_text_detailed":{}`) {
				t.Errorf("第 %d 行 %s 的 JSON 里 roma_text_detailed 应为 {}：%s", i, where, js)
			}
		}
	}

	// 其余对外字段照旧。
	if it := items[0]; it.Index != 0 || it.Text != "今晚食乜嘢" || it.SubText != "今晚吃什么" || it.RomaText != "gam maan sik mat je" ||
		len(it.TextDetailed.Words) != 5 || it.TextDetailed.Words[1].Text != "晚" {
		t.Errorf("all_lyrics 的行其余字段不对：%+v", it)
	}
	if u := updates[0]; u.Index != 0 || u.Text != "今晚食乜嘢" || u.SubText != "今晚吃什么" || u.RomaText != "gam maan sik mat je" ||
		len(u.TextDetailed.Words) != 5 || u.TextDetailed.Words[1].Text != "晚" || u.Position != 10.2 || u.PlayTime != items[0].PlayTime {
		t.Errorf("lyric_update 其余字段不对：%+v", u)
	}
	if it, u := items[2], updates[2]; it.RomaText != "ting jat zoi gong" || u.RomaText != "ting jat zoi gong" || it.SubText != "" || u.SubText != "" {
		t.Errorf("没有逐字歌词的行，行级音译应照常带上、翻译为空：%+v / %+v", it, u)
	}
}

// TestRomaDetailedAbsentForSongWithKana 含假名的歌（带逐字歌词的行里有假名）经 CDP 取词后整首没有逐字音译，
// 行级音译与逐字歌词照常；假名只出现在没有逐字歌词的署名行里时不算，配得上的行照常带上。
//
// 变异自证（隔离的 worktree，实测）：lyric.FillRomaDetailed 不排除、改成只跳过含假名的那一行、判据里去掉
// 平假名 → 第一例红；改回扫所有行（不问有没有逐字歌词）→ 第二例红；条件写反（只看没有逐字歌词的行）→ 两例
// 都红。
func TestRomaDetailedAbsentForSongWithKana(t *testing.T) {
	t.Run("带逐字歌词的行含假名", func(t *testing.T) {
		kanaYRC := "[16000,1000](16000,200,0)あ(16200,200,0)り(16400,200,0)が(16600,200,0)と(16800,200,0)う\n"
		parsed, ok := resolveCDPLyrics(wireLRC+"[00:16.00]ありがとう\n", wireTlyric, wireRomalrc, wireYRC+kanaYRC, wireOffset)
		if !ok || len(parsed) != 4 {
			t.Fatalf("应解析出 4 行，got ok=%v %d 行", ok, len(parsed))
		}
		if parsed[0].RomaText != "gam maan sik mat je" || len(parsed[0].TextDetailed.Words) != 5 || len(parsed[3].TextDetailed.Words) != 5 {
			t.Fatalf("用例有误：第一行的行级音译与逐字歌词应照常取到，含假名的那一行也带逐字歌词，got %+v", parsed)
		}
		active := toExtracted(parsed)
		for i, it := range toLyricItems(active, wireOffset) {
			if js := mustJSON(t, it); len(it.RomaTextDetailed.Words) != 0 || !strings.Contains(js, `"roma_text_detailed":{}`) {
				t.Errorf("all_lyrics 第 %d 行：含假名的歌不该有逐字音译：%s", i, js)
			}
			if u := lineUpdate(i, active[i], wireOffset, 10.2, 200); len(u.RomaTextDetailed.Words) != 0 {
				t.Errorf("lyric_update 第 %d 行：含假名的歌不该有逐字音译：%+v", i, u.RomaTextDetailed)
			}
		}
	})

	t.Run("假名只在没有逐字歌词的署名行里", func(t *testing.T) {
		parsed, ok := resolveCDPLyrics("[00:00.00]作曲 : やまだ\n"+wireLRC, wireTlyric, wireRomalrc, wireYRC, wireOffset)
		if !ok || len(parsed) != 4 || len(parsed[0].TextDetailed.Words) != 0 {
			t.Fatalf("用例有误：应解析出 4 行、署名行没有逐字歌词，got ok=%v %+v", ok, parsed)
		}
		active := toExtracted(parsed)
		items := toLyricItems(active, wireOffset)
		if n := len(items[1].RomaTextDetailed.Words); n != 5 {
			t.Errorf("all_lyrics：署名行里的假名不该让配得上的行没有逐字音译，got %d 个单元", n)
		}
		if n := len(lineUpdate(1, active[1], wireOffset, 10.2, 200).RomaTextDetailed.Words); n != 5 {
			t.Errorf("lyric_update：署名行里的假名不该让配得上的行没有逐字音译，got %d 个单元", n)
		}
		if n := len(items[0].RomaTextDetailed.Words); n != 0 {
			t.Errorf("署名行自己不该有逐字音译，got %d 个单元", n)
		}
	})
}

// TestRomaDetailedAbsentForReduxLyrics Redux 回落的歌词行没有音译，逐字音译为零值；lyric_update 的 index
// 取传入的下标，不取行自带的 Index（与改成 lineUpdate 之前相同）。
//
// 变异自证（隔离的 worktree，实测）：lineUpdate 的 index 改取行自带的 Index → 红。
func TestRomaDetailedAbsentForReduxLyrics(t *testing.T) {
	redux := []cdp.ExtractedLyric{{Index: 7, Time: 10, Text: "今晚食乜嘢", SubText: "今晚吃什么"}}
	items := toLyricItems(redux, wireOffset)
	u := lineUpdate(2, redux[0], wireOffset, 10.2, 200)
	if !reflect.DeepEqual(items[0].RomaTextDetailed, player.LyricTextDetailed{}) || !reflect.DeepEqual(u.RomaTextDetailed, player.LyricTextDetailed{}) {
		t.Errorf("Redux 的歌词行不该有逐字音译：%+v / %+v", items[0].RomaTextDetailed, u.RomaTextDetailed)
	}
	if items[0].Index != 7 || items[0].RomaText != "" || items[0].SubText != "今晚吃什么" {
		t.Errorf("all_lyrics 的行其余字段不对：%+v", items[0])
	}
	if u.Index != 2 || u.RomaText != "" || u.SubText != "今晚吃什么" {
		t.Errorf("lyric_update 的 index 应取传入的下标 2，其余字段照旧：%+v", u)
	}
}
