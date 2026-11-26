package assoc

// 本文件只测 collect 对设计 §2.3 四种实测样本给出的候选：路径与来源与该表一致。
// hash 与 LyricKey 的前 8 位取自设计，其余为补足 32 位的合成值；时间为合成值。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
)

const (
	sampleWumingzhi = "9d6e3b30c1f04a5e8b7d2c9a6f3e1d08" // 本地文件：1_门尼 - 无名指_(Instrumental)
	sampleTonghua   = "20f99236ab4c4d1e9f8a7b6c5d4e3f21" // 在线歌曲：暗杠 - 童话镇
	sampleSpectre   = "65aa8965d2e14f3a8c7b6a5948372615" // 在线歌曲：Alan Walker - Spectre
	keyWumingzhi    = "1685c8e7f0a14b2c9d3e4f5a6b7c8d9e"
	keyTonghua      = "a2fd05d0e1f2a3b4c5d6e7f8091a2b3c"
	zeroKey         = "00000000000000000000000000000000"

	pathWumingzhiKRC = `C:\Users\user\Music\门尼 - 无名指 (The Rust)-bee95e0c1d2e3f4a5b6c7d8e9f0a1b2c-704320507-00000000.krc`
	pathWumingzhiLRC = `1_门尼 - 无名指_(Instrumental)-9d6e3b30c1f04a5e8b7d2c9a6f3e1d08-565331094-00000000.lrc`
	pathTonghuaLRC   = `E:\Downloads\Lunette\伴奏\暗杠 - 童话镇.lrc`
	pathSpectreKRC   = `Alan Walker - Spectre-65aa8965d2e14f3a8c7b6a5948372615-12345678-00000000.krc`
)

// designSampleDB 按设计 §2.3 造库。associated 为假时是无名指关联之前（样本 2）：其歌曲条目的
// LyricKey 全为 0，LyricInfo 里还没有关联那一行。另有一张没有这两列的 ShareFileItems。
func designSampleDB(associated bool) *fakeTables {
	key := zeroKey
	if associated {
		key = keyWumingzhi
	}
	qqnt := songRow{sampleWumingzhi, zeroKey} // 同 hash 的另一份文件（QQNT 下载目录），始终未关联
	rows := []lyricRow{
		{"5e1c0d2b3a4f5e6d7c8b9a0f1e2d3c4b", pathWumingzhiLRC, sampleWumingzhi, 1763200000},
		{keyTonghua, pathTonghuaLRC, zeroKey, 1763290000},
		{"7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d", pathSpectreKRC, sampleSpectre, 1763250000},
	}
	if associated {
		rows = append(rows, lyricRow{keyWumingzhi, pathWumingzhiKRC, "bee95e0c1d2e3f4a5b6c7d8e9f0a1b2c", 1763280000})
	}
	return newDB(
		&fakeTable{name: "ShareFileItems", cols: []string{"FileHash", "Key"}, rows: [][]kgdb.Value{
			{text(sampleWumingzhi), text(keyTonghua)},
		}},
		songTable("LocalFile", songRow{sampleWumingzhi, key}, qqnt),
		songTable("RecentlyPlayedItem", songRow{sampleWumingzhi, key}),
		songTable("PlayQueueAssistItem_0",
			songRow{sampleWumingzhi, key},
			songRow{sampleTonghua, zeroKey},
			songRow{sampleTonghua, zeroKey},
			songRow{sampleTonghua, keyTonghua},
		),
		lyricInfo(rows...),
	)
}

// designSamples 是四种样本的期望候选；读库的用例（设计 §7「读库」）可复用。
var designSamples = []struct {
	name       string
	associated bool
	hash       string
	want       []candidate
}{
	{"本地文件、手动关联 krc", true, sampleWumingzhi, []candidate{
		{Path: pathWumingzhiKRC, Source: SourceAssociation, UpdateTime: 1763280000},
		{Path: pathWumingzhiLRC, Source: SourceCache, UpdateTime: 1763200000},
	}},
	{"本地文件、酷狗自动匹配", false, sampleWumingzhi, []candidate{
		{Path: pathWumingzhiLRC, Source: SourceCache, UpdateTime: 1763200000},
	}},
	{"在线歌曲、手动关联 lrc", true, sampleTonghua, []candidate{
		{Path: pathTonghuaLRC, Source: SourceAssociation, UpdateTime: 1763290000},
	}},
	{"在线歌曲、酷狗缓存", true, sampleSpectre, []candidate{
		{Path: pathSpectreKRC, Source: SourceCache, UpdateTime: 1763250000},
	}},
}

func TestCollectSamplesFromDesign(t *testing.T) {
	for _, s := range designSamples {
		t.Run(s.name, func(t *testing.T) {
			wantCandidates(t, mustCollect(t, designSampleDB(s.associated), s.hash), s.want)
		})
	}
}
