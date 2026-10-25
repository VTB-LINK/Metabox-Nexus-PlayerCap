package lyric

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeQueryMatchesClient(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "client_query.txt"))
	if err != nil {
		t.Fatalf("读夹具失败: %v", err)
	}
	var plainB64, want string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		switch {
		case strings.HasPrefix(line, "plain_gbk_b64="):
			plainB64 = strings.TrimSpace(strings.TrimPrefix(line, "plain_gbk_b64="))
		case strings.HasPrefix(line, "query="):
			want = strings.TrimSpace(strings.TrimPrefix(line, "query="))
		}
	}
	if plainB64 == "" || want == "" {
		t.Fatalf("夹具缺字段：plain=%q query=%q", plainB64, want)
	}
	gbkPlain, err := base64.StdEncoding.DecodeString(plainB64)
	if err != nil {
		t.Fatalf("夹具 plain base64: %v", err)
	}

	if got := EncodeQuery(gbkPlain); got != want {
		t.Errorf("请求编码与客户端不一致\n got=%q\nwant=%q", got, want)
	}
	back, err := DecodeQuery(want)
	if err != nil {
		t.Fatalf("DecodeQuery: %v", err)
	}
	if !bytes.Equal(back, gbkPlain) {
		t.Errorf("解码不回头\n got=%q\nwant=%q", string(back), string(gbkPlain))
	}
	// 明文必须是 GBK：UTF-8 视图里应能读出中文歌名。
	utf8Plain, err := gb18030Decode(back)
	if err != nil {
		t.Fatalf("gb18030Decode: %v", err)
	}
	if !strings.Contains(string(utf8Plain), "室内系的TrackMaker") {
		t.Errorf("明文里没有中文歌名，GBK 视图错了: %q", string(utf8Plain))
	}
}

func TestRequestPlainBytesMatchesClientShape(t *testing.T) {
	req := Request{UID: "6088623", Song: "室内系的TrackMaker", Artist: "hanser", RID: "204419549"}
	plain, err := req.PlainBytes()
	if err != nil {
		t.Fatalf("PlainBytes: %v", err)
	}
	if !bytes.HasPrefix(plain, []byte("user=6088623,MUSIC_9.6.0.0_W1,kwmusic_web_1.exe,KwMusic&requester=localhost&req=1 ")) {
		t.Errorf("明文前缀不对: %q", string(plain))
	}
	if !bytes.HasSuffix(plain, []byte("&rid=MUSIC_204419549&lrcx=1&olrc=1")) {
		t.Errorf("rid/尾部参数不对: %q", string(plain))
	}
	// 歌名以 GBK 字节出现（"室内系的" 的 GBK 是 CA D2 C4 DA CF B5 B5 C4）。
	if !bytes.Contains(plain, []byte{0xCA, 0xD2, 0xC4, 0xDA, 0xCF, 0xB5, 0xB5, 0xC4}) {
		t.Errorf("歌名不是 GBK 字节: %q", string(plain))
	}
	u, err := req.URL()
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	if !strings.HasPrefix(u, Endpoint) {
		t.Errorf("URL 前缀不对: %q", u)
	}
}

func TestDecodeResponseRealFixture(t *testing.T) {
	b64, err := os.ReadFile(filepath.Join("testdata", "resp_xiangni.b64"))
	if err != nil {
		t.Fatalf("读夹具失败: %v", err)
	}
	body, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b64)))
	if err != nil {
		t.Fatalf("夹具 base64: %v", err)
	}
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Header["tp"] != "content" {
		t.Errorf("tp=%q，应 content", resp.Header["tp"])
	}
	if resp.Header["lrcx"] != "1" {
		t.Errorf("lrcx=%q，应 1", resp.Header["lrcx"])
	}
	if resp.Header["lrc_length"] == "" {
		t.Errorf("缺 lrc_length 头")
	}
	if !strings.HasPrefix(resp.LRC, "[kuwo:") {
		t.Errorf("LRC 首行不是 [kuwo:，实为 %q", firstLine(resp.LRC))
	}
	if !strings.Contains(resp.LRC, "[ver:v1.0]") {
		t.Errorf("LRC 里没有 [ver:v1.0]，解码链可能错位")
	}
	lines := ParseLRC(resp.LRC)
	if len(lines) < 10 {
		t.Fatalf("解析出的行数太少: %d", len(lines))
	}
	if lines[0].TimeMs != 0 {
		t.Errorf("首行时间应为 0ms，实为 %d", lines[0].TimeMs)
	}
	if strings.ContainsAny(lines[0].Text, "<>") {
		t.Errorf("逐字坐标标记没去干净: %q", lines[0].Text)
	}
	for i := 1; i < len(lines); i++ {
		if lines[i].TimeMs < lines[i-1].TimeMs {
			t.Errorf("行时间倒退: 第 %d 行 %dms < 上一行 %dms", i, lines[i].TimeMs, lines[i-1].TimeMs)
		}
	}
}

func TestDecodeResponseRejectsGarbage(t *testing.T) {
	if _, err := DecodeResponse([]byte("no separator here")); err == nil {
		t.Error("没有头分隔时应当报错")
	}
	if _, err := DecodeResponse([]byte("tp=content\r\n\r\nnot-a-zlib-stream")); err == nil {
		t.Error("载荷不是 zlib 时应当报错")
	}
}

// 没有歌词的歌：`tp=none` + 零载荷（实测 4 首歌都是 68 字节头、0 字节正文）。
// 必须报 ErrNoLyrics，不能报 zlib 错误——否则日志里全是解码失败，掩盖真故障。
func TestDecodeResponseNoLyrics(t *testing.T) {
	body := []byte("tp=none\r\ncand_lrc_count=0\r\nshow=1\r\nwiki_entry=\r\nwiki_entry_sig=0\r\n\r\n")
	resp, err := DecodeResponse(body)
	if err == nil {
		t.Fatal("应当报错")
	}
	if !errors.Is(err, ErrNoLyrics) {
		t.Errorf("应报 ErrNoLyrics，实得 %v", err)
	}
	if resp.Header["tp"] != "none" {
		t.Errorf("头仍应解析出来，tp=%q", resp.Header["tp"])
	}
	// tp 是 content 但载荷为空，同样按"没有歌词"处理
	body2 := []byte("tp=content\r\n\r\n")
	if _, err := DecodeResponse(body2); !errors.Is(err, ErrNoLyrics) {
		t.Errorf("空载荷应报 ErrNoLyrics，实得 %v", err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimRight(s[:i], "\r")
	}
	return s
}
