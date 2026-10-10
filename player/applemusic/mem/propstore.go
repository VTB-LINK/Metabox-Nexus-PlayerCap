package mem

// 本文件是纯函数：从一段内存字节里解析 Windows 序列化属性存储（MS-PROPSTORE），挑出 Apple Music
// 交给系统媒体会话（SMTC）的那份「正在播放」存储。不碰进程，供离线单测直接喂字节。
//
// 格式（MS-PROPSTORE §2.2）：
//
//	属性存储 = Storage... + [u32 0]                      各 Storage 首尾相接，以一个 0 收尾
//	Storage  = [StorageSize u32]["1SPS"][FMTID 16][Value...][u32 0]
//	Value    = [ValueSize u32][PropID u32][Reserved u8][VT u16][pad u16][值]
//	VT_LPWSTR(0x1F) 的值 = [字符数 u32，含结尾 0][UTF-16LE]
//
// 为什么按格式整条解析、而不是「找到锚点后在附近取一个串」：同一进程里还有别的属性存储也带
// System.Title。审查方真机实测，"1SPS"+Title 锚点命中 18 处，其中 17 处是 Windows 设置项的搜索
// 关键词串（「聚焦;Windows 聚焦;图像;…」），旧实现按众数取值，150 秒内把关键词串当歌名发出
// 48 次。Apple Music 的那份存储形态固定（2026-10-08 真机，6 首歌的全部完整副本均如此）：
//
//	64440492 媒体类别 {D1607DBC-…}（可缺）> F29F85E0 标题（#2）+ 封面（#27，VT_STREAM）
//	> 56A3372E「歌手 — 专辑」（#2）> 6FE41453 应用名「Apple Music」（#1001）> BAC8804B
//
// 判据取其中两段：同一条链里同时有 System.Music 段和应用名段，且链按格式完整收尾。搜索关键词那类
// 存储没有这两段；换歌后旧歌留下的残片（只剩 Title > Music 两段、无应用名）也被挡掉。整条按记录
// 长度读，标题与歌手不再被截断（旧实现只看锚点后 120 字节）。
//
// 封面就在同一条存储里：标题段的 #27（System.ThumbnailStream，VT_STREAM）是整张图片的字节，与系统
// 媒体会话的缩略图逐字节相同（2026-10-06 真机比对）。它与标题同属一条链，归属是结构性的，不靠时间猜。
// VT_STREAM 的值 = [流名字节数 u32][流名 UTF-16「prop4294967295」含结尾 0][补齐到 4 字节][流长度 u32][流内容]
// （2026-10-08 真机，夹具 nowplaying_full.bin 即此布局）。实测全是 JPEG，22~188 KB。只有带媒体类别段的
// 完整副本带封面，完整副本也有不带封面的（2026-10-09 真机）。

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"
)

// NowPlaying 是一份 Apple Music「正在播放」属性存储里的元数据。
type NowPlaying struct {
	Title  string
	Artist string
	Album  string
	// Full 表示链首带媒体类别段（64440492）。实测当前歌总有至少一份完整副本，另有不带该段、
	// 也不带封面的副本。完整副本不一定是当前歌的：不带封面的完整副本会残留，跨好几首歌（2026-10-09 真机）；
	// 多份并存时怎么挑由调用方定。
	Full bool
	// Addr 是链头的位置：findNowPlaying 填的是它在所给字节里的偏移，Reader.Scan 换成进程内的绝对地址。
	// Sig 是链头起最多 sigLen 字节的原样拷贝。两者合起来用来廉价地盯住这份存储：一首歌播放期间它
	// 原地不动（2026-10-08 真机，155 秒没变），换歌时整条在新地址重建或就地改写成新歌、旧的被释放或覆盖——之后再读
	// Addr 处的字节与 Sig 对不上，就是「存储换了」。地址跨歌不稳定，别拿它去取下一首的元数据。
	Addr uintptr
	Sig  []byte
	// Cover 是同一条链里的封面图片字节（JPEG 或 PNG，已拷贝，不引用扫描缓冲）；没有、过大或不是这两种
	// 格式时为 nil。CoverMIME 是 "image/jpeg" 或 "image/png"。
	Cover     []byte
	CoverMIME string
}

var (
	spsMagic = []byte("1SPS")

	fmtidMediaClass = guid(0x64440492, 0x4C8B, 0x11D1, 0x8B, 0x70, 0x08, 0x00, 0x36, 0xB1, 0x1A, 0x03)
	fmtidSummary    = guid(0xF29F85E0, 0x4FF9, 0x1068, 0xAB, 0x91, 0x08, 0x00, 0x2B, 0x27, 0xB3, 0xD9) // System.Title = #2
	fmtidMusic      = guid(0x56A3372E, 0xCE9C, 0x11D2, 0x9F, 0x0E, 0x00, 0x60, 0x97, 0xC6, 0x86, 0xF6) // System.Music.Artist = #2
	fmtidAppName    = guid(0x6FE41453, 0xB3D2, 0x4342, 0xB9, 0xCB, 0x5C, 0x06, 0xEF, 0x1F, 0x4B, 0xB1) // #1001 = 应用名
)

const (
	pidTitle     = 2
	pidThumbnail = 27 // System.ThumbnailStream
	pidArtist    = 2
	pidAppName   = 1001
	vtLPWSTR     = 0x1F
	vtStream     = 0x42

	// maxCoverBytes 封面字节数上限，与 player.FetchCoverBase64 的 5MB 一致；超过就不带封面。
	maxCoverBytes = 5 << 20

	// sigLen 是 NowPlaying.Sig 的长度上限。完整副本里媒体类别段 125 字节、标题段头 24 字节、标题值
	// 17+2×(字数+1) 字节，随后就是封面流的头（含流长度）；512 字节能盖到 150 字以内标题的流长度，
	// 于是标题变了、封面换了（流长度不同）都会体现在这段字节里。
	sigLen = 512
)

var (
	jpegSOI = []byte{0xFF, 0xD8, 0xFF}
	jpegEOI = []byte{0xFF, 0xD9}
	pngSig  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	pngEnd  = []byte{'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82}
)

func guid(a uint32, b, c uint16, d ...byte) [16]byte {
	var g [16]byte
	binary.LittleEndian.PutUint32(g[0:], a)
	binary.LittleEndian.PutUint16(g[4:], b)
	binary.LittleEndian.PutUint16(g[6:], c)
	copy(g[8:], d)
	return g
}

// storage 是解析出的一段 Storage。strs 收 VT_LPWSTR 值、blobs 收 VT_STREAM 的流内容（都按 PropID）；
// 其余类型只跳过。blobs 引用输入缓冲、未拷贝，要留用须自行拷贝。
type storage struct {
	size  int
	fmtid [16]byte
	strs  map[uint32]string
	blobs map[uint32][]byte
}

// parseStorage 解析 off 处（StorageSize 字段）的一段 Storage；格式不符返回 ok=false。
// 每个长度都先校验再用：输入是别的进程的任意字节，越界即拒、绝不 panic。
func parseStorage(d []byte, off int) (storage, bool) {
	if off < 0 || off+24 > len(d) {
		return storage{}, false
	}
	size := int(binary.LittleEndian.Uint32(d[off:]))
	if size < 28 || size > len(d)-off || !bytes.Equal(d[off+4:off+8], spsMagic) {
		return storage{}, false
	}
	st := storage{size: size}
	copy(st.fmtid[:], d[off+8:off+24])
	end := off + size
	for p := off + 24; ; {
		if p+4 > end {
			return storage{}, false
		}
		vs := int(binary.LittleEndian.Uint32(d[p:]))
		if vs == 0 {
			if p+4 != end { // 值表的 0 结尾必须正好落在 Storage 末尾
				return storage{}, false
			}
			return st, true
		}
		if vs < 13 || vs > end-p {
			return storage{}, false
		}
		id := binary.LittleEndian.Uint32(d[p+4:])
		v := d[p+13 : p+vs]
		switch binary.LittleEndian.Uint16(d[p+9:]) {
		case vtLPWSTR:
			if len(v) >= 4 {
				n := int(binary.LittleEndian.Uint32(v))
				if n > 0 && n <= (len(v)-4)/2 {
					if st.strs == nil {
						st.strs = map[uint32]string{}
					}
					st.strs[id] = decodeUTF16Z(v[4 : 4+2*n])
				}
			}
		case vtStream:
			if b, ok := streamBytes(v); ok {
				if st.blobs == nil {
					st.blobs = map[uint32][]byte{}
				}
				st.blobs[id] = b
			}
		}
		p += vs
	}
}

// streamBytes 取 VT_STREAM 值里的流内容（布局见文件头）。长度逐个校验，对不上即拒。
func streamBytes(v []byte) ([]byte, bool) {
	if len(v) < 4 {
		return nil, false
	}
	nameLen := int(binary.LittleEndian.Uint32(v))
	if nameLen > len(v)-4 {
		return nil, false
	}
	o := (4 + nameLen + 3) &^ 3 // 流名之后补齐到 4 字节
	if o+4 > len(v) {
		return nil, false
	}
	n := int(binary.LittleEndian.Uint32(v[o:]))
	if n <= 0 || n > len(v)-o-4 {
		return nil, false
	}
	return v[o+4 : o+4+n], true
}

// imageMIME 按首尾标记判定图片格式：JPEG 以 FFD8FF 开头、FFD9 结尾，PNG 以签名开头、IEND 块结尾。
// 不是这两种、或首尾对不上（例如存储正被改写、只读到半张图）返回 ""。结尾标记之后允许有补齐的 0。
func imageMIME(b []byte) string {
	if len(b) > maxCoverBytes {
		return ""
	}
	t := bytes.TrimRight(b, "\x00")
	switch {
	case bytes.HasPrefix(t, jpegSOI) && bytes.HasSuffix(t, jpegEOI) && len(t) > len(jpegSOI)+len(jpegEOI):
		return "image/jpeg"
	case bytes.HasPrefix(t, pngSig) && bytes.HasSuffix(t, pngEnd):
		return "image/png"
	}
	return ""
}

// findNowPlaying 在一段内存里找出所有 Apple Music「正在播放」存储（判据见文件头）。
// 同值多份即返回多项，供调用方计票。
func findNowPlaying(d []byte) []NowPlaying {
	var offs []int
	for from := 0; ; {
		i := bytes.Index(d[from:], spsMagic)
		if i < 0 {
			break
		}
		if a := from + i; a >= 4 {
			offs = append(offs, a-4)
		}
		from += i + len(spsMagic)
	}
	parsed := make(map[int]storage, len(offs))
	pointed := map[int]bool{} // 被前一段 off+size 指到的，不是链头
	for _, o := range offs {
		if st, ok := parseStorage(d, o); ok {
			parsed[o] = st
			pointed[o+st.size] = true
		}
	}
	var out []NowPlaying
	for _, o := range offs { // offs 升序，结果顺序确定
		if _, ok := parsed[o]; !ok || pointed[o] {
			continue
		}
		np := NowPlaying{Addr: uintptr(o)}
		var cover []byte
		var hasMusic, hasApp bool
		cur := o
		for {
			st, ok := parsed[cur]
			if !ok {
				break
			}
			switch st.fmtid {
			case fmtidMediaClass:
				np.Full = true
			case fmtidSummary:
				np.Title = st.strs[pidTitle]
				cover = st.blobs[pidThumbnail]
			case fmtidMusic:
				if v, ok := st.strs[pidArtist]; ok {
					hasMusic = true
					np.Artist, np.Album = splitArtistAlbum(v)
				}
			case fmtidAppName:
				hasApp = st.strs[pidAppName] != ""
			}
			cur += st.size
		}
		// 链必须以 StorageSize=0 收尾：尾部被覆盖的残片不算。
		terminated := cur+4 <= len(d) && binary.LittleEndian.Uint32(d[cur:]) == 0
		if terminated && hasMusic && hasApp && np.Title != "" {
			if mime := imageMIME(cover); mime != "" {
				np.Cover, np.CoverMIME = append([]byte(nil), cover...), mime
			}
			sigEnd := cur + 4 // 链尾（含收尾的 0）
			if sigEnd > o+sigLen {
				sigEnd = o + sigLen
			}
			np.Sig = append([]byte(nil), d[o:sigEnd]...)
			out = append(out, np)
		}
	}
	return out
}

// splitArtistAlbum 拆 Apple Music 写进 System.Music.Artist 的「歌手 — 专辑」（U+2014，两侧各一空格）。
// 无分隔符则整串是歌手。
func splitArtistAlbum(v string) (artist, album string) {
	const sep = " — "
	if i := bytes.Index([]byte(v), []byte(sep)); i >= 0 {
		return v[:i], v[i+len(sep):]
	}
	return v, ""
}

// decodeUTF16Z 解 UTF-16LE，遇 0 截止；代理对按 unicode/utf16 合成（emoji、CJK 扩展 B 不变成 U+FFFD）。
func decodeUTF16Z(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}
