package applemusic

// 本文件只测 nextLine：推送行号只往前走，跳转后才允许往回。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player/krc"
)

// 行号只往前走；跳转后（last=-1）才允许往回。
//
// 变异自证：去掉 nextLine 里 `idx < last` 的拦截，「落锚回拉」一例变红。
func TestNextLine(t *testing.T) {
	lines := []krc.Line{{Time: 1}, {Time: 5}, {Time: 10}}
	cases := []struct {
		name string
		t    float32
		last int
		idx  int
		emit bool
	}{
		{"首行", 1.2, -1, 0, true},
		{"同一行不重推", 3, 0, 0, false},
		{"前进", 5.1, 0, 1, true},
		{"落锚回拉不往回推", 9.8, 2, 1, false},
		{"跳转后可往回", 2, -1, 0, true},
		{"还没到首行", 0.5, -1, -1, false},
	}
	for _, c := range cases {
		idx, emit := nextLine(lines, c.t, c.last)
		if emit != c.emit || (emit && idx != c.idx) {
			t.Errorf("%s: nextLine(t=%.1f,last=%d)=(%d,%v), want (%d,%v)", c.name, c.t, c.last, idx, emit, c.idx, c.emit)
		}
	}
}
