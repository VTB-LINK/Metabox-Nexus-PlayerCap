package sodamusic

// 本文件只测 EffectivePollInterval：汽水音乐轮询循环与 /service-status 的 config_effective
// 共用的轮询下限（< minPollInterval 即 200ms 时抬到 200ms）。

import (
	"testing"
	"time"
)

func TestEffectivePollInterval(t *testing.T) {
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{-1, 200 * time.Millisecond},
		{0, 200 * time.Millisecond},
		{199, 200 * time.Millisecond},
		{200, 200 * time.Millisecond},
		{201, 201 * time.Millisecond},
	}
	for _, c := range cases {
		if got := EffectivePollInterval(c.ms); got != c.want {
			t.Errorf("EffectivePollInterval(%d)=%v，期望 %v", c.ms, got, c.want)
		}
	}
}
