package qqmusic

// 本文件只测本地关联/缓存歌词加载的振假名提取、译轨隔离与预算降级。

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

func localFuriganaSkipLogPrefix() string {
	return strings.SplitN(i18n.T("Skipped QQ Music local furigana: %s"), "%", 2)[0]
}

func localFuriganaPartialLogPrefix() string {
	return strings.SplitN(i18n.T("Partially degraded QQ Music local furigana: %s"), "%", 2)[0]
}

// 变异自证（隔离克隆副本，实测）：
//   - loadLocalLyrics 加载主轨改传 withFurigana=false：加密 qrc 与手写 lrc 两例均红；
//   - loadLocalLyrics 加载译轨改传 withFurigana=true：主无 kana 译有畸形 kana 时打跳过日志而红；
//   - 删掉本地 Furigana 预算检查：字节超限降级用例红。
func TestLoadLocalLyricsFuriganaAndTransIsolation(t *testing.T) {
	t.Run("加密缓存qrc主轨提取振假名且译轨音译轨残留kana不干扰", func(t *testing.T) {
		logs := captureAssocLog(t)
		dir := t.TempDir()

		mainQRC := "[ti:心のプラカード]\n" +
			"[kana:1む1いっ1しゅん2あ(128976,624)し(129600,161)た(129761,159)]\n" +
			"[23045,3650]こ(23045,180)っ(23225,190)ち(23415,190)向(23605,200)い(23805,210)て(24015,135) (24150,135)一(25445,190)瞬(25635,210)だ(25845,520)け(26365,330)\n" +
			"[128976,2000]明(128976,500)日(129476,500)へ(129976,1000)\n"
		// 译轨拷贝了日文 [kana:] 头（与中文字数不守恒），音译轨含畸形 [kana:]。
		transQRC := "[ti:心のプラカード]\n" +
			"[kana:1む1いっ1しゅん2あ(128976,624)し(129600,161)た(129761,159)]\n" +
			"[00:23.04]朝我这边看 一瞬间就好\n" +
			"[02:08.97]向着明天\n"
		romaQRC := "[ti:心のプラカード]\n" +
			"[kana:0bad]\n" +
			"[23045,3650]ko(23045,500)tti(23545,500)\n" +
			"[128976,2000]a(128976,500)shita(129476,500)\n"

		mainPath := writeLyricFile(t, dir, "AKB48 - 心のプラカード_qm.qrc", qrcFile(t, qrcXML(mainQRC)))
		transPath := writeLyricFile(t, dir, "AKB48 - 心のプラカード_qmts.qrc", qrcFile(t, qrcXML(transQRC)))
		romaPath := writeLyricFile(t, dir, "AKB48 - 心のプラカード_qmRoma.qrc", qrcFile(t, qrcXML(romaQRC)))

		lines, failed, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{Trans: transPath, Roma: romaPath})
		if err != nil || len(failed) != 0 || len(lines) != 2 {
			t.Fatalf("loadLocalLyrics = (%d lines, failed=%+v, err=%v)，want 2 lines with no failure", len(lines), failed, err)
		}

		want0 := player.LyricFurigana{
			{Text: "こっち"},
			{Text: "向", Ruby: "む"},
			{Text: "いて "},
			{Text: "一", Ruby: "いっ"},
			{Text: "瞬", Ruby: "しゅん"},
			{Text: "だけ"},
		}
		want1 := player.LyricFurigana{
			{Text: "明日", Ruby: "あした"},
			{Text: "へ"},
		}
		if !reflect.DeepEqual(lines[0].Furigana, want0) {
			t.Errorf("line 0 Furigana = %#v\nwant %#v", lines[0].Furigana, want0)
		}
		if !reflect.DeepEqual(lines[1].Furigana, want1) {
			t.Errorf("line 1 Furigana = %#v\nwant %#v", lines[1].Furigana, want1)
		}
		if lines[0].SubText != "朝我这边看 一瞬间就好" || lines[1].SubText != "向着明天" {
			t.Errorf("SubText = (%q, %q)，译轨未正确合并", lines[0].SubText, lines[1].SubText)
		}
		if lines[0].RomaText != "kotti" || lines[1].RomaText != "ashita" {
			t.Errorf("RomaText = (%q, %q)，音译轨未正确合并", lines[0].RomaText, lines[1].RomaText)
		}
		if n := countLog(logs(), "+", localFuriganaSkipLogPrefix()); n != 0 {
			t.Errorf("正常主轨 + 残留 kana 附属轨不应打本地振假名跳过日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("手写含kana的UTF-8 LRC主轨正确提取振假名", func(t *testing.T) {
		logs := captureAssocLog(t)
		dir := t.TempDir()

		lrc := "[ti:心のプラカード]\n" +
			"[kana:1ぼく1そん1ざい1き]\n" +
			"[00:27.82]僕の存在に 気づいてよ\n" +
			"[00:35.00]だけどホントは\n"
		mainPath := writeLyricFile(t, dir, "handwritten.lrc", []byte(lrc))

		lines, failed, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{})
		if err != nil || len(failed) != 0 || len(lines) != 2 {
			t.Fatalf("loadLocalLyrics = (%d lines, failed=%+v, err=%v)，want 2 lines", len(lines), failed, err)
		}
		want0 := player.LyricFurigana{
			{Text: "僕", Ruby: "ぼく"},
			{Text: "の"},
			{Text: "存", Ruby: "そん"},
			{Text: "在", Ruby: "ざい"},
			{Text: "に "},
			{Text: "気", Ruby: "き"},
			{Text: "づいてよ"},
		}
		if !reflect.DeepEqual(lines[0].Furigana, want0) {
			t.Errorf("line 0 Furigana = %#v\nwant %#v", lines[0].Furigana, want0)
		}
		if lines[1].Furigana != nil {
			t.Errorf("纯假名行 Furigana = %#v，want nil", lines[1].Furigana)
		}
		if n := countLog(logs(), "+", localFuriganaSkipLogPrefix()); n != 0 {
			t.Errorf("合法手写 LRC 不应打跳过日志，实际 %d 条：\n%s", n, logs())
		}
		if n := countLog(logs(), "+", localFuriganaPartialLogPrefix()); n != 0 {
			t.Errorf("正常含纯假名行的手写 LRC 不应打局部降级日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("无ti头且kana紧贴QRC_XML_LyricContent头壳正常提取", func(t *testing.T) {
		logs := captureAssocLog(t)
		dir := t.TempDir()

		// 无 [ti:] 头，[kana:] 直接紧贴 <Lyric_1 ... LyricContent=" 之后。
		mainQRC := "[kana:1む]\n[23045,3650]こ(23045,180)っ(23225,190)ち(23415,190)向(23605,200)い(23805,210)て(24015,135)\n"
		mainPath := writeLyricFile(t, dir, "xmlhead_qm.qrc", qrcFile(t, qrcXML(mainQRC)))

		lines, failed, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{})
		if err != nil || len(failed) != 0 || len(lines) != 1 {
			t.Fatalf("loadLocalLyrics = (%d lines, failed=%+v, err=%v)，want 1 line", len(lines), failed, err)
		}
		want := player.LyricFurigana{
			{Text: "こっち"},
			{Text: "向", Ruby: "む"},
			{Text: "いて"},
		}
		if !reflect.DeepEqual(lines[0].Furigana, want) {
			t.Errorf("Furigana = %#v\nwant %#v", lines[0].Furigana, want)
		}
		if n := countLog(logs(), "+", localFuriganaSkipLogPrefix()); n != 0 {
			t.Errorf("合法 XML 头壳 [kana:] 不应打跳过日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("主轨无kana而译轨带kana时全部行Furigana为空且零日志", func(t *testing.T) {
		logs := captureAssocLog(t)
		dir := t.TempDir()

		mainQRC := "[ti:心のプラカード]\n[23045,3650]こ(23045,180)っ(23225,190)ち(23415,190)向(23605,200)い(23805,210)て(24015,135)\n"
		transQRC := "[ti:心のプラカード]\n[kana:1む1いっ1しゅん]\n[00:23.04]朝我这边看\n"
		romaQRC := "[ti:心のプラカード]\n[kana:0bad]\n[23045,3650]ko(23045,500)tti(23545,500)\n"

		mainPath := writeLyricFile(t, dir, "main_qm.qrc", qrcFile(t, qrcXML(mainQRC)))
		transPath := writeLyricFile(t, dir, "main_qmts.qrc", qrcFile(t, qrcXML(transQRC)))
		romaPath := writeLyricFile(t, dir, "main_qmRoma.qrc", qrcFile(t, qrcXML(romaQRC)))

		lines, failed, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{Trans: transPath, Roma: romaPath})
		if err != nil || len(failed) != 0 || len(lines) != 1 {
			t.Fatalf("loadLocalLyrics = (%d lines, failed=%+v, err=%v)，want 1 line", len(lines), failed, err)
		}
		if lines[0].Furigana != nil {
			t.Errorf("主轨无 [kana:] 时 Furigana 必须为 nil，实际 %#v", lines[0].Furigana)
		}
		if lines[0].SubText != "朝我这边看" || lines[0].RomaText != "kotti" {
			t.Errorf("译轨与音译轨应正常合并：sub=%q roma=%q", lines[0].SubText, lines[0].RomaText)
		}
		if n := countLog(logs(), "+", localFuriganaSkipLogPrefix()); n != 0 {
			t.Errorf("译轨与音译轨带 [kana:] 不应打跳过日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("本地主轨kana失配时降级为空且主歌词正常并打一条本地Detail日志", func(t *testing.T) {
		logs := captureAssocLog(t)
		dir := t.TempDir()

		lrc := "[kana:1こころ1む]\n[00:01.00]心\n"
		mainPath := writeLyricFile(t, dir, "mismatch.lrc", []byte(lrc))

		lines, failed, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{})
		if err != nil || len(failed) != 0 || len(lines) != 1 || lines[0].Text != "心" {
			t.Fatalf("loadLocalLyrics = (%+v, failed=%+v, err=%v)，want 1 main line without error", lines, failed, err)
		}
		if lines[0].Furigana != nil {
			t.Errorf("失配后 Furigana = %#v，want nil", lines[0].Furigana)
		}
		if n := countLog(logs(), "+", localFuriganaSkipLogPrefix()); n != 1 {
			t.Errorf("应打 1 条本地振假名跳过日志，实际 %d 条：\n%s", n, logs())
		}
		if n := countLog(logs(), "+", localFuriganaPartialLogPrefix()); n != 0 {
			t.Errorf("整曲失配跳过时不应再打局部降级日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("本地主轨单行未打轴纯假名尾巴局部降级时保留次行并打一条本地局部降级Detail日志", func(t *testing.T) {
		logs := captureAssocLog(t)
		dir := t.TempDir()

		mainQRC := "[ti:テスト]\n[kana:1む1こころ]\n[1000,1000]向(1000,1000)いて\n[3000,1000]心(3000,1000)\n"
		mainPath := writeLyricFile(t, dir, "partial_qm.qrc", qrcFile(t, qrcXML(mainQRC)))

		lines, failed, err := loadLocalLyrics(readShared, mainPath, assoc.LyricFiles{})
		if err != nil || len(failed) != 0 || len(lines) != 2 {
			t.Fatalf("loadLocalLyrics = (%d lines, failed=%+v, err=%v)，want 2 lines without error", len(lines), failed, err)
		}
		if lines[0].Furigana != nil {
			t.Errorf("首行局部降级后 Furigana = %#v，want nil", lines[0].Furigana)
		}
		want1 := player.LyricFurigana{{Text: "心", Ruby: "こころ"}}
		if !reflect.DeepEqual(lines[1].Furigana, want1) {
			t.Errorf("次行 Furigana = %#v，want %#v", lines[1].Furigana, want1)
		}
		if n := countLog(logs(), "+", localFuriganaSkipLogPrefix()); n != 0 {
			t.Errorf("局部降级不应打本地整曲跳过日志，实际 %d 条：\n%s", n, logs())
		}
		if n := countLog(logs(), "+", localFuriganaPartialLogPrefix()); n != 1 {
			t.Errorf("应打 1 条本地振假名局部降级 Detail 日志，实际 %d 条：\n%s", n, logs())
		}
	})

	t.Run("超行数上限早退时不做attachFurigana且判不可用", func(t *testing.T) {
		logs := captureAssocLog(t)
		dir := t.TempDir()

		var b strings.Builder
		b.WriteString("[kana:1こころ]\n") // 与 50001 行必不守恒；早退跳过 attachFurigana 故不打本地振假名日志
		for i := range maxLocalLines + 1 {
			fmt.Fprintf(&b, "[%d:%02d.00]心%d\n", i/60, i%60, i)
		}
		overPath := writeLyricFile(t, dir, "overlines.lrc", []byte(b.String()))
		lines, _, err := loadLocalLyrics(readShared, overPath, assoc.LyricFiles{})
		mustUnusable(t, "50,001 lines with kana", lines, err)
		if n := countLog(logs(), "+", localFuriganaSkipLogPrefix()); n != 0 {
			t.Errorf("超行数上限早退不应运行 attachFurigana 或打跳过日志，实际 %d 条：\n%s", n, logs())
		}
	})
}

func TestLoadLocalLyricsFuriganaBudgetDegradesGracefully(t *testing.T) {
	budgetLogLine := fmt.Sprintf(i18n.T("Skipped QQ Music local furigana: %s"), i18n.T("furigana exceeds local lyric size budget"))

	// 字节预算端到端测试：利用 GBK 编码中平假名「あ」占 2 字节、解码为 UTF-8 占 3 字节的膨胀特性，
	// 构造文件大小约 2.67 MiB（远小于 4 MiB 文件上限），但 Furigana 各段 len(Text)+len(Ruby)
	// 精确达到 4 MiB（保留）与 4 MiB + 1（清空降级、主歌词照常无错）的真实本地文件。
	// 计算：
	//   行 0：[00:00.00]心（len("心")=3, len("あ")=3 → 6 字节）
	//   行 1：[00:05.00]心 + 1,398,097 个「あ」（3 + 3 + 1,398,097*3 = 4,194,297 字节） + extra 个 'a'
	//   合计 = 6 + 4,194,297 + extra = 4,194,303 + extra；
	//   extra = 1 → 恰好 4,194,304（maxLocalTextBytes）；extra = 2 → 4,194,305（maxLocalTextBytes + 1）。
	t.Run("字节数预算恰好4MiB保留且多1字节清空全曲Furigana并保留主歌词", func(t *testing.T) {
		dir := t.TempDir()
		const kanaPadCount = 1_398_097
		pad := strings.Repeat("あ", kanaPadCount)

		for _, tc := range []struct {
			name     string
			extra    int
			wantKept bool
		}{
			{"恰好4MiB保留", 1, true},
			{"4MiB加1字节降级清空", 2, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				logs := captureAssocLog(t)
				utf8Text := "[kana:1あ1あ]\n[00:00.00]心\n[00:05.00]心" + pad + strings.Repeat("a", tc.extra) + "\n"
				gbkBytes, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(utf8Text))
				if err != nil {
					t.Fatalf("GBK 编码失败: %v", err)
				}
				if len(gbkBytes) >= maxLyricFileBytes {
					t.Fatalf("夹具前提不成立：GBK 文件大小 %d >= maxLyricFileBytes (%d)", len(gbkBytes), maxLyricFileBytes)
				}
				path := writeLyricFile(t, dir, tc.name+".lrc", gbkBytes)

				lines, failed, err := loadLocalLyrics(readShared, path, assoc.LyricFiles{})
				if err != nil || len(failed) != 0 || len(lines) != 2 {
					t.Fatalf("loadLocalLyrics = (%d lines, failed=%+v, err=%v)，want 2 main lines without error", len(lines), failed, err)
				}
				if lines[0].Text != "心" || !strings.HasPrefix(lines[1].Text, "心あ") {
					t.Fatalf("主歌词文本异常：line0=%q line1Prefix=%q", lines[0].Text, lines[1].Text[:min(12, len(lines[1].Text))])
				}
				if tc.wantKept {
					if len(lines[0].Furigana) == 0 || len(lines[1].Furigana) == 0 {
						t.Fatalf("恰好 4 MiB 应保留全部行 Furigana，实际 len=(%d, %d)", len(lines[0].Furigana), len(lines[1].Furigana))
					}
					gotBytes := 0
					for _, l := range lines {
						for _, seg := range l.Furigana {
							gotBytes += len(seg.Text) + len(seg.Ruby)
						}
					}
					if gotBytes != maxLocalTextBytes {
						t.Fatalf("夹具校验：累计字节 = %d，want 恰好 %d", gotBytes, maxLocalTextBytes)
					}
					if n := countLog(logs(), "+", budgetLogLine); n != 0 {
						t.Errorf("恰好 4 MiB 不应打预算跳过日志，实际 %d 条：\n%s", n, logs())
					}
				} else {
					for i, l := range lines {
						if l.Furigana != nil {
							t.Errorf("超字节预算后第 %d 行 Furigana 未清空，len=%d", i, len(l.Furigana))
						}
					}
					if n := countLog(logs(), "+", budgetLogLine); n != 1 {
						t.Errorf("超字节预算应打 1 条 %q 日志，实际 %d 条：\n%s", budgetLogLine, n, logs())
					}
				}
			})
		}
	})

	// 分段总数预算测试：恰好 maxLocalWords（200,000）个分段保留，多 1 个分段时清空全曲各行 Furigana 并记录日志。
	// （受 maxKanaTagBytes=256 KiB 与 maxLocalLines=50,000 约束，单文件合法解析的分段总数上界为 181,072，
	// 因此直接验证预算守门函数 enforceLocalFuriganaBudget 的精确边界。）
	t.Run("分段数预算恰好maxLocalWords保留且多1段清空全曲Furigana", func(t *testing.T) {
		makeSegs := func(n int) player.LyricFurigana {
			segs := make(player.LyricFurigana, n)
			for i := range segs {
				segs[i] = player.LyricFuriganaSegment{Text: "a", Ruby: "あ"}
			}
			return segs
		}

		for _, tc := range []struct {
			name     string
			second   int
			wantKept bool
		}{
			{"恰好200000段保留", maxLocalWords - 1, true},
			{"200001段清空全曲", maxLocalWords, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				logs := captureAssocLog(t)
				lines := []lyricLine{
					{Index: 0, Time: 0, Text: "心", Furigana: makeSegs(1)},
					{Index: 1, Time: 1, Text: "向", Furigana: makeSegs(tc.second)},
				}
				enforceLocalFuriganaBudget(lines)
				if tc.wantKept {
					if len(lines[0].Furigana) != 1 || len(lines[1].Furigana) != tc.second {
						t.Fatalf("恰好上限应保留全部分段，实际 (%d, %d)", len(lines[0].Furigana), len(lines[1].Furigana))
					}
					if n := countLog(logs(), "+", budgetLogLine); n != 0 {
						t.Errorf("恰好上限不应打预算跳过日志，实际 %d 条：\n%s", n, logs())
					}
				} else {
					for i, l := range lines {
						if l.Furigana != nil {
							t.Errorf("超分段上限后第 %d 行 Furigana 未清空，len=%d", i, len(l.Furigana))
						}
					}
					if n := countLog(logs(), "+", budgetLogLine); n != 1 {
						t.Errorf("超分段上限应打 1 条 %q 日志，实际 %d 条：\n%s", budgetLogLine, n, logs())
					}
				}
			})
		}
	})
}
