package launch

// 本文件只测 Gate 的判据（issue #212）：路径为空即拒绝，任一目录段为 WindowsApps 即拒绝，其余放行。纯函数，
// 不访问文件系统。

import (
	"errors"
	"testing"
)

// TestGate
//
// 防的缺陷与变异（均实测变红）：空路径放行；按子串（Contains）或前缀判断（WindowsAppsBackup 等被拒）；区分大小写；
// 取目录或切分时只认 \（正斜杠写法放行）；末段文件名也参与比较（文件名为 WindowsApps 被拒）；不去段尾的点与空格
// （WindowsApps.、「WindowsApps 」放行）；拒绝时不带上路径。
func TestGate(t *testing.T) {
	const store = `C:\Program Files\WindowsApps\1F8B0F94.122165AE053F_3.1.23.0_x64__j2p0p5q0044a6\VFS\ProgramFilesX86\NetEase\CloudMusic\cloudmusic.exe`
	cases := []struct {
		name string
		path string
		want string // "ok" / "unknown" / "packaged"
	}{
		{"#212 用户的商店版网易云", store, "packaged"},
		{"其他卷", `D:\WindowsApps\SpotifyAB.SpotifyMusic_1.2.3.0_x64__zpdnekdrzrea0\Spotify.exe`, "packaged"},
		{"全小写", `c:\program files\windowsapps\x\y.exe`, "packaged"},
		{"全大写", `C:\PROGRAM FILES\WINDOWSAPPS\X\Y.EXE`, "packaged"},
		{"正斜杠", `C:/Program Files/WindowsApps/x/y.exe`, "packaged"},
		{"混用分隔符", `C:\Program Files/WindowsApps\x.exe`, "packaged"},
		{"重复分隔符", `C:\Program Files\\WindowsApps\\x.exe`, "packaged"},
		{`\\?\ 前缀`, `\\?\C:\Program Files\WindowsApps\x\y.exe`, "packaged"},
		{"NT 设备路径", `\Device\HarddiskVolume3\Program Files\WindowsApps\x\y.exe`, "packaged"},
		{"UNC", `\\server\share\WindowsApps\x.exe`, "packaged"},
		{"段尾带点", `C:\Program Files\WindowsApps.\x\y.exe`, "packaged"},
		{"段尾带空格", `C:\Program Files\WindowsApps \x\y.exe`, "packaged"},
		{"段尾点与空格混合", `C:\Program Files\WindowsApps. .\x\y.exe`, "packaged"},
		{"以分隔符结尾", `C:\Program Files\WindowsApps\`, "packaged"},
		{"相对路径的首段", `WindowsApps\x.exe`, "packaged"},
		{"深层的目录段", `C:\a\b\c\WindowsApps\d\e\f.exe`, "packaged"},
		{"只有目录没有文件名", `WindowsApps/`, "packaged"},

		{"空路径", "", "unknown"},

		{"官网安装版网易云", `C:\Program Files\NetEase\CloudMusic\cloudmusic.exe`, "ok"},
		{"官网安装版 Spotify", `C:\Users\a b\AppData\Roaming\Spotify\Spotify.exe`, "ok"},
		{"段名以它开头", `C:\WindowsAppsBackup\x.exe`, "ok"},
		{"段名以它开头且带连字符", `C:\windowsapps-old\x.exe`, "ok"},
		{"段名以它结尾", `C:\XWindowsApps\x.exe`, "ok"},
		{"段名带数字后缀", `C:\WindowsApps2\x.exe`, "ok"},
		{"段名带下划线后缀", `C:\WindowsApps_\x.exe`, "ok"},
		{"段名里有空格", `C:\Program Files\Windows Apps\x.exe`, "ok"},
		{"段名前有空格", `C:\Program Files\ WindowsApps\x.exe`, "ok"},
		{"拆成两段", `C:\Windows\Apps\x.exe`, "ok"},
		{"段名里含它", `C:\My WindowsApps Folder\x.exe`, "ok"},
		{"文件名是它（带扩展名）", `C:\Program Files\NetEase\WindowsApps.exe`, "ok"},
		{"文件名是它（无扩展名）", `C:\Program Files\NetEase\WindowsApps`, "ok"},
		{"只有文件名", `cloudmusic.exe`, "ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Gate(c.path)
			var pe *PackagedError
			switch c.want {
			case "ok":
				if err != nil {
					t.Errorf("Gate(%q) = %v，期望 nil", c.path, err)
				}
			case "unknown":
				if !errors.Is(err, ErrExeUnknown) {
					t.Errorf("Gate(%q) = %v，期望 ErrExeUnknown", c.path, err)
				}
			case "packaged":
				if !errors.As(err, &pe) {
					t.Fatalf("Gate(%q) = %v，期望 *PackagedError", c.path, err)
				}
				if pe.ExePath != c.path {
					t.Errorf("PackagedError.ExePath = %q，期望 %q", pe.ExePath, c.path)
				}
				if errors.Is(err, ErrExeUnknown) {
					t.Errorf("Gate(%q) 的商店版错误不得被认作 ErrExeUnknown", c.path)
				}
			}
		})
	}
}
