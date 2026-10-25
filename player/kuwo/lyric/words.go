package lyric

import (
	"strconv"
	"strings"
)

const (
	maxLRCBytes     = 2 << 20
	maxLineBytes    = 32 << 10
	maxLines        = 10000
	maxWordsPerLine = 2048
	maxTimeMs       = 24 * 60 * 60 * 1000
)

// Word is one source timing span, which can contain several characters, spaces,
// or no text. TimeMs is absolute song time including the file's [offset:] only.
// DurationMs can be zero. Gaps and overlaps are retained, not interpolated.
type Word struct {
	TimeMs     int
	DurationMs int
	Text       string
}

func parseLines(lrc string) []Line {
	if len(lrc) > maxLRCBytes {
		return nil
	}
	var c1, c2, fileShift int
	seenHeader, badHeader := false, false
	multi, badMulti := false, false
	// Tags apply to the complete file, including when offset follows the lyrics.
	for raw := range strings.SplitSeq(lrc, "\n") {
		if len(raw) > maxLineBytes {
			continue
		}
		s := strings.TrimSpace(raw)
		if v, ok := tagValue(s, "ml"); ok {
			multi = true
			badMulti = badMulti || v != "1.0"
		}
		if v, ok := tagValue(s, "kuwo"); ok {
			a, b, valid := wordCoefficients(v)
			if !valid || (seenHeader && (a != c1 || b != c2)) {
				badHeader = true
			}
			c1, c2, seenHeader = a, b, true
		}
		if v, ok := tagValue(s, "offset"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= -maxTimeMs && n <= maxTimeMs {
				fileShift = n
			}
		}
	}
	var lines []Line
	rows := strings.Split(lrc, "\n")
	previousOriginal := -2
	for i, raw := range rows {
		if len(raw) > maxLineBytes {
			continue
		}
		// Preserve spaces in timed spans. Only trim the CR belonging to CRLF.
		s := strings.TrimSuffix(raw, "\r")
		ts, rest, ok := cutTimestamp(strings.TrimLeft(s, " \t"))
		if !ok {
			continue
		}
		text := strings.TrimSpace(stripWordMarks(rest))
		if text == "" {
			continue
		}
		line := Line{RawTimeMs: ts, TimeMs: ts + fileShift, Text: text}
		if seenHeader && !badHeader {
			line.Words = parseWords(rest, line.TimeMs, c1, c2)
		}
		// Observed ml:1.0 lrcx: a zero-mark translation follows its original,
		// but carries the NEXT row's timestamp. Kuwo's handleSetCurLyric uses
		// that lookahead to append to the previous line, not the next original.
		// Keep physical adjacency, including the terminal empty timestamp row;
		// unknown/malformed formats retain line lyrics instead of guessing.
		if multi && !badMulti && previousOriginal == i-1 && len(lines) > 0 &&
			i+1 < len(rows) && len(rows[i+1]) <= maxLineBytes &&
			len(line.Words) > 0 && strings.Count(rest, "<0,0>") == len(line.Words) {
			next, _, valid := cutTimestamp(strings.TrimLeft(strings.TrimSuffix(rows[i+1], "\r"), " \t"))
			prev := &lines[len(lines)-1]
			if valid && next == ts && ts > prev.RawTimeMs {
				prev.SubText = text
				continue
			}
		}
		lines = append(lines, line)
		previousOriginal = i
		if len(lines) == maxLines {
			break
		}
	}
	return lines
}

func wordCoefficients(s string) (int, int, bool) {
	// Deliberately reject non-octal and oversized headers instead of reproducing
	// the client's permissive digit-8 parser. Unknown formats keep line lyrics.
	if len(s) == 0 || len(s) > 6 {
		return 0, 0, false
	}
	for _, r := range s {
		if r < '0' || r > '7' {
			return 0, 0, false
		}
	}
	n, err := strconv.ParseUint(s, 8, 16)
	if err != nil || n > 99 || n/10 == 0 || n%10 == 0 {
		return 0, 0, false
	}
	return int(n / 10), int(n % 10), true
}

func parseWords(s string, lineTime, c1, c2 int) []Word {
	if c1 <= 0 || c2 <= 0 || !strings.HasPrefix(s, "<") {
		return nil
	}
	var words []Word
	lastStart := int64(-1)
	for len(s) > 0 {
		if len(words) >= maxWordsPerLine || s[0] != '<' {
			return nil
		}
		end := strings.IndexByte(s, '>')
		if end < 0 || end > 24 {
			return nil
		}
		left, right, ok := strings.Cut(s[1:end], ",")
		if !ok {
			return nil
		}
		a, e1 := strconv.ParseInt(left, 10, 32)
		b, e2 := strconv.ParseInt(right, 10, 32)
		if e1 != nil || e2 != nil {
			return nil
		}
		// KwModLyric.dll: signed idiv, truncating towards zero. int64 operands
		// avoid overflow of malicious int32 input before the conservative bounds.
		start := (a + b) / int64(2*c1)
		duration := (a - b) / int64(2*c2)
		if a+b < 0 || a-b < 0 || start < lastStart || start+duration > maxTimeMs {
			return nil
		}
		s = s[end+1:]
		next := strings.IndexByte(s, '<')
		if next < 0 {
			next = len(s)
		}
		words = append(words, Word{TimeMs: lineTime + int(start), DurationMs: int(duration), Text: s[:next]})
		lastStart = start
		s = s[next:]
	}
	return words
}
