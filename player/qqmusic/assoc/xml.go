package assoc

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"Metabox-Nexus-PlayerCap/i18n"
)

const (
	maxEntries       = 100_000
	maxAttrBytes     = 32767 * 3 // 最长路径 32767 个 UTF-16 码元，按 UTF-8 每个至多 3 字节
	maxLyricOffsetMs = 600_000
)

// Entry 是关联表的一条（设计 §2.2、§6.3）。Kind、Mid、ID 是 ClassifySongURL(SongURL) 的结果。
type Entry struct {
	SongURL, LyricURL string
	ByUser            bool // 只有属性值为 "1" 才为真
	LyricOffsetMs     int  // 毫秒，正数使歌词提前；解析失败或绝对值超过 600,000 时为 0
	Kind              URLKind
	Mid               string
	ID                uint32
}

// LyricFiles 是歌词索引里与一份原文歌词同属一首的翻译、音译文件路径，缺的为空串。
type LyricFiles struct{ Trans, Roma string }

// ParseAssociations 解析解密后的 LyricAssociation.dat：根元素 AssociateInfos 下各个 Associate，按文件顺序给出。
// 归不了类的条目照样列出（Kind 为 KindOther）。
func ParseAssociations(plain []byte) ([]Entry, error) {
	var es []Entry
	err := walkItems(plain, "AssociateInfos", "Associate", func(a []xml.Attr) {
		e := Entry{
			SongURL:       attr(a, "songUrl"),
			LyricURL:      attr(a, "lyricUrl"),
			ByUser:        attr(a, "byUser") == "1",
			LyricOffsetMs: parseLyricOffset(attr(a, "LyricOffset")),
		}
		e.Kind, e.Mid, e.ID = ClassifySongURL(e.SongURL)
		es = append(es, e)
	})
	if err != nil {
		return nil, err
	}
	return es, nil
}

func parseLyricOffset(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < -maxLyricOffsetMs || n > maxLyricOffsetMs {
		return 0
	}
	return n
}

// ParseLyricIndex 解析解密后的 lyricinfo.dat：根元素 LyricInfos 下各个 Lyric，给出「原文路径 → 翻译、音译路径」。
// 键为 strings.ToLower(url)，供不区分大小写地比较路径；url 为空的条目不收。
func ParseLyricIndex(plain []byte) (map[string]LyricFiles, error) {
	m := map[string]LyricFiles{}
	err := walkItems(plain, "LyricInfos", "Lyric", func(a []xml.Attr) {
		if u := attr(a, "url"); u != "" {
			m[strings.ToLower(u)] = LyricFiles{Trans: attr(a, "tsurl"), Roma: attr(a, "romaurl")}
		}
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ParseCachePath 从解密后的 ConfigInfoXML1.dat 取 DomainName="CachePath.ini" 那个 Domain 的 Path，须为绝对路径。
//
// 该配置不是合规的 XML（实测 DOMAIN_GLOBAL 带着名为 51_rate_free.ini 的属性，encoding/xml 读到即报错），
// 所以不按整份文档解析，而是逐个取出 <Domain …> 开始标签单独交给 encoding/xml，解析不了的节点跳过。
func ParseCachePath(plain []byte) (string, error) {
	s, err := decodeUTF16(plain)
	if err != nil {
		return "", err
	}
	for {
		i := strings.Index(s, "<Domain ")
		if i < 0 {
			return "", i18n.Errorf("%w: no CachePath.ini setting", ErrStructure)
		}
		tag := startTag(s[i:])
		s = s[i+len(tag):]
		tok, err := xml.NewDecoder(strings.NewReader(tag)).Token()
		el, ok := tok.(xml.StartElement)
		if err != nil || !ok || attr(el.Attr, "DomainName") != "CachePath.ini" {
			continue
		}
		if err := checkAttrs(el.Attr); err != nil {
			return "", err
		}
		p := attr(el.Attr, "Path")
		if !filepath.IsAbs(p) {
			return "", i18n.Errorf("%w: cache path is not absolute", ErrStructure)
		}
		return p, nil
	}
}

// startTag 返回 s 开头那个开始标签，到双引号之外的第一个 '>' 为止（含）；没有则返回 s。
// QQ 写的属性值都用双引号，值里的引号已转义，'>' 可以原样出现。
func startTag(s string) string {
	quoted := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case '>':
			if !quoted {
				return s[:i+1]
			}
		}
	}
	return s
}

// walkItems 解析整份文档：根元素须为 root；对根元素之下名为 item 的直接子元素逐个调用 fn。
// 根元素闭合即停，其后的内容不读。条目超过 maxEntries、任一属性值超过 maxAttrBytes 字节为结构错误。
func walkItems(plain []byte, root, item string, fn func([]xml.Attr)) error {
	s, err := decodeUTF16(plain)
	if err != nil {
		return err
	}
	d := xml.NewDecoder(strings.NewReader(s))
	depth, n := 0, 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return i18n.Errorf("%w: no root element", ErrStructure)
		}
		if err != nil {
			return i18n.Errorf("%w: %v", ErrStructure, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 && t.Name.Local != root {
				return i18n.Errorf("%w: root element is <%s>, expected <%s>", ErrStructure, t.Name.Local, root)
			}
			if err := checkAttrs(t.Attr); err != nil {
				return err
			}
			if depth == 2 && t.Name.Local == item {
				if n++; n > maxEntries {
					return i18n.Errorf("%w: more than %d entries", ErrStructure, maxEntries)
				}
				fn(t.Attr)
			}
		case xml.EndElement:
			if depth--; depth == 0 {
				return nil
			}
		}
	}
}

func checkAttrs(attrs []xml.Attr) error {
	for _, a := range attrs {
		if len(a.Value) > maxAttrBytes {
			return i18n.Errorf("%w: attribute %s is longer than %d bytes", ErrStructure, a.Name.Local, maxAttrBytes)
		}
	}
	return nil
}

func attr(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// decodeUTF16 把 UTF-16LE 明文（可带 BOM）解码为 UTF-8，不成对的代理项为结构错误；再去掉开头的 XML 声明，
// 因为声明写的 encoding="utf-8" 与实际编码不符（设计 §2.1）。
func decodeUTF16(plain []byte) (string, error) {
	if len(plain)%2 != 0 {
		return "", i18n.Errorf("%w: UTF-16 byte length %d is odd", ErrStructure, len(plain))
	}
	plain = bytes.TrimPrefix(plain, []byte{0xFF, 0xFE})
	var b strings.Builder
	b.Grow(len(plain) / 2)
	for i := 0; i < len(plain); i += 2 {
		r := rune(binary.LittleEndian.Uint16(plain[i:]))
		if utf16.IsSurrogate(r) {
			lo := utf8.RuneError
			if i+4 <= len(plain) {
				lo = rune(binary.LittleEndian.Uint16(plain[i+2:]))
			}
			if r = utf16.DecodeRune(r, lo); r == utf8.RuneError {
				return "", i18n.Errorf("%w: unpaired UTF-16 surrogate at byte %d", ErrStructure, i)
			}
			i += 2
		}
		b.WriteRune(r)
	}
	s := b.String()
	if strings.HasPrefix(s, "<?xml") {
		if _, rest, ok := strings.Cut(s, "?>"); ok {
			s = rest
		}
	}
	return s, nil
}
