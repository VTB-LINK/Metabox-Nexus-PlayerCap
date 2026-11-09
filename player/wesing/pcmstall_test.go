package wesing

import "testing"

// PCM 停滞标记只对这一次播放有效（设计 §4.2，代码审 F2、F6）：同一首回退重来期间保留，窗口换成别的歌、
// 待机、关窗时清除。runSession 在播放阶段每次看到窗口标题调 seen，待机与 exitWindowClosed 调 reset，
// exitPCMStalled 调 mark，选不选 PCM 只看 on。
//
// 变异自证（实测）：seen 不看空标题，「重来时标题短暂为空」红；seen 从不清除，「换成别的歌再回来」红；
// reset 不清除，「关窗」红；mark 不置 on，「重来时标题不变」红。
func TestPCMStall(t *testing.T) {
	cases := []struct {
		name  string
		steps func(s *pcmStall)
		want  bool
	}{
		{"没停滞过", func(s *pcmStall) { s.seen("晴天") }, false},
		{"重来时标题不变", func(s *pcmStall) { s.mark("晴天"); s.seen("晴天") }, true},
		// F6 复审的反例：读失败重来，lastTitle 被清空，窗口标题又短暂为空。
		{"重来时标题短暂为空", func(s *pcmStall) { s.mark("晴天"); s.seen(""); s.seen("晴天") }, true},
		{"换成别的歌再回来", func(s *pcmStall) { s.mark("晴天"); s.seen("七里香"); s.seen("晴天") }, false},
		{"关窗", func(s *pcmStall) { s.mark("晴天"); s.reset(); s.seen("晴天") }, false},
		{"停滞时标题为空，之后出现标题", func(s *pcmStall) { s.mark(""); s.seen(""); s.seen("晴天") }, false},
		{"再次停滞", func(s *pcmStall) { s.mark("晴天"); s.seen("七里香"); s.mark("七里香"); s.seen("七里香") }, true},
	}
	for _, c := range cases {
		var s pcmStall
		c.steps(&s)
		if s.on != c.want {
			t.Errorf("%s：on = %v，want %v", c.name, s.on, c.want)
		}
	}
}
