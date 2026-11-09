package wesing

import "testing"

// mid 交叉核对只用按窗口歌名匹配上的记录（设计 §4.1，真机 11-04）：getSongMeta 读到的记录歌名不等于窗口歌名时，
// 退回的可能是上一首，按 mid 为空处理。
//
// 变异自证（实测）：不比歌名直接返回 mid，「退回的是上一首的记录」红；不看窗口歌名是否为空，「窗口歌名为空」红；
// 歌名相等也返回空串，「匹配上」红。
func TestConfirmMID(t *testing.T) {
	cases := []struct {
		name                  string
		songName, window, mid string
		want                  string
	}{
		{"匹配上", "blue hour", "blue hour", "001rQ3L4029e5w", "001rQ3L4029e5w"},
		{"退回的是上一首的记录", "High", "blue hour", "003KxbcH3GDGsd", ""},
		{"只差大小写", "Blue Hour", "blue hour", "001rQ3L4029e5w", ""},
		{"窗口歌名为空", "", "", "003KxbcH3GDGsd", ""},
		{"没取到歌曲信息（mid 本来为空）", "blue hour", "blue hour", "", ""},
	}
	for _, c := range cases {
		if got := confirmMID(c.songName, c.window, c.mid); got != c.want {
			t.Errorf("%s：confirmMID = %q，want %q", c.name, got, c.want)
		}
	}
}

// 回跳检测：判为最小化的拍不判（设计 §4.3，真机 11-04）。兜底模式下最小化时读数停在 32.75 s，上一拍的位置是外推
// 出来的 34.78 s；不排除最小化的拍，此后每一拍都判为回跳。
//
// 变异自证（实测）：不看最小化，「最小化期间读数停住、外推值领先」红；门槛改成 1 s，「差 1.5 s 不算」红；
// 最小化时一律判为回跳，「最小化期间读数停住、外推值领先」红；未最小化也不判，「还原后第一拍：外推跑过了头」
// 「重唱」红。lastPlayTime > 0 是原有条件，读数不为负，去掉它判不出差别。
func TestSeekBack(t *testing.T) {
	cases := []struct {
		name            string
		last, now       float32
		minimized, want bool
	}{
		{"最小化期间读数停住、外推值领先", 34.78, 32.75, true, false},
		{"最小化期间重唱（还原前看不见）", 60, 0, true, false},
		{"还原后第一拍：外推跑过了头", 95, 80, false, true},
		{"还原后第一拍：外推与读数相近", 92.7, 92.75, false, false},
		{"重唱", 136.2, 0, false, true},
		{"差 1.5 s 不算", 40, 38.5, false, false},
		{"第一拍", -1, 0, false, false},
		{"正常推进", 40, 40.03, false, false},
	}
	for _, c := range cases {
		if got := seekBack(c.last, c.now, c.minimized); got != c.want {
			t.Errorf("%s：seekBack(%v, %v, %v) = %v，want %v", c.name, c.last, c.now, c.minimized, got, c.want)
		}
	}
}
