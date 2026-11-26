package qqmusic

// 本文件只测主歌词振假名从解析、偏移、附属轨隔离到对外事件载荷的端到端接线。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
)

// 真实样本片段：取自《心のプラカード》与《blue hour》（设计 §1 / §2.2 / §6.1）。
const (
	furiganaWireKanaBody = "1へん1きょく1む1とう1せい1じ" +
		"1む1いっ1しゅん" +
		"1ぼく1そん1ざい1き" +
		"1ご1びょう" +
		"2あ(128976,624)し(129600,161)た(129761,159)"

	furiganaWireMainBody = "[ti:心のプラカード]\n" +
		"[ar:AKB48]\n" +
		"[5170,1319]編(5170,160)曲(5330,170)：(5500,150)武(5650,180)藤(5830,160)星(5990,210)児(6200,289)\n" +
		"[23045,3650]こ(23045,180)っ(23225,190)ち(23415,190)向(23605,200)い(23805,210)て(24015,135) (24150,135)一(25445,190)瞬(25635,210)だ(25845,520)け(26365,330)\n" +
		"[27825,3800]僕(27825,350)の(28175,440)存(28615,370)在(28985,340)に(29325,155) (29480,155)気(30155,340)づ(30495,210)い(30705,260)て(30965,360)よ(31325,300)\n" +
		"[32000,2500]５(32000,300)秒(32300,400)で(32700,300)い(33000,500)い(33500,1000)\n" +
		"[35000,2000]だ(35000,250)け(35250,250)ど(35500,250)ホ(35750,250)ン(36000,250)ト(36250,250)は(36500,500)\n" +
		"[128976,2000]明(128976,500)日(129476,500)へ(129976,1000)\n"

	furiganaWireMainQRC = "[kana:" + furiganaWireKanaBody + "]\n" + furiganaWireMainBody

	// 实测陷阱：中文翻译轨原封不动拷贝了日文主轨的 [kana:...] 头，且行内另有汉字，绝不可触发解析或日志。
	furiganaWireTransLRC = "[ti:心のプラカード]\n" +
		"[kana:" + furiganaWireKanaBody + "]\n" +
		"[00:05.17]//\n" +
		"[00:23.04]朝我这边看 一瞬间就好\n" +
		"[00:27.82]请察觉到我的存在吧\n" +
		"[00:32.00]五秒就好\n" +
		"[00:35.00]可是其实\n" +
		"[02:08.97]向着明天\n"

	furiganaWireRomaQRC = "[ti:心のプラカード]\n" +
		"[5170,1319]//(5170,1319)\n" +
		"[23045,3650]ko 'c chi (23045,560)mu i te  (23605,680)i 's shu n (25445,400)da ke(25845,850)\n" +
		"[27825,3800]bo ku no (27825,790)so n za i ni  (28615,1020)ki zu i te yo(30155,1470)\n" +
		"[32000,2500]go (32000,300)byo u (32300,400)de i i(32700,1800)\n" +
		"[35000,2000]da ke do (35000,750)ho n to wa(35750,1250)\n" +
		"[128976,2000]a shi ta (128976,1000)e(129976,1000)\n"
)

func furiganaSkipLogPrefix() string {
	return strings.SplitN(i18n.T("Skipped QQ Music furigana: %s"), "%", 2)[0]
}

func furiganaPartialLogPrefix() string {
	return strings.SplitN(i18n.T("Partially degraded QQ Music furigana: %s"), "%", 2)[0]
}

// 变异自证（隔离克隆副本，实测）：
//   - 删掉 toLyricLines 的 out[i].Furigana = l.Furigana：端到端断言 all_lyrics.lyrics[i].furigana 红；
//   - 删掉 lineUpdate 的 u.Furigana = l.Furigana：端到端断言 lyric_update.furigana 红；
//   - mergeTrans 改回调用带振假名提取的 parseLRC：译轨含畸形/失配 [kana:] 时打印跳过日志而红。
func TestParseLRCAndWireFuriganaEndToEnd(t *testing.T) {
	t.Run("真实QRC主轨+含残留kana译轨+QRC罗马音经offset到对外事件载荷", func(t *testing.T) {
		logs := captureAssocLog(t)

		lines, name, singer := parseLRC(furiganaWireMainQRC, 246099)
		if name != "心のプラカード" || singer != "AKB48" {
			t.Fatalf("元数据 = (%q, %q)，want (心のプラカード, AKB48)", name, singer)
		}
		if len(lines) != 6 {
			t.Fatalf("len(lines) = %d，want 6", len(lines))
		}
		mergeTrans(lines, furiganaWireTransLRC)
		mergeRoma(lines, furiganaWireRomaQRC)
		applyDetailedOffset(lines, -0.1)

		// 无 [kana:] 对照基线：验证既有字段逐字一致、零污染。
		base, _, _ := parseLRC(furiganaWireMainBody, 246099)
		mergeTrans(base, furiganaWireTransLRC)
		mergeRoma(base, furiganaWireRomaQRC)
		applyDetailedOffset(base, -0.1)

		wantFurigana := []player.LyricFurigana{
			{
				{Text: "編", Ruby: "へん"},
				{Text: "曲", Ruby: "きょく"},
				{Text: "："},
				{Text: "武", Ruby: "む"},
				{Text: "藤", Ruby: "とう"},
				{Text: "星", Ruby: "せい"},
				{Text: "児", Ruby: "じ"},
			},
			{
				{Text: "こっち"},
				{Text: "向", Ruby: "む"},
				{Text: "いて "},
				{Text: "一", Ruby: "いっ"},
				{Text: "瞬", Ruby: "しゅん"},
				{Text: "だけ"},
			},
			{
				{Text: "僕", Ruby: "ぼく"},
				{Text: "の"},
				{Text: "存", Ruby: "そん"},
				{Text: "在", Ruby: "ざい"},
				{Text: "に "},
				{Text: "気", Ruby: "き"},
				{Text: "づいてよ"},
			},
			{
				{Text: "５", Ruby: "ご"},
				{Text: "秒", Ruby: "びょう"},
				{Text: "でいい"},
			},
			nil, // 纯假名行：だけどホントは
			{
				{Text: "明日", Ruby: "あした"},
				{Text: "へ"},
			},
		}

		items := toLyricLines(lines, -0.1)
		baseItems := toLyricLines(base, -0.1)
		if len(items) != len(wantFurigana) {
			t.Fatalf("len(items) = %d，want %d", len(items), len(wantFurigana))
		}

		for i := range items {
			if !reflect.DeepEqual(items[i].Furigana, wantFurigana[i]) {
				t.Errorf("all_lyrics[%d].Furigana = %#v\nwant %#v", i, items[i].Furigana, wantFurigana[i])
			}
			u := lineUpdate(i, lines[i], -0.1, items[i].Timestamp, 246.099)
			if !reflect.DeepEqual(u.Furigana, items[i].Furigana) {
				t.Errorf("行 %d：lyric_update.Furigana = %#v，want 与 all_lyrics 相等 %#v", i, u.Furigana, items[i].Furigana)
			}

			// 既有字段与无 [kana:] 基线逐字一致。
			if items[i].Text != baseItems[i].Text ||
				items[i].SubText != baseItems[i].SubText ||
				items[i].RomaText != baseItems[i].RomaText ||
				items[i].Timestamp != baseItems[i].Timestamp ||
				items[i].PlayTime != baseItems[i].PlayTime ||
				!reflect.DeepEqual(items[i].TextDetailed, baseItems[i].TextDetailed) ||
				!reflect.DeepEqual(items[i].RomaTextDetailed, baseItems[i].RomaTextDetailed) {
				t.Errorf("行 %d 既有字段被振假名改动：\n got %+v\nbase %+v", i, items[i], baseItems[i])
			}
			if u.Text != baseItems[i].Text ||
				u.SubText != baseItems[i].SubText ||
				u.RomaText != baseItems[i].RomaText ||
				!reflect.DeepEqual(u.TextDetailed, baseItems[i].TextDetailed) ||
				!reflect.DeepEqual(u.RomaTextDetailed, baseItems[i].RomaTextDetailed) {
				t.Errorf("行 %d lyric_update 既有字段被振假名改动：\n got %+v\nbase %+v", i, u, baseItems[i])
			}
		}

		// 纯假名行（行 4）在 all_lyrics 与 lyric_update 序列化中均为 "furigana":[]。
		lineJSON, err := json.Marshal(items[4])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(lineJSON), `"furigana":[]`) {
			t.Errorf("纯假名行 LyricLine JSON = %s，want 包含 \"furigana\":[]", lineJSON)
		}
		updJSON, err := json.Marshal(lineUpdate(4, lines[4], -0.1, 35.0, 246.099))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(updJSON), `"furigana":[]`) {
			t.Errorf("纯假名行 LyricUpdate JSON = %s，want 包含 \"furigana\":[]", updJSON)
		}

		if n := countLog(logs(), "+", furiganaSkipLogPrefix()); n != 0 {
			t.Errorf("正常含 [kana:] 主轨 + 残留 [kana:] 译轨不应打振假名跳过日志，实际 %d 条：\n%s", n, logs())
		}
		if n := countLog(logs(), "+", furiganaPartialLogPrefix()); n != 0 {
			t.Errorf("正常含纯假名行的歌曲不应打振假名局部降级日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("无时间戳纯文本均分兜底不提取振假名", func(t *testing.T) {
		logs := captureAssocLog(t)
		raw := "[kana:1こころ1む]\n心\n向いて\n"
		lines, _, _ := parseLRC(raw, 8000)
		if len(lines) != 2 || lines[0].Text != "心" || lines[1].Text != "向いて" {
			t.Fatalf("纯文本均分结果 = %+v，want 2 行 [心, 向いて]", lines)
		}
		for i, l := range lines {
			if l.Furigana != nil {
				t.Errorf("均分兜底行 %d 不应产出 Furigana，实际 %#v", i, l.Furigana)
			}
		}
		if n := countLog(logs(), "+", furiganaSkipLogPrefix()); n != 0 {
			t.Errorf("均分兜底不应打振假名跳过日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("国语粤语韩语与无kana日语歌每行均为空数组且零日志", func(t *testing.T) {
		cases := []struct {
			name string
			lrc  string
		}{
			{"国语歌", "[00:01.00]故事的小黄花\n[00:04.00]从出生那年就飘着\n"},
			{"粤语歌", "[1000,2000]今(1000,500)天(1500,500)我(2000,1000)\n"},
			{"韩语歌", "[00:01.00]뭐해라는 두 글자에\n"},
			{"无kana日语歌", "[1000,2000]も(1000,500)っ(1500,500)と(2000,500)会(2500,500)い(3000,500)た(3500,500)い(4000,500)\n"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				logs := captureAssocLog(t)
				lines, _, _ := parseLRC(tc.lrc, 60000)
				if len(lines) == 0 {
					t.Fatalf("用例前提不成立：解析出 0 行")
				}
				items := toLyricLines(lines, 0)
				for i, it := range items {
					if it.Furigana != nil {
						t.Errorf("行 %d Furigana = %#v，want nil", i, it.Furigana)
					}
					b, err := json.Marshal(it)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(b), `"furigana":[]`) {
						t.Errorf("行 %d JSON = %s，want \"furigana\":[]", i, b)
					}
				}
				if n := countLog(logs(), "+", furiganaSkipLogPrefix()); n != 0 {
					t.Errorf("%s 无 [kana:] 不应打跳过日志，实际 %d 条：\n%s", tc.name, n, logs())
				}
			})
		}
	})

	t.Run("主歌词无kana而翻译轨与音译轨带kana时输出全空且零日志", func(t *testing.T) {
		logs := captureAssocLog(t)
		lines, _, _ := parseLRC(furiganaWireMainBody, 246099)
		if len(lines) != 6 {
			t.Fatalf("len(lines) = %d，want 6", len(lines))
		}
		// 翻译轨带拷贝的合法/不守恒 [kana:]；音译轨带畸形 [kana:0bad]。
		mergeTrans(lines, furiganaWireTransLRC)
		mergeRoma(lines, "[kana:0bad]\n"+furiganaWireRomaQRC)

		for i, l := range lines {
			if l.Furigana != nil {
				t.Errorf("行 %d Furigana = %#v，want nil", i, l.Furigana)
			}
		}
		if lines[1].SubText != "朝我这边看 一瞬间就好" || lines[1].RomaText == "" {
			t.Errorf("附属轨合并应照常：sub=%q roma=%q", lines[1].SubText, lines[1].RomaText)
		}
		if n := countLog(logs(), "+", furiganaSkipLogPrefix()); n != 0 {
			t.Errorf("附属轨残留/畸形 [kana:] 必须被彻底忽略且零日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("主歌词kana语法非法或失配时降级为空并打一条Detail日志", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			lrc  string
		}{
			{"语法非法", "[kana:0あ]\n[1000,1000]心(1000,1000)\n"},
			{"词条数失配", "[kana:1こころ1む]\n[1000,1000]心(1000,1000)\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				logs := captureAssocLog(t)
				lines, _, _ := parseLRC(tc.lrc, 10000)
				if len(lines) != 1 || lines[0].Text != "心" {
					t.Fatalf("主歌词应正常返回，got %+v", lines)
				}
				if lines[0].Furigana != nil {
					t.Errorf("降级后 Furigana = %#v，want nil", lines[0].Furigana)
				}
				if n := countLog(logs(), "+", furiganaSkipLogPrefix()); n != 1 {
					t.Errorf("应打 1 条振假名跳过 Detail 日志，实际 %d 条：\n%s", n, logs())
				}
				if n := countLog(logs(), "+", furiganaPartialLogPrefix()); n != 0 {
					t.Errorf("整曲跳过时不应再打局部降级日志，实际 %d 条：\n%s", n, logs())
				}
			})
		}
	})

	t.Run("单行未打轴纯假名尾巴局部降级时保留次行并打一条局部降级Detail日志", func(t *testing.T) {
		logs := captureAssocLog(t)
		raw := "[kana:1む1こころ]\n[1000,1000]向(1000,1000)いて\n[3000,1000]心(3000,1000)\n"
		lines, _, _ := parseLRC(raw, 10000)
		if len(lines) != 2 {
			t.Fatalf("len(lines) = %d，want 2", len(lines))
		}
		if lines[0].Furigana != nil {
			t.Errorf("首行局部降级后 Furigana = %#v，want nil", lines[0].Furigana)
		}
		want1 := player.LyricFurigana{{Text: "心", Ruby: "こころ"}}
		if !reflect.DeepEqual(lines[1].Furigana, want1) {
			t.Errorf("次行 Furigana = %#v，want %#v", lines[1].Furigana, want1)
		}
		if n := countLog(logs(), "+", furiganaSkipLogPrefix()); n != 0 {
			t.Errorf("局部降级不应打整曲跳过日志，实际 %d 条：\n%s", n, logs())
		}
		if n := countLog(logs(), "+", furiganaPartialLogPrefix()); n != 1 {
			t.Errorf("应打 1 条在线振假名局部降级 Detail 日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("含纯假名行与空跳过词条行的正常歌曲零日志", func(t *testing.T) {
		logs := captureAssocLog(t)
		raw := "[kana:11こころ]\n[1000,1000]心(1000,1000)\n[2000,1000]だ(2000,500)け(2500,500)\n[3000,1000]心(3000,1000)\n"
		lines, _, _ := parseLRC(raw, 10000)
		if len(lines) != 3 {
			t.Fatalf("len(lines) = %d，want 3", len(lines))
		}
		if lines[0].Furigana != nil || lines[1].Furigana != nil {
			t.Errorf("空跳过词条行与纯假名行 Furigana 应为 nil，got (%#v, %#v)", lines[0].Furigana, lines[1].Furigana)
		}
		want2 := player.LyricFurigana{{Text: "心", Ruby: "こころ"}}
		if !reflect.DeepEqual(lines[2].Furigana, want2) {
			t.Errorf("第 2 行 Furigana = %#v，want %#v", lines[2].Furigana, want2)
		}
		if n := countLog(logs(), "+", furiganaSkipLogPrefix()); n != 0 {
			t.Errorf("正常歌曲不应打跳过日志，实际 %d 条：\n%s", n, logs())
		}
		if n := countLog(logs(), "+", furiganaPartialLogPrefix()); n != 0 {
			t.Errorf("含纯假名行与空跳过词条行的正常歌曲不应打局部降级日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("附属轨仅含XML包装kana元数据行时丢弃且不污染正文与日志", func(t *testing.T) {
		cases := []struct {
			name string
			aux  string
		}{
			{"合法XML包装kana", `<Lyric_1 LyricContent="[kana:1む]"/>`},
			{"畸形XML包装kana", `<Lyric_1 LyricContent="[kana:0bad]"/>`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				logs := captureAssocLog(t)
				lines, _, _ := parseLRC("[00:00.00]向いて", 10000)
				if len(lines) != 1 || lines[0].Text != "向いて" {
					t.Fatalf("主轨解析失败：%#v", lines)
				}
				mergeTrans(lines, tc.aux)
				mergeRoma(lines, tc.aux)

				if lines[0].SubText != "" {
					t.Errorf("SubText = %q，want 空串", lines[0].SubText)
				}
				if lines[0].RomaText != "" {
					t.Errorf("RomaText = %q，want 空串", lines[0].RomaText)
				}
				if lines[0].Furigana != nil {
					t.Errorf("Furigana = %#v，want nil", lines[0].Furigana)
				}
				if n := countLog(logs(), "+", furiganaSkipLogPrefix()); n != 0 {
					t.Errorf("附属轨 XML [kana:] 不应打振假名跳过日志，实际 %d 条：\n%s", n, logs())
				}
				if n := countLog(logs(), "+", furiganaPartialLogPrefix()); n != 0 {
					t.Errorf("附属轨 XML [kana:] 不应打振假名局部降级日志，实际 %d 条：\n%s", n, logs())
				}
			})
		}
	})
}
