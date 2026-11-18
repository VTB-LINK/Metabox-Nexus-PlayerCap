// Package kgdbtest 把明文 SQLite 按酷狗 KGMusicV3.db 的格式加密，只供测试使用，出货代码不得引用。
//
// 加密与 kgdb 的解密各自独立实现（不共用密钥派生），往返测试才能发现其中一方的笔误。
// 格式同 unlock-music 的 pc_kugou_db，见 kgdb 包的说明。
package kgdbtest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/binary"
)

const pageSize = 1024

var masterKey = [16]byte{0x1D, 0x61, 0x31, 0x45, 0xB2, 0x47, 0xBF, 0x7F, 0x3D, 0x18, 0x96, 0x72, 0x14, 0x4F, 0xE4, 0xBF}

// Encrypt 返回 plain 加密后的新切片，不改动 plain。长度须为 1024 的正整数倍，否则 panic。
//
// 第 n≥2 页整页 AES-128-CBC 加密。第 1 页取 C = Enc(P[16:1024])，
// 写成 [0:8] 置零、[8:16] = C[0:8]、[16:24] = P[16:24]（明文）、[24:1024] = C[8:]。
func Encrypt(plain []byte) []byte {
	if len(plain) == 0 || len(plain)%pageSize != 0 {
		panic("kgdbtest: plaintext length must be a positive multiple of 1024")
	}
	out := make([]byte, 0, len(plain))
	for i := 0; i < len(plain)/pageSize; i++ {
		out = append(out, EncryptPage(uint32(i+1), plain[i*pageSize:(i+1)*pageSize])...)
	}
	return out
}

// EncryptPage 返回第 n 页（1 起）明文 plain 加密后的新切片，与 Encrypt 加密整库时该页的结果相同，
// 供测试按页号逐页生成大库而不在内存里持有整库。不改动 plain。plain 长度须为 1024、n 须不小于 1，否则 panic。
func EncryptPage(n uint32, plain []byte) []byte {
	if len(plain) != pageSize || n == 0 {
		panic("kgdbtest: EncryptPage needs a 1024-byte page and a page number of at least 1")
	}
	dst := make([]byte, pageSize)
	if n > 1 {
		encryptPage(dst, plain, n)
		return dst
	}
	c := make([]byte, pageSize-16)
	encryptPage(c, plain[16:], 1)
	copy(dst[8:16], c[:8])
	copy(dst[16:24], plain[16:24])
	copy(dst[24:], c[8:])
	return dst
}

func encryptPage(dst, src []byte, n uint32) {
	keySrc := append(append(masterKey[:0:0], masterKey[:]...), 0, 0, 0, 0, 's', 'A', 'l', 'T')
	binary.LittleEndian.PutUint32(keySrc[16:20], n)
	key := md5.Sum(keySrc)

	seed := make([]byte, 0, 16)
	s := n + 1
	for range 4 {
		v := s*0x9EF4 - s/0xCE26*0x7FFFFF07
		if int32(v) < 0 {
			v += 0x7FFFFF07
		}
		s = v
		seed = binary.LittleEndian.AppendUint32(seed, s)
	}
	iv := md5.Sum(seed)

	block, _ := aes.NewCipher(key[:]) // 密钥恒为 16 字节，不会出错
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(dst, src)
}
