package assoc

// 本文件只测关联表与歌词索引共用的解码与结构校验：UTF-16LE 与 BOM、XML 声明、实体、根元素、闭合、条目数与属性长度上限。

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

// utf16le 把 s 编码为不带 BOM 的 UTF-16LE。
func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

const xmlDecl = "<?xml version=\"1.0\" encoding=\"utf-8\"?>\r\n"

func assocDoc(items string) string {
	return xmlDecl + "<AssociateInfos>\r\n" + items + "</AssociateInfos>\r\n"
}

const sampleItem = `<Associate songUrl="http://stream4.qqmusic.qq.com/000TestMid0001.wma" lyricUrl="C:\cache\a &amp; b_qm.qrc" byUser="1" LyricOffset="0"/>` + "\r\n"

func TestParseAssociationsDecodesUTF16WithAndWithoutBOM(t *testing.T) {
	body := utf16le(assocDoc(sampleItem))
	for name, plain := range map[string][]byte{
		"无 BOM": body,
		"有 BOM": append([]byte{0xFF, 0xFE}, body...),
	} {
		es, err := ParseAssociations(plain)
		if err != nil || len(es) != 1 {
			t.Fatalf("%s：es=%v err=%v", name, es, err)
		}
		if es[0].LyricURL != `C:\cache\a & b_qm.qrc` {
			t.Errorf("%s：lyricUrl = %q，&amp; 应还原为 &", name, es[0].LyricURL)
		}
	}
}

// 声明里写的编码与实际不符（实测写 utf-8），解析前去掉声明；写成 UTF-16 也照常解析，有无 BOM 都一样。
func TestParseAssociationsIgnoresDeclaredEncoding(t *testing.T) {
	for _, decl := range []string{
		xmlDecl,
		"<?xml version=\"1.0\" encoding=\"UTF-16\"?>\r\n",
		"",
	} {
		body := utf16le(decl + "<AssociateInfos>" + sampleItem + "</AssociateInfos>")
		for _, bom := range [][]byte{nil, {0xFF, 0xFE}} {
			if es, err := ParseAssociations(append(bom, body...)); err != nil || len(es) != 1 {
				t.Errorf("声明 %q、BOM % x：es=%v err=%v", decl, bom, es, err)
			}
		}
	}
}

func TestParseAssociationsKeepsSurrogatePairs(t *testing.T) {
	doc := assocDoc(`<Associate songUrl="E:\𠀀.flac" lyricUrl="" byUser="0" LyricOffset="0"/>`)
	es, err := ParseAssociations(utf16le(doc))
	if err != nil || len(es) != 1 || es[0].SongURL != `E:\𠀀.flac` {
		t.Fatalf("es=%v err=%v", es, err)
	}
}

func TestParseRejectsMalformedDocuments(t *testing.T) {
	cases := map[string][]byte{
		"根元素不对":    utf16le(xmlDecl + "<LyricInfos>" + sampleItem + "</LyricInfos>"),
		"根元素未闭合":   utf16le(xmlDecl + "<AssociateInfos>" + sampleItem),
		"条目未闭合":    utf16le(xmlDecl + `<AssociateInfos><Associate songUrl="x"></AssociateInfos>`),
		"属性值未闭合":   utf16le(xmlDecl + `<AssociateInfos><Associate songUrl="x/></AssociateInfos>`),
		"声明未闭合":    utf16le(`<?xml version="1.0"<AssociateInfos/>`),
		"没有根元素":    utf16le(xmlDecl + "<!-- x -->\r\n"),
		"空文档":      {},
		"奇数字节长度":   append(utf16le(assocDoc(sampleItem)), '\n'),
		"大端 BOM":   append([]byte{0xFE, 0xFF}, utf16le(assocDoc(sampleItem))...),
		"NUL 字符":   utf16le(assocDoc(`<Associate songUrl="E:\a` + "\x00" + `.flac"/>`)),
		"孤立高代理":    withUnits(itemHead, []uint16{0xD800}, itemTail),
		"高代理后非低代理": withUnits(itemHead, []uint16{0xD800, 'a'}, itemTail),
		"孤立低代理":    withUnits(itemHead, []uint16{0xDC00}, itemTail),
		"低高颠倒":     withUnits(itemHead, []uint16{0xDC00, 0xD800}, itemTail),
		"末尾高代理":    append(utf16le(assocDoc(sampleItem)), 0x00, 0xD8),
	}
	for name, plain := range cases {
		if _, err := ParseAssociations(plain); !errors.Is(err, ErrStructure) {
			t.Errorf("%s：ParseAssociations err=%v，应为 ErrStructure", name, err)
		}
	}
	// 歌词索引走同一套校验：根元素不对、未闭合同样报错。
	for name, doc := range map[string]string{
		"根元素不对":  xmlDecl + "<AssociateInfos/>",
		"根元素未闭合": xmlDecl + `<LyricInfos><Lyric url="C:\a_qm.qrc"/>`,
	} {
		if _, err := ParseLyricIndex(utf16le(doc)); !errors.Is(err, ErrStructure) {
			t.Errorf("%s：ParseLyricIndex err=%v，应为 ErrStructure", name, err)
		}
	}
}

const (
	itemHead = xmlDecl + `<AssociateInfos><Associate songUrl="E:\`
	itemTail = `a.flac"/></AssociateInfos>`
)

// withUnits 把 prefix、原样的 UTF-16 码元 units 与 suffix 拼成 UTF-16LE，用来造不成对的代理项。
func withUnits(prefix string, units []uint16, suffix string) []byte {
	b := utf16le(prefix)
	for _, u := range units {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return append(b, utf16le(suffix)...)
}

func TestParseAssociationsEntryCap(t *testing.T) {
	item := `<Associate/>`
	if es, err := ParseAssociations(utf16le(assocDoc(strings.Repeat(item, 100_000)))); err != nil || len(es) != 100_000 {
		t.Fatalf("100,000 条：len=%d err=%v，应全部解析", len(es), err)
	}
	if _, err := ParseAssociations(utf16le(assocDoc(strings.Repeat(item, 100_001)))); !errors.Is(err, ErrStructure) {
		t.Fatalf("100,001 条：err=%v，应为 ErrStructure", err)
	}
	// 只数根元素下名为 Associate 的直接子元素。
	if es, err := ParseAssociations(utf16le(assocDoc(strings.Repeat(`<Other/>`, 100_001) + item))); err != nil || len(es) != 1 {
		t.Fatalf("100,001 个别的元素加 1 条：len=%d err=%v", len(es), err)
	}
}

// 上限为 32767×3 字节，按 UTF-8 字节计：32767 个 BMP 汉字恰好到顶。
func TestParseAttributeLengthCap(t *testing.T) {
	limit := strings.Repeat("中", 32767) // 98,301 字节
	attrDoc := func(where, v string) string {
		switch where {
		case "条目":
			return assocDoc(`<Associate songUrl="` + v + `"/>`)
		case "别的元素":
			return assocDoc(`<Other x="` + v + `"/>`)
		default: // 根元素
			return `<AssociateInfos x="` + v + `"/>`
		}
	}
	for _, where := range []string{"条目", "别的元素", "根元素"} {
		if _, err := ParseAssociations(utf16le(attrDoc(where, limit))); err != nil {
			t.Errorf("%s属性 32767×3 字节：err=%v，应可解析", where, err)
		}
		if _, err := ParseAssociations(utf16le(attrDoc(where, limit+"a"))); !errors.Is(err, ErrStructure) {
			t.Errorf("%s属性 32767×3+1 字节：err=%v，应为 ErrStructure", where, err)
		}
	}
	if _, err := ParseLyricIndex(utf16le(`<LyricInfos><Lyric url="` + limit + "a" + `"/></LyricInfos>`)); !errors.Is(err, ErrStructure) {
		t.Errorf("歌词索引属性 32767×3+1 字节：err=%v，应为 ErrStructure", err)
	}
}
