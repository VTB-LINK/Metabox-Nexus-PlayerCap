package assoc

// 本文件只测 ParseAssociations 从各条目取出的字段：byUser、LyricOffset 与 songUrl 归类。

import "testing"

func TestParseAssociationsByUser(t *testing.T) {
	cases := []struct {
		attr string
		want bool
	}{
		{`byUser="1"`, true},
		{`byUser="0"`, false},
		{``, false},
		{`byUser="true"`, false},
		{`byUser=" 1"`, false},
		{`byUser="01"`, false},
	}
	for _, c := range cases {
		es, err := ParseAssociations(utf16le(assocDoc(`<Associate songUrl="E:\a.flac" ` + c.attr + `/>`)))
		if err != nil || len(es) != 1 || es[0].ByUser != c.want {
			t.Errorf("%s：es=%v err=%v，ByUser 应为 %v", c.attr, es, err, c.want)
		}
	}
}

func TestParseAssociationsLyricOffset(t *testing.T) {
	cases := []struct {
		attr string
		want int
	}{
		{`LyricOffset="-1000"`, -1000},
		{`LyricOffset="500"`, 500},
		{`LyricOffset="600000"`, 600000},
		{`LyricOffset="-600000"`, -600000},
		{`LyricOffset="0"`, 0},
		{`LyricOffset="abc"`, 0},
		{`LyricOffset="600001"`, 0},
		{`LyricOffset="-600001"`, 0},
		{`LyricOffset="1.5"`, 0},
		{`LyricOffset=" 5"`, 0},
		{`LyricOffset="99999999999999999999"`, 0},
		{`LyricOffset=""`, 0},
		{``, 0},
	}
	for _, c := range cases {
		es, err := ParseAssociations(utf16le(assocDoc(`<Associate songUrl="E:\a.flac" ` + c.attr + `/>`)))
		if err != nil || len(es) != 1 || es[0].LyricOffsetMs != c.want {
			t.Errorf("%s：es=%v err=%v，LyricOffsetMs 应为 %d", c.attr, es, err, c.want)
		}
	}
}

// 各条目按文件顺序给出，songUrl 归类的结果随条目带出；归不了类的条目照样列出（KindOther），由选条目时忽略。
func TestParseAssociationsClassifiesSongURL(t *testing.T) {
	doc := assocDoc(`<Associate songUrl="http://stream4.qqmusic.qq.com/000TestMid0001.wma" lyricUrl="C:\c\m_qm.qrc" byUser="1" LyricOffset="0"/>
<Associate songUrl="http://streamundefined.qqmusic.qq.com/123456789.wma" lyricUrl="C:\c\i_qm.qrc" byUser="0" LyricOffset="0"/>
<Associate songUrl="E:\a.flac" lyricUrl="E:\a.lrc" byUser="1" LyricOffset="-1000"/>
<Associate songUrl="a.flac" lyricUrl="" byUser="0" LyricOffset="0"/>
`)
	es, err := ParseAssociations(utf16le(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{SongURL: "http://stream4.qqmusic.qq.com/000TestMid0001.wma", LyricURL: `C:\c\m_qm.qrc`, ByUser: true, Kind: KindMid, Mid: "000TestMid0001"},
		{SongURL: "http://streamundefined.qqmusic.qq.com/123456789.wma", LyricURL: `C:\c\i_qm.qrc`, Kind: KindID, ID: 123456789},
		{SongURL: `E:\a.flac`, LyricURL: `E:\a.lrc`, ByUser: true, LyricOffsetMs: -1000, Kind: KindLocal},
		{SongURL: "a.flac", Kind: KindOther},
	}
	if len(es) != len(want) {
		t.Fatalf("len=%d，应为 %d：%v", len(es), len(want), es)
	}
	for i := range want {
		if es[i] != want[i] {
			t.Errorf("第 %d 条 = %+v，应为 %+v", i, es[i], want[i])
		}
	}
}

func TestParseAssociationsEmptyTable(t *testing.T) {
	for _, doc := range []string{assocDoc(""), xmlDecl + "<AssociateInfos/>"} {
		if es, err := ParseAssociations(utf16le(doc)); err != nil || len(es) != 0 {
			t.Errorf("%q：es=%v err=%v，应为空表", doc, es, err)
		}
	}
}
