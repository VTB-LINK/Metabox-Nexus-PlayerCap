package logger

// 本文件只测一件事：日志正文单独格式化，模块前缀不占格式实参。
//
// 修复前五个方法都是 log.Printf("[%s] [*] "+i18n.T(format), 模块名, 实参...)：前缀与正文拼成一个
// 格式串，模块名是第一个实参。正文里带显式序号的占位符因此错位——%[1]s 读到模块名，%[2]s 读到
// 调用方的第一个实参，最后一个实参没人读。英文原文里有六条这样的日志（server/router.go 的
// 「%[1]s player [%[2]s] → playing」等），英文环境下输出一直是错的；中文译文用顺序占位符，不受影响。

import (
	"bytes"
	"fmt"
	"log"
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
)

// capture 在指定语言下执行 emit，返回它写进标准 log 的内容（不带时间戳）。
func capture(t *testing.T, lang i18n.Lang, emit func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	i18n.SetLanguage(lang)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		i18n.SetLanguage(i18n.Chinese)
	}()
	emit()
	return buf.String()
}

// legacy 是修复前的拼法算出的一行：前缀与正文拼成一个格式串，模块名作第一个实参。
// 只在用例里当对照，用来证明顺序占位符的输出逐字节没变。
func legacy(module, icon, format string, args ...interface{}) string {
	return fmt.Sprintf("[%s] ["+icon+"] "+i18n.T(format), append([]interface{}{module}, args...)...) + "\n"
}

// TestExplicitIndexReadsCallerArgs 钉死带显式序号的格式串读到的是调用方的实参。
// 六条格式串取自 server/router.go 的调用点，是现有全部带显式序号的日志。
//
// 变异自证（隔离副本，实测）：改回前缀与正文拼成一个格式串、模块名作第一个实参，八例全红，
// 得到的正是下面「修复前」一列。
func TestExplicitIndexReadsCallerArgs(t *testing.T) {
	cases := []struct {
		format string
		args   []interface{}
		want   string // 英文环境下的正文
		before string // 修复前英文环境下的正文，留作对照
	}{
		{"%[1]s player [%[2]s] → playing", []interface{}{"prior", "wesing"},
			"prior player [wesing] → playing", "Router player [prior] → playing"},
		{"%[1]s player [%[2]s] → loading", []interface{}{"normal", "kugou"},
			"normal player [kugou] → loading", "Router player [normal] → loading"},
		{"%[1]s player [%[2]s] → paused", []interface{}{"prior", "wesing"},
			"prior player [wesing] → paused", "Router player [prior] → paused"},
		{"%[1]s player [%[2]s] → idle", []interface{}{"normal", "qqmusic"},
			"normal player [qqmusic] → idle", "Router player [normal] → idle"},
		{"%[1]s player [%[2]s] loading timeout (%[3]ds), marking idle", []interface{}{"prior", "wesing", 15},
			"prior player [wesing] loading timeout (15s), marking idle",
			"Router player [prior] loading timeout (%!d(string=wesing)s), marking idle"},
		{"%[1]s player [%[2]s] paused timeout (%[3]ds), clearing activated", []interface{}{"normal", "kugou", 20},
			"normal player [kugou] paused timeout (20s), clearing activated",
			"Router player [normal] paused timeout (%!d(string=kugou)s), clearing activated"},
		// 不在译表里的格式串：序号倒着用。
		{"%[2]s then %[1]s", []interface{}{"a", "b"}, "b then a", "a then Router"},
		// 序号越界：现在按调用方的实参数，给出 fmt 的报错标记；修复前模块名占了一位，读到的是前一个实参。
		{"%[3]s", []interface{}{"a", "b"}, "%!s(BADINDEX)", "b"},
	}
	l := New("Router")
	for _, c := range cases {
		var old string
		got := capture(t, i18n.English, func() {
			old = legacy("Router", "*", c.format, c.args...)
			l.Info(c.format, c.args...)
		})
		if want := "[Router] [*] " + c.want + "\n"; got != want {
			t.Errorf("英文环境 %q\n  得到 %q\n  预期 %q", c.format, got, want)
		}
		// 对照列须如实：修复前的拼法确实给出 before。
		if old != "[Router] [*] "+c.before+"\n" {
			t.Errorf("用例写错：修复前的拼法对 %q 给出 %q，对照列写的是 %q", c.format, old, c.before)
		}
	}
}

// TestChineseOutputUnchanged 钉死中文环境下的输出与修复前逐字节相同：上面那六条在中文环境走的是
// 顺序占位符的译文，修复前就是对的。另取几条普通日志一并核对。
//
// 变异自证（隔离副本，实测）：正文不过 i18n.T，红——输出成了英文原文。
func TestChineseOutputUnchanged(t *testing.T) {
	cases := []struct {
		format string
		args   []interface{}
		want   string // 留空则只与修复前的拼法比
	}{
		{"%[1]s player [%[2]s] → playing", []interface{}{"优先", "wesing"}, "[Router] [*] 优先播放器 [wesing] → playing\n"},
		{"%[1]s player [%[2]s] → loading", []interface{}{"普通", "kugou"}, ""},
		{"%[1]s player [%[2]s] → paused", []interface{}{"优先", "wesing"}, ""},
		{"%[1]s player [%[2]s] → idle", []interface{}{"普通", "qqmusic"}, ""},
		{"%[1]s player [%[2]s] loading timeout (%[3]ds), marking idle", []interface{}{"优先", "wesing", 15},
			"[Router] [*] 优先播放器 [wesing] loading 超时 (15s)，标记为 idle\n"},
		{"%[1]s player [%[2]s] paused timeout (%[3]ds), clearing activated", []interface{}{"普通", "kugou", 20},
			"[Router] [*] 普通播放器 [kugou] 暂停超时 (20s)，清除 activated\n"},
		{"paused @ %.2fs", []interface{}{12.5}, "[Router] [*] 暂停 @ 12.50s\n"},
		{"CDP connected, monitoring playback state", nil, "[Router] [*] CDP 连接成功，开始监听播放状态\n"},
	}
	l := New("Router")
	for _, c := range cases {
		var want string
		got := capture(t, i18n.Chinese, func() {
			want = legacy("Router", "*", c.format, c.args...)
			l.Info(c.format, c.args...)
		})
		if got != want {
			t.Errorf("中文环境 %q 与修复前不同\n  得到 %q\n  修复前 %q", c.format, got, want)
		}
		if c.want != "" && got != c.want {
			t.Errorf("中文环境 %q\n  得到 %q\n  预期 %q", c.format, got, c.want)
		}
	}
}

// TestSequentialFormatsUnchanged 钉死不带显式序号的格式串在两种语言下的输出都与修复前逐字节相同，
// 包括 fmt 报错的位置：不带实参、实参多于或少于占位符、%%、落单的 %、宽度取自实参，以及模块名里
// 的 % 与「] [」。五个方法各走一遍。
//
// 变异自证（隔离副本，实测）：
//   - 不带实参时跳过格式化、把正文原样输出：红，"100%% done" 的 %% 没有折成 %，落单的 % 与
//     缺实参的占位符也不再有 fmt 的报错标记。
//   - 模块名拼进格式串再格式化：红，模块名里的 %d 被当成占位符。
//   - Success 的图标写成 *、前缀里的图标写死、前缀少了模块名：红。
func TestSequentialFormatsUnchanged(t *testing.T) {
	formats := []struct {
		format string
		args   []interface{}
	}{
		{"plain message", nil},
		{"100%% done", nil},
		{"trailing percent %", nil},
		{"value %d", []interface{}{1}},
		{"extra %d", []interface{}{1, 2, "three"}}, // 实参多于占位符：末尾的 %!(EXTRA …)
		{"missing %d and %s", []interface{}{1}},    // 实参少于占位符：%!s(MISSING)
		{"missing all %d", nil},
		{"wrong type %d", []interface{}{"text"}},
		{"width %*d|", []interface{}{5, 42}},
		{"nil %v", []interface{}{nil}},
		{"♪ Song: %s", []interface{}{"歌名"}}, // 在译表里：中文环境走译文
	}
	methods := []struct {
		icon string
		call func(*Logger, string, ...interface{})
	}{
		{"*", (*Logger).Info}, {"✓", (*Logger).Success}, {"!", (*Logger).Warn}, {"✗", (*Logger).Error}, {"+", (*Logger).Detail},
	}
	for _, module := range []string{"Router", "CloudMusic] [CDP", "odd %d module"} {
		l := New(module)
		for _, lang := range []i18n.Lang{i18n.Chinese, i18n.English} {
			for _, m := range methods {
				for _, f := range formats {
					var want string
					got := capture(t, lang, func() {
						want = legacy(module, m.icon, f.format, f.args...)
						m.call(l, f.format, f.args...)
					})
					if got != want {
						t.Errorf("模块 %q 语言 %v 图标 %s 格式 %q 与修复前不同\n  得到 %q\n  修复前 %q", module, lang, m.icon, f.format, got, want)
					}
				}
			}
		}
	}
	// 具体的两行，免得「与修复前相同」成了两边一起错。
	l := New("CloudMusic] [CDP")
	if got, want := capture(t, i18n.English, func() { l.Warn("extra %d", 1, 2) }), "[CloudMusic] [CDP] [!] extra 1%!(EXTRA int=2)\n"; got != want {
		t.Errorf("得到 %q，预期 %q", got, want)
	}
	if got, want := capture(t, i18n.Chinese, func() { l.Success("♪ Song: %s", "歌名") }), "[CloudMusic] [CDP] [✓] ♪ 歌曲: 歌名\n"; got != want {
		t.Errorf("得到 %q，预期 %q", got, want)
	}
}
