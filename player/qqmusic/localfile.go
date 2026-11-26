package qqmusic

// 本文件是歌词关联的本地文件读取（设计 §6.2、§6.6）：以读、写、删除三种共享读取 QQ 的文件，解开 QQ 缓存目录里的
// qrc，解析关联歌词的原文、翻译、音译三轨。QQ 的文件一律经 readShared 读（assocshared_lint_test.go 守这一条）。

import (
	"bytes"
	"compress/zlib"
	"errors"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

// 本地歌词的上限（设计 §6.2、§6.6）。在线取词不受这些上限约束。
const (
	maxLyricFileBytes = 4 << 20   // 歌词文件
	maxQRCPlainBytes  = 4 << 20   // qrc 解压后的明文
	maxLocalLines     = 50000     // 每轨解析出的行数
	maxMergeCells     = 1_000_000 // 翻译或音译与原文合并的规模 (n+1)×(m+1)，即 alignByTime 动态规划表的格数
	maxLocalWords     = 200_000   // 每轨的逐字总数；音译复制给原文各行的逐字总数；主轨振假名分段总数
	maxLocalTextBytes = 4 << 20   // 音译复制给原文各行的文本总量（RomaText 与逐字文本的字节数之和）；主轨振假名各段 Text 与 Ruby 累计字节数
)

var (
	// errLocalTooLarge 表示文件或 qrc 明文超出上限。
	errLocalTooLarge = i18n.Sentinel("lyric exceeds size limits")
	// errLocalNotDisk 表示路径指向的不是磁盘文件（字符设备、管道等），读起来可能一直不返回。
	errLocalNotDisk = i18n.Sentinel("not a disk file")
	// errLyricFileMissing 表示关联的歌词文件读不到（不存在、打开或读取出错），稍后可能读到（设计 §6.4「暂缺」）。
	errLyricFileMissing = i18n.Sentinel("lyric file is missing")
	// errLyricFileUnusable 表示关联的歌词文件不可用：路径不是绝对路径、超限、解不开或解析不出行（设计 §6.4「不可用」）。
	errLyricFileUnusable = i18n.Sentinel("lyric file is unusable")
)

// openShared 只读打开 path，共享读、写、删除：持有句柄期间 QQ 照常覆盖写、改名、删除该文件（os.Open 的共享模式
// 不含删除）。不加锁、不映射。只接受磁盘文件：管道与字符设备打开后即关闭，返回包装 errLocalNotDisk 的错误。
// 打开失败返回 *fs.PathError，文件或目录不存在时 os.IsNotExist 为真。路径按 longPath 处理长度。
func openShared(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	if t, _ := windows.GetFileType(h); t != windows.FILE_TYPE_DISK {
		windows.CloseHandle(h)
		return nil, i18n.Errorf("open %s: %w", path, errLocalNotDisk)
	}
	return os.NewFile(uintptr(h), path), nil
}

// longPath 给 UTF-16 长度不小于 MAX_PATH（260，含结尾的 NUL 即超限）的绝对路径加 \\?\ 前缀（UNC 路径为 \\?\UNC\），
// 使系统未开启长路径支持时也能打开；带前缀的路径不再经 Win32 规范化，故先 Clean。其余路径，以及已带 \\?\ 或
// \\.\ 前缀的，原样返回。与 player/kugou/assoc 的同名函数相同。
func longPath(p string) string {
	n := 0
	for _, r := range p {
		n += utf16.RuneLen(r)
	}
	if n < windows.MAX_PATH || !filepath.IsAbs(p) || strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return p
	}
	p = filepath.Clean(p)
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

// readShared 以 openShared 读入 path 的全部内容，读完即关闭。内容超过 max 字节时读到 max+1 字节即停，返回
// errLocalTooLarge。打开的错误见 openShared。
func readShared(path string, max int64) ([]byte, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errLocalTooLarge
	}
	return b, nil
}

// qrcFileHeader 是 QQ 缓存目录里 qrc 文件的前 11 字节，即「[offset:0]\n」经 qmc1 异或的结果（设计 §2.3）。
var qrcFileHeader = []byte{0x98, 0x25, 0xb0, 0xac, 0xe3, 0x02, 0x83, 0x68, 0xe8, 0xfc, 0x6c}

// qmc1Key 是 qmc1 的静态异或表（jixunmoe/qmc-decode，LDDC 的 qmc1.py 沿用）。
var qmc1Key = [128]byte{
	0xc3, 0x4a, 0xd6, 0xca, 0x90, 0x67, 0xf7, 0x52, 0xd8, 0xa1, 0x66, 0x62, 0x9f, 0x5b, 0x09, 0x00,
	0xc3, 0x5e, 0x95, 0x23, 0x9f, 0x13, 0x11, 0x7e, 0xd8, 0x92, 0x3f, 0xbc, 0x90, 0xbb, 0x74, 0x0e,
	0xc3, 0x47, 0x74, 0x3d, 0x90, 0xaa, 0x3f, 0x51, 0xd8, 0xf4, 0x11, 0x84, 0x9f, 0xde, 0x95, 0x1d,
	0xc3, 0xc6, 0x09, 0xd5, 0x9f, 0xfa, 0x66, 0xf9, 0xd8, 0xf0, 0xf7, 0xa0, 0x90, 0xa1, 0xd6, 0xf3,
	0xc3, 0xf3, 0xd6, 0xa1, 0x90, 0xa0, 0xf7, 0xf0, 0xd8, 0xf9, 0x66, 0xfa, 0x9f, 0xd5, 0x09, 0xc6,
	0xc3, 0x1d, 0x95, 0xde, 0x9f, 0x84, 0x11, 0xf4, 0xd8, 0x51, 0x3f, 0xaa, 0x90, 0x3d, 0x74, 0x47,
	0xc3, 0x0e, 0x74, 0xbb, 0x90, 0xbc, 0x3f, 0x92, 0xd8, 0x7e, 0x11, 0x13, 0x9f, 0x23, 0x95, 0x5e,
	0xc3, 0x00, 0x09, 0x5b, 0x9f, 0x62, 0x66, 0xa1, 0xd8, 0x52, 0xf7, 0x67, 0x90, 0xca, 0xd6, 0x4a,
}

// decryptLocalQRC 解开 QQ 缓存目录里的 qrc 文件（设计 §2.3、§6.6 第 2 步）：整份按 qmc1 静态表异或（第 i 字节取表的
// (i > 0x7fff ? i % 0x7fff : i) & 0x7f 项），去掉前 11 字节，再经 QRC 3DES 解密与 zlib 解压。
//
// 前 11 字节不是 qrcFileHeader 时返回 ("", false, nil)，由调用方按文本解码。带文件头时第二个返回值为真：(长度 − 11)
// 不是 8 的整数倍、zlib 失败时报错；明文超过 maxQRCPlainBytes 时读到上限加一字节即停，返回 errLocalTooLarge。
func decryptLocalQRC(b []byte) (string, bool, error) {
	if !bytes.HasPrefix(b, qrcFileHeader) {
		return "", false, nil
	}
	body := b[len(qrcFileHeader):]
	if len(body)%8 != 0 {
		return "", true, i18n.Errorf("QRC body length %d is not a multiple of 8", len(body))
	}
	dec := make([]byte, len(body))
	for j, c := range body {
		i := j + len(qrcFileHeader) // 在整份文件里的下标
		if i > 0x7fff {
			i %= 0x7fff
		}
		dec[j] = c ^ qmc1Key[i&0x7f]
	}
	schedule := tripleDesKeySetup(qrc3DESKey, desDecrypt)
	for j := 0; j < len(dec); j += 8 {
		copy(dec[j:], tripleDesCrypt(dec[j:j+8], schedule))
	}
	zr, err := zlib.NewReader(bytes.NewReader(dec))
	if err != nil {
		return "", true, i18n.Errorf("zlib init: %w", err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(io.LimitReader(zr, maxQRCPlainBytes+1))
	if err != nil {
		return "", true, i18n.Errorf("zlib decompress: %w", err)
	}
	if len(plain) > maxQRCPlainBytes {
		return "", true, errLocalTooLarge
	}
	return string(plain), true, nil
}

// trackFailure 是读不成或跳过的一轨：path 为该轨的文件；missing 为真即暂缺（读不到，稍后可能读到）；skipped 为真即文件
// 读成、解析成了，只因与原文合并的规模或音译的复制预算而跳过，取决于原文，不是文件本身的问题；两者都假即文件本身不可用。
type trackFailure struct {
	path             string
	missing, skipped bool
}

// loadLocalLyrics 读取并解析关联的歌词（设计 §6.6）：mainPath 为原文，files 为歌词索引里与之同属一首的翻译、音译，
// 三个文件都经 read 读（出货代码为 readShared，读取者另包一层以响应取消、计算内容摘要、跳过未变的不可用文件）。
// 原文读不到返回包装 errLyricFileMissing 的错误，其余失败返回包装 errLyricFileUnusable 的错误（见 loadLyricTrack），
// failed 只有原文一项，翻译、音译都不读。翻译、音译按同样的步骤解析；读不到、不可用、与原文合并的规模 (n+1)×(m+1) 超过
// maxMergeCells，或音译超出复制预算（见 mergeRomaBounded）时只跳过该轨并打一条 Detail，不影响原文，依次列进 failed
// （后两种记为 skipped），供调用方判暂缺、记下文件本身不可用的轨。返回的行未做时间偏移，也未套 offset。
func loadLocalLyrics(read func(path string, max int64) ([]byte, error), mainPath string, files assoc.LyricFiles) (lines []lyricLine, failed []trackFailure, err error) {
	lines, err = loadLyricTrackWithFurigana(read, mainPath, true)
	if err != nil {
		return nil, []trackFailure{{mainPath, errors.Is(err, errLyricFileMissing), false}}, err
	}
	transMerge := func(lines, track []lyricLine) error {
		mergeTransLines(lines, track)
		return nil
	}
	for _, tr := range []struct {
		path, name string
		merge      func(lines, track []lyricLine) error
	}{
		{files.Trans, i18n.T("translation"), transMerge},
		{files.Roma, i18n.T("romanization"), mergeRomaBounded},
	} {
		if tr.path == "" {
			continue
		}
		track, err := loadLyricTrackWithFurigana(read, tr.path, false)
		loaded := err == nil
		if err == nil && int64(len(lines)+1)*int64(len(track)+1) > maxMergeCells {
			err = i18n.Errorf("%d and %d lines exceed the merge limit", len(lines), len(track))
		}
		if err == nil {
			err = tr.merge(lines, track)
		}
		if err != nil {
			failed = append(failed, trackFailure{tr.path, errors.Is(err, errLyricFileMissing), loaded})
			log.Detail("Skipped the %s track of the associated lyrics: %v", tr.name, err)
		}
	}
	return lines, failed, nil
}

// mergeRomaBounded 是本地歌词的音译合并：按 mergeRomaLines 的规则配对，合并前核对要复制的总量。同一行音译可以配给
// 多行原文（见 mergeRoma 的已知未修），每配一行都另行分配一份逐字切片，(n+1)×(m+1) 的规模判断管不住这一项。
// 复制给各行的逐字总数超过 maxLocalWords，或文本（RomaText 与逐字文本）总量超过 maxLocalTextBytes 时，
// 整轨不合并、不改动 lines，返回错误。
func mergeRomaBounded(lines, roma []lyricLine) error {
	match := romaMatches(lines, roma)
	words, text := 0, 0
	for _, j := range match {
		if j < 0 {
			continue
		}
		words += len(roma[j].Detailed.Words)
		text += len(roma[j].Text)
		for _, w := range roma[j].Detailed.Words {
			text += len(w.Text)
		}
		if words > maxLocalWords || text > maxLocalTextBytes {
			return i18n.Errorf("copying it to the lyric lines exceeds %d per-character units or %d bytes of text", maxLocalWords, maxLocalTextBytes)
		}
	}
	assignRoma(lines, roma, match)
	return nil
}

// loadLyricTrack 经 read 读取并解析一个歌词文件（设计 §6.6 第 1～3 步）：路径须为绝对路径；带 qrc 文件头的按 decryptLocalQRC
// 解开，否则按 player.DecodeLyricFileText 解码；经 html.UnescapeString 后用不带均分兜底的 parseLRCLines 解析。
// 读不到返回包装 errLyricFileMissing 的错误；路径不是绝对路径、超限、不是磁盘文件、解不开、解析出 0 行、超过
// maxLocalLines 行或逐字总数超过 maxLocalWords 返回包装 errLyricFileUnusable 的错误。read 自己返回包装
// errLyricFileUnusable 的错误（读取者认出未变的不可用文件，没有读）时原样返回。
func loadLyricTrack(read func(path string, max int64) ([]byte, error), path string) ([]lyricLine, error) {
	return loadLyricTrackWithFurigana(read, path, false)
}

func loadLyricTrackWithFurigana(read func(path string, max int64) ([]byte, error), path string, withFurigana bool) ([]lyricLine, error) {
	if !filepath.IsAbs(path) {
		return nil, i18n.Errorf("%w: path is not absolute", errLyricFileUnusable)
	}
	b, err := read(path, maxLyricFileBytes)
	switch {
	case errors.Is(err, errLyricFileUnusable):
		return nil, err
	case errors.Is(err, errLocalTooLarge) || errors.Is(err, errLocalNotDisk):
		return nil, i18n.Errorf("%w: %v", errLyricFileUnusable, err)
	case err != nil:
		return nil, i18n.Errorf("%w: %v", errLyricFileMissing, err)
	}
	text, isQRC, err := decryptLocalQRC(b)
	if !isQRC {
		text, err = player.DecodeLyricFileText(b)
	}
	if err != nil {
		return nil, i18n.Errorf("%w: %v", errLyricFileUnusable, err)
	}
	lines, _, _ := parseLRCLinesTrack(html.UnescapeString(text), withFurigana)
	if len(lines) == 0 {
		return nil, i18n.Errorf("%w: no timed lyric lines", errLyricFileUnusable)
	}
	if len(lines) > maxLocalLines {
		return nil, i18n.Errorf("%w: more than %d lines", errLyricFileUnusable, maxLocalLines)
	}
	words := 0
	for _, l := range lines {
		words += len(l.Detailed.Words)
	}
	if words > maxLocalWords {
		return nil, i18n.Errorf("%w: more than %d per-character units", errLyricFileUnusable, maxLocalWords)
	}
	if withFurigana {
		enforceLocalFuriganaBudget(lines)
	}
	return lines, nil
}

// enforceLocalFuriganaBudget 检查本地主歌词各行振假名的总量预算（设计 §6.3）：
// 全曲分段总数超过 maxLocalWords 或各段 Text 与 Ruby 累计字节数超过 maxLocalTextBytes 时，
// 清空全曲各行 Furigana 并记录一条 Detail，保留主歌词正常返回。
func enforceLocalFuriganaBudget(lines []lyricLine) {
	segs, textBytes := 0, 0
	for i := range lines {
		segs += len(lines[i].Furigana)
		for _, seg := range lines[i].Furigana {
			textBytes += len(seg.Text) + len(seg.Ruby)
		}
		if segs > maxLocalWords || textBytes > maxLocalTextBytes {
			for j := range lines {
				lines[j].Furigana = nil
			}
			log.Detail("Skipped QQ Music local furigana: %s", i18n.T("furigana exceeds local lyric size budget"))
			return
		}
	}
}
