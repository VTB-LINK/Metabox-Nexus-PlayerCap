// Package assoc 解密并解析 QQ 音乐记在本机的歌词关联（issue #218，设计见
// doc/design/qqmusic-lyric-association-design.md）。只用标准库，不碰 Windows API；读文件由调用方负责。
package assoc

import (
	"encoding/binary"

	"Metabox-Nexus-PlayerCap/i18n"
)

// Key 是 QQ 音乐加密本地 .dat 文件（关联表、歌词索引、配置）所用的 TEA 密钥（设计 §2.1）。
// 由 QQMusicCommon.dll .data 段的 7 个 dword 算出，是静态值，与机器、账号无关；写成常量，不从 DLL 读。
var Key = [16]byte{0x4a, 0x25, 0x65, 0x66, 0x68, 0x6a, 0x6f, 0x29, 0x6c, 0x2e, 0x2c, 0x6d, 0x6e, 0x56, 0x64, 0x34}

// ErrStructure 标记 .dat 文件的结构错误：长度、填充、末尾校验、UTF-16 编码或 XML 结构不合规，
// 以及配置里没有缓存目录设置或其不是绝对路径。QQ 写到一半时读到的文件同样落在这里，
// 是否判为不支持由调用方按设计 §6.3 第 4 步决定。
var ErrStructure = i18n.Sentinel("QQ Music data file is malformed")

// DecryptDat 按 oi_symmetry_decrypt2 解密整份 .dat 文件，返回去掉填充、盐与末尾 7 字节 0 之后的正文。
//
// 16 轮 TEA、密钥与数据均为大端。第 i 块密文先与上一块的 TEA 解密输出异或、再解密，
// 结果与上一块密文异或得明文（首块两者都取 0）。明文首字节的低 3 位为填充长度 p，
// 正文从第 1+p+2 字节起（跳过填充与 2 字节盐），末尾 7 字节须全为 0。
func DecryptDat(raw []byte) ([]byte, error) {
	if len(raw) < 16 || len(raw)%8 != 0 {
		return nil, i18n.Errorf("%w: length %d is below 16 or not a multiple of 8", ErrStructure, len(raw))
	}
	var k [4]uint32
	for i := range k {
		k[i] = binary.BigEndian.Uint32(Key[4*i:])
	}
	plain := make([]byte, len(raw))
	var prevOut, prevCipher [8]byte
	for off := 0; off < len(raw); off += 8 {
		var x [8]byte
		for j := range x {
			x[j] = raw[off+j] ^ prevOut[j]
		}
		teaDecryptBlock(prevOut[:], x[:], &k)
		for j := range prevOut {
			plain[off+j] = prevOut[j] ^ prevCipher[j]
		}
		copy(prevCipher[:], raw[off:off+8])
	}
	end := len(plain) - 7
	for _, b := range plain[end:] {
		if b != 0 {
			return nil, i18n.Errorf("%w: the last 7 bytes are not zero", ErrStructure)
		}
	}
	pad := int(plain[0] & 7)
	start := 1 + pad + 2
	if start > end {
		return nil, i18n.Errorf("%w: padding of %d bytes exceeds the %d-byte plaintext", ErrStructure, pad, len(plain))
	}
	return plain[start:end], nil
}

// teaDecryptBlock 是 16 轮、大端的 TEA 解密；sum 自 0x9E3779B9×16 截断到 32 位（0xE3779B90）起递减。
func teaDecryptBlock(dst, src []byte, k *[4]uint32) {
	const delta = 0x9E3779B9
	y, z := binary.BigEndian.Uint32(src), binary.BigEndian.Uint32(src[4:])
	sum := uint32(0xE3779B90)
	for range 16 {
		z -= ((y << 4) + k[2]) ^ (y + sum) ^ ((y >> 5) + k[3])
		y -= ((z << 4) + k[0]) ^ (z + sum) ^ ((z >> 5) + k[1])
		sum -= delta
	}
	binary.BigEndian.PutUint32(dst, y)
	binary.BigEndian.PutUint32(dst[4:], z)
}
