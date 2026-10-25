package lyric

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// syntheticResponse builds a response body the way the server does: text header,
// then zlib(base64(xor(LRC bytes))), with the LRC encoded as GB18030.
func syntheticResponse(t *testing.T, lrc string) []byte {
	t.Helper()
	enc, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(lrc))
	if err != nil {
		t.Fatal(err)
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write([]byte(base64.StdEncoding.EncodeToString(xorCycle(enc)))); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte("tp=content\r\nlrcx=1\r\n\r\n"), z.Bytes()...)
}

// The lyric body is GB18030, not GBK: Hangul, rare Han characters and emoji are
// four-byte sequences that a GBK decoder turns into replacement characters (seen
// live on a Korean song: "에스파" came out as "�2�3�2�1…").
// 变异自证：把 gb18030Decode 改回 simplifiedchinese.GBK，本用例变红（实测）。
func TestResponseDecodesGB18030FourByteText(t *testing.T) {
	for _, text := range []string{
		"에스파",             // Hangul syllables
		"𠮷野家",             // U+20BB7, a supplementary-plane Han character
		"🎵 Whiplash",      // emoji
		"室内系的 안녕 abc 123", // Chinese (two-byte), Hangul (four-byte), ASCII together
	} {
		lrc := "[00:00.762]<0,100>" + text + "\r\n"
		// The input must actually distinguish the two decoders, or this test is vacuous.
		enc, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(lrc))
		if err != nil {
			t.Fatal(err)
		}
		if gbk, _ := simplifiedchinese.GBK.NewDecoder().Bytes(enc); !strings.Contains(string(gbk), "�") && string(gbk) == lrc && strings.ContainsAny(text, "에𠮷🎵안") {
			t.Fatalf("%q: input does not exercise a four-byte sequence", text)
		}
		resp, err := DecodeResponse(syntheticResponse(t, lrc))
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if resp.LRC != lrc {
			t.Fatalf("%q decoded as %q", text, resp.LRC)
		}
		if strings.Contains(resp.LRC, "�") {
			t.Fatalf("%q: replacement character in output %q", text, resp.LRC)
		}
		lines := ParseLRC(resp.LRC)
		if len(lines) != 1 || lines[0].Text != text {
			t.Fatalf("%q parsed as %+v", text, lines)
		}
	}
}

// Old Chinese-only lyrics must decode exactly as before: GB18030 is a superset of
// GBK. The captured real response in testdata (resp_xiangni.b64, exercised by the
// parse and codec tests) is the end-to-end regression for that.
func TestResponseChineseUnchangedByGB18030(t *testing.T) {
	enc, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("[00:01.000]室内系的歌 你好，世界\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	g18030, err := simplifiedchinese.GB18030.NewDecoder().Bytes(enc)
	if err != nil {
		t.Fatal(err)
	}
	if string(g18030) != "[00:01.000]室内系的歌 你好，世界\r\n" {
		t.Fatalf("GBK bytes decoded differently by GB18030: %q", g18030)
	}
}
