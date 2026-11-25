package qqmusic

// 本文件只测会话一侧写详情结果的三个纯函数（设计 §6.4 第 2 步「交接」、§6.9）：applyDetail 在 fetchCoverURL 返回之后按所查
// 编号与状态转移决定写不写，exhaustDetail 在补取窗口用尽时把尚无记为已失败，unconfirmedDetail 决定会话一侧打不打「核验不过」
// 的 Detail。runSession 里的调用处由 assocwiring_test.go 守形状，控制流由 detailhandoff_test.go 复述。

import "testing"

var (
	gotMidB     = songDetail{state: detailGot, songMid: "MidB"}
	failedSeen  = songDetail{state: detailFailed, seen: "track id 200, mid \"MidB\", code 0"}
	failedSeen2 = songDetail{state: detailFailed, seen: "track id -, mid \"MidA\", code 0"}
)

// detailKey 是写详情结果用的当前歌曲：gen 3、reopen 2、songid 100，带详情结果 d；其余字段非零，断言原样。
func detailKey(d songDetail) songKey {
	return songKey{gen: 3, reopen: 2, songID: 100, name: "百", excludeLocal: `D:\上一首.flac`, detail: d}
}

// 状态转移：尚无 → 已得或已失败；已失败 → 已得。已得不被覆盖（已失败与另一个已得都不行），已失败到已失败不写（seen 不换），
// 所给为尚无不写。所查编号与 k 的不同（上一首的 songid、songmid 不同、v20.05 的结构体伪 songid）一律不写。写与不写，gen、reopen
// 与其余字段都原样。
// 变异自证（实测）：允许已得被已失败覆盖，「已得 + 已失败」红；不比所查编号，所查编号不同的六例红（TestDetailHandoffGenIsolation
// 同红）。
func TestApplyDetail(t *testing.T) {
	v2005 := songKey{gen: 3, reopen: 2, songMid: "MidA", name: "百"} // v20.05：结构体 songid 是伪值，assocKey 只记 songmid
	for _, c := range []struct {
		name       string
		k          songKey
		id         uint32
		mid        string
		d          songDetail
		wantDetail songDetail
		wantOK     bool
	}{
		{"尚无 + 已得", detailKey(songDetail{}), 100, "", gotMidA, gotMidA, true},
		{"尚无 + 已失败（核验不过）", detailKey(songDetail{}), 100, "", failedSeen, failedSeen, true},
		{"尚无 + 已失败（请求失败）", detailKey(songDetail{}), 100, "", failedDetail, failedDetail, true},
		{"已失败 + 已得", detailKey(failedSeen), 100, "", gotMidA, gotMidA, true},
		{"已得 + 已失败 → 保持已得", detailKey(gotMidA), 100, "", failedSeen, gotMidA, false},
		{"已得 + 另一个已得 → 保持", detailKey(gotMidA), 100, "", gotMidB, gotMidA, false},
		{"已得 + 同一个已得", detailKey(gotMidA), 100, "", gotMidA, gotMidA, false},
		{"已失败 + 已失败 → seen 不换", detailKey(failedSeen), 100, "", failedSeen2, failedSeen, false},
		{"尚无 + 尚无", detailKey(songDetail{}), 100, "", songDetail{}, songDetail{}, false},
		{"已失败 + 尚无", detailKey(failedSeen), 100, "", songDetail{}, failedSeen, false},
		{"所查为上一首的 songid", detailKey(songDetail{}), 99, "", gotMidA, songDetail{}, false},
		{"所查 songid 为 0", detailKey(songDetail{}), 0, "", gotMidA, songDetail{}, false},
		{"所查多了 songmid", detailKey(songDetail{}), 100, "MidA", gotMidA, songDetail{}, false},
		{"已失败 + 上一首的已得", detailKey(failedSeen), 99, "", gotMidA, failedSeen, false},
		{"songmid 只差大小写", v2005, 0, "mida", songDetail{state: detailGot, songID: 100}, songDetail{}, false},
		{"songmid 相同", v2005, 0, "MidA", songDetail{state: detailGot, songID: 100}, songDetail{state: detailGot, songID: 100}, true},
		{"v20.05：所查 songid 为结构体伪值", v2005, 7, "MidA", songDetail{state: detailGot, songID: 100}, songDetail{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := applyDetail(c.k, c.id, c.mid, c.d)
			want := c.k
			want.detail = c.wantDetail
			if ok != c.wantOK || got != want {
				t.Fatalf("applyDetail = %+v, %v\nwant %+v, %v", got, ok, want, c.wantOK)
			}
		})
	}
}

// 补取换了所查编号（终审第 4 版第 2 条）：所查编号与 k 的不同时以核验结果衔接身份。k 只有 songmid、按 songid 查得的已得其 mid
// 等于 k 的 songmid → 补上了 songid（已得、songID 为所查的）；k 只有 songid、按 songmid 查得的已得其 songid 等于 k 的 → 补上了
// songmid（所查另带的 songid 不论，按 songmid 查）。衔接得上的同样按状态转移写：已失败 → 已得，已得不被覆盖。衔接不上（核验不过、
// 请求失败、核验所得是另一首的、mid 只差大小写、k 两个编号都缺或都有、按另一个编号查、k 只有 songmid 而按 songmid 查）一律不写，
// 已失败照旧。写与不写，gen、reopen 与其余字段都原样。
// 变异自证（实测，仓库外的副本）：applyDetail 不衔接（所查编号不同即不写），「衔接得上」四例红；衔接不比核验所得的 mid，「核验得
// 另一首的 mid」「mid 只差大小写」「已失败而衔接不上」红；不比核验所得的 songid，「核验得另一首的 songid」与 TestApplyDetail 的
// 「所查多了 songmid」红。三者 TestDetailHandoffBackfillSwitchesIDs 各有一例同红（不比 songid 的除外：复述里没有按 songmid 查）。
func TestApplyDetailBridge(t *testing.T) {
	midOnly := songKey{gen: 2, reopen: 1, songMid: "MidA", name: "百", excludeLocal: `D:\上一首.flac`}
	idOnly := songKey{gen: 2, reopen: 1, songID: 100, name: "百", excludeLocal: `D:\上一首.flac`}
	gotID100 := songDetail{state: detailGot, songID: 100}
	for _, c := range []struct {
		name       string
		k          songKey
		id         uint32
		mid        string
		d          songDetail
		wantDetail songDetail
		wantOK     bool
	}{
		// 衔接得上
		{"只有 songmid、按 songid 查得 mid 相同", midOnly, 100, "", gotMidA, gotID100, true},
		{"只有 songmid、已失败、按 songid 查得 mid 相同", withDetail(midOnly, failedSeen), 100, "", gotMidA, gotID100, true},
		{"只有 songid、按 songmid 查得 songid 相同", idOnly, 0, "MidA", gotID100, gotMidA, true},
		{"只有 songid、所查另带 songid、按 songmid 查得 songid 相同", idOnly, 7, "MidA", gotID100, gotMidA, true},
		// 衔接得上而已得不被覆盖
		{"只有 songmid、已得、按另一个 songid 查得 mid 相同", withDetail(midOnly, gotID100), 200, "", gotMidA, gotID100, false},
		{"只有 songid、已得、按另一个 songmid 查得 songid 相同", withDetail(idOnly, gotMidA), 0, "MidB", gotID100, gotMidA, false},
		// 衔接不上
		{"核验得另一首的 mid", midOnly, 100, "", gotMidB, songDetail{}, false},
		{"mid 只差大小写", midOnly, 100, "", songDetail{state: detailGot, songMid: "mida"}, songDetail{}, false},
		{"按 songid 查而核验不过", midOnly, 100, "", failedSeen, songDetail{}, false},
		{"按 songid 查而请求失败", midOnly, 100, "", failedDetail, songDetail{}, false},
		{"所查两个编号都缺", midOnly, 0, "", gotMidA, songDetail{}, false},
		{"核验得另一首的 songid", idOnly, 0, "MidA", songDetail{state: detailGot, songID: 200}, songDetail{}, false},
		{"按 songmid 查而核验不过", idOnly, 0, "MidA", failedSeen2, songDetail{}, false},
		{"按 songmid 查而请求失败", idOnly, 0, "MidA", failedDetail, songDetail{}, false},
		{"只有 songid、按另一个 songid 查", idOnly, 99, "", gotMidA, songDetail{}, false},
		{"只有 songmid、按另一个 songmid 查", midOnly, 0, "MidB", gotID100, songDetail{}, false},
		{"只有 songmid、按 songmid 查（所查另带 songid）", midOnly, 100, "MidA", gotID100, songDetail{}, false},
		{"两个编号都缺", songKey{gen: 1, name: "百"}, 100, "", gotMidA, songDetail{}, false},
		{"两个编号都有", songKey{gen: 2, songID: 100, songMid: "MidA", name: "百"}, 100, "", gotMidA, songDetail{}, false},
		{"已失败而衔接不上", withDetail(midOnly, failedSeen), 100, "", gotMidB, failedSeen, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := applyDetail(c.k, c.id, c.mid, c.d)
			want := c.k
			want.detail = c.wantDetail
			if ok != c.wantOK || got != want {
				t.Fatalf("applyDetail = %+v, %v\nwant %+v, %v", got, ok, want, c.wantOK)
			}
		})
	}
}

// 尚无 → 已失败（不带 seen）、真，编号缺位时同样；已失败、已得 → 原样、假。gen、reopen 与其余字段原样。
// 变异自证（实测）：对已失败也返回真，「已失败」两例红；TestDetailHandoffHoldWhilePaused 的「Submit 恰一次」（保持的每拍都写入）
// 与 TestDetailHandoffExhaustedBeforeIDs 同红。
func TestExhaustDetail(t *testing.T) {
	noIDs := songKey{gen: 4, reopen: 1, name: "本地", excludeLocal: `D:\上一首.flac`}
	for _, c := range []struct {
		name   string
		k      songKey
		want   songDetail
		wantOK bool
	}{
		{"尚无", detailKey(songDetail{}), failedDetail, true},
		{"尚无、编号缺位", noIDs, failedDetail, true},
		{"已失败（核验不过）", detailKey(failedSeen), failedSeen, false},
		{"已失败（请求失败）", detailKey(failedDetail), failedDetail, false},
		{"已得", detailKey(gotMidA), gotMidA, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := exhaustDetail(c.k)
			want := c.k
			want.detail = c.want
			if ok != c.wantOK || got != want {
				t.Fatalf("exhaustDetail = %+v, %v\nwant %+v, %v", got, ok, want, c.wantOK)
			}
		})
	}
}

// 会话一侧的「核验不过」Detail（设计 §6.9）：所查编号等于 k 里的、详情为已失败且收到了响应（seen 非空）才打；所查编号的写法同
// fetchCoverURL 的分派（有 songmid 即按它查，否则按 songid）。反例：请求失败（seen 为空）、已得、尚无、所查编号与 k 的不同；
// k 里已是已得（读取者不会再请求，「将重试」不成立）。
func TestUnconfirmedDetail(t *testing.T) {
	midKey := songKey{gen: 1, songMid: "MidA", name: "百"}
	for _, c := range []struct {
		name   string
		k      songKey
		id     uint32
		mid    string
		d      songDetail
		want   string
		wantOK bool
	}{
		{"按 songid 查、核验不过", detailKey(songDetail{}), 100, "", failedSeen, "100", true},
		{"按 songmid 查、核验不过", midKey, 0, "MidA", failedSeen2, "MidA", true},
		{"key 里已是已失败", detailKey(failedSeen), 100, "", failedSeen, "100", true},
		{"key 里已是已得", detailKey(gotMidA), 100, "", failedSeen, "", false},
		{"请求失败（seen 为空）", detailKey(songDetail{}), 100, "", failedDetail, "", false},
		{"已得", detailKey(songDetail{}), 100, "", gotMidA, "", false},
		{"尚无", detailKey(songDetail{}), 100, "", songDetail{}, "", false},
		{"所查为上一首的 songid", detailKey(songDetail{}), 99, "", failedSeen, "", false},
		{"所查 songmid 不同", midKey, 0, "MidB", failedSeen, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := unconfirmedDetail(c.k, c.id, c.mid, c.d); got != c.want || ok != c.wantOK {
				t.Fatalf("unconfirmedDetail = %q, %v；want %q, %v", got, ok, c.want, c.wantOK)
			}
		})
	}
}
