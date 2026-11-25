package assoc

// 本文件只测在线歌曲的候选条目：Candidates 选出哪些条目（设计 §6.4 第 1 步），不论 byUser 与偏移。

import (
	"slices"
	"testing"
)

// 变异自证（实测）：恢复第 3 版的相关过滤（只留 byUser=1 或偏移非 0 的），候选里有 byUser=0、偏移 0 条目的五例红
// （「byUser=0、偏移 0 的条目照入」「两种键按文件顺序」等）。
func TestCandidates(t *testing.T) {
	midA := mkEntry(`http://stream4.qqmusic.qq.com/000AbCd.wma`, "midA", true, 0)
	midA0 := mkEntry(`http://stream4.qqmusic.qq.com/000AbCd.wma`, "midA0", false, 0)
	idA := mkEntry(`http://streamundefined.qqmusic.qq.com/123.wma`, "idA", true, 0)
	idA0 := mkEntry(`http://streamundefined.qqmusic.qq.com/123.wma`, "idA0", false, 0)
	id0 := mkEntry(`http://stream4.qqmusic.qq.com/0.wma`, "id0", true, 0)
	id12 := mkEntry(`http://stream12.qqmusic.qq.com/123.wma`, "id12", false, 0)
	id4 := mkEntry(`http://stream4.qqmusic.qq.com/123.wma`, "id4", false, 0)
	midO := mkEntry(`http://stream4.qqmusic.qq.com/000Other.wma`, "midO", true, 0)
	idO := mkEntry(`http://streamundefined.qqmusic.qq.com/456.wma`, "idO", true, 500)
	local := mkEntry(`E:\000AbCd.flac`, "local", true, 0)
	localID := mkEntry(`E:\123.wma`, "localID", true, 0)
	other := mkEntry(`http://example.com/000AbCd.wma`, "other", true, 0)
	otherID := mkEntry(`http://example.com/123.wma`, "otherID", true, 0)
	query := mkEntry(`http://stream4.qqmusic.qq.com/000AbCd.wma?fromtag=1`, "query", true, 0)
	queryID := mkEntry(`http://streamundefined.qqmusic.qq.com/123.wma?fromtag=1`, "queryID", true, 0)

	cases := []struct {
		name    string
		es      []Entry
		songID  uint32
		songMid string
		want    []Entry
	}{
		// mid 键：songMid 非空时比 Mid，区分大小写。
		{"songMid 命中", []Entry{midO, midA}, 0, "000AbCd", []Entry{midA}},
		{"songMid 大小写不同不命中", []Entry{midA}, 0, "000abcd", nil},
		{"songMid 是别的歌", []Entry{midA}, 123, "000Other", nil},
		// mid 键：songMid 为空时都不入，songID 相同也不入（第 3 版经换算表的那一支不复存在）。
		{"songMid 为空时 mid 键都不入", []Entry{midA, midA0, midO}, 123, "", nil},
		{"songMid 为空时只入 id 键", []Entry{midA, idA, midO}, 123, "", []Entry{idA}},
		// id 键：songID 非 0 时比 ID。
		{"id 命中", []Entry{idA}, 123, "", []Entry{idA}},
		{"id 不同", []Entry{idA}, 456, "000AbCd", nil},
		{"songID 为 0 时 id 键都不入", []Entry{idA, id0}, 0, "000AbCd", nil},
		{"songID 为 0 且 songMid 为空", []Entry{midA, idA, id0}, 0, "", nil},
		{"同一 id 的 stream12、stream4 两条都入、保序", []Entry{id12, midO, id4}, 123, "", []Entry{id12, id4}},
		{"同一 id 的 stream4、stream12 两条都入、保序", []Entry{id4, id12}, 123, "", []Entry{id4, id12}},
		// 不论 byUser 与偏移。
		{"byUser=0、偏移 0 的条目照入", []Entry{midA0, idA0}, 123, "000AbCd", []Entry{midA0, idA0}},
		{"两种键按文件顺序", []Entry{idA0, midO, midA, idO}, 123, "000AbCd", []Entry{idA0, midA}},
		{"两种键都入、同键多条都入", []Entry{midA, idA, midA0, idA0}, 123, "000AbCd", []Entry{midA, idA, midA0, idA0}},
		// 本地、其余写法与带查询串的写法不入。
		{"本地与 example.com 条目不入", []Entry{local, localID, other, otherID}, 123, "000AbCd", nil},
		{"带查询串的写法不入", []Entry{query, queryID}, 123, "000AbCd", nil},
		{"没有条目", nil, 123, "000AbCd", nil},
	}
	for _, c := range cases {
		if got := Candidates(c.es, c.songID, c.songMid); !slices.Equal(got, c.want) {
			t.Errorf("%s：Candidates = %v，应为 %v", c.name, lyricURLs(got), lyricURLs(c.want))
		}
	}
}

func lyricURLs(es []Entry) []string {
	var s []string
	for _, e := range es {
		s = append(s, e.LyricURL)
	}
	return s
}
