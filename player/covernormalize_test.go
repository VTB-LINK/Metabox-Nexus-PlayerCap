package player

// 本文件只测封面规范化的结果（issue #101，设计 §9 第 1～16、20 条）：哪些原样放行、哪些居中裁切缩放到 800×800、
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
	"math"
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

// 非 800×800 的都缩放到 800×800（非正方形先居中裁切）编成 JPEG：缩小、放大、只差一边、PNG 源（第 4～6、8 条）。
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

// centerSquare 取居中的正方形（issue #113）：边长取较短边，长边两端各裁一半；宽高之差为奇数时偏移向下取整，
// 多出的 1 像素在右（下）。矩形的 Min 非零时偏移要加上 Min（解码器出的图 Min 恒为 0，这一支只能在这里测）。
//
// 变异自证（均实测）：边长取较长边，非正方形各例红；横向或纵向的偏移取 Min 而不居中，各自的横、竖例红；横向偏移
// 向上取整，1078×875、1001×1000、1003×1000、1600×1 四例红；忘了加 Min，两个 Min 非零的例子红。
func TestCenterSquare(t *testing.T) {
	cases := []struct {
		name string
		in   image.Rectangle
		want image.Rectangle
	}{
		{"正方形", image.Rect(0, 0, 640, 640), image.Rect(0, 0, 640, 640)},
		{"横 1078×875", image.Rect(0, 0, 1078, 875), image.Rect(101, 0, 976, 875)},
		{"竖 875×1078", image.Rect(0, 0, 875, 1078), image.Rect(0, 101, 875, 976)},
		{"差 1 像素 1001×1000", image.Rect(0, 0, 1001, 1000), image.Rect(0, 0, 1000, 1000)},
		{"差 3 像素 1003×1000", image.Rect(0, 0, 1003, 1000), image.Rect(1, 0, 1001, 1000)},
		{"差 3 像素 1000×1003", image.Rect(0, 0, 1000, 1003), image.Rect(0, 1, 1000, 1001)},
		{"极端横 1600×100", image.Rect(0, 0, 1600, 100), image.Rect(750, 0, 850, 100)},
		{"极端竖 100×1600", image.Rect(0, 0, 100, 1600), image.Rect(0, 750, 100, 850)},
		{"1 像素高 1600×1", image.Rect(0, 0, 1600, 1), image.Rect(799, 0, 800, 1)},
		{"Min 非零 横", image.Rect(10, 20, 10+600, 20+400), image.Rect(110, 20, 510, 420)},
		{"Min 非零 竖", image.Rect(-5, 7, -5+400, 7+601), image.Rect(-5, 107, 395, 507)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := centerSquare(c.in); got != c.want {
				t.Fatalf("centerSquare(%v) = %v，期望 %v", c.in, got, c.want)
			}
		})
	}
}

// rampImage 生成 w×h 的渐变图：R 随 x 从 0 线性升到 255，G 随 y 从 0 线性升到 255，B 恒为 128。
func rampImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 255 / max(w-1, 1)), uint8(y * 255 / max(h-1, 1)), 128, 255})
		}
	}
	return img
}

// 非正方形居中裁切、不变形不留边（issue #113，第 7 条改订）：1078×875 左 1/4 红、其余蓝，居中裁出 875×875 后
// 红蓝分界在 (1078/4−101)×800/875≈154（拉伸会在 x≈200）；875×1078 上 1/4 红，分界在 y≈154。短边方向的首行与
// 末行是图像内容而不是留边。
//
// 变异自证（均实测）：Scale 的源矩形改回整幅图（拉伸），横、竖两例红（分界落在 200）；横向或纵向的偏移取 0
// （裁左或裁上），对应的例红；源矩形放宽 1 像素，两例红。
func TestCoverCropsNonSquare(t *testing.T) {
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
			if edge < 150 || edge > 158 {
				t.Fatalf("红蓝分界应在 154 附近（居中裁切），got %d", edge)
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

// 输出像素与源图的对应关系：渐变图里 R 只随 x、G 只随 y 变化，输出第 i 列取自源图的 x0+(i+0.5)×side/800−0.5，
// 逐点比对 R 与 G（容差 10 含 JPEG 与重采样误差）。覆盖横、竖、极端长宽比、宽高差为奇数、放大、缩小、只差一边
// （801×800 裁掉最右一列，不能被当成 800² 放行）。crop 是手算的裁切矩形，不经 centerSquare。
//
// 变异自证（均实测）：Scale 的源矩形改回整幅图（拉伸）、边长取较长边、偏移取 0（裁左或裁上）、源矩形放宽 1 像素，
// 本用例都有子用例红。
func TestCoverCropMapsPixels(t *testing.T) {
	cases := []struct {
		w, h int
		crop image.Rectangle
	}{
		{1078, 875, image.Rect(101, 0, 976, 875)},
		{875, 1078, image.Rect(0, 101, 875, 976)},
		{1600, 100, image.Rect(750, 0, 850, 100)},
		{100, 1600, image.Rect(0, 750, 100, 850)},
		{1001, 1000, image.Rect(0, 0, 1000, 1000)},
		{1003, 1000, image.Rect(1, 0, 1001, 1000)},
		{1000, 1003, image.Rect(0, 1, 1000, 1001)},
		{600, 400, image.Rect(100, 0, 500, 400)},
		{400, 600, image.Rect(0, 100, 400, 500)},
		{1500, 1200, image.Rect(150, 0, 1350, 1200)},
		{801, 800, image.Rect(0, 0, 800, 800)},
		{800, 801, image.Rect(0, 0, 800, 800)},
	}
	pts := []int{0, 1, 200, 399, 400, 599, 798, 799}
	near := func(a uint32, want float64) bool { return math.Abs(float64(a>>8)-want) <= 10 }
	for _, c := range cases {
		t.Run(fmt.Sprintf("%dx%d", c.w, c.h), func(t *testing.T) {
			out := assertResized(t, encodePNG(t, rampImage(c.w, c.h)), "image/png")
			dec, _, err := image.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			side := float64(c.crop.Dx())
			for _, p := range pts {
				sx := float64(c.crop.Min.X) + (float64(p)+0.5)*side/coverSide - 0.5
				sy := float64(c.crop.Min.Y) + (float64(p)+0.5)*side/coverSide - 0.5
				wantR := sx * 255 / float64(c.w-1)
				wantG := sy * 255 / float64(c.h-1)
				if r, _, _, _ := dec.At(p, 400).RGBA(); !near(r, wantR) {
					t.Errorf("输出第 %d 列 R=%d，期望约 %.0f（取自源图 x≈%.1f）", p, r>>8, wantR, sx)
				}
				if _, g, _, _ := dec.At(400, p).RGBA(); !near(g, wantG) {
					t.Errorf("输出第 %d 行 G=%d，期望约 %.0f（取自源图 y≈%.1f）", p, g>>8, wantG, sy)
				}
			}
		})
	}
}

// 被裁掉的区域不能渗进输出的边缘：1078×875 的左右各 101 与 102 列是红色、居中 875 列是蓝色，输出的首列与末列
// 必须是纯蓝（缩放核不能越过裁切矩形去取邻近的红色像素）；875×1078 的上下边同理。
//
// 变异自证（均实测）：Scale 的源矩形放宽 2 个像素（两端各 1），两例红；改回整幅图（拉伸）、边长取较长边，同样红。
func TestCoverCropDoesNotBleed(t *testing.T) {
	for _, c := range []struct {
		name     string
		w, h     int
		vertical bool
	}{{"横 1078x875", 1078, 875, false}, {"竖 875x1078", 875, 1078, true}} {
		t.Run(c.name, func(t *testing.T) {
			r := centerSquare(image.Rect(0, 0, c.w, c.h))
			img := image.NewNRGBA(image.Rect(0, 0, c.w, c.h))
			for y := 0; y < c.h; y++ {
				for x := 0; x < c.w; x++ {
					col := color.NRGBA{255, 0, 0, 255}
					if image.Pt(x, y).In(r) {
						col = color.NRGBA{0, 0, 255, 255}
					}
					img.SetNRGBA(x, y, col)
				}
			}
			dec, _, err := image.Decode(bytes.NewReader(assertResized(t, encodePNG(t, img), "image/png")))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range []int{0, coverSide - 1} {
				for _, k := range []int{0, 100, 400, 700, coverSide - 1} {
					x, y := e, k
					if c.vertical {
						x, y = k, e
					}
					if rr, g, b, _ := dec.At(x, y).RGBA(); rr>>8 > 40 || g>>8 > 40 || b>>8 < 200 {
						t.Fatalf("(%d,%d) 应是纯蓝（裁掉的红色不能渗进来），got rgb(%d,%d,%d)", x, y, rr>>8, g>>8, b>>8)
					}
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
