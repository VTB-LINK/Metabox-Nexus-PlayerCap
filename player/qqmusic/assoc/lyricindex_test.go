package assoc

// 本文件只测 ParseLyricIndex：原文路径（转小写）到翻译、音译路径的映射。

import (
	"errors"
	"testing"
)

func TestParseLyricIndex(t *testing.T) {
	doc := xmlDecl + `<LyricInfos>
<Lyric lyrictimetag="1" url="C:\Cache\QQMusicLyricNew\A - B_qm.qrc" tsurl="C:\Cache\QQMusicLyricNew\A - B_qmts.qrc" romaurl="" id="1" timetag="1"/>
<Lyric url="C:\Cache\QQMusicLyricNew\C &amp; D_qm.qrc" tsurl="" romaurl="C:\Cache\QQMusicLyricNew\C &amp; D_qmRoma.qrc"/>
<Lyric url="C:\Cache\QQMusicLyricNew\E_qm.qrc"/>
<Lyric url="" tsurl="C:\x_qmts.qrc"/>
<Lyric tsurl="C:\y_qmts.qrc"/>
</LyricInfos>
`
	m, err := ParseLyricIndex(utf16le(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]LyricFiles{
		`c:\cache\qqmusiclyricnew\a - b_qm.qrc`: {Trans: `C:\Cache\QQMusicLyricNew\A - B_qmts.qrc`},
		`c:\cache\qqmusiclyricnew\c & d_qm.qrc`: {Roma: `C:\Cache\QQMusicLyricNew\C & D_qmRoma.qrc`},
		`c:\cache\qqmusiclyricnew\e_qm.qrc`:     {},
	}
	if len(m) != len(want) {
		t.Fatalf("映射 = %v，应为 %v（url 为空或缺失的条目不收）", m, want)
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("m[%q] = %+v（存在=%v），应为 %+v", k, got, ok, v)
		}
	}
}

func TestParseLyricIndexRejectsAssociationTable(t *testing.T) {
	if _, err := ParseLyricIndex(utf16le(assocDoc(sampleItem))); !errors.Is(err, ErrStructure) {
		t.Fatalf("把关联表当歌词索引：err=%v，应为 ErrStructure", err)
	}
	if _, err := ParseLyricIndex(append(utf16le(xmlDecl+"<LyricInfos/>"), 0)); !errors.Is(err, ErrStructure) {
		t.Fatalf("奇数字节长度：err=%v，应为 ErrStructure", err)
	}
}
