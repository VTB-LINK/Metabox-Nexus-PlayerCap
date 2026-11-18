package assoc

// 本文件只测上报前的路径脱敏 redactPaths（裁定 V）：数据目录、歌词目录、用户主目录不分大小写、按字面依次替换为
// <data>、<lyric>、<home>，数据目录在主目录之下时整段记为 <data>；空的目录不参与替换；不含这些路径的文本原样。

import "testing"

func TestRedactPaths(t *testing.T) {
	const (
		home  = `C:\Users\Alice`
		data  = `C:\Users\Alice\AppData\Roaming\KuGou8`
		lyric = `D:\KuGou\Lyric`
	)
	cases := []struct {
		name, in, data, lyric, home, want string
	}{
		{"三种路径、大小写不同",
			`c:\users\alice\appdata\roaming\kugou8\KGMusicV3.db-wal; D:\KUGOU\LYRIC\a.krc; C:\USERS\ALICE\Music\b.lrc`,
			data, lyric, home,
			`<data>\KGMusicV3.db-wal; <lyric>\a.krc; <home>\Music\b.lrc`},
		{"同一路径出现多次", data + `\a ` + data + `\b`, data, lyric, home, `<data>\a <data>\b`},
		{"路径里有正则元字符", `E:\a.b (x)+[y]\KGMusicV3.db`, `E:\a.b (x)+[y]`, "", "", `<data>\KGMusicV3.db`},
		{"非 ASCII 用户名", `C:\Users\王五\AppData\x`, "", "", `c:\users\王五`, `<home>\AppData\x`},
		// 反例
		{"不含这些路径", `KuGou database changed during read: pages read in the two passes differ`, data, lyric, home,
			`KuGou database changed during read: pages read in the two passes differ`},
		{"目录为空不替换", `C:\Users\Alice\x`, "", "", "", `C:\Users\Alice\x`},
		{"只是前缀相近", `C:\Users\Alicia\x`, "", "", home, `C:\Users\Alicia\x`},
		{"空文本", "", data, lyric, home, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redactPaths(c.in, c.data, c.lyric, c.home); got != c.want {
				t.Fatalf("redactPaths(%q)\n got  %q\n want %q", c.in, got, c.want)
			}
		})
	}
}
