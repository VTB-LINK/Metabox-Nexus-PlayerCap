package player

// 本文件只放封面规范化测试的共用夹具：现场生成的 JPEG / PNG，以及 image/jpeg、image/png 编不出来、
// 只能手工拼的形态（渐进式与重复扫描的 JPEG、4 分量 SOF、Adam7 隔行 PNG）。本身不含测试。

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// photoImage 生成 w×h 的「照片样」图：平滑渐变叠加确定性的噪声（noise 为噪声幅度，0～255）。纯色图编码后
// 太小，压不出解码与熵解码的真实工作量。
func photoImage(w, h, noise int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := uint32(2463534242)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			for c := 0; c < 3; c++ {
				s ^= s << 13
				s ^= s >> 17
				s ^= s << 5
				v := (x*255/max(w, 1)+y*255/max(h, 1)+c*85)/2 + int(s%uint32(noise+1)) - noise/2
				img.Pix[i+c] = uint8(min(max(v, 0), 255))
			}
			img.Pix[i+3] = 255
		}
	}
	return img
}

func encodeJPEG(t testing.TB, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// photoJPEG、photoPNG 是最常用的夹具：w×h 的照片样图，JPEG 质量 92（与设计 §6.1 的合成样本同档）。
func photoJPEG(t testing.TB, w, h int) []byte { return encodeJPEG(t, photoImage(w, h, 40), 92) }
func photoPNG(t testing.TB, w, h int) []byte  { return encodePNG(t, photoImage(w, h, 40)) }

// decodeSize 解出 b 的格式与尺寸，解不出时让测试失败。
func decodeSize(t testing.TB, b []byte) (format string, w, h int) {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("输出解不出来: %v", err)
	}
	return format, img.Bounds().Dx(), img.Bounds().Dy()
}

// readTestdata 读 player/testdata 下的样本（原样拷自 go1.26.1 的 GOROOT/src/image/testdata）。
func readTestdata(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// allocBytes 返回 f 运行期间整个进程累计分配的字节数。用来断言「没有进入解码」：这些用例不并行。
func allocBytes(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// ── 手工拼的 JPEG ──

// jpegSeg 拼一个带长度的段：FF m、长度（含自身 2 字节）、body。
func jpegSeg(m byte, body []byte) []byte {
	return append([]byte{0xFF, m, byte((len(body) + 2) >> 8), byte(len(body) + 2)}, body...)
}

// jpegComp 是 SOF 里的一个分量：id 与采样因子 hv（量化表一律用 0）。
type jpegComp struct{ id, hv byte }

var (
	grayComp = []jpegComp{{1, 0x11}}
	yccComps = []jpegComp{{1, 0x11}, {2, 0x11}, {3, 0x11}} // 4:4:4
)

// jpegHead 拼 SOI、量化表 0（64 项全 1）、帧头 sofMarker（0xC0 基线、0xC2 渐进式）与两张 Huffman 表：
// DC 表 0 只有一个码「0」→ 差值类别 0（零 DC）；AC 表 0 是 EOB0～EOB14 共 15 个符号、各 4 位（见 eobRuns）。
func jpegHead(sofMarker byte, w, h int, comps []jpegComp) []byte {
	var out []byte
	out = append(out, 0xFF, 0xD8)
	out = append(out, jpegSeg(0xDB, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...))...)
	sof := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(len(comps))}
	for _, c := range comps {
		sof = append(sof, c.id, c.hv, 0)
	}
	out = append(out, jpegSeg(sofMarker, sof)...)
	dc := append([]byte{0x00, 1}, make([]byte, 15)...) // 码长 1 的 1 个
	dc = append(dc, 0)
	out = append(out, jpegSeg(0xC4, dc)...)
	ac := append([]byte{0x10, 0, 0, 0, 15}, make([]byte, 12)...) // 码长 4 的 15 个
	for r := byte(0); r < 15; r++ {
		ac = append(ac, r<<4)
	}
	return append(out, jpegSeg(0xC4, ac)...)
}

// jpegSOS 拼扫描头：comps 是各分量 id（DC、AC 表都用 0），ss、se、ahal 是渐进式的谱选择与逐次逼近参数
// （基线扫描照规范写 0、63、0）。
func jpegSOS(comps []byte, ss, se, ahal byte) []byte {
	body := []byte{byte(len(comps))}
	for _, c := range comps {
		body = append(body, c, 0x00)
	}
	return jpegSeg(0xDA, append(body, ss, se, ahal))
}

// jpegBits 按熵编码段的规矩写位：高位在前，写出的 0xFF 后面填 0x00，收尾补 1。
type jpegBits struct {
	buf []byte
	acc uint64
	n   uint
}

func (w *jpegBits) put(v uint32, n uint) {
	w.acc = w.acc<<n | uint64(v)&(1<<n-1)
	w.n += n
	for w.n >= 8 {
		b := byte(w.acc >> (w.n - 8))
		w.buf = append(w.buf, b)
		if b == 0xFF {
			w.buf = append(w.buf, 0)
		}
		w.n -= 8
	}
}

func (w *jpegBits) bytes() []byte {
	if w.n > 0 {
		w.put(1<<(8-w.n)-1, 8-w.n)
	}
	return w.buf
}

// zeroDC 是 n 个块的零 DC：每块一个码「0」。
func zeroDC(n int) []byte {
	var w jpegBits
	for i := 0; i < n; i++ {
		w.put(0, 1)
	}
	return w.bytes()
}

// eobRuns 是覆盖 n 个块的 EOB 游程（渐进式 AC 扫描用）：EOBr 的码是 r 的 4 位，后跟 r 位游程余量，单个游程
// 至多 32767 块。
func eobRuns(n int) []byte {
	var w jpegBits
	for n > 0 {
		run := min(n, 32767)
		r := uint(0)
		for 1<<(r+1) <= run {
			r++
		}
		w.put(uint32(r), 4)
		w.put(uint32(run-1<<r), r)
		n -= run
	}
	return w.bytes()
}

// blocks8 是 w×h 单个满分辨率分量的 8×8 块数。
func blocks8(w, h int) int { return ((w + 7) / 8) * ((h + 7) / 8) }

// progressiveGray 拼渐进式灰度 JPEG：一个零 DC 扫描，接 acScans 个 AC 扫描（ahal 为 0 是首次扫描，0x10 是
// Ah=1、Al=0 的逐次逼近扫描），每个都用 EOB 游程覆盖全部块。SOS 总数为 1+acScans。
func progressiveGray(w, h, acScans int, ahal byte) []byte {
	n := blocks8(w, h)
	out := jpegHead(0xC2, w, h, grayComp)
	out = append(out, jpegSOS([]byte{1}, 0, 0, 0)...)
	out = append(out, zeroDC(n)...)
	ac := append(jpegSOS([]byte{1}, 1, 63, ahal), eobRuns(n)...)
	for i := 0; i < acScans; i++ {
		out = append(out, ac...)
	}
	return append(out, 0xFF, 0xD9)
}

// progressiveYCC444 拼渐进式 YCbCr 4:4:4 JPEG：一个交错的零 DC 扫描，三个分量各一个 AC 扫描，共 4 个 SOS。
func progressiveYCC444(w, h int) []byte {
	n := blocks8(w, h)
	out := jpegHead(0xC2, w, h, yccComps)
	out = append(out, jpegSOS([]byte{1, 2, 3}, 0, 0, 0)...)
	out = append(out, zeroDC(3*n)...)
	for _, c := range []byte{1, 2, 3} {
		out = append(out, jpegSOS([]byte{c}, 1, 63, 0)...)
		out = append(out, eobRuns(n)...)
	}
	return append(out, 0xFF, 0xD9)
}

// progressiveRefine444 拼渐进式 YCbCr 4:4:4 JPEG：一个交错的零 DC 扫描，接 refines 个 Y 分量上 Ah=1 的 AC
// 逐次逼近扫描（EOB 游程覆盖全部块）。SOS 总数为 1+refines。
func progressiveRefine444(w, h, refines int) []byte {
	n := blocks8(w, h)
	out := jpegHead(0xC2, w, h, yccComps)
	out = append(out, jpegSOS([]byte{1, 2, 3}, 0, 0, 0)...)
	out = append(out, zeroDC(3*n)...)
	ac := append(jpegSOS([]byte{1}, 1, 63, 0x10), eobRuns(n)...)
	for i := 0; i < refines; i++ {
		out = append(out, ac...)
	}
	return append(out, 0xFF, 0xD9)
}

// baselineScans 拼基线 JPEG（分量为 comps、采样都是 1×1），依次写 scans 个扫描（每项是该扫描的分量 id）：
// 每块是零 DC 与 EOB（AC 符号 0x00，码「0000」）。image/jpeg 接受重复的基线扫描，每个都重新反量化、做 IDCT
// 并写像素。
func baselineScans(w, h int, comps []jpegComp, scans ...[]byte) []byte {
	n := blocks8(w, h)
	out := jpegHead(0xC0, w, h, comps)
	for _, sc := range scans {
		var bits jpegBits
		for i := 0; i < len(sc)*n; i++ {
			bits.put(0, 1)
			bits.put(0, 4)
		}
		out = append(out, jpegSOS(sc, 0, 63, 0)...)
		out = append(out, bits.bytes()...)
	}
	return append(out, 0xFF, 0xD9)
}

// repeatedBaseline 是基线 YCbCr 4:4:4 JPEG，重复 scans 个三分量交错扫描。
func repeatedBaseline(w, h, scans int) []byte {
	all := make([][]byte, scans)
	for i := range all {
		all[i] = []byte{1, 2, 3}
	}
	return baselineScans(w, h, yccComps, all...)
}

// insertAt 在 b 的第 i 个字节处插入 ins。
func insertAt(b []byte, i int, ins ...byte) []byte {
	return append(append(append([]byte(nil), b[:i]...), ins...), b[i:]...)
}

// sofIndex 返回 JPEG 夹具里第一个 SOF0/SOF2 段的下标（夹具按段排列、SOF 之前没有填充字节）。
func sofIndex(t testing.TB, b []byte) int {
	t.Helper()
	for i := 2; i+3 < len(b); i += 2 + int(binary.BigEndian.Uint16(b[i+2:])) {
		if b[i+1] == 0xC0 || b[i+1] == 0xC2 {
			return i
		}
	}
	t.Fatal("找不到 SOF")
	return -1
}

// progressiveCMYKHead 是 4 分量渐进式 JPEG 的头加一个零 DC 扫描：DecodeConfig 报 CMYK；真去解码则先按
// 四个分量分配系数数组（1600² 时约 41MB）。
func progressiveCMYKHead(w, h int) []byte {
	comps := []jpegComp{{1, 0x11}, {2, 0x11}, {3, 0x11}, {4, 0x11}}
	out := jpegHead(0xC2, w, h, comps)
	out = append(out, jpegSOS([]byte{1, 2, 3, 4}, 0, 0, 0)...)
	out = append(out, zeroDC(4*blocks8(w, h))...)
	return append(out, 0xFF, 0xD9)
}

// withSOFSize 把 JPEG 第一个 SOF0/SOF2 段里的宽高改成 w×h（其余字节不动）。
func withSOFSize(t testing.TB, b []byte, w, h int) []byte {
	t.Helper()
	out := append([]byte(nil), b...)
	for i := 2; i+9 < len(out); {
		if out[i] != 0xFF {
			t.Fatal("夹具不是按段排列的 JPEG")
		}
		m, n := out[i+1], int(binary.BigEndian.Uint16(out[i+2:]))
		if m == 0xC0 || m == 0xC2 {
			binary.BigEndian.PutUint16(out[i+5:], uint16(h))
			binary.BigEndian.PutUint16(out[i+7:], uint16(w))
			return out
		}
		i += 2 + n
	}
	t.Fatal("找不到 SOF")
	return nil
}

// stripAPP14 去掉 JPEG 里的 Adobe APP14 段（4 分量 JPEG 靠它区分 CMYK 与 YCCK）。
func stripAPP14(t testing.TB, b []byte) []byte {
	t.Helper()
	for i := 2; i+4 < len(b); {
		m, n := b[i+1], int(binary.BigEndian.Uint16(b[i+2:]))
		if m == 0xEE {
			return append(append([]byte(nil), b[:i]...), b[i+2+n:]...)
		}
		if m == 0xDA {
			break
		}
		i += 2 + n
	}
	t.Fatal("找不到 APP14")
	return nil
}

// ── 手工拼的 PNG ──

func pngChunk(typ string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, typ...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

// withIHDRSize 把 PNG 的 IHDR 宽高改成 w×h 并重算 CRC（解码炸弹的构造：头说很大，数据只有一点）。
func withIHDRSize(b []byte, w, h int) []byte {
	out := append([]byte(nil), b...)
	binary.BigEndian.PutUint32(out[16:], uint32(w))
	binary.BigEndian.PutUint32(out[20:], uint32(h))
	binary.BigEndian.PutUint32(out[29:], crc32.ChecksumIEEE(out[12:29]))
	return out
}

// adam7PNG 把 img 编成 8 位 RGBA 的 Adam7 隔行 PNG（image/png 只编逐行的）：七遍各自的扫描行都用滤波 0。
func adam7PNG(t testing.TB, img *image.NRGBA) []byte {
	t.Helper()
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	passes := []struct{ x0, y0, dx, dy int }{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}}
	var raw bytes.Buffer
	for _, p := range passes {
		for y := p.y0; y < h; y += p.dy {
			if p.x0 >= w {
				break
			}
			raw.WriteByte(0)
			for x := p.x0; x < w; x += p.dx {
				raw.Write(img.Pix[img.PixOffset(x, y) : img.PixOffset(x, y)+4])
			}
		}
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, 8, 6, 0, 0, 1) // 8 位、RGBA、隔行 Adam7
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, pngChunk("IHDR", ihdr)...)
	out = append(out, pngChunk("IDAT", z.Bytes())...)
	return append(out, pngChunk("IEND", nil)...)
}

// nrgba64Photo 是 16 位、带一点透明的照片样图（png.Encode 据此写出 16 位 RGBA）。
func nrgba64Photo(w, h int) *image.NRGBA64 {
	src := photoImage(w, h, 40)
	img := image.NewNRGBA64(src.Bounds())
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.RGBAAt(x, y)
			img.SetNRGBA64(x, y, color.NRGBA64{uint16(c.R) * 257, uint16(c.G) * 257, uint16(c.B) * 257, 0xFFFF - uint16(x%2)})
		}
	}
	return img
}
