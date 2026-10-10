package mem

// 本文件只测封面提取：从「正在播放」存储的 #27（VT_STREAM）取出图片字节，校验长度与首尾标记。
//
// 流的头部布局用真机夹具 testdata/nowplaying_full.bin 核对（该夹具的图片被裁到 16 字节，只能证明
// 布局读得对、不能当一张合法图片）；合法与非法图片的各种形态用合成链覆盖。

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// streamValue 编一个 VT_STREAM 属性值：[ValueSize][PropID][Reserved][VT 0x42][pad]
// [流名字节数][流名 UTF-16 含 0][补齐到 4][流长度][流内容][补齐到 4]。
func streamValue(id uint32, name string, data []byte) []byte {
	u := append(utf16.Encode([]rune(name)), 0)
	v := make([]byte, 13)
	binary.LittleEndian.PutUint32(v[4:], id)
	binary.LittleEndian.PutUint16(v[9:], vtStream)
	v = binary.LittleEndian.AppendUint32(v, uint32(2*len(u)))
	for _, c := range u {
		v = binary.LittleEndian.AppendUint16(v, c)
	}
	for (len(v)-13)%4 != 0 {
		v = append(v, 0)
	}
	v = binary.LittleEndian.AppendUint32(v, uint32(len(data)))
	v = append(v, data...)
	for len(v)%4 != 0 {
		v = append(v, 0)
	}
	binary.LittleEndian.PutUint32(v, uint32(len(v)))
	return v
}

// coverChain 编一条带封面的完整「正在播放」链（媒体类别 > 标题+封面 > 歌手 > 应用名）。
func coverChain(title string, img []byte) []byte {
	return buildChain(
		storageBytes(fmtidMediaClass, lpwstr(13, "{D1607DBC-E323-4BE2-86A1-48A42A28441E}")),
		storageBytes(fmtidSummary, lpwstr(pidTitle, title), streamValue(pidThumbnail, "prop4294967295", img)),
		storageBytes(fmtidMusic, lpwstr(pidArtist, "歌手 — 专辑")),
		storageBytes(fmtidAppName, lpwstr(pidAppName, "Apple Music")),
	)
}

func fakeJPEG(n int) []byte {
	b := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x5A}, n)...)
	return append(b, 0xFF, 0xD9)
}

func fakePNG(n int) []byte {
	b := append(append([]byte{}, pngSig...), bytes.Repeat([]byte{0x5A}, n)...)
	return append(b, pngEnd...)
}

// 真机夹具：流头部按文件头写的布局读出，正好是被裁剩的 16 字节、以 JPEG 的 FFD8FF 开头。
// 裁过的图片没有结尾标记，不当封面发出（Cover 为 nil）。
//
// 变异自证：把 streamBytes 里流名之后「补齐到 4 字节」去掉，长度字段读错位、取不出这 16 字节而红。
func TestStreamLayoutOnRealStore(t *testing.T) {
	d := readFixture(t, "nowplaying_full.bin")
	i := bytes.Index(d, fmtidSummary[:])
	if i < 8 {
		t.Fatal("夹具里找不到标题段")
	}
	st, ok := parseStorage(d, i-8)
	if !ok {
		t.Fatal("标题段解析失败")
	}
	blob := st.blobs[pidThumbnail]
	if len(blob) != 16 || !bytes.HasPrefix(blob, jpegSOI) {
		t.Fatalf("封面流应为 16 字节、以 FFD8FF 开头，got %d 字节 % x", len(blob), blob)
	}
	if got := findNowPlaying(d); len(got) != 1 || got[0].Cover != nil || got[0].CoverMIME != "" {
		t.Fatalf("被裁掉结尾的图片不应当封面发出：%d 份，cover=%d 字节", len(got), len(got[0].Cover))
	}
}

// 合法的 JPEG / PNG 原样取出（逐字节相同、MIME 正确），且是拷贝——改动扫描缓冲不影响已取出的封面。
//
// 变异自证：把 findNowPlaying 里拷贝封面的 append([]byte(nil), cover...) 改成直接引用 cover，
// 「改动缓冲后仍相同」一例变红。
func TestFindNowPlayingCover(t *testing.T) {
	cases := []struct {
		name string
		img  []byte
		mime string
	}{
		{"JPEG", fakeJPEG(3000), "image/jpeg"},
		{"PNG", fakePNG(3001), "image/png"}, // 奇数长度：流内容之后要补齐
		{"JPEG 结尾后有补齐的 0", append(fakeJPEG(500), 0, 0), "image/jpeg"},
	}
	for _, c := range cases {
		d := coverChain("带封面的歌", c.img)
		got := findNowPlaying(d)
		if len(got) != 1 {
			t.Fatalf("%s: 应认出 1 份，got %d", c.name, len(got))
		}
		if !bytes.Equal(got[0].Cover, c.img) || got[0].CoverMIME != c.mime || !got[0].Full {
			t.Fatalf("%s: cover=%d 字节 mime=%q full=%v，want %d 字节 %s", c.name, len(got[0].Cover), got[0].CoverMIME, got[0].Full, len(c.img), c.mime)
		}
		for i := range d {
			d[i] = 0
		}
		if !bytes.Equal(got[0].Cover, c.img) {
			t.Fatalf("%s: 封面引用了扫描缓冲，缓冲被改写后内容变了", c.name)
		}
	}
}

// 不合法的图片不当封面发出，但不影响认出这首歌（标题照取）：没有结尾标记（存储正被改写、只读到半张）、
// 不是 JPEG/PNG、超过上限。
//
// 变异自证：把 imageMIME 里 JPEG 的结尾校验去掉，「没有结尾标记」一例带出封面而红；去掉大小上限，
// 「超过上限」一例变红。
func TestFindNowPlayingRejectsBadCover(t *testing.T) {
	half := fakeJPEG(3000)
	half = half[:len(half)-2] // 去掉 FFD9
	cases := []struct {
		name string
		img  []byte
	}{
		{"没有结尾标记", half},
		{"不是图片", bytes.Repeat([]byte{0x11}, 2000)},
		{"超过上限", fakeJPEG(maxCoverBytes)},
		{"只有首尾标记", []byte{0xFF, 0xD8, 0xFF, 0xFF, 0xD9}[:4]},
	}
	for _, c := range cases {
		got := findNowPlaying(coverChain("歌", c.img))
		if len(got) != 1 || got[0].Title != "歌" {
			t.Fatalf("%s: 封面不合法也应认出这首歌，got %+v", c.name, len(got))
		}
		if got[0].Cover != nil || got[0].CoverMIME != "" {
			t.Fatalf("%s: 不应带出封面，got %d 字节 %q", c.name, len(got[0].Cover), got[0].CoverMIME)
		}
	}
}

// 流的长度字段指到值的外面（被覆盖的残片、任意字节）：拒收，不越界、不 panic。
//
// 变异自证：把 streamBytes 里 `n > len(v)-o-4` 的检查去掉，截到 40 字节时取出了 106 字节而红。
func TestStreamBytesBounds(t *testing.T) {
	good := streamValue(pidThumbnail, "prop4294967295", fakeJPEG(100))[13:]
	if b, ok := streamBytes(good); !ok || len(b) != 106 {
		t.Fatalf("对照组应取出 106 字节，got %d ok=%v", len(b), ok)
	}
	for n := 0; n < len(good); n++ {
		if b, ok := streamBytes(good[:n]); ok && len(b) > n {
			t.Fatalf("截到 %d 字节取出了 %d 字节", n, len(b))
		}
	}
	huge := append([]byte{}, good...)
	binary.LittleEndian.PutUint32(huge, 0xFFFFFFF0) // 流名长度巨大
	if _, ok := streamBytes(huge); ok {
		t.Fatal("流名长度超出值的范围应拒收")
	}
	o := (4 + 30 + 3) &^ 3
	huge = append([]byte{}, good...)
	binary.LittleEndian.PutUint32(huge[o:], 0x7FFFFFFF) // 流长度巨大
	if _, ok := streamBytes(huge); ok {
		t.Fatal("流长度超出值的范围应拒收")
	}
}
