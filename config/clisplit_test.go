package config

import (
	"reflect"
	"testing"
)

// splitCommaList 是 -prior-player 这类「逗号分隔列表」CLI flag 的解析。三条语义必须锁定：
// 去两端空白、跳过空项、空串得 nil —— 尤其空串：`-prior-player ""` 是「显式清空优先播放器」
// 的唯一写法（flag.Visit 只在显式传入时赋值），解析成 nil 才能让 IsPriorPlayer 全返回 false。
func TestSplitCommaList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},                      // 显式清空
		{"   ", nil},                   // 全空白等同清空
		{",", nil},                     // 全空项
		{" , , ", nil},                 // 空白 + 空项
		{"wesing", []string{"wesing"}}, // 单项
		{"wesing,kugou", []string{"wesing", "kugou"}},
		{" wesing , kugou ", []string{"wesing", "kugou"}}, // 去两端空白
		{"wesing,,kugou", []string{"wesing", "kugou"}},    // 跳过空项
	}
	for _, c := range cases {
		if got := splitCommaList(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitCommaList(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}
