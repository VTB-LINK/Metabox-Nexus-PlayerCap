package qqmusic

// 本文件只测 rereadSameSong —— 取词之后重读到的元数据能否当作本轮的读数。
//
// 轮询循环（runSession）没有离线夹具：这里只守判据本身，循环里采用之后替换了哪些量、位置取的是
// 哪个读数，靠读代码与真机。
//
// 变异均在仓库的一份分离 worktree 副本里逐条注入、跑完即还原。

import (
	"errors"
	"testing"
)

func TestRereadSameSong(t *testing.T) {
	readErr := errors.New("process not attached")
	cases := []struct {
		name  string
		fresh *SongMetadata
		err   error
		song  string
		want  bool
	}{
		// 变异自证：判据改为恒假、或把相等改成不等，本例红（实测）。
		{"读成功且歌名仍是已认领的那首：采用", &SongMetadata{Name: "晴天", ProgressMs: 1500}, nil, "晴天", true},
		{"歌名相同、歌手不同：循环只按歌名判换歌，这里同样采用", &SongMetadata{Name: "晴天", Singer: "翻唱者"}, nil, "晴天", true},
		// 变异自证：去掉 `err == nil`，本例红（实测）。
		{"读出错但带回了同名的元数据：不采用", &SongMetadata{Name: "晴天"}, readErr, "晴天", false},
		{"读出错、没有元数据：不采用", nil, readErr, "晴天", false},
		// 变异自证：去掉 `fresh != nil`，本例因空指针解引用而崩（实测；上一例有 err 判断挡在前面，不崩）。
		{"没有元数据也没有报错：不采用", nil, nil, "晴天", false},
		// 变异自证：去掉 `fresh.Name == name`，这三例与下一例红（实测）。
		{"取词期间又换了歌：不采用", &SongMetadata{Name: "七里香"}, nil, "晴天", false},
		{"读到空闲态的占位串：不采用", &SongMetadata{Name: "QQ音乐 听我想听"}, nil, "晴天", false},
		{"读到空歌名：不采用", &SongMetadata{Name: ""}, nil, "晴天", false},
		// 变异自证：改用 strings.HasPrefix(fresh.Name, name) 比较，本例红（实测）。
		{"歌名只是前缀相同：按不同处理", &SongMetadata{Name: "晴天 (Live)"}, nil, "晴天", false},
		// 变异自证：去掉 `name != ""`，本例红（实测）。
		{"尚未认领任何歌、读到的歌名也为空：不算同一首", &SongMetadata{Name: ""}, nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rereadSameSong(c.fresh, c.err, c.song); got != c.want {
				t.Fatalf("rereadSameSong(%+v, %v, %q) = %v，期望 %v", c.fresh, c.err, c.song, got, c.want)
			}
		})
	}
}
