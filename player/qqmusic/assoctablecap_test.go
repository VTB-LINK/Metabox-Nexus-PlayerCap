package qqmusic

// 本文件只测关联表超出 16 MiB 时读取者怎么办（设计 §6.3 第 4、5 步，§6.10「关联表」）：超出上限与结构错误同样处理，第一次按
// 暂时性失败退避，第二次读之前的（大小，修改时间）相同、错误相同即判不支持、上报并停用，不 panic。反例：恰为 16 MiB 的整表
// 照常解析。合成的整表每条约 494 字节（本机均值），16 MiB 约 3.4 万条。

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"Metabox-Nexus-PlayerCap/i18n"
)

// capTableDat 是加密后恰为 size 字节（8 的倍数）的合成关联表，返回它与条目数。每条为 mid 键、247 个 UTF-16 码元（加密后
// 494 字节）；条目之后、根元素闭合之前垫空格补足。testDat 的明文为 BOM 加 XML，加密后为 2·(1 + 码元数) + 10 再补到 8 的
// 倍数，故 XML 取 (size − 12)/2 个码元时恰为 size、不补。
func capTableDat(t *testing.T, size int) ([]byte, int) {
	t.Helper()
	const perEntry = 247
	head, tail := testXMLDecl+"<AssociateInfos>", "</AssociateInfos>"
	units := (size - 12) / 2
	n := (units - len(head) - len(tail)) / perEntry
	var b strings.Builder
	b.WriteString(head)
	for i := range n {
		h := fmt.Sprintf(`<Associate songUrl="%s" lyricUrl="D:\QQMusicCache\QQMusicLyricNew\歌手%05d - 歌曲%05d - 256 - 专辑`, midURL(fmt.Sprintf("%012dQm", i)), i, i)
		tl := `_qm.qrc" byUser="0" LyricOffset="0"/>` + "\r\n"
		b.WriteString(h + strings.Repeat("名", perEntry-utf8.RuneCountInString(h+tl)) + tl)
	}
	b.WriteString(strings.Repeat(" ", units-utf8.RuneCountInString(b.String())-len(tail)) + tail)
	dat := testDat(b.String())
	if len(dat) != size {
		t.Fatalf("夹具：加密后 %d 字节，want %d", len(dat), size)
	}
	return dat, n
}

// 关联表超出 16 MiB：16 MiB 加 1 字节（读到上限加一字节即停，内容无关），与恰比上限多一块、本身可解的整表各一例。第 0 秒读到即为
// 暂时性失败、结果为未知、读取者照常；第 1 秒重读，（大小，修改时间）与错误都相同，判不支持：上报一次 qqmusic.assoc_unsupported
// （错误为超出上限）、发停用结果、读取者退出，此后不再读；不打解析的 Detail。反例：恰为 16 MiB 的整表照常解析，打一条解析的
// Detail，结果为期望状态，读取者不停用、不再读。
// 变异自证（实测）：读取上限改为 32 MiB，「加一块的整表」一例第一次即解析成功而红，「加 1 字节」一例改为解密报长度不是 8 的倍数、
// 上报的错误不是超出上限而红；超出上限不按结构错误（只当暂时性、一直重试），两例都不停用而红。
func TestAssocTableOverCapUnsupported(t *testing.T) {
	const capBytes = 16 << 20 // 设计 §6.3 的上限，写成字面量：夹具不随 maxDatBytes 变
	atCap, entries := capTableDat(t, capBytes)
	oneBlockOver, _ := capTableDat(t, capBytes+8)
	for _, c := range []struct {
		name string
		data []byte
		stop bool
	}{
		{"16 MiB 加 1 字节", append(bytes.Clone(atCap), 0), true},
		{"16 MiB 加一块的整表", oneBlockOver, true},
		{"恰为 16 MiB 的整表", atCap, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			reports := stubAssocReports(t)
			logs := captureAssocLog(t)
			e := newReaderEnv()
			e.fs.put(testTablePath, c.data)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 3, songID: 100, name: "百"})
				got := latestResult(t, r)
				if !c.stop {
					wantDesired(t, got, 3, srcOnline, 0, false, "")
					sleepUntil(e, time.Minute)
					if n := assocReaders.Load(); n != 1 {
						t.Fatalf("恰为上限：存活的读取者 %d 个，want 1", n)
					}
					wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0))
					return
				}
				if got.kind != resUnknown || got.gen != 3 {
					t.Fatalf("第一次读到超出上限的关联表：结果 %+v，want gen 3 的未知", got)
				}
				sleepUntil(e, time.Second-time.Millisecond)
				if n := assocReaders.Load(); n != 1 {
					t.Fatal("第 1 秒之前读取者已退出")
				}
				sleepUntil(e, time.Second)
				if got := latestResult(t, r); got.kind != resStopped || got.gen != 3 {
					t.Fatalf("第二次读到同一份：结果 %+v，want gen 3 的停用", got)
				}
				if n := assocReaders.Load(); n != 0 {
					t.Fatalf("停用之后存活的读取者 %d 个，want 0", n)
				}
				sleepUntil(e, time.Minute)
				wantTimes(t, "读关联表", e.fs.readsOf(testTablePath), secs(0, 1))
			})
			parsed := fmt.Sprintf(i18n.T(parsedTableLog), entries, len(c.data), time.Duration(0))
			if !c.stop {
				if got := reports(); len(got) != 0 {
					t.Fatalf("恰为上限：上报 %+v，want 无", got)
				}
				if n := countLog(logs(), "+", parsed); n != 1 {
					t.Fatalf("恰为上限：解析的 Detail %d 条，want 1\n%s", n, logs())
				}
				return
			}
			got := reports()
			if len(got) != 1 || got[0].key != "qqmusic.assoc_unsupported" || got[0].tags["qqmusic.version"] != testQQVersion ||
				got[0].extra["error"] != errLocalTooLarge.Error() {
				t.Fatalf("上报 %+v，want 一次 qqmusic.assoc_unsupported，带 QQ 版本，错误为 %q", got, errLocalTooLarge.Error())
			}
			if n := countLog(logs(), "+", strings.SplitN(i18n.T(parsedTableLog), "%", 2)[0]); n != 0 {
				t.Fatalf("超出上限：解析的 Detail %d 条，want 0\n%s", n, logs())
			}
		})
	}
	if entries < 33_900 || entries > 34_000 {
		t.Fatalf("夹具：16 MiB 合 %d 条，want 约 3.4 万条", entries)
	}
}
