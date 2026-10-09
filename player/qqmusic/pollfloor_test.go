package qqmusic

// 本文件只测 EffectivePollInterval：QQ 音乐轮询循环与 /service-status 的 config_effective
// 共用的轮询下限（< 30ms 抬到 50ms，30ms 处有跳变）。

import (
	"testing"
	"time"
)

func TestEffectivePollInterval(t *testing.T) {
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{-5, 50 * time.Millisecond},
		{0, 50 * time.Millisecond},
		{29, 50 * time.Millisecond},
		{30, 30 * time.Millisecond},
		{31, 31 * time.Millisecond},
	}
	for _, c := range cases {
		if got := EffectivePollInterval(c.ms); got != c.want {
			t.Errorf("EffectivePollInterval(%d)=%v，期望 %v", c.ms, got, c.want)
		}
	}
}
