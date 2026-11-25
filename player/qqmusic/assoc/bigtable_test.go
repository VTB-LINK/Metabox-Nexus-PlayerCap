package assoc

// 本文件只测大曲库下关联表的解密与解析（设计 §6.10「关联表」、§7「大曲库」）：5k 条与 3 万条的耗时与累计分配（基准），
// 3 万条在 16 MiB 之内、解析成功、累计分配不超过文件大小的 8 倍。合成数据在测试内生成、以测试内的 TEA 加密，每条 494 字节，
// 即本机均值（§2.2：92 条 45,424 字节）。超出 16 MiB 时读取者怎么办见 player/qqmusic/assoctablecap_test.go。

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// bigEntryUnits 是合成关联表每条的 UTF-16 码元数，加密后每条 494 字节。
const bigEntryUnits = 247

// bigAssocEntry 是合成关联表的第 i 条：每 20 条里 1 条本地文件、1 条 id 键，其余为 mid 键（本机 92 条里在线 88 条、本地 4 条）。
// lyricUrl 是缓存目录里的 qrc，文件名为「歌手 - 歌名 - 秒数 - 专辑」，专辑以汉字垫长，使每条恰为 bigEntryUnits 个码元。
func bigAssocEntry(i int) string {
	song := fmt.Sprintf("http://stream4.qqmusic.qq.com/%012dQm.wma", i)
	switch i % 20 {
	case 0:
		song = fmt.Sprintf(`E:\音乐\歌手%05d\歌手%05d - 歌曲%05d.flac`, i, i, i)
	case 1:
		song = fmt.Sprintf("http://streamundefined.qqmusic.qq.com/%d.wma", 200_000_000+i)
	}
	head := fmt.Sprintf(`<Associate songUrl="%s" lyricUrl="D:\QQMusicCache\QQMusicLyricNew\歌手%05d - 歌曲%05d - 256 - 专辑`, song, i, i)
	tail := `_qm.qrc" byUser="0" LyricOffset="0"/>` + "\r\n"
	return head + strings.Repeat("名", bigEntryUnits-utf8.RuneCountInString(head+tail)) + tail
}

// bigAssocTable 是 n 条的合成关联表，即 QQ 的 .dat 文件。
func bigAssocTable(n int) []byte {
	var b strings.Builder
	b.WriteString(xmlDecl + "<AssociateInfos>\r\n")
	for i := range n {
		b.WriteString(bigAssocEntry(i))
	}
	b.WriteString("</AssociateInfos>\r\n")
	return encryptDat(b.String())
}

// encryptDat 把 XML 编码为带 BOM 的 UTF-16LE，按 oi_symmetry_encrypt2 加密（填充与盐取固定种子的随机值），DecryptDat 可解。
func encryptDat(xml string) []byte {
	return chainEncrypt(layoutPlain(rand.New(rand.NewPCG(7, 8)), append([]byte{0xFF, 0xFE}, utf16le(xml)...)))
}

// allocDuring 返回 f 执行期间的累计堆分配字节数（TotalAlloc 之差）。
func allocDuring(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// decryptAndParse 是读取者对关联表做的全部计算：解密、解析。
func decryptAndParse(raw []byte) ([]Entry, error) {
	plain, err := DecryptDat(raw)
	if err != nil {
		return nil, err
	}
	return ParseAssociations(plain)
}

func benchmarkParseAssociations(b *testing.B, n int) {
	raw := bigAssocTable(n)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if es, err := decryptAndParse(raw); err != nil || len(es) != n {
			b.Fatalf("%d 条：解析得 %d 条、err=%v", n, len(es), err)
		}
	}
}

func BenchmarkParseAssociations5k(b *testing.B)  { benchmarkParseAssociations(b, 5_000) }
func BenchmarkParseAssociations30k(b *testing.B) { benchmarkParseAssociations(b, 30_000) }

// 3 万条：加密文件不超过 16 MiB、每条约 494 字节（夹具自证）；解密加解析得 3 万条，各种写法的条目照造的归类；累计分配不超过
// 文件大小的 8 倍，只在非 race 构建下断言（见 racebuild_race_test.go）。
// 变异自证（实测）：解析前先遍历一遍数条目（解析两遍），累计分配为文件大小的 11.76 倍而红。
func TestParseAssociations30kWithinCap(t *testing.T) {
	const n = 30_000
	raw := bigAssocTable(n)
	if per := float64(len(raw)) / n; len(raw) > 16<<20 || per < 494 || per > 495 {
		t.Fatalf("夹具：%d 条 %d 字节（每条 %.2f 字节），want 不超过 16 MiB、每条约 494 字节", n, len(raw), per)
	}
	var es []Entry
	var err error
	total := allocDuring(func() { es, err = decryptAndParse(raw) })
	if err != nil || len(es) != n {
		t.Fatalf("解析得 %d 条、err=%v，want %d 条", len(es), err, n)
	}
	for _, c := range []struct {
		i    int
		kind URLKind
		mid  string
		id   uint32
	}{
		{0, KindLocal, "", 0},
		{1, KindID, "", 200_000_001},
		{2, KindMid, "000000000002Qm", 0},
		{n - 1, KindMid, fmt.Sprintf("%012dQm", n-1), 0},
		{n - 20, KindLocal, "", 0},
	} {
		e := es[c.i]
		if e.Kind != c.kind || e.Mid != c.mid || e.ID != c.id || e.ByUser || e.LyricOffsetMs != 0 ||
			!strings.HasPrefix(e.LyricURL, `D:\QQMusicCache\QQMusicLyricNew\`) || !strings.HasSuffix(e.LyricURL, "_qm.qrc") {
			t.Errorf("第 %d 条 = %+v，want 归类 %d、mid %q、id %d、byUser 0、偏移 0、缓存目录里的 qrc", c.i, e, c.kind, c.mid, c.id)
		}
	}
	t.Logf("%d 条、%d 字节：解密加解析累计分配 %.1f MiB，为文件大小的 %.2f 倍", n, len(raw), float64(total)/(1<<20), float64(total)/float64(len(raw)))
	if total > 8*uint64(len(raw)) && !raceBuild {
		t.Fatalf("累计分配 %d 字节，超过文件大小的 8 倍（%d 字节）", total, 8*len(raw))
	}
}
