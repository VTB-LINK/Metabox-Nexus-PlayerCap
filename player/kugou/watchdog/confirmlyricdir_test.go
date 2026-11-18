package watchdog

// 本文件只测确认闸门第 6 步：从数据目录的 KuGou.ini 取歌词目录（lyricDirFromINI），
// 缺项时退回「安装目录\Lyric」；所取目录存在与否都原样（规范化后）返回，确认照样成功。

import "testing"

// kugouINI 按设计 §2.5 的实测内容构造（UTF-16LE、[LyricConfigSection] LyricPath=C:\KuGou\Lyric\），
// 另加其他节里的 Path= 与同名键 LyricPath=，确认只认 [LyricConfigSection] 节内的那一个。
const kugouINI = "[DownloadConfigSection]\r\n" +
	"Path=D:\\KuGou\\Download\\\r\n" +
	"LyricPath=D:\\Wrong\\Lyric\\\r\n" +
	"\r\n" +
	"[LyricConfigSection]\r\n" +
	"ShowDesktopLyric=1\r\n" +
	"LyricPath=C:\\KuGou\\Lyric\\\r\n" +
	"\r\n" +
	"[CacheConfigSection]\r\n" +
	"Path=C:\\KuGou\\Temp\\\r\n"

func TestLyricDirFromINIUTF16(t *testing.T) {
	if got := lyricDirFromINI(utf16LEWithBOM(kugouINI)); got != `C:\KuGou\Lyric\` {
		t.Fatalf("got %q", got)
	}
}

func TestLyricDirFromINIUTF8(t *testing.T) {
	bom := append([]byte{0xEF, 0xBB, 0xBF}, kugouINI...)
	if got := lyricDirFromINI(bom); got != `C:\KuGou\Lyric\` {
		t.Fatalf("带 BOM：got %q", got)
	}
	if got := lyricDirFromINI([]byte(kugouINI)); got != `C:\KuGou\Lyric\` {
		t.Fatalf("无 BOM：got %q", got)
	}
}

// 节名与键名大小写不敏感，键与等号两侧的空白去掉；只有 \n 换行也认。
func TestLyricDirFromINICaseAndSpace(t *testing.T) {
	ini := "[lyricconfigsection]\n  lyricpath =  E:\\歌词\\  \n"
	if got := lyricDirFromINI(utf16LEWithBOM(ini)); got != `E:\歌词\` {
		t.Fatalf("got %q", got)
	}
}

func TestLyricDirFromININoKey(t *testing.T) {
	cases := map[string][]byte{
		"空文件":             nil,
		"只有 BOM":          {0xFF, 0xFE},
		"节里没有该键":          utf16LEWithBOM("[LyricConfigSection]\r\nShowDesktopLyric=1\r\n"),
		"只在其他节":           utf16LEWithBOM("[DownloadConfigSection]\r\nLyricPath=D:\\Wrong\\\r\n"),
		"节外（文件开头）":        utf16LEWithBOM("LyricPath=D:\\Wrong\\\r\n[LyricConfigSection]\r\n"),
		"值为空":             utf16LEWithBOM("[LyricConfigSection]\r\nLyricPath=\r\n"),
		"节名只是前缀相同":        utf16LEWithBOM("[LyricConfigSectionX]\r\nLyricPath=D:\\Wrong\\\r\n"),
		"键名只是后缀相同":        utf16LEWithBOM("[LyricConfigSection]\r\nOldLyricPath=D:\\Wrong\\\r\n"),
		"被注释掉":            utf16LEWithBOM("[LyricConfigSection]\r\n;LyricPath=D:\\Wrong\\\r\n"),
		"UTF-16 奇数长度的残字节": append(utf16LEWithBOM("[LyricConfigSection]\r\n"), 'L'),
	}
	for name, b := range cases {
		if got := lyricDirFromINI(b); got != "" {
			t.Errorf("%s：got %q，应为空", name, got)
		}
	}
}

// 歌词目录不要求存在：确认时还没建出的目录，之后酷狗建了、写了缓存，读取者按暂缺重试即可取到。ini 缺项、
// 读不出或值不是绝对路径时退回「安装目录\Lyric」，ini 给出绝对路径即用它、不退回（那才是酷狗在用的目录）；
// 结果经 filepath.Clean，确认照样成功。各例里的目录在假机器上都不存在（ini 指定的目录存在即
// TestConfirmHappyPath）。
func TestConfirmLyricDirNotRequiredToExist(t *testing.T) {
	cases := []struct {
		name string
		ini  []byte // nil 即 ini 读不出
		want string
	}{
		{"ini 指定的目录不存在：用它，不退回安装目录",
			utf16LEWithBOM("[LyricConfigSection]\r\nLyricPath=E:\\歌词\\Lyric\\\r\n"), `E:\歌词\Lyric`},
		{"ini 的绝对路径带 . 与重复分隔符：规范化",
			utf16LEWithBOM("[LyricConfigSection]\r\nLyricPath=E:\\a\\.\\\\Lyric\\\r\n"), `E:\a\Lyric`},
		{"ini 缺项、安装目录下无 Lyric",
			utf16LEWithBOM("[LyricConfigSection]\r\nShowDesktopLyric=1\r\n"), stubInstall + `\Lyric`},
		{"ini 读不出", nil, stubInstall + `\Lyric`},
		{"ini 的值是相对路径：按缺项处理，不按本程序的当前目录解析",
			utf16LEWithBOM("[LyricConfigSection]\r\nLyricPath=Lyric\\\r\n"), stubInstall + `\Lyric`},
		{"ini 的值只有盘符没有根（C:Lyric）：不是绝对路径",
			utf16LEWithBOM("[LyricConfigSection]\r\nLyricPath=C:Lyric\r\n"), stubInstall + `\Lyric`},
		{"ini 的值只有根没有盘符（\\Lyric）：不是绝对路径",
			utf16LEWithBOM("[LyricConfigSection]\r\nLyricPath=\\Lyric\r\n"), stubInstall + `\Lyric`},
	}
	for _, c := range cases {
		m := newFakeMachine()
		if c.ini == nil {
			delete(m.files, pathKey(stubINI))
		} else {
			m.files[pathKey(stubINI)] = c.ini
		}
		got, err := confirmWith(m.deps())
		if err != nil || got.DataDir != stubDataDir {
			t.Errorf("%s：(%+v, %v)，want 确认成功", c.name, got, err)
			continue
		}
		if got.LyricDir != c.want {
			t.Errorf("%s：LyricDir = %q，want %q", c.name, got.LyricDir, c.want)
		}
	}
}
