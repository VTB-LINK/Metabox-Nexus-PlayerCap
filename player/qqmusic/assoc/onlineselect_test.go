package assoc

// 本文件只测 SelectOnline：在线歌曲有候选即要认键，按当前歌曲记录的网址逐字节选关联条目（设计 §6.4 第 3、4 步）。

import "testing"

// mkEntry 照 ParseAssociations 的做法由 songUrl 造一条条目；lyricURL 用来区分选中的是哪一条。
func mkEntry(songURL, lyricURL string, byUser bool, lyricOffsetMs int) Entry {
	e := Entry{SongURL: songURL, LyricURL: lyricURL, ByUser: byUser, LyricOffsetMs: lyricOffsetMs}
	e.Kind, e.Mid, e.ID = ClassifySongURL(songURL)
	return e
}

// 变异自证（实测）：恢复「所选条目须在候选里」（第 3 版的不变式），「只有 songMid、记录为 id 键网址」与「记录网址选中
// 别的歌的条目」两例红；恢复相关过滤，候选只有 byUser=0、偏移 0 条目的七例红。
func TestSelectOnline(t *testing.T) {
	const (
		uMid4    = `http://stream4.qqmusic.qq.com/000AbCd.wma`
		uMid12   = `http://stream12.qqmusic.qq.com/000AbCd.wma`
		uIDu     = `http://streamundefined.qqmusic.qq.com/123.wma`
		uID12    = `http://stream12.qqmusic.qq.com/123.wma`
		uID4     = `http://stream4.qqmusic.qq.com/123.wma`
		uOther   = `http://stream4.qqmusic.qq.com/000Other.wma`
		uOtherID = `http://streamundefined.qqmusic.qq.com/456.wma`
		uQuery   = `http://stream4.qqmusic.qq.com/000AbCd.wma?fromtag=1`
	)
	midA := mkEntry(uMid4, "midA", true, 0)
	midA0 := mkEntry(uMid4, "midA0", false, 0)
	idA := mkEntry(uIDu, "idA", true, 0)
	idA0 := mkEntry(uIDu, "idA0", false, 0)
	midO := mkEntry(uOther, "midO", true, 0)
	idO := mkEntry(uOtherID, "idO", false, 300)
	query := mkEntry(uQuery, "query", true, 0)
	local := mkEntry(`E:\000AbCd.flac`, "local", true, 0)
	// mid 键与 id 键都在、lyricUrl 只差大小写、偏移相同（第 4 版评估而不采用的免扫规则的情形）。
	caseMid := mkEntry(uMid4, `C:\Lyrics\A.qrc`, false, 0)
	caseID := mkEntry(uIDu, `c:\lyrics\a.qrc`, false, 0)
	empty := mkEntry("", "empty", false, 0) // songUrl 属性缺失
	// 同一 songid 三条：mid 键 → A、stream12/<id> → A、stream4/<id> → B。
	drMid := mkEntry(uMid4, "A", true, 0)
	dr12 := mkEntry(uID12, "A", true, 0)
	dr4 := mkEntry(uID4, "B", true, 0)

	cases := []struct {
		name     string
		es       []Entry
		songID   uint32
		songMid  string
		rec      string
		want     Entry // 不选中时为零值
		ok, need bool
	}{
		// 没有候选：need 为假，ok 为假，不论记录。
		{"没有条目", nil, 123, "000AbCd", uMid4, Entry{}, false, false},
		{"没有条目、记录为空", nil, 123, "000AbCd", "", Entry{}, false, false},
		{"条目都属于别的歌", []Entry{midO, idO}, 123, "000AbCd", uOther, Entry{}, false, false},
		{"只有带查询串、归不进的写法", []Entry{query}, 123, "000AbCd", uQuery, Entry{}, false, false},
		{"本地条目不算候选", []Entry{local}, 123, "000AbCd", `E:\000AbCd.flac`, Entry{}, false, false},
		{"songMid 为空时 mid 键条目不算候选", []Entry{midA}, 123, "", uMid4, Entry{}, false, false},
		{"songID 为 0 时 id 键条目不算候选", []Entry{idA}, 0, "000AbCd", uIDu, Entry{}, false, false},

		// 有候选而记录为空：ok 为假，各条结果相同也如此。
		{"只有一条 byUser=0、偏移 0 的 mid 键条目、记录为空", []Entry{midA0}, 123, "000AbCd", "", Entry{}, false, true},
		{"只有 id 键、记录为空", []Entry{idA0}, 123, "000AbCd", "", Entry{}, false, true},
		{"两种键都在、lyricUrl 只差大小写、偏移相同、记录为空", []Entry{caseMid, caseID}, 123, "000AbCd", "", Entry{}, false, true},
		{"同一 songid 三条、记录为空", []Entry{drMid, dr12, dr4}, 123, "000AbCd", "", Entry{}, false, true},
		{"记录为空而表里有 songUrl 为空的条目", []Entry{midA0, empty}, 123, "000AbCd", "", Entry{}, false, true},

		// 记录认出：只有 mid 键一条。
		{"只有 mid 键一条、记录为 mid 键网址", []Entry{midA}, 123, "000AbCd", uMid4, midA, true, true},
		{"只有 mid 键一条（byUser=0、偏移 0）、记录为 mid 键网址", []Entry{midA0}, 123, "000AbCd", uMid4, midA0, true, true},
		{"只有 mid 键一条、记录为 id 键网址", []Entry{midA}, 123, "000AbCd", uIDu, Entry{}, false, true},
		// 只有 id 键一条。
		{"只有 id 键一条、记录为 id 键网址", []Entry{idA0}, 123, "000AbCd", uIDu, idA0, true, true},
		{"只有 id 键一条、记录为 mid 键网址", []Entry{idA}, 123, "000AbCd", uMid4, Entry{}, false, true},
		// 两种键都在：各选各的，不按 mid 键优先。
		{"两种键都在、记录为 mid 键网址", []Entry{midA, idA0}, 123, "000AbCd", uMid4, midA, true, true},
		{"两种键都在、记录为 id 键网址", []Entry{midA, idA0}, 123, "000AbCd", uIDu, idA0, true, true},
		{"两种键都在、id 在前、记录为 mid 键网址", []Entry{idA, midA0}, 123, "000AbCd", uMid4, midA0, true, true},
		{"两种键都在、lyricUrl 只差大小写、记录为 id 键网址", []Entry{caseMid, caseID}, 123, "000AbCd", uIDu, caseID, true, true},
		// 同一 songid 三条。
		{"同一 songid 三条、记录为 stream4/<id>", []Entry{drMid, dr12, dr4}, 123, "000AbCd", uID4, dr4, true, true},
		{"同一 songid 三条、记录为 stream12/<id>", []Entry{drMid, dr12, dr4}, 123, "000AbCd", uID12, dr12, true, true},
		{"同一 songid 三条、记录为 mid 键网址", []Entry{drMid, dr12, dr4}, 123, "000AbCd", uMid4, drMid, true, true},

		// 不再核对所选条目在不在候选里，由 ConcludeRecord 的归属核对涵盖（设计 §6.4 第 4 步）。
		{"只有 songMid、记录为 id 键网址", []Entry{midA, idA}, 0, "000AbCd", uIDu, idA, true, true},
		{"记录网址选中别的歌的条目", []Entry{idA, midO}, 123, "", uOther, midO, true, true},

		// 逐字节比较，不归一。
		{"记录为 stream12/<mid>、表里为 stream4/<mid>", []Entry{midA}, 123, "000AbCd", uMid12, Entry{}, false, true},
		{"记录主机名大写", []Entry{midA}, 123, "000AbCd", `http://STREAM4.qqmusic.qq.com/000AbCd.wma`, Entry{}, false, true},
		{"记录为 https、表里为 http", []Entry{midA}, 123, "000AbCd", `https://stream4.qqmusic.qq.com/000AbCd.wma`, Entry{}, false, true},
		{"记录 mid 大小写不同", []Entry{midA}, 123, "000AbCd", `http://stream4.qqmusic.qq.com/000abcd.wma`, Entry{}, false, true},
		{"记录为 stream4/<id>、表里为 streamundefined/<id>", []Entry{midA, idA0}, 123, "000AbCd", uID4, Entry{}, false, true},
		{"记录网址表里没有", []Entry{midA, idA}, 123, "000AbCd", uOtherID, Entry{}, false, true},
	}
	for _, c := range cases {
		got, ok, need := SelectOnline(c.es, c.songID, c.songMid, c.rec)
		if got != c.want || ok != c.ok || need != c.need {
			t.Errorf("%s：得到 (%q → %q, ok=%v, need=%v)，应为 (%q → %q, ok=%v, need=%v)",
				c.name, got.SongURL, got.LyricURL, ok, need, c.want.SongURL, c.want.LyricURL, c.ok, c.need)
		}
	}
}
