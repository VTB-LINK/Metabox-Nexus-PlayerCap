package qqmusic

// 本文件只放会话侧歌词状态机（lyricState）各测试共用的夹具与断言。

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// stateOffsetSec 是各用例的配置 offset：输出的 PlayTime 比起始晚 0.5 秒，可见 applyDetailedOffset 已套上。
const stateOffsetSec float32 = 0.5

// localFlac 是本地文件用例采纳的音频文件。
const localFlac = `D:\music\x.flac`

// assocFixture 是一份关联歌词：第 0 行 5 秒、带翻译与两个逐字（5、5.5 秒），第 1 行 7 秒、只有行级。tag 区分不同的几份。
func assocFixture(tag string) []lyricLine {
	return []lyricLine{
		{
			Index: 0, Time: 5, Text: tag + "1", SubText: tag + "-t",
			Detailed: player.LyricTextDetailed{Timestamp: 5, Duration: 1, Words: []player.LyricTextDetailedWord{
				{Timestamp: 5, Duration: 0.5, Text: tag}, {Timestamp: 5.5, Duration: 0.5, Text: tag},
			}},
		},
		{Index: 1, Time: 7, Text: tag + "2"},
	}
}

// 在线原始行（shiftFixture）与关联歌词按条目偏移平移、套 0.5 秒 offset 之后的样子（dumpLines 的形式）。
var (
	onlineAt0 = []string{
		"0:a||@0.200 d0.200/0.700 x:0.200/0.700 x:0.600/1.100 r0.250/0.750 x:0.250/0.750 x:0.700/1.200",
		"1:b||@2.000",
	}
	onlineDelayed500 = []string{ // 条目偏移 -500
		"0:a||@0.700 d0.700/1.200 x:0.700/1.200 x:1.100/1.600 r0.750/1.250 x:0.750/1.250 x:1.200/1.700",
		"1:b||@2.500",
	}
	onlineDelayed1s = []string{ // 条目偏移 -1000
		"0:a||@1.200 d1.200/1.700 x:1.200/1.700 x:1.600/2.100 r1.250/1.750 x:1.250/1.750 x:1.700/2.200",
		"1:b||@3.000",
	}
	onlineAdvanced500 = []string{ // 条目偏移 500：提前半秒，低于 0 的取 0
		"0:a||@0.000 d0.000/0.500 x:0.000/0.500 x:0.100/0.600 r0.000/0.500 x:0.000/0.500 x:0.200/0.700",
		"1:b||@1.500",
	}
	assocA0         = []string{"0:A1|A-t|@5.000 d5.000/5.500 A:5.000/5.500 A:5.500/6.000", "1:A2||@7.000"}
	assocADelayed2s = []string{"0:A1|A-t|@7.000 d7.000/7.500 A:7.000/7.500 A:7.500/8.000", "1:A2||@9.000"} // 条目偏移 -2000
	assocB0         = []string{"0:B1|B-t|@5.000 d5.000/5.500 B:5.000/5.500 B:5.500/6.000", "1:B2||@7.000"}
	localLDelayed1s = []string{"0:L1|L-t|@6.000 d6.000/6.500 L:6.000/6.500 L:6.500/7.000", "1:L2||@8.000"} // 条目偏移 -1000
)

// desiredRes 构造 gen 的期望状态；digest 照读取者的做法由来源、偏移与内容标识 content 算出。
func desiredRes(gen uint64, src lyricSource, lyricOffsetMs int, lines []lyricLine, content string) assocResult {
	return assocResult{
		gen: gen, kind: resDesired, source: src, lyricOffsetMs: lyricOffsetMs, lines: lines,
		digest: sha256.Sum256(fmt.Appendf(nil, "%d %d %s", src, lyricOffsetMs, content)),
	}
}

func onlineRes(gen uint64, lyricOffsetMs int) assocResult {
	return desiredRes(gen, srcOnline, lyricOffsetMs, nil, "")
}

func noneRes(gen uint64) assocResult { return desiredRes(gen, srcNone, 0, nil, "") }

func stoppedRes(gen uint64) assocResult { return assocResult{gen: gen, kind: resStopped} }

// localRes 是本地文件 localFlac 关联的歌词 assocFixture("L")，条目偏移 -1000。
func localRes(gen uint64) assocResult {
	r := desiredRes(gen, srcLocal, -1000, assocFixture("L"), "L")
	r.localPath = localFlac
	return r
}

// dumpLines 把每行写成「序号:原文|翻译|音译@行时间」，有逐字时接「d起始/播出」与各字「字:起始/播出」，音译逐字以 r 开头，
// 时间取到毫秒。
func dumpLines(lines []lyricLine) []string {
	var out []string
	for _, l := range lines {
		s := fmt.Sprintf("%d:%s|%s|%s@%.3f", l.Index, l.Text, l.SubText, l.RomaText, l.Time)
		for _, d := range []struct {
			tag string
			v   player.LyricTextDetailed
		}{{"d", l.Detailed}, {"r", l.RomaDetailed}} {
			if len(d.v.Words) == 0 {
				continue
			}
			s += fmt.Sprintf(" %s%.3f/%.3f", d.tag, d.v.Timestamp, d.v.PlayTime)
			for _, w := range d.v.Words {
				s += fmt.Sprintf(" %s:%.3f/%.3f", w.Text, w.Timestamp, w.PlayTime)
			}
		}
		out = append(out, s)
	}
	return out
}

// applyWant 以 songHasIDs 应用 res，断言是否要求重发与给出的行；不要求重发时给出的行须为空。
func applyWant(t *testing.T, s *lyricState, res assocResult, songHasIDs, changed bool, want ...string) {
	t.Helper()
	lines, ch := s.apply(res, songHasIDs, stateOffsetSec)
	checkOutput(t, "apply", lines, ch, changed, want)
}

// refreshWant 同 applyWant，对 refresh。
func refreshWant(t *testing.T, s *lyricState, songHasIDs, changed bool, want ...string) {
	t.Helper()
	lines, ch := s.refresh(songHasIDs, stateOffsetSec)
	checkOutput(t, "refresh", lines, ch, changed, want)
}

func checkOutput(t *testing.T, op string, lines []lyricLine, changed, wantChanged bool, want []string) {
	t.Helper()
	if got := dumpLines(lines); changed != wantChanged || !slices.Equal(got, want) {
		t.Fatalf("%s gave %q, changed %v\nwant %q, changed %v", op, got, changed, want, wantChanged)
	}
}

// wantSource 断言已给出的那份歌词的来源与采纳的本地路径。
func wantSource(t *testing.T, s *lyricState, src lyricSource, localPath string) {
	t.Helper()
	if s.source() != src || s.localPath() != localPath {
		t.Fatalf("source %d, localPath %q; want %d, %q", s.source(), s.localPath(), src, localPath)
	}
}
