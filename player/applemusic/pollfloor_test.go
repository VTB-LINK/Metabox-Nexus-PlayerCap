package applemusic

// 本文件只测 EffectivePollInterval：Apple Music 轮询循环与 /service-status 的 config_effective
// 共用的轮询下限（< 50ms 抬到 50ms）。

import (
	"testing"
	"time"
)

// 变异自证：把 EffectivePollInterval 的下限改成只挡 ≤ 0，30 一例得到 30ms 而红。
func TestEffectivePollInterval(t *testing.T) {
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{-5, 50 * time.Millisecond},
		{0, 50 * time.Millisecond},
		{30, 50 * time.Millisecond},
		{49, 50 * time.Millisecond},
		{50, 50 * time.Millisecond},
		{51, 51 * time.Millisecond},
		{300, 300 * time.Millisecond},
	}
	for _, c := range cases {
		if got := EffectivePollInterval(c.ms); got != c.want {
			t.Errorf("EffectivePollInterval(%d)=%v，期望 %v", c.ms, got, c.want)
		}
	}
}
