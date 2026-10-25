package lyric

// Translation pairing is deliberately limited to the observed ml:1.0 shape.
import (
	"strings"
	"testing"
)

func TestTranslationPairingBoundaries(t *testing.T) {
	const body = "[00:01.000]<400,-400>original\n[00:02.000]<0,0>translated\n[00:02.000]"
	const header = "[ml:1.0]\n[kuwo:054]\n"
	for _, tc := range []struct {
		name, input string
		paired      bool
	}{
		{"known", header + body, true},
		{"CRLF", strings.ReplaceAll(header+body, "\n", "\r\n"), true},
		{"trailing offset", header + body + "\n[offset:-100]", true},
		{"no multi tag", "[kuwo:054]\n" + body, false},
		{"unknown multi", "[ml:2.0]\n[kuwo:054]\n" + body, false},
		{"conflicting multi", header + "[ml:2.0]\n" + body, false},
		{"bad word header", "[ml:1.0]\n[kuwo:invalid]\n" + body, false},
		{"missing sentinel", header + strings.TrimSuffix(body, "\n[00:02.000]"), false},
		{"different next timestamp", header + strings.TrimSuffix(body, "[00:02.000]") + "[00:03.000]", false},
		{"nonzero span", header + strings.ReplaceAll(body, "<0,0>", "<8,-8>"), false},
		{"rounded zero is not marker", header + strings.ReplaceAll(body, "<0,0>", "<1,0>"), false},
		{"broken previous adjacency", header + strings.ReplaceAll(body, "original\n", "original\ninvalid\n"), false},
		{"broken next adjacency", header + strings.ReplaceAll(body, "translated\n", "translated\ninvalid\n"), false},
		{"backwards time", header + strings.ReplaceAll(body, "[00:01.000]", "[00:03.000]"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := ParseLRC(tc.input)
			want := 2
			if tc.paired {
				want = 1
			}
			if len(lines) != want || (lines[0].SubText == "translated") != tc.paired {
				t.Fatalf("paired=%v: %+v", tc.paired, lines)
			}
		})
	}
}

func TestTranslationBlankAndOrphanRows(t *testing.T) {
	input := "[ml:1.0]\n[kuwo:054]\n[00:00.000]<0,0>orphan\n[00:00.000]<400,-400>title\n" +
		"[00:01.000]<0,0> <0,0> \n[00:01.000]<400,-400>original\n[00:02.000]<0,0>translated\n[00:02.000]"
	lines := ParseLRC(input)
	if len(lines) != 3 || lines[0].Text != "orphan" || lines[1].SubText != "" || lines[2].SubText != "translated" {
		t.Fatalf("blank/orphan row misassigned: %+v", lines)
	}
}
