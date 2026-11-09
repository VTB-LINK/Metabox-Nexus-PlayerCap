package lyric

// 本文件只测主歌词逐字时间轴的判据（loadLyrics 里的 wordsOK）：字级单元的起始与时长怎样算合法；
// 不合法时这一行照常给行级、Detailed 为零值。假内存沿用 roma_test.go（fakeMem、entryTimed、unit）。

import (
	"encoding/json"
	"math"
	"testing"
)

// 字级单元的起始要合理（IsPlausiblePlayTime）、行内不回退超过 1ms，时长在 [0,100]，两者都要读得出；
// 任一处不合法，整行只给行级（Text、Time 照常），Detailed 为零值、序列化成 {}。时长写成接受式
// （issue #85）：拒绝式 `wDur < 0 || wDur > 100` 对 NaN 恒假，NaN 进了 words 与整行 duration，
// encoding/json 编码不了 NaN，事件序列化失败——所以每例都实际序列化一次 Detailed。
//
// 变异自证：把时长判断改回 `wDur < 0 || wDur > 100`，三个 NaN 例带出了逐字、且序列化失败而红；把读字
// 时间改回丢弃读错误（`wStart, _ :=`、`wDur, _ :=`，去掉 `!ok1 || !ok2`），「起始读不出」「时长读不出」两例
// 读成 0、带出了逐字而红；把 `wDur <= 100` 改成 `< 100`，「时长 100」一例红；把 `wDur >= 0` 改成 `> 0`，
// 「时长 0」一例红（均实测）。
func TestLoadLyricsWordTiming(t *testing.T) {
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	// unmap 让第 i 个字级单元的 off 处 4 字节读不出。
	unmap := func(i int, off uint32) func(f *fakeMem, e uint32) {
		return func(f *fakeMem, e uint32) {
			vec, _ := f.u32(e + 0x08)
			elem, _ := f.u32(vec + uint32(i)*4)
			for k := uint32(0); k < 4; k++ {
				delete(f.b, elem+off+k)
			}
		}
	}
	cases := []struct {
		name    string
		units   []unit
		corrupt func(f *fakeMem, e uint32)
		wantOK  bool // Detailed 是否带上逐字
	}{
		{"对照组", []unit{{"你", 10, 0.3}, {"好", 10.3, 0.4}}, nil, true},
		{"时长 0", []unit{{"你", 10, 0}, {"好", 10.3, 0.4}}, nil, true},
		{"时长 100", []unit{{"你", 10, 0.3}, {"好", 10.3, 100}}, nil, true},
		{"起始回退不足 1ms", []unit{{"你", 10.3, 0.3}, {"好", 10.2995, 0.4}}, nil, true},
		{"末字时长 NaN", []unit{{"你", 10, 0.3}, {"好", 10.3, nan}}, nil, false},
		{"首字时长 NaN", []unit{{"你", 10, nan}, {"好", 10.3, 0.4}}, nil, false},
		{"两字时长都是 NaN", []unit{{"你", 10, nan}, {"好", 10.3, nan}}, nil, false},
		{"时长 +Inf", []unit{{"你", 10, 0.3}, {"好", 10.3, inf}}, nil, false},
		{"时长为负", []unit{{"你", 10, 0.3}, {"好", 10.3, -0.1}}, nil, false},
		{"时长大于 100", []unit{{"你", 10, 0.3}, {"好", 10.3, 100.5}}, nil, false},
		{"起始 NaN", []unit{{"你", 10, 0.3}, {"好", nan, 0.4}}, nil, false},
		{"起始回退超过 1ms", []unit{{"你", 10, 0.3}, {"好", 9.9, 0.4}}, nil, false},
		// 首字：读不出时读数是 0，排在第二个字之前、不触发回退判断，只能靠读错误拦下。
		{"起始读不出", []unit{{"你", 10, 0.3}, {"好", 10.3, 0.4}}, unmap(0, 0x04), false},
		{"时长读不出", []unit{{"你", 10, 0.3}, {"好", 10.3, 0.4}}, unmap(1, 0x08), false},
	}
	for _, c := range cases {
		f := newFakeMem()
		e := f.entryTimed(10, c.units...)
		if c.corrupt != nil {
			c.corrupt(f, e)
		}
		f.track(testMain, testVtable, mainPath, []uint32{e})
		lines, err := loadLyrics(f, testMain)
		if err != nil || len(lines) != 1 || lines[0].Text != "你好" || lines[0].Time != 10 {
			t.Errorf("%s：行级应照常给出，got %+v err=%v", c.name, lines, err)
			continue
		}
		d := lines[0].Detailed
		if _, err := json.Marshal(d); err != nil {
			t.Errorf("%s：Detailed 序列化失败：%v", c.name, err)
		}
		if got := len(d.Words) == 2; got != c.wantOK {
			t.Errorf("%s：逐字 %d 个单元，want 带上逐字=%v（%+v）", c.name, len(d.Words), c.wantOK, d)
		}
		if c.wantOK && (d.Timestamp != 10 || math.Abs(float64(d.Duration-(c.units[1].start+c.units[1].dur-c.units[0].start))) > 1e-4) {
			t.Errorf("%s：整行 timestamp=%v duration=%v，want 10 与末字终点 − 首字起点", c.name, d.Timestamp, d.Duration)
		}
		if !c.wantOK && (d.Timestamp != 0 || d.Duration != 0 || d.Words != nil) {
			t.Errorf("%s：时间轴不合法时 Detailed 应为零值，got %+v", c.name, d)
		}
	}
}

// 字级文本是 UTF-16：扩展汉字与 emoji 是一对代理码元，要整体解码，行级 Text 与逐字的 Text 都不能
// 变成 U+FFFD。
//
// 变异自证：把 loadLyrics 里的解码改回逐个码元 `rune(wchar)`，三个单元里两个变成两个 U+FFFD 而红（实测）。
func TestLoadLyricsDecodesSurrogatePairs(t *testing.T) {
	f := newFakeMem()
	e := f.entryTimed(10, unit{"𠮷", 10, 0.3}, unit{"🎤", 10.3, 0.4}, unit{"好", 10.7, 0.2})
	f.track(testMain, testVtable, mainPath, []uint32{e})
	lines, err := loadLyrics(f, testMain)
	if err != nil || len(lines) != 1 {
		t.Fatalf("got %+v err=%v", lines, err)
	}
	if lines[0].Text != "𠮷🎤好" {
		t.Errorf("行级 Text = %q，want %q", lines[0].Text, "𠮷🎤好")
	}
	want := []string{"𠮷", "🎤", "好"}
	if len(lines[0].Detailed.Words) != len(want) {
		t.Fatalf("逐字 %d 个单元，want %d", len(lines[0].Detailed.Words), len(want))
	}
	for i, w := range lines[0].Detailed.Words {
		if w.Text != want[i] {
			t.Errorf("第 %d 个单元 Text = %q，want %q", i, w.Text, want[i])
		}
	}
}
