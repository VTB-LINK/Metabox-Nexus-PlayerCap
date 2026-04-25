package qqmusic

// QRC 3DES decryption - ported from QQMusicApi Python implementation.
// Go's crypto/des uses different internal bit representations than QQ Music's
// custom DES, so we port the exact algorithm for byte-perfect compatibility.

const (
	desEncrypt = 1
	desDecrypt = 0
)

var desSbox = [8][64]int{
	{14, 4, 13, 1, 2, 15, 11, 8, 3, 10, 6, 12, 5, 9, 0, 7,
		0, 15, 7, 4, 14, 2, 13, 1, 10, 6, 12, 11, 9, 5, 3, 8,
		4, 1, 14, 8, 13, 6, 2, 11, 15, 12, 9, 7, 3, 10, 5, 0,
		15, 12, 8, 2, 4, 9, 1, 7, 5, 11, 3, 14, 10, 0, 6, 13},
	{15, 1, 8, 14, 6, 11, 3, 4, 9, 7, 2, 13, 12, 0, 5, 10,
		3, 13, 4, 7, 15, 2, 8, 15, 12, 0, 1, 10, 6, 9, 11, 5,
		0, 14, 7, 11, 10, 4, 13, 1, 5, 8, 12, 6, 9, 3, 2, 15,
		13, 8, 10, 1, 3, 15, 4, 2, 11, 6, 7, 12, 0, 5, 14, 9},
	{10, 0, 9, 14, 6, 3, 15, 5, 1, 13, 12, 7, 11, 4, 2, 8,
		13, 7, 0, 9, 3, 4, 6, 10, 2, 8, 5, 14, 12, 11, 15, 1,
		13, 6, 4, 9, 8, 15, 3, 0, 11, 1, 2, 12, 5, 10, 14, 7,
		1, 10, 13, 0, 6, 9, 8, 7, 4, 15, 14, 3, 11, 5, 2, 12},
	{7, 13, 14, 3, 0, 6, 9, 10, 1, 2, 8, 5, 11, 12, 4, 15,
		13, 8, 11, 5, 6, 15, 0, 3, 4, 7, 2, 12, 1, 10, 14, 9,
		10, 6, 9, 0, 12, 11, 7, 13, 15, 1, 3, 14, 5, 2, 8, 4,
		3, 15, 0, 6, 10, 10, 13, 8, 9, 4, 5, 11, 12, 7, 2, 14},
	{2, 12, 4, 1, 7, 10, 11, 6, 8, 5, 3, 15, 13, 0, 14, 9,
		14, 11, 2, 12, 4, 7, 13, 1, 5, 0, 15, 10, 3, 9, 8, 6,
		4, 2, 1, 11, 10, 13, 7, 8, 15, 9, 12, 5, 6, 3, 0, 14,
		11, 8, 12, 7, 1, 14, 2, 13, 6, 15, 0, 9, 10, 4, 5, 3},
	{12, 1, 10, 15, 9, 2, 6, 8, 0, 13, 3, 4, 14, 7, 5, 11,
		10, 15, 4, 2, 7, 12, 9, 5, 6, 1, 13, 14, 0, 11, 3, 8,
		9, 14, 15, 5, 2, 8, 12, 3, 7, 0, 4, 10, 1, 13, 11, 6,
		4, 3, 2, 12, 9, 5, 15, 10, 11, 14, 1, 7, 6, 0, 8, 13},
	{4, 11, 2, 14, 15, 0, 8, 13, 3, 12, 9, 7, 5, 10, 6, 1,
		13, 0, 11, 7, 4, 9, 1, 10, 14, 3, 5, 12, 2, 15, 8, 6,
		1, 4, 11, 13, 12, 3, 7, 14, 10, 15, 6, 8, 0, 5, 9, 2,
		6, 11, 13, 8, 1, 4, 10, 7, 9, 5, 0, 15, 14, 2, 3, 12},
	{13, 2, 8, 4, 6, 15, 11, 1, 10, 9, 3, 14, 5, 0, 12, 7,
		1, 15, 13, 8, 10, 3, 7, 4, 12, 5, 6, 11, 0, 14, 9, 2,
		7, 11, 4, 1, 9, 12, 14, 2, 0, 6, 10, 13, 15, 3, 5, 8,
		2, 1, 14, 7, 4, 10, 8, 13, 15, 12, 9, 0, 3, 5, 6, 11},
}

func desBitnum(a []byte, b int, c int) int {
	return int(((int(a[(b/32)*4+3-(b%32)/8]) >> (7 - b%8)) & 1) << c)
}

func desBitnumIntr(a int, b int, c int) int {
	return ((a >> (31 - b)) & 1) << c
}

func desBitnumIntl(a int, b int, c int) int {
	return int(((uint32(a) << b) & 0x80000000) >> c)
}

func desSboxBit(a int) int {
	return (a & 32) | ((a & 31) >> 1) | ((a & 1) << 4)
}

func desInitialPermutation(input []byte) (int, int) {
	return (desBitnum(input, 57, 31) | desBitnum(input, 49, 30) | desBitnum(input, 41, 29) | desBitnum(input, 33, 28) |
			desBitnum(input, 25, 27) | desBitnum(input, 17, 26) | desBitnum(input, 9, 25) | desBitnum(input, 1, 24) |
			desBitnum(input, 59, 23) | desBitnum(input, 51, 22) | desBitnum(input, 43, 21) | desBitnum(input, 35, 20) |
			desBitnum(input, 27, 19) | desBitnum(input, 19, 18) | desBitnum(input, 11, 17) | desBitnum(input, 3, 16) |
			desBitnum(input, 61, 15) | desBitnum(input, 53, 14) | desBitnum(input, 45, 13) | desBitnum(input, 37, 12) |
			desBitnum(input, 29, 11) | desBitnum(input, 21, 10) | desBitnum(input, 13, 9) | desBitnum(input, 5, 8) |
			desBitnum(input, 63, 7) | desBitnum(input, 55, 6) | desBitnum(input, 47, 5) | desBitnum(input, 39, 4) |
			desBitnum(input, 31, 3) | desBitnum(input, 23, 2) | desBitnum(input, 15, 1) | desBitnum(input, 7, 0)),
		(desBitnum(input, 56, 31) | desBitnum(input, 48, 30) | desBitnum(input, 40, 29) | desBitnum(input, 32, 28) |
			desBitnum(input, 24, 27) | desBitnum(input, 16, 26) | desBitnum(input, 8, 25) | desBitnum(input, 0, 24) |
			desBitnum(input, 58, 23) | desBitnum(input, 50, 22) | desBitnum(input, 42, 21) | desBitnum(input, 34, 20) |
			desBitnum(input, 26, 19) | desBitnum(input, 18, 18) | desBitnum(input, 10, 17) | desBitnum(input, 2, 16) |
			desBitnum(input, 60, 15) | desBitnum(input, 52, 14) | desBitnum(input, 44, 13) | desBitnum(input, 36, 12) |
			desBitnum(input, 28, 11) | desBitnum(input, 20, 10) | desBitnum(input, 12, 9) | desBitnum(input, 4, 8) |
			desBitnum(input, 62, 7) | desBitnum(input, 54, 6) | desBitnum(input, 46, 5) | desBitnum(input, 38, 4) |
			desBitnum(input, 30, 3) | desBitnum(input, 22, 2) | desBitnum(input, 14, 1) | desBitnum(input, 6, 0))
}

func desInversePermutation(s0, s1 int) []byte {
	data := make([]byte, 8)
	data[3] = byte(desBitnumIntr(s1, 7, 7) | desBitnumIntr(s0, 7, 6) | desBitnumIntr(s1, 15, 5) | desBitnumIntr(s0, 15, 4) | desBitnumIntr(s1, 23, 3) | desBitnumIntr(s0, 23, 2) | desBitnumIntr(s1, 31, 1) | desBitnumIntr(s0, 31, 0))
	data[2] = byte(desBitnumIntr(s1, 6, 7) | desBitnumIntr(s0, 6, 6) | desBitnumIntr(s1, 14, 5) | desBitnumIntr(s0, 14, 4) | desBitnumIntr(s1, 22, 3) | desBitnumIntr(s0, 22, 2) | desBitnumIntr(s1, 30, 1) | desBitnumIntr(s0, 30, 0))
	data[1] = byte(desBitnumIntr(s1, 5, 7) | desBitnumIntr(s0, 5, 6) | desBitnumIntr(s1, 13, 5) | desBitnumIntr(s0, 13, 4) | desBitnumIntr(s1, 21, 3) | desBitnumIntr(s0, 21, 2) | desBitnumIntr(s1, 29, 1) | desBitnumIntr(s0, 29, 0))
	data[0] = byte(desBitnumIntr(s1, 4, 7) | desBitnumIntr(s0, 4, 6) | desBitnumIntr(s1, 12, 5) | desBitnumIntr(s0, 12, 4) | desBitnumIntr(s1, 20, 3) | desBitnumIntr(s0, 20, 2) | desBitnumIntr(s1, 28, 1) | desBitnumIntr(s0, 28, 0))
	data[7] = byte(desBitnumIntr(s1, 3, 7) | desBitnumIntr(s0, 3, 6) | desBitnumIntr(s1, 11, 5) | desBitnumIntr(s0, 11, 4) | desBitnumIntr(s1, 19, 3) | desBitnumIntr(s0, 19, 2) | desBitnumIntr(s1, 27, 1) | desBitnumIntr(s0, 27, 0))
	data[6] = byte(desBitnumIntr(s1, 2, 7) | desBitnumIntr(s0, 2, 6) | desBitnumIntr(s1, 10, 5) | desBitnumIntr(s0, 10, 4) | desBitnumIntr(s1, 18, 3) | desBitnumIntr(s0, 18, 2) | desBitnumIntr(s1, 26, 1) | desBitnumIntr(s0, 26, 0))
	data[5] = byte(desBitnumIntr(s1, 1, 7) | desBitnumIntr(s0, 1, 6) | desBitnumIntr(s1, 9, 5) | desBitnumIntr(s0, 9, 4) | desBitnumIntr(s1, 17, 3) | desBitnumIntr(s0, 17, 2) | desBitnumIntr(s1, 25, 1) | desBitnumIntr(s0, 25, 0))
	data[4] = byte(desBitnumIntr(s1, 0, 7) | desBitnumIntr(s0, 0, 6) | desBitnumIntr(s1, 8, 5) | desBitnumIntr(s0, 8, 4) | desBitnumIntr(s1, 16, 3) | desBitnumIntr(s0, 16, 2) | desBitnumIntr(s1, 24, 1) | desBitnumIntr(s0, 24, 0))
	return data
}

func desF(state int, key [6]int) int {
	t1 := desBitnumIntl(state, 31, 0) | int((uint32(state)&0xF0000000)>>1) |
		desBitnumIntl(state, 4, 5) | desBitnumIntl(state, 3, 6) |
		int((uint32(state)&0x0F000000)>>3) |
		desBitnumIntl(state, 8, 11) | desBitnumIntl(state, 7, 12) |
		int((uint32(state)&0x00F00000)>>5) |
		desBitnumIntl(state, 12, 17) | desBitnumIntl(state, 11, 18) |
		int((uint32(state)&0x000F0000)>>7) |
		desBitnumIntl(state, 16, 23)

	t2 := desBitnumIntl(state, 15, 0) | ((state & 0x0000F000) << 15) |
		desBitnumIntl(state, 20, 5) | desBitnumIntl(state, 19, 6) |
		((state & 0x00000F00) << 13) |
		desBitnumIntl(state, 24, 11) | desBitnumIntl(state, 23, 12) |
		((state & 0x000000F0) << 11) |
		desBitnumIntl(state, 28, 17) | desBitnumIntl(state, 27, 18) |
		((state & 0x0000000F) << 9) |
		desBitnumIntl(state, 0, 23)

	lrgstate := [6]int{
		((t1 >> 24) & 0xFF) ^ key[0],
		((t1 >> 16) & 0xFF) ^ key[1],
		((t1 >> 8) & 0xFF) ^ key[2],
		((t2 >> 24) & 0xFF) ^ key[3],
		((t2 >> 16) & 0xFF) ^ key[4],
		((t2 >> 8) & 0xFF) ^ key[5],
	}

	state = (desSbox[0][desSboxBit(lrgstate[0]>>2)] << 28) |
		(desSbox[1][desSboxBit(((lrgstate[0]&0x03)<<4)|(lrgstate[1]>>4))] << 24) |
		(desSbox[2][desSboxBit(((lrgstate[1]&0x0F)<<2)|(lrgstate[2]>>6))] << 20) |
		(desSbox[3][desSboxBit(lrgstate[2]&0x3F)] << 16) |
		(desSbox[4][desSboxBit(lrgstate[3]>>2)] << 12) |
		(desSbox[5][desSboxBit(((lrgstate[3]&0x03)<<4)|(lrgstate[4]>>4))] << 8) |
		(desSbox[6][desSboxBit(((lrgstate[4]&0x0F)<<2)|(lrgstate[5]>>6))] << 4) |
		desSbox[7][desSboxBit(lrgstate[5]&0x3F)]

	return desBitnumIntl(state, 15, 0) | desBitnumIntl(state, 6, 1) | desBitnumIntl(state, 19, 2) |
		desBitnumIntl(state, 20, 3) | desBitnumIntl(state, 28, 4) | desBitnumIntl(state, 11, 5) |
		desBitnumIntl(state, 27, 6) | desBitnumIntl(state, 16, 7) | desBitnumIntl(state, 0, 8) |
		desBitnumIntl(state, 14, 9) | desBitnumIntl(state, 22, 10) | desBitnumIntl(state, 25, 11) |
		desBitnumIntl(state, 4, 12) | desBitnumIntl(state, 17, 13) | desBitnumIntl(state, 30, 14) |
		desBitnumIntl(state, 9, 15) | desBitnumIntl(state, 1, 16) | desBitnumIntl(state, 7, 17) |
		desBitnumIntl(state, 23, 18) | desBitnumIntl(state, 13, 19) | desBitnumIntl(state, 31, 20) |
		desBitnumIntl(state, 26, 21) | desBitnumIntl(state, 2, 22) | desBitnumIntl(state, 8, 23) |
		desBitnumIntl(state, 18, 24) | desBitnumIntl(state, 12, 25) | desBitnumIntl(state, 29, 26) |
		desBitnumIntl(state, 5, 27) | desBitnumIntl(state, 21, 28) | desBitnumIntl(state, 10, 29) |
		desBitnumIntl(state, 3, 30) | desBitnumIntl(state, 24, 31)
}

func desCrypt(input []byte, key [16][6]int) []byte {
	s0, s1 := desInitialPermutation(input)
	for idx := 0; idx < 15; idx++ {
		prevS1 := s1
		s1 = desF(s1, key[idx]) ^ s0
		s0 = prevS1
	}
	s0 = desF(s1, key[15]) ^ s0
	return desInversePermutation(s0, s1)
}

var desKeyRndShift = [16]int{1, 1, 2, 2, 2, 2, 2, 2, 1, 2, 2, 2, 2, 2, 2, 1}
var desKeyPermC = [28]int{56, 48, 40, 32, 24, 16, 8, 0, 57, 49, 41, 33, 25, 17, 9, 1, 58, 50, 42, 34, 26, 18, 10, 2, 59, 51, 43, 35}
var desKeyPermD = [28]int{62, 54, 46, 38, 30, 22, 14, 6, 61, 53, 45, 37, 29, 21, 13, 5, 60, 52, 44, 36, 28, 20, 12, 4, 27, 19, 11, 3}
var desKeyCompression = [48]int{13, 16, 10, 23, 0, 4, 2, 27, 14, 5, 20, 9, 22, 18, 11, 3, 25, 7, 15, 6, 26, 19, 12, 1, 40, 51, 30, 36, 46, 54, 29, 39, 50, 44, 32, 47, 43, 48, 38, 55, 33, 52, 45, 41, 49, 35, 28, 31}

func desKeySchedule(key []byte, mode int) [16][6]int {
	var schedule [16][6]int
	c := 0
	for i := 0; i < 28; i++ {
		c |= desBitnum(key, desKeyPermC[i], 31-i)
	}
	d := 0
	for i := 0; i < 28; i++ {
		d |= desBitnum(key, desKeyPermD[i], 31-i)
	}
	for i := 0; i < 16; i++ {
		c = int((uint32(c)<<desKeyRndShift[i])|(uint32(c)>>(28-desKeyRndShift[i]))) & int(0xFFFFFFF0)
		d = int((uint32(d)<<desKeyRndShift[i])|(uint32(d)>>(28-desKeyRndShift[i]))) & int(0xFFFFFFF0)
		togen := i
		if mode == desDecrypt {
			togen = 15 - i
		}
		for j := 0; j < 6; j++ {
			schedule[togen][j] = 0
		}
		for j := 0; j < 24; j++ {
			schedule[togen][j/8] |= desBitnumIntr(c, desKeyCompression[j], 7-(j%8))
		}
		for j := 24; j < 48; j++ {
			schedule[togen][j/8] |= desBitnumIntr(d, desKeyCompression[j]-27, 7-(j%8))
		}
	}
	return schedule
}

func tripleDesKeySetup(key []byte, mode int) [3][16][6]int {
	var result [3][16][6]int
	if mode == desEncrypt {
		result[0] = desKeySchedule(key[0:], desEncrypt)
		result[1] = desKeySchedule(key[8:], desDecrypt)
		result[2] = desKeySchedule(key[16:], desEncrypt)
	} else {
		result[0] = desKeySchedule(key[16:], desDecrypt)
		result[1] = desKeySchedule(key[8:], desEncrypt)
		result[2] = desKeySchedule(key[0:], desDecrypt)
	}
	return result
}

func tripleDesCrypt(data []byte, key [3][16][6]int) []byte {
	d := make([]byte, 8)
	copy(d, data)
	for i := 0; i < 3; i++ {
		d = desCrypt(d, key[i])
	}
	return d
}
