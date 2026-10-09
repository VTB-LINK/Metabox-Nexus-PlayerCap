package cloudmusic

// 本文件只测 EffectivePollInterval：网易云轮询循环与 /service-status 的 config_effective
// 共用的轮询下限（< 50ms 抬到 100ms；兼防 time.NewTicker(<=0) panic，故 0 与负值必须被抬高）。

import (
	"testing"
	"time"
)

func TestEffectivePollInterval(t *testing.T) {
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{-1, 100 * time.Millisecond},
		{0, 100 * time.Millisecond},
		{49, 100 * time.Millisecond},
		{50, 50 * time.Millisecond},
		{51, 51 * time.Millisecond},
	}
	for _, c := range cases {
		if got := EffectivePollInterval(c.ms); got != c.want {
			t.Errorf("EffectivePollInterval(%d)=%v，期望 %v", c.ms, got, c.want)
		}
	}
}
