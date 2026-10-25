package lyric

// These tests guard fail-closed detailed timing without losing line lyrics.
import (
	"strings"
	"testing"
)

func TestWordFallbackInvalidHeaders(t *testing.T) {
	for _, header := range []string{"", "[kuwo:]", "[kuwo:000]", "[kuwo:12]", "[kuwo:24]", "[kuwo:8]", "[kuwo:999]", "[kuwo:-067]", "[kuwo:+067]", "[kuwo:7777777777777777777777]", "[kuwo:067]\n[kuwo:065]", "[kuwo:bad]\n[kuwo:067]"} {
		got := ParseLRC(header + "\n[00:01.000]<50,-50>甲<100,0>乙")
		if len(got) != 1 || got[0].Text != "甲乙" || got[0].TimeMs != 1000 || len(got[0].Words) != 0 {
			t.Fatalf("invalid header %q did not retain plain line: %+v", header, got)
		}
	}
}

func TestWordFallbackMalformedMarks(t *testing.T) {
	for _, body := range []string{"甲<50,-50>乙", "<50,-50>甲<bad,0>乙", "<50,-50>甲<-1,-1>乙", "<1,2>甲", "<2147483648,0>甲", "<2147483647,-2147483648>甲", "<100,0>甲<50,-50>乙", "<999999999999999999999999999,0>甲", "<50,-50>甲<0,0,0>乙", "<50,-50>甲<"} {
		got := ParseLRC("[kuwo:067]\n[00:01.000]" + body)
		if len(got) != 1 || len(got[0].Words) != 0 {
			t.Fatalf("malformed marks %q accepted: %+v", body, got)
		}
	}
}

func TestWordFallbackResourceBounds(t *testing.T) {
	if got := ParseLRC(strings.Repeat("x", maxLRCBytes+1)); got != nil {
		t.Fatal("oversized input accepted")
	}
	if got := ParseLRC("[00:00.000]" + strings.Repeat("x", maxLineBytes)); got != nil {
		t.Fatal("oversized line accepted")
	}
	got := ParseLRC("[kuwo:067]\n[00:01.000]" + strings.Repeat("<0,0>甲", maxWordsPerLine+1))
	if len(got) != 1 || len(got[0].Words) != 0 {
		t.Fatal("excessive word count did not fall back")
	}
	got = ParseLRC(strings.Repeat("[00:00.000]甲\n", maxLines+1))
	if len(got) != maxLines {
		t.Fatalf("line bound not enforced: %d", len(got))
	}
}

func TestWordFallbackInvalidLineTimes(t *testing.T) {
	for _, input := range []string{"[-1:00.000]甲", "[00:60.000]甲", "[1440:01.000]甲", "[99999999999999999999:00.000]甲", "[00:-1.000]甲", "[00:01.123oops]甲", "[00:01.-00]甲", "[00:01.]甲", "[+1:00.000]甲"} {
		if got := ParseLRC(input); len(got) != 0 {
			t.Fatalf("invalid time %q accepted: %+v", input, got)
		}
	}
}
