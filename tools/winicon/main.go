// winicon 从一张高分辨率正方形 PNG 母版生成 Windows 多尺寸图标资源（.syso）。
//
// 为什么存在：Windows 的 .ico 是多分辨率容器，应内含 16/24/32/48/64/128/256 各尺寸、
// 每张按该分辨率单独渲染好；显示时系统直接取对应尺寸那张。若 .ico 只塞一张大图（旧做法），
// 小尺寸场景（任务栏/列表 ~16–32px）就得由 Windows 在显示时用劣质采样器现缩 → 锯齿。
// 本工具在构建期用高质量重采样（CatmullRom）把母版缩成全套尺寸再打包，等价于 electron-builder
// 对图标做的事。纯 Go、跨平台：Linux CI 也能产出 windows/amd64 的 .syso，无需 Windows。
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
	"image/png"
	"os"
	"strconv"
	"strings"

	"github.com/josephspurrier/goversioninfo"
	xdraw "golang.org/x/image/draw"
)

// Windows 标准图标尺寸。全部按该分辨率从母版单独缩出、以 PNG 存入 .ico
//（Vista+ 支持 .ico 内嵌 PNG；electron-builder 生成的参考图标同样是全 PNG）。
var iconSizes = []int{16, 24, 32, 48, 64, 128, 256}

func main() {
	master := flag.String("master", "", "母版正方形 PNG 路径（越大越好，≥256）")
	out := flag.String("o", "resource_windows_amd64.syso", "输出 .syso 路径")
	version := flag.String("version", "0.0.0.0", "产品版本，如 3.0.0-rc.9（预发布后缀会保留在字符串版本里）")
	icoOut := flag.String("ico", "", "可选：额外落盘中间多尺寸 .ico")
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

	imgs := make([]image.Image, 0, len(iconSizes))
	for _, s := range iconSizes {
		imgs = append(imgs, resize(src, s))
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
	fmt.Printf("winicon: %s ← %s（%d 档：%v，version=%s）\n", *out, *master, len(iconSizes), iconSizes, *version)
}

func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

// resize 用 CatmullRom（双三次族，高质量）把源缩/放到 size×size。
func resize(src image.Image, size int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
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
		buf.WriteByte(wh)                       // width
		buf.WriteByte(wh)                       // height（正方形）
		buf.WriteByte(0)                        // color count
		buf.WriteByte(0)                        // reserved
		binary.Write(&buf, le, uint16(1))       // color planes
		binary.Write(&buf, le, uint16(32))      // bits per pixel
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
			ProductName:     "Metabox Nexus PlayerCap",
			FileDescription: "Metabox Nexus PlayerCap",
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
