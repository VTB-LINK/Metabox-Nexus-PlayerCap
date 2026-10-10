package mem

// 本文件只测「正在播放」存储的识别结果不引用输入字节：Reader 的读缓冲跨区、跨全扫复用，结果若引用它，
// 下一个区读进来就被改写。封面的拷贝另见 cover_test.go 的 TestFindNowPlayingCover。

import (
	"bytes"
	"testing"
)

// 认出一份存储后把输入整段改写，Sig、封面与各字符串都须不变。
//
// 变异自证（隔离副本，实测）：把 findNowPlaying 里 `np.Sig = append([]byte(nil), d[o:sigEnd]...)` 改成
// 直接引用 `d[o:sigEnd]`，Sig 随输入被改写而红。
func TestFindNowPlayingResultDoesNotAliasInput(t *testing.T) {
	img := fakeJPEG(3000)
	d := coverChain("留用的歌", img)
	orig := append([]byte(nil), d...)
	got := findNowPlaying(d)
	if len(got) != 1 {
		t.Fatalf("应认出 1 份，got %d", len(got))
	}
	g := got[0]
	wantSig := orig[:sigLen]
	if !bytes.Equal(g.Sig, wantSig) {
		t.Fatalf("Sig 应为链头起 %d 字节的原样内容", sigLen)
	}
	for i := range d {
		d[i] = 0xEE
	}
	if !bytes.Equal(g.Sig, wantSig) {
		t.Error("Sig 引用了输入字节，输入被改写后内容变了")
	}
	if !bytes.Equal(g.Cover, img) {
		t.Error("Cover 引用了输入字节，输入被改写后内容变了")
	}
	if g.Title != "留用的歌" || g.Artist != "歌手" || g.Album != "专辑" || g.CoverMIME != "image/jpeg" {
		t.Errorf("输入被改写后字符串变了: %q / %q / %q / %q", g.Title, g.Artist, g.Album, g.CoverMIME)
	}
}
