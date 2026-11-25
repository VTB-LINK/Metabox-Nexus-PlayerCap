package qqmusic

// 本文件只测编号补全的核验（设计 §6.4 第 2 步「核验」、§7「编号补全的核验」）：detailFromTrack 与 detailFromSingle
// 只在响应里的编号与所查编号对得上时交出另一个编号，其余一律为已失败、两个编号都为零值、seen 写明响应里的编号。
// 响应里的 id 以 JSON 原文交入，按十进制无符号 32 位整数解析，解析不出即核验不过；单曲的 mid 同样以 JSON 原文交入，
// 按 JSON 字符串解析，不是字符串即该首核验不过。

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// checkFailed 断言 d 为已失败、两个编号都为零值、seen 非空且含 want 里的每一项。
func checkFailed(t *testing.T, d songDetail, want ...string) {
	t.Helper()
	if d.state != detailFailed || d.songID != 0 || d.songMid != "" {
		t.Fatalf("详情 = %+v，应为已失败、编号为零值", d)
	}
	if d.seen == "" {
		t.Fatalf("已失败的 seen 为空，应写明响应里的编号")
	}
	for _, w := range want {
		if !strings.Contains(d.seen, w) {
			t.Fatalf("seen = %q，应含 %q", d.seen, w)
		}
	}
}

// longRaw 是超长的 id 原文：多字节字符跨过截断处，seen 须截短且仍是合法的 UTF-8。
var longRaw = `"` + strings.Repeat("中", 400) + `"`

// checkSeenBounded 断言 seen 截短过（不超过 max 字节）且是合法的 UTF-8。
func checkSeenBounded(t *testing.T, seen string, max int) {
	t.Helper()
	if len(seen) > max || !utf8.ValidString(seen) {
		t.Fatalf("seen 长 %d 字节、合法 UTF-8 %v（%q），应截短到 %d 字节以内", len(seen), utf8.ValidString(seen), seen, max)
	}
}

func TestDetailFromTrack(t *testing.T) {
	const mid = "000TestMid0001"
	for _, raw := range []string{"100", "0100"} { // 前导零按十进制照样解析（合法 JSON 里不会出现，直接调用时）
		got := detailFromTrack(100, 0, json.RawMessage(raw), mid)
		if got != (songDetail{state: detailGot, songMid: mid}) {
			t.Fatalf("detailFromTrack(100, 0, %s, %q) = %+v，应为已得、songMid 为 %q", raw, mid, got, mid)
		}
	}
	if got := detailFromTrack(4294967295, 0, json.RawMessage("4294967295"), mid); got != (songDetail{state: detailGot, songMid: mid}) {
		t.Fatalf("uint32 上限的 songid：%+v，应为已得", got)
	}

	// 反例：编号是另一首的、响应缺 id、所查为 0（含响应的 id 同为 0）、code 非 0、mid 不像 songmid，
	// 以及 id 的各种异常写法（oddIDs）。
	cases := []struct {
		name     string
		queried  uint32
		code     int
		trackID  string // JSON 原文
		trackMid string
		want     []string // seen 应含的响应里的编号
	}{
		{"id 是另一首的", 100, 0, "200", mid, []string{"200", mid}},
		{"所查为 0", 0, 0, "100", mid, []string{"100", mid}},
		{"所查与响应的 id 都为 0", 0, 0, "0", mid, []string{mid}},
		{"code 非 0", 100, -1, "100", mid, []string{"100", mid, "-1"}},
		{"code 非 0 且无 id", 100, 2000, "", "", []string{"2000"}},
		{"mid 为空", 100, 0, "100", "", []string{"100"}},
		{"mid 含 -", 100, 0, "100", "000Test-Mid01", []string{"100", "000Test-Mid01"}},
		{"mid 全是数字", 100, 0, "100", "268359544", []string{"100", "268359544"}},
		{"mid 含非 ASCII", 100, 0, "100", "000TestMidé01", []string{"100", "000TestMid"}},
		{"mid 带空白", 100, 0, "100", mid + " ", []string{"100", mid}},
		{"id 带正号", 268359544, 0, "+268359544", mid, []string{"+268359544"}},
		{"id 带空白", 268359544, 0, " 268359544", mid, []string{"268359544"}},
	}
	for name, raw := range oddIDs {
		want := []string{mid}
		if raw != "" {
			want = append(want, raw)
		}
		cases = append(cases, struct {
			name     string
			queried  uint32
			code     int
			trackID  string
			trackMid string
			want     []string
		}{"id " + name, 268359544, 0, raw, mid, want})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFailed(t, detailFromTrack(c.queried, c.code, json.RawMessage(c.trackID), c.trackMid), c.want...)
		})
	}

	// id 与 mid 的原文很长（接口异常）时 seen 截短，不把整段写进日志。
	t.Run("id 原文很长", func(t *testing.T) {
		d := detailFromTrack(268359544, 0, json.RawMessage(longRaw), mid)
		checkFailed(t, d, mid)
		checkSeenBounded(t, d.seen, 120)
	})
	t.Run("mid 很长", func(t *testing.T) {
		d := detailFromTrack(268359544, 0, json.RawMessage("1"), strings.Repeat("中", 400))
		checkFailed(t, d)
		checkSeenBounded(t, d.seen, 120)
	})
}

func TestDetailFromSingle(t *testing.T) {
	const mid = "001TestMid0001"
	// song 是 data 里的一首，id 与 mid 都是 JSON 原文。
	song := func(id, m string) singleSong { return singleSong{ID: json.RawMessage(id), Mid: json.RawMessage(m)} }
	q := strconv.Quote
	// data 的第二首才是所查的：取它的 id，第一首不影响。
	got := detailFromSingle(mid, 0, []singleSong{song("268000002", q("001OtherMid001")), song("268000001", q(mid))})
	if got != (songDetail{state: detailGot, songID: 268000001}) {
		t.Fatalf("detailFromSingle = %+v，应为已得、songID 为 268000001", got)
	}
	// 同一 mid 的第一首 id 写法异常、第二首正常：取第二首。
	got = detailFromSingle(mid, 0, []singleSong{song(`"268000002"`, q(mid)), song("268000001", q(mid))})
	if got != (songDetail{state: detailGot, songID: 268000001}) {
		t.Fatalf("detailFromSingle（第一首 id 为字符串）= %+v，应取第二首 268000001", got)
	}
	// 第一首 mid 写法异常（数字）、第二首正常：取第二首。
	got = detailFromSingle(mid, 0, []singleSong{song("268000002", "123"), song("268000001", q(mid))})
	if got != (songDetail{state: detailGot, songID: 268000001}) {
		t.Fatalf("detailFromSingle（第一首 mid 为数字）= %+v，应取第二首 268000001", got)
	}
	// mid 按 JSON 字符串解码后比较：带转义而解码后与所查相同的照认。
	got = detailFromSingle(mid, 0, []singleSong{song("268000001", `"001TestMid0001"`)})
	if got != (songDetail{state: detailGot, songID: 268000001}) {
		t.Fatalf("detailFromSingle（mid 带 \\u 转义）= %+v，应为已得", got)
	}

	one := func(id, m string) []singleSong { return []singleSong{song(id, m)} }
	cases := []struct {
		name    string
		queried string
		code    int
		data    []singleSong
		want    []string
	}{
		{"mid 只差大小写", mid, 0, one("268000001", q("001testmid0001")), []string{"268000001", "001testmid0001"}},
		{"id 为 0", mid, 0, one("0", q(mid)), []string{mid}},
		{"data 为空", mid, 0, nil, []string{"0"}},
		{"code 非 0", mid, -1, one("268000001", q(mid)), []string{"268000001", mid, "-1"}},
		{"所查为空串（data 里有 mid 为空串、id 非 0 的一首）", "", 0, one("268000001", `""`), []string{"268000001"}},
		{"所查为空串（data 里有 mid 为 null、id 非 0 的一首）", "", 0, one("268000001", `null`), []string{"268000001", "null"}},
		{"只有另一首", mid, 0, one("268000002", q("001OtherMid001")), []string{"268000002", "001OtherMid001"}},
		{"id 带正号", mid, 0, one("+268000001", q(mid)), []string{"+268000001"}},
		{"mid 带首尾空白（字符串内）", mid, 0, one("268000001", q(" "+mid)), []string{mid}},
		{"mid 原文不带引号", mid, 0, one("268000001", mid), []string{mid}}, // 直接调用时才会出现：不是 JSON 字符串
	}
	for name, raw := range oddIDs {
		want := []string{mid}
		if raw != "" {
			want = append(want, raw)
		}
		cases = append(cases, struct {
			name    string
			queried string
			code    int
			data    []singleSong
			want    []string
		}{"id " + name, mid, 0, one(raw, q(mid)), want})
	}
	// mid 的各种异常写法（oddMids）：该首核验不过，seen 写明原文（缺省写作 -）。
	for name, raw := range oddMids {
		want := []string{"268000001", "-"}
		if raw != "" {
			want = []string{"268000001", raw}
		}
		cases = append(cases, struct {
			name    string
			queried string
			code    int
			data    []singleSong
			want    []string
		}{"mid " + name, mid, 0, one("268000001", raw), want})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFailed(t, detailFromSingle(c.queried, c.code, c.data), c.want...)
		})
	}

	// data 很长（接口异常）时 seen 只列前几首与总数，不把整份响应写进日志。
	t.Run("data 很长", func(t *testing.T) {
		data := make([]singleSong, 1000)
		for i := range data {
			data[i] = song(strconv.Itoa(268000000+i), q("001OtherMid"+strconv.Itoa(i)))
		}
		d := detailFromSingle(mid, 0, data)
		checkFailed(t, d, "1000", "268000000")
		if len(d.seen) > 300 || strings.Contains(d.seen, "268000999") {
			t.Fatalf("seen 长 %d 字节（%q），应只列前几首", len(d.seen), d.seen)
		}
	})
	// 每首的 id 与 mid 原文很长时同样截短。
	t.Run("id 与 mid 原文很长", func(t *testing.T) {
		data := []singleSong{song(longRaw, q(strings.Repeat("中", 400))), song(longRaw, q(mid))}
		d := detailFromSingle(mid, 0, data)
		checkFailed(t, d, mid)
		checkSeenBounded(t, d.seen, 300)
	})
}
