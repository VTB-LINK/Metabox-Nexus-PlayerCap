package qqmusic

// 本文件测第 5 版的本地文件认定不论有无编号（设计 §6.5、§6.8 的表、§7「本地认定」第 5 版「不论有无编号」组与 11-24 改名序列）：
// 有编号的歌同样按第 0、1、2、4、8 秒认定，认定即以本地关联为准、本代次不再读当前歌曲记录、不再补发编号请求；认定的文件读不到
// 时有编号退回在线、缺位为无；条目从表里消失按未认定；认定之后本代次不复查。读取者是真的，依赖全为桩（占用查询、读记录与兜底
// 请求都计数，时钟为 synctest 的假时钟）。

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/i18n"
)

// 夹具：本地文件 F（11-24 验收的那首伴奏，文件名不含匹配名），关联用户的 lrc、偏移 -1000；该曲被 QQ 匹配成的在线歌曲编号为
// widID、widMid，匹配名为 widMatch。在线条目 A 是 widMid 的 mid 键、指向 lyrOK（assocLines）、偏移 300。G 是另一首的本地文件。
const (
	widF     = `E:\Music\曲甲\1_歌手甲 - 曲甲_(Instrumental).flac`
	widStem  = "1_歌手甲 - 曲甲_(Instrumental)"
	widLyrF  = `E:\Music\曲甲\曲甲.lrc`
	widG     = `E:\Music\别的\另一首.flac`
	widLyrG  = `E:\Music\别的\另一首.lrc`
	widOther = `E:\Music\曲乙\曲乙_(Instrumental).flac` // 再次放时的上一首
	widMatch = "匹配名 (伴奏)"
	widID    = 900000001
	widMid   = "000Test1Mid9Ab"
	widLRC   = "[00:01.00]本地第一行\n[00:03.00]本地第二行\n"
)

var widLines = []string{"本地第一行|", "本地第二行|"}

var (
	widItemA = assocItem{song: midURL(widMid), lyric: lyrOK, byUser: true, offset: 300}
	widItemF = assocItem{song: widF, lyric: widLyrF, byUser: true, offset: -1000}
	widItemG = assocItem{song: widG, lyric: widLyrG, byUser: true, offset: 200}
)

// widEnv 的关联表为 items；F、G、widOther 的音频文件都存在，F、G 的歌词可用；假内存里当前歌曲是 widMatch，记录为 mid 键 widMid
// （认出即选中 A）；兜底请求按 widID 与 widMid 互查成功。
func widEnv(items ...assocItem) *readerEnv {
	e := recordEnv(items...)
	for _, p := range []string{widF, widG, widOther} {
		e.fs.put(p, []byte("audio"))
	}
	e.fs.put(widLyrF, []byte(widLRC))
	e.fs.put(widLyrG, []byte(testAssocLRC2))
	e.mem.play(widMatch, midURL(widMid), widMid)
	e.byID.set(widID, widMid)
	e.byMid.set(widMid, widID)
	return e
}

// widKeys 是有编号的三种：只有 songid、只有 songmid（这两种带已失败的详情结果，第 4 版下会兜底请求）、两者都有。
func widKeys() []struct {
	name string
	key  songKey
} {
	return []struct {
		name string
		key  songKey
	}{
		{"只有 songid", songKey{gen: 1, songID: widID, name: widMatch, detail: failedDetail}},
		{"只有 songmid", songKey{gen: 1, songMid: widMid, name: widMatch, detail: failedDetail}},
		{"两者都有", songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch}},
	}
}

// 有编号、表里有该曲的在线条目 A 与本地条目 F，F 被 QQ 打开：第 0 秒即认定 F，本地文件、偏移 -1000、F 的歌词；本代次不读
// 当前歌曲记录、不补发编号请求（第 4 版的反例：按在线认键，只有一个编号时先兜底请求，结果为在线关联 A）；只查一次占用，此后
// 不再有新结果。
// 变异自证（实测）：localDue 留着 w.online()，三例都为在线或在线关联而红；recordWanted 不看本地认定，只有 songmid、两者都有
// 两例读了记录而红；fillDue 不看本地认定，只有 songid、只有 songmid 两例兜底请求了而红；desired 先看在线一侧、本地在后，
// 三例来源都不是本地文件而红。
func TestAssocLocalOverridesOnline(t *testing.T) {
	for _, c := range widKeys() {
		t.Run(c.name, func(t *testing.T) {
			e := widEnv(widItemA, widItemF)
			e.rm.fn = openBy([]string{widF}, testQQPID)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(c.key)
				wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, widF, widLines...)
				sleepUntil(e, 30*time.Second)
				noAssocResult(t, r)
				wantTimes(t, "读记录", e.mem.scanTimes(), nil)
				wantTimes(t, "按 songid 兜底请求", e.byID.callTimes(), nil)
				wantTimes(t, "按 songmid 兜底请求", e.byMid.callTimes(), nil)
				wantTimes(t, "本地认定", e.rm.callTimes(), secs(0))
			})
		})
	}
}

// F 第 1.5 秒才被 QQ 打开（两个编号都有）：第 0、1 秒照 §6.4 读记录、给出在线关联 A（第 1 秒的复扫结论不变、不发新结果）；
// 第 2 秒的认定成功即为本地文件，此后读记录停在 2 次，占用查询也停在第 2 秒。
// 变异自证（实测）：recordWanted 不看本地认定，第 2、4、8 秒照读而红。
func TestAssocLocalOpenedLater(t *testing.T) {
	e := widEnv(widItemA, widItemF)
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch})
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 300, false, "", assocLines...)
		sleepUntil(e, 1500*time.Millisecond)
		noAssocResult(t, r)
		wantTimes(t, "读记录", e.mem.scanTimes(), secs(0, 1))
		e.rm.setFn(openBy([]string{widF}, testQQPID))
		sleepUntil(e, 2*time.Second)
		wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, widF, widLines...)
		sleepUntil(e, 30*time.Second)
		noAssocResult(t, r)
		wantTimes(t, "读记录", e.mem.scanTimes(), secs(0, 1))
		wantTimes(t, "本地认定", e.rm.callTimes(), secs(0, 1, 2))
	})
}

// widObserve 自提交起每 0.5 秒取一次结果（没有即记「-」）到第 12 秒，再取第 30 秒的，供两次运行逐个比较。
func widObserve(e *readerEnv, r *assocReader) []string {
	var out []string
	for _, at := range append(secs(0, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 4.5, 5, 6, 7, 8, 8.5, 9, 10, 12), 30*time.Second) {
		sleepUntil(e, at)
		select {
		case res := <-r.Results():
			out = append(out, fmt.Sprintf("%v gen %d.%d kind %d src %d off %d pending %v local %q lines %q digest %x",
				at, res.gen, res.reopen, res.kind, res.source, res.lyricOffsetMs, res.pending, res.localPath, lineTexts(res.lines), res.digest))
		default:
			out = append(out, fmt.Sprintf("%v -", at))
		}
	}
	return out
}

// 五次都没打开：结果与关联表里没有 F 时逐个相同（同一夹具去掉 F 再跑一遍对照），读记录与兜底请求的时刻也相同；有 F 时占用
// 查询恰 5 次（第 0、1、2、4、8 秒），没有 F 时 0 次。只有 songid、详情结果已失败：第 0 秒兜底得 songmid，随即认出 A。
func TestAssocLocalNeverOpened(t *testing.T) {
	key := songKey{gen: 1, songID: widID, name: widMatch, detail: failedDetail}
	type run struct {
		obs                []string
		scans, fills, rmAt []time.Duration
	}
	do := func(items ...assocItem) run {
		var got run
		e := widEnv(items...)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			got.obs = widObserve(e, r)
			got.scans, got.fills, got.rmAt = e.mem.scanTimes(), e.byID.callTimes(), e.rm.callTimes()
		})
		return got
	}
	with, without := do(widItemA, widItemF), do(widItemA)
	if !slices.Equal(with.obs, without.obs) {
		t.Fatalf("有 F 而没被打开时的结果与没有 F 时不同：\n有 F：%q\n没有：%q", with.obs, without.obs)
	}
	if !strings.Contains(with.obs[0], fmt.Sprintf("src %d off 300", srcAssoc)) {
		t.Fatalf("第 0 秒的结果 %q，want 在线关联 A、偏移 300", with.obs[0])
	}
	wantTimes(t, "有 F 时读记录", with.scans, without.scans)
	wantTimes(t, "有 F 时兜底请求", with.fills, without.fills)
	wantTimes(t, "有 F 时本地认定", with.rmAt, secs(0, 1, 2, 4, 8))
	wantTimes(t, "没有 F 时本地认定", without.rmAt, nil)
}

// 候选为空即不查占用：表里没有本地条目（有编号的两首、关联表变化后再求一次），或本地条目的音频文件都不存在。
func TestAssocLocalNoEntriesNoQuery(t *testing.T) {
	for _, c := range []struct {
		name  string
		items []assocItem
	}{
		{"表里没有本地条目", []assocItem{widItemA}},
		{"本地条目的文件都不存在", []assocItem{widItemA, {song: `E:\Gone\不存在.flac`, lyric: widLyrF}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := widEnv(c.items...)
			e.rm.fn = openBy([]string{widF, `E:\Gone\不存在.flac`}, testQQPID)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch})
				wantDesired(t, latestResult(t, r), 1, srcAssoc, 300, false, "", assocLines...)
				sleepUntil(e, 10*time.Second)
				e.fs.put(testTablePath, tableDat(append(slices.Clone(c.items), assocItem{song: midURL("003Else"), lyric: lyrOK})...))
				sleepUntil(e, 12*time.Second)
				r.Submit(songKey{gen: 2, songID: 7, name: "七"})
				wantDesired(t, latestResult(t, r), 2, srcOnline, 0, false, "")
				sleepUntil(e, 30*time.Second)
				wantTimes(t, "本地认定", e.rm.callTimes(), nil)
			})
		})
	}
}

// Restart Manager 报致命错误（ERROR_SEM_TIMEOUT）：本会话停做本地认定，有编号的歌照 §6.4 认键、为在线关联 A，换歌之后同样；
// 占用查询只有第 0 秒那一次，Warn 一次。
func TestAssocLocalFatalThenOnline(t *testing.T) {
	logs := captureAssocLog(t)
	e := widEnv(widItemA, widItemF)
	e.rm.fn = func([]string) ([]uint32, error) {
		return nil, i18n.Errorf("RmStartSession failed: %w", windows.ERROR_SEM_TIMEOUT)
	}
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		k := songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch}
		r.Submit(k)
		wantDesired(t, latestResult(t, r), 1, srcAssoc, 300, false, "", assocLines...)
		sleepUntil(e, 20*time.Second)
		k.gen = 2
		r.Submit(k)
		wantDesired(t, latestResult(t, r), 2, srcAssoc, 300, false, "", assocLines...)
		sleepUntil(e, 60*time.Second)
		wantTimes(t, "本地认定", e.rm.callTimes(), secs(0))
		wantTimes(t, "读记录", e.mem.scanTimes(), secs(0, 1, 2, 4, 8, 20, 21, 22, 24, 28))
	})
	if n := countLog(logs(), "!", ""); n != 1 {
		t.Fatalf("Warn %d 条，want 1\n%s", n, logs())
	}
}

// 认定了 F 而它所指的歌词读不成（设计 §6.8 的表「本地文件已认定」两行）：暂缺 → 有编号为在线、偏移 0、待重试，编号缺位为无、
// 待重试；不可用（解析不出行）→ 有编号为在线、偏移 0、不待重试，缺位为无、不待重试。都不读记录、不兜底请求（本地已认定）。
// 变异自证（实测）：本地读不成时有编号也给无，有编号的两例来源为无而红。
func TestAssocLocalFileUnreadable(t *testing.T) {
	for _, c := range []struct {
		name    string
		lyric   []byte // nil 即不存在
		key     songKey
		src     lyricSource
		pending bool
	}{
		{"暂缺、有编号", nil, songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch}, srcOnline, true},
		{"暂缺、编号缺位", nil, songKey{gen: 1, name: widStem}, srcNone, true},
		{"不可用、有编号", []byte("plain text without timestamps"), songKey{gen: 1, songID: widID, name: widMatch, detail: failedDetail}, srcOnline, false},
		{"不可用、编号缺位", []byte("plain text without timestamps"), songKey{gen: 1, name: widStem}, srcNone, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := widEnv(widItemA, widItemF)
			if c.lyric == nil {
				e.fs.remove(widLyrF)
			} else {
				e.fs.put(widLyrF, c.lyric)
			}
			e.rm.fn = openBy([]string{widF}, testQQPID)
			runAssocReader(t, e, func(t *testing.T, r *assocReader) {
				r.Submit(c.key)
				wantDesired(t, latestResult(t, r), 1, c.src, 0, c.pending, "")
				sleepUntil(e, 30*time.Second)
				wantTimes(t, "读记录", e.mem.scanTimes(), nil)
				wantTimes(t, "按 songid 兜底请求", e.byID.callTimes(), nil)
				wantTimes(t, "本地认定", e.rm.callTimes(), secs(0))
			})
		})
	}
}

// 认定 F 之后关联表改写、F 的条目消失（第 2.5 秒写、第 4 秒重读）：按未认定求结果。有编号：读记录的一轮当即开始（第 4 秒）、
// 照日程复扫，按记录选中 A；编号缺位：无。不重新认定（占用查询只有第 0 秒那一次）。
// 变异自证（实测）：本地已认定只看 localPath、不看条目在不在表里（recordWanted、fillDue 判 localPath 非空），有编号一例不读
// 记录、为在线而红。
func TestAssocLocalEntryVanished(t *testing.T) {
	t.Run("有编号", func(t *testing.T) {
		e := widEnv(widItemA, widItemF)
		e.rm.fn = openBy([]string{widF}, testQQPID)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch})
			wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, widF, widLines...)
			sleepUntil(e, 2500*time.Millisecond)
			e.fs.put(testTablePath, tableDat(widItemA))
			sleepUntil(e, 4*time.Second)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 300, false, "", assocLines...)
			sleepUntil(e, 30*time.Second)
			wantTimes(t, "读记录", e.mem.scanTimes(), secs(4, 5, 6, 8, 12))
			wantTimes(t, "本地认定", e.rm.callTimes(), secs(0))
		})
	})
	t.Run("编号缺位", func(t *testing.T) {
		e := widEnv(widItemA, widItemF)
		e.rm.fn = openBy([]string{widF}, testQQPID)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(songKey{gen: 1, name: widStem})
			wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, widF, widLines...)
			sleepUntil(e, 2500*time.Millisecond)
			e.fs.put(testTablePath, tableDat(widItemA))
			sleepUntil(e, 4*time.Second)
			wantDesired(t, latestResult(t, r), 1, srcNone, 0, false, "")
			sleepUntil(e, 30*time.Second)
			wantTimes(t, "读记录", e.mem.scanTimes(), nil)
			wantTimes(t, "本地认定", e.rm.callTimes(), secs(0))
		})
	})
}

// 关联表变化（两个编号都有）：尚未认定时日程从头（第 20.5 秒写、第 22 秒重读，随后第 22、23、24、26、30 秒各查一次）；已认定时
// 不再查，只按新表给出（F 的偏移改为 -1200）。
func TestAssocLocalTableChange(t *testing.T) {
	key := songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch}
	moved := assocItem{song: widF, lyric: widLyrF, byUser: true, offset: -1200}
	t.Run("尚未认定", func(t *testing.T) {
		e := widEnv(widItemA, widItemF)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcAssoc, 300, false, "", assocLines...)
			sleepUntil(e, 20500*time.Millisecond)
			e.fs.put(testTablePath, tableDat(widItemA, moved))
			sleepUntil(e, 40*time.Second)
			wantTimes(t, "本地认定", e.rm.callTimes(), secs(0, 1, 2, 4, 8, 22, 23, 24, 26, 30))
		})
	})
	t.Run("已认定", func(t *testing.T) {
		e := widEnv(widItemA, widItemF)
		e.rm.fn = openBy([]string{widF}, testQQPID)
		runAssocReader(t, e, func(t *testing.T, r *assocReader) {
			r.Submit(key)
			wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, widF, widLines...)
			sleepUntil(e, 2500*time.Millisecond)
			e.fs.put(testTablePath, tableDat(widItemA, moved))
			sleepUntil(e, 4*time.Second)
			wantDesired(t, latestResult(t, r), 1, srcLocal, -1200, false, widF, widLines...)
			sleepUntil(e, 40*time.Second)
			wantTimes(t, "本地认定", e.rm.callTimes(), secs(0))
			wantTimes(t, "读记录", e.mem.scanTimes(), nil)
		})
	})
}

// 认定之后本代次不复查（设计 §6.5 第 6 步）：第 0 秒认定 F，此后占用查询桩改报 F 没被打开；同代次写入详情结果、等过整个日程，
// 占用查询都停在 1 次，结果仍为本地文件。
func TestAssocLocalNoRecheck(t *testing.T) {
	e := widEnv(widItemA, widItemF)
	e.rm.fn = openBy([]string{widF}, testQQPID)
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		k := songKey{gen: 1, songID: widID, name: widMatch}
		r.Submit(k)
		wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, widF, widLines...)
		e.rm.setFn(nil)
		sleepUntil(e, 1500*time.Millisecond)
		r.Submit(withDetail(k, songDetail{state: detailGot, songMid: widMid}))
		wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, widF, widLines...)
		sleepUntil(e, 30*time.Second)
		noAssocResult(t, r)
		wantTimes(t, "本地认定", e.rm.callTimes(), secs(0))
		wantTimes(t, "读记录", e.mem.scanTimes(), nil)
	})
}

// widRenameStep 是 11-24 序列里的一个代次：写入 key，期望来源（本地文件时为 F、偏移 -1000）。
type widRenameStep struct {
	key songKey
	src lyricSource
}

// runRename 依次写入各代次（F 一直被 QQ 打开），断言每个代次的结果，最后断言读记录的次数为 0。
func runRename(t *testing.T, e *readerEnv, steps ...widRenameStep) {
	t.Helper()
	e.rm.fn = openBy([]string{widF}, testQQPID)
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		for i, s := range steps {
			sleepUntil(e, time.Duration(i)*time.Second)
			r.Submit(s.key)
			if s.src == srcLocal {
				wantDesired(t, latestResult(t, r), s.key.gen, srcLocal, -1000, false, widF, widLines...)
			} else {
				wantDesired(t, latestResult(t, r), s.key.gen, s.src, 0, false, "")
			}
		}
		sleepUntil(e, 30*time.Second)
		wantTimes(t, "读记录", e.mem.scanTimes(), nil)
	})
}

// 11-24 的首次放与再次放（设计 §2.5、§7「本地认定」的改名序列），读取者层按代次写入，excludeLocal 记为 X。表里只有本地条目
// （该在线歌曲当时没有条目），代次 3 未认定即按 §6.4 为在线。
//   - X 为空（会话判有改名证据）：代次 1（歌名为文件名、编号缺位）、代次 2（匹配名、编号缺位）、代次 3（同名、songid 落位）
//     都认定 F；代次 3 不读记录。第 4 版的反例：代次 3 有编号即不认定。
//   - X 为 F（第 4 版，以及门槛 G 为 0 时会话交出的）：代次 2 不认定、为无；代次 3 不认定、为在线。
//   - 再次放：代次 1（匹配名、编号缺位、X 为上一首 widOther）认定 F；代次 2（songmid 落位）认定 F。
//   - 本地条目 5000 条、F 的文件名不含匹配名：代次 1 按文件名筛得出 F 而认定；代次 2、3 筛不出，代次 2 为无、代次 3 为在线
//     （已知限制，设计 §8）。
func TestAssocLocalRenameSequence(t *testing.T) {
	first := songKey{gen: 1, name: widStem}
	renamed := songKey{gen: 2, name: widMatch}
	landed := songKey{gen: 3, songID: widID, name: widMatch}
	withX := func(k songKey, x string) songKey { k.excludeLocal = x; return k }
	others := assocItem{song: widOther, lyric: lyrOK}

	t.Run("首次放、X 为空", func(t *testing.T) {
		runRename(t, widEnv(widItemF, others),
			widRenameStep{first, srcLocal}, widRenameStep{renamed, srcLocal}, widRenameStep{landed, srcLocal})
	})
	t.Run("首次放、X 为 F", func(t *testing.T) {
		runRename(t, widEnv(widItemF, others),
			widRenameStep{first, srcLocal}, widRenameStep{withX(renamed, widF), srcNone}, widRenameStep{withX(landed, widF), srcOnline})
	})
	t.Run("再次放", func(t *testing.T) {
		runRename(t, widEnv(widItemF, others),
			widRenameStep{withX(songKey{gen: 1, name: widMatch}, widOther), srcLocal},
			widRenameStep{withX(songKey{gen: 2, songMid: widMid, name: widMatch}, widOther), srcLocal})
	})
	t.Run("本地条目 5000 条", func(t *testing.T) {
		items := []assocItem{widItemF}
		for i := range 4999 {
			items = append(items, assocItem{song: fmt.Sprintf(`E:\Lib\曲目%04d.flac`, i), lyric: lyrOK})
		}
		runRename(t, widEnv(items...),
			widRenameStep{first, srcLocal}, widRenameStep{renamed, srcNone}, widRenameStep{landed, srcOnline})
	})
}

// 放在线歌曲时 QQ 开着关联表里另一首的本地文件 G（设计 §8 的风险，作为已知行为钉住，要改须回到设计）：认定 G、显示 G 的歌词
// 与偏移，不读记录（记录本可认出 A）。
func TestAssocLocalHeldDuringOnlineSong(t *testing.T) {
	e := widEnv(widItemA, widItemF, widItemG)
	e.rm.fn = openBy([]string{widG}, testQQPID)
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		r.Submit(songKey{gen: 1, songID: widID, songMid: widMid, name: widMatch})
		wantDesired(t, latestResult(t, r), 1, srcLocal, 200, false, widG, assocLines2...)
		sleepUntil(e, 30*time.Second)
		wantTimes(t, "读记录", e.mem.scanTimes(), nil)
	})
}

// 认定成功打一条 Detail（设计 §6.9）：注明有无在线编号与被 QQ 打开的候选数；同一代次不重复（同代次写入详情结果、关联表变化）；
// 新代次再认定再打一条；两个被打开而按歌名消歧认定时候选数为 2；认定不成（没打开、两个被打开而消歧不成）不打这一条。
// 变异自证（实测）：认定成功不打或不分有无编号，相应各条计数不符而红。
func TestAssocLocalIdentifyDetail(t *testing.T) {
	logs := captureAssocLog(t)
	const twin = `E:\Music\别的\` + widMatch + `.flac` // 文件名与匹配名相等
	e := widEnv(widItemA, widItemF, widItemG, assocItem{song: twin, lyric: widLyrG})
	e.fs.put(twin, []byte("audio"))
	withIDs := fmt.Sprintf(i18n.T("Identified the local file QQ Music is playing: %s (the song has online IDs; %d candidates open in QQ Music)"), widF, 1)
	noIDs := fmt.Sprintf(i18n.T("Identified the local file QQ Music is playing: %s (the song has no online IDs; %d candidates open in QQ Music)"), widF, 1)
	twoOpen := fmt.Sprintf(i18n.T("Identified the local file QQ Music is playing: %s (the song has online IDs; %d candidates open in QQ Music)"), twin, 2)
	head := strings.SplitN(i18n.T("Identified the local file QQ Music is playing: %s (the song has online IDs; %d candidates open in QQ Music)"), "%s", 2)[0]
	runAssocReader(t, e, func(t *testing.T, r *assocReader) {
		e.rm.setFn(openBy([]string{widF}, testQQPID))
		k := songKey{gen: 1, songID: widID, name: widMatch}
		r.Submit(k)
		wantDesired(t, latestResult(t, r), 1, srcLocal, -1000, false, widF, widLines...)
		r.Submit(withDetail(k, songDetail{state: detailGot, songMid: widMid}))
		sleepUntil(e, 2500*time.Millisecond)
		e.fs.put(testTablePath, tableDat(widItemA, widItemF, widItemG, assocItem{song: twin, lyric: widLyrG, offset: 50}))
		sleepUntil(e, 10*time.Second)
		if n := countLog(logs(), "+", head); n != 1 {
			t.Fatalf("同一代次认定的 Detail %d 条，want 1\n%s", n, logs())
		}

		r.Submit(songKey{gen: 2, name: widStem})
		wantDesired(t, latestResult(t, r), 2, srcLocal, -1000, false, widF, widLines...)

		e.rm.setFn(openBy([]string{widF, twin}, testQQPID))
		r.Submit(songKey{gen: 3, songID: widID, songMid: widMid, name: widMatch})
		wantDesired(t, latestResult(t, r), 3, srcLocal, 50, false, twin, assocLines2...)

		e.rm.setFn(openBy([]string{widF, widG}, testQQPID))
		r.Submit(songKey{gen: 4, songID: widID, songMid: widMid, name: widMatch})
		latestResult(t, r)
		e.rm.setFn(nil)
		r.Submit(songKey{gen: 5, songID: widID, songMid: widMid, name: widMatch})
		latestResult(t, r)
		sleepUntil(e, 30*time.Second)
	})
	out := logs()
	for _, c := range []struct {
		line string
		want int
	}{{withIDs, 1}, {noIDs, 1}, {twoOpen, 1}} {
		if n := countLog(out, "+", c.line); n != c.want {
			t.Fatalf("Detail %q %d 条，want %d\n%s", c.line, n, c.want, out)
		}
	}
	if n := countLog(out, "+", head); n != 3 {
		t.Fatalf("认定的 Detail 共 %d 条，want 3（认定不成不打）\n%s", n, out)
	}
}
