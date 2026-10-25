package lyric

// These numerical excerpts were independently fetched on 2026-10-23 and
// cross-checked against KwModLyric.dll's signed integer conversion. Expected
// milliseconds are constants, not computed with the implementation formula.
import (
	"encoding/base64"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestWordTimingRealCoefficientVariants(t *testing.T) {
	cases := []struct {
		name, header, body string
		want               []Word
	}{
		{"491280316", "104", "<4000,-4000>你<3256,2744>的<3464,2920>背", []Word{{1000, 500, "你"}, {1500, 32, "的"}, {1532, 34, "背"}}},
		{"658118183", "065", "<153,-153>湮<408,102>灭 <663,357>(《", []Word{{1000, 51, "湮"}, {1051, 51, "灭 "}, {1102, 51, "(《"}}},
		{"568213041", "0115", "<2464,-2464>作<4928,0>词<7392,2464>：", []Word{{1000, 352, "作"}, {1352, 352, "词"}, {1704, 352, "："}}},
		{"204419549", "126", "<360,-360>室<840,120>内<1320,600>系", []Word{{1000, 60, "室"}, {1060, 60, "内"}, {1120, 60, "系"}}},
		{"fixture_xiangni", "067", "<745,-745>想<1490,0>你<2235,745>爱", []Word{{1000, 149, "想"}, {1149, 149, "你"}, {1298, 149, "爱"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := ParseLRC("[kuwo:" + c.header + "]\n[00:01.000]" + c.body)
			if len(lines) != 1 || !reflect.DeepEqual(lines[0].Words, c.want) {
				t.Fatalf("got %+v; want words %+v", lines, c.want)
			}
		})
	}
}

func TestWordTimingPreservesSourceSpansAndGaps(t *testing.T) {
	// Real 491280316 zero-time punctuation and 568213041 one-ms overlap.
	cases := []struct {
		input string
		want  []Word
	}{
		{"[kuwo:104]\n[00:00.000]<1704,-1704>词<1278,1278>：<2662,-106>林", []Word{{0, 213, "词"}, {213, 0, "："}, {213, 173, "林"}}},
		{"[kuwo:0115]\n[00:32.310]<1680,-1680>日<3353,-7>子", []Word{{32310, 240, "日"}, {32549, 240, "子"}}},
		{"[kuwo:065]\n[00:08.934]<2586,-2586>甲<11009,8441>乙", []Word{{8934, 862, "甲"}, {10879, 428, "乙"}}},
		// Synthetic layout regression: empty spans, leading/trailing spaces and
		// multiple glyphs stay attached to their own timing, including zero time.
		{"[kuwo:067]\n[00:00.000]<50,-50><100,0> 两字 <100,100> ", []Word{{0, 10, ""}, {10, 10, " 两字 "}, {20, 0, " "}}},
	}
	for _, c := range cases {
		got := ParseLRC(c.input)
		if len(got) != 1 || !reflect.DeepEqual(got[0].Words, c.want) {
			t.Fatalf("%q: got %+v, want %+v", c.input, got, c.want)
		}
	}
}

func TestWordTimingFileOffsetIsNotConfigurationDelay(t *testing.T) {
	for _, input := range []string{
		"[kuwo:067]\n[offset:-500]\n[00:01.000]<50,-50>甲<100,0>乙",
		"[00:01.000]<50,-50>甲<100,0>乙\n[offset:-500]\n[kuwo:067]",
	} {
		got := ParseLRC(input)
		if len(got) != 1 || got[0].RawTimeMs != 1000 || got[0].TimeMs != 500 || !reflect.DeepEqual(got[0].Words, []Word{{500, 10, "甲"}, {510, 10, "乙"}}) {
			t.Fatalf("source offset not applied exactly once: %+v", got)
		}
	}
}

func TestWordTimingRealResponseFixture(t *testing.T) {
	b64, err := os.ReadFile("testdata/resp_xiangni.b64")
	if err != nil {
		t.Fatal(err)
	}
	body, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b64)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	lines := ParseLRC(resp.LRC)
	count := 0
	for _, line := range lines {
		count += len(line.Words)
	}
	if len(lines) != 35 || count != 413 || lines[0].Words[0] != (Word{0, 149, "想"}) {
		t.Fatalf("fixture timing lost: lines=%d words=%d first=%+v", len(lines), count, lines[0])
	}
}

func TestWordTimingIntegerTruncation(t *testing.T) {
	// Intentional non-divisible synthetic input checks native idiv semantics.
	got := ParseLRC("[kuwo:067]\n[00:00.000]<13,-2>甲")
	if len(got) != 1 || !reflect.DeepEqual(got[0].Words, []Word{{1, 1, "甲"}}) {
		t.Fatalf("expected truncation, got %+v", got)
	}
}
