package assoc

// 本文件只测大曲库下歌词索引的解密与解析（设计 §6.10「歌词索引」、§7「大曲库」）：5k 条的耗时与累计分配（基准），2 万条在
// 16 MiB 之内、解析成功。合成数据在测试内生成、以测试内的 TEA 加密，每条约 800 字节，即本机均值（§2.3：83 条 66,208 字节）。
// 索引按（大小，修改时间）缓存、稳定的结构错误与 IO 错误怎么办见 player/qqmusic/associndexcache_test.go。

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// bigIndexUnits 是合成歌词索引每条的 UTF-16 码元数（约数，见 bigIndexEntry）。
const bigIndexUnits = 400

// bigIndexPath 是合成歌词索引第 i 条的原文路径（不含 _qm.qrc），专辑以 pad 个汉字垫长。
func bigIndexPath(i, pad int) string {
	return fmt.Sprintf(`D:\QQMusicCache\QQMusicLyricNew\歌手%05d - 歌曲%05d - 256 - 专辑`, i, i) + strings.Repeat("名", pad)
}

// bigIndexEntry 是合成歌词索引的第 i 条：偶数条有翻译、每 3 条有音译（本机 83 条里翻译 39 条、音译 25 条），三个文件同名、
// 后缀不同。专辑垫长使每条约 bigIndexUnits 个码元（各路径垫同样多，余数不足路径数个码元）。
func bigIndexEntry(i int) string {
	hasTS, hasRoma := i%2 == 0, i%3 == 0
	entry := func(name string) string {
		ts, roma := "", ""
		if hasTS {
			ts = name + "_qmts.qrc"
		}
		if hasRoma {
			roma = name + "_qmRoma.qrc"
		}
		return fmt.Sprintf(`<Lyric url="%s_qm.qrc" tsurl="%s" romaurl="%s" lyrictimetag="1763800000" transtimetag="1763800000" romatimetag="1763800000" id="%d" timetag="1763800000"/>`+"\r\n",
			name, ts, roma, i+1)
	}
	paths := 1
	for _, has := range []bool{hasTS, hasRoma} {
		if has {
			paths++
		}
	}
	pad := (bigIndexUnits - utf8.RuneCountInString(entry(bigIndexPath(i, 0)))) / paths
	return entry(bigIndexPath(i, pad))
}

// bigLyricIndex 是 n 条的合成歌词索引，即 QQ 的 .dat 文件。
func bigLyricIndex(n int) []byte {
	var b strings.Builder
	b.WriteString(xmlDecl + "<LyricInfos>\r\n")
	for i := range n {
		b.WriteString(bigIndexEntry(i))
	}
	b.WriteString("</LyricInfos>\r\n")
	return encryptDat(b.String())
}

func decryptAndParseIndex(raw []byte) (map[string]LyricFiles, error) {
	plain, err := DecryptDat(raw)
	if err != nil {
		return nil, err
	}
	return ParseLyricIndex(plain)
}

func BenchmarkParseLyricIndex5k(b *testing.B) {
	const n = 5_000
	raw := bigLyricIndex(n)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if m, err := decryptAndParseIndex(raw); err != nil || len(m) != n {
			b.Fatalf("%d 条：解析得 %d 条、err=%v", n, len(m), err)
		}
	}
}

// 2 万条：加密文件不超过 16 MiB、每条约 800 字节（夹具自证）；解析得 2 万条。按原文路径（不区分大小写）取得翻译、音译：
// 两者都有、只有音译、都没有各一例；不在索引里的路径、把翻译当原文去查都取不到（反例）。
// 变异自证（实测）：键不转小写，前四例都取不到而红。
func TestParseLyricIndex20kWithinCap(t *testing.T) {
	const n = 20_000
	raw := bigLyricIndex(n)
	if per := float64(len(raw)) / n; len(raw) > 16<<20 || per < 795 || per > 801 {
		t.Fatalf("夹具：%d 条 %d 字节（每条 %.2f 字节），want 不超过 16 MiB、每条约 800 字节", n, len(raw), per)
	}
	m, err := decryptAndParseIndex(raw)
	if err != nil || len(m) != n {
		t.Fatalf("解析得 %d 条、err=%v，want %d 条", len(m), err, n)
	}
	path := func(i int) string {
		e := bigIndexEntry(i)
		_, after, _ := strings.Cut(e, `url="`)
		p, _, _ := strings.Cut(after, `"`)
		return p
	}
	main0, main3, main1 := path(0), path(3), path(1)
	trans0 := strings.TrimSuffix(main0, "_qm.qrc") + "_qmts.qrc"
	for _, c := range []struct {
		name string
		key  string
		want LyricFiles
		ok   bool
	}{
		{"翻译与音译都有", strings.ToLower(main0), LyricFiles{Trans: trans0, Roma: strings.TrimSuffix(main0, "_qm.qrc") + "_qmRoma.qrc"}, true},
		{"只有音译", strings.ToLower(main3), LyricFiles{Roma: strings.TrimSuffix(main3, "_qm.qrc") + "_qmRoma.qrc"}, true},
		{"都没有", strings.ToLower(main1), LyricFiles{}, true},
		{"最后一条", strings.ToLower(path(n - 1)), LyricFiles{}, true},
		{"不在索引里", strings.ToLower(bigIndexPath(n, 0) + "_qm.qrc"), LyricFiles{}, false},
		{"翻译不是原文", strings.ToLower(trans0), LyricFiles{}, false},
	} {
		if got, ok := m[c.key]; ok != c.ok || got != c.want {
			t.Errorf("%s：得到 (%+v, %v)，want (%+v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}
