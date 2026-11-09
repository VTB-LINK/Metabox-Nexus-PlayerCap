package lyric

// 本文件只测 parsePrimaryVtables：在构造的假映像里按 MSVC x86 RTTI 找某个类的主虚表。CKSongsGraph（#139）
// 与 CAudioDecodeForPCM（#145）共用它。

import (
	"encoding/binary"
	"testing"
)

const testImageBase = 0x10000000

// rttiImage 是一份全零的假映像，装载于 testImageBase。
type rttiImage []byte

func newRTTIImage() rttiImage { return make(rttiImage, 0x1000) }

func (im rttiImage) put32(off int, v uint32) { binary.LittleEndian.PutUint32(im[off:], v) }

// name 在 off 处写 CKSongsGraph 的修饰名（含结尾的 0），返回 TypeDescriptor 的地址（名字 − 8）。
func (im rttiImage) name(off int) uint32 { return im.typeName(off, songsGraphTypeName) }

// typeName 同 name，修饰名由调用方给出。
func (im rttiImage) typeName(off int, typeName string) uint32 {
	copy(im[off:], typeName)
	return testImageBase + uint32(off) - tdNameOff
}

// col 在 off 处写一个 COL{签名, OffsetToTop, CDOffset, pTD, pCHD}，返回它的地址。
func (im rttiImage) col(off int, sig, offsetToTop, td uint32) uint32 {
	im.put32(off, sig)
	im.put32(off+4, offsetToTop)
	im.put32(off+12, td)
	im.put32(off+16, testImageBase+0x800) // pClassHierarchyDescriptor，不看
	return testImageBase + uint32(off)
}

// vtable 在 off 处写 vftable[-1] = col，返回虚表地址（off + 4）。
func (im rttiImage) vtable(off int, col uint32) uint32 {
	im.put32(off, col)
	return testImageBase + uint32(off) + 4
}

// 真机的形状：offset 0 与 4 的 COL 各一个（多重继承），各有一张虚表。只取 offset 0 那张。
//
// 变异自证：去掉 `le32(img, p-8) != 0`，次虚表也算进来，n=2 而红；把 vtable 写成 base+q（漏 +4）
// 而红；TD 写成名字地址（漏 −8）则找不到 COL 而红。
func TestParseSongsGraphRTTIPrimaryOnly(t *testing.T) {
	im := newRTTIImage()
	td := im.name(0x100)
	primary := im.vtable(0x300, im.col(0x200, 0, 0, td))
	im.vtable(0x340, im.col(0x220, 0, 4, td))
	vt, n := parsePrimaryVtables(im, testImageBase, songsGraphTypeName)
	if n != 1 || vt != primary {
		t.Fatalf("得到虚表 %#x（%d 个），want %#x（1 个）", vt, n, primary)
	}
}

// 同一份映像里两个类各有一张主虚表：按类型名各自只认自己的那张；CAudioDecodeForPCM 另有一张 offset 4 的
// 次虚表，不收。
//
// 变异自证（实测）：parsePrimaryVtables 不用传入的类型名、写死 songsGraphTypeName，按 CAudioDecodeForPCM 找
// 得到的是 CKSongsGraph 的虚表而红；去掉 `le32(img, p-8) != 0`，PCM 的次虚表也算进来、n=2 而红。
func TestParsePrimaryVtablesByTypeName(t *testing.T) {
	im := newRTTIImage()
	graph := im.vtable(0x300, im.col(0x200, 0, 0, im.typeName(0x100, songsGraphTypeName)))
	pcmTD := im.typeName(0x140, pcmTypeName)
	pcm := im.vtable(0x340, im.col(0x220, 0, 0, pcmTD))
	im.vtable(0x380, im.col(0x240, 0, 4, pcmTD))
	if vt, n := parsePrimaryVtables(im, testImageBase, songsGraphTypeName); n != 1 || vt != graph {
		t.Errorf("CKSongsGraph：得到虚表 %#x（%d 个），want %#x（1 个）", vt, n, graph)
	}
	if vt, n := parsePrimaryVtables(im, testImageBase, pcmTypeName); n != 1 || vt != pcm {
		t.Errorf("CAudioDecodeForPCM：得到虚表 %#x（%d 个），want %#x（1 个）", vt, n, pcm)
	}
	// 映像里只有 CKSongsGraph 的 RTTI 时，按 CAudioDecodeForPCM 找不到。
	only := newRTTIImage()
	only.vtable(0x300, only.col(0x200, 0, 0, only.name(0x100)))
	if vt, n := parsePrimaryVtables(only, testImageBase, pcmTypeName); n != 0 {
		t.Errorf("只有 CKSongsGraph 时按 CAudioDecodeForPCM 得到 %d 个（最后一个 %#x），want 0", n, vt)
	}
}

// 反例：解析不出恰好一个主虚表，调用方视为不支持。
//
// 变异自证：alignedRefs 改成逐字节找（p++），「TD 引用未对齐」一例找到虚表而红；去掉
// `le32(img, p-12) != 0`，「签名非 0」一例而红；去掉 `at < tdNameOff` 的判断，TD 回绕成映像基址 − 4，
// 「名字在映像开头」一例找到虚表而红；去掉 `p < 12`，「TD 引用在映像开头」一例下标越界 panic 而红；
// 查找的名字去掉结尾的 0，「名字紧贴映像末尾」一例找到虚表而红。
func TestParseSongsGraphRTTIRejects(t *testing.T) {
	cases := []struct {
		name  string
		build func(im rttiImage) rttiImage
		wantN int
	}{
		{"名字不存在", func(im rttiImage) rttiImage {
			td := uint32(testImageBase + 0xF8) // 有 COL 与虚表，只是没有名字
			im.vtable(0x300, im.col(0x200, 0, 0, td))
			return im
		}, 0},
		{"名字是别的类", func(im rttiImage) rttiImage {
			copy(im[0x100:], ".?AVCKSongsGraphX@@\x00")
			im.vtable(0x300, im.col(0x200, 0, 0, testImageBase+0xF8))
			return im
		}, 0},
		{"名字紧贴映像末尾（结尾的 0 落在映像外）", func(im rttiImage) rttiImage {
			name := songsGraphTypeName[:len(songsGraphTypeName)-1]
			off := len(im) - len(name)
			copy(im[off:], name)
			im.vtable(0x300, im.col(0x200, 0, 0, testImageBase+uint32(off)-tdNameOff))
			return im
		}, 0},
		{"名字在映像开头（TD 落在映像之前）", func(im rttiImage) rttiImage {
			copy(im[4:], songsGraphTypeName)
			im.vtable(0x300, im.col(0x200, 0, 0, testImageBase-4))
			return im
		}, 0},
		{"TD 引用在映像开头（前面放不下签名与 OffsetToTop）", func(im rttiImage) rttiImage {
			td := im.name(0x100)
			im.put32(8, td)
			im.vtable(0x300, testImageBase-4) // 指向那个放不下的「COL」
			return im
		}, 0},
		{"TD 引用未对齐", func(im rttiImage) rttiImage {
			td := im.name(0x100)
			im.vtable(0x300, im.col(0x201, 0, 0, td))
			return im
		}, 0},
		{"签名非 0（64 位的 COL 签名为 1）", func(im rttiImage) rttiImage {
			td := im.name(0x100)
			im.vtable(0x300, im.col(0x200, 1, 0, td))
			return im
		}, 0},
		{"只有 OffsetToTop ≠ 0 的 COL", func(im rttiImage) rttiImage {
			td := im.name(0x100)
			im.vtable(0x340, im.col(0x220, 0, 4, td))
			return im
		}, 0},
		{"COL 无虚表引用", func(im rttiImage) rttiImage {
			im.col(0x200, 0, 0, im.name(0x100))
			return im
		}, 0},
		{"两个主 COL 各有虚表（歧义）", func(im rttiImage) rttiImage {
			td := im.name(0x100)
			im.vtable(0x300, im.col(0x200, 0, 0, td))
			im.vtable(0x340, im.col(0x220, 0, 0, td))
			return im
		}, 2},
		{"一个主 COL 被两张虚表引用（歧义）", func(im rttiImage) rttiImage {
			col := im.col(0x200, 0, 0, im.name(0x100))
			im.vtable(0x300, col)
			im.vtable(0x340, col)
			return im
		}, 2},
		{"名字出现两次、各有主虚表（歧义）", func(im rttiImage) rttiImage {
			im.vtable(0x300, im.col(0x200, 0, 0, im.name(0x100)))
			im.vtable(0x340, im.col(0x220, 0, 0, im.name(0x180)))
			return im
		}, 2},
	}
	for _, c := range cases {
		vt, n := parsePrimaryVtables(c.build(newRTTIImage()), testImageBase, songsGraphTypeName)
		if n != c.wantN {
			t.Errorf("%s：得到 %d 个主虚表（最后一个 %#x），want %d", c.name, n, vt, c.wantN)
		}
	}
}
