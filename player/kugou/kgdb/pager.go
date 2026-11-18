// Package kgdb 读取酷狗的歌词关联库 KGMusicV3.db：逐页加密的 SQLite。
//
// 加密格式与主密钥同 unlock-music 的 pc_kugou_db（参考其 Go 移植
// https://github.com/skxxxkx666/Kugo-Music-Converter/blob/main/backend/internal/algo/kgg/database.go）：
// 页 1024 字节，第 n 页以 AES-128-CBC 加密，无填充；
// 密钥 = MD5(主密钥 ‖ LE32(n) ‖ "sAlT")，IV = MD5(由 n 线性同余递推出的 16 字节)。
// 第 1 页的 [16:24] 存明文头字节，对应的密文挪在 [8:16]，解密后须与明文头相等（完整性校验）。
package kgdb

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/binary"
	"io"
	"math"

	"Metabox-Nexus-PlayerCap/i18n"
)

// PageSize 是加密的分组单位，也是库的页大小。
const PageSize = 1024

// ErrUnsupportedDB 表示库不是认得的酷狗加密格式；具体原因以 %w 包装在外。
// 读失败不归入此类（调用方按暂时性失败处理）。
var ErrUnsupportedDB = i18n.Sentinel("unsupported KuGou database format")

// Pager 按页号取解密后的页。
type Pager interface {
	// PageCount 返回库的总页数。
	PageCount() uint32
	// Page 返回第 n 页（1 起）解密后的 PageSize 字节，调用方不得改写。
	// 每次调用恰好对底层做一次 ReadAt(buf[:PageSize], int64(n-1)*PageSize)，不缓存：
	// 上层包装 ReaderAt，按偏移得出页号，把每次取页按顺序记入摘要并检查取消，缓存会让取页漏记；
	// 读取器也不持有整库（设计 §6.10）。
	Page(n uint32) ([]byte, error)
}

// 主密钥，同 pc_kugou_db。
var masterKey = [16]byte{0x1D, 0x61, 0x31, 0x45, 0xB2, 0x47, 0xBF, 0x7F, 0x3D, 0x18, 0x96, 0x72, 0x14, 0x4F, 0xE4, 0xBF}

var sqliteMagic = []byte("SQLite format 3\x00")

type pager struct {
	r     io.ReaderAt
	count uint32
}

// NewPager 以文件大小 size 建立取页层，读第 1 页并做头部与完整性校验。
// size 不是 PageSize 的正整数倍或页数超出 uint32、未加密的 SQLite、校验不过：返回包装 ErrUnsupportedDB 的错误；
// 读失败原样包装返回。
func NewPager(r io.ReaderAt, size int64) (Pager, error) {
	if size <= 0 || size%PageSize != 0 || size/PageSize > math.MaxUint32 {
		return nil, i18n.Errorf("%w: invalid file size %d", ErrUnsupportedDB, size)
	}
	p := &pager{r: r, count: uint32(size / PageSize)}
	if _, err := p.Page(1); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *pager) PageCount() uint32 { return p.count }

// Page 每次新分配缓冲区，返回的切片不与其他调用共享。
func (p *pager) Page(n uint32) ([]byte, error) {
	if n == 0 || n > p.count {
		return nil, i18n.Errorf("%w: page %d out of range [1, %d]", ErrUnsupportedDB, n, p.count)
	}
	buf := make([]byte, PageSize)
	// io.ReaderAt 读满时可能同时返回 io.EOF，以读到的字节数为准。
	if got, err := p.r.ReadAt(buf, int64(n-1)*PageSize); got < PageSize {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, i18n.Errorf("read KuGou database page %d: %w", n, err)
	}
	if n > 1 {
		decryptPage(buf, n)
		return buf, nil
	}
	if err := decryptPage1(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// decryptPage1 原地解密第 1 页并换回 SQLite 魔数。
func decryptPage1(buf []byte) error {
	// 明文库在这里显式拒绝，不依赖解密乱码碰巧通不过完整性校验。
	if bytes.Equal(buf[:16], sqliteMagic) {
		return i18n.Errorf("%w: database is not encrypted", ErrUnsupportedDB)
	}
	if !validatePage1Header(buf[16:24]) {
		return i18n.Errorf("%w: page 1 header check failed", ErrUnsupportedDB)
	}
	var want [8]byte
	copy(want[:], buf[16:24])
	copy(buf[16:24], buf[8:16]) // 密文 = [8:16] ‖ [24:1024]
	decryptPage(buf[16:], 1)
	if !bytes.Equal(buf[16:24], want[:]) {
		return i18n.Errorf("%w: page 1 integrity check failed", ErrUnsupportedDB)
	}
	copy(buf[:16], sqliteMagic)
	return nil
}

// validatePage1Header 核对第 1 页 [16:24] 的明文头，判据同 pc_kugou_db：
// 偏移 0x14 起的小端 uint32 为 0x20204000（保留字节 0 与负载比例 64/32/32），
// 偏移 0x10 的大端页大小为 512～32768 的 2 的幂（字段值 1 表示的 65536 不认）。
func validatePage1Header(h []byte) bool {
	ps := uint32(binary.BigEndian.Uint16(h[0:2]))
	return binary.LittleEndian.Uint32(h[4:8]) == 0x20204000 && ps >= 512 && ps&(ps-1) == 0
}

// decryptPage 以第 n 页的密钥与 IV 原地 AES-128-CBC 解密 buf（长度为 16 的倍数）。
func decryptPage(buf []byte, n uint32) {
	var k [24]byte
	copy(k[:], masterKey[:])
	binary.LittleEndian.PutUint32(k[16:], n)
	binary.LittleEndian.PutUint32(k[20:], 0x546C4173) // "sAlT"
	key := md5.Sum(k[:])

	var seed [16]byte
	s := n + 1
	for i := 0; i < len(seed); i += 4 {
		s = nextIVSeed(s)
		binary.LittleEndian.PutUint32(seed[i:], s)
	}
	iv := md5.Sum(seed[:])

	block, _ := aes.NewCipher(key[:]) // 密钥恒为 16 字节，不会出错
	cipher.NewCBCDecrypter(block, iv[:]).CryptBlocks(buf, buf)
}

// nextIVSeed 是 IV 种子的线性同余递推，均为 uint32 运算，同 pc_kugou_db。
func nextIVSeed(s uint32) uint32 {
	v := s*0x9EF4 - s/0xCE26*0x7FFFFF07
	if v&0x80000000 != 0 {
		v += 0x7FFFFF07
	}
	return v
}
