package assoc

// 本文件只测 DecryptDat：与测试内实现的 oi_symmetry_encrypt2 往返，以及长度、填充、末尾校验、任一位损坏都报 ErrStructure。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"
)

// teaEncryptBlock 是 16 轮、大端的 TEA 加密，密钥为 Key。
func teaEncryptBlock(dst, src []byte) {
	var k [4]uint32
	for i := range k {
		k[i] = binary.BigEndian.Uint32(Key[4*i:])
	}
	y, z := binary.BigEndian.Uint32(src), binary.BigEndian.Uint32(src[4:])
	var sum uint32
	for range 16 {
		sum += 0x9E3779B9
		y += ((z << 4) + k[0]) ^ (z + sum) ^ ((z >> 5) + k[1])
		z += ((y << 4) + k[2]) ^ (y + sum) ^ ((y >> 5) + k[3])
	}
	binary.BigEndian.PutUint32(dst, y)
	binary.BigEndian.PutUint32(dst[4:], z)
}

// chainEncrypt 对已排好版的明文（长度为 8 的倍数）做 oi_symmetry_encrypt2 的链式加密：
// 明文块先与上一块密文异或再 TEA 加密，结果再与上一块的 TEA 输入异或得密文。
func chainEncrypt(plain []byte) []byte {
	out := make([]byte, len(plain))
	var prevCipher, prevIn [8]byte
	for off := 0; off < len(plain); off += 8 {
		var in, enc [8]byte
		for j := range in {
			in[j] = plain[off+j] ^ prevCipher[j]
		}
		teaEncryptBlock(enc[:], in[:])
		for j := range enc {
			out[off+j] = enc[j] ^ prevIn[j]
		}
		copy(prevCipher[:], out[off:off+8])
		prevIn = in
	}
	return out
}

// layoutPlain 按 oi_symmetry_encrypt2 排版：首字节高 5 位随机、低 3 位为填充长度，
// 随后是填充、2 字节盐、正文与 7 字节 0；填充长度使总长为 8 的倍数。
func layoutPlain(r *rand.Rand, payload []byte) []byte {
	pad := (8 - (len(payload)+10)%8) % 8
	b := []byte{byte(r.IntN(256))&0xF8 | byte(pad)}
	for range pad + 2 {
		b = append(b, byte(r.IntN(256)))
	}
	b = append(b, payload...)
	return append(b, make([]byte, 7)...)
}

func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	return b
}

func TestDecryptDatRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	seenPad := map[int]bool{}
	for n := 0; n <= 40; n++ {
		payload := randomBytes(r, n)
		plain := layoutPlain(r, payload)
		seenPad[int(plain[0]&7)] = true
		got, err := DecryptDat(chainEncrypt(plain))
		if err != nil {
			t.Fatalf("正文 %d 字节：%v", n, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("正文 %d 字节：解出 % x，应为 % x", n, got, payload)
		}
	}
	if len(seenPad) != 8 {
		t.Fatalf("只覆盖了填充长度 %v，应为 0～7 全部", seenPad)
	}

	payload := randomBytes(r, 64<<10)
	got, err := DecryptDat(chainEncrypt(layoutPlain(r, payload)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("64 KiB 正文往返失败：err=%v", err)
	}
}

func TestDecryptDatRejectsBadLength(t *testing.T) {
	for _, n := range []int{0, 1, 7, 8, 15, 17, 23, 31} {
		if _, err := DecryptDat(make([]byte, n)); !errors.Is(err, ErrStructure) {
			t.Errorf("长度 %d：err=%v，应为 ErrStructure", n, err)
		}
	}
	if _, err := DecryptDat(nil); !errors.Is(err, ErrStructure) {
		t.Errorf("nil：err=%v，应为 ErrStructure", err)
	}
}

func TestDecryptDatRejectsNonZeroTail(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for pos := range 7 {
		plain := layoutPlain(r, []byte("<AssociateInfos/>"))
		plain[len(plain)-7+pos] = 1
		if _, err := DecryptDat(chainEncrypt(plain)); !errors.Is(err, ErrStructure) {
			t.Errorf("末 7 字节第 %d 字节非 0：err=%v，应为 ErrStructure", pos, err)
		}
	}
}

// 首字节声明的填充长度使「填充 + 盐 + 末 7 字节」超出整个明文：解不出正文，不得越界。
func TestDecryptDatRejectsOverlongPadding(t *testing.T) {
	for _, head := range []byte{0x07, 0xFF} {
		plain := make([]byte, 16)
		plain[0] = head // 1 + 7 + 2 + 7 = 17 > 16
		if _, err := DecryptDat(chainEncrypt(plain)); !errors.Is(err, ErrStructure) {
			t.Errorf("首字节 %#x：err=%v，应为 ErrStructure", head, err)
		}
	}
	// 正例边界：16 字节、填充 6，正文恰为 0 字节。
	plain := make([]byte, 16)
	plain[0] = 0xF6
	got, err := DecryptDat(chainEncrypt(plain))
	if err != nil || len(got) != 0 {
		t.Fatalf("填充 6 的 16 字节明文：got=% x err=%v，应为空正文", got, err)
	}
}

func TestDecryptDatRejectsAnyBitFlip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	cipher := chainEncrypt(layoutPlain(r, randomBytes(r, 37)))
	for bit := range len(cipher) * 8 {
		bad := bytes.Clone(cipher)
		bad[bit/8] ^= 1 << (bit % 8)
		if _, err := DecryptDat(bad); !errors.Is(err, ErrStructure) {
			t.Fatalf("翻转第 %d 块的第 %d 位：err=%v，应为 ErrStructure", bit/64, bit%64, err)
		}
	}
}
