package lyric

// 本文件只测本地 LRC 跳过非法时间戳：NaN、±Inf 与负数的时间标签不产生行，同一行其余合法时间戳
// 照常展开。

import "testing"

func TestParseLocalLRCSkipsInvalidTimestamps(t *testing.T) {
	for _, s := range []string{
		"[00:NaN]x\n",
		"[00:Inf]x\n",
		"[00:-Inf]x\n",
		"[-1:00.00]x\n",
		"[00:-0.5]x\n",
	} {
		if lines, err := ParseLocal([]byte(s)); lines != nil || err != nil {
			t.Fatalf("%q：得 (%+v, %v)，want (nil, nil)", s, lines, err)
		}
	}

	for _, s := range []string{
		"[00:00.00][00:NaN]x\n",
		"[00:NaN][00:00.00]x\n",
		"[-1:00.00]y\n[00:00.00][00:Inf]x\n",
	} {
		lines := mustParseLocal(t, []byte(s))
		if len(lines) != 1 || lines[0].Time != 0 || lines[0].Text != "x" || lines[0].Index != 0 {
			t.Fatalf("%q：得 %+v，want 只有 0 秒的 x 一行", s, lines)
		}
	}
}
