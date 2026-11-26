package krc

// 本文件只测 [offset:] 标签提取（ParseOffsetTag）与 ParsePlainKRC / ParsePlainKRCBounded 不受
// [offset:] 影响的不变量（汽水音乐共用 player/krc，解析结果的行与逐字时间戳绝不套用 offset）。

import (
	"reflect"
	"testing"
)

func TestParseOffsetTag(t *testing.T) {
	cases := []struct {
		name    string
		plain   string
		wantOff int
		wantOK  bool
	}{
		{
			name:    "正整数",
			plain:   "[ti:晴天]\n[offset:1500]\n[0,1000]<0,500,0>晴\n",
			wantOff: 1500,
			wantOK:  true,
		},
		{
			name:    "负整数",
			plain:   "[offset:-500]\n[00:01.00]歌词\n",
			wantOff: -500,
			wantOK:  true,
		},
		{
			name:    "大写键与前导加号及空白",
			plain:   "[OFFSET: +0 ]\n",
			wantOff: 0,
			wantOK:  true,
		},
		{
			name:    "键与值两端含空白",
			plain:   "[  Offset : -1200  ]\n",
			wantOff: -1200,
			wantOK:  true,
		},
		{
			name:    "首行带 UTF-8 BOM",
			plain:   "\uFEFF[offset:800]\n[00:01.00]词\n",
			wantOff: 800,
			wantOK:  true,
		},
		{
			name:    "多条合法取最后一条",
			plain:   "[offset:100]\n[ti:测试]\n[offset:-250]\n[offset:420]\n",
			wantOff: 420,
			wantOK:  true,
		},
		{
			name:    "合法行后跟非法行保留合法值",
			plain:   "[offset:300]\n[offset:abc]\n[offset:]\n[offset:1.5]\n[offset:+]\n",
			wantOff: 300,
			wantOK:  true,
		},
		{
			name:    "空值",
			plain:   "[offset:]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "小数",
			plain:   "[offset:1.5]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "非数字",
			plain:   "[offset:abc]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "仅加号",
			plain:   "[offset:+]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "仅减号",
			plain:   "[offset:-]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "符号与数字间有空白",
			plain:   "[offset:+ 100]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "无 offset 标签",
			plain:   "[ti:晴天]\n[ar:周杰伦]\n[0,1000]<0,500,0>晴\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "KRC 正文伪标签",
			plain:   "[1000,500]<0,500,0>[offset:9999]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "LRC 时间戳同行多标签",
			plain:   "[00:12.00][offset:9999]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "LRC 时间戳加伪标签与正文",
			plain:   "[00:12.00][offset:9999]歌词\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "LRC 正文末尾伪标签",
			plain:   "[00:12.00]歌词[offset:9999]\n",
			wantOff: 0,
			wantOK:  false,
		},
		{
			name:    "恰好正上界",
			plain:   "[offset:600000]\n",
			wantOff: 600_000,
			wantOK:  true,
		},
		{
			name:    "恰好负下界",
			plain:   "[offset:-600000]\n",
			wantOff: -600_000,
			wantOK:  true,
		},
		{
			name:    "超 int32 正整数钳位",
			plain:   "[offset:3000000000]\n",
			wantOff: 600_000,
			wantOK:  true,
		},
		{
			name:    "超 int32 负整数钳位",
			plain:   "[offset:-3000000000]\n",
			wantOff: -600_000,
			wantOK:  true,
		},
		{
			name:    "超 int64 正整数钳位",
			plain:   "[offset:99999999999999999999999999]\n",
			wantOff: 600_000,
			wantOK:  true,
		},
		{
			name:    "超 int64 带正号整数钳位",
			plain:   "[offset:+99999999999999999999999999]\n",
			wantOff: 600_000,
			wantOK:  true,
		},
		{
			name:    "超 int64 负整数钳位",
			plain:   "[offset:-99999999999999999999999999]\n",
			wantOff: -600_000,
			wantOK:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotOff, gotOK := ParseOffsetTag(c.plain)
			if gotOff != c.wantOff || gotOK != c.wantOK {
				t.Fatalf("ParseOffsetTag(%q) = (%d, %v), want (%d, %v)", c.plain, gotOff, gotOK, c.wantOff, c.wantOK)
			}
		})
	}
}

func TestParsePlainKRCIgnoresOffsetTag(t *testing.T) {
	const body = "[ti:晴天]\n[ar:周杰伦]\n" +
		"[0,2250]<0,160,0>晴<160,160,0>天\n" +
		"[2250,2250]<0,450,0>词<450,450,0>：<900,450,0>周\n"
	withOffset := "[offset:1500]\n" + body

	want := ParsePlainKRC(body)
	if len(want) != 2 || len(want[1].Detailed.Words) != 3 {
		t.Fatalf("夹具异常：want = %+v", want)
	}

	got := ParsePlainKRC(withOffset)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParsePlainKRC 含 [offset:1500] 改变了结果：\ngot  = %+v\nwant = %+v", got, want)
	}

	gotBounded, ok := ParsePlainKRCBounded(withOffset, 100, 100)
	if !ok {
		t.Fatalf("ParsePlainKRCBounded 返回 ok=false, want true")
	}
	wantBounded, ok := ParsePlainKRCBounded(body, 100, 100)
	if !ok {
		t.Fatalf("夹具 ParsePlainKRCBounded 返回 ok=false")
	}
	if !reflect.DeepEqual(gotBounded, wantBounded) {
		t.Fatalf("ParsePlainKRCBounded 含 [offset:1500] 改变了结果：\ngot  = %+v\nwant = %+v", gotBounded, wantBounded)
	}
}
