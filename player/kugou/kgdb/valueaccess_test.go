package kgdb

import (
	"math"
	"testing"
)

// 本文件只测 Value 的取值方法 String 与 Int64 对各存储类型的结果。

func TestValueString(t *testing.T) {
	cases := []struct {
		v    Value
		want string
	}{
		{Value{}, ""},
		{Value{Kind: Null, Int: 5, Bytes: []byte("ignored")}, ""},
		{Value{Kind: Integer, Int: -42}, "-42"},
		{Value{Kind: Integer, Int: math.MinInt64}, "-9223372036854775808"},
		{Value{Kind: Integer, Bytes: []byte("ignored")}, "0"},
		{Value{Kind: Float, Float: 1.5}, "1.5"},
		{Value{Kind: Text, Bytes: []byte("歌词")}, "歌词"},
		{Value{Kind: Text}, ""},
		{Value{Kind: Blob, Bytes: []byte{0x00, 0xFF}}, "\x00\xff"},
		{Value{Kind: Kind(99), Bytes: []byte("x")}, ""},
	}
	for _, c := range cases {
		if got := c.v.String(); got != c.want {
			t.Errorf("%+v.String() = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestValueInt64(t *testing.T) {
	cases := []struct {
		v    Value
		want int64
	}{
		{Value{Kind: Integer, Int: math.MaxInt64}, math.MaxInt64},
		{Value{Kind: Integer, Int: -1}, -1},
		{Value{}, 0},
		{Value{Kind: Null, Int: 7}, 0},
		{Value{Kind: Float, Float: 3, Int: 3}, 0},
		{Value{Kind: Text, Bytes: []byte("123"), Int: 123}, 0},
		{Value{Kind: Blob, Bytes: []byte{1}}, 0},
	}
	for _, c := range cases {
		if got := c.v.Int64(); got != c.want {
			t.Errorf("%+v.Int64() = %d, want %d", c.v, got, c.want)
		}
	}
}
