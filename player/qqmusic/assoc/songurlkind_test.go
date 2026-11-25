package assoc

// 本文件只测 songUrl 的归类：ClassifySongURL 的四种归类（设计 §6.3），与它对 songmid 形式的要求 IsSongMid。

import "testing"

func TestClassifySongURL(t *testing.T) {
	cases := []struct {
		in   string
		kind URLKind
		mid  string
		id   uint32
	}{
		{`http://stream4.qqmusic.qq.com/000TestMid0001.wma`, KindMid, "000TestMid0001", 0},
		{`http://streamundefined.qqmusic.qq.com/123456789.wma`, KindID, "", 123456789},
		{`http://stream12.qqmusic.qq.com/1.wma`, KindID, "", 1},
		{`HTTP://STREAM4.QQMUSIC.QQ.COM/abc.wma`, KindMid, "abc", 0},
		{`https://stream4.qqmusic.qq.com/000TestMid0001.wma`, KindMid, "000TestMid0001", 0},
		{`https://stream4.qqmusic.qq.com/123456789.wma`, KindID, "", 123456789},
		{`http://stream4.qqmusic.qq.com/4294967295.wma`, KindID, "", 4294967295},

		// 数字超出 uint32、查询串、多级路径、别的扩展名、空名、非字母数字：忽略。
		{`http://stream4.qqmusic.qq.com/4294967296.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/000TestMid0001.wma?x=1`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/000TestMid0001.wma#x`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/a/b.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/000TestMid0001.mp3`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/a%42c.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/12-34.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/abc.wma/`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com`, KindOther, "", 0},

		// 主机名不是 stream*.qqmusic.qq.com、带端口或用户信息、协议不对：忽略。
		{`http://stream4.qqmusic.qq.com.example.com/abc.wma`, KindOther, "", 0},
		{`http://xstream4.qqmusic.qq.com/abc.wma`, KindOther, "", 0},
		{`http://stream4.a.qqmusic.qq.com/abc.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com:80/abc.wma`, KindOther, "", 0},
		{`http://u@stream4.qqmusic.qq.com/abc.wma`, KindOther, "", 0},
		{`ftp://stream4.qqmusic.qq.com/abc.wma`, KindOther, "", 0},
		{`stream4.qqmusic.qq.com/abc.wma`, KindOther, "", 0},

		// 用户信息（含以 stream 开头、主机名合规的）、显式端口（含空端口）、查询串与片段（含空的）：忽略。
		// 第一例由按 URL 结构取主机名挡住（旧实现按字符串切分时把它归为 KindID）。变异自证（实测）：去掉用户信息检查，
		// 带 @ 的另三例红；去掉端口检查，三个端口例红；去掉 ? 与 # 的检查，查询串与片段各例红。
		{`http://stream@other.qqmusic.qq.com/123.wma`, KindOther, "", 0},
		{`http://stream4@stream4.qqmusic.qq.com/123.wma`, KindOther, "", 0},
		{`http://@stream4.qqmusic.qq.com/123.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com:80/123.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com:/123.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/123.wma#frag`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/123.wma#`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com/123.wma?`, KindOther, "", 0},
		{`http:/stream4.qqmusic.qq.com/123.wma`, KindOther, "", 0},
		{`http://stream4.qqmusic.qq.com./123.wma`, KindOther, "", 0},

		// 本地文件：盘符绝对路径、UNC、带 \\?\ 前缀。
		{`E:\a.flac`, KindLocal, "", 0},
		{`E:/a.flac`, KindLocal, "", 0},
		{`\\srv\s\a.flac`, KindLocal, "", 0},
		{`\\?\E:\a.flac`, KindLocal, "", 0},

		// 相对路径、盘符相对、根相对、空串：忽略。
		{`a.flac`, KindOther, "", 0},
		{`E:a.flac`, KindOther, "", 0},
		{`\a.flac`, KindOther, "", 0},
		{``, KindOther, "", 0},
	}
	for _, c := range cases {
		kind, mid, id := ClassifySongURL(c.in)
		if kind != c.kind || mid != c.mid || id != c.id {
			t.Errorf("ClassifySongURL(%q) = (%d, %q, %d)，应为 (%d, %q, %d)", c.in, kind, mid, id, c.kind, c.mid, c.id)
		}
	}
}

// IsSongMid 同 ClassifySongURL 对 mid 的要求：非空、只由 ASCII 字母数字组成、不全是数字。正例取上一条用例里归为 KindMid
// 的各名，反例取归为 KindID 或因名不合归为 KindOther 的各名，另加非 ASCII 的字母与数字、空白与控制字符。
func TestIsSongMid(t *testing.T) {
	for _, s := range []string{"000TestMid0001", "abc", "A", "0a", "a0", "ABCxyz0123456789"} {
		if !IsSongMid(s) {
			t.Errorf("IsSongMid(%q) = false，应为 true", s)
		}
	}
	for _, s := range []string{
		"", "123456789", "1", "4294967295", "4294967296", "00000000000000000000", // 空、全是数字（含超出 uint32 的）
		"a%42c", "12-34", "abc.wma", "a/b", "a_b", "a+b", "-abc", // 非字母数字的 ASCII
		"abc ", " abc", "a\x00b", "a\tb", // 空白与控制字符
		"中文", "abcé", "ab１", "ＡＢＣ", "\uFEFFabc", // 非 ASCII 的字母与数字
	} {
		if IsSongMid(s) {
			t.Errorf("IsSongMid(%q) = true，应为 false", s)
		}
	}
}
