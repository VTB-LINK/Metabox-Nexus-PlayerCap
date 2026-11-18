package assoc

// 本文件只测 pick 怎样由 LyricPath 得出要读的路径（设计 §6.5 第 3 步）：绝对路径原样用，相对路径拼在
// 歌词目录下；逃出歌词目录、带根或盘符的相对路径、歌词目录为空时的相对路径不可用，且不读。

import (
	"slices"
	"testing"
)

const testLRC = "[00:01.00]第一行\n[00:02.00]第二行\n"

// recordRead 返回一个记下每次所读路径、恒读出 testLRC 的 read。
func recordRead(paths *[]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		*paths = append(*paths, p)
		return []byte(testLRC), nil
	}
}

func TestPickRelativeJoinAndEscape(t *testing.T) {
	const dir = `C:\KuGou\Lyric`
	usable := []struct{ path, dir, want string }{
		{"a.lrc", dir, `C:\KuGou\Lyric\a.lrc`},
		{`sub\a.lrc`, dir + `\`, `C:\KuGou\Lyric\sub\a.lrc`},
		{`sub\..\a.lrc`, dir, `C:\KuGou\Lyric\a.lrc`},
		{`E:\Downloads\伴奏\暗杠 - 童话镇.lrc`, dir, `E:\Downloads\伴奏\暗杠 - 童话镇.lrc`},
		{`E:\Downloads\x.lrc`, "", `E:\Downloads\x.lrc`}, // 绝对路径不依赖歌词目录
		{`\\nas\music\x.lrc`, dir, `\\nas\music\x.lrc`},
	}
	for _, c := range usable {
		var read []string
		r := pick(collected{cands: []candidate{{c.path, SourceCache, 1}}}, c.dir, recordRead(&read))
		if !slices.Equal(read, []string{c.want}) || r.Kind != KindFound || len(r.Lines) != 2 {
			t.Errorf("LyricPath %q、歌词目录 %q：读了 %q、结果 %v，want 读 %q 并取到", c.path, c.dir, read, r.Kind, c.want)
		}
	}

	unusable := []struct{ path, dir string }{
		{`..\x.lrc`, dir},
		{`a\..\..\x.lrc`, dir},
		{`\x.lrc`, dir},
		{"/x.lrc", dir},
		{"C:x.lrc", dir},
		{"CON", dir}, // 设备名：读它会阻塞或读到别处
		{"", dir},
		{"a.lrc", ""}, // 歌词目录为空时相对路径跳过
	}
	for _, c := range unusable {
		var read []string
		r := pick(collected{cands: []candidate{{c.path, SourceCache, 1}}}, c.dir, recordRead(&read))
		if len(read) != 0 || r.Kind != KindNone || r.Retry {
			t.Errorf("LyricPath %q、歌词目录 %q：读了 %q、结果 %+v，want 不读、KindNone、不待重试", c.path, c.dir, read, r)
		}
	}
}
