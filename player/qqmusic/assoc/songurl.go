package assoc

import (
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// URLKind 是关联表 songUrl 的归类（设计 §6.3）。
type URLKind int

const (
	KindOther URLKind = iota // 其余写法，不参与选条目
	KindMid                  // 在线歌曲，以 songmid 为键
	KindID                   // 在线歌曲，以数字 songid 为键
	KindLocal                // 本地文件，键为音频文件的绝对路径
)

// ClassifySongURL 归类 songUrl。
//
// 在线写法为 http(s)://stream*.qqmusic.qq.com/<x>.wma，按 net/url 解析：协议与主机名不区分大小写，* 不含点，
// 不带用户信息、端口（含空端口）、查询串与片段（含空的 ? 与 #）；x 取转义形式的路径，只由 ASCII 字母数字组成，
// 全为数字且在 uint32 范围内为数字 songid（KindID），否则为 songmid（KindMid，区分大小写原样返回），全为数字而
// 超出 uint32 的忽略。本地写法为盘符绝对路径或 UNC 路径（按 Windows 规则，filepath.IsAbs）。其余为 KindOther。
func ClassifySongURL(u string) (kind URLKind, mid string, id uint32) {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "http" && p.Scheme != "https" {
		if filepath.IsAbs(u) {
			return KindLocal, "", 0
		}
		return KindOther, "", 0
	}
	server, ok := strings.CutSuffix(strings.ToLower(p.Hostname()), ".qqmusic.qq.com")
	if !ok || !strings.HasPrefix(server, "stream") || strings.Contains(server, ".") ||
		p.User != nil || p.Host != p.Hostname() || strings.ContainsAny(u, "?#") {
		return KindOther, "", 0
	}
	file, ok := strings.CutPrefix(p.EscapedPath(), "/")
	if !ok {
		return KindOther, "", 0
	}
	x, ok := strings.CutSuffix(file, ".wma")
	if !ok || x == "" || strings.IndexFunc(x, notASCIIAlnum) >= 0 {
		return KindOther, "", 0
	}
	if IsSongMid(x) {
		return KindMid, x, 0
	}
	n, err := strconv.ParseUint(x, 10, 32)
	if err != nil {
		return KindOther, "", 0
	}
	return KindID, "", uint32(n)
}

// IsSongMid 报告 s 是否合 songmid 的形式：非空、只由 ASCII 字母数字组成、不全是数字（同 ClassifySongURL 对 mid 的要求）。
func IsSongMid(s string) bool {
	return s != "" && strings.IndexFunc(s, notASCIIAlnum) < 0 && strings.IndexFunc(s, notASCIIDigit) >= 0
}

func notASCIIDigit(r rune) bool { return r < '0' || r > '9' }

func notASCIIAlnum(r rune) bool {
	return notASCIIDigit(r) && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
}
