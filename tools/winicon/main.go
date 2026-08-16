// winicon 从一张高分辨率正方形 PNG 母版生成 Windows 多尺寸图标资源（.syso）。
//
// 为什么存在：Windows 的 .ico 是多分辨率容器，应内含覆盖各 DPI 的多档尺寸、
// 每张按该分辨率单独渲染好；显示时系统直接取对应尺寸那张。若 .ico 只塞一张大图（旧做法），
// 小尺寸场景（任务栏/列表 ~16–32px）就得由 Windows 在显示时用劣质采样器现缩 → 锯齿。
// 本工具在构建期用高质量重采样（Lanczos3）把母版缩成全套尺寸、再补一道 unsharp 锐化后打包，
// 等价于 electron-builder/sharp 对图标做的事——小尺寸才有那种「脆」。纯 Go、跨平台：Linux CI
// 也能产出 windows/amd64 的 .syso，无需 Windows。
//
// 用法：
//
//	go run ./tools/winicon -master build-assets/winicon/masters/metabox5-sqr.png \
//	    -version 3.0.0-rc.9 -o resource_windows_amd64.syso
//
// -ico 可选：额外把中间多尺寸 .ico 落盘，便于人工检视。
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/josephspurrier/goversioninfo"
	xdraw "golang.org/x/image/draw"
)

// Windows 图标尺寸，覆盖 100%–250% DPI 下的小图标（标题栏/列表）与大图标（任务栏/alt-tab）：
// 小图标 SM_CXSMICON = 16·DPI → 16(100%)/20(125%)/24(150%)/32(200%)/40(250%)；
// 大图标 SM_CXICON  = 32·DPI → 32/40/48/64/96(≈250% 的 80)。缺档会让 Windows 现缩别的尺寸 →
// 高分屏标题栏发糊（250% 要 40，早期档位没有 40 就是这个坑）。全部按该分辨率单独缩出、以 PNG 存入。
var iconSizes = []int{16, 20, 24, 32, 40, 48, 64, 96, 128, 256}

func main() {
	master := flag.String("master", "", "母版正方形 PNG 路径（越大越好，≥256）")
	out := flag.String("o", "resource_windows_amd64.syso", "输出 .syso 路径")
	version := flag.String("version", "0.0.0.0", "产品版本，如 3.0.0-rc.9（预发布后缀会保留在字符串版本里）")
	icoOut := flag.String("ico", "", "可选：额外落盘中间多尺寸 .ico")
	filter := flag.String("filter", "lanczos3", "降采样核：lanczos3（锐，默认）/ catmullrom")
	sharpen := flag.Float64("sharpen", 0.6, "降采样后 unsharp 锐化量（0 关；小图标建议 0.4–0.8）")
	flag.Parse()

	if *master == "" {
		fatalf("需要 -master 指定母版 PNG")
	}

	src, err := loadPNG(*master)
	if err != nil {
		fatalf("读母版失败: %v", err)
	}
	b := src.Bounds()
	if b.Dx() != b.Dy() {
		fatalf("母版必须是正方形，当前 %dx%d", b.Dx(), b.Dy())
	}
	if b.Dx() < iconSizes[len(iconSizes)-1] {
		fmt.Fprintf(os.Stderr, "警告：母版 %dpx 小于最大目标 %dpx，256 档会被放大\n", b.Dx(), iconSizes[len(iconSizes)-1])
	}

	scaler := pickScaler(*filter)
	srcRGBA := toRGBA(src) // 转预乘 alpha，边缘干净
	imgs := make([]image.Image, 0, len(iconSizes))
	for _, s := range iconSizes {
		imgs = append(imgs, unsharpMask(resize(srcRGBA, s, scaler), *sharpen))
	}
	icoBytes := buildICO(imgs)

	// goversioninfo 需要 .ico 的文件路径；若未指定 -ico 就落到临时文件。
	icoPath := *icoOut
	if icoPath == "" {
		tf, err := os.CreateTemp("", "winicon-*.ico")
		if err != nil {
			fatalf("建临时 .ico 失败: %v", err)
		}
		icoPath = tf.Name()
		tf.Close()
		defer os.Remove(icoPath)
	}
	if err := os.WriteFile(icoPath, icoBytes, 0o644); err != nil {
		fatalf("写 .ico 失败: %v", err)
	}

	if err := writeSyso(icoPath, *out, *version); err != nil {
		fatalf("写 .syso 失败: %v", err)
	}
	fmt.Printf("winicon: %s ← %s（%d 档：%v，filter=%s，sharpen=%.2g，version=%s）\n", *out, *master, len(iconSizes), iconSizes, *filter, *sharpen, *version)
}

func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

// lanczos3 核：sinc(t)·sinc(t/3)，|t|<3。比 CatmullRom 保留更多高频 → 小尺寸边缘更锐，
// 与 electron/sharp 默认的 lanczos3 一致。
func lanczos3() *xdraw.Kernel {
	return &xdraw.Kernel{Support: 3, At: func(t float64) float64 {
		if t < 0 {
			t = -t
		}
		if t >= 3 {
			return 0
		}
		if t < 1e-8 {
			return 1
		}
		x := math.Pi * t
		return 3 * math.Sin(x) * math.Sin(x/3) / (x * x)
	}}
}

func pickScaler(name string) xdraw.Interpolator {
	switch name {
	case "catmullrom":
		return xdraw.CatmullRom
	case "lanczos3", "":
		return lanczos3()
	default:
		fatalf("未知 -filter %q（可选 lanczos3 / catmullrom）", name)
		return nil
	}
}

// toRGBA 把源转成预乘 alpha 的 *image.RGBA。透明底 logo 必须在预乘空间做降采样平均，
// 否则近透明像素的 RGB（常为 0）会污染边缘、发糊发暗。
func toRGBA(src image.Image) *image.RGBA {
	if r, ok := src.(*image.RGBA); ok {
		return r
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src) // Go 的 image/draw 落入预乘 RGBA
	return dst
}

// resize 用指定核把预乘源缩到 size×size。
func resize(src *image.RGBA, size int, scaler xdraw.Interpolator) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	scaler.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	return dst
}

// unsharpMask：dst = src + amount·(src − blur(src))，在预乘 RGBA 上按通道做，结果 clamp。
// 高分母版极度降采样后边缘偏软；小图标补一道锐化才有 electron/sharp 那种「脆」。amount<=0 直接跳过。
func unsharpMask(img *image.RGBA, amount float64) *image.RGBA {
	if amount <= 0 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// 3×3 高斯（1 2 1 / 2 4 2 / 1 2 1，和 16）作为低频。
	blur := make([]float64, len(img.Pix))
	at := func(x, y, c int) float64 {
		if x < 0 {
			x = 0
		} else if x >= w {
			x = w - 1
		}
		if y < 0 {
			y = 0
		} else if y >= h {
			y = h - 1
		}
		return float64(img.Pix[(y*w+x)*4+c])
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < 4; c++ {
				s := at(x-1, y-1, c) + 2*at(x, y-1, c) + at(x+1, y-1, c) +
					2*at(x-1, y, c) + 4*at(x, y, c) + 2*at(x+1, y, c) +
					at(x-1, y+1, c) + 2*at(x, y+1, c) + at(x+1, y+1, c)
				blur[(y*w+x)*4+c] = s / 16
			}
		}
	}
	out := image.NewRGBA(b)
	sharp := func(i int) float64 {
		v := float64(img.Pix[i]) + amount*(float64(img.Pix[i])-blur[i])
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return v
	}
	for p := 0; p < len(img.Pix); p += 4 {
		r, g, bch, a := sharp(p), sharp(p+1), sharp(p+2), sharp(p+3)
		// 保持合法预乘：RGB 不得超过 A，否则 png 解预乘会溢出成亮边光晕。
		if r > a {
			r = a
		}
		if g > a {
			g = a
		}
		if bch > a {
			bch = a
		}
		out.Pix[p] = uint8(r + 0.5)
		out.Pix[p+1] = uint8(g + 0.5)
		out.Pix[p+2] = uint8(bch + 0.5)
		out.Pix[p+3] = uint8(a + 0.5)
	}
	return out
}

// buildICO 把若干正方形图像打包成多尺寸 .ico（每张以 PNG 存储）。
// 格式：ICONDIR(6B) + N×ICONDIRENTRY(16B) + 各 PNG 数据。
func buildICO(imgs []image.Image) []byte {
	pngs := make([][]byte, len(imgs))
	for i, im := range imgs {
		var pb bytes.Buffer
		if err := png.Encode(&pb, im); err != nil {
			fatalf("编码 PNG 失败: %v", err)
		}
		pngs[i] = pb.Bytes()
	}

	var buf bytes.Buffer
	le := binary.LittleEndian
	// ICONDIR
	binary.Write(&buf, le, uint16(0)) // reserved
	binary.Write(&buf, le, uint16(1)) // type = icon
	binary.Write(&buf, le, uint16(len(imgs)))

	offset := 6 + 16*len(imgs) // 目录之后即图像数据
	for i, im := range imgs {
		d := im.Bounds().Dx()
		// 目录项宽/高为单字节，256 及以上写 0（.ico 目录上限即此）。
		wh := byte(d)
		if d >= 256 {
			wh = 0
		}
		buf.WriteByte(wh)                            // width
		buf.WriteByte(wh)                            // height（正方形）
		buf.WriteByte(0)                             // color count
		buf.WriteByte(0)                             // reserved
		binary.Write(&buf, le, uint16(1))            // color planes
		binary.Write(&buf, le, uint16(32))           // bits per pixel
		binary.Write(&buf, le, uint32(len(pngs[i]))) // bytes in resource
		binary.Write(&buf, le, uint32(offset))       // image offset
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return buf.Bytes()
}

// writeSyso 用 goversioninfo 把多尺寸 .ico 连同基本版本信息编成 windows/amd64 的 .syso。
func writeSyso(icoPath, outPath, version string) error {
	maj, min, pat, bld := parseVersion(version)
	vi := &goversioninfo.VersionInfo{
		IconPath: icoPath,
		FixedFileInfo: goversioninfo.FixedFileInfo{
			FileVersion:    goversioninfo.FileVersion{Major: maj, Minor: min, Patch: pat, Build: bld},
			ProductVersion: goversioninfo.FileVersion{Major: maj, Minor: min, Patch: pat, Build: bld},
			FileFlagsMask:  "3f",
			FileFlags:      "00",
			FileOS:         "040004", // VOS_NT_WINDOWS32
			FileType:       "01",     // VFT_APP
			FileSubType:    "00",
		},
		StringFileInfo: goversioninfo.StringFileInfo{
			ProductName:     "VTB-TOOLS Metabox Nexus PlayerCap",
			FileDescription: "VTB-TOOLS Metabox Nexus PlayerCap",
			ProductVersion:  version,
			FileVersion:     version,
		},
		VarFileInfo: goversioninfo.VarFileInfo{
			Translation: goversioninfo.Translation{LangID: goversioninfo.LngUSEnglish, CharsetID: goversioninfo.CsUnicode},
		},
	}
	vi.Build()
	vi.Walk()
	return vi.WriteSyso(outPath, "amd64")
}

// parseVersion 取版本字符串的前导 X.Y.Z[.B] 数字段（丢弃 -rc.N 等预发布后缀）用于定长版本信息。
func parseVersion(v string) (maj, min, pat, bld int) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	get := func(i int) int {
		if i < len(parts) {
			n, _ := strconv.Atoi(strings.TrimSpace(parts[i]))
			return n
		}
		return 0
	}
	return get(0), get(1), get(2), get(3)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "winicon: "+format+"\n", a...)
	os.Exit(1)
}
