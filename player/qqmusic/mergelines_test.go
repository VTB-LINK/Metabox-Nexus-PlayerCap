package qqmusic

// 本文件只测已解析行的合并 mergeTransLines、mergeRomaLines：与在线取词的 mergeTrans、mergeRoma 结果相同，
// 空输入不动主歌词。在线两函数的配对规则另有 trans_test.go、roma_test.go。

import (
	"reflect"
	"testing"
)

func mainLinesForMerge() []lyricLine {
	lines, _, _ := parseLRC(qrcXML("[ti:x]\n[1000,2000]第(1000,500)一(1500,1500)\n[3000,2000]第(3000,500)二(3500,1500)\n"), 0)
	return lines
}

func TestMergeLinesMatchOnlineMerge(t *testing.T) {
	trans := "[00:01.00]//\n[00:01.00]译一\n[00:03.00]译二\n"
	roma := qrcXML("[ti:x]\n[1000,2000]da(1000,500)i(1500,1500)\n[3000,2000]ni(3000,2000)\n")

	online := mainLinesForMerge()
	mergeTrans(online, trans)
	mergeRoma(online, roma)

	local := mainLinesForMerge()
	transLines, _, _ := parseLRCLines(trans)
	romaLines, _, _ := parseLRCLines(roma)
	mergeTransLines(local, transLines)
	mergeRomaLines(local, romaLines)

	if online[0].SubText != "译一" || online[1].RomaText != "ni" {
		t.Fatalf("fixture: online merge = %+v", online)
	}
	if !reflect.DeepEqual(local, online) {
		t.Errorf("merged lines differ:\nlocal  %+v\nonline %+v", local, online)
	}
}

// 反例：翻译或音译为空、主歌词为空，都不改主歌词、不出错。
func TestMergeLinesEmptyIsNoop(t *testing.T) {
	want := mainLinesForMerge()
	got := mainLinesForMerge()
	mergeTransLines(got, nil)
	mergeRomaLines(got, []lyricLine{})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merging empty tracks changed the lines: %+v", got)
	}
	track := mainLinesForMerge()
	mergeTransLines(nil, track)
	mergeRomaLines(nil, track)
}
