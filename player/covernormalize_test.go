package player

// 本文件只测封面规范化的结果（issue #101，设计 §9 第 1～16、20 条）：哪些原样放行、哪些拉伸到 800×800、
// 哪些原样退回，以及 CoverDataURI 拼出的 data URI。JPEG 扫描次数的预检另见 coverscans_test.go。
//
// 变异自证（均实测，逐条记在各用例注释里）。

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"strings"
	"testing"
)

// mustNormalize 断言 normalizeCover 成功并返回输出与 MIME。
func mustNormalize(t *testing.T, src []byte, mime string) ([]byte, string) {
	t.Helper()
	out, outMIME, _, _, err := normalizeCover(src, mime)
	if err != nil {
		t.Fatalf("不应失败: %v", err)
	}
	return out, outMIME
}

// assertAsIs 断言原样退回：原字节、原 mime、带错误。
func assertAsIs(t *testing.T, src []byte, mime string) {
	t.Helper()
	out, outMIME, _, _, err := normalizeCover(src, mime)
	if err == nil {
		t.Fatal("应原样退回并报错，err 为 nil")
	}
	if !bytes.Equal(out, src) || outMIME != mime {
		t.Fatalf("原样退回应是原字节、原 mime %q，got %d 字节 %q", mime, len(out), outMIME)
	}
}

// assertResized 断言输出是 800×800 的 JPEG。
func assertResized(t *testing.T, src []byte, mime string) []byte {
	t.Helper()
	out, outMIME := mustNormalize(t, src, mime)
	if outMIME != "image/jpeg" {
		t.Fatalf("缩放后 MIME 应为 image/jpeg，got %q", outMIME)
	}
	if format, w, h := decodeSize(t, out); format != "jpeg" || w != coverSide || h != coverSide {
		t.Fatalf("输出应为 800×800 的 JPEG，got %s %dx%d", format, w, h)
	}
	return out
}

// 已是 800×800 的 JPEG / PNG 放行：原字节，MIME 按格式改正（第 1～3 条）。熵编码段截断的 800² JPEG 照样放行，
// 证明放行不解码（与今天相同：FetchCoverBase64 已按 Content-Length 拒收不完整的下载）。
//
// 变异自证：删掉 normalizeCover 里 800×800 放行那一句，四例都红（截断的那例解码失败而报错，其余被重编码）；
// 放行时 MIME 改回沿用入参，两例「标 image/jpg」红。
func TestCoverPassesThrough800(t *testing.T) {
	jpg, png := photoJPEG(t, 800, 800), photoPNG(t, 800, 800)
	cases := []struct {
		name, mime, want string
		src              []byte
	}{
		{"JPEG", "image/jpeg", "image/jpeg", jpg},
		{"JPEG 标 image/jpg", "image/jpg", "image/jpeg", jpg},
		{"PNG 标 image/jpg（网易云 CDN 的 Content-Type）", "image/jpg", "image/png", png},
		{"JPEG 熵编码段截断", "image/jpeg", "image/jpeg", jpg[:len(jpg)/3]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, mime, w, h, err := normalizeCover(c.src, c.mime)
			if err != nil {
				t.Fatalf("应放行，got err %v", err)
			}
			if !bytes.Equal(out, c.src) || mime != c.want || w != 800 || h != 800 {
				t.Fatalf("应原字节放行、MIME %q，got %d 字节 %q %dx%d", c.want, len(out), mime, w, h)
			}
		})
	}
}

// 非 800×800 的都拉伸到 800×800 编成 JPEG：缩小、放大、只差一边、PNG 源（第 4～6、8 条）。
//
// 变异自证：放行条件改成「宽或高是 800」，800×600、600×800、801×800、800×799 四例红；改成「宽是 800 且高不小于 800」，800×801、800×1200 两例红（主会话补的反例）；尺寸上限改成
// `>= coverMaxSide`，三个边长 1600 的例子红。
func TestCoverResizesTo800(t *testing.T) {
	sizes := [][2]int{{1200, 1200}, {1080, 1080}, {640, 640}, {300, 300}, {120, 120}, {1, 1},
		{800, 600}, {600, 800}, {801, 800}, {800, 799}, {800, 801}, {800, 1200}, {1200, 800}, {1600, 1600}, {1600, 1}}
	for _, s := range sizes {
		w, h := s[0], s[1]
		t.Run(fmt.Sprintf("JPEG %dx%d", w, h), func(t *testing.T) {
			assertResized(t, photoJPEG(t, w, h), "image/jpeg")
		})
	}
	for _, s := range [][2]int{{1414, 1414}, {640, 640}, {1600, 1600}} {
		t.Run(fmt.Sprintf("PNG %dx%d", s[0], s[1]), func(t *testing.T) {
			assertResized(t, photoPNG(t, s[0], s[1]), "image/png")
		})
	}
}

// 单边超过 1600、宽或高为 0 的不解码，原样退回（第 8 条）。
//
// 变异自证：上限改成 `> coverMaxSide+1`，三个 1601 的例子红；下限改成 `w < 0 || h < 0`，宽或高为 0 的
// 两例进入解码并返回成功而红。
func TestCoverSizeLimits(t *testing.T) {
	jpg := photoJPEG(t, 64, 64)
	cases := []struct {
		name string
		src  []byte
	}{
		{"1601x10", photoJPEG(t, 1601, 10)},
		{"10x1601", photoJPEG(t, 10, 1601)},
		{"1601x1601 PNG", photoPNG(t, 1601, 1601)},
		{"SOF 宽为 0", withSOFSize(t, jpg, 0, 64)},
		{"SOF 高为 0", withSOFSize(t, jpg, 64, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertAsIs(t, c.src, "image/jpeg") })
	}
}

// 非正方形拉伸、不裁切不留边（第 7 条，需求原文）：1078×875 左 1/4 红、其余蓝，输出的红蓝分界在 x≈200
// （拉伸；居中裁切会在 x≈154），首行与末行是图像内容；875×1078 上 1/4 红，分界在 y≈200。
//
// 变异自证：Scale 的源矩形改成居中的正方形（只裁横向），横例分界落在 154 而红；目标矩形改成上下留边，
// 横例首行变黑、竖例分界移到 64，两例红。
func TestCoverStretchesNonSquare(t *testing.T) {
	quarter := func(w, h int, vertical bool) []byte {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := color.RGBA{0, 0, 255, 255}
				if (!vertical && x*4 < w) || (vertical && y*4 < h) {
					c = color.RGBA{255, 0, 0, 255}
				}
				img.SetRGBA(x, y, c)
			}
		}
		return encodePNG(t, img)
	}
	boundary := func(out []byte, vertical bool) (edge int, img image.Image) {
		img, _, err := image.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < coverSide; i++ {
			x, y := i, 400
			if vertical {
				x, y = 400, i
			}
			r, _, b, _ := img.At(x, y).RGBA()
			if b > r {
				return i, img
			}
		}
		return -1, img
	}
	for _, c := range []struct {
		name     string
		w, h     int
		vertical bool
	}{{"横 1078x875", 1078, 875, false}, {"竖 875x1078", 875, 1078, true}} {
		t.Run(c.name, func(t *testing.T) {
			out := assertResized(t, quarter(c.w, c.h, c.vertical), "image/png")
			edge, img := boundary(out, c.vertical)
			if edge < 196 || edge > 204 {
				t.Fatalf("红蓝分界应在 200 附近（拉伸），got %d", edge)
			}
			for _, y := range []int{0, coverSide - 1} {
				x := 600
				if c.vertical {
					x, y = y, 600
				}
				if r, g, b, _ := img.At(x, y).RGBA(); b>>8 < 200 || r>>8 > 60 || g>>8 > 60 {
					t.Fatalf("(%d,%d) 应是蓝色的图像内容而不是留边，got rgb(%d,%d,%d)", x, y, r>>8, g>>8, b>>8)
				}
			}
		})
	}
}

// 解码炸弹：PNG 的 IHDR 改为 30000²（CRC 重算），JPEG 的 SOF 改为 65000²，都在第 5 步拦下、不解码（第 9 条）。
//
// 变异自证：删掉尺寸上限（只留宽高不小于 1），两例都红：解码按头部尺寸分配像素，PNG 一例分配约 3.6GB、
// JPEG 一例约 6.3GB；未变异时分别约 8KB、20KB。
func TestCoverHeaderBombNotDecoded(t *testing.T) {
	cases := []struct {
		name string
		src  []byte
	}{
		{"PNG 30000²", withIHDRSize(photoPNG(t, 1, 1), 30000, 30000)},
		{"JPEG 65000²", withSOFSize(t, photoJPEG(t, 16, 16), 65000, 65000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(c.src)); err != nil || cfg.Width < 30000 {
				t.Fatalf("前置：头部应读得出超大尺寸，got %v %v", cfg, err)
			}
			n := allocBytes(func() { assertAsIs(t, c.src, "image/jpeg") })
			if n > 1<<20 {
				t.Fatalf("不应进入解码，got 分配 %d 字节", n)
			}
		})
	}
}

// 头部完整而数据截断的非 800 JPEG、PNG：解码失败，原样退回（第 10 条）。
//
// 变异自证：解码失败那一支改成照常往下走，src 为 nil、Scale 处 panic 而红。
func TestCoverTruncatedNot800(t *testing.T) {
	jpg, png := photoJPEG(t, 1200, 1200), photoPNG(t, 640, 640)
	assertAsIs(t, jpg[:len(jpg)*2/3], "image/jpeg")
	assertAsIs(t, png[:len(png)/2], "image/png")
}

// CMYK JPEG（基线样本、去掉 APP14 的同一文件、手工拼的 1600² 渐进式 4 分量头）原样退回，且不进入解码
// （第 11 条）。
//
// 变异自证：删掉第 5a 步（按头部的颜色模型拦下），基线两例解码后在第 7 步兜底、仍原样退回，但分配量超过
// 门槛而红（实测解码分配约 167KB 与 104KB，未解码三例都约 18KB）；渐进式一例解码分配约 51MB、同样红。
func TestCoverCMYKNotDecoded(t *testing.T) {
	cmyk := readTestdata(t, "video-001.cmyk.jpeg")
	cases := []struct {
		name  string
		src   []byte
		limit uint64
	}{
		{"基线（带 APP14）", cmyk, 64 << 10},
		{"基线（去掉 APP14）", stripAPP14(t, cmyk), 64 << 10},
		{"渐进式 1600²", progressiveCMYKHead(1600, 1600), 1 << 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(c.src)); err != nil || cfg.ColorModel != color.CMYKModel {
				t.Fatalf("前置：头部应报 CMYK，got %v", err)
			}
			n := allocBytes(func() { assertAsIs(t, c.src, "image/jpeg") })
			if n > c.limit {
				t.Fatalf("不应进入解码，got 分配 %d 字节（门槛 %d）", n, c.limit)
			}
		})
	}
}

// 格式不在白名单（GIF：本测试 import image/gif 注册了它）、读不出头（只有 RIFF/WEBP 头、非图片字节、只有 SOI、
// 空切片）：原样退回，MIME 不变（第 12、13 条；applemusic 现有会话测试的假封面字节依赖这一条）。
//
// 变异自证：去掉格式白名单的 default 分支，GIF 被解码并缩放成 JPEG 而红。
func TestCoverUnsupportedAsIs(t *testing.T) {
	var g bytes.Buffer
	pal := image.NewPaletted(image.Rect(0, 0, 100, 100), color.Palette{color.Black, color.White})
	if err := gif.Encode(&g, pal, nil); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, mime string
		src        []byte
	}{
		{"GIF", "image/gif", g.Bytes()},
		{"RIFF/WEBP 头", "image/webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00")},
		{"非图片字节", "image/jpeg", []byte("图片字节")},
		{"只有 SOI", "image/jpeg", []byte{0xFF, 0xD8}},
		{"空切片", "image/jpeg", []byte{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertAsIs(t, c.src, c.mime) })
	}
}

// 带透明区的 PNG：输出 JPEG，透明处按黑底（第 15 条）。左半是 alpha 为 0 的白色，右半不透明的红色。
//
// 变异自证：缩放前先把目标图铺成白色、改用 Over 合成，左半变白而红。
func TestCoverTransparentOnBlack(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 400, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 400; x++ {
			c := color.NRGBA{255, 0, 0, 255}
			if x < 200 {
				c = color.NRGBA{255, 255, 255, 0}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	out := assertResized(t, encodePNG(t, img), "image/png")
	dec, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, b, _ := dec.At(200, 400).RGBA(); r>>8 > 16 || g>>8 > 16 || b>>8 > 16 {
		t.Fatalf("透明处应接近黑色，got rgb(%d,%d,%d)", r>>8, g>>8, b>>8)
	}
	if r, g, b, _ := dec.At(600, 400).RGBA(); r>>8 < 200 || g>>8 > 60 || b>>8 > 60 {
		t.Fatalf("不透明处应是红色，got rgb(%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

// 同一输入两次调用输出逐字节相同（第 16 条）：server/dedup.go 按 cover_base64 全文去重。
//
// 变异自证：编码质量改成每次调用递增（用包级计数器），第二次输出不同而红。
func TestCoverDeterministic(t *testing.T) {
	for _, src := range [][]byte{photoJPEG(t, 1200, 1200), photoPNG(t, 300, 500)} {
		a, _ := mustNormalize(t, src, "image/jpeg")
		b, _ := mustNormalize(t, src, "image/jpeg")
		if !bytes.Equal(a, b) {
			t.Fatal("同一输入两次输出应逐字节相同")
		}
	}
}

// 16 位 RGBA PNG、Adam7 隔行 PNG、渐进式 YCbCr 4:4:4 JPEG（1600² 与 1200²）：缩放成功、输出 800×800
// （第 20 条；分配量见 BenchmarkNormalizeCover）。
//
// 变异自证：jpegScanBudget 把渐进式判成顺序式（`== 0xC2` 改成 `== 0xC0`），两个渐进式 4:4:4 的 4 个扫描共 6 个
// 分量、超过帧的 3 个而被原样退回，两例红；尺寸上限改成 `>= coverMaxSide`，三个 1600² 的红。
func TestCoverUnusualSources(t *testing.T) {
	cases := []struct {
		name string
		src  []byte
	}{
		{"16 位 RGBA PNG 1600²", encodePNG(t, nrgba64Photo(1600, 1600))},
		{"Adam7 隔行 PNG 1600²", adam7PNG(t, (*image.NRGBA)(photoImage(1600, 1600, 40)))},
		{"渐进式 4:4:4 1600²", progressiveYCC444(1600, 1600)},
		{"渐进式 4:4:4 1200²", progressiveYCC444(1200, 1200)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertResized(t, c.src, "image/jpeg") })
	}
}

// CoverDataURI：空字节返回空串（第 14 条）；原样退回的拼原 mime；放行的拼改正后的 MIME；缩放的拼 JPEG。
//
// 变异自证：删掉 `len(b) == 0` 的提前返回，空字节得到 "data:image/jpeg;base64," 而红；原样退回时改用
// normalizeCover 之外写死的 image/jpeg，image/png 的那例红。
func TestCoverDataURI(t *testing.T) {
	if got := CoverDataURI("Test", nil, "image/jpeg"); got != "" {
		t.Fatalf("nil 应返回空串，got %q", got)
	}
	if got := CoverDataURI("Test", []byte{}, "image/jpeg"); got != "" {
		t.Fatalf("空切片应返回空串，got %q", got)
	}
	fake := []byte("图片字节")
	if got, want := CoverDataURI("Test", fake, "image/png"), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(fake); got != want {
		t.Fatalf("原样退回应拼原 mime，got %q want %q", got, want)
	}
	png800 := photoPNG(t, 800, 800)
	if got, want := CoverDataURI("Test", png800, "image/jpg"), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png800); got != want {
		t.Fatal("800² PNG 应原字节放行并拼 image/png")
	}
	got := CoverDataURI("Test", photoJPEG(t, 1200, 1200), "image/jpeg")
	b64, ok := strings.CutPrefix(got, "data:image/jpeg;base64,")
	if !ok {
		t.Fatalf("缩放后应拼 image/jpeg，got 前缀 %q", got[:min(len(got), 30)])
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if _, w, h := decodeSize(t, raw); w != coverSide || h != coverSide {
		t.Fatalf("应为 800×800，got %dx%d", w, h)
	}
}
