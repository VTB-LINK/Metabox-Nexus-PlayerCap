package player

// 本文件只测 JPEG 解码前的扫描次数预检（jpegScanBudget 与它找帧头用的 jpegSOF，设计 §5.3、§9 第 18、19 条）：
// image/jpeg 对扫描次数没有上限，渐进式按 SOS 个数、顺序式按各扫描的分量数之和限住，超了原样退回、不解码。
//
// 变异自证（均实测，逐条记在各用例注释里）。

import (
	"bytes"
	"testing"
	"time"
)

var (
	sofProgGray = progressiveGray(64, 48, 0, 0) // 只有一个 DC 扫描的渐进式灰度小图，用来摆帧头之前的字节
	baseYCC     = baselineScans(64, 48, yccComps, []byte{1, 2, 3})
)

// 渐进式 JPEG 炸弹：1600² 灰度，一个零 DC 扫描后重复 20 万次只带几字节熵数据的 AC 扫描（约 3MB，在字节与尺寸
// 上限之内）：预检拦下、原样退回，不进入解码（设计 §9 第 18 条 (a)）。
//
// 变异自证：删掉 normalizeCover 里对 jpegScanBudget 的调用，超过时限而红。本机不加预检时这张图要 4 分 16 秒
// 才处理完（解码成功并缩放），加了预检约 5ms 拦下。
func TestCoverProgressiveScanBomb(t *testing.T) {
	bomb := progressiveGray(1600, 1600, 200000, 0)
	if len(bomb) > 5<<20 {
		t.Fatalf("前置：炸弹应在 5MB 字节上限之内，got %d", len(bomb))
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		out, _, _, _, err := normalizeCover(bomb, "image/jpeg")
		if err == nil || !bytes.Equal(out, bomb) {
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("应原样退回并报错")
		}
		t.Logf("预检拦下用时 %v：%v", time.Since(start), err)
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内没有返回：进入了解码")
	}
}

// 扫描个数的边界（设计 §9 第 19 条）。渐进式：真实样本（libjpeg 的 10 个扫描）缩放；恰好 maxJPEGScans 个 SOS
// 缩放、多一个原样（含 1600² 的逐次逼近压力夹具）。顺序式：一个交错扫描、三个分量各一个扫描缩放；有分量被
// 重复扫描的原样。取值改了这里不必改。
//
// 变异自证：`scans > maxJPEGScans` 改成 `>=`，两个「恰好上限」红；改成 `> maxJPEGScans+1`，两个「多一个」红；
// 顺序式的 `scanComps > frameComps` 改成 `> frameComps+1`，「三个分量后又扫一次 Y」「灰度两个扫描」红；去掉
// 顺序式那条判定，四个应原样退回的顺序式全红；顺序式的判定对渐进式也生效（去掉 `!progressive &&`），真实样本
// 与两个「恰好上限」红；帧类型判反（`== 0xC2` 改成 `== 0xC0`），渐进式三个应缩放的与四个应原样退回的
// 顺序式全红。
func TestCoverScanLimits(t *testing.T) {
	cases := []struct {
		name    string
		src     []byte
		resized bool
	}{
		{"渐进式真实样本（10 个扫描）", readTestdata(t, "video-001.progressive.jpeg"), true},
		{"渐进式恰好上限", progressiveGray(64, 48, maxJPEGScans-1, 0), true},
		{"渐进式多一个", progressiveGray(64, 48, maxJPEGScans, 0), false},
		{"渐进式 1600² 逐次逼近恰好上限", progressiveGray(1600, 1600, maxJPEGScans-1, 0x10), true},
		{"渐进式 1600² 逐次逼近多一个", progressiveGray(1600, 1600, maxJPEGScans, 0x10), false},
		{"顺序式一个交错扫描", baseYCC, true},
		{"顺序式三个分量各一个扫描", baselineScans(64, 48, yccComps, []byte{1}, []byte{2}, []byte{3}), true},
		{"顺序式灰度一个扫描", baselineScans(64, 48, grayComp, []byte{1}), true},
		// 分量数之和不超过帧的分量数即放行：重复扫描 Y 三次的工作量不超过一个 4:4:4 交错扫描（规则的已知宽松处）。
		{"顺序式同一分量扫三次", baselineScans(64, 48, yccComps, []byte{1}, []byte{1}, []byte{1}), true},
		{"顺序式交错扫描重复一次", repeatedBaseline(64, 48, 2), false},
		{"顺序式 1600² 交错扫描重复一次", repeatedBaseline(1600, 1600, 2), false},
		{"顺序式三个分量后又扫一次 Y", baselineScans(64, 48, yccComps, []byte{1}, []byte{2}, []byte{3}, []byte{1}), false},
		{"顺序式灰度两个扫描", baselineScans(64, 48, grayComp, []byte{1}, []byte{1}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.resized {
				assertResized(t, c.src, "image/jpeg")
			} else {
				assertAsIs(t, c.src, "image/jpeg")
			}
		})
	}
}

// 帧头之前的字节不影响判定（jpegSOF 照解码器的走法按长度跳段）：
//   - EXIF 缩略图（APP1 里一整张带 SOF0 与 SOS 的基线 JPEG）不算扫描数、不被当成帧头：渐进式恰好上限、
//     顺序式一个交错扫描都照样缩放。
//   - COM 段里的假 FF C2 不能让顺序式的帧被当成渐进式：重复扫描的顺序式照样原样退回。
//   - 段之间的多余字节、帧头前的填充 FF（解码器都容忍）：照样找到帧头。
//
// 变异自证：从整个文件而不是 SOF 之后数 FF DA，两个 EXIF 例红；jpegSOF 改成在字节里找第一个 FF C0/C1/C2，
// 两个 EXIF 例与 COM 例红（缩略图的 SOF0、COM 里的 C2 被当成帧头）；去掉 jpegSOF 跳过填充 FF 的循环，
// 「填充 FF」例红；遇到不是 FF 的字节就返回 -1，「段间多余字节」例红。
func TestCoverScanBudgetFindsFrameHeader(t *testing.T) {
	thumb := photoJPEG(t, 16, 16)
	exif := jpegSeg(0xE1, append([]byte("Exif\x00\x00"), thumb...))
	fakeSOF2 := jpegSeg(0xFE, []byte{0xFF, 0xC2, 0x00, 0x0B, 0x08, 0x00, 0x30, 0x00, 0x40, 0x03, 0x01})
	prog := progressiveGray(64, 48, maxJPEGScans-1, 0)
	cases := []struct {
		name    string
		src     []byte
		resized bool
	}{
		{"渐进式恰好上限 + EXIF 缩略图", insertAt(prog, 2, exif...), true},
		{"顺序式 + EXIF 缩略图", insertAt(baseYCC, 2, exif...), true},
		{"顺序式重复扫描 + COM 里的假 SOF2", insertAt(repeatedBaseline(64, 48, 2), 2, fakeSOF2...), false},
		{"段间多余字节", insertAt(prog, 2, 0x00, 0x11, 0x22), true},
		{"帧头前的填充 FF", insertAt(prog, sofIndex(t, prog), 0xFF, 0xFF), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.resized {
				assertResized(t, c.src, "image/jpeg")
			} else {
				assertAsIs(t, c.src, "image/jpeg")
			}
		})
	}
}

// jpegSOF 返回的是帧头那个 FF 的下标；找不到帧头（只有 SOI、帧头前就遇到 EOI、长度越界）返回 -1。
//
// 变异自证：填充 FF 时不更新 p（返回最前面那个 FF），「填充 FF」例红；去掉 `m == 0xD9` 的判定，「EOI 在前」例
// 越过 EOI 找到后面的 SOF 而红。
func TestJPEGSOF(t *testing.T) {
	sof := sofIndex(t, sofProgGray)
	cases := []struct {
		name string
		src  []byte
		want int
	}{
		{"按段排列", sofProgGray, sof},
		{"段间多余字节", insertAt(sofProgGray, 2, 0x00, 0x11, 0x22), sof + 3},
		{"帧头前的填充 FF", insertAt(sofProgGray, sof, 0xFF, 0xFF), sof + 2},
		{"FF00 当作多余字节", insertAt(sofProgGray, 2, 0xFF, 0x00), sof + 2},
		{"RST 当作多余标记", insertAt(sofProgGray, 2, 0xFF, 0xD3), sof + 2},
		{"只有 SOI", []byte{0xFF, 0xD8}, -1},
		{"EOI 在前", insertAt(sofProgGray, 2, 0xFF, 0xD9, 0x00, 0x02), -1},
		{"长度越界", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x7F, 0xFF, 0x00}, -1},
		{"截在标记上", []byte{0xFF, 0xD8, 0xFF, 0xFF, 0xFF}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jpegSOF(c.src); got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
}
