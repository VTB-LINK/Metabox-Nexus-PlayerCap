package spotify

// 本文件测「页面读数 → 真实播放位置」这一步（truePositionSec）。
//
// 校正量的实测依据写在 spotify.go 的 progressLeadSec 注释里；这里的用例只钉住**它的作用条件**：
// 播放中减掉超前量、暂停时原样、减成负数取 0。变异自证：把 playing 判据删掉（两种状态都减）→
// 暂停那条红；把减改成加 → 两条红；去掉下限 → 开播首帧那条红。

import "testing"

func TestTruePositionAppliesLeadOnlyWhilePlaying(t *testing.T) {
	cases := []struct {
		raw     float32
		playing bool
		want    float32
	}{
		{120.0, true, 119.0},   // 播放中：读数超前 1s
		{120.0, false, 120.0},  // 暂停：读数就是位置，原样
		{0.5, true, 0.0},       // 刚开播：读数还没有超前 1s，取 0 而不是负数
		{1.0, true, 0.0},       // 边界：正好等于超前量
		{1.2, true, 0.200000},  // 略过边界
		{178.63, true, 177.63}, // 真机读数样例（/spotify/lyric_update 实测过 178.64）
	}
	for _, c := range cases {
		got := truePositionSec(c.raw, c.playing)
		if diff := got - c.want; diff > 0.001 || diff < -0.001 {
			t.Errorf("truePositionSec(%.3f, %v) = %.3f，期望 %.3f", c.raw, c.playing, got, c.want)
		}
	}
}
