package assoc

import "slices"

// Candidates 返回在线歌曲的候选条目（设计 §6.4 第 1 步），按文件顺序，不论 byUser 与偏移。mid 键条目：songMid 非空时 Mid 须
// 等于它（区分大小写）；id 键条目：songID 非 0 时 ID 须等于它。songID 为 0、songMid 为空即缺失，缺的那种键的条目不入候选。
// 本地与其余写法的条目不入候选。
func Candidates(es []Entry, songID uint32, songMid string) []Entry {
	var cs []Entry
	for _, e := range es {
		switch {
		case e.Kind == KindMid && songMid != "" && e.Mid == songMid,
			e.Kind == KindID && songID != 0 && e.ID == songID:
			cs = append(cs, e)
		}
	}
	return cs
}

// SelectOnline 按当前歌曲记录的网址 recordURL 为在线歌曲选关联条目（设计 §6.4 第 3、4 步）。need 为有无候选，为假时不必读
// 记录，ok 为假（无条目）。recordURL 为空即记录尚未认出，ok 为假。否则取 songUrl 与 recordURL 逐字节相同的第一条，不按
// ClassifySongURL 归一；表里没有即 ok 为假。不核对所选条目在不在候选里：recordURL 来自 ConcludeRecord 认出的网址，已经过
// 归属核对（设计 §6.4 第 4 步）。ok 为假时按无条目处理。
func SelectOnline(es []Entry, songID uint32, songMid string, recordURL string) (e Entry, ok bool, need bool) {
	if len(Candidates(es, songID, songMid)) == 0 {
		return Entry{}, false, false
	}
	if recordURL == "" {
		return Entry{}, false, true
	}
	i := slices.IndexFunc(es, func(e Entry) bool { return e.SongURL == recordURL })
	if i < 0 {
		return Entry{}, false, true
	}
	return es[i], true, true
}
