package player

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // 只注册 jpeg 与 png：不注册 gif、webp、tiff、bmp，也缩小了解码器的攻击面

	xdraw "golang.org/x/image/draw"

	"Metabox-Nexus-PlayerCap/i18n"
)

const (
	coverSide    = 800  // cover_base64 的统一边长（issue #101）
	coverMaxSide = 1600 // 单边超过它的不解码、原样发出：限住解码与缩放的内存与耗时（设计 §6.2）
	coverQuality = 90

	// maxJPEGScans 渐进式 JPEG 允许的 SOS 个数上限（见 jpegScanBudget）。libjpeg 的默认渐进脚本是彩色 10 个、
	// 灰度 6 个扫描，留 2 个余量。本机（i9-11900KF，go1.26.1）实测：1600² 灰度、1 个零 DC 扫描加 n-1 个不耗熵位的
	// AC 逐次逼近扫描（设计 §9 第 18 条 (b)），全程 n=12 约 90ms、n=16 约 103ms，每多一个扫描约 +3ms；
	// 同样的扫描加在 1600² YCbCr 4:4:4 上，n=4 约 100ms、n=12 约 122ms。
	maxJPEGScans = 12
)

// normalizeCover 是 CoverDataURI 的纯函数部分（不打日志）：已是 800×800 的 JPEG / PNG 原字节放行、MIME 按
// 格式改正；其余 JPEG / PNG 居中裁成正方形（非正方形裁去长边两端）再缩放到 800×800 编成 JPEG。处理不了的
// 返回原字节、原 mime 与说明原因的错误。
// w、h 是源图尺寸（读不出头时为 0）。
func normalizeCover(b []byte, mime string) (out []byte, outMIME string, w, h int, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return b, mime, 0, 0, i18n.Errorf("cannot read the image header: %v", err)
	}
	w, h = cfg.Width, cfg.Height
	switch format {
	case "jpeg":
		outMIME = "image/jpeg"
	case "png":
		outMIME = "image/png"
	default: // 本包只注册了 jpeg 与 png；这一支防将来有依赖注册了别的格式
		return b, mime, w, h, i18n.Errorf("unsupported image format %q", format)
	}
	if w == coverSide && h == coverSide {
		return b, outMIME, w, h, nil // 放行不解码：只改标签不改字节
	}
	if w < 1 || h < 1 || w > coverMaxSide || h > coverMaxSide {
		return b, mime, w, h, i18n.Errorf("image size %dx%d is outside 1-%d", w, h, coverMaxSide)
	}

	if format == "jpeg" {
		// 4 分量 JPEG 被 Go 按固定公式转色、不做色彩管理，重编码后偏色；原样交给浏览器。在解码前拦下，
		// 也省掉渐进式 CMYK 解码时的大块内存。
		if cfg.ColorModel == color.CMYKModel {
			return b, mime, w, h, i18n.Errorf("CMYK JPEG is not re-encoded")
		}
		if err := jpegScanBudget(b); err != nil {
			return b, mime, w, h, err
		}
	}
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return b, mime, w, h, i18n.Errorf("image decode failed: %v", err)
	}
	if _, ok := src.(*image.CMYK); ok { // 上面已按头部拦下，这里兜底
		return b, mime, w, h, i18n.Errorf("CMYK JPEG is not re-encoded")
	}

	// 源矩形取居中的正方形：非正方形裁去长边两端、等比缩放，不变形（issue #113）。一次重采样完成，不先缩放再裁。
	// 透明处按黑底：jpeg.Encode 读 RGBA 预乘后的像素、忽略 alpha。
	dst := image.NewRGBA(image.Rect(0, 0, coverSide, coverSide))
	xdraw.BiLinear.Scale(dst, dst.Bounds(), src, centerSquare(src.Bounds()), xdraw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: coverQuality}); err != nil {
		return b, mime, w, h, i18n.Errorf("JPEG encode failed: %v", err)
	}
	return buf.Bytes(), "image/jpeg", w, h, nil
}

// centerSquare 返回 r 内居中的正方形：边长取宽高中较小者，长边两端各裁去一半。宽高之差为奇数时多出的 1 像素
// 留在右侧（下侧）：偏移向下取整，所以裁切区最多比正中偏左（上）半个像素。
func centerSquare(r image.Rectangle) image.Rectangle {
	side := min(r.Dx(), r.Dy())
	x0 := r.Min.X + (r.Dx()-side)/2
	y0 := r.Min.Y + (r.Dy()-side)/2
	return image.Rect(x0, y0, x0+side, y0+side)
}

// jpegScanBudget 在解码前限住 image/jpeg 的扫描次数：它对每个 SOS 都照做不误、没有上限，尺寸与字节上限
// 拦不住重复上万次扫描的文件（设计 §5.3）。两种帧的每扫描成本差一个量级，分开限：
//   - 渐进式（SOF2）：扫描只累积系数，IDCT 在最后做一次；SOS 个数不超过 maxJPEGScans。
//   - 顺序式（SOF0/SOF1）：每个扫描都重做反量化、IDCT 与写像素（1600² 4:4:4 每多一遍约 +45ms，实测）。规范里
//     每个分量恰在一个扫描里出现，所以各 SOS 的分量数之和不超过帧的分量数；超了即有分量被重复扫描。
//
// 只数 SOF 之后的 FF DA：解码器处理的每个 SOS 都以 FF DA 出现在那里，按字节数只会多数、不会漏数；SOF 之前
// 的 APP 段（EXIF 缩略图自带 SOS）不算。
func jpegScanBudget(b []byte) error {
	sof := jpegSOF(b)
	if sof < 0 || sof+10 > len(b) {
		return i18n.Errorf("cannot locate the JPEG frame header")
	}
	progressive, frameComps := b[sof+1] == 0xC2, int(b[sof+9])
	scans, scanComps := 0, 0
	for rest := b[sof:]; ; {
		i := bytes.Index(rest, []byte{0xFF, 0xDA})
		if i < 0 {
			break
		}
		ns := 4 // 读不到分量数（文件在这里截断）就按上限算
		if i+4 < len(rest) {
			ns = int(rest[i+4])
		}
		scans, scanComps = scans+1, scanComps+ns
		rest = rest[i+2:]
	}
	if progressive && scans > maxJPEGScans {
		return i18n.Errorf("progressive JPEG has %d scans (limit %d)", scans, maxJPEGScans)
	}
	if !progressive && scanComps > frameComps {
		return i18n.Errorf("sequential JPEG scans cover %d components but the frame has %d", scanComps, frameComps)
	}
	return nil
}

// jpegSOF 照 image/jpeg 解码主循环的走法（段间多余字节、填充的 FF、FF00 与 RST 照样跳过，其余按长度跳段）
// 找到帧头 SOF0/SOF1/SOF2，返回其 FF 的下标；找不到返回 -1。只在 DecodeConfig 已成功之后调用：解码器走到 SOF
// 这一段时没有出错，按同样的走法到达的是同一个 SOF。
func jpegSOF(b []byte) int {
	for i := 2; i+3 < len(b); {
		if b[i] != 0xFF {
			i++
			continue
		}
		p := i
		m := b[i+1]
		i += 2
		if m == 0 {
			continue
		}
		for m == 0xFF && i < len(b) {
			p, m = i-1, b[i]
			i++
		}
		if 0xD0 <= m && m <= 0xD7 {
			continue
		}
		if m == 0xD9 || i+2 > len(b) {
			return -1
		}
		if m == 0xC0 || m == 0xC1 || m == 0xC2 {
			return p
		}
		i += int(b[i])<<8 | int(b[i+1])
	}
	return -1
}
