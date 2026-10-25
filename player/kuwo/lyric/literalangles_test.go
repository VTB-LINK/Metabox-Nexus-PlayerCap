package lyric

import "testing"

// Only signed decimal timing pairs are markup; ordinary angle text survives.
func TestLiteralAnglesSurviveWordMarkStripping(t *testing.T) {
	for input, want := range map[string]string{
		"I <3 you": "I <3 you", "<3 you": "<3 you", "a > b": "a > b",
		"<label> lyric": "<label> lyric", "<1,2>x<broken>y": "x<broken>y",
		"<<1,2>x": "<x", "<1,x>text": "<1,x>text", "<1,2,3>text": "<1,2,3>text",
		"<+1,-2>text": "text", "<1.5,2>text": "<1.5,2>text", "<,2>text": "<,2>text",
		"<00000000001,0>text": "text", "<2147483648,0>text": "<2147483648,0>text",
	} {
		if got := stripWordMarks(input); got != want {
			t.Errorf("%q => %q, want %q", input, got, want)
		}
		lines := ParseLRC("[00:01.000]" + input)
		if len(lines) != 1 || lines[0].Text != want {
			t.Errorf("lost line for %q: %+v", input, lines)
		}
	}
}
