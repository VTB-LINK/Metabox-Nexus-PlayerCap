package kugou

// 本文件只测 rereadSameSong —— 换歌取词之后重读到的播放信息能否当作本轮的读数。
//
// 轮询循环（runSession）没有离线夹具：这里只守判据本身，循环里采用之后替换了哪些量、lastStatus
// 记的是哪个状态，靠读代码与真机。
//
// 变异均在仓库的一份分离 worktree 副本里逐条注入、跑完即还原。

import (
	"errors"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
)

func TestRereadSameSong(t *testing.T) {
	readErr := errors.New("read: i/o timeout")
	cases := []struct {
		name  string
		fresh *cdp.PlayInfo
		err   error
		hash  string
		want  bool
	}{
		// 变异自证：判据改为恒假、或把相等改成不等，本例红（实测）。
		{"读成功且仍是这首：采用", &cdp.PlayInfo{Hash: "ABC123"}, nil, "ABC123", true},
		// 变异自证：去掉 `err == nil`，本例红（实测）。
		{"读出错但带回了同一首的信息：不采用", &cdp.PlayInfo{Hash: "ABC123"}, readErr, "ABC123", false},
		{"读出错、没有信息（连接断开）：不采用", nil, readErr, "ABC123", false},
		// 变异自证：去掉 `fresh != nil`，本例因空指针解引用而崩（实测；上一例有 err 判断挡在前面，不崩）。
		{"没有歌（GetPlayInfo 返回 nil, nil）：不采用", nil, nil, "ABC123", false},
		// 变异自证：去掉 `fresh.Hash == hash`，这两例与下一例红（实测）。
		{"取词期间又换了歌：不采用", &cdp.PlayInfo{Hash: "DEF456"}, nil, "ABC123", false},
		{"读到空 hash（歌曲已卸载）：不采用", &cdp.PlayInfo{Hash: ""}, nil, "ABC123", false},
		// 变异自证：改用 strings.EqualFold 比较，本例红（实测）。
		{"hash 只差大小写：与循环的换歌判据一样按不同处理", &cdp.PlayInfo{Hash: "abc123"}, nil, "ABC123", false},
		// 变异自证：去掉 `hash != ""`，本例红（实测）。
		{"两边都是空 hash：不算同一首", &cdp.PlayInfo{Hash: ""}, nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rereadSameSong(c.fresh, c.err, c.hash); got != c.want {
				t.Fatalf("rereadSameSong(%+v, %v, %q) = %v，期望 %v", c.fresh, c.err, c.hash, got, c.want)
			}
		})
	}
}
