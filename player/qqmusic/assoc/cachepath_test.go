package assoc

// 本文件只测 ParseCachePath：从配置里 DomainName="CachePath.ini" 的 Domain 取 Path，须为绝对路径。
//
// 实测的配置不是合规 XML：DomainName="DOMAIN_GLOBAL" 的 Domain 带着名为 51_rate_free.ini 的属性（XML 名不能以数字开头），
// encoding/xml 读到它即报错。所以这里的样本都照实测的形状带上这样一个 Domain。

import (
	"errors"
	"strings"
	"testing"
)

const (
	asyDomain    = `<Domain DomainName="ASY" _asyncapi_classname_CAsyncAutoDetectIEProxy="1"/>` + "\r\n"
	globalDomain = `<Domain DomainName="DOMAIN_GLOBAL" 51_rate_free.ini="1" AVolumDown_0="7" note="&lt;item&gt;` + "\r\n" +
		`&lt;jumpurl&gt;http://example.com/?&lt;/jumpurl&gt;` + "\r\n" + `&lt;/item&gt;"/>` + "\r\n"
)

func cachePathDomain(path string) string {
	return `<Domain DomainName="CachePath.ini" Convert="" NeedCleanWebkitPath="0" Path="` + path + `" Volumn="0" bCacheSizeAuto="1"/>` + "\r\n"
}

func configDoc(domains ...string) []byte {
	return utf16le(xmlDecl + "<QQMusicOption>\r\n" + strings.Join(domains, "") + "</QQMusicOption>\r\n")
}

func TestParseCachePath(t *testing.T) {
	cases := map[string][]byte{
		"实测顺序":        configDoc(asyDomain, cachePathDomain(`C:\Users\u\QQMusicCache\`), globalDomain),
		"排在不合规的节点之后":  configDoc(asyDomain, globalDomain, cachePathDomain(`C:\Users\u\QQMusicCache\`)),
		"属性值里有 >":     configDoc(`<Domain DomainName="CachePath.ini" note="a>b" Path="C:\Users\u\QQMusicCache\"/>`),
		"带子元素的节点":     configDoc(`<Domain DomainName="CachePath.ini" Path="C:\Users\u\QQMusicCache\"><x/></Domain>`),
		"有 BOM":       append([]byte{0xFF, 0xFE}, configDoc(cachePathDomain(`C:\Users\u\QQMusicCache\`))...),
		"别的元素上也有同名属性": configDoc(`<Other DomainName="CachePath.ini" Path="D:\wrong\"/>`, cachePathDomain(`C:\Users\u\QQMusicCache\`)),
	}
	for name, plain := range cases {
		got, err := ParseCachePath(plain)
		if err != nil || got != `C:\Users\u\QQMusicCache\` {
			t.Errorf("%s：got=%q err=%v", name, got, err)
		}
	}
	if got, err := ParseCachePath(configDoc(cachePathDomain(`\\nas\m &amp; n\Cache\`))); err != nil || got != `\\nas\m & n\Cache\` {
		t.Errorf("UNC 与 &amp;：got=%q err=%v", got, err)
	}
	long := `C:\` + strings.Repeat("中", 32766) // 恰为 32767×3 字节
	if got, err := ParseCachePath(configDoc(cachePathDomain(long))); err != nil || got != long {
		t.Errorf("路径 32767×3 字节：len=%d err=%v，应可解析", len(got), err)
	}
}

func TestParseCachePathRejects(t *testing.T) {
	cases := map[string][]byte{
		"相对路径":         configDoc(cachePathDomain(`QQMusicCache\`)),
		"盘符相对":         configDoc(cachePathDomain(`C:QQMusicCache\`)),
		"根相对":          configDoc(cachePathDomain(`\QQMusicCache\`)),
		"空路径":          configDoc(cachePathDomain(``)),
		"没有 Path 属性":   configDoc(`<Domain DomainName="CachePath.ini" Convert=""/>`),
		"没有该节点":        configDoc(asyDomain, globalDomain),
		"只在别的元素上":      configDoc(`<Other DomainName="CachePath.ini" Path="C:\x\"/>`),
		"该节点本身不合规":     configDoc(`<Domain DomainName="CachePath.ini" 1x="1" Path="C:\x\"/>`),
		"属性值未闭合":       configDoc(`<Domain DomainName="CachePath.ini" Path="C:\x\/>`),
		"奇数字节长度":       append(configDoc(cachePathDomain(`C:\x\`)), 0),
		"孤立代理项":        withUnits(`<QQMusicOption>`+cachePathDomain(`C:\x\`), []uint16{0xDC00}, `</QQMusicOption>`),
		"路径 32767×3+1": configDoc(cachePathDomain(`C:\` + strings.Repeat("中", 32766) + "a")),
		"空文档":          {},
	}
	for name, plain := range cases {
		if got, err := ParseCachePath(plain); !errors.Is(err, ErrStructure) {
			t.Errorf("%s：got=%q err=%v，应为 ErrStructure", name, got, err)
		}
	}
}
